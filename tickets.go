package lingo

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// Attachments 是接入方附件系统中的附件引用（单个工单含补充最多 10 个）。
	Attachments []Attachment
}

// ForwardOptions 是后台人员转出工单时可修改的内容；零值表示不修改。
type ForwardOptions struct {
	Title string
	Type  TicketType
	Repo  string // "owner/name"，须属于工单所在项目
	Note  string // 补充说明，写入 Issue
	// AttachmentIDs 是要随 Issue 转出的附件 ID（截图可能含敏感信息，由后台人员勾选；含转出前补充评论的附件，附件 ID
	// 在整个工单内唯一）；nil 表示全部，空切片表示不转出。转出之后提交人再补充的附件不受勾选限制。
	AttachmentIDs *[]string
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
	t, err := l.tickets.Create(ctx, l.principal(u), ticket.NewTicket{Project: in.Project, Repo: in.Repo, Type: string(in.Type),
		Title: in.Title, Body: in.Content, Fields: in.Fields, Context: in.Context, Version: in.Version,
		Attachments: internalAttachments(in.Attachments)})
	return l.ticketResult(u, t, err)
}

// GetTicket 返回工单 id；无权访问时与不存在一样返回 ErrNotFound（见数据隔离）。external 还能查看 Config.TicketAudience
// 放宽可见的同事工单（只读，Mine 为 false）。
func (l *Lingo) GetTicket(ctx context.Context, u User, id int64) (Ticket, error) {
	t, err := l.tickets.View(ctx, l.viewer(ctx, u), id)
	return l.ticketResult(u, t, err)
}

// ForwardTicket 把待确认工单转出到 Gitea（仅 reviewer / admin）。
func (l *Lingo) ForwardTicket(ctx context.Context, u User, id int64, o ForwardOptions) (Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return Ticket{}, err
	}
	t, err := l.tickets.Forward(ctx, l.principal(u), id, ticket.ForwardInput{Title: o.Title, Type: string(o.Type), Repo: o.Repo, Note: o.Note,
		Attachments: o.AttachmentIDs})
	return l.ticketResult(u, t, err)
}

// AnswerTicket 直接答复待确认工单并结束，不转出（仅 reviewer / admin）；答复对提交人可见。
func (l *Lingo) AnswerTicket(ctx context.Context, u User, id int64, answer string) (Ticket, error) {
	t, err := l.tickets.Answer(ctx, l.principal(u), id, answer)
	return l.ticketResult(u, t, err)
}

// MergeTicket 把待确认工单合并到同项目的工单 target（仅 reviewer / admin）。
func (l *Lingo) MergeTicket(ctx context.Context, u User, id, target int64, note string) (Ticket, error) {
	t, err := l.tickets.Merge(ctx, l.principal(u), id, target, note)
	return l.ticketResult(u, t, err)
}

// RejectTicket 驳回待确认工单（仅 reviewer / admin），reason 必填且对提交人可见。
func (l *Lingo) RejectTicket(ctx context.Context, u User, id int64, reason string) (Ticket, error) {
	t, err := l.tickets.Reject(ctx, l.principal(u), id, reason)
	return l.ticketResult(u, t, err)
}

// RejectForwardedTicket 驳回已转研发的工单（仅 reviewer / admin），reason 必填且对提交人可见（ticket.status 事件正文）。
// 可驳回的状态：submitted、open、in_progress、need_info、abnormal；已修复（fixed / released / deployed）、已关闭的
// 返回 ErrConflict，待确认工单请用 RejectTicket。工单、评论、历史全部保留，状态改为 rejected；工单关联了主 Issue 时
// 由 Run 异步关闭该 Issue（持久化、失败按退避重试，同一工单只入队一次），只关闭这一个 Issue：关闭前在其上留一条驳回原因评论
// （只署 u.Name，不含提交人信息，重试沿用原原因），不改标签、不动 PR。有补充评论正在发往 Gitea 时驳回不会先于它提交：
// 最多等待 5 秒，仍在发送则返回 ErrConflict，稍后重试即可；驳回先提交时排队中的补充评论不再发出。
// 驳回后 Gitea 侧的关闭、重开、删除与对账都不再改变工单状态。对已驳回的工单重复调用返回当前工单，不重复通知或入队。
// 关闭进度用 TicketIssueClose 查询，失败后用 RetryTicketIssueClose 重新入队。
func (l *Lingo) RejectForwardedTicket(ctx context.Context, u User, id int64, reason string) (Ticket, error) {
	t, err := l.tickets.RejectForwarded(ctx, l.principal(u), id, reason)
	return l.ticketResult(u, t, err)
}

// TicketIssueClose 返回工单驳回后关闭关联 Issue 的进度（仅 reviewer / admin）；不改变任何数据。
func (l *Lingo) TicketIssueClose(ctx context.Context, u User, id int64) (IssueClose, error) {
	c, err := l.tickets.IssueClose(ctx, l.principal(u), id)
	return publicIssueClose(c, err)
}

