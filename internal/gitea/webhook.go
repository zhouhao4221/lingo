package gitea

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Webhook 请求头。
const (
	HeaderEvent     = "X-Gitea-Event"      // 事件大类，如 "issues"
	HeaderEventType = "X-Gitea-Event-Type" // 细分类型，如 "issue_label"；分发以它为准
	HeaderDelivery  = "X-Gitea-Delivery"   // 每次投递的 UUID；手动重投会生成新值
	HeaderSignature = "X-Gitea-Signature"  // hex(HMAC-SHA256(secret, body))
)

// X-Gitea-Event-Type 取值中灵问处理的事件。
const (
	EventIssues         = "issues"
	EventIssueAssign    = "issue_assign"
	EventIssueLabel     = "issue_label"
	EventIssueMilestone = "issue_milestone"
	EventIssueComment   = "issue_comment"
	EventPullRequest    = "pull_request"
	EventPullComment    = "pull_request_comment"
	EventRelease        = "release"
	EventCreate         = "create"
	EventStatus         = "status"
	EventWorkflowRun    = "workflow_run"
	EventPush           = "push" // TestHook 发出的测试投递
)

// Issue / PR 事件的 action 取值。
const (
	ActionOpened       = "opened"
	ActionClosed       = "closed"
	ActionReopened     = "reopened"
	ActionEdited       = "edited"
	ActionDeleted      = "deleted"
	ActionAssigned     = "assigned"
	ActionUnassigned   = "unassigned"
	ActionLabelUpdated = "label_updated"
	ActionLabelCleared = "label_cleared"
	ActionMilestoned   = "milestoned"
	ActionDemilestoned = "demilestoned"
	ActionSynchronized = "synchronized"
	ActionCreated      = "created"   // 评论
	ActionPublished    = "published" // Release
	ActionUpdated      = "updated"   // Release
	ActionCompleted    = "completed" // workflow_run
)

// DefaultMaxPayloadBytes 是 ReadDelivery 默认允许的最大请求体。
const DefaultMaxPayloadBytes = 5 << 20

// Webhook 读取与验签错误。
var (
	ErrMissingSignature = errors.New("gitea: webhook signature missing")
	ErrBadSignature     = errors.New("gitea: webhook signature mismatch")
	ErrPayloadTooLarge  = errors.New("gitea: webhook payload too large")
)

// Delivery 是一次已验签的 Webhook 投递。
type Delivery struct {
	ID         string // X-Gitea-Delivery
	Event      string // X-Gitea-Event
	EventType  string // X-Gitea-Event-Type，旧版本缺失时回退为 Event
	Body       []byte // 原始请求体
	ReceivedAt time.Time
}

// ReadDelivery 读取请求体（最多 maxBytes，≤0 时取 DefaultMaxPayloadBytes）并用 secret 验签。
// 验签基于原始字节，必须在反序列化之前完成。
func ReadDelivery(r *http.Request, secret string, maxBytes int64) (*Delivery, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPayloadBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gitea: read webhook body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrPayloadTooLarge
	}
	if err := VerifySignature(secret, body, r.Header.Get(HeaderSignature)); err != nil {
		return nil, err
	}
	d := &Delivery{
		ID:         r.Header.Get(HeaderDelivery),
		Event:      r.Header.Get(HeaderEvent),
		EventType:  r.Header.Get(HeaderEventType),
		Body:       body,
		ReceivedAt: time.Now(),
	}
	if d.EventType == "" {
		d.EventType = d.Event
	}
	return d, nil
}

// VerifySignature 校验 X-Gitea-Signature（也接受 GitHub 兼容的 "sha256=" 前缀写法），常量时间比较。
func VerifySignature(secret string, body []byte, signature string) error {
	if signature == "" {
		return ErrMissingSignature
	}
	signature = strings.TrimPrefix(signature, "sha256=")
	got, err := hex.DecodeString(signature)
	if err != nil {
		return ErrBadSignature
	}
	if !hmac.Equal(got, Sign(secret, body)) {
		return ErrBadSignature
	}
	return nil
}

// Sign 计算 HMAC-SHA256(secret, body)，测试中可用来构造合法签名。
func Sign(secret string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return m.Sum(nil)
}

// ChangesFrom 是修改前的值。
type ChangesFrom struct {
	From string `json:"from"`
}

// Changes 描述 edited 等事件中改动的字段。
type Changes struct {
	Title         *ChangesFrom `json:"title,omitempty"`
	Body          *ChangesFrom `json:"body,omitempty"`
	Ref           *ChangesFrom `json:"ref,omitempty"`
	AddedLabels   []*Label     `json:"added_labels"`
	RemovedLabels []*Label     `json:"removed_labels"`
}

// IssuePayload 对应 issues / issue_assign / issue_label / issue_milestone 事件。
type IssuePayload struct {
	Action     string      `json:"action"`
	Number     int64       `json:"number"`
	Changes    *Changes    `json:"changes,omitempty"`
	Issue      *Issue      `json:"issue"`
	Repository *Repository `json:"repository"`
	Sender     *User       `json:"sender"`
}

// IssueCommentPayload 对应 issue_comment 事件（PR 上的评论 IsPull 为真）。
type IssueCommentPayload struct {
	Action      string       `json:"action"`
	Issue       *Issue       `json:"issue"`
	PullRequest *PullRequest `json:"pull_request,omitempty"`
	Comment     *Comment     `json:"comment"`
	Changes     *Changes     `json:"changes,omitempty"`
	Repository  *Repository  `json:"repository"`
	Sender      *User        `json:"sender"`
	IsPull      bool         `json:"is_pull"`
}

// PullRequestPayload 对应 pull_request 事件；合并表现为 action=closed 且 PullRequest.Merged。
type PullRequestPayload struct {
	Action      string       `json:"action"`
	Number      int64        `json:"number"`
	Changes     *Changes     `json:"changes,omitempty"`
	PullRequest *PullRequest `json:"pull_request"`
	Repository  *Repository  `json:"repository"`
	Sender      *User        `json:"sender"`
}

// ReleasePayload 对应 release 事件。
type ReleasePayload struct {
	Action     string      `json:"action"`
	Release    *Release    `json:"release"`
	Repository *Repository `json:"repository"`
	Sender     *User       `json:"sender"`
}

// CreatePayload 对应 create 事件（新建分支或 tag，RefType 为 "branch" / "tag"）。
type CreatePayload struct {
	SHA        string      `json:"sha"`
	Ref        string      `json:"ref"`
	RefType    string      `json:"ref_type"`
	Repository *Repository `json:"repository"`
	Sender     *User       `json:"sender"`
}

// CommitStatusPayload 对应 status 事件。
type CommitStatusPayload struct {
	ID          int64       `json:"id"`
	SHA         string      `json:"sha"`
	Context     string      `json:"context"`
	State       string      `json:"state"` // pending | success | error | failure | warning
	Description string      `json:"description"`
	TargetURL   string      `json:"target_url"`
	CreatedAt   time.Time   `json:"created_at"`
	Repository  *Repository `json:"repository"`
	Sender      *User       `json:"sender"`
}

// WorkflowRunPayload 对应 workflow_run 事件（需 Gitea ≥ 1.25）。
type WorkflowRunPayload struct {
	Action      string       `json:"action"` // requested | in_progress | completed
	Workflow    *Workflow    `json:"workflow"`
	WorkflowRun *WorkflowRun `json:"workflow_run"`
	Repository  *Repository  `json:"repository"`
	Sender      *User        `json:"sender"`
}
