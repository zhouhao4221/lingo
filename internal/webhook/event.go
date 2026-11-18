package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// Kind 是归一后的事件类型。
type Kind string

const (
	IssueOpened       Kind = "issue.opened"
	IssueEdited       Kind = "issue.edited"
	IssueClosed       Kind = "issue.closed"
	IssueReopened     Kind = "issue.reopened"
	IssueDeleted      Kind = "issue.deleted"
	IssueAssigned     Kind = "issue.assigned"
	IssueUnassigned   Kind = "issue.unassigned"
	IssueLabeled      Kind = "issue.labeled" // 标签增删或清空，见 Event.Changes 与 Issue.Labels
	IssueMilestoned   Kind = "issue.milestoned"
	IssueDemilestoned Kind = "issue.demilestoned"

	CommentCreated Kind = "comment.created"
	CommentEdited  Kind = "comment.edited"
	CommentDeleted Kind = "comment.deleted"

	PullOpened   Kind = "pull.opened"
	PullEdited   Kind = "pull.edited" // 正文可能新增 / 删除了 Fixes 引用
	PullReopened Kind = "pull.reopened"
	PullClosed   Kind = "pull.closed" // 未合并关闭
	PullMerged   Kind = "pull.merged"

	ReleasePublished Kind = "release.published"
	ReleaseUpdated   Kind = "release.updated"
	ReleaseDeleted   Kind = "release.deleted"
	TagCreated       Kind = "tag.created"

	StatusChanged        Kind = "status.changed"
	WorkflowRunCompleted Kind = "workflow_run.completed"

	// Ping 是 push 投递：TestHook 的测试投递与真实 push 无法区分，Handler 不把它写入投递表，
	// 只经 Store.RecordPing 记录仓库最近一次收到投递的时间，供启动自检确认连通。
	Ping    Kind = "ping"
	Ignored Kind = "ignored" // 灵问不处理的事件或动作
	// Invalid 是验签通过但载荷无法解析的投递：照常落库（便于排查与 Gitea 版本升级后重放），不丢弃。
	Invalid Kind = "invalid"
)

// Event 是归一后的 Webhook 事件。
type Event struct {
	Kind       Kind
	DeliveryID string
	EventType  string // X-Gitea-Event-Type
	Action     string // 原始 action
	DedupKey   string

	Repo        string // "owner/name"
	IssueNumber int64  // Issue 或 PR 编号
	IsPull      bool   // 评论是否发在 PR 上
	Sender      string // 触发人 login
	FromBot     bool   // 触发人是灵问机器人自己：只作确认，不再通知，防回环
	At          time.Time

	// MarkerTicketID 是 Issue 正文中 lingo:ticket 标记的工单 ID，只在 Issue 由机器人创建时采信，否则为 0。
	// 正文可被有权限的用户编辑，标记只是提示：工单与 Issue 的对应以数据库中保存的 repo + issue number 为准，
	// worker 必须核对后才能使用。
	MarkerTicketID int64
	Refs           []gitea.IssueRef // PR 标题与正文中的 Issue 引用（含 Fixes 等关闭关键词）
	Public         bool             // 评论按 ReplyMode 判定为面向提交人

	Issue       *gitea.Issue
	Comment     *gitea.Comment
	PullRequest *gitea.PullRequest
	Changes     *gitea.Changes
	Release     *gitea.Release
	Ref         string // tag.created 的 tag 名
	SHA         string // tag.created、status.changed 的提交
	Status      *gitea.CommitStatusPayload
	WorkflowRun *gitea.WorkflowRun
	Workflow    *gitea.Workflow
}

// ReplyMode 与 lingo.GiteaConfig.ReplyMode 取值一致。
type ReplyMode string

const (
	ReplyMarked ReplyMode = "marked"
	ReplyAll    ReplyMode = "all"
)

// ParseOptions 控制归一化时依赖配置的判定。
type ParseOptions struct {
	BotLogin  string    // 机器人账号 login，用于 FromBot；为空时不判定
	ReplyMode ReplyMode // 为空按 ReplyMarked
}

