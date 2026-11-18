package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/store"
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
}

// Drafts 读取待建 Issue 的工单并记录建单结果，由 ticket.Service 实现。
type Drafts interface {
	Draft(ctx context.Context, id int64) (*ticket.Ticket, error)
	IssueCreated(ctx context.Context, id, number int64, via string) (bool, error)
	IssueFailed(ctx context.Context, id int64, reason string) error
}

// RepoConfig 是仓库在 lingo.Repo 中的建单配置。
type RepoConfig struct {
	Assignees []string
	Templates map[string]string // 工单类型 → 模板文件名
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
	err := w.runTask(tctx, t)
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
			ForwardedBy string `json:"forwarded_by"`
			Note        string `json:"note"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil {
			return permanent(fmt.Errorf("task payload: %w", err))
		}
		return w.createIssue(ctx, t.TicketID, p.ForwardedBy, p.Note, t.Attempts > 1)
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

// createIssue 建 Issue；retried 为真表示任务此前被领取过（可能已建好 Issue 但未保存），需先查重。
func (w *Worker) createIssue(ctx context.Context, id int64, forwardedBy, note string, retried bool) error {
	t, err := w.o.Drafts.Draft(ctx, id)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Status != ticket.StatusSubmitted || t.IssueNumber != 0 {
		return nil // 已建单（或被 Webhook 按标记补记），或工单已不再待建
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
				_, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "dedup")
				return err
			}
		}
	}

	wantLabels := []labelSpec{
		{Name: w.o.LabelPrefix, Color: "#6f42c1", Description: "由灵问创建"},
		{Name: w.o.LabelPrefix + "/type/" + t.Type, Color: typeColors[t.Type], Exclusive: true},
	}
	labels, err := w.labels.ensure(ctx, w.o.Issues, owner, name, wantLabels)
	if err != nil {
		return err
	}
	cfg := w.o.Repos(t.Repo)
	tmpl, err := w.template(ctx, owner, name, t.Type, cfg)
	if err != nil {
		return err
	}
	opt := gitea.CreateIssueOption{Title: t.Title, Body: IssueBody(t, tmpl, forwardedBy, note), Labels: labels, Assignees: cfg.Assignees}
	if tmpl != nil && tmpl.Title != "" && !strings.HasPrefix(t.Title, tmpl.Title) {
		opt.Title = tmpl.Title + t.Title
	}
	opt.Title = clipRunes(opt.Title, maxIssueTitle)
	is, err := w.o.Issues.CreateIssue(ctx, owner, name, opt)
	var ae *gitea.APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusUnprocessableEntity {
		// 422 常见于缓存的标签 ID 已失效（标签被删除重建）或指派人不存在：刷新标签、不指派再建一次，
		// 不让这类配置变化挡住建单
		w.log.Warn("create issue rejected, retrying with fresh labels and no assignees", "repo", t.Repo, "err", err)
		w.labels.forget(owner, name)
		w.templates.forget(owner, name)
		if opt.Labels, err = w.labels.ensure(ctx, w.o.Issues, owner, name, wantLabels); err != nil {
			return err
		}
		opt.Assignees = nil
		is, err = w.o.Issues.CreateIssue(ctx, owner, name, opt)
	}
	if err != nil {
		return giteaErr("create issue", err)
	}
	if _, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "create"); err != nil {
		return err // 重试时会按标记查到这个 Issue，不会重复建
	}
	return nil
}

var typeColors = map[string]string{ticket.TypeConsult: "#1d76db", ticket.TypeBug: "#d73a4a", ticket.TypeFeature: "#0e8a16"}

// template 按 Repo.Templates 的文件名、再按模板标签匹配工单类型的 Issue 模板；没有时返回 nil（用内置结构）。
func (w *Worker) template(ctx context.Context, owner, name, typ string, cfg RepoConfig) (*gitea.IssueTemplate, error) {
	ts, err := w.templates.get(ctx, w.o.Issues, owner, name, w.o.Now())
	if err != nil {
		if gitea.IsRetryable(err) {
			return nil, fmt.Errorf("list issue templates: %w", err)
		}
		w.log.Warn("list issue templates failed, using built-in body", "repo", owner+"/"+name, "err", err)
		return nil, nil
	}
	if file := cfg.Templates[typ]; file != "" {
		for _, t := range ts {
			if t.FileName == file || strings.HasSuffix(t.FileName, "/"+file) {
				return t, nil
			}
		}
		w.log.Warn("configured issue template not found", "repo", owner+"/"+name, "template", file)
	}
	want := []string{"type/" + typ, w.o.LabelPrefix + "/type/" + typ, typ}
	for _, t := range ts {
		for _, l := range t.Labels {
			if slices.ContainsFunc(want, func(s string) bool { return strings.EqualFold(s, l) }) {
				return t, nil
			}
		}
	}
	return nil, nil
}

// IssueBody 生成 Issue 正文：有表单模板且提交了字段时按 Gitea 表单格式渲染，否则用内置结构；
// 末尾附项目、版本、提交环境、确认人与隐藏标记。不写入提交人姓名、邮箱等身份信息（PRD 3.2.0.6）。
func IssueBody(t *ticket.Ticket, tmpl *gitea.IssueTemplate, forwardedBy, note string) string {
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

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// templateCache 按仓库缓存 Issue 模板 5 分钟，避免每建一个 Issue 都重新解析全部模板。
type templateCache struct {
	mu     sync.Mutex
	byRepo map[string]cachedTemplates
}

type cachedTemplates struct {
	at time.Time
	ts []*gitea.IssueTemplate
}

const templateTTL = 5 * time.Minute

func (c *templateCache) get(ctx context.Context, g IssueWriter, owner, name string, now time.Time) ([]*gitea.IssueTemplate, error) {
	key := strings.ToLower(owner + "/" + name)
	c.mu.Lock()
	if e, ok := c.byRepo[key]; ok && now.Sub(e.at) < templateTTL {
		c.mu.Unlock()
		return e.ts, nil
	}
	c.mu.Unlock()
	ts, err := g.ListIssueTemplates(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRepo == nil {
		c.byRepo = map[string]cachedTemplates{}
	}
	c.byRepo[key] = cachedTemplates{at: now, ts: ts}
	return ts, nil
}

func (c *templateCache) forget(owner, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byRepo, strings.ToLower(owner+"/"+name))
}

type labelSpec struct {
	Name, Color, Description string
	Exclusive                bool
}

// labelCache 缓存各仓库标签 name → id，缺的标签创建后加入缓存。
type labelCache struct {
	mu     sync.Mutex
	byRepo map[string]map[string]int64
}

func (c *labelCache) forget(owner, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byRepo, strings.ToLower(owner+"/"+name))
}

func (c *labelCache) ensure(ctx context.Context, g IssueWriter, owner, name string, want []labelSpec) ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := strings.ToLower(owner + "/" + name)
	if c.byRepo == nil {
		c.byRepo = map[string]map[string]int64{}
	}
	ids := c.byRepo[key]
	missing := func() bool {
		for _, l := range want {
			if _, ok := ids[strings.ToLower(l.Name)]; !ok {
				return true
			}
		}
		return false
	}
	if ids == nil || missing() {
		ls, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error) {
			return g.ListLabels(ctx, owner, name, o)
		})
		if err != nil {
			return nil, giteaErr("list labels", err)
		}
		ids = map[string]int64{}
		for _, l := range ls {
			ids[strings.ToLower(l.Name)] = l.ID
		}
		c.byRepo[key] = ids
	}
	out := make([]int64, 0, len(want))
	for _, l := range want {
		id, ok := ids[strings.ToLower(l.Name)]
		if !ok {
			created, err := g.CreateLabel(ctx, owner, name, gitea.CreateLabelOption{Name: l.Name, Color: l.Color,
				Description: l.Description, Exclusive: l.Exclusive})
			if err != nil {
				return nil, giteaErr("create label "+l.Name, err)
			}
			id = created.ID
			ids[strings.ToLower(l.Name)] = id
		}
		out = append(out, id)
	}
	return out, nil
}
