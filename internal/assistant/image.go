package assistant

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // 注册解码器：只读取图片头（尺寸），不解码像素
	_ "image/png"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 带图提问（REQ-009）的限制。
const (
	MaxImages      = 4       // 单条提问的图片上限
	MaxImageBytes  = 5 << 20 // 单张图片的大小上限（按实际读取的字节数）
	MaxImageSide   = 8192    // 图片宽、高的上限（像素）
	imageFetchTime = 30 * time.Second
)

// 图片类型：只接受这三种（按内容识别，声明的类型须与实际一致）。
const (
	ImagePNG  = "image/png"
	ImageJPEG = "image/jpeg"
	ImageWebP = "image/webp"
)

// 图片读取失败的类别（Message.Error）。
const (
	// ErrKindAttachmentRejected 表示图片不可用：接入方拒绝读取（ErrNotFound / ErrForbidden）、内容不是声明的图片类型、
	// 超过大小或尺寸上限。重试会重新读取，接入方仍拒绝或内容不变时结果相同。
	ErrKindAttachmentRejected = "attachment_rejected"
	// ErrKindAttachmentError 表示读取图片时出现临时错误（接入方返回其他错误、读取超时），可以重试。
	ErrKindAttachmentError = "attachment_error"
)

var (
	// ErrImagesUnsupported 表示带图提问不可用：当前模型没有声明支持图片，或接入方没有配置读取附件。
	ErrImagesUnsupported = errors.New("assistant: images are not supported")
	// ErrImageDenied 由 Options.FetchImage 返回（可包装），表示接入方拒绝读取该图片（不存在或不属于提问人）。
	ErrImageDenied = errors.New("assistant: image access denied")
)

// Attachment 是提问附带的图片引用：文件由接入方的附件系统保存，灵工只记录引用（不含地址），回答时经 Options.FetchImage
// 以提问人身份读取内容。随用户消息存在 lingo_assistant_messages.attachments 中。
type Attachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size,omitempty"` // 调用方声明的大小（可空），以实际读取的为准
}

// Image 是读取并校验过的图片，随提问交给模型。
type Image struct {
	ContentType string // ImagePNG | ImageJPEG | ImageWebP，按内容识别
	Data        []byte
}

// FetchImage 以提问人 userID 的身份读取会话 convID 中用户消息 msgID 附带的图片 a；接入方拒绝时返回（包装）ErrImageDenied。
type FetchImage func(ctx context.Context, userID string, convID, msgID int64, a Attachment) (io.ReadCloser, error)

