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

// projectScope 返回调用者范围收窄到项目 key（为空不收窄）后的范围；key 不在范围内时 ok=false。
func projectScope(p auth.Principal, key string) (Scope, bool) {
	switch {
	case key == "":
		return scopeOf(p), true
	case !p.InProject(key):
		return Scope{}, false
	}
	return Scope{Projects: []string{key}}, true
}

// DefaultLimit / MaxLimit 是列表查询的默认与最大条数。
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Limit 把请求的条数规范到 [1, MaxLimit]，≤0 时取 def。
func Limit(n, def int) int { return limitOf(n, def) }

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

// 检索时间字段（SearchQuery.TimeField）。
const (
	TimeCreated   = "created"
	TimeUpdated   = "updated"
	TimeForwarded = "forwarded" // 转出时间：后台确认转出的时间，自动转出的为提交时间
)

// SearchQuery 是工单检索的条件，供知识问答按自然语言条件查找工单。范围（Scope、UserID、ForwardedOnly）由
// Service.Search 按调用者角色填写，调用方传入的值会被覆盖。
type SearchQuery struct {
	Scope         Scope
	UserID        string // 非空时只含该提交人的工单
	ForwardedOnly bool   // 只含已转出的工单（agent）
	IDs           []int64
	IssueNumber   int64 // 主 Issue 编号（与 Repo 一起使用时更精确）
	Repo          string
	Type          string
	Statuses      []string
	Keywords      []string  // 各关键词须同时命中，每个匹配标题或正文（不区分大小写）
	TimeField     string    // TimeCreated | TimeUpdated | TimeForwarded；为空时按 TimeCreated
	From, To      time.Time // 零值不限；To 不含
	BeforeID      int64     // 游标：按 ID 降序
	Limit         int
}

// StatRow 是工单统计的一行。
type StatRow struct {
	ProjectKey string
	Type       string
	Status     string
	Count      int
}

// QueryStore 是只读查询，由 internal/store 实现。
type QueryStore interface {
	ListTriage(ctx context.Context, q TriageQuery) ([]*Ticket, error)
	TriageCounts(ctx context.Context, s Scope, overdueBefore time.Time) ([]TriageCount, error)
	ListMine(ctx context.Context, q MineQuery) ([]*Ticket, error)
	// Search 按条件检索工单（新的在前）。
	Search(ctx context.Context, q SearchQuery) ([]*Ticket, error)
	// Stats 按项目、类型、状态统计符合条件的工单数（忽略 BeforeID 与 Limit）。
	Stats(ctx context.Context, q SearchQuery) ([]StatRow, error)
	Events(ctx context.Context, q EventQuery) ([]notify.Event, error)
	Comments(ctx context.Context, ticketID int64, publicOnly bool) ([]Comment, error)
	// Perspectives 返回工单的视角补充（不含首段），按时间升序。
	Perspectives(ctx context.Context, ticketID int64) ([]Perspective, error)
	AutoForwardOverrides(ctx context.Context, keys []string) (map[string]map[string]bool, error)
	// TicketAttachments 返回工单的附件（不含内容）；publicOnly 时排除随内部评论上传的，forwardedOnly 时只含已上传到 Gitea 的。
	TicketAttachments(ctx context.Context, ticketID int64, publicOnly, forwardedOnly bool) ([]Attachment, error)
}

// ListTriage 列出调用者项目范围内的待确认工单（reviewer / admin），按进入待确认的先后（ID 升序）。
func (s *Service) ListTriage(ctx context.Context, p auth.Principal, project string, q TriageQuery) ([]*Ticket, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: triage requires reviewer or admin", ErrForbidden)
	}
	if q.Type != "" && !ValidType(q.Type) {
		return nil, fmt.Errorf("%w: type %q", ErrInvalid, q.Type)
	}
	sc, ok := projectScope(p, project)
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

// Search 在调用者可见的工单中按条件检索（新的在前），可见范围与 Visible 一致：external 只含本人的，agent 只含
// 已转出的，reviewer / admin 为项目范围内全部；mine 为真时只含调用者本人提交的。project 不在范围内时为空。
func (s *Service) Search(ctx context.Context, p auth.Principal, project string, mine bool, q SearchQuery) ([]*Ticket, error) {
	q, ok, err := s.searchScope(p, project, mine, q)
	if err != nil || !ok {
		return nil, err
	}
	q.Limit = limitOf(q.Limit, DefaultLimit)
	return s.queries().Search(ctx, q)
}

