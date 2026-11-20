package ticket

import (
	"errors"
	"time"
)

// Status 是工单状态，取值与 tickets.status 约束一致（PRD 3.2.2）。
type Status string

const (
	StatusPendingTriage Status = "pending_triage" // 待后台确认
	StatusAnswered      Status = "answered"       // 后台直接答复，未转出
	StatusMerged        Status = "merged"         // 已合并到其他工单
	StatusRejected      Status = "rejected"       // 已驳回，未转出
	StatusSubmitted     Status = "submitted"      // 已确认，Issue 待创建
	StatusOpen          Status = "open"           // 已转研发
	StatusInProgress    Status = "in_progress"    // 已领取 / 开发中
	StatusNeedInfo      Status = "need_info"      // 需提交人补充
	StatusFixed         Status = "fixed"          // 已修复，待发版
	StatusReleased      Status = "released"       // 修复已随版本发布
	StatusDeployed      Status = "deployed"       // 已上线
	StatusClosed        Status = "closed"         // 关闭未修复
	StatusAbnormal      Status = "abnormal"       // Issue 被删除或仓库不可访问
)

// active 表示 Issue 尚在处理中：Gitea 侧的指派、标签、PR 等事件只对这些状态生效。
func (s Status) active() bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusNeedInfo:
		return true
	}
	return false
}

// fixed 表示已有合并的修复（含已发布、已上线）。
func (s Status) fixed() bool {
	switch s {
	case StatusFixed, StatusReleased, StatusDeployed:
		return true
	}
	return false
}

// 工单类型，取值与 lingo.TicketType 一致。
const (
	TypeConsult = "consult"
	TypeBug     = "bug"
	TypeFeature = "feature"
)

// ValidType 报告 t 是否为合法工单类型。
func ValidType(t string) bool { return t == TypeConsult || t == TypeBug || t == TypeFeature }

// forwarded 表示工单已确认转出（研发 Agent 可见）。
func (s Status) forwarded() bool {
	switch s {
	case StatusPendingTriage, StatusAnswered, StatusMerged, StatusRejected:
		return false
	}
	return true
}

// Ticket 是工单在领域层的视图。
type Ticket struct {
	ID             int64
	ProjectID      int64
	ProjectKey     string
	RepoID         int64  // 未选定仓库时为 0
	Repo           string // "owner/name"，未选定仓库时为空
	Type           string // consult | bug | feature
	Title          string
	TitleAuto      bool // 标题由灵问从正文生成（提交人未填写），建 Issue 时可由模型改写
	Body           string
	Fields         map[string][]string // Issue 表单模板字段取值（按字段 ID）
	Context        map[string]string   // 页面 URL、浏览器等提交环境
	Version        string              // 提交人使用的版本
	Status         Status
	UserID         string // 提交人（接入方用户 ID）
	UserName       string // 提交人姓名快照，仅展示
	UserEmail      string
	IssueNumber    int64 // 主 Issue 编号，Issue 创建前为 0
	MergedInto     int64
	TriageSince    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FixedTag       string     // 修复版本（只读，取自 FixedReleaseID）
	FixedReleaseID int64      // 最早包含修复的正式版本
	DeployedAt     *time.Time // 上线时间

	GiteaUpdatedAt time.Time // 最近一次采纳的 Issue updated_at，用于丢弃乱序到达的旧事件
	FixedPR        int64
	FixedCommit    string
	FixedRepo      string // 修复 PR 所在仓库 "owner/name"；为空表示与工单仓库相同
	Milestone      string
	DueDate        *time.Time
}

var (
	// ErrNotFound 表示工单（或项目、仓库）不存在，或调用者无权访问。
	ErrNotFound = errors.New("ticket: not found")
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("ticket: invalid input")
	// ErrForbidden 表示调用者的角色不能执行该操作。
	ErrForbidden = errors.New("ticket: forbidden")
	// ErrConflict 表示工单当前状态不允许该操作（如对已处理的工单再次转出）。
	ErrConflict = errors.New("ticket: conflict")
)

// DeferError 表示事件暂不能处理，应在 Until 之后重试（不算失败）。
type DeferError struct {
	Until  time.Time
	Reason string
}

func (e *DeferError) Error() string {
	return "ticket: deferred until " + e.Until.Format(time.RFC3339) + ": " + e.Reason
}
