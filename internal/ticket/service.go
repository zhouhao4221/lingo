package ticket

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

type sourceKey struct{}

// WithSource 标记调用入口（api | mcp | go），写入审计记录；未标记时为 go。
func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

func sourceOf(ctx context.Context) string {
	if s, ok := ctx.Value(sourceKey{}).(string); ok && s != "" {
		return s
	}
	return "go"
}

// ServiceOptions 是 Service 的配置。
type ServiceOptions struct {
	Store   Store
	Queries QueryStore // 列表与变更流查询；为空时用 Store（若其实现了 QueryStore）
	// Attachments 是附件仓储；为空时用 Store（若其实现了 AttachmentStore）。
	Attachments AttachmentStore
	// AutoForward 返回代码中声明的默认值（lingo.Project.AutoForward），运行时覆盖优先；为空时一律手动确认。
	AutoForward func(projectKey, typ string) bool
	IssueURL    func(repo string, number int64) string
	// Declared 报告项目（repo 为空时）或项目下的仓库是否在当前 Config 中声明；为空时不限。数据库里的项目与仓库
	// 不会因配置删除而停用（见 store.SyncProjects），以此保证只用当前声明的。
	Declared func(projectKey, repo string) bool
	// NeedInfoLabel 是“需补充信息”标签全名；提交人在 need_info 时补充后，由机器人从 Issue 上移除。为空时不移除。
	NeedInfoLabel string
	Now           func() time.Time
}

// Service 是工单的提交、后台确认与查询；所有方法在领域层执行访问控制（PRD 3.2.0.6）。
type Service struct {
	o ServiceOptions
}

// NewService 创建 Service。
func NewService(o ServiceOptions) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("ticket: Store is required")
	}
	if o.AutoForward == nil {
		o.AutoForward = func(string, string) bool { return false }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Queries == nil {
		q, ok := o.Store.(QueryStore)
		if !ok {
			return nil, errors.New("ticket: Queries is required")
		}
		o.Queries = q
	}
	if o.Attachments == nil {
		if a, ok := o.Store.(AttachmentStore); ok {
			o.Attachments = a
		}
	}
	if o.Declared == nil {
		o.Declared = func(string, string) bool { return true }
	}
	return &Service{o: o}, nil
}

// NewTicket 是提交工单的输入。
type NewTicket struct {
	Project      string // 项目 key，必填（根包负责缺省为默认项目）
	Repo         string // "owner/name"，可选，须属于该项目；为空取项目默认仓库
	Type         string
	Title        string // 为空时取正文首行
	Body         string
	Fields       map[string][]string
	Context      map[string]string
	Version      string
	Conversation int64
	Source       string  // api | mcp | go
	Attachments  []int64 // 调用者已上传、尚未关联的附件
}

const (
	maxSummaryRunes = 80
	// MaxTitleRunes 是工单标题上限；Gitea Issue 标题上限 255，留出模板标题前缀的余量。
	MaxTitleRunes = 200
)

