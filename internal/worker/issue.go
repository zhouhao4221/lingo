package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhouhao4221/lingo/internal/cache"
	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// TaskStore 是 Gitea 写操作任务队列，由 store.Store 实现。
type TaskStore interface {
	ClaimTasks(ctx context.Context, limit int, lease time.Duration) ([]store.Task, error)
	CompleteTask(ctx context.Context, t store.Task) error
	RetryTask(ctx context.Context, t store.Task, reason string, retryAt time.Time) error
	ReleaseTask(ctx context.Context, t store.Task, reason string, retryAt time.Time) error
	FailTask(ctx context.Context, t store.Task, reason string) error
}

// IssueWriter 是建 Issue 用到的 Gitea 接口，由 gitea.Client 实现。
type IssueWriter interface {
	ListIssues(ctx context.Context, owner, repo string, o gitea.ListIssuesOptions) ([]*gitea.Issue, *gitea.Response, error)
	CreateIssue(ctx context.Context, owner, repo string, opt gitea.CreateIssueOption) (*gitea.Issue, error)
	ListLabels(ctx context.Context, owner, repo string, o gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error)
	CreateLabel(ctx context.Context, owner, repo string, opt gitea.CreateLabelOption) (*gitea.Label, error)
	ListIssueTemplates(ctx context.Context, owner, repo string) ([]*gitea.IssueTemplate, error)
	GetIssue(ctx context.Context, owner, repo string, index int64) (*gitea.Issue, error)
	EditIssue(ctx context.Context, owner, repo string, index int64, opt gitea.EditIssueOption) (*gitea.Issue, error)
	CreateComment(ctx context.Context, owner, repo string, index int64, body string) (*gitea.Comment, error)
	EditComment(ctx context.Context, owner, repo string, id int64, body string) (*gitea.Comment, error)
	CreateIssueAttachment(ctx context.Context, owner, repo string, index int64, name string, r io.Reader) (*gitea.Attachment, error)
	CreateCommentAttachment(ctx context.Context, owner, repo string, commentID int64, name string, r io.Reader) (*gitea.Attachment, error)
	RemoveIssueLabel(ctx context.Context, owner, repo string, index, labelID int64) error
	AddIssueLabels(ctx context.Context, owner, repo string, index int64, labelIDs []int64) ([]*gitea.Label, error)
}

// Forwarding 是转出 Issue 时读取的补充评论与附件，由 store.Store 实现。
type Forwarding interface {
	PendingComments(ctx context.Context, ticketID int64) ([]ticket.Comment, error)
	ForwardAttachments(ctx context.Context, ticketID, commentID int64, exclude []string) ([]ticket.Attachment, error)
	MarkAttachment(ctx context.Context, ticketID int64, a ticket.Attachment) error
	Comment(ctx context.Context, id int64) (*ticket.Comment, error)
	CommentPosted(ctx context.Context, id, giteaID int64) error
	PerspectivePosted(ctx context.Context, id, giteaID int64) error
}

// Drafts 读取待建 Issue 的工单并记录建单结果，由 ticket.Service 实现。
type Drafts interface {
	Draft(ctx context.Context, id int64) (*ticket.Ticket, error)
	IssueCreated(ctx context.Context, id, number int64, via string, commentIDs []int64) (bool, error)
	IssueFailed(ctx context.Context, id int64, reason string) error
	// Perspectives 返回工单的需求视角补充（含首段与缺失的必填视角）。
	Perspectives(ctx context.Context, id int64) (*ticket.PerspectiveSet, error)
}

// RepoConfig 是仓库在 lingo.Repo 中的建单配置。
type RepoConfig struct {
	Assignees []string
	Templates map[string]string // 工单类型 → 模板文件名

	DeployWorkflow string // 部署工作流文件名，运行成功即上线
	DeployStatus   string // 提交状态 context，state=success 即上线
	DeployEnv      string // 环境名，默认 production
}

// ProcessTasks 领取并执行一批 Gitea 写操作任务，返回领取到的条数；未配置时返回 0。
func (w *Worker) ProcessTasks(ctx context.Context) (int, error) {
	if w.o.Tasks == nil {
		return 0, nil
	}
	claimed := time.Now()
	ts, err := w.o.Tasks.ClaimTasks(ctx, w.o.BatchSize, w.o.Lease)
	if err != nil {
		return 0, err
	}
	budget := w.batchBudget(claimed)
	for i, t := range ts {
		switch {
		case ctx.Err() != nil:
			w.releaseTask(ctx, t, "worker shutting down")
		case i > 0 && time.Now().After(budget):
			w.releaseTask(ctx, t, "lease budget exhausted")
		default:
			w.handleTask(ctx, t)
		}
	}
	return len(ts), nil
}

func (w *Worker) releaseTask(ctx context.Context, t store.Task, reason string) {
	fctx, cancel := finishContext(ctx)
	defer cancel()
	w.finishTask(t, w.o.Tasks.ReleaseTask(fctx, t, reason, w.o.Now()))
}

