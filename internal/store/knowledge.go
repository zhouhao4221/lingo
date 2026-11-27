package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/knowledge"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// sqlRunner 是 *sql.DB 与 *sql.Tx 的公共方法。
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const knowledgeColumns = `k.id, p.key, COALESCE(r.owner || '/' || r.name, ''), k.question, k.answer, k.source,
	COALESCE(k.source_ticket_id, 0), k.status, k.cite_count, k.created_by, k.updated_by, k.created_at, k.updated_at,
	COALESCE(k.duplicate_of, 0), k.helpful_count, k.unhelpful_count, ` + knowledgeModelExpr

// knowledgeModelExpr 是条目可作为模型输入（knowledge.Entry.ModelInput，REQ-012）：人工在灵工中填写的来源，或 model_ok 为
// true 的派生条目；升级前生成的派生条目（NULL，来源未知）与不可信的（false）不准入。
const knowledgeModelExpr = `(k.source IN ('manual', 'ticket_answer', 'ticket_comment') OR k.model_ok IS TRUE)`

const knowledgeFrom = ` FROM lingo_knowledge k JOIN lingo_projects p ON p.id = k.project_id
	LEFT JOIN lingo_repos r ON r.id = k.repo_id`

func scanKnowledge(sc interface{ Scan(...any) error }, extra ...any) (*knowledge.Entry, error) {
	e := &knowledge.Entry{}
	err := sc.Scan(append([]any{&e.ID, &e.ProjectKey, &e.Repo, &e.Question, &e.Answer, &e.Source, &e.SourceTicketID, &e.Status,
		&e.CiteCount, &e.CreatedBy, &e.UpdatedBy, &e.CreatedAt, &e.UpdatedAt, &e.DuplicateOf, &e.HelpfulCount, &e.UnhelpfulCount,
		&e.ModelInput},
		extra...)...)
	return e, err
}

// knowledgeRepo 解析项目与可选仓库的 ID；项目或仓库不存在时返回 ErrInvalid。
func knowledgeRepo(ctx context.Context, q sqlRunner, project, repo string) (int64, sql.NullInt64, error) {
	var pid int64
	if err := q.QueryRowContext(ctx, "SELECT id FROM lingo_projects WHERE key = $1", project).Scan(&pid); errors.Is(err, sql.ErrNoRows) {
		return 0, sql.NullInt64{}, fmt.Errorf("%w: project %q", ticket.ErrInvalid, project)
	} else if err != nil {
		return 0, sql.NullInt64{}, err
	}
	if repo == "" {
		return pid, sql.NullInt64{}, nil
	}
	var rid int64
	err := q.QueryRowContext(ctx, "SELECT id FROM lingo_repos WHERE project_id = $1 AND lower(owner || '/' || name) = lower($2)",
		pid, repo).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, sql.NullInt64{}, fmt.Errorf("%w: repo %q not in project %q", ticket.ErrInvalid, repo, project)
	}
	return pid, sql.NullInt64{Int64: rid, Valid: err == nil}, err
}

func insertKnowledge(ctx context.Context, q sqlRunner, e knowledge.NewEntry) (int64, error) {
	pid, rid, err := knowledgeRepo(ctx, q, e.ProjectKey, e.Repo)
	if err != nil {
		return 0, err
	}
	status := e.Status
	if status == "" {
		status = knowledge.StatusActive
	}
	var id int64
	err = q.QueryRowContext(ctx, `INSERT INTO lingo_knowledge (project_id, repo_id, question, answer, source, source_ticket_id,
		search_tokens, created_by, updated_by, status, source_message_id, duplicate_of, model_ok)
		VALUES ($1, $2, $3, $4, $5, $6, ($7::text)::text[], $8, $8, $9, $10, $11, $12)
		ON CONFLICT DO NOTHING RETURNING id`,
		pid, rid, e.Question, e.Answer, e.Source, nullInt64(e.SourceTicketID), textArray(e.Tokens), e.By, status,
		nullInt64(e.SourceMessageID), nullInt64(e.DuplicateOf), nullBool(e.ModelOK)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// 同一回答（source_message_id）或同一工单的抽取候选已存在
		return 0, fmt.Errorf("%w: knowledge from this source already exists", ticket.ErrConflict)
	}
	if err != nil {
		return 0, fmt.Errorf("store: insert knowledge: %w", err)
	}
	return id, nil
}