// clip 把 s 截到最多 n 个字符。
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func hasValue(fields map[string][]string) bool {
	for _, vs := range fields {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

// Create 提交工单：按项目 + 类型的开关自动转出（submitted，入队建 Issue）或进入待确认（pending_triage）。
func (s *Service) Create(ctx context.Context, p auth.Principal, in NewTicket) (*Ticket, error) {
	switch {
	case !p.Known() || p.ID == "":
		return nil, fmt.Errorf("%w: caller has no user id or role", ErrForbidden)
	case !ValidType(in.Type):
		return nil, fmt.Errorf("%w: type %q", ErrInvalid, in.Type)
	case !p.InProject(in.Project) || !s.o.Declared(in.Project, ""):
		return nil, fmt.Errorf("%w: project %q", ErrNotFound, in.Project)
	}
	in.Title, in.Body = clip(strings.TrimSpace(in.Title), MaxTitleRunes), strings.TrimSpace(in.Body)
	if in.Body == "" && !hasValue(in.Fields) {
		return nil, fmt.Errorf("%w: content is required", ErrInvalid)
	}
	if in.Title == "" {
		in.Title = summarize(in.Body, in.Fields)
	}
	if in.Source == "" {
		in.Source = sourceOf(ctx)
	}
	var t *Ticket
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		proj, err := tx.Project(ctx, in.Project)
		if err != nil {
			return err
		}
		if in.Conversation != 0 {
			owner, err := tx.ConversationOwner(ctx, in.Conversation)
			if errors.Is(err, ErrNotFound) || (err == nil && owner != p.ID) {
				return fmt.Errorf("%w: conversation %d", ErrInvalid, in.Conversation)
			}
			if err != nil {
				return err
			}
		}
		t = &Ticket{ProjectID: proj.ID, ProjectKey: proj.Key, Type: in.Type, Title: in.Title, Body: in.Body,
			Fields: in.Fields, Context: in.Context, Version: in.Version, UserID: p.ID, UserName: p.Name, UserEmail: p.Email,
			Conversation: in.Conversation}
		if err := s.setRepo(ctx, tx, t, proj, in.Repo); err != nil {
			return err
		}
		on, ok, err := tx.AutoForward(ctx, proj.ID, in.Type)
		if err != nil {
			return err
		}
		if !ok {
			on = s.o.AutoForward(proj.Key, in.Type)
		}
		if on && t.RepoID == 0 {
			on = false // 项目没有可用仓库：只能等后台人员确认并指定仓库
		}
		if on {
			t.Status = StatusSubmitted
		} else {
			now := s.o.Now()
			t.Status, t.TriageSince = StatusPendingTriage, &now
		}
		if err := tx.Insert(ctx, t); err != nil {
			return err
		}
		if err := linkAttachments(ctx, tx, p, t.ID, 0, in.Attachments); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: in.Source, Kind: string(notify.TicketCreated),
			Payload: map[string]any{"status": string(t.Status), "auto_forward": on}}); err != nil {
			return err
		}
		if err := tx.Emit(ctx, s.event(t, notify.TicketCreated, "", p.ID, "")); err != nil {
			return err
		}
		if !on {
			return tx.Emit(ctx, s.event(t, notify.TriageRequired, "", p.ID, ""))
		}
		return enqueueCreate(ctx, tx, t, "", "", nil, time.Time{})
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// setRepo 按“指定仓库 → 项目默认仓库”确定工单仓库。
func (s *Service) setRepo(ctx context.Context, tx Tx, t *Ticket, proj *Project, repo string) error {
	if repo != "" && !s.o.Declared(proj.Key, repo) {
		return fmt.Errorf("%w: repo %q is not in project %q", ErrInvalid, repo, proj.Key)
	}
	if repo == "" && t.Repo != "" {
		// 已选的仓库须仍声明在该项目下（配置可能已调整），否则退回默认仓库
		if r, err := tx.Repo(ctx, proj.ID, t.Repo); err == nil && s.o.Declared(proj.Key, r.Name) {
			t.RepoID, t.Repo = r.ID, r.Name
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		t.RepoID, t.Repo = 0, ""
	}
	if repo != "" {
		r, err := tx.Repo(ctx, proj.ID, repo)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: repo %q is not in project %q", ErrInvalid, repo, proj.Key)
		}
		if err != nil {
			return err
		}
		t.RepoID, t.Repo = r.ID, r.Name
		return nil
	}
	if t.RepoID == 0 && proj.DefaultRepo != nil && s.o.Declared(proj.Key, proj.DefaultRepo.Name) {
		t.RepoID, t.Repo = proj.DefaultRepo.ID, proj.DefaultRepo.Name
	}
	return nil
}

// enqueueCreate 入队建 Issue；attachments 为要转出的工单附件，nil 表示全部。
func enqueueCreate(ctx context.Context, tx Tx, t *Ticket, forwardedBy, note string, attachments *[]int64, retry time.Time) error {
	key := OpCreateIssue + ":ticket:" + strconv.FormatInt(t.ID, 10)
	if !retry.IsZero() {
		key += ":retry:" + strconv.FormatInt(retry.UnixNano(), 10) // 上次建单已放弃，重新入队需要新键
	}
	payload := map[string]any{"forwarded_by": forwardedBy, "note": note}
	if attachments != nil {
		ids := dedupIDs(*attachments)
		if ids == nil {
			ids = []int64{} // 指向空切片表示一个都不转出，不能编码成 null（null 表示全部）
		}
		payload["attachment_ids"] = ids
	}
	return tx.Enqueue(ctx, Task{Key: key, TicketID: t.ID, Op: OpCreateIssue, Payload: payload})
}

// summarize 取正文（或第一个字段值）首个非空行作标题，最长 80 个字符。
func summarize(body string, fields map[string][]string) string {
	src := body
	if src == "" {
		ids := make([]string, 0, len(fields))
		for id := range fields {
			ids = append(ids, id)
		}
		sort.Strings(ids) // map 无序：按字段 ID 排序，标题才稳定
		for _, id := range ids {
			if vs := fields[id]; len(vs) > 0 && strings.TrimSpace(vs[0]) != "" {
				src = vs[0]
				break
			}
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(line, "#>-* ")); line != "" {
			return clip(line, maxSummaryRunes)
		}
	}
	return "未命名工单"
}

// ForwardInput 是转出时后台人员可修改的内容；空值表示不修改。
type ForwardInput struct {
	Title string
	Type  string
	Repo  string
	Note  string // 补充说明，写入 Issue
	// Attachments 是要随 Issue 转出的工单附件（截图可能含敏感信息，由后台人员勾选）；nil 表示全部，空切片表示不转出。
	Attachments *[]int64
}

// Forward 把待确认工单转出到 Gitea（reviewer / admin）；建 Issue 失败而变为 abnormal 的工单也可再次转出。
func (s *Service) Forward(ctx context.Context, p auth.Principal, id int64, in ForwardInput) (*Ticket, error) {
	if in.Type != "" && !ValidType(in.Type) {
		return nil, fmt.Errorf("%w: type %q", ErrInvalid, in.Type)
	}
	return s.triage(ctx, p, id, "forward", func(tx Tx, t *Ticket) (string, error) {
		retry := time.Time{}
		if t.Status == StatusAbnormal {
			retry = s.o.Now()
		}
		if in.Title = clip(strings.TrimSpace(in.Title), MaxTitleRunes); in.Title != "" {
			t.Title = in.Title
		}
		if in.Type != "" {
			t.Type = in.Type
		}
		proj, err := tx.Project(ctx, t.ProjectKey)
		if err != nil {
			return "", err
		}
		if err := s.setRepo(ctx, tx, t, proj, in.Repo); err != nil {
			return "", err
		}
		if t.RepoID == 0 {
			return "", fmt.Errorf("%w: repo is required (project has no default repo)", ErrInvalid)
		}
		t.Status = StatusSubmitted
		if err := tx.AddTriage(ctx, Triage{TicketID: t.ID, ActorID: p.ID, Action: "forward", Note: in.Note}); err != nil {
			return "", err
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		return "", enqueueCreate(ctx, tx, t, name, in.Note, in.Attachments, retry)
	})
}

// Answer 直接答复待确认工单并结束（reviewer / admin），答复对提交人可见。
func (s *Service) Answer(ctx context.Context, p auth.Principal, id int64, answer string) (*Ticket, error) {
	if answer = strings.TrimSpace(answer); answer == "" {
		return nil, fmt.Errorf("%w: answer is required", ErrInvalid)
	}
	return s.triage(ctx, p, id, "answer", func(tx Tx, t *Ticket) (string, error) {
		t.Status = StatusAnswered
		if _, err := tx.AddComment(ctx, Comment{TicketID: t.ID, Source: "staff", AuthorID: p.ID, AuthorName: p.Name, Body: answer,
			Public: true, At: s.o.Now()}); err != nil {
			return "", err
		}
		return answer, tx.AddTriage(ctx, Triage{TicketID: t.ID, ActorID: p.ID, Action: "answer", Note: answer})
	})
}

// Reject 驳回待确认工单（reviewer / admin），必须给出原因，原因对提交人可见。
func (s *Service) Reject(ctx context.Context, p auth.Principal, id int64, reason string) (*Ticket, error) {
	if reason = strings.TrimSpace(reason); reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	return s.triage(ctx, p, id, "reject", func(tx Tx, t *Ticket) (string, error) {
		t.Status = StatusRejected
		return reason, tx.AddTriage(ctx, Triage{TicketID: t.ID, ActorID: p.ID, Action: "reject", Note: reason})
	})
}

// Merge 把待确认工单合并到同项目的另一工单（reviewer / admin）。
func (s *Service) Merge(ctx context.Context, p auth.Principal, id, target int64, note string) (*Ticket, error) {
	if target == id || target <= 0 {
		return nil, fmt.Errorf("%w: invalid merge target", ErrInvalid)
	}
	return s.triageWith(ctx, p, id, target, "merge", func(tx Tx, t, dst *Ticket) (string, error) {
		// 目标在同一事务中按 ID 顺序加锁：两张工单互相合并时只有一个能成功
		if dst == nil || dst.ProjectID != t.ProjectID {
			return "", fmt.Errorf("%w: merge target %d not found in project", ErrInvalid, target)
		}
		if dst.Status == StatusMerged || dst.Status == StatusRejected {
			return "", fmt.Errorf("%w: merge target %d is %s", ErrConflict, target, dst.Status)
		}
		t.Status, t.MergedInto = StatusMerged, target
		return note, tx.AddTriage(ctx, Triage{TicketID: t.ID, ActorID: p.ID, Action: "merge", Note: note, TargetID: target})
	})
}

// triage 是四种后台处理的公共流程：鉴权、锁定待确认工单、执行 apply、留痕并发出 ticket.status 与 triage.resolved。
func (s *Service) triage(ctx context.Context, p auth.Principal, id int64, action string,
	apply func(tx Tx, t *Ticket) (body string, err error)) (*Ticket, error) {
	return s.triageWith(ctx, p, id, 0, action, func(tx Tx, t, _ *Ticket) (string, error) { return apply(tx, t) })
}

// triageWith 同 triage，另在同一事务内按 ID 顺序锁定关联工单 other（不存在时传给 apply 的为 nil）。
func (s *Service) triageWith(ctx context.Context, p auth.Principal, id, other int64, action string,
	apply func(tx Tx, t, other *Ticket) (body string, err error)) (*Ticket, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: triage requires reviewer or admin", ErrForbidden)
	}
	var t *Ticket
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		var o *Ticket
		lockOther := func() error {
			if other == 0 {
				return nil
			}
			var err error
			if o, err = tx.LockByID(ctx, other); errors.Is(err, ErrNotFound) {
				o, err = nil, nil
			}
			return err
		}
		if other != 0 && other < id {
			if err := lockOther(); err != nil {
				return err
			}
		}
		var err error
		if t, err = tx.LockByID(ctx, id); err != nil {
			return err
		}
		if other > id {
			if err := lockOther(); err != nil {
				return err
			}
		}
		if !p.InProject(t.ProjectKey) {
			return ErrNotFound
		}
		refwd := action == "forward" && t.Status == StatusAbnormal && t.IssueNumber == 0
		if t.Status != StatusPendingTriage && !refwd {
			return fmt.Errorf("%w: ticket %d is %s, not pending_triage", ErrConflict, id, t.Status)
		}
		prev := t.Status
		body, err := apply(tx, t, o)
		if err != nil {
			return err
		}
		t.TriageSince = nil
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "triage." + action,
			Payload: map[string]any{"status": string(t.Status), "prev_status": string(prev)}}); err != nil {
			return err
		}
		if err := tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, p.ID, body)); err != nil {
			return err
		}
		e := s.event(t, notify.TriageResolved, prev, p.ID, "")
		e.Action = action
		return tx.Emit(ctx, e)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Get 返回调用者可见的工单；不可见与不存在一样返回 ErrNotFound。
