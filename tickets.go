package lingo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/project"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/worker"
)

var (
	// ErrInvalidInput 表示参数不合法（类型未知、内容为空、仓库不属于项目等），错误信息说明原因。
	ErrInvalidInput = errors.New("lingo: invalid input")
	// ErrForbidden 表示调用者的角色不能执行该操作（如 external 确认转出）。
	ErrForbidden = errors.New("lingo: forbidden")
	// ErrConflict 表示工单当前状态不允许该操作（如对已处理的工单再次转出）。
	ErrConflict = errors.New("lingo: conflict")
)

// TicketInput 是提交工单的参数（REST POST /tickets 同名字段）。
type TicketInput struct {
	Project string // 项目 key；为空取 Config.Projects[0]
	Repo    string // "owner/name"，可选，须属于该项目；为空取项目默认仓库
	Type    TicketType
	Title   string // 为空时取正文首行
	Content string
	// Fields 是仓库 Issue 表单模板的字段取值（字段 ID → 值；下拉与多选为选项标题），转出时按模板格式渲染。
	Fields  map[string][]string
	Context map[string]string // 页面 URL、页面标题、浏览器等，转出时附在 Issue 末尾；不要放个人身份信息
	Version string            // 提交人正在使用的版本
	// ConversationID 是从问答转出时的会话 ID。
	ConversationID int64
}

// ForwardOptions 是后台人员转出工单时可修改的内容；零值表示不修改。
type ForwardOptions struct {
	Title string
	Type  TicketType
	Repo  string // "owner/name"，须属于工单所在项目
	Note  string // 补充说明，写入 Issue
}

// CreateTicket 以 u 的身份提交工单。按项目与类型的自动转出开关（Project.AutoForward，可被 SetAutoForward 覆盖）
// 直接转出（submitted，后台由 Run 建 Gitea Issue）或进入待确认（pending_triage，发出 triage.required）。
func (l *Lingo) CreateTicket(ctx context.Context, u User, in TicketInput) (Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return Ticket{}, err
	}
	if in.Project == "" {
		in.Project = l.cfg.Projects[0].Key
	}
	t, err := l.tickets.Create(ctx, principal(u), ticket.NewTicket{Project: in.Project, Repo: in.Repo, Type: string(in.Type),
		Title: in.Title, Body: in.Content, Fields: in.Fields, Context: in.Context, Version: in.Version,
		Conversation: in.ConversationID, Source: "go"})
	return l.ticketResult(t, err)
}

// GetTicket 返回工单 id；无权访问时与不存在一样返回 ErrNotFound（见数据隔离）。
func (l *Lingo) GetTicket(ctx context.Context, u User, id int64) (Ticket, error) {
	t, err := l.tickets.Get(ctx, principal(u), id)
	return l.ticketResult(t, err)
}

// ForwardTicket 把待确认工单转出到 Gitea（仅 reviewer / admin）。
func (l *Lingo) ForwardTicket(ctx context.Context, u User, id int64, o ForwardOptions) (Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return Ticket{}, err
	}
	t, err := l.tickets.Forward(ctx, principal(u), id, ticket.ForwardInput{Title: o.Title, Type: string(o.Type), Repo: o.Repo, Note: o.Note})
	return l.ticketResult(t, err)
}

// AnswerTicket 直接答复待确认工单并结束，不转出（仅 reviewer / admin）；答复对提交人可见。
func (l *Lingo) AnswerTicket(ctx context.Context, u User, id int64, answer string) (Ticket, error) {
	t, err := l.tickets.Answer(ctx, principal(u), id, answer)
	return l.ticketResult(t, err)
}

// MergeTicket 把待确认工单合并到同项目的工单 target（仅 reviewer / admin）。
func (l *Lingo) MergeTicket(ctx context.Context, u User, id, target int64, note string) (Ticket, error) {
	t, err := l.tickets.Merge(ctx, principal(u), id, target, note)
	return l.ticketResult(t, err)
}

// RejectTicket 驳回待确认工单（仅 reviewer / admin），reason 必填且对提交人可见。
func (l *Lingo) RejectTicket(ctx context.Context, u User, id int64, reason string) (Ticket, error) {
	t, err := l.tickets.Reject(ctx, principal(u), id, reason)
	return l.ticketResult(t, err)
}

// SetAutoForward 在运行时覆盖项目 project 下工单类型 t 是否自动转出（仅 admin），存入数据库并优先于
// Project.AutoForward；u 为操作人，用于权限校验与留痕。
func (l *Lingo) SetAutoForward(ctx context.Context, u User, project string, t TicketType, on bool) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	return publicErr(l.tickets.SetAutoForward(ctx, principal(u), project, string(t), &on))
}

// ResetAutoForward 删除运行时覆盖，恢复为 Project.AutoForward 的默认值（仅 admin）。
func (l *Lingo) ResetAutoForward(ctx context.Context, u User, project string, t TicketType) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	return publicErr(l.tickets.SetAutoForward(ctx, principal(u), project, string(t), nil))
}