// CleanAttachments 校验并规整提问附带的图片引用：最多 MaxImages 张；ID 必填、不重复；文件名只保留名称部分；类型必填，
// 只能是 PNG、JPEG、WebP（image/jpg 视为 image/jpeg）；声明的大小不能为负或超过 MaxImageBytes。
func CleanAttachments(in []Attachment) ([]Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxImages {
		return nil, fmt.Errorf("%w: at most %d images per message", ticket.ErrInvalid, MaxImages)
	}
	refs := make([]ticket.Attachment, 0, len(in))
	for _, a := range in {
		refs = append(refs, ticket.Attachment{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	refs, err := ticket.CleanAttachments(refs) // ID、文件名与大小的规则与工单附件一致
	if err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(refs))
	for i, r := range refs {
		ct := imageType(in[i].ContentType)
		switch {
		case ct == "":
			return nil, fmt.Errorf("%w: attachment %q must be a PNG, JPEG or WebP image (content_type)", ticket.ErrInvalid, r.ID)
		case r.Size > MaxImageBytes:
			return nil, fmt.Errorf("%w: attachment %q exceeds %d MB", ticket.ErrInvalid, r.ID, MaxImageBytes>>20)
		}
		out = append(out, Attachment{ID: r.ID, Name: r.Name, ContentType: ct, Size: r.Size})
	}
	return out, nil
}

// imageType 规整声明的类型，不是支持的图片类型时为空。
func imageType(s string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	switch mt = strings.ToLower(mt); mt {
	case ImagePNG, ImageJPEG, ImageWebP:
		return mt
	case "image/jpg", "image/pjpeg":
		return ImageJPEG
	}
	return ""
}

// imageError 是读取或校验图片失败，kind 为 ErrKindAttachmentRejected 或 ErrKindAttachmentError。
type imageError struct {
	kind string
	err  error
}

func (e *imageError) Error() string { return e.err.Error() }
func (e *imageError) Unwrap() error { return e.err }

// loadImages 以提问人身份读取用户消息附带的全部图片并校验内容；任何一张不可用时整轮失败（不丢图作答）。
func (s *Service) loadImages(ctx context.Context, p string, m *Message) ([]Image, error) {
	if len(m.Attachments) == 0 {
		return nil, nil
	}
	if s.o.FetchImage == nil {
		return nil, ErrImagesUnsupported
	}
	out := make([]Image, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		img, err := s.loadImage(ctx, p, m, a)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

func (s *Service) loadImage(ctx context.Context, userID string, m *Message, a Attachment) (Image, error) {
	fctx, cancel := context.WithTimeout(ctx, imageFetchTime)
	defer cancel()
	rejected := func(format string, args ...any) (Image, error) {
		return Image{}, &imageError{kind: ErrKindAttachmentRejected, err: fmt.Errorf("attachment %q: "+format, append([]any{a.ID}, args...)...)}
	}
	rc, err := s.o.FetchImage(fctx, userID, m.ConversationID, m.ID, a)
	if err != nil {
		if errors.Is(err, ErrImageDenied) {
			return rejected("access denied")
		}
		if ctx.Err() != nil {
			return Image{}, ctx.Err()
		}
		return Image{}, &imageError{kind: ErrKindAttachmentError, err: fmt.Errorf("attachment %q: fetch failed", a.ID)}
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, MaxImageBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Image{}, ctx.Err()
		}
		return Image{}, &imageError{kind: ErrKindAttachmentError, err: fmt.Errorf("attachment %q: read failed", a.ID)}
	}
	if len(data) > MaxImageBytes {
		return rejected("exceeds %d MB", MaxImageBytes>>20)
	}
	ct, w, h, err := sniffImage(data)
	switch {
	case err != nil:
		return rejected("%v", err)
	case ct != a.ContentType:
		return rejected("content is %s, declared %s", ct, a.ContentType)
	case w <= 0 || h <= 0 || w > MaxImageSide || h > MaxImageSide:
		return rejected("image size %dx%d is out of range (max %d px per side)", w, h, MaxImageSide)
	}
	return Image{ContentType: ct, Data: data}, nil
}

var errNotImage = errors.New("content is not a PNG, JPEG or WebP image")

// sniffImage 按文件头识别图片类型并读取宽高（不解码像素）。
func sniffImage(b []byte) (ct string, w, h int, err error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		ct = ImagePNG
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		ct = ImageJPEG
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		w, h, err = webpSize(b)
		return ImageWebP, w, h, err
	default:
		return "", 0, 0, errNotImage
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return "", 0, 0, errNotImage
	}
	return ct, cfg.Width, cfg.Height, nil
}

// webpSize 读取 WebP 的宽高：VP8（有损）、VP8L（无损）、VP8X（扩展）三种首块。
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 {
		return 0, 0, errNotImage
	}
	chunk, data := string(b[12:16]), b[20:]
	switch chunk {
	case "VP8 ":
		// 帧头：3 字节帧标记 + 起始码 9d 01 2a + 14 位宽、14 位高
		if len(data) < 10 || data[3] != 0x9d || data[4] != 0x01 || data[5] != 0x2a {
			return 0, 0, errNotImage
		}
		return int(binary.LittleEndian.Uint16(data[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(data[8:10]) & 0x3fff), nil
	case "VP8L":
		if len(data) < 5 || data[0] != 0x2f {
			return 0, 0, errNotImage
		}
		v := binary.LittleEndian.Uint32(data[1:5])
		return int(v&0x3fff) + 1, int((v>>14)&0x3fff) + 1, nil
	case "VP8X":
		if len(data) < 10 {
			return 0, 0, errNotImage
		}
		u24 := func(x []byte) int { return int(x[0]) | int(x[1])<<8 | int(x[2])<<16 }
		return u24(data[4:7]) + 1, u24(data[7:10]) + 1, nil
	}
	return 0, 0, errNotImage
}
