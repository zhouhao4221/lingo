package settings

import (
	"context"
	"encoding/json"
)

// Store 是连接配置仓储，由 internal/store 实现。
type Store interface {
	// LoadSettings 返回全部类别的当前行。
	LoadSettings(ctx context.Context) ([]Record, error)
	// MutateSettings 在一个事务中锁定全部行，用 fn 返回的内容回写对应类别（版本 +1，记录修改人与时间），
	// 返回回写后的全部行；fn 返回错误时回滚。
	MutateSettings(ctx context.Context, by string, fn func(cur []Record) (map[Category]json.RawMessage, error)) ([]Record, error)
}

// Service 读写连接配置。
type Service struct {
	Store     Store
	Overrides Overrides
}

// Load 返回数据库中保存的配置。
func (s *Service) Load(ctx context.Context) (Stored, error) {
	rs, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return Stored{}, err
	}
	return Decode(rs)
}

// Update 应用修改并保存，返回保存后的配置与涉及的字段（无字段时不写库）。
func (s *Service) Update(ctx context.Context, by string, p Patch) (Stored, Changed, error) {
	var changed Changed
	rs, err := s.Store.MutateSettings(ctx, by, func(cur []Record) (map[Category]json.RawMessage, error) {
		st, err := Decode(cur)
		if err != nil {
			return nil, err
		}
		next, ch, err := Apply(s.Overrides, st, p)
		if err != nil {
			return nil, err
		}
		changed = ch
		return Encode(next, ch)
	})
	if err != nil {
		return Stored{}, nil, err
	}
	st, err := Decode(rs)
	return st, changed, err
}