func (w *Worker) handleTask(ctx context.Context, t store.Task) {
	tctx, cancel := w.taskContext(ctx)
	err := w.runTask(context.WithValue(tctx, parentCtxKey{}, ctx), t)
	cancel()
	fctx, fcancel := finishContext(ctx)
	defer fcancel()
	log := w.log.With("task", t.OutboxID, "op", t.Op, "ticket", t.TicketID, "attempt", t.Attempts, "failures", t.Failures)
	var de *ticket.DeferError
	var pe *permanentError
	switch {
	case err == nil:
		w.finishTask(t, w.o.Tasks.CompleteTask(fctx, t))
	case errors.As(err, &de):
		log.Debug("task deferred", "until", de.Until, "reason", de.Reason)
		w.finishTask(t, w.o.Tasks.ReleaseTask(fctx, t, de.Reason, de.Until))
	case errors.As(err, &pe) || t.Failures+1 >= w.o.MaxAttempts:
		log.Error("task abandoned", "err", err)
		if t.Op == ticket.OpCreateIssue && t.TicketID != 0 {
			// 工单不能停在 submitted 无人知晓：标记 abnormal 并通知，由后台人员排查
			if ferr := w.o.Drafts.IssueFailed(fctx, t.TicketID, err.Error()); ferr != nil {
				log.Error("mark ticket abnormal failed", "err", ferr)
				w.finishTask(t, w.o.Tasks.RetryTask(fctx, t, ferr.Error(), w.o.Now().Add(backoff(t.Failures+1))))
				return
			}
		}
		w.finishTask(t, w.o.Tasks.FailTask(fctx, t, err.Error()))
	default:
		retryAt := w.o.Now().Add(backoff(t.Failures + 1))
		log.Warn("task failed, will retry", "err", err, "retry_at", retryAt)
		w.finishTask(t, w.o.Tasks.RetryTask(fctx, t, err.Error(), retryAt))
	}
}

func (w *Worker) finishTask(t store.Task, err error) {
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		w.log.Warn("task lease lost", "task", t.OutboxID, "attempt", t.Attempts)
	case err != nil:
		w.log.Error("task update failed", "task", t.OutboxID, "err", err)
	}
}

