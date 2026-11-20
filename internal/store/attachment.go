package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ ticket.AttachmentStore = (*Store)(nil)

// SaveAttachment 实现 ticket.AttachmentStore。
func (s *Store) SaveAttachment(ctx context.Context, a *ticket.Attachment) error {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO attachments (user_id, filename, content_type, size, sha256, data) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`, a.UserID, a.Filename, a.ContentType, a.Size, a.SHA256, a.Data).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: save attachment: %w", err)
	}
	return nil
}

const attachmentColumns = `id, user_id, COALESCE(ticket_id, 0), COALESCE(comment_id, 0), filename, content_type, size, sha256,
	forwarded, gitea_attachment_uuid, created_at`

func scanAttachment(row rowScanner, withData bool) (*ticket.Attachment, error) {
	var a ticket.Attachment
	dest := []any{&a.ID, &a.UserID, &a.TicketID, &a.CommentID, &a.Filename, &a.ContentType, &a.Size, &a.SHA256,
		&a.Forwarded, &a.GiteaUUID, &a.CreatedAt}
	if withData {
		dest = append(dest, &a.Data)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ticket.ErrNotFound
		}
		return nil, fmt.Errorf("store: read attachment: %w", err)
	}
	return &a, nil
}

// GetAttachment 实现 ticket.AttachmentStore。
func (s *Store) GetAttachment(ctx context.Context, id int64, withData bool) (*ticket.Attachment, error) {
	cols := attachmentColumns
	if withData {
		cols += ", data"
	}
	return scanAttachment(s.db.QueryRowContext(ctx, "SELECT "+cols+" FROM attachments WHERE id = $1", id), withData)
}

// CommentPublic 实现 ticket.AttachmentStore。
func (s *Store) CommentPublic(ctx context.Context, commentID int64) (bool, error) {
	var pub bool
	err := s.db.QueryRowContext(ctx, "SELECT public AND deleted_at IS NULL FROM ticket_comments WHERE id = $1", commentID).Scan(&pub)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: comment %d: %w", commentID, err)
	}
	return pub, nil
}

// DeleteOrphanAttachments 实现 ticket.AttachmentStore。
func (s *Store) DeleteOrphanAttachments(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM attachments WHERE ticket_id IS NULL AND created_at < $1", before)
	if err != nil {
		return 0, fmt.Errorf("store: delete orphan attachments: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TicketAttachments 实现 ticket.QueryStore。
func (s *Store) TicketAttachments(ctx context.Context, ticketID int64, publicOnly, forwardedOnly bool) ([]*ticket.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+attachmentColumnsA+` FROM attachments a
		LEFT JOIN ticket_comments c ON c.id = a.comment_id
		WHERE a.ticket_id = $1 AND (NOT $2 OR a.comment_id IS NULL OR (c.public AND c.deleted_at IS NULL))
		  AND (NOT $3 OR a.forwarded)
		ORDER BY a.id`, ticketID, publicOnly, forwardedOnly)
	if err != nil {
		return nil, fmt.Errorf("store: ticket attachments: %w", err)
	}
	defer rows.Close()
	var out []*ticket.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// attachmentColumnsA 同 attachmentColumns，带表别名 a。
const attachmentColumnsA = `a.id, a.user_id, COALESCE(a.ticket_id, 0), COALESCE(a.comment_id, 0), a.filename, a.content_type, a.size,
	a.sha256, a.forwarded, a.gitea_attachment_uuid, a.created_at`

// ForwardableAttachments 返回待上传到 Gitea 的附件（含内容）：未上传过、未被 Gitea 拒绝。commentID 为 0 时取
// 随 Issue 转出的附件：工单本身的附件，以及已写进 Issue 正文的补充评论的附件；否则取该评论的附件。
// ids 非 nil 时只取其中的（后台人员勾选的）。
func (s *Store) ForwardableAttachments(ctx context.Context, ticketID, commentID int64, ids *[]int64) ([]*ticket.Attachment, error) {
	all, filter := true, "{}"
	if ids != nil {
		all, filter = false, textArrayInt(*ids)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+attachmentColumnsA+`, a.data FROM attachments a
		LEFT JOIN ticket_comments c ON c.id = a.comment_id
		WHERE a.ticket_id = $1 AND NOT a.forwarded AND a.forward_error = ''
		  AND (($2 = 0 AND (a.comment_id IS NULL OR c.forwarded_at IS NOT NULL)) OR ($2 <> 0 AND a.comment_id = $2))
		  AND ($3 OR a.id = ANY(($4::text)::bigint[]))
		ORDER BY a.id`, ticketID, commentID, all, filter)
	if err != nil {
		return nil, fmt.Errorf("store: forwardable attachments: %w", err)
	}
	defer rows.Close()
	var out []*ticket.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttachmentLink 是已处理（上传成功或被拒）的附件，用于在正文中引用。
type AttachmentLink struct {
	Filename    string
	ContentType string
	URL         string // 被 Gitea 拒绝时为空
}

// AttachmentLinks 返回已处理的附件（范围同 ForwardableAttachments，不按勾选过滤），按 ID 顺序；
// 正文中的附件段落每次都按它整体重写，重试与部分失败后引用也不会丢。
func (s *Store) AttachmentLinks(ctx context.Context, ticketID, commentID int64) ([]AttachmentLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.filename, a.content_type, a.gitea_url FROM attachments a
		LEFT JOIN ticket_comments c ON c.id = a.comment_id
		WHERE a.ticket_id = $1 AND (a.forwarded OR a.forward_error <> '')
		  AND (($2 = 0 AND (a.comment_id IS NULL OR c.forwarded_at IS NOT NULL)) OR ($2 <> 0 AND a.comment_id = $2))
		ORDER BY a.id`, ticketID, commentID)
	if err != nil {
		return nil, fmt.Errorf("store: attachment links: %w", err)
	}
	defer rows.Close()
	var out []AttachmentLink
	for rows.Next() {
		var l AttachmentLink
		if err := rows.Scan(&l.Filename, &l.ContentType, &l.URL); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkAttachmentForwarded 记录附件已上传到 Gitea（uuid、下载地址），或被 Gitea 拒绝的原因（reason 非空，不再重试）。
func (s *Store) MarkAttachmentForwarded(ctx context.Context, id int64, uuid, url, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attachments SET forwarded = ($4 = ''), gitea_attachment_uuid = $2, gitea_url = $3,
		forward_error = $4 WHERE id = $1`, id, uuid, url, reason)
	if err != nil {
		return fmt.Errorf("store: mark attachment %d: %w", id, err)
	}
	return nil
}

// PendingComments 返回工单中尚未同步到 Gitea 的对外补充评论（用户与后台），按时间顺序。
func (s *Store) PendingComments(ctx context.Context, ticketID int64) ([]ticket.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ticket_id, source, author_id, author_name, body, public, created_at FROM ticket_comments
		WHERE ticket_id = $1 AND source IN ('user', 'staff') AND public AND gitea_comment_id IS NULL AND forwarded_at IS NULL
		  AND deleted_at IS NULL
		ORDER BY created_at, id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("store: pending comments: %w", err)
	}
	defer rows.Close()
	var out []ticket.Comment
	for rows.Next() {
		var c ticket.Comment
		if err := rows.Scan(&c.ID, &c.TicketID, &c.Source, &c.AuthorID, &c.AuthorName, &c.Body, &c.Public, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Comment 读取一条评论（含是否已同步）；不存在返回 ticket.ErrNotFound。
func (s *Store) Comment(ctx context.Context, id int64) (*ticket.Comment, error) {
	var c ticket.Comment
	var fwd sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, ticket_id, COALESCE(gitea_comment_id, 0), source, author_id, author_name, body, public,
		created_at, forwarded_at FROM ticket_comments WHERE id = $1 AND deleted_at IS NULL`, id).Scan(
		&c.ID, &c.TicketID, &c.GiteaID, &c.Source, &c.AuthorID, &c.AuthorName, &c.Body, &c.Public, &c.At, &fwd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: comment %d: %w", id, err)
	}
	c.Forwarded = fwd.Valid
	return &c, nil
}

