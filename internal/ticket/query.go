package ticket

import (
	"context"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

// Scope 是查询的项目范围。store 的列表查询一律带 Scope，没有不带范围的版本（PRD 3.2.0.6）。
type Scope struct {
	All      bool     // 不限项目
	Projects []string // All 为假时只含这些项目
}

func scopeOf(p auth.Principal) Scope {
	if len(p.Projects) == 0 {
		return Scope{All: true}
	}
	return Scope{Projects: p.Projects}
}

// narrow 把范围收窄到单个项目；不在范围内时返回 ok=false。
func (s Scope) narrow(key string) (Scope, bool) {
	if key == "" {
		return s, true
	}
	if !s.All && !contains(s.Projects, key) {
		return Scope{}, false
	}
	return Scope{Projects: []string{key}}, true
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// DefaultLimit / MaxLimit 是列表查询的默认与最大条数。
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

func limitOf(n, def int) int {
	switch {
	case n <= 0:
		return def
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}

// TriageQuery 是待确认列表的条件。
type TriageQuery struct {
	Scope         Scope
	Type          string
	PendingBefore time.Time // 非零时只返回在此之前进入待确认的
	AfterID       int64     // 游标：按 ID 升序
	Limit         int
}

// TriageCount 是单个项目的待确认统计。
type TriageCount struct {
	ProjectKey string
	Pending    int
	Overdue    int
}

// MineQuery 是提交人本人工单列表的条件。
type MineQuery struct {
	UserID   string
	Status   string
	BeforeID int64 // 游标：按 ID 降序（新的在前）
	Limit    int
}

// EventQuery 是变更流的条件。
type EventQuery struct {
	AfterID       int64
	Limit         int
	Scope         Scope
	UserID        string // 非空时只返回该提交人的工单事件
	ExcludeTriage bool   // 不返回 triage.* 事件
	ForwardedOnly bool   // 只返回已转出工单的事件
}

// QueryStore 是只读查询，由 internal/store 实现。
type QueryStore interface {
	ListTriage(ctx context.Context, q TriageQuery) ([]*Ticket, error)
	TriageCounts(ctx context.Context, s Scope, overdueBefore time.Time) ([]TriageCount, error)
	ListMine(ctx context.Context, q MineQuery) ([]*Ticket, error)
	Events(ctx context.Context, q EventQuery) ([]notify.Event, error)
	Comments(ctx context.Context, ticketID int64, publicOnly bool) ([]Comment, error)
}

// ListTriage 列出调用者项目范围内的待确认工单（reviewer / admin），按进入待确认的先后（ID 升序）。
func (s *Service) ListTriage(ctx context.Context, p auth.Principal, project string, q TriageQuery) ([]*Ticket, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: triage requires reviewer or admin", ErrForbidden)
	}
	if q.Type != "" && !ValidType(q.Type) {
		return nil, fmt.Errorf("%w: type %q", ErrInvalid, q.Type)
	}
	sc, ok := scopeOf(p).narrow(project)
	if !ok {
		return nil, nil
	}
	q.Scope, q.Limit = sc, limitOf(q.Limit, DefaultLimit)
	return s.queries().ListTriage(ctx, q)
}

// TriageSummary 返回调用者项目范围内各项目的待确认数与超过 overdueAfter 的数量（reviewer / admin）。
func (s *Service) TriageSummary(ctx context.Context, p auth.Principal, overdueAfter time.Duration) ([]TriageCount, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: triage requires reviewer or admin", ErrForbidden)
	}
	return s.queries().TriageCounts(ctx, scopeOf(p), s.o.Now().Add(-overdueAfter))
}

// ListMine 返回调用者本人提交的工单（新的在前）。
func (s *Service) ListMine(ctx context.Context, p auth.Principal, q MineQuery) ([]*Ticket, error) {
	if !p.Known() || p.ID == "" {
		return nil, fmt.Errorf("%w: caller has no user id or role", ErrForbidden)
	}
	if q.Status != "" && !validStatus(Status(q.Status)) {
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, q.Status)
	}
	q.UserID, q.Limit = p.ID, limitOf(q.Limit, DefaultLimit)
	return s.queries().ListMine(ctx, q)
}

