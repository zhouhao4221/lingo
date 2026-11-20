package ticket

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

// 视角，取值与 lingo.Perspective 一致：只给需求补充归类，不授予权限（PRD 3.2.2.1）。
const (
	PerspectiveUser     = "user"     // 使用者
	PerspectiveBusiness = "business" // 业务方
	PerspectiveExpert   = "expert"   // 专家
	PerspectiveLeader   = "leader"   // 决策者
)

// PerspectiveOrder 是视角在 Issue 区块与查询结果中的固定顺序。
var PerspectiveOrder = []string{PerspectiveUser, PerspectiveBusiness, PerspectiveExpert, PerspectiveLeader}

// MaxPerspectiveRunes 是单段视角补充的长度上限（Unicode 字符）。
const MaxPerspectiveRunes = 10000

// NormalizePerspective 把视角规范为固定取值：为空视为 user，非法值 ok 为 false。
func NormalizePerspective(p string) (string, bool) {
	if p == "" {
		return PerspectiveUser, true
	}
	return p, slices.Contains(PerspectiveOrder, p)
}

// PerspectiveName 是视角的中文名，用于标签与提示（使用者 / 业务方 / 专家 / 决策者）。
func PerspectiveName(p string) string {
	switch p {
	case PerspectiveBusiness:
		return "业务方"
	case PerspectiveExpert:
		return "专家"
	case PerspectiveLeader:
		return "决策者"
	}
	return "使用者"
}

// PerspectiveSection 是视角在 Issue 区块中的段落标题（使用者反馈 / 业务补充 / 专家意见 / 决策意见）。
func PerspectiveSection(p string) string {
	switch p {
	case PerspectiveBusiness:
		return "业务补充"
	case PerspectiveExpert:
		return "专家意见"
	case PerspectiveLeader:
		return "决策意见"
	}
	return "使用者反馈"
}

// Perspective 是一段需求视角补充（lingo_ticket_perspectives）；Original 为真的是工单原始提交派生的首段（ID 为 0）。
type Perspective struct {
	ID          int64
	TicketID    int64
	Perspective string
	AuthorID    string // 接入方用户 ID，仅留痕，不对外
	AuthorName  string
	Body        string
	At          time.Time
	Original    bool
	PostedID    int64 // 提示评论已由机器人发到 Gitea 的评论 ID
}

// PerspectiveSet 是一张需求工单的全部视角补充：首段在前，其余按时间升序；Missing 为缺失的必填视角（固定顺序）。
type PerspectiveSet struct {
	Ticket  *Ticket
	Items   []Perspective
	Missing []string
}

// Present 返回出现过的视角（固定顺序）。
func (s *PerspectiveSet) Present() []string {
	var out []string
	for _, p := range PerspectiveOrder {
		if slices.ContainsFunc(s.Items, func(it Perspective) bool { return it.Perspective == p }) {
			out = append(out, p)
		}
	}
	return out
}

// AddPerspective 以调用者的视角给需求类工单追加一段补充（只追加，不能修改、撤回）。谁能补充哪条需求由接入方判断：
// 这里只校验项目范围与工单类型，不受 external 仅本人可见的限制（PRD 3.2.0.6 的例外）。工单已转出时入队同步到 Issue。
func (s *Service) AddPerspective(ctx context.Context, p auth.Principal, id int64, body string) (*Perspective, error) {
	if !p.Known() || p.ID == "" {
		return nil, fmt.Errorf("%w: caller has no user id or role", ErrForbidden)
	}
	persp, ok := NormalizePerspective(p.Perspective)
	if !ok {
		return nil, fmt.Errorf("%w: perspective %q", ErrInvalid, p.Perspective)
	}
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return nil, fmt.Errorf("%w: content is required", ErrInvalid)
	case utf8.RuneCountInString(body) > MaxPerspectiveRunes:
		return nil, fmt.Errorf("%w: content exceeds %d characters", ErrInvalid, MaxPerspectiveRunes)
	}
	var out *Perspective
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, id)
		if err != nil {
			return err
		}
		if err := s.perspectiveTarget(p, t); err != nil {
			return err
		}
		if t.Status.ended() {
			return fmt.Errorf("%w: ticket %d is %s", ErrConflict, t.ID, t.Status)
		}
		out = &Perspective{TicketID: t.ID, Perspective: persp, AuthorID: p.ID, AuthorName: p.Name, Body: body, At: s.o.Now()}
		if out.ID, err = tx.AddPerspective(ctx, *out); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "perspective.added",
			Payload: map[string]any{"perspective_id": out.ID, "perspective": persp}}); err != nil {
			return err
		}
		if t.Status.forwarded() && t.Status != StatusAbnormal {
			if err := tx.Enqueue(ctx, Task{Key: fmt.Sprintf("%s:perspective:%d", OpSyncPerspectives, out.ID), TicketID: t.ID,
				Op: OpSyncPerspectives, Payload: map[string]any{"perspective_id": out.ID}}); err != nil {
				return err
			}
		}
		e := s.event(t, notify.TicketPerspectiveAdded, "", p.ID, PerspectiveNotice(persp, p.Name))
		e.Perspectives = []string{persp}
		return tx.Emit(ctx, e)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPerspectives 返回需求类工单的视角补充与缺失的必填视角；可见范围同 AddPerspective（由接入方判断）。
