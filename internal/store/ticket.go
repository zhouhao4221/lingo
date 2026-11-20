package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ ticket.Store = (*Store)(nil)

// InTx 实现 ticket.Store。
func (s *Store) InTx(ctx context.Context, fn func(ticket.Tx) error) error {
	return s.inTx(ctx, func(x *ticketTx) error { return fn(x) })
}

// inTx 在一个事务中执行 fn，fn 返回错误时回滚；各领域的事务入口共用。
func (s *Store) inTx(ctx context.Context, fn func(*ticketTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	if err := fn(&ticketTx{tx: tx, notify: s.notify}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// HasIssue 实现 ticket.Store。
func (s *Store) HasIssue(ctx context.Context, repo string, number int64) (bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || number <= 0 {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM lingo_tickets t JOIN lingo_repos r ON r.id = t.repo_id
		WHERE lower(r.owner) = lower($1) AND lower(r.name) = lower($2) AND t.gitea_issue_number = $3)`, owner, name, number).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: has issue %s#%d: %w", repo, number, err)
	}
	return exists, nil
}

type ticketTx struct {
	tx     *sql.Tx
	notify bool
}

const ticketColumns = `t.id, t.project_id, p.key, COALESCE(t.repo_id, 0), COALESCE(r.owner || '/' || r.name, ''), t.type, t.title, t.title_auto,
	t.body, t.fields, t.context, t.reported_version, t.status, t.user_id, t.user_name, t.user_email, COALESCE(t.conversation_id, 0),
	COALESCE(t.gitea_issue_number, 0), COALESCE(t.merged_into_ticket_id, 0), t.triage_since, t.created_at, t.updated_at,
	t.gitea_updated_at, COALESCE(t.fixed_pr, 0), t.fixed_commit, t.milestone, t.due_date, t.deployed_at, COALESCE(rel.tag, ''),
	COALESCE(t.fixed_release_id, 0), t.fixed_repo
	FROM lingo_tickets t JOIN lingo_projects p ON p.id = t.project_id LEFT JOIN lingo_repos r ON r.id = t.repo_id
	LEFT JOIN lingo_releases rel ON rel.id = t.fixed_release_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanTicket(row rowScanner) (*ticket.Ticket, error) {
	var t ticket.Ticket
	var fields, tctx []byte
	var triage, updated, due, deployed sql.NullTime
	err := row.Scan(&t.ID, &t.ProjectID, &t.ProjectKey, &t.RepoID, &t.Repo, &t.Type, &t.Title, &t.TitleAuto,
		&t.Body, &fields, &tctx, &t.Version, &t.Status, &t.UserID, &t.UserName, &t.UserEmail, &t.Conversation,
		&t.IssueNumber, &t.MergedInto, &triage, &t.CreatedAt, &t.UpdatedAt,
		&updated, &t.FixedPR, &t.FixedCommit, &t.Milestone, &due, &deployed, &t.FixedTag, &t.FixedReleaseID, &t.FixedRepo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read ticket: %w", err)
	}
	if err := json.Unmarshal(fields, &t.Fields); err != nil {
		return nil, fmt.Errorf("store: ticket %d fields: %w", t.ID, err)
	}
	if err := json.Unmarshal(tctx, &t.Context); err != nil {
		return nil, fmt.Errorf("store: ticket %d context: %w", t.ID, err)
	}
	t.TriageSince, t.DueDate, t.DeployedAt = timePtr(triage), timePtr(due), timePtr(deployed)
	if updated.Valid {
		t.GiteaUpdatedAt = updated.Time
	}
	return &t, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// Get 实现 ticket.Store。
func (s *Store) Get(ctx context.Context, id int64) (*ticket.Ticket, error) {
	return scanTicket(s.db.QueryRowContext(ctx, "SELECT "+ticketColumns+" WHERE t.id = $1", id))
}

// GetMany 实现 ticket.Store。
func (s *Store) GetMany(ctx context.Context, ids []int64) ([]*ticket.Ticket, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.tickets(ctx, "SELECT "+ticketColumns+" WHERE t.id = ANY(($1::text)::bigint[]) ORDER BY t.id", textArrayInt(ids))
}

func (x *ticketTx) lock(ctx context.Context, where string, args ...any) (*ticket.Ticket, error) {
	return scanTicket(x.tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" WHERE "+where+" FOR UPDATE OF t", args...))
}

func (x *ticketTx) LockByIssue(ctx context.Context, repo string, number int64) (*ticket.Ticket, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || number <= 0 {
		return nil, ticket.ErrNotFound
	}
	return x.lock(ctx, "lower(r.owner) = lower($1) AND lower(r.name) = lower($2) AND t.gitea_issue_number = $3", owner, name, number)
}

func (x *ticketTx) LockByID(ctx context.Context, id int64) (*ticket.Ticket, error) {
	return x.lock(ctx, "t.id = $1", id)
}

func (x *ticketTx) Update(ctx context.Context, t *ticket.Ticket) error {
	var updated sql.NullTime
	if !t.GiteaUpdatedAt.IsZero() {
		updated = sql.NullTime{Time: t.GiteaUpdatedAt, Valid: true}
	}
	err := x.tx.QueryRowContext(ctx, `
		UPDATE lingo_tickets SET status = $2, gitea_issue_number = $3, gitea_updated_at = $4, fixed_pr = $5, fixed_commit = $6,
		    milestone = $7, due_date = $8, repo_id = $9, type = $10, title = $11, title_auto = $17, merged_into_ticket_id = $12,
		    triage_since = $13, fixed_release_id = $14, deployed_at = $15, fixed_repo = lower($16), updated_at = now()
		WHERE id = $1 RETURNING updated_at`,
		t.ID, string(t.Status), nullInt64(t.IssueNumber), updated, nullInt64(t.FixedPR), t.FixedCommit, t.Milestone, nullTime(t.DueDate),
		nullInt64(t.RepoID), t.Type, t.Title, nullInt64(t.MergedInto), nullTime(t.TriageSince), nullInt64(t.FixedReleaseID),
		nullTime(t.DeployedAt), t.FixedRepo, t.TitleAuto).Scan(&t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: update ticket %d: %w", t.ID, err)
	}
	return nil
}

func (x *ticketTx) Insert(ctx context.Context, t *ticket.Ticket) error {
	fields, err := json.Marshal(nonNilMap(t.Fields))
	if err != nil {
		return err
	}
	tctx, err := json.Marshal(nonNilMap(t.Context))
	if err != nil {
		return err
	}
	err = x.tx.QueryRowContext(ctx, `
		INSERT INTO lingo_tickets (user_id, user_name, user_email, conversation_id, project_id, repo_id, type, title, body, status,
		    context, fields, reported_version, triage_since, title_auto)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id, created_at, updated_at`,
		t.UserID, t.UserName, t.UserEmail, nullInt64(t.Conversation), t.ProjectID, nullInt64(t.RepoID), t.Type, t.Title, t.Body,
		string(t.Status), tctx, fields, t.Version, nullTime(t.TriageSince), t.TitleAuto).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: insert ticket: %w", err)
	}
	return nil
}

func nonNilMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

func (x *ticketTx) Project(ctx context.Context, key string) (*ticket.Project, error) {
	var p ticket.Project
	var repoID sql.NullInt64
	var repo sql.NullString
	err := x.tx.QueryRowContext(ctx, `
		SELECT p.id, p.key, r.id, r.owner || '/' || r.name FROM lingo_projects p
		LEFT JOIN lingo_repos r ON r.id = p.default_repo_id AND r.enabled
		WHERE p.key = $1 AND p.enabled`, key).Scan(&p.ID, &p.Key, &repoID, &repo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: project %q", ticket.ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("store: project %s: %w", key, err)
	}
	if repoID.Valid {
		p.DefaultRepo = &ticket.RepoRef{ID: repoID.Int64, Name: repo.String}
	}
	return &p, nil
}

func (x *ticketTx) Repo(ctx context.Context, projectID int64, name string) (*ticket.RepoRef, error) {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok {
		return nil, ticket.ErrNotFound
	}
	var r ticket.RepoRef
	err := x.tx.QueryRowContext(ctx, `SELECT id, owner || '/' || name FROM lingo_repos
		WHERE project_id = $1 AND lower(owner) = lower($2) AND lower(name) = lower($3) AND enabled`, projectID, owner, repo).Scan(&r.ID, &r.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: repo %s: %w", name, err)
	}
	return &r, nil
}

func (x *ticketTx) AutoForward(ctx context.Context, projectID int64, typ string) (on, ok bool, err error) {
	err = x.tx.QueryRowContext(ctx, "SELECT auto_forward FROM lingo_project_settings WHERE project_id = $1 AND ticket_type = $2",
		projectID, typ).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: auto forward: %w", err)
	}
	return on, true, nil
}

func (x *ticketTx) SetAutoForward(ctx context.Context, projectID int64, typ string, on *bool, by string) error {
	var err error
	if on == nil {
		_, err = x.tx.ExecContext(ctx, "DELETE FROM lingo_project_settings WHERE project_id = $1 AND ticket_type = $2", projectID, typ)
	} else {
		_, err = x.tx.ExecContext(ctx, `
			INSERT INTO lingo_project_settings (project_id, ticket_type, auto_forward, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (project_id, ticket_type) DO UPDATE SET auto_forward = EXCLUDED.auto_forward,
			    updated_by = EXCLUDED.updated_by, updated_at = now()`, projectID, typ, *on, by)
	}
	if err != nil {
		return fmt.Errorf("store: set auto forward: %w", err)
	}
	return nil
}

func (x *ticketTx) ConversationOwner(ctx context.Context, id int64) (string, error) {
	var owner string
	err := x.tx.QueryRowContext(ctx, "SELECT user_id FROM lingo_conversations WHERE id = $1", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ticket.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: conversation %d: %w", id, err)
	}
	return owner, nil
}

func (x *ticketTx) AddComment(ctx context.Context, c ticket.Comment) (int64, error) {
	at := c.At
	if at.IsZero() {
		at = time.Now()
	}
	var id int64
	if err := x.tx.QueryRowContext(ctx, `
		INSERT INTO lingo_ticket_comments (ticket_id, author_id, author_name, source, body, public, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING id`, c.TicketID, c.AuthorID, c.AuthorName, c.Source, c.Body, c.Public, at).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: add comment %d: %w", c.TicketID, err)
	}
	return id, nil
}

func (x *ticketTx) AddTriage(ctx context.Context, a ticket.Triage) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO lingo_triage_actions (ticket_id, actor_id, action, note, target_ticket_id)
		VALUES ($1, $2, $3, $4, $5)`, a.TicketID, a.ActorID, a.Action, a.Note, nullInt64(a.TargetID)); err != nil {
		return fmt.Errorf("store: add triage %d: %w", a.TicketID, err)
	}
	return nil
}

func (x *ticketTx) Enqueue(ctx context.Context, t ticket.Task) error {
	p := map[string]any{"op": t.Op}
	for k, v := range t.Payload {
		p[k] = v
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO lingo_outbox (kind, key, ticket_id, payload) VALUES ('gitea', $1, $2, $3)
		ON CONFLICT (key) DO NOTHING`, t.Key, nullInt64(t.TicketID), payload); err != nil {
		return fmt.Errorf("store: enqueue %s: %w", t.Key, err)
	}
	return nil
}

// RepointMerged 把合并到 from 的工单改为合并到 to（合并链只保留一层，跟随最终目标）。
func (x *ticketTx) RepointMerged(ctx context.Context, from, to int64) error {
	if _, err := x.tx.ExecContext(ctx, "UPDATE lingo_tickets SET merged_into_ticket_id = $2 WHERE merged_into_ticket_id = $1", from, to); err != nil {
		return fmt.Errorf("store: repoint merged %d: %w", from, err)
	}
	return nil
}

// PendingTask 报告工单是否有未完成的 op 任务。
func (x *ticketTx) PendingTask(ctx context.Context, ticketID int64, op string) (bool, error) {
	var ok bool
	err := x.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM lingo_outbox WHERE kind = 'gitea' AND status = 'pending'
		AND ticket_id = $1 AND payload->>'op' = $2)`, ticketID, op).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: pending task: %w", err)
	}
	return ok, nil
}

func (x *ticketTx) SeenPull(ctx context.Context, ticketID int64, kind, pull string) (bool, error) {
	var ok bool
	err := x.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM lingo_ticket_events WHERE ticket_id = $1 AND kind = $2
		AND lower(payload->>'pull') = lower($3))`, ticketID, kind, pull).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: seen pull: %w", err)
	}
	return ok, nil
}