func (w *Worker) runTask(ctx context.Context, t store.Task) error {
	switch t.Op {
	case ticket.OpCreateIssue:
		var p struct {
			ForwardedBy string   `json:"forwarded_by"`
			Note        string   `json:"note"`
			Exclude     []string `json:"exclude_attachment_ids"` // 后台人员未勾选的附件
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil {
			return permanent(fmt.Errorf("task payload: %w", err))
		}
		return w.createIssue(withFailures(ctx, t.Failures), t.TicketID, p.ForwardedBy, p.Note, p.Exclude, t.Attempts > 1)
	case ticket.OpRemoveLabel:
		var p struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil || p.Label == "" {
			return permanent(fmt.Errorf("task payload: %s", t.Payload))
		}
		return w.removeLabel(ctx, t.TicketID, p.Label)
	case ticket.OpCreateComment:
		var p struct {
			CommentID int64 `json:"comment_id"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil || p.CommentID == 0 {
			return permanent(fmt.Errorf("task payload: %s", t.Payload))
		}
		return w.createComment(withFailures(ctx, t.Failures), t.TicketID, p.CommentID)
	case ticket.OpSyncPerspectives:
		var p struct {
			PerspectiveID int64 `json:"perspective_id"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil || p.PerspectiveID == 0 {
			return permanent(fmt.Errorf("task payload: %s", t.Payload))
		}
		return w.syncPerspectives(ctx, t.TicketID, p.PerspectiveID)
	case ticket.OpCloseIssue:
		return w.closeIssue(ctx, t.TicketID)
	}
	return permanent(fmt.Errorf("unknown task op %q", t.Op))
}

// giteaErr 把 Gitea 错误分为可重试与不可重试（后者直接放弃，避免无意义的重试）。
func giteaErr(what string, err error) error {
	err = fmt.Errorf("%s: %w", what, err)
	if gitea.IsRetryable(err) {
		return err
	}
	return permanent(err)
}

// createIssue 建 Issue 并上传附件；retried 为真表示任务此前被领取过（可能已建好 Issue 但未保存），需先查重。
// Issue 已存在（上次建好后附件未传完）时只继续上传附件。
func (w *Worker) createIssue(ctx context.Context, id int64, forwardedBy, note string, exclude []string, retried bool) error {
	t, err := w.o.Drafts.Draft(ctx, id)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber != 0 {
		if t.Status == ticket.StatusRejected {
			return nil // 已驳回：Issue 由 close_issue 关闭，不再补传附件
		}
		return w.uploadIssueAttachments(ctx, t, exclude)
	}
	// 建单途中被驳回的工单不再建 Issue；但上次可能已建好而没来得及保存，重试时仍按标记查重，查到后补记并关闭
	if t.Status != ticket.StatusSubmitted && (t.Status != ticket.StatusRejected || !retried) {
		return nil // 工单已不再待建
	}
	if t.Repo == "" {
		return permanent(fmt.Errorf("ticket %d has no repo", id))
	}
	bot := w.o.BotLogin()
	if bot == "" {
		return &ticket.DeferError{Until: w.o.Now().Add(botRetry), Reason: "bot login not yet known"}
	}
	owner, name, _ := strings.Cut(t.Repo, "/")

	// 幂等：上次可能已建好 Issue 但没来得及保存（进程崩溃、超时），按隐藏标记查重
	// 关键词搜索会命中 ticket=50 等前缀相同的标记，翻完全部结果；只认正文最后一个标记（灵问追加的那个，
	// 用户文本中的标记在建单时已去掉）。搜索索引有延迟，刚建好的 Issue 可能暂时搜不到，Webhook 的标记补记兜底。
	// 不按来源标签过滤：研发可能已去掉标签。首次执行不可能有先前的 Issue，跳过查重。
	if retried {
		existing, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Issue, *gitea.Response, error) {
			return w.o.Issues.ListIssues(ctx, owner, name, gitea.ListIssuesOptions{ListOptions: o, State: gitea.StateAll,
				Query: gitea.TicketMarkerQuery(id)})
		})
		if err != nil {
			return giteaErr("search existing issue", err)
		}
		for _, is := range existing {
			if is.User != nil && strings.EqualFold(is.User.Login, bot) && gitea.ParseTicketMarker(is.Body) == id {
				// 只把正文里确实写进了的补充评论标记为已转出；建单之后才加的补充仍由 create_comment 单独发
				comments, err := w.o.Forwarding.PendingComments(ctx, id)
				if err != nil {
					return err
				}
				var inBody []ticket.Comment
				for _, c := range comments {
					if strings.Contains(is.Body, stripMarkers(c.Body)) {
						inBody = append(inBody, c)
					}
				}
				if _, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "dedup", commentIDs(inBody)); err != nil {
					return err
				}
				if t.Status == ticket.StatusRejected {
					return nil // IssueCreated 已补记关联并入队关闭
				}
				t.IssueNumber = is.Number
				return w.uploadIssueAttachments(ctx, t, exclude)
			}
		}
	}
	if t.Status != ticket.StatusSubmitted {
		return nil // 已驳回且 Gitea 中没有为它建过 Issue
	}

	var perspectives *ticket.PerspectiveSet
	if t.Type == ticket.TypeFeature {
		if perspectives, err = w.o.Drafts.Perspectives(ctx, id); err != nil {
			return err
		}
	}
	wantLabels := append(issueLabels(w.o.LabelPrefix, t.Type), perspectiveLabels(w.o.LabelPrefix, perspectives)...)
	labels, err := w.labels.ensure(ctx, w.o.Issues, owner, name, wantLabels)
	if err != nil {
		return err
	}
	cfg := w.o.Repos(t.Repo)
	tmpl, err := w.template(ctx, owner, name, t.Type, cfg)
	if err != nil {
		return err
	}
	comments, err := w.o.Forwarding.PendingComments(ctx, id)
	if err != nil {
		return err
	}
	// 后台人员确认过的标题、提交人自己写的标题都保留，只有灵问从正文生成的标题交给模型改写
	draft, original := w.draftIssue(ctx, t, tmpl, forwardedBy != "" || !t.TitleAuto)
	opt := gitea.CreateIssueOption{Title: draft.Title, Body: IssueBody(draft, tmpl, comments, forwardedBy, note, original,
		perspectiveSection(perspectives)), Labels: labels, Assignees: cfg.Assignees}
	if tmpl != nil && tmpl.Title != "" {
		opt.Title = tmpl.Title + stripTitlePrefix(opt.Title, tmpl.Title)
	}
	opt.Title = textutil.Clip(opt.Title, maxIssueTitle)
	is, err := w.o.Issues.CreateIssue(ctx, owner, name, opt)
	var ae *gitea.APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusUnprocessableEntity {
		// 422 常见于缓存的标签 ID 已失效（标签被删除重建）或指派人不存在：刷新标签、不指派再建一次，
		// 不让这类配置变化挡住建单
		w.log.Warn("create issue rejected, retrying with fresh labels and no assignees", "repo", t.Repo, "err", err)
		w.labels.forget(owner, name)
		w.templates.Forget(owner, name)
		if opt.Labels, err = w.labels.ensure(ctx, w.o.Issues, owner, name, wantLabels); err != nil {
			return err
		}
		opt.Assignees = nil
		is, err = w.o.Issues.CreateIssue(ctx, owner, name, opt)
	}
	if err != nil {
		return giteaErr("create issue", err)
	}
	w.drafts.drop(id)
	if _, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "create", commentIDs(comments)); err != nil {
		return err // 重试时会按标记查到这个 Issue，不会重复建
	}
	t.IssueNumber = is.Number
	return w.uploadIssueAttachments(ctx, t, exclude)
}

// draftTimeout 是整理工单的时限；draftReserve 是留给建单本身的任务时间，剩余不足时不整理。
const (
	draftTimeout = 45 * time.Second
	draftReserve = 20 * time.Second
)

// parentCtxKey 在任务 context 中携带可取消的上层 context：退出时停止整理（按原内容建单），不等模型。
type parentCtxKey struct{}

