package store

import (
	"context"
	"database/sql"
	"errors"
)

// Store 是基于 *sql.DB 的仓储实现，可并发使用。
type Store struct {
	db     *sql.DB
	notify bool // Emit 时是否写通知 outbox
}

// Option 是 New 的可选配置。
type Option func(*Store)

// WithNotify 让 Emit 在写入对外事件的同时写通知 outbox（接入方配置了 Notifier 时使用）；
// 未启用时事件只进 events 表，供 ListEvents 拉取。
func WithNotify() Option { return func(s *Store) { s.notify = true } }

// New 创建 Store；db 由接入方提供并负责关闭。
func New(db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("store: db is required")
	}
	s := &Store{db: db}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Ping 检查数据库连通性。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// nullInt64 把 0 映射为 NULL。
func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }
