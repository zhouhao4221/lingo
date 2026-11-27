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
	// IssueSynced 是对账时按 Issue 当前快照对齐（不知道具体发生了什么变化）：只在快照新于已采纳的状态时生效。
	IssueSynced IssueChange = "issue.synced"
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
	Change    CommentChange
	Repo      string
	Number    int64
	CommentID int64
	Author    string // 评论人 login
	Body      string // 对外评论为去掉 /reply 前缀后的正文
	Public    bool   // 按 ReplyMode 判定为面向提交人
	// Model 是按原始正文标记（/reply 开头或带 lingo:public 标记）判定属于 ReplyMarked 内容范围，与 ReplyMode 无关；
	// 模型准入用（REQ-012），随正文一起保存，乱序旧版本与已删除的评论不覆盖。
	Model      bool
	FromBot    bool // 机器人自己发的评论（灵工转发的用户补充），只作确认
	DeliveryID string
	At         time.Time
	// TicketID 非零时只写入该工单：Issue 关联的工单不是它时跳过（历史评论重新同步，防止写到其他工单）。
	TicketID int64
	// Silent 为真时不发 ticket.comment 事件（历史回补不通知提交人）；保存、对外判定与审计照常。
	Silent bool
}

// CommentOutcome 是一条评论变化的处理结果（ResyncComment 返回）。
type CommentOutcome string

const (
	CommentSkipped   CommentOutcome = "skipped"   // 机器人评论，或 Issue 不关联（指定的）工单
	CommentAdded     CommentOutcome = "added"     // 新保存
	CommentUpdated   CommentOutcome = "updated"   // 正文或对外判定有变化
	CommentUnchanged CommentOutcome = "unchanged" // 与已保存的一致（含灵工本地已删除的评论）
)

// CommentResult 是 ResyncComment 的结果：Published / Unpublished 报告对提交人的可见性是否因此变化。
type CommentResult struct {
	Outcome     CommentOutcome
	Published   bool // 由不可见变为对提交人可见（含新增即对外）
	Unpublished bool // 由对外变为内部
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
	// Replay 为真表示来自对账：同一 PR 的同类变化此前已应用过时跳过（避免重放已被重开撤销的合并）。
	Replay bool
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
	// PendingMerges 报告与仓库 repo 同项目的仓库中，是否还有在 before 之前收到、尚未处理（且未失败过）的 PR 合并事件；
	// 可空。worker 积压时等待期已过，但合并事件可能仍在队列中，此时继续推迟关闭的判定。在事务外调用。
	PendingMerges func(ctx context.Context, repo string, before time.Time) (bool, error)
	Now           func() time.Time // 测试用，默认 time.Now
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
	// 队列检查放在事务外：事务持有连接与行锁时再从连接池取连接，连接池耗尽时会互相等待
	mergePending := false
	if (u.Change == IssueClosed || (u.Change == IssueSynced && u.Current.State == "closed")) && s.o.PendingMerges != nil && s.o.CloseGrace >= 0 {
		var err error
		if mergePending, err = s.o.PendingMerges(ctx, u.Repo, u.ReceivedAt.Add(s.o.CloseGrace)); err != nil {
			return err
		}
	}
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByIssue(ctx, u.Repo, u.Number)
		if errors.Is(err, ErrNotFound) {
			if u.Change == IssueOpened && u.MarkerTicketID > 0 {
				return s.linkByMarker(ctx, tx, u, mergePending)
			}
			return nil
		}
		if err != nil {
			return err
		}
		return s.applyIssue(ctx, tx, t, u, mergePending)
	})
}

// linkByMarker 在建单结果未能保存（如进程在 Gitea 返回后崩溃）时，按 Issue 中的工单标记补记关联。
func (s *Sync) linkByMarker(ctx context.Context, tx Tx, u IssueUpdate, mergePending bool) error {
	t, err := tx.LockByID(ctx, u.MarkerTicketID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.IssueNumber == 0 && t.Status == StatusRejected && strings.EqualFold(t.Repo, u.Repo) {
		// 建单途中被驳回、建单结果未能保存：补记关联并入队关闭这个 Issue，工单保持驳回
		t.IssueNumber = u.Number
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: "issue.linked", Payload: map[string]any{
			"delivery": u.DeliveryID, "issue": u.Number, "repo": t.Repo, "rejected": true}}); err != nil {
			return err
		}
		return enqueueClose(ctx, tx, t, 1)
	}
	if t.IssueNumber != 0 || t.Status != StatusSubmitted || !strings.EqualFold(t.Repo, u.Repo) {
		return nil // 标记与库中记录不符：不采信
	}
	t.IssueNumber = u.Number
	return s.applyIssue(ctx, tx, t, u, mergePending)
}