// draftCache 缓存整理结果（按工单与其更新时间，1 小时），建单重试时不重复调用模型、内容保持一致。
type draftCache struct {
	once sync.Once
	c    *cache.Cache[string, drafted]
}

type drafted struct {
	t        *ticket.Ticket
	original string
}

func (c *draftCache) cache() *cache.Cache[string, drafted] {
	c.once.Do(func() { c.c = cache.New[string, drafted](time.Hour, 1000) })
	return c.c
}

func (c *draftCache) key(t *ticket.Ticket) string {
	return strconv.FormatInt(t.ID, 10) + "@" + strconv.FormatInt(t.UpdatedAt.UnixNano(), 10)
}

func (c *draftCache) get(t *ticket.Ticket) (drafted, bool) { return c.cache().Get(c.key(t)) }
func (c *draftCache) put(t *ticket.Ticket, d drafted)      { c.cache().Put(c.key(t), d) }

func (c *draftCache) drop(id int64) {
	prefix := strconv.FormatInt(id, 10) + "@"
	c.cache().DeleteFunc(func(k string) bool { return strings.HasPrefix(k, prefix) })
}

// draftIssue 用轻量模型整理工单（标题、模板字段、描述），返回用于建单的副本与原始描述（只有描述被改写时非空）。
// 未配置模型、剩余任务时间不足、退出中或整理失败时返回原工单与空串（按原内容建单，不影响建单）。
// keepTitle 为真时不改标题。
func (w *Worker) draftIssue(ctx context.Context, t *ticket.Ticket, tmpl *gitea.IssueTemplate, keepTitle bool) (*ticket.Ticket, string) {
	if w.o.Drafter == nil {
		return t, ""
	}
	if d, ok := w.drafts.get(t); ok {
		return d.t, d.original
	}
	timeout := draftTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)-draftReserve)
	}
	parent, _ := ctx.Value(parentCtxKey{}).(context.Context)
	if timeout < 5*time.Second || (parent != nil && parent.Err() != nil) {
		return t, ""
	}
	in := llm.DraftInput{Type: t.Type, Title: t.Title, Body: t.Body, Fields: t.Fields, Context: t.Context, Version: t.Version}
	if tmpl != nil && tmpl.IsForm() {
		in.TitlePrefix = tmpl.Title
		for _, f := range tmpl.Fields {
			if f.IsInput() && f.ID != "" {
				in.Template = append(in.Template, llm.FieldSpec{ID: f.ID, Label: f.Label(), Kind: f.Type, Description: f.Description(),
					Required: f.Required(), Options: f.Options(), Multiple: f.Multiple()})
			}
		}
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if parent != nil {
		stop := context.AfterFunc(parent, cancel)
		defer stop()
	}
	start := time.Now()
	d, usage, err := w.o.Drafter.DraftIssue(dctx, in)
	if !errors.Is(err, llm.ErrNotCalled) { // 没调用到模型（如提示词构造失败）不计用量
		w.recordUsage(ctx, "draft_issue", t.ID, usage, err == nil, time.Since(start))
	}
	if err != nil {
		w.log.Warn("issue drafting failed, using original content", "ticket", t.ID, "err", err)
		return t, ""
	}
	out, original := *t, ""
	if !keepTitle {
		out.Title = d.Title
	}
	if d.Description != "" && d.Description != t.Body {
		out.Body, original = d.Description, t.Body
	}
	out.Fields = d.Fields
	w.drafts.put(t, drafted{t: &out, original: original})
	return &out, original
}

// recordUsage 记录一次模型调用的用量（写库失败只记日志），并在日志中给出前缀缓存命中情况，便于观察提示词前缀是否稳定。
func (w *Worker) recordUsage(ctx context.Context, kind string, ticketID int64, u llm.Usage, ok bool, latency time.Duration) {
	w.log.Debug("llm usage", "kind", kind, "ticket", ticketID, "model", u.Model, "input", u.Input, "cached", u.Cached,
		"output", u.Output, "ok", ok, "latency_ms", latency.Milliseconds())
	if w.o.Usage == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second) // 记账不拖住建单
	defer cancel()
	if err := w.o.Usage.RecordLLMUsage(rctx, store.LLMUsage{Kind: kind, Model: u.Model, TicketID: ticketID,
		Input: u.Input, Output: u.Output, Cached: u.Cached, OK: ok, Latency: latency}); err != nil {
		w.log.Warn("record llm usage failed", "err", err)
	}
}

// stripTitlePrefix 去掉模型可能自带的模板标题前缀（如 "[Bug]: "、"[Bug]"），避免拼成 "[Bug]: [Bug] …"。
func stripTitlePrefix(title, prefix string) string {
	core := strings.TrimRight(prefix, " :：")
	for _, p := range []string{prefix, strings.TrimSpace(prefix), core} {
		if p != "" && strings.HasPrefix(title, p) {
			return strings.TrimLeft(strings.TrimPrefix(title, p), " :：")
		}
	}
	return title
}