// ListEvents 返回调用者可见的、ID 大于 afterID 的事件（升序）：external 只看本人工单的非 triage 事件；
// agent 看项目内已转出工单的非 triage 事件；reviewer / admin 看项目内全部。
func (s *Service) ListEvents(ctx context.Context, p auth.Principal, afterID int64, limit int) ([]notify.Event, error) {
	q := EventQuery{AfterID: afterID, Limit: limitOf(limit, 100), Scope: scopeOf(p)}
	switch p.Role {
	case auth.RoleExternal:
		if p.ID == "" {
			return nil, fmt.Errorf("%w: caller has no user id", ErrForbidden)
		}
		q.Scope, q.UserID, q.ExcludeTriage = Scope{All: true}, p.ID, true
	case auth.RoleAgent:
		q.ExcludeTriage, q.ForwardedOnly = true, true
	case auth.RoleReviewer, auth.RoleAdmin:
	default:
		return nil, fmt.Errorf("%w: unknown role", ErrForbidden)
	}
	return s.queries().Events(ctx, q)
}

// Comments 返回工单评论：external 只看对外评论，其他可见该工单的角色看全部（含研发内部讨论）。
func (s *Service) Comments(ctx context.Context, p auth.Principal, id int64) ([]Comment, error) {
	if _, err := s.Get(ctx, p, id); err != nil {
		return nil, err
	}
	return s.queries().Comments(ctx, id, p.Role == auth.RoleExternal)
}

// RemindOverdue 对待确认超过 every（自进入待确认或上次提醒起）的工单发出 triage.overdue，最多 limit 条；
// 多实例并发执行时各自领取不同的工单（SKIP LOCKED）。返回提醒的条数。
func (s *Service) RemindOverdue(ctx context.Context, every time.Duration, limit int) (int, error) {
	n := 0
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		now := s.o.Now()
		ts, err := tx.LockOverdue(ctx, now.Add(-every), limitOf(limit, DefaultLimit))
		if err != nil {
			return err
		}
		for _, t := range ts {
			if err := tx.MarkReminded(ctx, t.ID, now); err != nil {
				return err
			}
			e := s.event(t, notify.TriageOverdue, "", "", "")
			if err := tx.Emit(ctx, e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// AutoForwardSetting 是一个项目 + 类型当前生效的开关。
type AutoForwardSetting struct {
	ProjectKey string
	Type       string
	On         bool
	Overridden bool
}

// AutoForwardSettings 返回 projects 中调用者可见的项目的自动转出开关（reviewer / admin）。
func (s *Service) AutoForwardSettings(ctx context.Context, p auth.Principal, projects []string) ([]AutoForwardSetting, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: settings require reviewer or admin", ErrForbidden)
	}
	var out []AutoForwardSetting
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		for _, key := range projects {
			if !p.InProject(key) {
				continue
			}
			proj, err := tx.Project(ctx, key)
			if err != nil {
				return err
			}
			for _, typ := range []string{TypeConsult, TypeBug, TypeFeature} {
				on, ok, err := tx.AutoForward(ctx, proj.ID, typ)
				if err != nil {
					return err
				}
				if !ok {
					on = s.o.AutoForward(key, typ)
				}
				out = append(out, AutoForwardSetting{ProjectKey: key, Type: typ, On: on, Overridden: ok})
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) queries() QueryStore { return s.o.Queries }

func validStatus(st Status) bool {
	switch st {
	case StatusPendingTriage, StatusAnswered, StatusMerged, StatusRejected, StatusSubmitted, StatusOpen, StatusInProgress,
		StatusNeedInfo, StatusFixed, StatusReleased, StatusDeployed, StatusClosed, StatusAbnormal:
		return true
	}
	return false
}