// CommentPosted 记录评论已由机器人发到 Gitea。
func (s *Store) CommentPosted(ctx context.Context, id, giteaID int64) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE ticket_comments SET gitea_comment_id = $2 WHERE id = $1", id, giteaID); err != nil {
		return fmt.Errorf("store: comment posted %d: %w", id, err)
	}
	return nil
}

func (x *ticketTx) MarkCommentsForwarded(ctx context.Context, ticketID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := x.tx.ExecContext(ctx, `UPDATE ticket_comments SET forwarded_at = now()
		WHERE ticket_id = $1 AND id = ANY(($2::text)::bigint[])`, ticketID, textArrayInt(ids)); err != nil {
		return fmt.Errorf("store: mark comments forwarded: %w", err)
	}
	return nil
}

func (x *ticketTx) CountAttachments(ctx context.Context, ticketID int64) (int, error) {
	var n int
	if err := x.tx.QueryRowContext(ctx, "SELECT count(*) FROM attachments WHERE ticket_id = $1", ticketID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count attachments: %w", err)
	}
	return n, nil
}

func (x *ticketTx) LinkAttachments(ctx context.Context, ids []int64, userID string, ticketID, commentID int64) (int, error) {
	res, err := x.tx.ExecContext(ctx, `UPDATE attachments SET ticket_id = $3, comment_id = $4
		WHERE id = ANY(($1::text)::bigint[]) AND user_id = $2 AND ticket_id IS NULL`,
		textArrayInt(ids), userID, ticketID, nullInt64(commentID))
	if err != nil {
		return 0, fmt.Errorf("store: link attachments: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: link attachments: %w", err)
	}
	return int(n), nil
}
