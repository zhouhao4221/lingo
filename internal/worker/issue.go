package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	GetIssue(ctx context.Context, owner, repo string, index int64) (*gitea.Issue, error)
	EditIssue(ctx context.Context, owner, repo string, index int64, opt gitea.EditIssueOption) (*gitea.Issue, error)
	CreateComment(ctx context.Context, owner, repo string, index int64, body string) (*gitea.Comment, error)
	EditComment(ctx context.Context, owner, repo string, id int64, body string) (*gitea.Comment, error)
	CreateIssueAttachment(ctx context.Context, owner, repo string, index int64, name string, r io.Reader) (*gitea.Attachment, error)
	CreateCommentAttachment(ctx context.Context, owner, repo string, commentID int64, name string, r io.Reader) (*gitea.Attachment, error)
}

// Forwarding 是转出 Issue 时读取的补充评论与附件，由 store.Store 实现。
type Forwarding interface {
	PendingComments(ctx context.Context, ticketID int64) ([]ticket.Comment, error)
	ForwardableAttachments(ctx context.Context, ticketID, commentID int64, ids *[]int64) ([]*ticket.Attachment, error)
	MarkAttachmentForwarded(ctx context.Context, id int64, uuid, url, reason string) error
	AttachmentLinks(ctx context.Context, ticketID, commentID int64) ([]store.AttachmentLink, error)
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
			ForwardedBy   string   `json:"forwarded_by"`
			Note          string   `json:"note"`
			AttachmentIDs *[]int64 `json:"attachment_ids"`
		}
		if err := json.Unmarshal(t.Payload, &p); err != nil {
			return permanent(fmt.Errorf("task payload: %w", err))
		}
		return w.createIssue(ctx, t.TicketID, p.ForwardedBy, p.Note, p.AttachmentIDs, t.Attempts > 1)
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
func (w *Worker) createIssue(ctx context.Context, id int64, forwardedBy, note string, attachments *[]int64, retried bool) error {
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
				// 正文里已写进的补充评论无法确定，按全部已转出处理（宁可漏发也不重复）
				comments, err := w.o.Forwarding.PendingComments(ctx, id)
				if err != nil {
					return err
				}
				if _, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "dedup", commentIDs(comments)); err != nil {
					return err
				}
				t.IssueNumber = is.Number
				return w.uploadIssueAttachments(ctx, t, attachments)
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
	comments, err := w.o.Forwarding.PendingComments(ctx, id)
	if err != nil {
		return err
	}
	opt := gitea.CreateIssueOption{Title: t.Title, Body: IssueBody(t, tmpl, comments, forwardedBy, note), Labels: labels, Assignees: cfg.Assignees}
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
	if _, err := w.o.Drafts.IssueCreated(ctx, id, is.Number, "create", commentIDs(comments)); err != nil {
		return err // 重试时会按标记查到这个 Issue，不会重复建
	}
	t.IssueNumber = is.Number
	return w.uploadIssueAttachments(ctx, t, attachments)
}

