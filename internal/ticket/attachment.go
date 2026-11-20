package ticket

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

// Attachment 是用户上传的附件（截图、日志、文件），存在数据库中。
type Attachment struct {
	ID          int64
	UserID      string // 上传人
	TicketID    int64  // 未关联工单时为 0
	CommentID   int64  // 随补充评论上传时为评论 ID
	Filename    string
	ContentType string
	Size        int64
	SHA256      []byte
	Data        []byte // 读取元数据时为空
	Forwarded   bool   // 已上传到 Gitea
	GiteaUUID   string
	CreatedAt   time.Time
}

// MaxAttachmentsPerTicket 是单个工单（含补充评论）的附件上限。
const MaxAttachmentsPerTicket = 10

// allowedTypes 是附件类型白名单：扩展名 → 保存的 Content-Type。图片按内容再校验一次。
var allowedTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf", ".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8", ".zip": "application/zip",
}

// AttachmentStore 是附件仓储，由 internal/store 实现。
type AttachmentStore interface {
	SaveAttachment(ctx context.Context, a *Attachment) error
	// GetAttachment 读取附件；withData 为假时不读内容。不存在返回 ErrNotFound。
	GetAttachment(ctx context.Context, id int64, withData bool) (*Attachment, error)
	// CommentPublic 报告评论是否对外（附件随评论上传时用于可见性判断）。
	CommentPublic(ctx context.Context, commentID int64) (bool, error)
	// DeleteOrphanAttachments 删除 before 之前上传、仍未关联工单的附件，返回删除条数。
	DeleteOrphanAttachments(ctx context.Context, before time.Time) (int, error)
}

// Upload 保存附件（尚未关联工单）。校验文件名、类型白名单与大小；图片按内容判定类型，扩展名不符时拒绝。
func (s *Service) Upload(ctx context.Context, p auth.Principal, filename string, data []byte, maxBytes int64) (*Attachment, error) {
	if !p.Known() || p.ID == "" {
		return nil, fmt.Errorf("%w: caller has no user id or role", ErrForbidden)
	}
	name := cleanFilename(filename)
	ext := strings.ToLower(filepath.Ext(name))
	ctype, ok := allowedTypes[ext]
	switch {
	case name == "":
		return nil, fmt.Errorf("%w: filename is required", ErrInvalid)
	case !ok:
		return nil, fmt.Errorf("%w: file type %q is not allowed (png, jpg, gif, webp, pdf, txt, log, zip)", ErrInvalid, ext)
	case len(data) == 0:
		return nil, fmt.Errorf("%w: file is empty", ErrInvalid)
	case int64(len(data)) > maxBytes:
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrInvalid, maxBytes)
	}
	if strings.HasPrefix(ctype, "image/") && http.DetectContentType(data) != ctype {
		return nil, fmt.Errorf("%w: file content is not %s", ErrInvalid, ctype)
	}
	sum := sha256.Sum256(data)
	a := &Attachment{UserID: p.ID, Filename: name, ContentType: ctype, Size: int64(len(data)), SHA256: sum[:], Data: data}
	if err := s.attachments().SaveAttachment(ctx, a); err != nil {
		return nil, err
	}
	a.Data = nil
	return a, nil
}

// cleanFilename 只保留文件名部分，去掉控制字符，最长 200 个字符。
func cleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "." || name == "/" {
		return ""
	}
	const max = 200
	if utf8.RuneCountInString(name) > max {
		ext := filepath.Ext(name)
		if n := utf8.RuneCountInString(ext); n < max/2 {
			name = string([]rune(strings.TrimSuffix(name, ext))[:max-n]) + ext
		} else {
			name = string([]rune(name)[:max]) // 扩展名异常长：直接截断，之后的白名单校验会拒绝
		}
	}
	return name
}