// RetryTicketIssueClose 在关闭关联 Issue 失败（State 为 failed）后重新入队一轮（仅 reviewer / admin），返回入队后的进度；
// 任务仍在排队或已完成时不重复入队，直接返回当前进度；工单没有要关闭的 Issue 时返回 ErrConflict。
func (l *Lingo) RetryTicketIssueClose(ctx context.Context, u User, id int64) (IssueClose, error) {
	c, err := l.tickets.RetryIssueClose(ctx, l.principal(u), id)
	return publicIssueClose(c, err)
}

func publicIssueClose(c *ticket.IssueClose, err error) (IssueClose, error) {
	if err != nil {
		return IssueClose{}, publicErr(err)
	}
	return IssueClose{TicketID: c.TicketID, Repo: c.Repo, Number: c.Number, State: c.State, Attempt: c.Attempt,
		Failures: c.Failures, LastError: c.LastError, UpdatedAt: c.UpdatedAt}, nil
}

// SetAutoForward 在运行时覆盖项目 project 下工单类型 t 是否自动转出（仅 admin），存入数据库并优先于
// Project.AutoForward；u 为操作人，用于权限校验与留痕。
func (l *Lingo) SetAutoForward(ctx context.Context, u User, project string, t TicketType, on bool) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	if err := l.tickets.SetAutoForward(ctx, l.principal(u), project, string(t), &on); err != nil {
		return publicErr(err)
	}
	l.log.Info("auto-forward override set", "project", project, "type", t, "on", on, "by", u.ID) // 开关变更留痕
	return nil
}

// ResetAutoForward 删除运行时覆盖，恢复为 Project.AutoForward 的默认值（仅 admin）。
func (l *Lingo) ResetAutoForward(ctx context.Context, u User, project string, t TicketType) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	if err := l.tickets.SetAutoForward(ctx, l.principal(u), project, string(t), nil); err != nil {
		return publicErr(err)
	}
	l.log.Info("auto-forward override reset", "project", project, "type", t, "by", u.ID)
	return nil
}

// ListMyTickets 返回 u 本人提交的工单，新的在前；status 可空；beforeID 为上一页最后一条的 ID（首页传 0）；
// limit 为 0 时默认 50，最多 200。
func (l *Lingo) ListMyTickets(ctx context.Context, u User, status string, beforeID int64, limit int) ([]Ticket, error) {
	ts, err := l.tickets.ListMine(ctx, l.principal(u), ticket.MineQuery{Status: status, BeforeID: beforeID, Limit: limit})
	return l.ticketList(u, ts, err)
}

// ListVisibleTickets 返回 u 本人提交的工单，加上 Config.TicketAudience 放宽可见的提交人在 u 项目内的工单（新的在前，
// 只读；同事的工单 Mine 为 false）。未配置 TicketAudience 或非 external 时与 ListMyTickets 相同（可按项目筛选）。
func (l *Lingo) ListVisibleTickets(ctx context.Context, u User, f VisibleTicketFilter) ([]Ticket, error) {
	ts, err := l.tickets.ListVisible(ctx, l.viewer(ctx, u), f.Project,
		ticket.VisibleQuery{Status: f.Status, BeforeID: f.BeforeID, Limit: f.Limit})
	return l.ticketList(u, ts, err)
}

// ticketDetail 返回工单、评论与附件（一次鉴权），供 REST 详情接口使用。
func (l *Lingo) ticketDetail(ctx context.Context, u User, id int64) (Ticket, []Comment, []Attachment, error) {
	t, cs, as, err := l.tickets.Detail(ctx, l.viewer(ctx, u), id)
	if err != nil {
		return Ticket{}, nil, nil, publicErr(err)
	}
	out := make([]Attachment, 0, len(as))
	for _, a := range as {
		out = append(out, publicAttachment(a))
	}
	return l.publicTicketFor(u, t), publicComments(cs), out, nil
}

func publicAttachment(a ticket.Attachment) Attachment {
	return Attachment{ID: a.ID, Name: a.Name, URL: a.URL, ContentType: a.ContentType, Size: a.Size, CommentID: a.CommentID,
		Forwarded: a.Forwarded(), GiteaURL: a.GiteaURL, Error: a.Error}
}

func internalAttachments(as []Attachment) []ticket.Attachment {
	if len(as) == 0 {
		return nil
	}
	out := make([]ticket.Attachment, 0, len(as))
	for _, a := range as {
		out = append(out, ticket.Attachment{ID: a.ID, Name: a.Name, URL: a.URL, ContentType: a.ContentType, Size: a.Size})
	}
	return out
}

// AddTicketComment 在工单上写一条对提交人可见的评论：提交人本人补充、reviewer / admin 答复（二者可带附件引用），
// 或研发 Agent 回复提交人（agent 只能回复已有 Gitea Issue 的工单，否则返回 ErrConflict；不能带附件）。
// 工单已转出时由机器人同步为 Issue 评论；提交人在 need_info 时补充，工单回到 in_progress。
func (l *Lingo) AddTicketComment(ctx context.Context, u User, id int64, body string, attachments []Attachment) (Comment, error) {
	c, err := l.tickets.AddComment(ctx, l.principal(u), id, body, internalAttachments(attachments))
	if err != nil {
		return Comment{}, publicErr(err)
	}
	return publicComments([]ticket.Comment{*c})[0], nil
}

