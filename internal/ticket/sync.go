package ticket

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
)

// IssueChange 是 Gitea Issue 上发生的变化。
type IssueChange string

const (
	IssueOpened       IssueChange = "issue.opened"
	IssueEdited       IssueChange = "issue.edited"
	IssueClosed       IssueChange = "issue.closed"
	IssueReopened     IssueChange = "issue.reopened"
	IssueDeleted      IssueChange = "issue.deleted"
	IssueAssigned     IssueChange = "issue.assigned"
	IssueUnassigned   IssueChange = "issue.unassigned"
	IssueLabeled      IssueChange = "issue.labeled"
	IssueMilestoned   IssueChange = "issue.milestoned"
	IssueDemilestoned IssueChange = "issue.demilestoned"
)

// IssueState 是 Issue 的状态快照。Webhook 只是触发器：调用方应尽量传入从 Gitea 重新读取的当前状态，
// 读取失败时退回载荷中的状态。
type IssueState struct {
	State     string // open | closed
	Labels    []string
	Assignees []string
	Milestone string
	DueDate   *time.Time
	UpdatedAt time.Time
}

// IssueUpdate 是一次 Issue 变化。
type IssueUpdate struct {
	Change IssueChange
	Repo   string // "owner/name"
	Number int64
	// MarkerTicketID 是机器人创建的 Issue 正文中的工单标记；仅用于 opened 时补记关联，且须与工单所选仓库一致。
	MarkerTicketID int64
	Sender         string // 触发人 login
	DeliveryID     string
	ReceivedAt     time.Time
	Current        IssueState
}

// CommentChange 是 Gitea 评论上发生的变化。
type CommentChange string

const (
	CommentCreated CommentChange = "comment.created"
	CommentEdited  CommentChange = "comment.edited"
	CommentDeleted CommentChange = "comment.deleted"
)

// CommentUpdate 是一次评论变化。
type CommentUpdate struct {
	Change     CommentChange
	Repo       string
	Number     int64
	CommentID  int64
	Author     string // 评论人 login
	Body       string // 对外评论为去掉 /reply 前缀后的正文
	Public     bool   // 按 ReplyMode 判定为面向提交人
	FromBot    bool   // 机器人自己发的评论（灵问转发的用户补充），只作确认
	DeliveryID string
	At         time.Time
}

// PullChange 是 Gitea PR 上发生的变化。
type PullChange string

const (
	PullOpened   PullChange = "pull.opened"
	PullEdited   PullChange = "pull.edited"
	PullReopened PullChange = "pull.reopened"
	PullClosed   PullChange = "pull.closed"
	PullMerged   PullChange = "pull.merged"
)

// IssueRef 是 PR 对 Issue 的引用，Repo 已补全为 "owner/name"。
type IssueRef struct {
	Repo   string
	Number int64
	Closes bool // 带关闭关键词（Fixes #N 等）
}

// PullUpdate 是一次 PR 变化。
type PullUpdate struct {
	Change      PullChange
	Repo        string
	Number      int64
	Refs        []IssueRef
	MergeCommit string
	Sender      string
	DeliveryID  string
}

// SyncOptions 是 Sync 的配置。
type SyncOptions struct {
	Store Store
	// NeedInfoLabel 是“需补充信息”标签的完整名称，如 "lingo/review/need-info"。
	NeedInfoLabel string
	// IssueURL 返回 Issue 的网页地址，用于对外事件；为空时事件不带链接。
	IssueURL func(repo string, number int64) string
	// CloseGrace 是 Issue 关闭但尚无修复 PR 时的等待时间：PR 合并自动关闭 Issue 时，issues closed 事件
	// 常先于 pull_request merged 到达，等待期内推迟处理，避免先把工单误判为“关闭未修复”并通知提交人。
	// 0 表示默认 30 秒，负数不等待。
	CloseGrace time.Duration
	Now        func() time.Time // 测试用，默认 time.Now
}

// Sync 把 Gitea 侧的变化同步到工单：推进状态、记录审计、写对外事件。各方法幂等，可安全重放。
type Sync struct {
	o SyncOptions
}

