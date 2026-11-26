package lingo

import (
	"context"
	"errors"
	"fmt"
	"github.com/zhouhao4221/lingo/internal/knowledge"
	"github.com/zhouhao4221/lingo/internal/textutil"
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

// ListVisibleTickets 返回 u 在 User.Projects 内可查看的工单：本人提交的，加上 Config.TicketAudience 放宽可见的提交人的
// （新的在前，只读；同事的工单 Mine 为 false），可按项目与状态筛选。与 ListMyTickets 不同，结果限定在 User.Projects 内
// （ListMyTickets 返回本人的全部工单）；未配置 TicketAudience 或非 external 时只含本人的。
func (l *Lingo) ListVisibleTickets(ctx context.Context, u User, f VisibleTicketFilter) ([]Ticket, error) {
	ts, err := l.tickets.ListVisible(ctx, l.viewer(ctx, u), f.Project,
		ticket.VisibleQuery{Status: f.Status, BeforeID: f.BeforeID, Limit: f.Limit})
	return l.ticketList(u, ts, err)
}

// ListTickets 返回 u 可查看的工单的一页与总数，供接入方的反馈管理列表使用（REQ-010），可按项目、仓库、类型、状态、关键词、
// 时间筛选（新的在前，游标分页）。可见范围与 GetTicket、知识问答检索相同，不放宽任何角色：
//   - external：默认只含本人提交的；IncludeShared 为真时另含 Config.TicketAudience 放宽可见的同事工单（只读）；
//   - agent：只含 User.Projects 内已转出的工单（Mine 为真时为空）；
//   - reviewer / admin：User.Projects 内全部（User.Projects 为空时为 Config.Projects 全部）；Mine 为真时只含本人提交的。
//
// Project 不在调用者范围内时返回空页（不区分项目不存在与无权访问）；角色未知或 external 没有 User.ID 时返回 ErrForbidden；
// 条件不合法（类型、状态、时间字段、仓库格式、关键词过多或过长、提交人姓名过长、排序或游标不合法）时返回 ErrInvalidInput。
//
// Sort 为 TicketSortTriageSince 时只含当前待确认的工单，按实际进入待确认的时间（triage_since）从早到晚、同一时间按 ID
// 从小到大，游标为 (triage_since, id)，用 Cursor / NextCursor 翻页：无重复，翻页开始前已提交且仍在待确认的不会遗漏，翻页期间
// 新进入的通常排在末尾但不保证出现在本轮翻页里（triage_since 取应用时钟，并发提交或多实例时钟偏差时可能落在已翻过的位置）；
// 默认排序与 BeforeID / NextBefore 不变（REQ-010 增补，待办 #789）。
func (l *Lingo) ListTickets(ctx context.Context, u User, f TicketListFilter) (TicketPage, error) {
	q := ticket.SearchQuery{Repo: f.Repo, Type: string(f.Type), Statuses: f.Statuses, Keywords: strings.Fields(f.Keyword),
		TimeField: f.TimeField, From: f.From, To: f.To, BeforeID: f.BeforeID, Limit: f.Limit,
		SubmitterName: strings.TrimSpace(f.SubmitterName), Order: f.Sort}
	if f.Cursor != "" {
		if f.Sort != TicketSortTriageSince {
			return TicketPage{}, fmt.Errorf("%w: cursor requires sort=%s", ErrInvalidInput, TicketSortTriageSince)
		}
		var err error
		if q.AfterTriage, q.AfterID, err = ticket.ParseTriageCursor(f.Cursor); err != nil {
			return TicketPage{}, publicErr(err)
		}
	}
	if f.Repo != "" {
		if owner, name, ok := strings.Cut(f.Repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return TicketPage{}, fmt.Errorf("%w: repo %q must be owner/name", ErrInvalidInput, f.Repo)
		}
	}
	if err := l.ensureProjects(ctx); err != nil {
		return TicketPage{}, err
	}
	p, mine := l.principal(u), f.Mine
	if u.Role == RoleExternal && !f.Mine {
		if f.IncludeShared {
			p = l.viewer(ctx, u) // 只在要看同事工单时调用 TicketAudience
		} else {
			mine = true
		}
	}
	ts, total, more, err := l.tickets.List(ctx, p, f.Project, mine, q)
	out, err := l.ticketList(u, ts, err)
	if err != nil {
		return TicketPage{}, err
	}
	page := TicketPage{Tickets: out, Total: total}
	switch {
	case !more || len(ts) == 0:
	case q.Order == ticket.OrderTriageAsc:
		page.NextCursor = ticket.TriageCursor(ts[len(ts)-1])
	default:
		page.NextBefore = out[len(out)-1].ID
	}
	return page, nil
}

// resyncBotWait 是重新同步评论时等待机器人账号获取完成的最长时间。
const resyncBotWait = 5 * time.Second

// ResyncTicketComments 按当前 GiteaConfig.ReplyMode 重新读取工单 id 关联的主 Issue 的全部 Gitea 评论并写回工单评论
// （REQ-011），用于切换 ReplyMode（如改为 ReplyAll）后回补此前已保存为内部的历史评论：定时对账的游标只前进，不会重读旧评论。
//
// 只允许 reviewer / admin，且工单须在 u 的项目范围内（User.Projects，为空时为 Config.Projects 全部）：其他角色返回
// ErrForbidden（先于查找），工单不存在或不在范围内返回 ErrNotFound；工单没有关联的主 Issue（待确认、待建单、已合并、
// 直接答复等）、Gitea 未配置、机器人账号暂未获取（稍后重试）、或 Issue 在 Gitea 上已不存在时返回 ErrConflict。
//
// 逐条与 Webhook / 定时对账同一套处理：跳过机器人账号的评论，按 ReplyMode 判定是否对外、去掉 /reply 前缀，按 Gitea
// 评论 ID 去重，旧于已保存版本的不覆盖，灵工已记为删除的不恢复；只写入该工单。默认不发通知（o.Notify 为真时同 Webhook）；
// 每条有变化的评论记一条审计（delivery 为 resync）。幂等：重复调用不再产生变化（全部计入 Unchanged）。
// 不会因为 Gitea 列表中缺少某条评论而把它标记为删除（删除仍以 Webhook 为准）；只读 Gitea，不写 Gitea。
func (l *Lingo) ResyncTicketComments(ctx context.Context, u User, id int64, o CommentResyncOptions) (CommentResync, error) {
	l.touchSettings()
	t, err := l.tickets.ResyncTarget(ctx, l.principal(u), id)
	if err != nil {
		return CommentResync{}, publicErr(err)
	}
	if !l.gitea.Configured() {
		return CommentResync{}, fmt.Errorf("%w: Gitea is not configured", ErrConflict)
	}
	deadline := time.Now().Add(resyncBotWait)
	for l.botLogin() == "" && time.Now().Before(deadline) { // 首次调用时机器人账号可能仍在后台获取
		select {
		case <-ctx.Done():
			return CommentResync{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	r, err := l.worker.ResyncIssueComments(ctx, t.ID, t.Repo, t.IssueNumber, o.Notify)
	switch {
	case errors.Is(err, worker.ErrNotReady):
		return CommentResync{}, fmt.Errorf("%w: Gitea is not configured", ErrConflict)
	case errors.Is(err, worker.ErrBotUnknown):
		return CommentResync{}, fmt.Errorf("%w: Gitea bot account not yet known, retry later", ErrConflict)
	case errors.Is(err, gitea.ErrNotFound):
		return CommentResync{}, fmt.Errorf("%w: linked Gitea issue not found", ErrConflict)
	case err != nil:
		return CommentResync{}, fmt.Errorf("lingo: resync ticket %d comments: %w", t.ID, err)
	}
	l.log.Info("ticket comments resynced", "ticket", t.ID, "by", u.ID, "added", r.Added, "updated", r.Updated,
		"published", r.Published, "unpublished", r.Unpublished, "notify", o.Notify) // 留痕
	return CommentResync{TicketID: t.ID, Repo: t.Repo, Issue: t.IssueNumber, Fetched: r.Fetched, Bot: r.Bot, Added: r.Added,
		Updated: r.Updated, Unchanged: r.Unchanged, Skipped: r.Skipped, Published: r.Published, Unpublished: r.Unpublished,
		Notified: o.Notify && r.Published > 0}, nil
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

// CommentInput 是 AddTicketCommentWith 的输入：Knowledge 非 nil 时把这条评论加入知识库（仅 reviewer / admin，直接生效）。
// Knowledge 的问题与答案为空时分别取工单标题与正文、评论原文；建议改写为通用的问题与答案。
type CommentInput struct {
	Body        string
	Attachments []Attachment
	Knowledge   *KnowledgeDraft
}

// AddTicketCommentWith 同 AddTicketComment，另可在同一事务内把评论加入知识库（REQ-007）。非 reviewer / admin 要求入库时
// 返回 ErrForbidden；知识内容不合法时返回 ErrInvalidInput，评论也不发表。
func (l *Lingo) AddTicketCommentWith(ctx context.Context, u User, id int64, in CommentInput) (Comment, error) {
	if in.Knowledge == nil {
		return l.AddTicketComment(ctx, u, id, in.Body, in.Attachments)
	}
	p := l.principal(u)
	if !p.Staff() {
		return Comment{}, fmt.Errorf("%w: only reviewers and admins can add comments to the knowledge base", ErrForbidden)
	}
	kd := *in.Knowledge
	c, err := l.tickets.AddCommentWith(ctx, p, id, in.Body, internalAttachments(in.Attachments),
		func(ctx context.Context, tx ticket.Tx, t *ticket.Ticket, c *ticket.Comment) error {
			w, ok := tx.(knowledge.Writer)
			if !ok {
				return errors.New("lingo: store does not support knowledge")
			}
			d := knowledge.Draft{ProjectKey: t.ProjectKey, Repo: kd.Repo, Question: strings.TrimSpace(kd.Question), Answer: strings.TrimSpace(kd.Answer)}
			if d.Question == "" {
				d.Question = textutil.ClipEllipsis(knowledge.Scrub(oneLineText(t.Title+"："+t.Body)), knowledge.MaxQuestionRunes)
			}
			if d.Answer == "" {
				d.Answer = textutil.ClipEllipsis(knowledge.Scrub(c.Body), knowledge.MaxAnswerRunes)
			}
			d, err := knowledge.Prepare(d)
			if err != nil {
				return err
			}
			_, err = w.InsertKnowledge(ctx, knowledge.NewFromComment(d, t.ID, p.ID))
			return err
		})
	if err != nil {
		return Comment{}, publicErr(err)
	}
	l.kickEmbed()
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
