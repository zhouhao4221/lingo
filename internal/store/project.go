package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhouhao4221/lingo/internal/project"
)

// syncLockKey 是项目同步的 advisory lock 键，多实例同时启动时串行执行。
const syncLockKey = 0x6c696e676f01

// SyncProjects 把声明的项目与仓库同步入库（一个事务）：按 key / owner+name 新增或更新；声明中不再出现的
// 项目与仓库标记为停用（不删除，已有工单仍可查询）；仓库可在项目间移动。调用前应先经 project.Validate 校验。
func (s *Store) SyncProjects(ctx context.Context, specs []project.Spec) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", syncLockKey); err != nil {
		return fmt.Errorf("store: sync lock: %w", err)
	}
	keys := make([]string, 0, len(specs))
	var repoNames []string
	for _, p := range specs {
		keys = append(keys, p.Key)
		var pid int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO projects (key, name, description) VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, enabled = true, updated_at = now()
			RETURNING id`, p.Key, p.Name, p.Description).Scan(&pid); err != nil {
			return fmt.Errorf("store: sync project %s: %w", p.Key, err)
		}
		var defaultID int64
		for i, r := range p.Repos {
			kind := r.Kind
			if kind == "" {
				kind = "other"
			}
			var rid int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO repos (project_id, owner, name, kind) VALUES ($1, $2, $3, $4)
				ON CONFLICT (lower(owner), lower(name)) DO UPDATE SET project_id = EXCLUDED.project_id, owner = EXCLUDED.owner,
				    name = EXCLUDED.name, kind = EXCLUDED.kind, enabled = true, updated_at = now()
				RETURNING id`, pid, r.Owner, r.Name, kind).Scan(&rid); err != nil {
				return fmt.Errorf("store: sync repo %s: %w", r.FullName(), err)
			}
			repoNames = append(repoNames, strings.ToLower(r.FullName()))
			if (p.DefaultRepo == "" && i == 0) || strings.EqualFold(p.DefaultRepo, r.FullName()) {
				defaultID = rid
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET default_repo_id = $2 WHERE id = $1", pid, nullInt64(defaultID)); err != nil {
			return fmt.Errorf("store: sync default repo %s: %w", p.Key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET enabled = false, updated_at = now() WHERE enabled AND NOT (key = ANY(($1::text)::text[]))`,
		pgTextArray(keys)); err != nil {
		return fmt.Errorf("store: disable projects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repos SET enabled = false, updated_at = now()
		WHERE enabled AND NOT (lower(owner || '/' || name) = ANY(($1::text)::text[]))`, pgTextArray(repoNames)); err != nil {
		return fmt.Errorf("store: disable repos: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// pgTextArray 把字符串切片编码为 PostgreSQL text[] 字面量，不依赖驱动的数组支持。
func pgTextArray(ss []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
