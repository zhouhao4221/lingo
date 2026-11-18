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

// Ticket 是工单在领域层的视图，只含 Gitea 回流处理需要的字段。
type Ticket struct {
	ID          int64
	ProjectID   int64
	ProjectKey  string
	RepoID      int64  // 转出前为 0
	Repo        string // "owner/name"，转出前为空
	Type        string // consult | bug | feature
	Title       string
	Status      Status
	UserID      string // 提交人（接入方用户 ID）
	IssueNumber int64  // 主 Issue 编号，Issue 创建前为 0

	GiteaUpdatedAt time.Time // 最近一次采纳的 Issue updated_at，用于丢弃乱序到达的旧事件
	FixedPR        int64
	FixedCommit    string
	Milestone      string
	DueDate        *time.Time
}

// ErrNotFound 表示工单不存在。
var ErrNotFound = errors.New("ticket: not found")

// DeferError 表示事件暂不能处理，应在 Until 之后重试（不算失败）。
type DeferError struct {
	Until  time.Time
	Reason string
}

func (e *DeferError) Error() string {
	return "ticket: deferred until " + e.Until.Format(time.RFC3339) + ": " + e.Reason
}