// Parse 把一次投递归一为 Event；不认识的事件返回 Kind=Ignored 而非错误，
// 只有载荷不是合法 JSON 或缺少必需对象时返回错误。
func Parse(d *gitea.Delivery, o ParseOptions) (*Event, error) {
	e := &Event{DeliveryID: d.ID, EventType: d.EventType, At: d.ReceivedAt}
	var err error
	switch d.EventType {
	case gitea.EventIssues, gitea.EventIssueAssign, gitea.EventIssueLabel, gitea.EventIssueMilestone:
		err = parseIssue(e, d.Body, o)
	case gitea.EventIssueComment, gitea.EventPullComment:
		err = parseComment(e, d.Body, o)
	case gitea.EventPullRequest:
		err = parsePull(e, d.Body)
	case gitea.EventRelease:
		err = parseRelease(e, d.Body)
	case gitea.EventCreate:
		err = parseCreate(e, d.Body)
	case gitea.EventStatus:
		err = parseStatus(e, d.Body)
	case gitea.EventWorkflowRun:
		err = parseWorkflowRun(e, d.Body)
	case gitea.EventPush:
		e.Kind = Ping
		var p struct {
			Repository *gitea.Repository `json:"repository"`
			Sender     *gitea.User       `json:"sender"`
		}
		if err = json.Unmarshal(d.Body, &p); err == nil {
			e.Repo, e.Sender = repoName(p.Repository), login(p.Sender)
		}
	default:
		e.Kind = Ignored
	}
	if err != nil {
		return nil, fmt.Errorf("webhook: parse %s delivery %s: %w", d.EventType, d.ID, err)
	}
	if o.BotLogin != "" && strings.EqualFold(e.Sender, o.BotLogin) {
		e.FromBot = true
	}
	if e.DedupKey == "" {
		e.DedupKey = "delivery:" + d.ID
	}
	return e, nil
}

var issueKinds = map[string]Kind{
	gitea.ActionOpened:       IssueOpened,
	gitea.ActionEdited:       IssueEdited,
	gitea.ActionClosed:       IssueClosed,
	gitea.ActionReopened:     IssueReopened,
	gitea.ActionDeleted:      IssueDeleted,
	gitea.ActionAssigned:     IssueAssigned,
	gitea.ActionUnassigned:   IssueUnassigned,
	gitea.ActionLabelUpdated: IssueLabeled,
	gitea.ActionLabelCleared: IssueLabeled,
	gitea.ActionMilestoned:   IssueMilestoned,
	gitea.ActionDemilestoned: IssueDemilestoned,
}

func parseIssue(e *Event, body []byte, o ParseOptions) error {
	var p gitea.IssuePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	if p.Issue == nil {
		return fmt.Errorf("missing issue")
	}
	e.Action, e.Issue, e.Changes = p.Action, p.Issue, p.Changes
	e.Repo, e.Sender = repoName(p.Repository), login(p.Sender)
	e.IssueNumber = p.Issue.Number
	e.MarkerTicketID = markerTicket(p.Issue, o.BotLogin)
	e.At = p.Issue.UpdatedAt
	k, ok := issueKinds[p.Action]
	if !ok {
		e.Kind = Ignored
		return nil
	}
	e.Kind = k
	// 业务键：对象 + 动作 + 更新时间，再加区分同一秒内不同变化的细节
	detail := ""
	switch k {
	case IssueLabeled:
		detail = labelIDs(p.Issue.Labels)
	case IssueAssigned, IssueUnassigned:
		detail = assigneeLogins(p.Issue.Assignees)
	case IssueMilestoned, IssueDemilestoned:
		if p.Issue.Milestone != nil {
			detail = strconv.FormatInt(p.Issue.Milestone.ID, 10)
		}
	case IssueEdited:
		detail = digest(p.Issue.Title, p.Issue.Body)
	}
	switch {
	case k == IssueOpened || k == IssueDeleted:
		// 一次性事件：对象 + 动作即唯一，对账时也能按同样的键重建
		e.DedupKey = key("issue", p.Issue.ID, string(k))
	case !p.Issue.UpdatedAt.IsZero():
		// 状态类事件只是触发器：worker 处理时以 Gitea 当前状态为准，同一秒内重复的相同变化被合并不影响结果
		e.DedupKey = key("issue", p.Issue.ID, string(k), stamp(p.Issue.UpdatedAt), detail)
	}
	return nil
}

