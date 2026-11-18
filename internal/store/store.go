package store

import (
	"context"
	"database/sql"
	"errors"
)

// Store 是基于 *sql.DB 的仓储实现，可并发使用。
type Store struct {
	db *sql.DB
}

// New 创建 Store；db 由接入方提供并负责关闭。
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("store: db is required")
	}
	return &Store{db: db}, nil
}

// Ping 检查数据库连通性。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// nullInt64 把 0 映射为 NULL。
func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }
