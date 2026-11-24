package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/assistant"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ assistant.Store = (*Store)(nil)

type assistantTx struct{ *ticketTx }

// InAssistantTx 实现 assistant.Store。
func (s *Store) InAssistantTx(ctx context.Context, fn func(assistant.Tx) error) error {
	return s.inTx(ctx, func(x *ticketTx) error { return fn(assistantTx{x}) })
}

const conversationColumns = `id, user_id, title, activity_seq, last_active_at, created_at`

func scanConversation(row rowScanner) (*assistant.Conversation, error) {
	var c assistant.Conversation
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Seq, &c.LastActiveAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read conversation: %w", err)
	}
	return &c, nil
}

// CreateConversation 实现 assistant.Store。
func (s *Store) CreateConversation(ctx context.Context, userID, title string) (*assistant.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `INSERT INTO lingo_assistant_conversations (user_id, title)
		VALUES ($1, $2) RETURNING `+conversationColumns, userID, title))
}

// ListConversations 实现 assistant.Store。
func (s *Store) ListConversations(ctx context.Context, userID string, beforeSeq int64, limit int) ([]*assistant.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+` FROM lingo_assistant_conversations
		WHERE user_id = $1 AND deleted_at IS NULL AND ($2::bigint = 0 OR activity_seq < $2::bigint)
		ORDER BY activity_seq DESC LIMIT $3`, userID, beforeSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	defer rows.Close()
	var out []*assistant.Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversation 实现 assistant.Store。
func (s *Store) GetConversation(ctx context.Context, userID string, id int64) (*assistant.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM lingo_assistant_conversations
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID))
}

// DeleteConversation 实现 assistant.Store。
func (s *Store) DeleteConversation(ctx context.Context, userID string, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE lingo_assistant_conversations SET deleted_at = now()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID)
	if err != nil {
		return fmt.Errorf("store: delete conversation %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ticket.ErrNotFound
	}
	return nil
}

const messageColumns = `m.id, m.conversation_id, m.role, m.content, m.status, m.error, COALESCE(m.reply_to, 0), m.intent, m.outcome,
	m.citations, m.created_at, m.updated_at`

func scanMessage(row rowScanner) (*assistant.Message, error) {
	var m assistant.Message
	var cites []byte
	err := row.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status, &m.Error, &m.ReplyTo, &m.Intent, &m.Outcome,
		&cites, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read message: %w", err)
	}
	if err := json.Unmarshal(cites, &m.Citations); err != nil {
		return nil, fmt.Errorf("store: message %d citations: %w", m.ID, err)
	}
	return &m, nil
}

func (s *Store) messages(ctx context.Context, query string, args ...any) ([]*assistant.Message, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	var out []*assistant.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages 实现 assistant.Store。
func (s *Store) Messages(ctx context.Context, conversationID, beforeID int64, limit int) ([]*assistant.Message, error) {
	ms, err := s.messages(ctx, `SELECT * FROM (SELECT `+messageColumns+` FROM lingo_assistant_messages m
		WHERE m.conversation_id = $1 AND ($2::bigint = 0 OR m.id < $2::bigint) ORDER BY m.id DESC LIMIT $3) x ORDER BY id`,
		conversationID, beforeID, limit)
	if err != nil || len(ms) == 0 {
		return ms, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+actionColumns+` FROM lingo_assistant_actions a
		WHERE a.conversation_id = $1 AND a.message_id BETWEEN $2 AND $3`, conversationID, ms[0].ID, ms[len(ms)-1].ID)
	if err != nil {
		return nil, fmt.Errorf("store: message actions: %w", err)
	}
	defer rows.Close()
	byMsg := map[int64]*assistant.Message{}
	for _, m := range ms {
		byMsg[m.ID] = m
	}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		if m := byMsg[a.MessageID]; m != nil {
			m.Action = a
		}
	}
	return ms, rows.Err()
}

// RecentDone 实现 assistant.Store。
func (s *Store) RecentDone(ctx context.Context, conversationID, beforeID int64, n int) ([]*assistant.Message, error) {
	return s.messages(ctx, `SELECT * FROM (SELECT `+messageColumns+` FROM lingo_assistant_messages m
		WHERE m.conversation_id = $1 AND m.id < $2 AND m.status = 'done' ORDER BY m.id DESC LIMIT $3) x ORDER BY id`,
		conversationID, beforeID, n)
}

func (x assistantTx) ClaimTurn(ctx context.Context, userID string, id int64, hold time.Duration) (*assistant.Conversation, bool, error) {
	var c assistant.Conversation
	err := x.tx.QueryRowContext(ctx, `UPDATE lingo_assistant_conversations
		SET turn_until = now() + ($3::float8) * interval '1 millisecond'
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL AND (turn_until IS NULL OR turn_until < now())
		RETURNING `+conversationColumns+`, turn_until`, id, userID, hold.Milliseconds()).Scan(
		&c.ID, &c.UserID, &c.Title, &c.Seq, &c.LastActiveAt, &c.CreatedAt, &c.Lease)
	switch {
	case err == nil:
		return &c, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, fmt.Errorf("store: claim turn %d: %w", id, err)
	}
	var exists bool
	if err := x.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM lingo_assistant_conversations
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL)`, id, userID).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("store: claim turn %d: %w", id, err)
	}
	if !exists {
		return nil, false, ticket.ErrNotFound
	}
	return nil, false, nil
}

func (x assistantTx) ReleaseTurn(ctx context.Context, id int64, lease time.Time) error {
	if _, err := x.tx.ExecContext(ctx, "UPDATE lingo_assistant_conversations SET turn_until = NULL WHERE id = $1 AND turn_until = $2",
		id, lease); err != nil {
		return fmt.Errorf("store: release turn %d: %w", id, err)
	}
	return nil
}

func (x assistantTx) Touch(ctx context.Context, id int64, title string) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE lingo_assistant_conversations
		SET activity_seq = nextval('lingo_assistant_activity_seq'), last_active_at = now(),
		    title = CASE WHEN title = '' THEN $2 ELSE title END
		WHERE id = $1`, id, title); err != nil {
		return fmt.Errorf("store: touch conversation %d: %w", id, err)
	}
	return nil
}