func commentIDs(cs []ticket.Comment) []int64 {
	ids := make([]int64, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

// uploadIssueAttachments 转出随 Issue 的附件（exclude 为后台人员未勾选的附件 ID），并重写正文中的附件段落。
// 段落按全部已处理的附件整体生成，重试或部分失败后已转出的引用也不会丢。
func (w *Worker) uploadIssueAttachments(ctx context.Context, t *ticket.Ticket, exclude []string) error {
	owner, name, _ := strings.Cut(t.Repo, "/")
	as, err := w.forwardAttachments(ctx, t.ID, 0, exclude, func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error) {
		return w.o.Issues.CreateIssueAttachment(ctx, owner, name, t.IssueNumber, a.Name, r)
	})
	if err != nil {
		return err
	}
	section := attachmentSection(as)
	if section == "" {
		return nil
	}
	is, err := w.o.Issues.GetIssue(ctx, owner, name, t.IssueNumber)
	if err != nil {
		return giteaErr("get issue", err)
	}
	body := withSection(is.Body, section)
	if body == is.Body {
		return nil
	}
	if _, err := w.o.Issues.EditIssue(ctx, owner, name, t.IssueNumber, gitea.EditIssueOption{Body: &body}); err != nil {
		return giteaErr("edit issue body", err)
	}
	return nil
}

// forwardAttachments 逐个转出待处理的附件，返回本次涉及的全部附件（含之前已处理的）。配置了 FetchAttachment 时读取内容
// 上传为 Gitea 附件，否则只把地址写进正文（linked）。已处理的不会重复处理。
func (w *Worker) forwardAttachments(ctx context.Context, ticketID, commentID int64, exclude []string,
	upload func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error)) ([]ticket.Attachment, error) {
	as, err := w.o.Forwarding.ForwardAttachments(ctx, ticketID, commentID, exclude)
	if err != nil {
		return nil, err
	}
	for i := range as {
		a := &as[i]
		if a.Forwarded() {
			continue
		}
		if err := w.forwardAttachment(ctx, a, upload); err != nil {
			return nil, err
		}
		// 上传可能耗尽了任务时限：记录结果不受影响，否则重试时会重复上传
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		err := w.o.Forwarding.MarkAttachment(mctx, ticketID, *a)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	return as, nil
}

const (
	// maxAttachmentBytes 是从接入方读取的单个附件上限（Gitea 另有自己的限制）。
	maxAttachmentBytes = 100 << 20
	// fetchAttempts 是读取 / 上传附件出现临时错误时随任务重试的次数，之后记为失败（Issue 中写链接）。
	fetchAttempts = 3
)

var errAttachmentTooLarge = errors.New("attachment exceeds 100 MB")

// forwardAttachment 转出一个附件并把结果写进 a。读取被接入方拒绝（ErrNotFound / ErrForbidden）、超过大小上限、
// 被 Gitea 拒收（400 / 403 / 404 / 413 / 415 / 422：类型、大小限制或实例关闭了附件）的记为失败；临时错误返回给调用方
// 随任务重试，重试 fetchAttempts 次后记为失败；其他上传错误（证书等配置问题）让任务失败。
func (w *Worker) forwardAttachment(ctx context.Context, a *ticket.Attachment, upload func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error)) error {
	if w.o.FetchAttachment == nil {
		a.State = ticket.AttachmentLinked
		return nil
	}
	fail := func(reason string) error {
		w.log.Warn("attachment not uploaded", "attachment", a.ID, "file", a.Name, "reason", reason)
		a.State, a.Error = ticket.AttachmentFailed, textutil.Clip(reason, 500)
		return nil
	}
	// transient 处理临时错误：还能重试时返回错误（任务按退避重试），否则记为失败；进程退出时不记
	transient := func(what string, err error) error {
		if parent, ok := ctx.Value(parentCtxKey{}).(context.Context); ok && parent.Err() != nil {
			return parent.Err()
		}
		if taskFailures(ctx)+1 < fetchAttempts {
			return fmt.Errorf("%s attachment %s: %w", what, a.ID, err)
		}
		return fail(what + ": " + err.Error())
	}
	if a.Size > maxAttachmentBytes {
		return fail(errAttachmentTooLarge.Error())
	}
	rc, err := w.o.FetchAttachment(ctx, *a)
	switch {
	case errors.Is(err, ticket.ErrNotFound) || errors.Is(err, ticket.ErrForbidden):
		return fail("fetch: " + err.Error())
	case err != nil:
		return transient("fetch", err)
	}
	lr := &limitedReader{r: rc, n: maxAttachmentBytes}
	ga, err := upload(*a, lr)
	_ = rc.Close()
	var ae *gitea.APIError
	switch {
	case lr.over:
		return fail(errAttachmentTooLarge.Error())
	case err == nil:
		a.State, a.GiteaURL = ticket.AttachmentUploaded, ga.BrowserDownloadURL
		return nil
	case errors.As(err, &ae) && rejectedUpload(ae.StatusCode):
		return fail(err.Error())
	case gitea.IsRetryable(err) || ctx.Err() != nil:
		return transient("upload", err)
	default:
		return giteaErr("upload attachment "+a.ID, err)
	}
}

