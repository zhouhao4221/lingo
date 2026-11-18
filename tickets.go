package lingo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhouhao4221/lingo/internal/auth"
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
	// ConversationID 是从问答转出时的会话 ID。
	ConversationID int64
}

// ForwardOptions 是后台人员转出工单时可修改的内容；零值表示不修改。
type ForwardOptions struct {
	Title string
	Type  TicketType
	Repo  string // "owner/name"，须属于工单所在项目
	Note  string // 补充说明，写入 Issue
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
	t, err := l.tickets.Create(ctx, principal(u), ticket.NewTicket{Project: in.Project, Repo: in.Repo, Type: string(in.Type),
		Title: in.Title, Body: in.Content, Fields: in.Fields, Context: in.Context, Version: in.Version,
		Conversation: in.ConversationID, Source: "go"})
	return l.ticketResult(t, err)
}

// GetTicket 返回工单 id；无权访问时与不存在一样返回 ErrNotFound（见数据隔离）。
func (l *Lingo) GetTicket(ctx context.Context, u User, id int64) (Ticket, error) {
	t, err := l.tickets.Get(ctx, principal(u), id)
	return l.ticketResult(t, err)
}

// ForwardTicket 把待确认工单转出到 Gitea（仅 reviewer / admin）。
func (l *Lingo) ForwardTicket(ctx context.Context, u User, id int64, o ForwardOptions) (Ticket, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return Ticket{}, err
	}
	t, err := l.tickets.Forward(ctx, principal(u), id, ticket.ForwardInput{Title: o.Title, Type: string(o.Type), Repo: o.Repo, Note: o.Note})
	return l.ticketResult(t, err)
}

// AnswerTicket 直接答复待确认工单并结束，不转出（仅 reviewer / admin）；答复对提交人可见。
func (l *Lingo) AnswerTicket(ctx context.Context, u User, id int64, answer string) (Ticket, error) {
	t, err := l.tickets.Answer(ctx, principal(u), id, answer)
	return l.ticketResult(t, err)
}

// MergeTicket 把待确认工单合并到同项目的工单 target（仅 reviewer / admin）。
func (l *Lingo) MergeTicket(ctx context.Context, u User, id, target int64, note string) (Ticket, error) {
	t, err := l.tickets.Merge(ctx, principal(u), id, target, note)
	return l.ticketResult(t, err)
}

// RejectTicket 驳回待确认工单（仅 reviewer / admin），reason 必填且对提交人可见。
func (l *Lingo) RejectTicket(ctx context.Context, u User, id int64, reason string) (Ticket, error) {
	t, err := l.tickets.Reject(ctx, principal(u), id, reason)
	return l.ticketResult(t, err)
}

// SetAutoForward 在运行时覆盖项目 project 下工单类型 t 是否自动转出（仅 admin），存入数据库并优先于
// Project.AutoForward；u 为操作人，用于权限校验与留痕。
func (l *Lingo) SetAutoForward(ctx context.Context, u User, project string, t TicketType, on bool) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	return publicErr(l.tickets.SetAutoForward(ctx, principal(u), project, string(t), &on))
}

// ResetAutoForward 删除运行时覆盖，恢复为 Project.AutoForward 的默认值（仅 admin）。
func (l *Lingo) ResetAutoForward(ctx context.Context, u User, project string, t TicketType) error {
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	return publicErr(l.tickets.SetAutoForward(ctx, principal(u), project, string(t), nil))
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

func (l *Lingo) repoConfig(repo string) worker.RepoConfig {
	for _, p := range l.cfg.Projects {
		for _, r := range p.Repos {
			if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
				return worker.RepoConfig{Assignees: r.Assignees, Templates: r.Templates}
			}
		}
	}
	return worker.RepoConfig{}
}

func principal(u User) auth.Principal {
	return auth.Principal{ID: u.ID, Name: u.Name, Email: u.Email, Role: string(u.Role), Projects: u.Projects}
}

func (l *Lingo) ticketResult(t *ticket.Ticket, err error) (Ticket, error) {
	if err != nil {
		return Ticket{}, publicErr(err)
	}
	return l.publicTicket(t), nil
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