// 只含视角补充，不含工单的其他评论、附件与提交人联系方式。
func (s *Service) ListPerspectives(ctx context.Context, p auth.Principal, id int64) (*PerspectiveSet, error) {
	if !p.Known() || p.ID == "" {
		return nil, fmt.Errorf("%w: caller has no user id or role", ErrForbidden)
	}
	t, err := s.o.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.perspectiveTarget(p, t); err != nil {
		return nil, err
	}
	rows, err := s.queries().Perspectives(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.perspectiveSet(t, rows), nil
}

// Perspectives 返回工单的视角补充（不做访问控制，供 worker 渲染 Issue 区块）。
func (s *Service) Perspectives(ctx context.Context, id int64) (*PerspectiveSet, error) {
	t, err := s.o.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries().Perspectives(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.perspectiveSet(t, rows), nil
}

// perspectiveTarget 校验视角补充的目标：项目在调用者范围内且仍声明，工单为需求类型。
func (s *Service) perspectiveTarget(p auth.Principal, t *Ticket) error {
	if !p.InProject(t.ProjectKey) || !s.o.Declared(t.ProjectKey, "") {
		return ErrNotFound
	}
	if t.Type != TypeFeature {
		return fmt.Errorf("%w: perspectives are only for feature tickets", ErrInvalid)
	}
	return nil
}

// perspectiveSet 在补充行前加上由工单派生的首段，并计算缺失的必填视角。
func (s *Service) perspectiveSet(t *Ticket, rows []Perspective) *PerspectiveSet {
	first, _ := NormalizePerspective(t.UserPerspective)
	items := append([]Perspective{{TicketID: t.ID, Perspective: first, AuthorName: t.UserName, Body: originalText(t),
		At: t.CreatedAt, Original: true}}, rows...)
	set := &PerspectiveSet{Ticket: t, Items: items}
	present := set.Present()
	for _, r := range s.required(t.ProjectKey) {
		if !slices.Contains(present, r) {
			set.Missing = append(set.Missing, r)
		}
	}
	return set
}

// required 返回项目的必填视角（去重、固定顺序，忽略非法值）。
func (s *Service) required(projectKey string) []string {
	if s.o.RequiredPerspectives == nil {
		return nil
	}
	want := s.o.RequiredPerspectives(projectKey)
	var out []string
	for _, p := range PerspectiveOrder {
		if slices.Contains(want, p) {
			out = append(out, p)
		}
	}
	return out
}

// emitMissing 在需求工单转出时检查必填视角，缺失时发出 ticket.perspective_missing。
func (s *Service) emitMissing(ctx context.Context, tx Tx, t *Ticket, actor string) error {
	if t.Type != TypeFeature || len(s.required(t.ProjectKey)) == 0 {
		return nil
	}
	rows, err := tx.Perspectives(ctx, t.ID)
	if err != nil {
		return err
	}
	set := s.perspectiveSet(t, rows)
	if len(set.Missing) == 0 {
		return nil
	}
	names := make([]string, len(set.Missing))
	for i, m := range set.Missing {
		names[i] = PerspectiveSection(m)
	}
	e := s.event(t, notify.TicketPerspectiveMissing, "", actor, "缺少："+strings.Join(names, "、"))
	e.Perspectives = set.Missing
	return tx.Emit(ctx, e)
}

// PerspectiveNotice 是追加补充后的提示文案（事件正文与 Issue 提示评论）："业务方 · 张三 补充了内容"，姓名为空时省略。
func PerspectiveNotice(perspective, name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return PerspectiveName(perspective) + " · " + name + " 补充了内容"
	}
	return PerspectiveName(perspective) + " 补充了内容"
}

// originalText 是首段的内容：工单正文；只填了表单字段时按字段 ID 顺序拼接。
func originalText(t *Ticket) string {
	if t.Body != "" {
		return t.Body
	}
	ids := make([]string, 0, len(t.Fields))
	for id := range t.Fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		var vs []string
		for _, v := range t.Fields[id] {
			if v = strings.TrimSpace(v); v != "" {
				vs = append(vs, v)
			}
		}
		if len(vs) > 0 {
			lines = append(lines, id+"："+strings.Join(vs, "、"))
		}
	}
	return strings.Join(lines, "\n")
}
