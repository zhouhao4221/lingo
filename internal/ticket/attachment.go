package ticket

import (
	"context"
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

// Attachment 是工单或补充评论的附件引用。文件由接入方自己的附件系统保存，灵工只记录引用（ID、文件名、地址），
// 以及转出到 Gitea 的结果。随工单 / 评论一起存在 jsonb 列中。
type Attachment struct {
	ID          string `json:"id"`            // 接入方的附件 ID，在整个工单（含补充评论）内唯一
	Name        string `json:"name"`          // 文件名
	URL         string `json:"url,omitempty"` // http(s) 地址，转出时写进 Issue
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	// 以下为转出状态，由 worker 写入
	State    AttachmentState `json:"state,omitempty"`
	GiteaURL string          `json:"gitea_url,omitempty"` // 上传到 Gitea 后的下载地址
	Error    string          `json:"error,omitempty"`     // 上传失败的原因（不再重试）

	CommentID int64  `json:"-"` // 读取时回填：所属评论，工单本身的附件为 0
	UserID    string `json:"-"` // 读取时回填：提交该附件的用户（工单提交人或评论作者）
	TicketID  int64  `json:"-"` // 读取时回填：所属工单
}

// AttachmentState 是附件转出到 Gitea 的结果。
type AttachmentState string

const (
	AttachmentPending  AttachmentState = ""         // 尚未转出
	AttachmentUploaded AttachmentState = "uploaded" // 已上传为 Gitea 附件
	AttachmentLinked   AttachmentState = "linked"   // 未配置读取附件内容：只把地址写进 Issue
	AttachmentFailed   AttachmentState = "failed"   // 读取或上传失败：Issue 中写明，有地址时附上
)

// Forwarded 报告附件是否已转出（已上传、已写入链接或已写明失败），研发能在 Issue 中看到。
func (a Attachment) Forwarded() bool { return a.State != AttachmentPending }

// MaxAttachmentsPerTicket 是单个工单（含补充评论）的附件上限；附件 ID 在整个工单内唯一（后台人员按 ID 勾选转出）。
const MaxAttachmentsPerTicket = 10

const (
	maxAttachmentIDRunes = 200
	maxAttachmentURLLen  = 2048
)

// CleanAttachments 校验并规整调用方传入的附件引用：ID 必填且不重复；文件名只保留名称部分（为空时取 ID）；
// URL 可空，非空时须为 http(s) 绝对地址；Content-Type 不合法时丢弃。转出状态一律清空。
func CleanAttachments(in []Attachment) ([]Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAttachmentsPerTicket {
		return nil, fmt.Errorf("%w: at most %d attachments per ticket", ErrInvalid, MaxAttachmentsPerTicket)
	}
	seen := map[string]bool{}
	out := make([]Attachment, 0, len(in))
	for _, a := range in {
		id := strings.TrimSpace(a.ID)
		switch {
		case id == "":
			return nil, fmt.Errorf("%w: attachment id is required", ErrInvalid)
		case utf8.RuneCountInString(id) > maxAttachmentIDRunes || strings.IndexFunc(id, unicode.IsControl) >= 0:
			return nil, fmt.Errorf("%w: invalid attachment id %q", ErrInvalid, id)
		case seen[id]:
			return nil, fmt.Errorf("%w: duplicate attachment id %q", ErrInvalid, id)
		case a.Size < 0:
			return nil, fmt.Errorf("%w: attachment %q has negative size", ErrInvalid, id)
		}
		seen[id] = true
		u, err := cleanURL(a.URL)
		if err != nil {
			return nil, fmt.Errorf("%w: attachment %q: %v", ErrInvalid, id, err)
		}
		name := cleanFilename(a.Name)
		if name == "" {
			name = cleanFilename(id)
		}
		ctype := strings.TrimSpace(a.ContentType)
		if _, _, err := mime.ParseMediaType(ctype); err != nil || len(ctype) > 200 {
			ctype = ""
		}
		out = append(out, Attachment{ID: id, Name: name, URL: u, ContentType: ctype, Size: a.Size})
	}
	return out, nil
}

// cleanURL 只接受 http(s) 绝对地址（写进 Issue 的 Markdown 链接，不能含空白与尖括号）。
func cleanURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > maxAttachmentURLLen || strings.ContainsAny(s, "<>\"'") || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("invalid url")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be an absolute http(s) address")
	}
	return s, nil
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
			name = string([]rune(name)[:max])
		}
	}
	return name
}