func (x assistantTx) InsertMessage(ctx context.Context, m *assistant.Message) error {
	cites, err := json.Marshal(m.Citations)
	if err != nil {
		return err
	}
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO lingo_assistant_messages
		(conversation_id, role, content, status, error, reply_to, intent, outcome, citations)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at, updated_at`,
		m.ConversationID, m.Role, m.Content, m.Status, m.Error, nullInt64(m.ReplyTo), m.Intent, m.Outcome, cites).Scan(
		&m.ID, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return fmt.Errorf("store: insert message: %w", err)
	}
	return nil
}

func (x assistantTx) UpdateMessage(ctx context.Context, m *assistant.Message) error {
	cites, err := json.Marshal(m.Citations)
	if err != nil {
		return err
	}
	if err := x.tx.QueryRowContext(ctx, `UPDATE lingo_assistant_messages
		SET content = $2, status = $3, error = $4, intent = $5, outcome = $6, citations = $7, updated_at = now()
		WHERE id = $1 RETURNING updated_at`, m.ID, m.Content, m.Status, m.Error, m.Intent, m.Outcome, cites).Scan(&m.UpdatedAt); err != nil {
		return fmt.Errorf("store: update message %d: %w", m.ID, err)
	}
	return nil
}

func (x assistantTx) GetMessage(ctx context.Context, conversationID, id int64) (*assistant.Message, error) {
	return scanMessage(x.tx.QueryRowContext(ctx, "SELECT "+messageColumns+` FROM lingo_assistant_messages m
		WHERE m.id = $1 AND m.conversation_id = $2`, id, conversationID))
}

func (x assistantTx) LastUserMessageID(ctx context.Context, conversationID int64) (int64, error) {
	var id int64
	if err := x.tx.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) FROM lingo_assistant_messages
		WHERE conversation_id = $1 AND role = 'user'`,
		conversationID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: last message: %w", err)
	}
	return id, nil
}

const actionColumns = `a.id, a.conversation_id, a.message_id, a.user_id, a.kind, a.payload, a.summary, a.status, a.result,
	a.expires_at, a.resolved_at, a.created_at`

func scanAction(row rowScanner) (*assistant.Action, error) {
	var a assistant.Action
	var payload, result []byte
	var resolved sql.NullTime
	err := row.Scan(&a.ID, &a.ConversationID, &a.MessageID, &a.UserID, &a.Kind, &payload, &a.Summary, &a.Status, &result,
		&a.ExpiresAt, &resolved, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read action: %w", err)
	}
	a.Payload, a.Result, a.ResolvedAt = payload, result, timePtr(resolved)
	return &a, nil
}

func (x assistantTx) PendingAction(ctx context.Context, conversationID int64) (*assistant.Action, error) {
	a, err := scanAction(x.tx.QueryRowContext(ctx, "SELECT "+actionColumns+` FROM lingo_assistant_actions a
		WHERE a.conversation_id = $1 AND a.status = 'pending' FOR UPDATE`, conversationID))
	if errors.Is(err, ticket.ErrNotFound) {
		return nil, nil
	}
	return a, err
}

func (x assistantTx) LockAction(ctx context.Context, conversationID, id int64) (*assistant.Action, error) {
	return scanAction(x.tx.QueryRowContext(ctx, "SELECT "+actionColumns+` FROM lingo_assistant_actions a
		WHERE a.id = $1 AND a.conversation_id = $2 FOR UPDATE`, id, conversationID))
}

func (x assistantTx) InsertAction(ctx context.Context, a *assistant.Action) error {
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}
	if len(a.Result) == 0 {
		a.Result = json.RawMessage("{}")
	}
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO lingo_assistant_actions
		(conversation_id, message_id, user_id, kind, payload, summary, status, result, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at`,
		a.ConversationID, a.MessageID, a.UserID, a.Kind, []byte(a.Payload), a.Summary, a.Status, []byte(a.Result), a.ExpiresAt).Scan(
		&a.ID, &a.CreatedAt); err != nil {
		return fmt.Errorf("store: insert action: %w", err)
	}
	return nil
}

func (x assistantTx) ResolveAction(ctx context.Context, a *assistant.Action) error {
	if len(a.Result) == 0 {
		a.Result = json.RawMessage("{}")
	}
	if _, err := x.tx.ExecContext(ctx, `UPDATE lingo_assistant_actions SET status = $2, result = $3, resolved_at = $4 WHERE id = $1`,
		a.ID, a.Status, []byte(a.Result), nullTime(a.ResolvedAt)); err != nil {
		return fmt.Errorf("store: resolve action %d: %w", a.ID, err)
	}
	return nil
}