// ListMyTickets 返回 u 本人提交的工单，新的在前；status 可空；beforeID 为上一页最后一条的 ID（首页传 0）；
// limit 为 0 时默认 50，最多 200。
func (l *Lingo) ListMyTickets(ctx context.Context, u User, status string, beforeID int64, limit int) ([]Ticket, error) {
	ts, err := l.tickets.ListMine(ctx, principal(u), ticket.MineQuery{Status: status, BeforeID: beforeID, Limit: limit})
	return l.ticketList(ts, err)
}

// ListTicketComments 返回工单评论（时间顺序）：external 只能看到对外评论；无权访问时返回 ErrNotFound。
func (l *Lingo) ListTicketComments(ctx context.Context, u User, id int64) ([]Comment, error) {
	cs, err := l.tickets.Comments(ctx, principal(u), id)
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, Comment{ID: c.ID, Source: c.Source, AuthorID: c.AuthorID, AuthorName: c.AuthorName, Body: c.Body,
			Public: c.Public, At: c.At})
	}
	return out, nil
}

// ListTriage 列出 u 可见项目中的待确认工单（仅 reviewer / admin），按进入待确认的先后。
func (l *Lingo) ListTriage(ctx context.Context, u User, f TriageFilter) ([]Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	ts, err := l.tickets.ListTriage(ctx, principal(u), f.ProjectKey, ticket.TriageQuery{Type: string(f.Type),
		PendingBefore: f.PendingBefore, AfterID: f.AfterID, Limit: f.Limit})
	return l.ticketList(ts, err)
}

// TriageSummary 返回 u 可见项目的待确认数与超时数（仅 reviewer / admin）；超时按 Config.TriageRemindAfter
// 计算（关闭提醒时按 24 小时）。
func (l *Lingo) TriageSummary(ctx context.Context, u User) ([]TriageCount, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	cs, err := l.tickets.TriageSummary(ctx, principal(u), l.overdueAfter())
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]TriageCount, 0, len(cs))
	for _, c := range cs {
		if !l.declared(c.ProjectKey, "") {
			continue
		}
		out = append(out, TriageCount{ProjectKey: c.ProjectKey, Pending: c.Pending, Overdue: c.Overdue})
	}
	return out, nil
}

// ListEvents 返回 ID 大于 afterID 的事件（按 ID 升序，最多 limit 条，0 表示默认 100，最多 200），
// 与 Notifier 推送的事件同源；按 u 的角色与项目过滤：external 只看本人工单的非 triage 事件，agent 看项目内
// 已转出工单的非 triage 事件，reviewer / admin 看项目内全部。用最后一条的 ID 作下次的 afterID。
func (l *Lingo) ListEvents(ctx context.Context, u User, afterID int64, limit int) ([]Event, error) {
	es, err := l.tickets.ListEvents(ctx, principal(u), afterID, limit)
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]Event, 0, len(es))
	for _, e := range es {
		out = append(out, publicEvent(e))
	}
	return out, nil
}

// TicketTypes 返回项目（为空取默认项目）下可提交的工单类型与各自的表单字段，取自仓库 repo（为空取项目默认仓库）
// 的 Issue 表单模板，供接入方弹窗渲染；读取模板失败时只返回类型、不带字段。
func (l *Lingo) TicketTypes(ctx context.Context, u User, projectKey, repo string) ([]TicketTypeInfo, error) {
	if projectKey == "" {
		projectKey = l.cfg.Projects[0].Key
	}
	p := principal(u)
	var proj *Project
	for i := range l.cfg.Projects {
		if l.cfg.Projects[i].Key == projectKey {
			proj = &l.cfg.Projects[i]
		}
	}
	if proj == nil || !p.Known() || !p.InProject(projectKey) {
		return nil, ErrNotFound
	}
	if repo == "" {
		repo = proj.DefaultRepo
		if repo == "" && len(proj.Repos) > 0 {
			repo = proj.Repos[0].Owner + "/" + proj.Repos[0].Name
		}
	}
	if repo != "" && !l.declared(projectKey, repo) {
		return nil, fmt.Errorf("%w: repo %q is not in project %q", ErrInvalidInput, repo, projectKey)
	}
	var tmpls []*gitea.IssueTemplate
	if owner, name, ok := strings.Cut(repo, "/"); ok {
		var err error
		if tmpls, err = l.gitea.ListIssueTemplates(ctx, owner, name); err != nil {
			l.log.Warn("lingo: list issue templates failed", "repo", repo, "err", err)
		}
	}
	cfg := l.repoConfig(repo)
	var out []TicketTypeInfo
	for _, typ := range []TicketType{TicketConsult, TicketBug, TicketFeature} {
		info := TicketTypeInfo{Type: typ}
		if t := gitea.MatchTemplate(tmpls, cfg.Templates[string(typ)], string(typ), l.labelPrefix()); t != nil && t.IsForm() {
			info.Template = t.Name
			for _, f := range t.Fields {
				if !f.IsInput() || f.ID == "" {
					continue
				}
				info.Fields = append(info.Fields, FormField{ID: f.ID, Kind: f.Type, Label: f.Label(), Description: f.Description(),
					Placeholder: f.Placeholder(), Required: f.Required(), Options: f.Options(), Multiple: f.Multiple()})
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// AutoForwardSettings 返回 u 可见项目各工单类型当前生效的自动转出开关（reviewer / admin）。
func (l *Lingo) AutoForwardSettings(ctx context.Context, u User) ([]AutoForwardSetting, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(l.cfg.Projects))
	for _, p := range l.cfg.Projects {
		keys = append(keys, p.Key)
	}
	ss, err := l.tickets.AutoForwardSettings(ctx, principal(u), keys)
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]AutoForwardSetting, 0, len(ss))
	for _, s := range ss {
		out = append(out, AutoForwardSetting{ProjectKey: s.ProjectKey, Type: TicketType(s.Type), On: s.On, Overridden: s.Overridden})
	}
	return out, nil
}

func (l *Lingo) overdueAfter() time.Duration {
	if d := l.cfg.TriageRemindAfter; d > 0 {
		return d
	}
	return 24 * time.Hour
}

func (l *Lingo) ticketList(ts []*ticket.Ticket, err error) ([]Ticket, error) {
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]Ticket, 0, len(ts))
	for _, t := range ts {
		out = append(out, l.publicTicket(t))
	}
	return out, nil
}