// InsertKnowledge 实现 knowledge.Store。
func (s *Store) InsertKnowledge(ctx context.Context, e knowledge.NewEntry) (int64, error) {
	return insertKnowledge(ctx, s.db, e)
}

// InsertKnowledge 在工单事务内入库（答复工单时勾选入库，实现 knowledge.Writer）。
func (x *ticketTx) InsertKnowledge(ctx context.Context, e knowledge.NewEntry) (int64, error) {
	return insertKnowledge(ctx, x.tx, e)
}

// GetKnowledge 实现 knowledge.Store；不存在时返回 ticket.ErrNotFound。
func (s *Store) GetKnowledge(ctx context.Context, id int64) (*knowledge.Entry, error) {
	e, err := scanKnowledge(s.db.QueryRowContext(ctx, "SELECT "+knowledgeColumns+knowledgeFrom+" WHERE k.id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	return e, err
}

func (s *Store) knowledgeList(ctx context.Context, query string, args ...any) ([]*knowledge.Entry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list knowledge: %w", err)
	}
	defer rows.Close()
	var out []*knowledge.Entry
	for rows.Next() {
		e, err := scanKnowledge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// KnowledgeByIDs 实现 knowledge.Store。
func (s *Store) KnowledgeByIDs(ctx context.Context, ids []int64) ([]*knowledge.Entry, error) {
	return s.knowledgeList(ctx, "SELECT "+knowledgeColumns+knowledgeFrom+" WHERE k.id = ANY(($1::text)::bigint[])", textArrayInt(ids))
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ticket.ErrNotFound
	}
	return nil
}

// UpdateKnowledge 实现 knowledge.Store。
func (s *Store) UpdateKnowledge(ctx context.Context, id int64, d knowledge.Draft, tokens []string, by string) error {
	if err := s.db.QueryRowContext(ctx, "SELECT p.key FROM lingo_knowledge k JOIN lingo_projects p ON p.id = k.project_id WHERE k.id = $1",
		id).Scan(&d.ProjectKey); errors.Is(err, sql.ErrNoRows) {
		return ticket.ErrNotFound
	} else if err != nil {
		return err
	}
	_, rid, err := knowledgeRepo(ctx, s.db, d.ProjectKey, d.Repo)
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE lingo_knowledge SET question = $2, answer = $3, repo_id = $4,
		search_tokens = ($5::text)::text[], embedding = NULL, embed_model = NULL, embed_locked_until = NULL, updated_by = $6,
		updated_at = now() WHERE id = $1`, id, d.Question, d.Answer, rid, textArray(tokens), by))
}

// SetKnowledgeStatus 实现 knowledge.Store。
func (s *Store) SetKnowledgeStatus(ctx context.Context, id int64, status, by string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE lingo_knowledge SET status = $2, updated_by = $3, updated_at = now()
		WHERE id = $1`, id, status, by))
}

// DeleteKnowledge 实现 knowledge.Store。
func (s *Store) DeleteKnowledge(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM lingo_knowledge WHERE id = $1", id))
}

// DisableKnowledgeByTicket 实现 knowledge.Store。
func (s *Store) DisableKnowledgeByTicket(ctx context.Context, ticketID int64, projects []string, by string) (int, error) {
	// 已生效的停用，待审核的候选驳回（避免之后被审核通过）
	res, err := s.db.ExecContext(ctx, `UPDATE lingo_knowledge k
		SET status = CASE k.status WHEN 'active' THEN 'disabled' ELSE 'rejected' END, updated_by = $3, updated_at = now()
		FROM lingo_projects p WHERE p.id = k.project_id AND k.source_ticket_id = $1 AND k.status IN ('active', 'pending')
		AND p.key = ANY(($2::text)::text[])`, ticketID, textArray(projects), by)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListKnowledge 实现 knowledge.Store。
func (s *Store) ListKnowledge(ctx context.Context, q knowledge.ListQuery) ([]*knowledge.Entry, error) {
	order := "k.updated_at DESC, k.id DESC"
	if q.Sort == knowledge.SortCites {
		order = "k.cite_count DESC, k.id DESC"
	}
	return s.knowledgeList(ctx, "SELECT "+knowledgeColumns+knowledgeFrom+`
		WHERE p.key = ANY(($1::text)::text[]) AND ($2 = '' OR p.key = $2) AND ($3 = '' OR k.status = $3)
		ORDER BY `+order+" OFFSET $4 LIMIT $5", textArray(q.Projects), q.Project, q.Status, q.Offset, q.Limit)
}

func (s *Store) knowledgeHits(ctx context.Context, query string, args ...any) ([]knowledge.Hit, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search knowledge: %w", err)
	}
	defer rows.Close()
	var out []knowledge.Hit
	for rows.Next() {
		var score float64
		e, err := scanKnowledge(rows, &score)
		if err != nil {
			return nil, err
		}
		out = append(out, knowledge.Hit{Entry: e, Score: score})
	}
	return out, rows.Err()
}