func (x *ticketTx) AddHistory(ctx context.Context, h ticket.History) error {
	payload, err := jsonObject(h.Payload)
	if err != nil {
		return err
	}
	if _, err := x.tx.ExecContext(ctx, "INSERT INTO lingo_ticket_events (ticket_id, actor_id, source, kind, payload) VALUES ($1, $2, $3, $4, $5)",
		h.TicketID, h.ActorID, h.Source, h.Kind, payload); err != nil {
		return fmt.Errorf("store: add ticket event %d: %w", h.TicketID, err)
	}
	return nil
}

func (x *ticketTx) SaveGiteaComment(ctx context.Context, c ticket.Comment) (*ticket.Comment, error) {
	at, known := c.At, !c.At.IsZero()
	if !known {
		at = time.Now()
	}
	// 先读旧值再写；调用方已在事务内锁定工单，同一评论不会被并发写入
	var prev ticket.Comment
	var deleted sql.NullTime
	err := x.tx.QueryRowContext(ctx, "SELECT ticket_id, gitea_comment_id, author_name, body, public, updated_at, deleted_at FROM lingo_ticket_comments WHERE gitea_comment_id = $1",
		c.GiteaID).Scan(&prev.TicketID, &prev.GiteaID, &prev.AuthorName, &prev.Body, &prev.Public, &prev.At, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = x.tx.ExecContext(ctx, `
			INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, created_at, updated_at)
			VALUES ($1, $2, 'gitea', $3, $4, $5, $6, $6)`,
			c.TicketID, c.AuthorName, c.Body, c.Public, c.GiteaID, at)
		if err != nil {
			return nil, fmt.Errorf("store: insert comment %d: %w", c.GiteaID, err)
		}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read comment %d: %w", c.GiteaID, err)
	}
	if deleted.Valid || (known && at.Before(prev.At)) {
		// 已删除，或旧于已保存版本的乱序编辑：不写入，并按“可见性未变”返回，调用方不会因此发通知
		p := prev
		p.Public = p.Public || c.Public
		return &p, nil
	}
	if _, err := x.tx.ExecContext(ctx, "UPDATE lingo_ticket_comments SET body = $2, public = $3, author_name = $4, updated_at = $5 WHERE gitea_comment_id = $1",
		c.GiteaID, c.Body, c.Public, c.AuthorName, at); err != nil {
		return nil, fmt.Errorf("store: update comment %d: %w", c.GiteaID, err)
	}
	switch {
	case prev.Public && !c.Public:
		if err := x.redactCommentEvents(ctx, c.GiteaID); err != nil {
			return nil, err
		}
	case prev.Public && c.Public && prev.Body != c.Body:
		// 对外评论被编辑（如删掉误贴的密钥）：变更流随之返回新正文
		if _, err := x.tx.ExecContext(ctx, `UPDATE lingo_events SET payload = jsonb_set(payload, '{body}', to_jsonb($2::text))
			WHERE kind = 'ticket.comment' AND payload->>'comment_id' = $1`, strconv.FormatInt(c.GiteaID, 10), c.Body); err != nil {
			return nil, fmt.Errorf("store: update comment events %d: %w", c.GiteaID, err)
		}
	}
	return &prev, nil
}

