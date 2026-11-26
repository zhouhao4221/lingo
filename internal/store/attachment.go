package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 附件引用存在 lingo_tickets.attachments（工单本身）与 lingo_ticket_comments.attachments（补充评论）两个 jsonb 列中，
// 元素为 ticket.Attachment；转出结果由 MarkAttachment 按附件 ID 原地更新。

func marshalAttachments(as []ticket.Attachment) ([]byte, error) {
	if as == nil {
		as = []ticket.Attachment{}
	}
	return json.Marshal(as)
}

// queryAttachments 列出工单的附件：工单本身的（CommentID = 0）与各补充评论的，按评论、序号排序，并回填提交人。
// commentID 非 0 时只取该评论的；publicOnly 时排除内部评论与已删除评论的附件。
func (s *Store) queryAttachments(ctx context.Context, ticketID, commentID int64, publicOnly bool) ([]scannedAttachment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT comment_id, user_id, a, in_body FROM (
			SELECT 0::bigint AS comment_id, t.user_id, x.ord, x.a, true AS in_body
			FROM lingo_tickets t, jsonb_array_elements(t.attachments) WITH ORDINALITY x(a, ord)
			WHERE t.id = $1 AND $2 = 0
			UNION ALL
			SELECT c.id, c.author_id, x.ord, x.a, c.forwarded_at IS NOT NULL
			FROM lingo_ticket_comments c, jsonb_array_elements(c.attachments) WITH ORDINALITY x(a, ord)
			WHERE c.ticket_id = $1 AND ($2 = 0 OR c.id = $2) AND (NOT $3 OR (c.public AND c.deleted_at IS NULL))
		) q ORDER BY comment_id, ord`, ticketID, commentID, publicOnly)
	if err != nil {
		return nil, fmt.Errorf("store: ticket attachments: %w", err)
	}
	defer rows.Close()
	var out []scannedAttachment
	for rows.Next() {
		var a scannedAttachment
		var raw []byte
		if err := rows.Scan(&a.CommentID, &a.UserID, &raw, &a.inBody); err != nil {
			return nil, err
		}
		cid, uid := a.CommentID, a.UserID
		if err := json.Unmarshal(raw, &a.Attachment); err != nil {
			return nil, fmt.Errorf("store: decode attachment: %w", err)
		}
		a.CommentID, a.UserID, a.TicketID = cid, uid, ticketID
		out = append(out, a)
	}
	return out, rows.Err()
}

type scannedAttachment struct {
	ticket.Attachment
	inBody bool // 工单本身的附件，或已随 Issue 正文转出的补充评论的附件
}

// TicketAttachments 实现 ticket.QueryStore。
func (s *Store) TicketAttachments(ctx context.Context, ticketID int64, publicOnly, forwardedOnly bool) ([]ticket.Attachment, error) {
	all, err := s.queryAttachments(ctx, ticketID, 0, publicOnly)
	if err != nil {
		return nil, err
	}
	out := []ticket.Attachment{}
	for _, a := range all {
		if !forwardedOnly || a.Forwarded() {
			out = append(out, a.Attachment)
		}
	}
	return out, nil
}

// ForwardAttachments 返回一次转出涉及的附件（含已处理的），按顺序：commentID 为 0 时为随 Issue 转出的附件——工单本身的，
// 以及已写进 Issue 正文的补充评论的；否则为该评论的附件。exclude 是后台人员转出时未勾选的附件 ID，尚未处理的跳过。
func (s *Store) ForwardAttachments(ctx context.Context, ticketID, commentID int64, exclude []string) ([]ticket.Attachment, error) {
	all, err := s.queryAttachments(ctx, ticketID, commentID, false)
	if err != nil {
		return nil, err
	}
	var out []ticket.Attachment
	for _, a := range all {
		if commentID == 0 && !a.inBody {
			continue
		}
		if !a.Forwarded() && slices.Contains(exclude, a.ID) {
			continue
		}
		out = append(out, a.Attachment)
	}
	return out, nil
}

// MarkAttachment 记录附件的转出结果（按所属工单 / 评论与附件 ID 原地更新）。不修改 updated_at：建单整理的缓存以它为键。
func (s *Store) MarkAttachment(ctx context.Context, ticketID int64, a ticket.Attachment) error {
	patch, err := json.Marshal(map[string]any{"state": a.State, "gitea_url": a.GiteaURL, "error": a.Error})
	if err != nil {
		return err
	}
	const update = ` SET attachments = (SELECT COALESCE(jsonb_agg(CASE WHEN x.a->>'id' = $2 THEN x.a || $3::jsonb ELSE x.a END ORDER BY x.ord), '[]')
		FROM jsonb_array_elements(attachments) WITH ORDINALITY x(a, ord))`
	const hasID = `attachments @> jsonb_build_array(jsonb_build_object('id', $2::text))` // 确实有这个附件，否则报错而不是静默成功
	var res sql.Result
	if a.CommentID == 0 {
		res, err = s.db.ExecContext(ctx, "UPDATE lingo_tickets"+update+" WHERE id = $1 AND "+hasID, ticketID, a.ID, patch)
	} else {
		res, err = s.db.ExecContext(ctx, "UPDATE lingo_ticket_comments"+update+" WHERE id = $1 AND ticket_id = $4 AND "+hasID,
			a.CommentID, a.ID, patch, ticketID)
	}
	if err != nil {
		return fmt.Errorf("store: mark attachment %s: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: mark attachment %s: %w", a.ID, ticket.ErrNotFound)
	}
	return nil
}

// PendingComments 返回工单中尚未同步到 Gitea 的对外补充评论（用户与后台），按时间顺序。
func (s *Store) PendingComments(ctx context.Context, ticketID int64) ([]ticket.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ticket_id, source, author_id, author_name, body, public, created_at FROM lingo_ticket_comments
		WHERE ticket_id = $1 AND source IN ('user', 'staff', 'agent') AND public AND gitea_posted_id IS NULL AND forwarded_at IS NULL
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
	err := s.db.QueryRowContext(ctx, `SELECT id, ticket_id, COALESCE(gitea_posted_id, 0), source, author_id, author_name, body, public,
		created_at, forwarded_at FROM lingo_ticket_comments WHERE id = $1 AND deleted_at IS NULL`, id).Scan(
		&c.ID, &c.TicketID, &c.PostedID, &c.Source, &c.AuthorID, &c.AuthorName, &c.Body, &c.Public, &c.At, &fwd)
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
	if _, err := s.db.ExecContext(ctx, "UPDATE lingo_ticket_comments SET gitea_posted_id = $2 WHERE id = $1", id, giteaID); err != nil {
		return fmt.Errorf("store: comment posted %d: %w", id, err)
	}
	return nil
}

func (x *ticketTx) MarkCommentsForwarded(ctx context.Context, ticketID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := x.tx.ExecContext(ctx, `UPDATE lingo_ticket_comments SET forwarded_at = now()
		WHERE ticket_id = $1 AND id = ANY(($2::text)::bigint[])`, ticketID, textArrayInt(ids)); err != nil {
		return fmt.Errorf("store: mark comments forwarded: %w", err)
	}
	return nil
}

func (x *ticketTx) AttachmentIDs(ctx context.Context, ticketID int64) ([]string, error) {
	rows, err := x.tx.QueryContext(ctx, `
		SELECT a->>'id' FROM lingo_tickets t, jsonb_array_elements(t.attachments) a WHERE t.id = $1
		UNION ALL
		SELECT a->>'id' FROM lingo_ticket_comments c, jsonb_array_elements(c.attachments) a WHERE c.ticket_id = $1`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("store: attachment ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