// SearchKnowledgeTokens 实现 knowledge.Store：重合词数不少于 min(提问词数, 条目词数) × minRatio 的条目，按重合度 =
// 重合词数 / sqrt(条目词数) 降序，同分时新的在前。
func (s *Store) SearchKnowledgeTokens(ctx context.Context, projects []string, project string, tokens []string, minRatio float64, limit int, modelOnly bool) ([]knowledge.Hit, error) {
	return s.knowledgeHits(ctx, "SELECT "+knowledgeColumns+`, o.n::float8 / sqrt(GREATEST(cardinality(k.search_tokens), 1)) AS score`+
		knowledgeFrom+`
		CROSS JOIN LATERAL (SELECT count(*) AS n FROM unnest(k.search_tokens) t WHERE t = ANY(($3::text)::text[])) o
		WHERE k.status = 'active' AND k.search_tokens && ($3::text)::text[]
		  AND o.n >= GREATEST(1, ceil($5::float8 * LEAST($6::int, cardinality(k.search_tokens))))
		  AND p.key = ANY(($1::text)::text[]) AND ($2 = '' OR p.key = $2) AND (NOT $7 OR `+knowledgeModelExpr+`)
		ORDER BY score DESC, k.updated_at DESC LIMIT $4`, textArray(projects), project, textArray(tokens), limit, minRatio, len(tokens), modelOnly)
}

// SearchKnowledgeWords 实现 knowledge.Store。
func (s *Store) SearchKnowledgeWords(ctx context.Context, projects []string, project string, words []string, limit int, modelOnly bool) ([]knowledge.Hit, error) {
	pats := make([]string, len(words))
	for i, w := range words {
		pats[i] = "%" + likeEscape(w) + "%"
	}
	return s.knowledgeHits(ctx, "SELECT "+knowledgeColumns+`,
		(SELECT count(*) FROM unnest(($3::text)::text[]) w WHERE k.question ILIKE w ESCAPE '\' OR k.answer ILIKE w ESCAPE '\')::float8 AS score`+
		knowledgeFrom+`
		WHERE k.status = 'active' AND p.key = ANY(($1::text)::text[]) AND ($2 = '' OR p.key = $2) AND (NOT $5 OR `+knowledgeModelExpr+`)
		  AND EXISTS (SELECT 1 FROM unnest(($3::text)::text[]) w WHERE k.question ILIKE w ESCAPE '\' OR k.answer ILIKE w ESCAPE '\')
		ORDER BY score DESC, k.updated_at DESC LIMIT $4`, textArray(projects), project, textArray(pats), limit, modelOnly)
}

// TicketExtracts 实现 knowledge.Store。
func (s *Store) TicketExtracts(ctx context.Context, ticketID int64) ([]*knowledge.Entry, error) {
	return s.knowledgeList(ctx, "SELECT "+knowledgeColumns+knowledgeFrom+` WHERE k.source_ticket_id = $1 AND k.source = 'ticket_extract'
		ORDER BY k.id`, ticketID)
}