func (s *Service) Get(ctx context.Context, p auth.Principal, id int64) (*Ticket, error) {
	t, err := s.o.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !Visible(p, t) {
		return nil, ErrNotFound
	}
	return t, nil
}

// Visible 实现 PRD 3.2.0.6 的可见范围：external 只看本人；agent 看项目内已转出的；reviewer / admin 看项目内全部。
func Visible(p auth.Principal, t *Ticket) bool {
	switch p.Role {
	case auth.RoleExternal:
		return p.ID != "" && t.UserID == p.ID && p.InProject(t.ProjectKey)
	case auth.RoleAgent:
		return p.InProject(t.ProjectKey) && t.Status.forwarded()
	case auth.RoleReviewer, auth.RoleAdmin:
		return p.InProject(t.ProjectKey)
	}
	return false
}

// SetAutoForward 写入（on 非空）或删除（on 为空，恢复代码默认值）项目 + 类型的自动转出开关（仅 admin）。
func (s *Service) SetAutoForward(ctx context.Context, p auth.Principal, project, typ string, on *bool) error {
	switch {
	case !p.Admin():
		return fmt.Errorf("%w: auto-forward settings require admin", ErrForbidden)
	case !ValidType(typ):
		return fmt.Errorf("%w: type %q", ErrInvalid, typ)
	case !p.InProject(project):
		return fmt.Errorf("%w: project %q", ErrNotFound, project)
	}
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		proj, err := tx.Project(ctx, project)
		if err != nil {
			return err
		}
		return tx.SetAutoForward(ctx, proj.ID, typ, on, p.ID)
	})
}