func commentIDs(cs []ticket.Comment) []int64 {
	ids := make([]int64, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

// uploadIssueAttachments 把随 Issue 转出的附件（attachments 为后台人员勾选的工单附件，nil 表示全部）上传为 Issue 附件，
// 并重写正文中的附件段落。段落按全部已处理的附件整体生成，重试或部分失败后已上传的引用也不会丢。
func (w *Worker) uploadIssueAttachments(ctx context.Context, t *ticket.Ticket, attachments *[]int64) error {
	owner, name, _ := strings.Cut(t.Repo, "/")
	uploaded, err := w.uploadAttachments(ctx, t.ID, 0, attachments, func(a *ticket.Attachment) (*gitea.Attachment, error) {
		return w.o.Issues.CreateIssueAttachment(ctx, owner, name, t.IssueNumber, a.Filename, bytes.NewReader(a.Data))
	})
	if err != nil {
		return err
	}
	section, err := w.attachmentSection(ctx, t.ID, 0)
	if err != nil || section == "" {
		return err
	}
	is, err := w.o.Issues.GetIssue(ctx, owner, name, t.IssueNumber)
	if err != nil {
		if !uploaded {
			return nil // 本次没有新上传，段落已在上次写好或无需改动
		}
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

// uploadAttachments 逐个上传待转出的附件，返回是否有新上传或新拒绝。Gitea 拒绝的附件（类型、大小限制）
// 记下原因不再重试；可重试的错误返回给调用方重试（已上传的不会重复上传）。
func (w *Worker) uploadAttachments(ctx context.Context, ticketID, commentID int64, ids *[]int64,
	upload func(a *ticket.Attachment) (*gitea.Attachment, error)) (bool, error) {
	as, err := w.o.Forwarding.ForwardableAttachments(ctx, ticketID, commentID, ids)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		ga, err := upload(a)
		switch {
		case err != nil && gitea.IsRetryable(err):
			return true, fmt.Errorf("upload attachment %d: %w", a.ID, err)
		case err != nil:
			w.log.Warn("attachment rejected by gitea", "attachment", a.ID, "file", a.Filename, "err", err)
			err = w.o.Forwarding.MarkAttachmentForwarded(ctx, a.ID, "", "", err.Error())
		default:
			err = w.o.Forwarding.MarkAttachmentForwarded(ctx, a.ID, ga.UUID, ga.BrowserDownloadURL, "")
		}
		if err != nil {
			return true, err
		}
	}
	return len(as) > 0, nil
}

// attachmentSection 按已处理的附件生成正文中的附件段落；没有附件时为空。
func (w *Worker) attachmentSection(ctx context.Context, ticketID, commentID int64) (string, error) {
	ls, err := w.o.Forwarding.AttachmentLinks(ctx, ticketID, commentID)
	if err != nil || len(ls) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString(sectionStart + "\n### 附件\n\n")
	for _, l := range ls {
		name := escapeMarkdown(l.Filename)
		switch {
		case l.URL == "":
			b.WriteString("- " + name + "（未能上传到 Gitea，请在灵问后台查看）\n")
		case strings.HasPrefix(l.ContentType, "image/"):
			b.WriteString("- ![" + name + "](" + l.URL + ")\n")
		default:
			b.WriteString("- [" + name + "](" + l.URL + ")\n")
		}
	}
	b.WriteString(sectionEnd)
	return b.String(), nil
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

// commentWait 是补充评论等待 Issue 建好时的推迟间隔：建单任务可能在退避重试，不必频繁空转。
const commentWait = time.Minute

// createComment 把补充评论由机器人发到 Issue，并上传评论附件；评论已发出时（上次附件未传完）只继续处理附件。
// 建单前的补充已写进 Issue 正文，不再发。
func (w *Worker) createComment(ctx context.Context, ticketID, commentID int64) error {
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
	t, err := w.o.Drafts.Draft(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber == 0 {
		if t.Status == ticket.StatusSubmitted {
			return &ticket.DeferError{Until: w.o.Now().Add(commentWait), Reason: "issue not created yet"}
		}
		return nil // 工单未转出（或建单失败）：补充留在灵问
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	who := "提交人"
	if c.Source == "staff" {
		who = "灵问后台"
		if c.AuthorName != "" {
			who = "灵问后台（" + escapeMarkdown(c.AuthorName) + "）"
		}
	}
	body := "**来自" + who + "的补充：**\n\n" + stripMarkers(c.Body)
	giteaID := c.GiteaID
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
	uploaded, err := w.uploadAttachments(ctx, t.ID, c.ID, nil, func(a *ticket.Attachment) (*gitea.Attachment, error) {
		return w.o.Issues.CreateCommentAttachment(ctx, owner, name, giteaID, a.Filename, bytes.NewReader(a.Data))
	})
	if err != nil {
		return err // 评论已发出并记录：重试时跳过发评论，继续上传剩余附件
	}
	if !uploaded && c.GiteaID != 0 {
		return nil
	}
	section, err := w.attachmentSection(ctx, t.ID, c.ID)
	if err != nil || section == "" {
		return err
	}
	if _, err := w.o.Issues.EditComment(ctx, owner, name, giteaID, body+"\n\n"+section); err != nil {
		return giteaErr("edit comment", err)
	}
	return nil
}

var typeColors = map[string]string{ticket.TypeConsult: "#1d76db", ticket.TypeBug: "#d73a4a", ticket.TypeFeature: "#0e8a16"}

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
func IssueBody(t *ticket.Ticket, tmpl *gitea.IssueTemplate, comments []ticket.Comment, forwardedBy, note string) string {
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
		who := "提交人"
		if c.Source == "staff" {
			who = "灵问后台"
		}
		b.WriteString("### 补充（" + who + "，" + c.At.UTC().Format("2006-01-02 15:04 UTC") + "）\n\n" + stripMarkers(c.Body) + "\n\n")
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