// NewSync 创建 Sync。
func NewSync(o SyncOptions) (*Sync, error) {
	if o.Store == nil {
		return nil, errors.New("ticket: Store is required")
	}
	if o.CloseGrace == 0 {
		o.CloseGrace = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Sync{o: o}, nil
}

// Tracks 报告 Issue 是否属于某个工单；调用方可据此跳过无关 Issue，免去向 Gitea 读取当前状态。
func (s *Sync) Tracks(ctx context.Context, repo string, number int64) (bool, error) {
	return s.o.Store.HasIssue(ctx, repo, number)
}

// ApplyIssue 处理 Issue 变化；Issue 不属于任何工单时什么也不做。
func (s *Sync) ApplyIssue(ctx context.Context, u IssueUpdate) error {
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByIssue(ctx, u.Repo, u.Number)
		if errors.Is(err, ErrNotFound) {
			if u.Change == IssueOpened && u.MarkerTicketID > 0 {
				return s.linkByMarker(ctx, tx, u)
			}
			return nil
		}
		if err != nil {
			return err
		}
		return s.applyIssue(ctx, tx, t, u)
	})
}

// linkByMarker 在建单结果未能保存（如进程在 Gitea 返回后崩溃）时，按 Issue 中的工单标记补记关联。
func (s *Sync) linkByMarker(ctx context.Context, tx Tx, u IssueUpdate) error {
	t, err := tx.LockByID(ctx, u.MarkerTicketID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber != 0 || t.Status != StatusSubmitted || !strings.EqualFold(t.Repo, u.Repo) {
		return nil // 标记与库中记录不符：不采信
	}
	t.IssueNumber = u.Number
	return s.applyIssue(ctx, tx, t, u)
}

func (s *Sync) applyIssue(ctx context.Context, tx Tx, t *Ticket, u IssueUpdate) error {
	cur := u.Current
	stale := !cur.UpdatedAt.IsZero() && !t.GiteaUpdatedAt.IsZero() && cur.UpdatedAt.Before(t.GiteaUpdatedAt)
	prev, prevMilestone := t.Status, t.Milestone
	if !stale {
		if u.Change == IssueClosed && cur.State != "open" && !t.Status.fixed() && t.FixedPR == 0 && t.Status.active() {
			if wait := u.ReceivedAt.Add(s.o.CloseGrace); s.o.CloseGrace > 0 && s.o.Now().Before(wait) {
				return &DeferError{Until: wait, Reason: "issue closed without merged fix, waiting for pull request event"}
			}
		}
		s.transitIssue(t, u)
		if cur.UpdatedAt.After(t.GiteaUpdatedAt) {
			t.GiteaUpdatedAt = cur.UpdatedAt
		}
	}
	if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: string(u.Change), Payload: map[string]any{
		"delivery": u.DeliveryID, "sender": u.Sender, "issue": u.Number, "stale": stale,
		"status": string(t.Status), "prev_status": string(prev),
	}}); err != nil {
		return err
	}
	if stale {
		return nil // 旧于已采纳状态的乱序事件只留痕
	}
	if err := tx.Update(ctx, t); err != nil {
		return err
	}
	if t.Status != prev {
		if err := tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, "")); err != nil {
			return err
		}
	}
	if t.Milestone != prevMilestone && t.Milestone != "" {
		body := "计划版本 " + t.Milestone
		if t.DueDate != nil {
			body += "，预计 " + t.DueDate.Format("2006-01-02")
		}
		if err := tx.Emit(ctx, s.event(t, notify.TicketSchedule, "", body)); err != nil {
			return err
		}
	}
	return nil
}

// transitIssue 按变化与 Issue 当前状态推进工单（PRD 3.2.2 工单状态表）。
func (s *Sync) transitIssue(t *Ticket, u IssueUpdate) {
	cur := u.Current
	switch u.Change {
	case IssueOpened:
		if t.Status == StatusSubmitted {
			t.Status = StatusOpen
		}
	case IssueClosed:
		if cur.State == "open" { // 事件之后又被重开
			return
		}
		switch {
		case t.Status.fixed():
		case t.FixedPR != 0 && (t.Status.active() || t.Status == StatusClosed):
			t.Status = StatusFixed
		case t.Status.active() || t.Status == StatusSubmitted:
			t.Status = StatusClosed
		}
	case IssueReopened:
		if cur.State == "closed" {
			return
		}
		if t.Status == StatusClosed || t.Status.fixed() {
			// 修复被否定：清掉修复记录，之后的合并重新记录
			t.Status, t.FixedPR, t.FixedCommit = StatusInProgress, 0, ""
		}
	case IssueDeleted:
		t.Status = StatusAbnormal
	case IssueAssigned:
		if t.Status == StatusOpen && len(cur.Assignees) > 0 {
			t.Status = StatusInProgress
		}
	case IssueLabeled:
		needInfo := s.o.NeedInfoLabel != "" && slices.ContainsFunc(cur.Labels, func(l string) bool {
			return strings.EqualFold(l, s.o.NeedInfoLabel)
		})
		switch {
		case needInfo && (t.Status == StatusOpen || t.Status == StatusInProgress):
			t.Status = StatusNeedInfo
		case !needInfo && t.Status == StatusNeedInfo:
			t.Status = StatusInProgress
		}
	case IssueMilestoned, IssueDemilestoned:
		t.Milestone, t.DueDate = cur.Milestone, cur.DueDate
	}
	if u.Change != IssueMilestoned && u.Change != IssueDemilestoned && u.Change != IssueDeleted && !cur.UpdatedAt.IsZero() {
		// 其他事件携带的快照同样反映当前排期，顺带校正
		t.Milestone, t.DueDate = cur.Milestone, cur.DueDate
	}
}