func publicComments(cs []ticket.Comment) []Comment {
	out := make([]Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, Comment{ID: c.ID, Source: c.Source, AuthorID: c.AuthorID, AuthorName: c.AuthorName, Body: c.Body,
			Public: c.Public, At: c.At})
	}
	return out
}

// ListTicketComments 返回工单评论（时间顺序）：external 只能看到对外评论；无权访问时返回 ErrNotFound。
func (l *Lingo) ListTicketComments(ctx context.Context, u User, id int64) ([]Comment, error) {
	cs, err := l.tickets.Comments(ctx, l.viewer(ctx, u), id)
	if err != nil {
		return nil, publicErr(err)
	}
	return publicComments(cs), nil
}

// ListTriage 列出 u 可见项目中的待确认工单（仅 reviewer / admin），按进入待确认的先后。
func (l *Lingo) ListTriage(ctx context.Context, u User, f TriageFilter) ([]Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	ts, err := l.tickets.ListTriage(ctx, l.principal(u), f.ProjectKey, ticket.TriageQuery{Type: string(f.Type),
		PendingBefore: f.PendingBefore, AfterID: f.AfterID, Limit: f.Limit})
	return l.ticketList(u, ts, err)
}

// TriageSummary 返回 u 可见项目的待确认数与超时数（仅 reviewer / admin）；超时按 Config.TriageRemindAfter
// 计算（关闭提醒时按 24 小时）。
func (l *Lingo) TriageSummary(ctx context.Context, u User) ([]TriageCount, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	cs, err := l.tickets.TriageSummary(ctx, l.principal(u), l.overdueAfter())
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
	es, err := l.tickets.ListEvents(ctx, l.principal(u), afterID, limit)
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
	p := l.principal(u)
	proj := l.project(projectKey)
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
	if owner, name, ok := strings.Cut(repo, "/"); ok && l.gitea.Configured() {
		var err error
		if tmpls, err = l.tmpls.Get(ctx, l.gitea, owner, name); err != nil {
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
	ss, err := l.tickets.AutoForwardSettings(ctx, l.principal(u), l.projectKeys())
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]AutoForwardSetting, 0, len(ss))
	for _, s := range ss {
		out = append(out, AutoForwardSetting{ProjectKey: s.ProjectKey, Type: TicketType(s.Type), On: s.On, Overridden: s.Overridden})
	}
	return out, nil
}

func (l *Lingo) ticketList(u User, ts []*ticket.Ticket, err error) ([]Ticket, error) {
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]Ticket, 0, len(ts))
	for _, t := range ts {
		out = append(out, l.publicTicketFor(u, t))
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

// projectKeys 返回 Config 中声明的全部项目 key。
func (l *Lingo) projectKeys() []string {
	keys := make([]string, 0, len(l.cfg.Projects))
	for _, p := range l.cfg.Projects {
		keys = append(keys, p.Key)
	}
	return keys
}

// project 返回声明的项目；不存在时为 nil。
func (l *Lingo) project(key string) *Project {
	for i := range l.cfg.Projects {
		if l.cfg.Projects[i].Key == key {
			return &l.cfg.Projects[i]
		}
	}
	return nil
}

// declared 报告项目（repo 为空时）或项目下的仓库是否在 Config.Projects 中声明。
func (l *Lingo) declared(projectKey, repo string) bool {
	p := l.project(projectKey)
	if p == nil {
		return false
	}
	if repo == "" {
		return true
	}
	for _, r := range p.Repos {
		if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
			return true
		}
	}
	return false
}

func (l *Lingo) repoConfig(repo string) worker.RepoConfig {
	for _, p := range l.cfg.Projects {
		for _, r := range p.Repos {
			if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
				return worker.RepoConfig{Assignees: r.Assignees, Templates: r.Templates,
					DeployWorkflow: r.Deploy.Workflow, DeployStatus: r.Deploy.StatusContext, DeployEnv: r.Deploy.Env}
			}
		}
	}
	return worker.RepoConfig{}
}

// principal 转换调用者身份；User.Projects 为空时限定为 Config 中声明的全部项目，
// 使所有查询都只覆盖当前声明的项目（数据库中的项目不会因配置删除而停用）。
func (l *Lingo) principal(u User) auth.Principal {
	ps := u.Projects
	if len(ps) == 0 {
		ps = l.projectKeys()
	}
	return auth.Principal{ID: u.ID, Name: u.Name, Email: u.Email, Role: string(u.Role), Projects: ps,
		Perspective: string(u.Perspective)}
}

func (l *Lingo) ticketResult(u User, t *ticket.Ticket, err error) (Ticket, error) {
	if err != nil {
		return Ticket{}, publicErr(err)
	}
	return l.publicTicketFor(u, t), nil
}

// publicTicketFor 同 publicTicket，并按调用者填写 Mine。
func (l *Lingo) publicTicketFor(u User, t *ticket.Ticket) Ticket {
	out := l.publicTicket(t)
	out.Mine = u.ID != "" && t.UserID == u.ID
	return out
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
