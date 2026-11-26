package assistant

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 测试只用程序生成的合成图片。
func synthPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func synthJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// synthWebP 构造一个只有文件头的 VP8L（无损）WebP：宽 w、高 h。
func synthWebP(w, h int) []byte {
	v := uint32(w-1) | uint32(h-1)<<14
	data := []byte{0x2f, byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24), 0, 0, 0, 0, 0}
	b := []byte("RIFF\x00\x00\x00\x00WEBPVP8L")
	b = append(b, byte(len(data)), 0, 0, 0)
	return append(b, data...)
}

func TestCleanAttachments(t *testing.T) {
	got, err := CleanAttachments([]Attachment{{ID: " a1 ", Name: "../x/截图.png", ContentType: "image/PNG"}, {ID: "a2", ContentType: "image/jpg", Size: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "a1" || got[0].Name != "截图.png" || got[0].ContentType != ImagePNG || got[1].ContentType != ImageJPEG || got[1].Name != "a2" {
		t.Errorf("cleaned = %+v", got)
	}
	for name, in := range map[string][]Attachment{
		"too many":   {{ID: "1", ContentType: ImagePNG}, {ID: "2", ContentType: ImagePNG}, {ID: "3", ContentType: ImagePNG}, {ID: "4", ContentType: ImagePNG}, {ID: "5", ContentType: ImagePNG}},
		"no type":    {{ID: "1"}},
		"not image":  {{ID: "1", ContentType: "application/pdf"}},
		"gif":        {{ID: "1", ContentType: "image/gif"}},
		"svg":        {{ID: "1", ContentType: "image/svg+xml"}},
		"no id":      {{ContentType: ImagePNG}},
		"duplicate":  {{ID: "1", ContentType: ImagePNG}, {ID: "1", ContentType: ImagePNG}},
		"too large":  {{ID: "1", ContentType: ImagePNG, Size: MaxImageBytes + 1}},
		"negative":   {{ID: "1", ContentType: ImagePNG, Size: -1}},
		"control id": {{ID: "a\nb", ContentType: ImagePNG}},
	} {
		if _, err := CleanAttachments(in); !errors.Is(err, ticket.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if got, err := CleanAttachments(nil); got != nil || err != nil {
		t.Errorf("nil: %v %v", got, err)
	}
}

func TestSniffImage(t *testing.T) {
	for name, c := range map[string]struct {
		data []byte
		ct   string
		w, h int
	}{
		"png":  {synthPNG(t, 4, 3), ImagePNG, 4, 3},
		"jpeg": {synthJPEG(t), ImageJPEG, 3, 2},
		"webp": {synthWebP(640, 480), ImageWebP, 640, 480},
	} {
		ct, w, h, err := sniffImage(c.data)
		if err != nil || ct != c.ct || w != c.w || h != c.h {
			t.Errorf("%s: %s %dx%d %v", name, ct, w, h, err)
		}
	}
	for name, b := range map[string][]byte{
		"text":          []byte("hello, not an image at all"),
		"svg":           []byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"),
		"truncated png": synthPNG(t, 2, 2)[:12],
		"riff wav":      []byte("RIFF\x00\x00\x00\x00WAVEfmt \x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		"bad webp":      append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 20)...),
	} {
		if _, _, _, err := sniffImage(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type fetchStub struct {
	data map[string][]byte
	err  error
	got  []string // userID/conv/msg/id
}

func (f *fetchStub) fetch(_ context.Context, userID string, conv, msg int64, a Attachment) (io.ReadCloser, error) {
	f.got = append(f.got, strings.Join([]string{userID, itoa(conv), itoa(msg), a.ID}, "/"))
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.data[a.ID])), nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLoadImages(t *testing.T) {
	ok := synthPNG(t, 8, 8)
	big := append(append([]byte{}, ok...), make([]byte, MaxImageBytes)...)
	f := &fetchStub{data: map[string][]byte{"ok": ok, "jpg": synthJPEG(t), "big": big, "wide": synthPNG(t, MaxImageSide+1, 1),
		"text": []byte("plain text")}}
	s := &Service{o: Options{FetchImage: f.fetch}}
	msg := func(as ...Attachment) *Message { return &Message{ID: 7, ConversationID: 3, Attachments: as} }

	imgs, err := s.loadImages(context.Background(), "u1", msg(Attachment{ID: "ok", ContentType: ImagePNG}, Attachment{ID: "jpg", ContentType: ImageJPEG}))
	if err != nil || len(imgs) != 2 || imgs[0].ContentType != ImagePNG || !bytes.Equal(imgs[0].Data, ok) || imgs[1].ContentType != ImageJPEG {
		t.Fatalf("load: %v %d", err, len(imgs))
	}
	if f.got[0] != "u1/3/7/ok" {
		t.Errorf("fetch called with %v", f.got)
	}
	for name, a := range map[string]Attachment{
		"mismatch":  {ID: "jpg", ContentType: ImagePNG},
		"too large": {ID: "big", ContentType: ImagePNG},
		"too wide":  {ID: "wide", ContentType: ImagePNG},
		"not image": {ID: "text", ContentType: ImagePNG},
	} {
		_, err := s.loadImages(context.Background(), "u1", msg(a))
		var ie *imageError
		if !errors.As(err, &ie) || ie.kind != ErrKindAttachmentRejected || errKind(context.Background(), err) != ErrKindAttachmentRejected {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	f.err = ErrImageDenied
	if _, err := s.loadImages(context.Background(), "u1", msg(Attachment{ID: "ok", ContentType: ImagePNG})); errKind(context.Background(), err) != ErrKindAttachmentRejected {
		t.Errorf("denied: %v", err)
	}
	f.err = errors.New("storage down")
	_, err = s.loadImages(context.Background(), "u1", msg(Attachment{ID: "ok", ContentType: ImagePNG}))
	if errKind(context.Background(), err) != ErrKindAttachmentError || strings.Contains(err.Error(), "storage down") {
		t.Errorf("transient: %v", err) // 接入方的错误原文不进入消息
	}
	if imgs, err := s.loadImages(context.Background(), "u1", msg()); imgs != nil || err != nil {
		t.Errorf("no attachments: %v %v", imgs, err)
	}
	if _, err := (&Service{}).loadImages(context.Background(), "u1", msg(Attachment{ID: "ok", ContentType: ImagePNG})); !errors.Is(err, ErrImagesUnsupported) {
		t.Errorf("no fetcher: %v", err)
	}
}

// blockingFetch 返回一个不理会 ctx 的阻塞读取器（io.Pipe 的读端，写端永不写入），只有 Close 能让 Read 返回。
func blockingFetch(context.Context, string, int64, int64, Attachment) (io.ReadCloser, error) {
	r, _ := io.Pipe()
	return r, nil
}

func TestLoadImageReadHonorsDeadline(t *testing.T) {
	s := &Service{o: Options{FetchImage: blockingFetch}}
	msg := &Message{ID: 7, ConversationID: 3, Attachments: []Attachment{{ID: "slow", ContentType: ImagePNG}}}
	within := func(name string, d time.Duration, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(d):
			t.Fatalf("%s: loadImages still blocked after %v", name, d)
		}
	}

	// 整轮时限：返回 ctx 错误，归为 timeout
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	within("turn deadline", 2*time.Second, func() {
		_, err := s.loadImages(ctx, "u1", msg)
		if !errors.Is(err, context.DeadlineExceeded) || errKind(ctx, err) != ErrKindTimeout {
			t.Errorf("turn deadline: %v kind=%s", err, errKind(ctx, err))
		}
	})

	// 取消：归为 canceled
	cctx, ccancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, ccancel)
	within("cancel", 2*time.Second, func() {
		_, err := s.loadImages(cctx, "u1", msg)
		if errKind(cctx, err) != ErrKindCanceled {
			t.Errorf("cancel: %v kind=%s", err, errKind(cctx, err))
		}
	})

	// 只是单图时限到：可重试的 attachment_error
	defer func(d time.Duration) { imageFetchTime = d }(imageFetchTime)
	imageFetchTime = 200 * time.Millisecond
	within("image deadline", 2*time.Second, func() {
		_, err := s.loadImages(context.Background(), "u1", msg)
		var ie *imageError
		if !errors.As(err, &ie) || errKind(context.Background(), err) != ErrKindAttachmentError {
			t.Errorf("image deadline: %v kind=%s", err, errKind(context.Background(), err))
		}
	})
}