// markerTicket 只采信机器人自己创建的 Issue 上的工单标记。
func markerTicket(is *gitea.Issue, bot string) int64 {
	if is == nil || bot == "" || is.User == nil || !strings.EqualFold(is.User.Login, bot) {
		return 0
	}
	return gitea.ParseTicketMarker(is.Body)
}

func parseComment(e *Event, body []byte, o ParseOptions) error {
	var p gitea.IssueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	if p.Comment == nil || p.Issue == nil {
		return fmt.Errorf("missing comment or issue")
	}
	e.Action, e.Issue, e.Comment, e.PullRequest, e.Changes = p.Action, p.Issue, p.Comment, p.PullRequest, p.Changes
	e.Repo, e.Sender = repoName(p.Repository), login(p.Sender)
	e.IssueNumber, e.IsPull = p.Issue.Number, p.IsPull || e.EventType == gitea.EventPullComment
	e.MarkerTicketID = markerTicket(p.Issue, o.BotLogin)
	e.At = p.Comment.UpdatedAt
	switch p.Action {
	case gitea.ActionCreated:
		e.Kind = CommentCreated
	case gitea.ActionEdited:
		e.Kind = CommentEdited
	case gitea.ActionDeleted:
		e.Kind = CommentDeleted
	default:
		e.Kind = Ignored
		return nil
	}
	e.Public = o.ReplyMode == ReplyAll || gitea.IsPublicReply(p.Comment.Body)
	switch {
	case e.Kind != CommentEdited:
		// 创建与删除各只发生一次
		e.DedupKey = key("comment", p.Comment.ID, string(e.Kind))
	case !p.Comment.UpdatedAt.IsZero():
		e.DedupKey = key("comment", p.Comment.ID, string(e.Kind), stamp(p.Comment.UpdatedAt), digest(p.Comment.Body))
	}
	return nil
}

func parsePull(e *Event, body []byte) error {
	var p gitea.PullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	pr := p.PullRequest
	if pr == nil {
		return fmt.Errorf("missing pull_request")
	}
	e.Action, e.PullRequest, e.Changes = p.Action, pr, p.Changes
	e.Repo, e.Sender = repoName(p.Repository), login(p.Sender)
	e.IssueNumber, e.IsPull = pr.Number, true
	e.At = pr.UpdatedAt
	e.Refs = gitea.ParseIssueRefs(pr.Title + "\n" + pr.Body)
	switch p.Action {
	case gitea.ActionOpened:
		e.Kind = PullOpened
	case gitea.ActionEdited:
		e.Kind = PullEdited
	case gitea.ActionReopened:
		e.Kind = PullReopened
	case gitea.ActionClosed:
		e.Kind = PullClosed
		if pr.Merged {
			e.Kind = PullMerged
		}
	default:
		e.Kind = Ignored
		return nil
	}
	switch {
	case e.Kind == PullOpened || e.Kind == PullMerged:
		e.DedupKey = key("pull", pr.ID, string(e.Kind))
	case !pr.UpdatedAt.IsZero():
		detail := ""
		if e.Kind == PullEdited {
			detail = digest(pr.Title, pr.Body)
		}
		e.DedupKey = key("pull", pr.ID, string(e.Kind), stamp(pr.UpdatedAt), detail)
	}
	return nil
}

