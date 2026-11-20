package store

import (
	"context"
	"fmt"
	"time"
)

// LLMUsage 是一次模型调用的用量。
type LLMUsage struct {
	Kind     string // 用途，如 draft_issue
	Model    string
	TicketID int64
	Input    int
	Output   int
	Cached   int // 输入中命中服务端前缀缓存的部分
	OK       bool
	Latency  time.Duration
}

// RecordLLMUsage 写入一次模型调用的用量。
func (s *Store) RecordLLMUsage(ctx context.Context, u LLMUsage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_usage (kind, model, ticket_id, input, output, cached, ok, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, u.Kind, u.Model, nullInt64(u.TicketID), u.Input, u.Output, u.Cached, u.OK,
		u.Latency.Milliseconds())
	if err != nil {
		return fmt.Errorf("store: record llm usage: %w", err)
	}
	return nil
}

// LLMUsageSummary 是按天、用途、模型汇总的用量。
type LLMUsageSummary struct {
	Day      time.Time // UTC 零点
	Kind     string
	Model    string
	Calls    int
	Failed   int
	Input    int64
	Output   int64
	Cached   int64
	AvgMilli int64 // 平均耗时（毫秒）
}

// LLMUsageSummary 返回 [from, to) 内的用量，按天（UTC）、用途、模型汇总，按天升序。
func (s *Store) LLMUsageSummary(ctx context.Context, from, to time.Time) ([]LLMUsageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT date_trunc('day', created_at AT TIME ZONE 'UTC'), kind, model, count(*), count(*) FILTER (WHERE NOT ok),
		       coalesce(sum(input), 0), coalesce(sum(output), 0), coalesce(sum(cached), 0), coalesce(avg(latency_ms), 0)::bigint
		FROM llm_usage WHERE created_at >= $1 AND created_at < $2
		GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: llm usage summary: %w", err)
	}
	defer rows.Close()
	var out []LLMUsageSummary
	for rows.Next() {
		var u LLMUsageSummary
		if err := rows.Scan(&u.Day, &u.Kind, &u.Model, &u.Calls, &u.Failed, &u.Input, &u.Output, &u.Cached, &u.AvgMilli); err != nil {
			return nil, err
		}
		u.Day = time.Date(u.Day.Year(), u.Day.Month(), u.Day.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, u)
	}
	return out, rows.Err()
}