// GetMany 一次读取多张工单（不做访问控制，调用方用 Visible 过滤）。
func (s *Service) GetMany(ctx context.Context, ids []int64) ([]*Ticket, error) {
	return s.o.Store.GetMany(ctx, ids)
}

// Draft 读取待建 Issue 的工单（供 worker 使用，不做访问控制）。
func (s *Service) Draft(ctx context.Context, id int64) (*Ticket, error) {
	return s.o.Store.Get(ctx, id)
}

// IssueCreated 记录建好的 Issue：工单 submitted → open。工单已关联 Issue（如 Webhook 先按标记补记）时不覆盖，返回 false。
// commentIDs 是已写进 Issue 正文的补充评论，一并标记为已转出。
func (s *Service) IssueCreated(ctx context.Context, id int64, number int64, via string, commentIDs []int64) (linked bool, err error) {
	err = s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, id)
		if err != nil {
			return err
		}
		if t.IssueNumber == number {
			// Webhook 已按标记补记了同一个 Issue：正文里的补充评论仍要标记已转出，避免再单独发一遍
			return tx.MarkCommentsForwarded(ctx, t.ID, commentIDs)
		}
		if t.IssueNumber != 0 || t.Status != StatusSubmitted {
			return nil
		}
		t.IssueNumber, t.Status = number, StatusOpen
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		if err := tx.MarkCommentsForwarded(ctx, t.ID, commentIDs); err != nil {
			return err
		}
		linked = true
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "system", Kind: "issue.created",
			Payload: map[string]any{"issue": number, "repo": t.Repo, "via": via}}); err != nil {
			return err
		}
		return tx.Emit(ctx, s.event(t, notify.TicketStatus, StatusSubmitted, "", ""))
	})
	return linked, err
}