// Stats 按项目、类型、状态统计调用者可见、符合条件的工单数，可见范围同 Search。
func (s *Service) Stats(ctx context.Context, p auth.Principal, project string, mine bool, q SearchQuery) ([]StatRow, error) {
	q, ok, err := s.searchScope(p, project, mine, q)
	if err != nil || !ok {
		return nil, err
	}
	return s.queries().Stats(ctx, q)
}

func (s *Service) searchScope(p auth.Principal, project string, mine bool, q SearchQuery) (SearchQuery, bool, error) {
	if q.Type != "" && !ValidType(q.Type) {
		return q, false, fmt.Errorf("%w: type %q", ErrInvalid, q.Type)
	}
	for _, st := range q.Statuses {
		if !validStatus(Status(st)) {
			return q, false, fmt.Errorf("%w: status %q", ErrInvalid, st)
		}
	}
	switch q.TimeField {
	case "", TimeCreated, TimeUpdated, TimeForwarded:
	default:
		return q, false, fmt.Errorf("%w: time field %q", ErrInvalid, q.TimeField)
	}
	q.UserID, q.ForwardedOnly = "", false
	switch p.Role {
	case auth.RoleExternal:
		if p.ID == "" {
			return q, false, fmt.Errorf("%w: caller has no user id", ErrForbidden)
		}
		q.UserID = p.ID
	case auth.RoleAgent:
		q.ForwardedOnly = true
		if mine { // agent 不提交工单
			return q, false, nil
		}
	case auth.RoleReviewer, auth.RoleAdmin:
		if mine {
			if p.ID == "" {
				return q, false, nil
			}
			q.UserID = p.ID
		}
	default:
		return q, false, fmt.Errorf("%w: unknown role", ErrForbidden)
	}
	sc, ok := projectScope(p, project)
	q.Scope = sc
	return q, ok, nil
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
		q.UserID, q.ExcludeTriage = p.ID, true
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

// Detail 返回工单、评论与附件（一次鉴权），可见范围同 Comments。
func (s *Service) Detail(ctx context.Context, p auth.Principal, id int64) (*Ticket, []Comment, []Attachment, error) {
	t, err := s.Get(ctx, p, id)
	if err != nil {
		return nil, nil, nil, err
	}
	ext := p.Role == auth.RoleExternal
	cs, err := s.queries().Comments(ctx, id, ext)
	if err != nil {
		return nil, nil, nil, err
	}
	// agent 只看已转出到 Gitea 的附件：后台人员没勾选的截图可能含敏感信息（附件地址本身的访问控制由接入方负责）
	as, err := s.queries().TicketAttachments(ctx, id, ext, p.Role == auth.RoleAgent)
	if err != nil {
		return nil, nil, nil, err
	}
	if ext {
		for i := range as { // 转出失败的原因（Gitea / 接入方返回的错误原文）与 Gitea 地址是内部信息
			as[i].Error, as[i].GiteaURL = "", ""
		}
	}
	return t, cs, as, nil
}

// RemindOverdue 对项目 projects 中待确认超过 every（自进入待确认或上次提醒起）的工单发出 triage.overdue，
// 最多 limit 条（上限 MaxLimit）；多实例并发执行时各自领取不同的工单（SKIP LOCKED）。返回提醒的条数。
func (s *Service) RemindOverdue(ctx context.Context, projects []string, every time.Duration, limit int) (int, error) {
	n := 0
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		now := s.o.Now()
		ts, err := tx.LockOverdue(ctx, Scope{Projects: projects}, now.Add(-every), limitOf(limit, DefaultLimit))
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
	var keys []string
	for _, key := range projects {
		if p.InProject(key) {
			keys = append(keys, key)
		}
	}
	overrides, err := s.queries().AutoForwardOverrides(ctx, keys)
	if err != nil {
		return nil, err
	}
	var out []AutoForwardSetting
	for _, key := range keys {
		for _, typ := range []string{TypeConsult, TypeBug, TypeFeature} {
			on, ok := overrides[key][typ]
			if !ok {
				on = s.o.AutoForward(key, typ)
			}
			out = append(out, AutoForwardSetting{ProjectKey: key, Type: typ, On: on, Overridden: ok})
		}
	}
	return out, nil
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