func (s *Sync) applyIssue(ctx context.Context, tx Tx, t *Ticket, u IssueUpdate, mergePending bool) error {
	cur := u.Current
	// 删除是终态，不受乱序判断影响（Issue 已无法读取，快照只能来自旧载荷）
	stale := u.Change != IssueDeleted && !cur.UpdatedAt.IsZero() && !t.GiteaUpdatedAt.IsZero() && cur.UpdatedAt.Before(t.GiteaUpdatedAt)
	if u.Change == IssueSynced && !t.GiteaUpdatedAt.IsZero() && !cur.UpdatedAt.After(t.GiteaUpdatedAt) {
		return nil // 对账：这个状态 Webhook 已处理过，不重复留痕
	}
	prev, prevMilestone := t.Status, t.Milestone
	if !stale {
		closing := u.Change == IssueClosed || (u.Change == IssueSynced && cur.State == "closed")
		if closing && cur.State != "open" && t.FixedPR == 0 && t.Status.active() {
			if err := s.awaitFix(u, mergePending); err != nil {
				return err
			}
		}
		// 提交人已补充、机器人尚未移除需补充标签时，快照里残留的标签不算数
		removing, err := tx.PendingTask(ctx, t.ID, OpRemoveLabel)
		if err != nil {
			return err
		}
		s.transitIssue(t, u, removing)
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

// awaitFix 在 Issue 关闭而工单尚无修复 PR 时决定是否推迟判定，等待可能随后到达或仍在队列中的 PR 合并事件。
func (s *Sync) awaitFix(u IssueUpdate, mergePending bool) error {
	if s.o.CloseGrace < 0 {
		return nil
	}
	deadline := u.ReceivedAt.Add(s.o.CloseGrace)
	if now := s.o.Now(); now.Before(deadline) {
		return &DeferError{Until: deadline, Reason: "issue closed without merged fix, waiting for pull request event"}
	}
	if mergePending {
		return &DeferError{Until: s.o.Now().Add(5 * time.Second), Reason: "pull request merge events still queued"}
	}
	return nil
}

// transitIssue 按变化与 Issue 当前状态推进工单（PRD 3.2.2 工单状态表）。
func (s *Sync) transitIssue(t *Ticket, u IssueUpdate, ignoreNeedInfo bool) {
	cur := u.Current
	if t.Status == StatusRejected {
		// 本地驳回是终态：灵工关闭 Issue 回流的 closed、研发重开、删除或对账快照都不改变工单状态与排期，只留痕
		return
	}
	switch u.Change {
	case IssueOpened:
		if t.Status == StatusSubmitted {
			t.Status = StatusOpen
		}
	case IssueClosed:
		if cur.State != "open" { // 事件之后又被重开时不处理
			closeTicket(t)
		}
	case IssueReopened:
		if cur.State != "closed" && (t.Status == StatusClosed || t.Status.fixed()) {
			reopenTicket(t)
		}
	case IssueDeleted:
		if t.Status == StatusClosed {
			// 已关闭（未修复）是终态：Issue 删除或读取 404（含权限被收回）只留痕，保持 closed，不转 abnormal，
			// 也就不会因此变成可驳回（RejectForwarded 不开放已关闭工单）
			return
		}
		t.Status = StatusAbnormal
		return
	case IssueAssigned:
		assignTicket(t, cur)
	case IssueLabeled:
		s.labelTicket(t, cur, ignoreNeedInfo)
	case IssueSynced:
		// 对账：按快照补上漏掉的关闭 / 重开 / 领取 / 标签变化。Issue 仍打开而工单已修复（PR 合并但未自动关闭）
		// 是正常情况，只有“关闭未修复”的工单才按打开的 Issue 重开
		if cur.State == "closed" {
			closeTicket(t)
			break
		}
		if t.Status == StatusClosed {
			reopenTicket(t)
		}
		assignTicket(t, cur)
		s.labelTicket(t, cur, ignoreNeedInfo)
	case IssueMilestoned, IssueDemilestoned:
		t.Milestone, t.DueDate = cur.Milestone, cur.DueDate
	}
	if t.Status == StatusAbnormal && cur.State != "" {
		// 此前读取 Issue 返回 404（如令牌权限被临时收回），现在又能读到：按当前状态恢复
		switch {
		case cur.State == "open":
			t.Status = StatusInProgress
		case t.FixedPR != 0:
			t.Status = StatusFixed
		default:
			t.Status = StatusClosed
		}
	}
	if u.Change != IssueMilestoned && u.Change != IssueDemilestoned && !cur.UpdatedAt.IsZero() {
		// 其他事件携带的快照同样反映当前排期，顺带校正
		t.Milestone, t.DueDate = cur.Milestone, cur.DueDate
	}
}

func closeTicket(t *Ticket) {
	switch {
	case t.Status.fixed():
	case t.FixedPR != 0 && (t.Status.active() || t.Status == StatusClosed):
		t.Status = StatusFixed
	case t.Status.active() || t.Status == StatusSubmitted:
		t.Status = StatusClosed
	}
}

// reopenTicket 回到开发中；修复被否定，清掉修复记录，之后的合并重新记录。
func reopenTicket(t *Ticket) {
	t.Status, t.FixedPR, t.FixedCommit, t.FixedRepo, t.FixedReleaseID, t.DeployedAt = StatusInProgress, 0, "", "", 0, nil
}

func assignTicket(t *Ticket, cur IssueState) {
	if t.Status == StatusOpen && len(cur.Assignees) > 0 {
		t.Status = StatusInProgress
	}
}

func (s *Sync) labelTicket(t *Ticket, cur IssueState, ignoreNeedInfo bool) {
	needInfo := !ignoreNeedInfo && s.o.NeedInfoLabel != "" && slices.ContainsFunc(cur.Labels, func(l string) bool {
		return strings.EqualFold(l, s.o.NeedInfoLabel)
	})
	switch {
	case needInfo && (t.Status == StatusOpen || t.Status == StatusInProgress):
		t.Status = StatusNeedInfo
	case !needInfo && t.Status == StatusNeedInfo:
		t.Status = StatusInProgress
	}
}

// ApplyComment 处理评论变化：保存到工单评论；对外评论首次出现时通知提交人。
func (s *Sync) ApplyComment(ctx context.Context, u CommentUpdate) error {
	_, err := s.applyComment(ctx, u)
	return err
}

// ResyncComment 同 ApplyComment（同一套保存、去重、乱序与已删除判断），另返回处理结果，供历史评论重新同步统计。
func (s *Sync) ResyncComment(ctx context.Context, u CommentUpdate) (CommentResult, error) {
	return s.applyComment(ctx, u)
}

func (s *Sync) applyComment(ctx context.Context, u CommentUpdate) (CommentResult, error) {
	res := CommentResult{Outcome: CommentSkipped}
	if u.FromBot {
		return res, nil // 灵工自己转发的评论，已在发出时记录
	}
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByIssue(ctx, u.Repo, u.Number)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.TicketID != 0 && t.ID != u.TicketID {
			return nil // Issue 已关联到其他工单：不写入
		}
		history := func() error {
			return tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: string(u.Change), Payload: map[string]any{
				"delivery": u.DeliveryID, "sender": u.Author, "comment": u.CommentID, "public": u.Public,
			}})
		}
		if u.Change == CommentDeleted {
			if err := history(); err != nil {
				return err
			}
			return tx.DeleteGiteaComment(ctx, u.CommentID, u.At)
		}
		prev, saved, err := tx.SaveGiteaComment(ctx, Comment{TicketID: t.ID, GiteaID: u.CommentID, AuthorName: u.Author,
			Body: u.Body, Public: u.Public, Model: u.Model, At: u.At})
		if err != nil {
			return err
		}
		if !saved || (prev != nil && prev.Body == u.Body && prev.Public == u.Public) {
			// 未写入（灵工已删除、乱序旧编辑）或内容未变（重投、对账重放、重新同步）：不留痕、不通知。
			// 只有模型准入判定变化（如升级前同步的评论首次判定）时已随本次写入保存，同样不留痕、不改可见性计数
			res.Outcome = CommentUnchanged
			return nil
		}
		res.Outcome = CommentAdded
		if prev != nil {
			res.Outcome = CommentUpdated
			res.Unpublished = prev.Public && !u.Public
		}
		res.Published = u.Public && (prev == nil || !prev.Public)
		if err := history(); err != nil {
			return err
		}
		if res.Published && !u.Silent {
			e := s.event(t, notify.TicketComment, "", u.Body)
			e.CommentID = u.CommentID
			return tx.Emit(ctx, e)
		}
		return nil
	})
	if err != nil {
		return CommentResult{}, err
	}
	return res, nil
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
	pull := u.Repo + "#" + strconv.FormatInt(u.Number, 10)
	if u.Replay {
		seen, err := tx.SeenPull(ctx, t.ID, string(u.Change), pull)
		if err != nil || seen {
			return err
		}
	}
	prev, prevPR, prevCommit, prevRepo := t.Status, t.FixedPR, t.FixedCommit, t.FixedRepo
	switch u.Change {
	case PullOpened, PullReopened:
		if t.Status == StatusOpen {
			t.Status = StatusInProgress
		}
	case PullMerged:
		if r.Closes && (t.Status.active() || t.Status == StatusSubmitted || t.Status == StatusClosed || t.Status == StatusFixed) {
			t.Status, t.FixedPR, t.FixedCommit, t.FixedRepo = StatusFixed, u.Number, u.MergeCommit, u.Repo
		}
	}
	if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "gitea", Kind: string(u.Change), Payload: map[string]any{
		"delivery": u.DeliveryID, "sender": u.Sender, "pull": pull,
		"closes": r.Closes, "merge_commit": u.MergeCommit, "status": string(t.Status), "prev_status": string(prev),
	}}); err != nil {
		return err
	}
	if t.Status == prev && t.FixedPR == prevPR && t.FixedCommit == prevCommit && t.FixedRepo == prevRepo {
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