// KnowledgeEmbeddings 实现 knowledge.Store。
func (s *Store) KnowledgeEmbeddings(ctx context.Context, ids []int64, model string) (map[int64][]float32, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, embedding::text FROM lingo_knowledge
		WHERE id = ANY(($1::text)::bigint[]) AND embed_model = $2 AND embedding IS NOT NULL`, textArrayInt(ids), model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]float32{}
	for rows.Next() {
		var id int64
		var txt string
		if err := rows.Scan(&id, &txt); err != nil {
			return nil, err
		}
		if v, err := parseRealArray(txt); err == nil {
			out[id] = v
		}
	}
	return out, rows.Err()
}

// AddKnowledgeCites 实现 knowledge.Store。
func (s *Store) AddKnowledgeCites(ctx context.Context, ids []int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE lingo_knowledge SET cite_count = cite_count + 1 WHERE id = ANY(($1::text)::bigint[])",
		textArrayInt(ids))
	return err
}

// ClaimKnowledgeEmbeds 实现 knowledge.Store。
func (s *Store) ClaimKnowledgeEmbeds(ctx context.Context, model string, limit int, lease time.Duration, modelOnly bool) ([]knowledge.Pending, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE lingo_knowledge k SET embed_locked_until = now() + $3 * interval '1 millisecond'
		FROM (SELECT k.id FROM lingo_knowledge k
		      WHERE k.status = 'active' AND (k.embedding IS NULL OR ($1 <> '' AND k.embed_model IS DISTINCT FROM $1))
		        AND (k.embed_locked_until IS NULL OR k.embed_locked_until < now()) AND (NOT $4 OR `+knowledgeModelExpr+`)
		      ORDER BY k.id LIMIT $2 FOR UPDATE OF k SKIP LOCKED) c
		WHERE k.id = c.id RETURNING k.id, k.question || E'\n' || k.answer, k.updated_at`, model, limit, lease.Milliseconds(), modelOnly)
	if err != nil {
		return nil, fmt.Errorf("store: claim knowledge embeds: %w", err)
	}
	defer rows.Close()
	var out []knowledge.Pending
	for rows.Next() {
		var p knowledge.Pending
		if err := rows.Scan(&p.ID, &p.Text, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveKnowledgeEmbedding 实现 knowledge.Store。
func (s *Store) SaveKnowledgeEmbedding(ctx context.Context, id int64, updatedAt time.Time, vec []float32, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE lingo_knowledge SET embedding = ($3::text)::real[], embed_model = $4, embed_locked_until = NULL
		WHERE id = $1 AND updated_at = $2`, id, updatedAt, realArray(vec), model)
	return err
}

func realArray(v []float32) string {
	ss := make([]string, len(v))
	for i, f := range v {
		ss[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
	}
	return "{" + strings.Join(ss, ",") + "}"
}

func parseRealArray(s string) ([]float32, error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, err
		}
		out[i] = float32(f)
	}
	return out, nil
}

// TransitionKnowledge 实现 knowledge.Store。
func (s *Store) TransitionKnowledge(ctx context.Context, id int64, from, to string, d *knowledge.Draft, tokens []string, by string) error {
	var res sql.Result
	var err error
	if d == nil {
		res, err = s.db.ExecContext(ctx, `UPDATE lingo_knowledge SET status = $3, updated_by = $4, updated_at = now()
			WHERE id = $1 AND status = $2`, id, from, to, by)
	} else {
		var key string
		if err := s.db.QueryRowContext(ctx, "SELECT p.key FROM lingo_knowledge k JOIN lingo_projects p ON p.id = k.project_id WHERE k.id = $1",
			id).Scan(&key); errors.Is(err, sql.ErrNoRows) {
			return ticket.ErrNotFound
		} else if err != nil {
			return err
		}
		_, rid, rerr := knowledgeRepo(ctx, s.db, key, d.Repo)
		if rerr != nil {
			return rerr
		}
		res, err = s.db.ExecContext(ctx, `UPDATE lingo_knowledge SET status = $3, updated_by = $4, updated_at = now(),
			question = $5, answer = $6, repo_id = $7, search_tokens = ($8::text)::text[], embedding = NULL, embed_model = NULL,
			embed_locked_until = NULL WHERE id = $1 AND status = $2`, id, from, to, by, d.Question, d.Answer, rid, textArray(tokens))
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var st string
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM lingo_knowledge WHERE id = $1", id).Scan(&st); errors.Is(err, sql.ErrNoRows) {
		return ticket.ErrNotFound
	} else if err != nil {
		return err
	}
	return fmt.Errorf("%w: knowledge %d is %s, not %s", ticket.ErrConflict, id, st, from)
}

// SimilarKnowledge 实现 knowledge.Store。
func (s *Store) SimilarKnowledge(ctx context.Context, project string, tokens []string) (int64, float64, error) {
	var id int64
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT k.id, (SELECT count(*) FROM unnest(k.search_tokens) t WHERE t = ANY(($2::text)::text[]))
		FROM lingo_knowledge k JOIN lingo_projects p ON p.id = k.project_id
		WHERE k.status = 'active' AND p.key = $1 AND k.search_tokens && ($2::text)::text[]
		ORDER BY 2 DESC, k.id LIMIT 1`, project, textArray(tokens)).Scan(&id, &n)
	if errors.Is(err, sql.ErrNoRows) || len(tokens) == 0 {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return id, float64(n) / float64(len(tokens)), nil
}

// RecordKnowledgeMiss 实现 knowledge.Store（同一事务内先锁候选再更新）。
func (s *Store) RecordKnowledgeMiss(ctx context.Context, project, question string, tokens []string, ratio float64, since time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	var id int64
	var n int
	err = tx.QueryRowContext(ctx, `SELECT id, (SELECT count(*) FROM unnest(m.tokens) t WHERE t = ANY(($2::text)::text[]))
		FROM lingo_knowledge_misses m
		WHERE m.project_key = $1 AND m.last_at >= $3 AND m.tokens && ($2::text)::text[]
		ORDER BY 2 DESC, m.last_at DESC LIMIT 1 FOR UPDATE`, project, textArray(tokens), since).Scan(&id, &n)
	switch {
	case err == nil && float64(n)/float64(len(tokens)) >= ratio:
		_, err = tx.ExecContext(ctx, "UPDATE lingo_knowledge_misses SET hits = hits + 1, last_at = now() WHERE id = $1", id)
	case err == nil || errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO lingo_knowledge_misses (project_key, question, tokens)
			VALUES ($1, $2, ($3::text)::text[])`, project, question, textArray(tokens))
	}
	if err != nil {
		return fmt.Errorf("store: record knowledge miss: %w", err)
	}
	return tx.Commit()
}

// ListKnowledgeMisses 实现 knowledge.Store。
func (s *Store) ListKnowledgeMisses(ctx context.Context, q knowledge.MissQuery) ([]knowledge.Miss, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, question, hits, first_at, last_at FROM lingo_knowledge_misses
		WHERE project_key = ANY(($1::text)::text[]) AND ($2 = '' OR project_key = $2) AND last_at >= $3
		ORDER BY hits DESC, last_at DESC, id DESC OFFSET $4 LIMIT $5`, textArray(q.Projects), q.Project, q.Since, q.Offset, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []knowledge.Miss
	for rows.Next() {
		var m knowledge.Miss
		if err := rows.Scan(&m.ID, &m.Question, &m.Hits, &m.FirstAt, &m.LastAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PurgeKnowledgeMisses 实现 knowledge.Store。
func (s *Store) PurgeKnowledgeMisses(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM lingo_knowledge_misses WHERE last_at < $1", before)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ClaimKnowledgeExtractions 实现 knowledge.Store。
func (s *Store) ClaimKnowledgeExtractions(ctx context.Context, limit int, lease time.Duration) ([]int64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO lingo_knowledge_extractions (ticket_id)
		SELECT t.id FROM lingo_tickets t WHERE t.type IN ('bug', 'feature') AND t.status IN ('released', 'deployed', 'closed')
		  AND NOT EXISTS (SELECT 1 FROM lingo_knowledge_extractions e WHERE e.ticket_id = t.id)
		ON CONFLICT (ticket_id) DO NOTHING`); err != nil {
		return nil, fmt.Errorf("store: enqueue knowledge extractions: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `UPDATE lingo_knowledge_extractions e SET locked_until = now() + $2 * interval '1 millisecond',
		updated_at = now()
		FROM (SELECT ticket_id FROM lingo_knowledge_extractions
		      WHERE status = 'pending' AND (locked_until IS NULL OR locked_until < now())
		      ORDER BY ticket_id LIMIT $1 FOR UPDATE SKIP LOCKED) c
		WHERE e.ticket_id = c.ticket_id RETURNING e.ticket_id`, limit, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim knowledge extractions: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FinishKnowledgeExtraction 实现 knowledge.Store。
func (s *Store) FinishKnowledgeExtraction(ctx context.Context, ticketID int64, status, errMsg string, maxAttempts int, retryAt time.Time) error {
	if status != "failed" {
		_, err := s.db.ExecContext(ctx, `UPDATE lingo_knowledge_extractions SET status = $2, last_error = '', locked_until = NULL,
			updated_at = now() WHERE ticket_id = $1`, ticketID, status)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE lingo_knowledge_extractions SET attempts = attempts + 1, last_error = left($2, 500),
		status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END, locked_until = $4, updated_at = now()
		WHERE ticket_id = $1`, ticketID, errMsg, maxAttempts, retryAt)
	return err
}