func (x *ticketTx) DeleteGiteaComment(ctx context.Context, giteaID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := x.tx.ExecContext(ctx, "UPDATE lingo_ticket_comments SET deleted_at = $2 WHERE gitea_comment_id = $1 AND deleted_at IS NULL",
		giteaID, at); err != nil {
		return fmt.Errorf("store: delete comment %d: %w", giteaID, err)
	}
	return x.redactCommentEvents(ctx, giteaID)
}

// redactCommentEvents 清除该评论对应事件的正文：评论被删除或改为内部后，变更流不再返回原文
// （已推送给 Notifier 的无法撤回）。
func (x *ticketTx) redactCommentEvents(ctx context.Context, giteaID int64) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE lingo_events SET payload = payload - 'body'
		WHERE kind = 'ticket.comment' AND payload->>'comment_id' = $1`, strconv.FormatInt(giteaID, 10)); err != nil {
		return fmt.Errorf("store: redact comment events %d: %w", giteaID, err)
	}
	return nil
}

// eventPayload 是 lingo_events.payload 中不单独成列的字段。
type eventPayload struct {
	TicketType string `json:"ticket_type,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	URL        string `json:"url,omitempty"`
	CommentID  int64  `json:"comment_id,omitempty"`
}

// mergedFanout 是转发给被合并工单提交人的事件类型：目标工单的对外进展（不含后台事件与提交回执）。
var mergedFanout = map[notify.Kind]bool{notify.TicketStatus: true, notify.TicketComment: true, notify.TicketSchedule: true,
	notify.TicketReleased: true, notify.TicketDeployed: true}

