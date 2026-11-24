package lingo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// PerspectiveEntry 是一段需求视角补充。Original 为真的是工单原始提交（首段，ID 为 0，内容为工单正文）。
type PerspectiveEntry struct {
	ID          int64       `json:"id"`
	Perspective Perspective `json:"perspective"`
	AuthorName  string      `json:"author_name,omitempty"`
	Content     string      `json:"content"`
	At          time.Time   `json:"at"`
	Original    bool        `json:"original,omitempty"`
}

// PerspectiveGroup 是同一视角的补充，按时间升序。
type PerspectiveGroup struct {
	Perspective Perspective        `json:"perspective"`
	Title       string             `json:"title"` // 使用者反馈 / 业务补充 / 专家意见 / 决策意见
	Entries     []PerspectiveEntry `json:"entries"`
}

// TicketPerspectives 是需求工单按视角分组的补充：Groups 按 使用者 → 业务方 → 专家 → 决策者 排列，只含有内容的视角；
// Missing 是缺失的必填视角（见 Config.RequiredPerspectives）。
type TicketPerspectives struct {
	TicketID int64              `json:"ticket_id"`
	Groups   []PerspectiveGroup `json:"groups"`
	Missing  []Perspective      `json:"missing"`
}

// AddPerspective 以 u.Perspective 的视角给需求工单追加一段补充（只追加，不能修改或撤回），content 为纯文本 / Markdown，
// 不超过 10000 字。工单已转出时由机器人重写 Issue 正文中的视角区块、补打视角标签并发提示评论；发出 ticket.perspective_added。
//
// 能否补充由 Config.AuthorizePerspective 判断（未配置时同工单可见范围）。补充人姓名（User.Name）会写入 Gitea Issue，
// 邮箱与用户 ID 不会。
// 非需求工单、视角或内容不合法返回 ErrInvalidInput；工单已答复 / 合并 / 驳回 / 关闭返回 ErrConflict。
func (l *Lingo) AddPerspective(ctx context.Context, u User, ticketID int64, content string) (PerspectiveEntry, error) {
	p, err := l.tickets.AddPerspective(withCaller(ctx, u), l.principal(u), ticketID, content)
	if err != nil {
		return PerspectiveEntry{}, publicErr(err)
	}
	return publicPerspective(*p), nil
}

// ListPerspectives 返回需求工单按视角分组的补充与缺失的必填视角；能否查看由 Config.AuthorizePerspective 判断（write 为假）。
// 只含视角补充（视角、姓名、时间、内容），不含工单的其他评论、附件与提交人联系方式。
func (l *Lingo) ListPerspectives(ctx context.Context, u User, ticketID int64) (TicketPerspectives, error) {
	set, err := l.tickets.ListPerspectives(withCaller(ctx, u), l.principal(u), ticketID)
	if err != nil {
		return TicketPerspectives{}, publicErr(err)
	}
	out := TicketPerspectives{TicketID: ticketID, Groups: []PerspectiveGroup{}, Missing: publicPerspectives(set.Missing)}
	if out.Missing == nil {
		out.Missing = []Perspective{}
	}
	for _, g := range set.Groups() {
		pg := PerspectiveGroup{Perspective: Perspective(g.Perspective), Title: ticket.PerspectiveSection(g.Perspective)}
		for _, it := range g.Items {
			pg.Entries = append(pg.Entries, publicPerspective(it))
		}
		out.Groups = append(out.Groups, pg)
	}
	return out, nil
}

type callerKey struct{}

// withCaller 在 context 中带上调用者原始身份，供 authorizePerspective 把完整的 User（含 Scopes）交给接入方。
func withCaller(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, callerKey{}, u)
}

// authorizePerspective 把领域层的视角补充鉴权交给 Config.AuthorizePerspective；未配置时按工单可见范围。
func (l *Lingo) authorizePerspective(ctx context.Context, p auth.Principal, t *ticket.Ticket, write bool) error {
	if l.cfg.AuthorizePerspective == nil {
		if !ticket.Visible(p, t) {
			return ticket.ErrNotFound
		}
		return nil
	}
	u, ok := ctx.Value(callerKey{}).(User)
	if !ok {
		return ticket.ErrForbidden
	}
	err := l.cfg.AuthorizePerspective(ctx, u, l.publicTicketFor(u, t), write)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return ticket.ErrNotFound
	case errors.Is(err, ErrForbidden):
		return fmt.Errorf("%w: %v", ticket.ErrForbidden, err)
	}
	return err
}

func publicPerspective(p ticket.Perspective) PerspectiveEntry {
	return PerspectiveEntry{ID: p.ID, Perspective: Perspective(p.Perspective), AuthorName: p.AuthorName, Content: p.Body, At: p.At,
		Original: p.Original}
}

func publicPerspectives(ps []string) []Perspective {
	if len(ps) == 0 {
		return nil
	}
	out := make([]Perspective, len(ps))
	for i, p := range ps {
		out[i] = Perspective(p)
	}
	return out
}

// requiredPerspectives 返回项目的必填视角：项目声明了（含空切片）用项目的，否则用 Config 的。
func (l *Lingo) requiredPerspectives(projectKey string) []string {
	ps := l.cfg.RequiredPerspectives
	for _, p := range l.cfg.Projects {
		if p.Key == projectKey && p.RequiredPerspectives != nil {
			ps = p.RequiredPerspectives
		}
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

// validatePerspectives 校验配置中的必填视角取值。
func validatePerspectives(cfg Config) error {
	check := func(where string, ps []Perspective) error {
		for _, p := range ps {
			if _, ok := ticket.NormalizePerspective(string(p)); !ok || p == "" {
				return fmt.Errorf("lingo: %s: unknown perspective %q", where, p)
			}
		}
		return nil
	}
	if err := check("Config.RequiredPerspectives", cfg.RequiredPerspectives); err != nil {
		return err
	}
	for _, p := range cfg.Projects {
		if err := check("Project "+p.Key+" RequiredPerspectives", p.RequiredPerspectives); err != nil {
			return err
		}
	}
	return nil
}