// limitedReader 读到超过 n 字节时报错（而不是像 io.LimitReader 那样静默截断，截断的文件不能上传）。
type limitedReader struct {
	r    io.Reader
	n    int64
	over bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if l.n -= int64(n); l.n < 0 {
		l.over = true
		return n, errAttachmentTooLarge
	}
	return n, err
}

// rejectedUpload 报告 Gitea 是否按内容或设置拒收了附件：Issue 已由同一机器人创建，403 / 404 只可能是实例关闭了附件。
func rejectedUpload(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

type failuresKey struct{}

// withFailures 记下任务此前的失败次数，供附件读取判断是否还要重试。
func withFailures(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, failuresKey{}, n)
}

func taskFailures(ctx context.Context) int {
	n, _ := ctx.Value(failuresKey{}).(int)
	return n
}

// attachmentSection 按已处理的附件生成正文中的附件段落；没有时为空。
func attachmentSection(as []ticket.Attachment) string {
	var b strings.Builder
	for _, a := range as {
		if !a.Forwarded() {
			continue
		}
		name := escapeMarkdown(a.Name)
		switch {
		case a.State == ticket.AttachmentUploaded && strings.HasPrefix(a.ContentType, "image/"):
			b.WriteString("- ![" + name + "](<" + a.GiteaURL + ">)\n")
		case a.State == ticket.AttachmentUploaded:
			b.WriteString("- [" + name + "](<" + a.GiteaURL + ">)\n")
		case a.URL != "" && a.State == ticket.AttachmentFailed:
			b.WriteString("- [" + name + "](<" + a.URL + ">)（未能上传到 Gitea）\n")
		case a.URL != "":
			b.WriteString("- [" + name + "](<" + a.URL + ">)\n")
		default:
			b.WriteString("- " + name + "（请在业务系统中查看）\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return sectionStart + "\n### 附件\n\n" + b.String() + sectionEnd
}

const (
	sectionStart = "<!-- lingo:attachments -->"
	sectionEnd   = "<!-- /lingo:attachments -->"
)

// withSection 把附件段落写进正文：替换已有段落，否则插在工单标记之前，保证工单标记仍是正文最后一个。
func withSection(body, section string) string {
	return withBlock(body, sectionStart, sectionEnd, section)
}

var mdEscaper = strings.NewReplacer("[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "<", "&lt;", ">", "&gt;")

func escapeMarkdown(s string) string { return mdEscaper.Replace(s) }

// commentAuthor 是补充评论在 Gitea 中的署名：提交人不具名（PRD 3.2.0.6），后台人员注明姓名。
func commentAuthor(c *ticket.Comment) string {
	switch c.Source {
	case "agent":
		return "研发"
	case "staff":
	default:
		return "提交人"
	}
	if c.AuthorName != "" {
		return "灵问后台（" + escapeMarkdown(c.AuthorName) + "）"
	}
	return "灵问后台"
}

// commentHeading 是补充评论在 Gitea 中的标题：研发 Agent 的回复标明已回复提交人，其余为“补充（署名）”。
func commentHeading(c *ticket.Comment) string {
	if c.Source == "agent" {
		return "研发已回复提交人"
	}
	return "补充（" + commentAuthor(c) + "）"
}

// commentText 是机器人发到 Issue 的评论正文；研发 Agent 的回复带对外标记，研发在 Gitea 里能看出这条已转给提交人。
func commentText(c *ticket.Comment) string {
	text := "**" + commentHeading(c) + "：**\n\n" + stripMarkers(c.Body)
	if c.Source == "agent" {
		text += "\n\n" + gitea.PublicMarker
	}
	return text
}

// commentWait 是补充评论等待 Issue 建好时的推迟间隔：建单任务可能在退避重试，不必频繁空转。
const commentWait = time.Minute

// removeLabel 从工单的 Issue 上移除标签；标签或 Issue 不存在时视为完成。
func (w *Worker) removeLabel(ctx context.Context, ticketID int64, label string) error {
	t, err := w.o.Drafts.Draft(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber == 0 {
		return nil
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	for attempt := 0; ; attempt++ {
		id, ok, err := w.labels.lookup(ctx, w.o.Issues, owner, name, label)
		if err != nil || !ok {
			return err
		}
		err = w.o.Issues.RemoveIssueLabel(ctx, owner, name, t.IssueNumber, id)
		switch {
		case errors.Is(err, gitea.ErrNotFound) && attempt == 0:
			w.labels.forget(owner, name) // 缓存的标签 ID 可能已失效（标签被删除重建）：刷新后再试一次
		case errors.Is(err, gitea.ErrNotFound):
			return nil
		case err != nil:
			return giteaErr("remove label "+label, err)
		default:
			return nil
		}
	}
}

// closeIssue 关闭被驳回工单的主 Issue：只处理库中记录的这一个 Issue（工单仓库 + 编号），不按标记搜索、不动 PR。
// Issue 已关闭或已删除时视为完成；工单不再是驳回状态（不应出现）时不关闭。
func (w *Worker) closeIssue(ctx context.Context, ticketID int64) error {
	t, err := w.o.Drafts.Draft(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Status != ticket.StatusRejected || t.IssueNumber == 0 || t.Repo == "" {
		return nil
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	is, err := w.o.Issues.GetIssue(ctx, owner, name, t.IssueNumber)
	if errors.Is(err, gitea.ErrNotFound) {
		return nil // Issue 已被删除：没有可关闭的
	}
	if err != nil {
		return giteaErr("get issue", err)
	}
	if is.State == gitea.StateClosed {
		return nil // 已关闭（重试或研发已手动关闭）：幂等
	}
	state := gitea.StateClosed
	if _, err := w.o.Issues.EditIssue(ctx, owner, name, t.IssueNumber, gitea.EditIssueOption{State: &state}); err != nil {
		if errors.Is(err, gitea.ErrNotFound) {
			return nil
		}
		return giteaErr("close issue", err)
	}
	return nil
}

// createComment 把补充评论由机器人发到 Issue，并上传评论附件；评论已发出时（上次附件未传完）只继续处理附件。
// 建单前的补充已写进 Issue 正文，不再发。
func (w *Worker) createComment(ctx context.Context, ticketID, commentID int64) error {
	// 先读工单再读评论：建单事务同时写入 Issue 编号与评论的 forwarded_at，按这个顺序读，
	// 读到 Issue 编号时一定也能读到 forwarded_at，不会把已写进正文的补充再发一遍
	t, err := w.o.Drafts.Draft(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := w.o.Forwarding.Comment(ctx, commentID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Forwarded {
		return nil
	}
	if t.IssueNumber == 0 {
		if t.Status == ticket.StatusSubmitted {
			return &ticket.DeferError{Until: w.o.Now().Add(commentWait), Reason: "issue not created yet"}
		}
		return nil // 工单未转出（或建单失败）：补充留在灵问
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	body := commentText(c)
	giteaID := c.PostedID
	if giteaID == 0 {
		gc, err := w.o.Issues.CreateComment(ctx, owner, name, t.IssueNumber, body)
		if err != nil {
			return giteaErr("create comment", err)
		}
		if err := w.o.Forwarding.CommentPosted(ctx, c.ID, gc.ID); err != nil {
			return err
		}
		giteaID = gc.ID
	}
	as, err := w.forwardAttachments(ctx, t.ID, c.ID, nil, func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error) {
		return w.o.Issues.CreateCommentAttachment(ctx, owner, name, giteaID, a.Name, r)
	})
	if err != nil {
		return err // 评论已发出并记录：重试时跳过发评论，继续处理剩余附件
	}
	section := attachmentSection(as)
	if section == "" {
		return nil
	}
	if _, err := w.o.Issues.EditComment(ctx, owner, name, giteaID, body+"\n\n"+section); err != nil {
		return giteaErr("edit comment", err)
	}
	return nil
}

// issueLabels 是新建 Issue 要打的标签：来源标签与类型标签（定义同 gitea.LingoLabels，与启动自检补齐的一致）。
func issueLabels(prefix, typ string) []gitea.CreateLabelOption {
	return []gitea.CreateLabelOption{gitea.LingoLabels(prefix)[0], gitea.TypeLabel(prefix, typ, "")}
}

// template 按 Repo.Templates 的文件名、再按模板标签匹配工单类型的 Issue 模板；没有时返回 nil（用内置结构）。
func (w *Worker) template(ctx context.Context, owner, name, typ string, cfg RepoConfig) (*gitea.IssueTemplate, error) {
	ts, err := w.templates.Get(ctx, w.o.Issues, owner, name)
	if err != nil {
		if gitea.IsRetryable(err) {
			return nil, fmt.Errorf("list issue templates: %w", err)
		}
		w.log.Warn("list issue templates failed, using built-in body", "repo", owner+"/"+name, "err", err)
		return nil, nil
	}
	t := gitea.MatchTemplate(ts, cfg.Templates[typ], typ, w.o.LabelPrefix)
	if t == nil && cfg.Templates[typ] != "" {
		w.log.Warn("configured issue template not found", "repo", owner+"/"+name, "template", cfg.Templates[typ])
	}
	return t, nil
}

// IssueBody 生成 Issue 正文：有表单模板且提交了字段时按 Gitea 表单格式渲染，否则用内置结构；
// 末尾附项目、版本、提交环境、确认人与隐藏标记。不写入提交人姓名、邮箱等身份信息（PRD 3.2.0.6）；
// 例外是需求视角区块 perspectives（见 perspectiveSection），写在元信息之前。
// original 非空时（内容经模型整理过）在末尾附上提交人原始描述，整理有误时研发仍能看到原话。
func IssueBody(t *ticket.Ticket, tmpl *gitea.IssueTemplate, comments []ticket.Comment, forwardedBy, note, original, perspectives string) string {
	var b strings.Builder
	fields := map[string][]string{}
	for k, vs := range t.Fields {
		for _, v := range vs {
			fields[k] = append(fields[k], stripMarkers(v))
		}
	}
	body := stripMarkers(t.Body)
	used := map[string]bool{}
	if tmpl != nil && tmpl.IsForm() && len(fields) > 0 {
		b.WriteString(gitea.RenderForm(tmpl, gitea.FormValues(fields)))
		for _, f := range tmpl.Fields {
			used[f.ID] = true
		}
		if body != "" {
			b.WriteString("### 补充说明\n\n" + body + "\n\n")
		}
	} else {
		b.WriteString("### 描述\n\n" + body + "\n\n")
	}
	// 模板中没有的字段（内置结构，或模板改版后旧字段 ID）照原样附上，不丢用户填写的内容
	ids := make([]string, 0, len(fields))
	for id := range fields {
		if !used[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		b.WriteString("### " + id + "\n\n" + strings.Join(fields[id], "\n") + "\n\n")
	}
	for _, c := range comments {
		b.WriteString("### " + commentHeading(&c) + "（" + c.At.UTC().Format("2006-01-02 15:04 UTC") + "）\n\n" + stripMarkers(c.Body) + "\n\n")
	}
	if original = strings.TrimSpace(stripMarkers(original)); original != "" {
		b.WriteString("<details><summary>提交人原始描述</summary>\n\n" + original + "\n\n</details>\n\n")
	}
	if perspectives != "" {
		b.WriteString(perspectives + "\n\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("- 项目：" + t.ProjectKey + "\n")
	if t.Version != "" {
		b.WriteString("- 版本：" + t.Version + "\n")
	}
	keys := make([]string, 0, len(t.Context))
	for k := range t.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := strings.TrimSpace(stripMarkers(t.Context[k])); v != "" {
			b.WriteString("- " + stripMarkers(k) + "：" + v + "\n")
		}
	}
	if forwardedBy != "" {
		b.WriteString("- 由 " + forwardedBy + " 确认转出\n")
	}
	if note = strings.TrimSpace(stripMarkers(note)); note != "" {
		b.WriteString("\n> " + strings.ReplaceAll(note, "\n", "\n> ") + "\n")
	}
	b.WriteString("\n" + gitea.TicketMarker(t.ID) + "\n")
	return b.String()
}

// markerRe 匹配灵问的隐藏标记（lingo:ticket、lingo:public 等，含区块结束标记 /lingo:…）。
var markerRe = regexp.MustCompile(`<!--\s*/?\s*lingo:[^>]*-->`)

// stripMarkers 去掉用户文本中的灵问标记，防止伪造工单关联或对外标记；反复替换直到不变，
// 嵌套构造（去掉内层后外层拼成新标记）也会被清除。
func stripMarkers(s string) string {
	for {
		next := markerRe.ReplaceAllString(s, "")
		if next == s {
			return s
		}
		s = next
	}
}

// maxIssueTitle 是 Gitea Issue 标题上限（字符）。
const maxIssueTitle = 255

// labelCache 缓存各仓库标签 name（小写）→ id（10 分钟），缺的标签创建后更新缓存。
type labelCache struct {
	once  sync.Once
	c     *cache.Cache[string, map[string]int64]
	locks sync.Map // 仓库 → *sync.Mutex：同一仓库的“列出再创建”串行，避免并发建出同名标签
}

func (c *labelCache) lock(key string) func() {
	m, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (c *labelCache) cache() *cache.Cache[string, map[string]int64] {
	c.once.Do(func() { c.c = cache.New[string, map[string]int64](10*time.Minute, 500) })
	return c.c
}

// lookup 返回仓库中标签 label 的 ID（不创建）；缓存未命中时重新列出标签（在锁外进行，不阻塞其他仓库）。
func (c *labelCache) lookup(ctx context.Context, g IssueWriter, owner, name, label string) (int64, bool, error) {
	key := strings.ToLower(owner + "/" + name)
	if ids, ok := c.cache().Get(key); ok {
		if id, ok := ids[strings.ToLower(label)]; ok {
			return id, true, nil
		}
	}
	ids, _, err := gitea.EnsureLabels(ctx, g, owner, name, nil)
	if err != nil {
		return 0, false, giteaErr("list labels", err)
	}
	c.cache().Put(key, ids)
	id, ok := ids[strings.ToLower(label)]
	return id, ok, nil
}

func (c *labelCache) forget(owner, name string) {
	c.cache().Delete(strings.ToLower(owner + "/" + name))
}

// ensure 返回 want 中各标签的 ID，缺的创建。
func (c *labelCache) ensure(ctx context.Context, g IssueWriter, owner, name string, want []gitea.CreateLabelOption) ([]int64, error) {
	key := strings.ToLower(owner + "/" + name)
	defer c.lock(key)()
	ids, ok := c.cache().Get(key)
	if ok {
		for _, l := range want {
			if _, has := ids[strings.ToLower(l.Name)]; !has {
				ok = false
				break
			}
		}
	}
	if !ok {
		var err error
		if ids, _, err = gitea.EnsureLabels(ctx, g, owner, name, want); err != nil {
			return nil, giteaErr("ensure labels", err)
		}
		c.cache().Put(key, ids)
	}
	out := make([]int64, 0, len(want))
	for _, l := range want {
		out = append(out, ids[strings.ToLower(l.Name)])
	}
	return out, nil
}