func (x *ticketTx) Emit(ctx context.Context, e notify.Event) error {
	if err := x.emit(ctx, e); err != nil {
		return err
	}
	if e.TicketID == 0 || !mergedFanout[e.Kind] {
		return nil
	}
	if e.Kind == notify.TicketComment && e.ActorID != "" && e.ActorID == e.UserID {
		return nil // 目标工单提交人自己的补充属于其个人内容，不转给其他提交人
	}
	// 被合并到该工单的其他工单：其提交人一并收到目标工单的进展（PRD 3.2.1.2），事件挂在各自的工单上，
	// 按提交人隔离；正文注明来自合并到的工单
	rows, err := x.tx.QueryContext(ctx, "SELECT id, user_id FROM lingo_tickets WHERE merged_into_ticket_id = $1 AND status = 'merged' ORDER BY id", e.TicketID)
	if err != nil {
		return fmt.Errorf("store: merged tickets: %w", err)
	}
	type merged struct {
		id   int64
		user string
	}
	var ms []merged
	for rows.Next() {
		var m merged
		if err := rows.Scan(&m.id, &m.user); err != nil {
			rows.Close()
			return err
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range ms {
		// 副本挂在被合并的工单上：状态仍是 merged（与 GetTicket 一致），目标工单的进展写在正文里；
		// 保留 CommentID，评论被删除或改为内部时副本一并清除
		f := e
		f.TicketID, f.UserID, f.Status, f.PrevStatus = m.id, m.user, "merged", ""
		note := fmt.Sprintf("你的反馈已合并到工单 #%d", e.TicketID)
		if e.Kind == notify.TicketStatus {
			note += "，该工单状态变为 " + e.Status
		}
		if f.Body == "" {
			f.Body = note
		} else {
			f.Body = note + "：" + f.Body
		}
		if err := x.emit(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

func (x *ticketTx) emit(ctx context.Context, e notify.Event) error {
	payload, err := json.Marshal(eventPayload{TicketType: e.TicketType, Title: e.Title, Body: e.Body, URL: e.URL, CommentID: e.CommentID})
	if err != nil {
		return err
	}
	var id int64
	if err := x.tx.QueryRowContext(ctx, `
		INSERT INTO lingo_events (kind, project_id, ticket_id, user_id, actor_id, status, prev_status, action, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		string(e.Kind), nullInt64(e.ProjectID), nullInt64(e.TicketID), e.UserID, e.ActorID, e.Status, e.PrevStatus, e.Action, payload,
	).Scan(&id); err != nil {
		return fmt.Errorf("store: insert event: %w", err)
	}
	if !x.notify {
		return nil
	}
	if _, err := x.tx.ExecContext(ctx, "INSERT INTO lingo_outbox (kind, key, event_id) VALUES ('notify', $1, $2)",
		fmt.Sprintf("notify:event:%d", id), id); err != nil {
		return fmt.Errorf("store: enqueue notify %d: %w", id, err)
	}
	return nil
}

func jsonObject(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}