// ApplyComment 处理评论变化：保存到工单评论；对外评论首次出现时通知提交人。
func (s *Sync) ApplyComment(ctx context.Context, u CommentUpdate) error {
	if u.FromBot {
		return nil // 灵问自己转发的评论，已在发出时记录
	}
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByIssue(ctx, u.Repo, u.Number)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: string(u.Change), Payload: map[string]any{
			"delivery": u.DeliveryID, "sender": u.Author, "comment": u.CommentID, "public": u.Public,
		}}); err != nil {
			return err
		}
		if u.Change == CommentDeleted {
			return tx.DeleteGiteaComment(ctx, u.CommentID, u.At)
		}
		prev, err := tx.SaveGiteaComment(ctx, Comment{TicketID: t.ID, GiteaID: u.CommentID, AuthorName: u.Author,
			Body: u.Body, Public: u.Public, At: u.At})
		if err != nil {
			return err
		}
		if u.Public && (prev == nil || !prev.Public) {
			return tx.Emit(ctx, s.event(t, notify.TicketComment, "", u.Body))
		}
		return nil
	})
}

// ApplyPull 处理 PR 变化：引用工单 Issue 的 PR 打开时工单进入开发中，带关闭关键词的 PR 合并时记为已修复。
func (s *Sync) ApplyPull(ctx context.Context, u PullUpdate) error {
	refs := slices.Clone(u.Refs)
	// 固定加锁顺序，避免并发处理多个 PR 时死锁
	slices.SortFunc(refs, func(a, b IssueRef) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo)), cmp.Compare(a.Number, b.Number))
	})
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		for _, r := range refs {
			t, err := tx.LockByIssue(ctx, r.Repo, r.Number)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := s.applyPull(ctx, tx, t, u, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Sync) applyPull(ctx context.Context, tx Tx, t *Ticket, u PullUpdate, r IssueRef) error {
	prev, prevPR, prevCommit := t.Status, t.FixedPR, t.FixedCommit
	switch u.Change {
	case PullOpened, PullReopened:
		if t.Status == StatusOpen {
			t.Status = StatusInProgress
		}
	case PullMerged:
		if r.Closes && (t.Status.active() || t.Status == StatusClosed || t.Status == StatusFixed) {
			t.Status, t.FixedPR, t.FixedCommit = StatusFixed, u.Number, u.MergeCommit
		}
	}
	if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: string(u.Change), Payload: map[string]any{
		"delivery": u.DeliveryID, "sender": u.Sender, "pull": u.Repo + "#" + strconv.FormatInt(u.Number, 10),
		"closes": r.Closes, "merge_commit": u.MergeCommit, "status": string(t.Status), "prev_status": string(prev),
	}}); err != nil {
		return err
	}
	if t.Status == prev && t.FixedPR == prevPR && t.FixedCommit == prevCommit {
		return nil
	}
	if err := tx.Update(ctx, t); err != nil {
		return err
	}
	if t.Status != prev {
		return tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, ""))
	}
	return nil
}

func (s *Sync) event(t *Ticket, kind notify.Kind, prev Status, body string) notify.Event {
	e := notify.Event{Kind: kind, ProjectID: t.ProjectID, ProjectKey: t.ProjectKey, TicketID: t.ID, TicketType: t.Type,
		Title: t.Title, Status: string(t.Status), PrevStatus: string(prev), UserID: t.UserID, Body: body}
	if s.o.IssueURL != nil && t.Repo != "" && t.IssueNumber != 0 {
		e.URL = s.o.IssueURL(t.Repo, t.IssueNumber)
	}
	return e
}

// IssueURL 返回 Gitea Issue 的网页地址。
func IssueURL(baseURL, repo string, number int64) string {
	return fmt.Sprintf("%s/%s/issues/%d", strings.TrimRight(baseURL, "/"), repo, number)
}