// checkAttachments 检查工单（含补充评论）加上 add 后不超过上限，且附件 ID 不与已有的重复。
func checkAttachments(ctx context.Context, tx Tx, ticketID int64, add []Attachment) error {
	if len(add) == 0 {
		return nil
	}
	have, err := tx.AttachmentIDs(ctx, ticketID)
	if err != nil {
		return err
	}
	if len(have)+len(add) > MaxAttachmentsPerTicket {
		return fmt.Errorf("%w: at most %d attachments per ticket", ErrInvalid, MaxAttachmentsPerTicket)
	}
	for _, a := range add {
		if slices.Contains(have, a.ID) {
			return fmt.Errorf("%w: attachment id %q already used in this ticket", ErrInvalid, a.ID)
		}
	}
	return nil
}

// AddComment 写一条对提交人可见的评论：提交人本人补充、后台人员（reviewer / admin）答复，或研发 Agent
// 回复（agent 只能对已有 Gitea Issue 的工单回复）。工单已转出时由机器人同步为 Issue 评论（建单前的补充随 Issue 正文一起转出）。
// 提交人在 need_info 时补充，工单回到 in_progress。
func (s *Service) AddComment(ctx context.Context, p auth.Principal, id int64, body string, attachments []Attachment) (*Comment, error) {
	body = strings.TrimSpace(body)
	attachments, err := CleanAttachments(attachments)
	if err != nil {
		return nil, err
	}
	if body == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	var c *Comment
	err = s.o.Store.InTx(ctx, func(tx Tx) error {
		var err error
		c, err = s.addComment(ctx, tx, p, id, body, attachments)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// AddCommentWith 同 AddComment，另在同一事务内、评论写入后调用 after（如后台评论时勾选入库知识，REQ-007）；after 返回
// 错误时整体回滚。
func (s *Service) AddCommentWith(ctx context.Context, p auth.Principal, id int64, body string, attachments []Attachment,
	after func(ctx context.Context, tx Tx, t *Ticket, c *Comment) error) (*Comment, error) {
	body = strings.TrimSpace(body)
	attachments, err := CleanAttachments(attachments)
	if err != nil {
		return nil, err
	}
	if body == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	var c *Comment
	err = s.o.Store.InTx(ctx, func(tx Tx) error {
		var err error
		if c, err = s.addComment(ctx, tx, p, id, body, attachments); err != nil || after == nil {
			return err
		}
		t, err := tx.LockByID(ctx, id) // 已在 addComment 中锁定，同一事务内再取一次工单
		if err != nil {
			return err
		}
		return after(ctx, tx, t, c)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// AddCommentTx 在调用方的事务 tx 中执行 AddComment（校验、写入、同步与通知相同），供需要与其他写入同事务提交的
// 领域使用（如知识问答确认留言）。
func (s *Service) AddCommentTx(ctx context.Context, tx Tx, p auth.Principal, id int64, body string, attachments []Attachment) (*Comment, error) {
	body = strings.TrimSpace(body)
	attachments, err := CleanAttachments(attachments)
	if err != nil {
		return nil, err
	}
	if body == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	return s.addComment(ctx, tx, p, id, body, attachments)
}

func (s *Service) addComment(ctx context.Context, tx Tx, p auth.Principal, id int64, body string, attachments []Attachment) (*Comment, error) {
	var c *Comment
	err := func() error {
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
			if len(attachments) > 0 {
				return fmt.Errorf("%w: agents cannot attach files", ErrInvalid)
			}
			if t.IssueNumber == 0 || t.Status == StatusAbnormal {
				return fmt.Errorf("%w: ticket %d has no Gitea issue yet", ErrConflict, t.ID)
			}
		default:
			return ErrNotFound
		}
		if t.Status == StatusMerged {
			return fmt.Errorf("%w: ticket %d was merged into %d", ErrConflict, t.ID, t.MergedInto)
		}
		if err := checkAttachments(ctx, tx, t.ID, attachments); err != nil {
			return err
		}
		c = &Comment{TicketID: t.ID, Source: source, AuthorID: p.ID, AuthorName: p.Name, Body: body, Public: true, At: s.o.Now(),
			Attachments: attachments}
		if c.ID, err = tx.AddComment(ctx, *c); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "comment.added",
			Payload: map[string]any{"comment": c.ID, "attachments": len(attachments)}}); err != nil {
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
			if s.o.NeedInfoLabel != "" && t.IssueNumber != 0 {
				// 去掉 Issue 上的需补充标签：否则研发之后改其他标签时，工单会按仍在的标签被改回 need_info
				if err := tx.Enqueue(ctx, Task{Key: fmt.Sprintf("%s:comment:%d", OpRemoveLabel, c.ID), TicketID: t.ID, Op: OpRemoveLabel,
					Payload: map[string]any{"label": s.o.NeedInfoLabel}}); err != nil {
					return err
				}
			}
			return tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, p.ID, ""))
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return c, nil
}
