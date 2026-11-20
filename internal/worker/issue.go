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
}

// Forwarding 是转出 Issue 时读取的补充评论与附件，由 store.Store 实现。
type Forwarding interface {
	PendingComments(ctx context.Context, ticketID int64) ([]ticket.Comment, error)
	ForwardAttachments(ctx context.Context, ticketID, commentID int64, ids *[]string) ([]ticket.Attachment, error)
	MarkAttachment(ctx context.Context, ticketID int64, a ticket.Attachment) error
	Comment(ctx context.Context, id int64) (*ticket.Comment, error)
	CommentPosted(ctx context.Context, id, giteaID int64) error
}

// Drafts 读取待建 Issue 的工单并记录建单结果，由 ticket.Service 实现。
type Drafts interface {
	Draft(ctx context.Context, id int64) (*ticket.Ticket, error)
	IssueCreated(ctx context.Context, id, number int64, via string, commentIDs []int64) (bool, error)
	IssueFailed(ctx context.Context, id int64, reason string) error
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
			ForwardedBy   string    `json:"forwarded_by"`
			Note          string    `json:"note"`
			AttachmentIDs *[]string `json:"attachment_ids"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil {
			return permanent(fmt.Errorf("task payload: %w", err))
		}
		return w.createIssue(ctx, t.TicketID, p.ForwardedBy, p.Note, p.AttachmentIDs, t.Attempts > 1)
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
		return w.createComment(ctx, t.TicketID, p.CommentID)
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
func (w *Worker) createIssue(ctx context.Context, id int64, forwardedBy, note string, attachments *[]string, retried bool) error {
	t, err := w.o.Drafts.Draft(ctx, id)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber != 0 {
		return w.uploadIssueAttachments(ctx, t, attachments)
	}
	if t.Status != ticket.StatusSubmitted {
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
				t.IssueNumber = is.Number
				return w.uploadIssueAttachments(ctx, t, attachments)
			}
		}
	}

	wantLabels := issueLabels(w.o.LabelPrefix, t.Type)
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
	opt := gitea.CreateIssueOption{Title: draft.Title, Body: IssueBody(draft, tmpl, comments, forwardedBy, note, original), Labels: labels, Assignees: cfg.Assignees}
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
	return w.uploadIssueAttachments(ctx, t, attachments)
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

// uploadIssueAttachments 转出随 Issue 的附件（attachments 为后台人员勾选的工单附件 ID，nil 表示全部），
// 并重写正文中的附件段落。段落按全部已处理的附件整体生成，重试或部分失败后已转出的引用也不会丢。
func (w *Worker) uploadIssueAttachments(ctx context.Context, t *ticket.Ticket, attachments *[]string) error {
	owner, name, _ := strings.Cut(t.Repo, "/")
	as, err := w.forwardAttachments(ctx, t.ID, 0, attachments, func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error) {
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

// forwardAttachments 逐个转出待处理的附件，返回本次涉及的全部附件（含之前已处理的）。配置了 Fetch 时读取内容
// 上传为 Gitea 附件，否则只把地址写进正文（linked）。读取失败、Gitea 按内容拒收（400 / 413 / 415 / 422：类型、
// 大小限制）的记为 failed 不再重试；可重试的错误返回给调用方重试；其他上传错误（权限、证书等配置问题）让任务失败，
// 不把附件误记为失败。已处理的不会重复处理。
func (w *Worker) forwardAttachments(ctx context.Context, ticketID, commentID int64, ids *[]string,
	upload func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error)) ([]ticket.Attachment, error) {
	as, err := w.o.Forwarding.ForwardAttachments(ctx, ticketID, commentID, ids)
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
		if err := w.o.Forwarding.MarkAttachment(ctx, ticketID, *a); err != nil {
			return nil, err
		}
	}
	return as, nil
}

func (w *Worker) forwardAttachment(ctx context.Context, a *ticket.Attachment, upload func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error)) error {
	if w.o.FetchAttachment == nil {
		a.State = ticket.AttachmentLinked
		return nil
	}
	rc, err := w.o.FetchAttachment(ctx, *a)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.log.Warn("fetch attachment failed", "attachment", a.ID, "file", a.Name, "err", err)
		a.State, a.Error = ticket.AttachmentFailed, textutil.Clip("fetch: "+err.Error(), 500)
		return nil
	}
	ga, err := upload(*a, rc)
	_ = rc.Close()
	var ae *gitea.APIError
	switch {
	case err == nil:
		a.State, a.GiteaURL = ticket.AttachmentUploaded, ga.BrowserDownloadURL
	case errors.As(err, &ae) && rejectedByContent(ae.StatusCode):
		w.log.Warn("attachment rejected by gitea", "attachment", a.ID, "file", a.Name, "err", err)
		a.State, a.Error = ticket.AttachmentFailed, textutil.Clip(err.Error(), 500)
	default:
		return giteaErr("upload attachment "+a.ID, err)
	}
	return nil
}

func rejectedByContent(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
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
	if i := strings.Index(body, sectionStart); i >= 0 {
		if j := strings.Index(body[i:], sectionEnd); j >= 0 {
			return body[:i] + section + body[i+j+len(sectionEnd):]
		}
	}
	if i := strings.LastIndex(body, "<!-- lingo:ticket="); i >= 0 {
		return strings.TrimRight(body[:i], "\n") + "\n\n" + section + "\n\n" + body[i:]
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section
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
// 末尾附项目、版本、提交环境、确认人与隐藏标记。不写入提交人姓名、邮箱等身份信息（PRD 3.2.0.6）。
// original 非空时（内容经模型整理过）在末尾附上提交人原始描述，整理有误时研发仍能看到原话。
func IssueBody(t *ticket.Ticket, tmpl *gitea.IssueTemplate, comments []ticket.Comment, forwardedBy, note, original string) string {
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

// markerRe 匹配灵问的隐藏标记（lingo:ticket、lingo:public 等）。
var markerRe = regexp.MustCompile(`<!--\s*lingo:[^>]*-->`)

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