// ensureProjects 把 Config.Projects 同步入库，成功一次后不再重复；Handler 与 Run 分进程部署时都能触发。
func (l *Lingo) ensureProjects(ctx context.Context) error {
	l.projectsMu.Lock()
	defer l.projectsMu.Unlock()
	if l.projectsSynced {
		return nil
	}
	if err := l.store.SyncProjects(ctx, projectSpecs(l.cfg.Projects)); err != nil {
		return fmt.Errorf("lingo: sync projects: %w", err)
	}
	l.projectsSynced = true
	return nil
}

func projectSpecs(ps []Project) []project.Spec {
	out := make([]project.Spec, 0, len(ps))
	for _, p := range ps {
		s := project.Spec{Key: p.Key, Name: p.Name, Description: p.Description, DefaultRepo: p.DefaultRepo}
		if s.Name == "" {
			s.Name = p.Key
		}
		for _, r := range p.Repos {
			s.Repos = append(s.Repos, project.RepoSpec{Owner: r.Owner, Name: r.Name, Kind: string(r.Kind)})
		}
		out = append(out, s)
	}
	return out
}

func (l *Lingo) defaultAutoForward(projectKey, typ string) bool {
	for _, p := range l.cfg.Projects {
		if p.Key == projectKey {
			return p.AutoForward[TicketType(typ)]
		}
	}
	return false
}

// declared 报告项目（repo 为空时）或项目下的仓库是否在 Config.Projects 中声明。
func (l *Lingo) declared(projectKey, repo string) bool {
	for _, p := range l.cfg.Projects {
		if p.Key != projectKey {
			continue
		}
		if repo == "" {
			return true
		}
		for _, r := range p.Repos {
			if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
				return true
			}
		}
	}
	return false
}

func (l *Lingo) repoConfig(repo string) worker.RepoConfig {
	for _, p := range l.cfg.Projects {
		for _, r := range p.Repos {
			if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
				return worker.RepoConfig{Assignees: r.Assignees, Templates: r.Templates}
			}
		}
	}
	return worker.RepoConfig{}
}

func principal(u User) auth.Principal {
	return auth.Principal{ID: u.ID, Name: u.Name, Email: u.Email, Role: string(u.Role), Projects: u.Projects}
}

func (l *Lingo) ticketResult(t *ticket.Ticket, err error) (Ticket, error) {
	if err != nil {
		return Ticket{}, publicErr(err)
	}
	return l.publicTicket(t), nil
}

func (l *Lingo) publicTicket(t *ticket.Ticket) Ticket {
	out := Ticket{ID: t.ID, ProjectKey: t.ProjectKey, Repo: t.Repo, Type: TicketType(t.Type), Title: t.Title, Body: t.Body,
		Status: string(t.Status), UserID: t.UserID, UserName: t.UserName, FixedTag: t.FixedTag, DeployedAt: t.DeployedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, TriageSince: t.TriageSince}
	if t.Repo != "" && t.IssueNumber != 0 {
		out.IssueURL = l.issueURL(t.Repo, t.IssueNumber)
	}
	return out
}

// publicErr 把领域错误映射为公开错误，保留原因说明。
func publicErr(err error) error {
	for _, m := range []struct{ domain, public error }{
		{ticket.ErrNotFound, ErrNotFound},
		{ticket.ErrInvalid, ErrInvalidInput},
		{ticket.ErrForbidden, ErrForbidden},
		{ticket.ErrConflict, ErrConflict},
	} {
		if errors.Is(err, m.domain) {
			if m.public == ErrNotFound {
				return ErrNotFound // 不暴露不存在的原因（越权与不存在不区分）
			}
			detail := strings.TrimPrefix(err.Error(), m.domain.Error())
			return fmt.Errorf("%w%s", m.public, detail)
		}
	}
	return err
}