func parseRelease(e *Event, body []byte) error {
	var p gitea.ReleasePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	r := p.Release
	if r == nil {
		return fmt.Errorf("missing release")
	}
	e.Action, e.Release = p.Action, r
	e.Repo, e.Sender = repoName(p.Repository), login(p.Sender)
	if r.PublishedAt != nil {
		e.At = *r.PublishedAt
	}
	switch p.Action {
	case gitea.ActionPublished:
		e.Kind = ReleasePublished
	case gitea.ActionUpdated:
		e.Kind = ReleaseUpdated
	case gitea.ActionDeleted:
		e.Kind = ReleaseDeleted
	default:
		e.Kind = Ignored
		return nil
	}
	// 发布与删除只发生一次；Release 没有 updated_at，多次更新无法可靠区分（改回旧内容与重投一模一样），
	// 因此 updated 只按 delivery 去重，由 worker 按 release.id 幂等更新版本记录。
	if e.Kind != ReleaseUpdated {
		e.DedupKey = key("release", r.ID, string(e.Kind))
	}
	return nil
}

func parseCreate(e *Event, body []byte) error {
	var p gitea.CreatePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	e.Repo, e.Sender, e.Ref, e.SHA = repoName(p.Repository), login(p.Sender), p.Ref, p.SHA
	if p.RefType != "tag" {
		e.Kind = Ignored
		return nil
	}
	e.Kind = TagCreated
	e.DedupKey = "tag:" + e.Repo + ":" + p.Ref + ":" + p.SHA
	return nil
}

func parseStatus(e *Event, body []byte) error {
	var p gitea.CommitStatusPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	e.Kind, e.Status, e.SHA = StatusChanged, &p, p.SHA
	e.Repo, e.Sender, e.At = repoName(p.Repository), login(p.Sender), p.CreatedAt
	e.DedupKey = key("status", p.ID, p.State)
	return nil
}

func parseWorkflowRun(e *Event, body []byte) error {
	var p gitea.WorkflowRunPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	if p.WorkflowRun == nil {
		return fmt.Errorf("missing workflow_run")
	}
	e.Action, e.WorkflowRun, e.Workflow = p.Action, p.WorkflowRun, p.Workflow
	e.Repo, e.Sender, e.SHA = repoName(p.Repository), login(p.Sender), p.WorkflowRun.HeadSHA
	if p.WorkflowRun.CompletedAt != nil {
		e.At = *p.WorkflowRun.CompletedAt
	}
	if p.Action != gitea.ActionCompleted {
		e.Kind = Ignored // 只关心完成（成功与否看 WorkflowRun.Conclusion）
		return nil
	}
	e.Kind = WorkflowRunCompleted
	e.DedupKey = key("run", p.WorkflowRun.ID, strconv.FormatInt(p.WorkflowRun.RunAttempt, 10), p.WorkflowRun.Conclusion)
	return nil
}

func repoName(r *gitea.Repository) string {
	if r == nil {
		return ""
	}
	if r.FullName != "" {
		return r.FullName
	}
	if r.Owner != nil {
		return r.Owner.Login + "/" + r.Name
	}
	return r.Name
}

func login(u *gitea.User) string {
	if u == nil {
		return ""
	}
	return u.Login
}

func key(kind string, id int64, parts ...string) string {
	var b strings.Builder
	b.WriteString(kind + ":" + strconv.FormatInt(id, 10))
	for _, p := range parts {
		if p != "" {
			b.WriteString(":" + p)
		}
	}
	return b.String()
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func labelIDs(ls []*gitea.Label) string {
	ids := make([]string, 0, len(ls))
	for _, l := range ls {
		ids = append(ids, strconv.FormatInt(l.ID, 10))
	}
	slices.Sort(ids)
	return "l" + strings.Join(ids, ",")
}

func assigneeLogins(us []*gitea.User) string {
	ls := make([]string, 0, len(us))
	for _, u := range us {
		ls = append(ls, u.Login)
	}
	slices.Sort(ls)
	return "a" + strings.Join(ls, ",")
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