// Download 返回附件（含内容）：上传人本人，或能看到所属工单的人（随内部评论上传的附件 external 看不到；
// agent 只能看到已转出到 Gitea 的）。无权访问与不存在一样返回 ErrNotFound。先按元数据鉴权，再读内容。
func (s *Service) Download(ctx context.Context, p auth.Principal, id int64) (*Attachment, error) {
	a, err := s.attachments().GetAttachment(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if err := s.canDownload(ctx, p, a); err != nil {
		return nil, err
	}
	return s.attachments().GetAttachment(ctx, id, true)
}

func (s *Service) canDownload(ctx context.Context, p auth.Principal, a *Attachment) error {
	if p.ID != "" && a.UserID == p.ID && p.Known() {
		return nil
	}
	if a.TicketID == 0 {
		return ErrNotFound
	}
	if _, err := s.Get(ctx, p, a.TicketID); err != nil {
		return err
	}
	switch p.Role {
	case auth.RoleAgent:
		if !a.Forwarded {
			return ErrNotFound
		}
	case auth.RoleExternal:
		if a.CommentID != 0 {
			pub, err := s.attachments().CommentPublic(ctx, a.CommentID)
			if err != nil || !pub {
				return ErrNotFound
			}
		}
	}
	return nil
}

// linkAttachments 把调用者上传、尚未关联的附件关联到工单（与评论），并检查单工单上限。
func linkAttachments(ctx context.Context, tx Tx, p auth.Principal, ticketID, commentID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ids = dedupIDs(ids)
	n, err := tx.CountAttachments(ctx, ticketID)
	if err != nil {
		return err
	}
	if n+len(ids) > MaxAttachmentsPerTicket {
		return fmt.Errorf("%w: at most %d attachments per ticket", ErrInvalid, MaxAttachmentsPerTicket)
	}
	linked, err := tx.LinkAttachments(ctx, ids, p.ID, ticketID, commentID)
	if err != nil {
		return err
	}
	if linked != len(ids) {
		return fmt.Errorf("%w: attachments must be your own uploads not yet attached to a ticket", ErrInvalid)
	}
	return nil
}

func dedupIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// AddComment 写一条对提交人可见的评论：提交人本人补充、后台人员（reviewer / admin）答复，或研发 Agent
// 回复（agent 只能对已转出的工单回复）。工单已转出时由机器人同步为 Issue 评论（建单前的补充随 Issue 正文一起转出）。
// 提交人在 need_info 时补充，工单回到 in_progress。
func (s *Service) AddComment(ctx context.Context, p auth.Principal, id int64, body string, attachmentIDs []int64) (*Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" && len(attachmentIDs) == 0 {
		return nil, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	var c *Comment
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, id)
		if err != nil {
			return err
		}
		if !Visible(p, t) {
			return ErrNotFound
		}
		var source string
		switch {
		case p.Staff():
			source = "staff"
		case t.UserID == p.ID && p.Role == auth.RoleExternal:
			source = "user"
		case p.Role == auth.RoleAgent:
			source = "agent" // 研发 Agent 经 MCP 回复提交人：由机器人同步到 Issue
			if len(attachmentIDs) > 0 {
				return fmt.Errorf("%w: agents cannot attach files", ErrInvalid)
			}
		default:
			return ErrNotFound
		}
		if t.Status == StatusMerged {
			return fmt.Errorf("%w: ticket %d was merged into %d", ErrConflict, t.ID, t.MergedInto)
		}
		c = &Comment{TicketID: t.ID, Source: source, AuthorID: p.ID, AuthorName: p.Name, Body: body, Public: true, At: s.o.Now()}
		if c.ID, err = tx.AddComment(ctx, *c); err != nil {
			return err
		}
		if err := linkAttachments(ctx, tx, p, t.ID, c.ID, attachmentIDs); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "comment.added",
			Payload: map[string]any{"comment": c.ID, "attachments": len(attachmentIDs)}}); err != nil {
			return err
		}
		if t.Status.forwarded() && t.Status != StatusAbnormal {
			if err := tx.Enqueue(ctx, Task{Key: fmt.Sprintf("%s:comment:%d", OpCreateComment, c.ID), TicketID: t.ID,
				Op: OpCreateComment, Payload: map[string]any{"comment_id": c.ID}}); err != nil {
				return err
			}
		}
		e := s.event(t, notify.TicketComment, "", p.ID, body)
		if err := tx.Emit(ctx, e); err != nil {
			return err
		}
		if source == "user" && t.Status == StatusNeedInfo {
			prev := t.Status
			t.Status = StatusInProgress
			if err := tx.Update(ctx, t); err != nil {
				return err
			}
			return tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, p.ID, ""))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CleanupAttachments 删除上传超过 maxAge 仍未关联工单的附件。
func (s *Service) CleanupAttachments(ctx context.Context, maxAge time.Duration) (int, error) {
	return s.attachments().DeleteOrphanAttachments(ctx, s.o.Now().Add(-maxAge))
}

func (s *Service) attachments() AttachmentStore { return s.o.Attachments }