// IssueFailed 在建 Issue 被放弃（仓库不可写、配置错误等重试无用的情况）时把工单标记为 abnormal 并通知，
// 后台人员据此排查；工单已关联 Issue 或不再待建时不变。
func (s *Service) IssueFailed(ctx context.Context, id int64, reason string) error {
	return s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if t.IssueNumber != 0 || t.Status != StatusSubmitted {
			return nil
		}
		t.Status = StatusAbnormal
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, Source: "system", Kind: "issue.create_failed",
			Payload: map[string]any{"repo": t.Repo, "reason": reason}}); err != nil {
			return err
		}
		return tx.Emit(ctx, s.event(t, notify.TicketStatus, StatusSubmitted, "", ""))
	})
}

// EventFor 构造工单的对外事件（不含链接），供其他领域（如版本）发事件使用。
func EventFor(t *Ticket, kind notify.Kind, prev Status, actor, body string) notify.Event {
	return notify.Event{Kind: kind, ProjectID: t.ProjectID, ProjectKey: t.ProjectKey, TicketID: t.ID, TicketType: t.Type,
		Title: t.Title, Status: string(t.Status), PrevStatus: string(prev), UserID: t.UserID, ActorID: actor, Body: body}
}

func (s *Service) event(t *Ticket, kind notify.Kind, prev Status, actor, body string) notify.Event {
	e := EventFor(t, kind, prev, actor, body)
	if s.o.IssueURL != nil && t.Repo != "" && t.IssueNumber != 0 {
		e.URL = s.o.IssueURL(t.Repo, t.IssueNumber)
	}
	return e
}
