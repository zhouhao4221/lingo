package lingo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/assistant"
	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/connector"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/store"
)

// ErrNoModel 表示未配置 Config.Model：知识问答的发送与重试不可用（会话列表、历史、确认与取消不受影响）。
var ErrNoModel = errors.New("lingo: model not configured")

// ErrImagesUnsupported 表示带图提问不可用（REQ-009）：当前的主模型没有声明支持图片（见 ImageModel、
// OpenAIConfig.Images、ModelSlotSettings.Images），或未配置 Config.FetchAttachment。提问不会保存（重试时不会改动消息）。
var ErrImagesUnsupported = errors.New("lingo: images are not supported by the current model or attachment configuration")

// 带图提问（REQ-009）的限制：每条提问最多 MaxQuestionImages 张图片；只接受 PNG、JPEG、WebP；单张按实际读取的内容
// 不超过 MaxQuestionImageBytes，宽高各不超过 MaxQuestionImageSide 像素。
const (
	MaxQuestionImages     = assistant.MaxImages
	MaxQuestionImageBytes = assistant.MaxImageBytes
	MaxQuestionImageSide  = assistant.MaxImageSide
)

// Conversation 是知识问答的会话，只有创建者本人可访问（admin 也看不到他人的会话）。
type Conversation struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"` // 为空时由首条提问生成
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	Cursor       int64     `json:"cursor"` // 会话列表的分页游标：下一页传入 before
}

// ConversationMessage 是会话中的一条消息。
type ConversationMessage struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"conversation_id"`
	Role           string `json:"role"` // user | assistant
	Content        string `json:"content"`
	Status         string `json:"status"` // done | failed（模型出错，可用 RetryMessage 重试）
	// Error 是失败类别：model_error | timeout | canceled | internal_error；带图提问另有 attachment_rejected（接入方拒绝
	// 读取、内容不是声明的图片类型或超出限制）与 attachment_error（读取图片的临时错误），均可用 RetryMessage 重试
	Error string `json:"error,omitempty"`
	// Attachments 是用户消息附带的图片引用（REQ-009，只有 ID、Name、ContentType、Size；forwarded 恒为 false：
	// 问答图片不转出到 Gitea）；没有时省略
	Attachments   []Attachment   `json:"attachments,omitempty"`
	ReplyTo       int64          `json:"reply_to,omitempty"` // 助手消息所回答的用户消息
	Citations     Citations      `json:"citations"`
	PendingAction *PendingAction `json:"pending_action,omitempty"` // 该消息提出的待确认操作
	CreatedAt     time.Time      `json:"created_at"`
}

// Citations 是回答引用的工单与版本（回答当时的快照），接入方可渲染为卡片或链接。
type Citations struct {
	Tickets   []TicketCitation    `json:"tickets"`
	Releases  []ReleaseCitation   `json:"releases"`
	Knowledge []KnowledgeCitation `json:"knowledge"` // 知识条目（REQ-006）
	// Tools 是回答调用的业务数据工具（REQ-008），只有工具名；没有调用时省略
	Tools []ToolCitation `json:"tools,omitempty"`
}

// ToolCitation 是回答调用的业务数据工具（不含结果）。
type ToolCitation struct {
	Name string `json:"name"`
}

// KnowledgeCitation 是回答引用的知识条目。Question 为空表示条目已停用、删除或不再可访问（只保留编号）；
// SourceTicketID 只在调用者能查看来源工单时填写。
type KnowledgeCitation struct {
	ID             int64  `json:"id"`
	Question       string `json:"question"`
	SourceTicketID int64  `json:"source_ticket_id,omitempty"`
}

// TicketCitation 是回答引用的工单。
type TicketCitation struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	IssueURL string `json:"issue_url,omitempty"`
}

// ReleaseCitation 是回答引用的版本。
type ReleaseCitation struct {
	ID          int64     `json:"id"`
	ProjectKey  string    `json:"project"`
	Repo        string    `json:"repo"`
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url,omitempty"`
}

// PendingAction 是待确认操作：灵工在对话中提出、用户确认后才执行的写操作（本期为 ticket_comment：给工单留言）。
// 确认方式：调用 ConfirmPendingAction（确认按钮），或在对话中紧接着回复确认的话；30 分钟内有效，用户取消、
// 发送与之无关的新消息或超时后作废。
type PendingAction struct {
	ID             int64           `json:"id"`
	ConversationID int64           `json:"conversation_id"`
	Kind           string          `json:"kind"`    // ticket_comment
	Summary        string          `json:"summary"` // 给用户看的操作描述
	Payload        json.RawMessage `json:"payload"` // ticket_comment: {"ticket_id":12,"body":"…"}
	Status         string          `json:"status"`  // pending | executed | cancelled | expired | failed
	ExpiresAt      time.Time       `json:"expires_at"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
}

// ActionResult 是确认或取消待确认操作的结果。
type ActionResult struct {
	Action  PendingAction        `json:"action"`
	Message *ConversationMessage `json:"message,omitempty"` // 追加到会话的结果消息；重复确认、重复取消时为空
	Comment *Comment             `json:"comment,omitempty"` // ticket_comment 执行成功时写入的评论
}

// MessageInput 是一次提问。
type MessageInput struct {
	Content string // 最多 4000 字；带图片时可以为空
	// Attachments 是随提问附带的图片（REQ-009）：沿用 Attachment，只填 ID（必填，本条提问内唯一）、Name、ContentType
	// （必填，image/png | image/jpeg | image/webp）与可选的 Size，不接受 URL（灵工不请求地址）。数量、类型与大小限制见
	// MaxQuestionImages 等；回答时经 Config.FetchAttachment 以提问人身份读取（Purpose 为 AttachmentForQuestion），
	// 按内容校验后与提问一起发给回答模型。模型不支持图片时返回 ErrImagesUnsupported。
	Attachments []Attachment
	// TimeZone 是 IANA 时区名（如 "Asia/Shanghai"），用于理解“今天”“上周”；为空时用服务进程的本地时区。
	TimeZone string
}

// CreateConversation 为 u 新建知识问答会话；title 可空（由首条提问生成）。
func (l *Lingo) CreateConversation(ctx context.Context, u User, title string) (Conversation, error) {
	c, err := l.assistant.Create(ctx, l.principal(u), title)
	if err != nil {
		return Conversation{}, assistantErr(err)
	}
	return publicConversation(c), nil
}

// ListConversations 返回 u 的会话，按最近活动倒序；before 为上一页最后一条的 Cursor，limit 为 0 时默认 50，最多 200。
func (l *Lingo) ListConversations(ctx context.Context, u User, before int64, limit int) ([]Conversation, error) {
	cs, err := l.assistant.List(ctx, l.principal(u), before, limit)
	if err != nil {
		return nil, assistantErr(err)
	}
	out := make([]Conversation, 0, len(cs))
	for _, c := range cs {
		out = append(out, publicConversation(c))
	}
	return out, nil
}

// ListConversationMessages 返回会话中 ID 小于 beforeID（0 不限）的最近 limit 条消息，页内按 ID 升序；
// limit 为 0 时默认 50，最多 200。他人的会话返回 ErrNotFound。
func (l *Lingo) ListConversationMessages(ctx context.Context, u User, conversationID, beforeID int64, limit int) ([]ConversationMessage, error) {
	ms, err := l.assistant.Messages(ctx, l.viewer(ctx, u), conversationID, beforeID, limit)
	if err != nil {
		return nil, assistantErr(err)
	}
	out := make([]ConversationMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, publicMessage(m))
	}
	return out, nil
}

// RateMessage 记录 u 对本人会话中一条助手消息的反馈（helpful：有帮助 / 没帮助，以最后一次为准）。回答引用的知识条目
// 累加有帮助 / 没帮助次数；知识类问题的回答第一次被标为有帮助时，以提问与回答入库一条待审核的知识候选（REQ-007）。
// 他人的会话或消息返回 ErrNotFound；用户消息或失败的回答返回 ErrInvalidInput。
func (l *Lingo) RateMessage(ctx context.Context, u User, conversationID, messageID int64, helpful bool) error {
	return assistantErr(l.assistant.Rate(ctx, l.principal(u), conversationID, messageID, helpful))
}

// DeleteConversation 删除 u 的会话，删除后不再可见。
func (l *Lingo) DeleteConversation(ctx context.Context, u User, conversationID int64) error {
	return assistantErr(l.assistant.Delete(ctx, l.principal(u), conversationID))
}

// SendMessage 在会话中提问，返回助手消息。灵工把问题解析为查询意图，按 u 的权限查询工单与版本记录后作答，
// 回答附引用（Citations）；要求给工单留言时返回带 PendingAction 的消息，确认后才写入。
// 模型出错时提问照常保存，返回 Status 为 failed 的助手消息（err 为 nil），可用 RetryMessage 重试。
// 未配置 Config.Model 时返回 ErrNoModel；同一会话上一条提问还在处理时返回 ErrConflict。
func (l *Lingo) SendMessage(ctx context.Context, u User, conversationID int64, in MessageInput) (ConversationMessage, error) {
	return l.SendMessageStream(ctx, u, conversationID, in, nil)
}

// SendMessageStream 同 SendMessage，回答文本逐片段回调 onDelta（模型未实现 StreamModel、或固定回复时整段回调一次）。
// 以返回消息的 Content 为准：极少数情况下（回答引用了数据之外的工单号）灵工会改用按数据生成的回答，与已回调的片段不同。
func (l *Lingo) SendMessageStream(ctx context.Context, u User, conversationID int64, in MessageInput, onDelta func(delta string)) (ConversationMessage, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return ConversationMessage{}, err
	}
	atts, err := questionAttachments(in.Attachments)
	if err != nil {
		return ConversationMessage{}, err
	}
	m, err := l.assistant.Send(withDataCaller(ctx, u), l.viewer(ctx, u), conversationID, in.Content, atts, in.TimeZone, onDelta)
	if err != nil {
		return ConversationMessage{}, assistantErr(err)
	}
	return publicMessage(m), nil
}

// RetryMessage 重新回答失败的助手消息（须是最近一次提问的回答），原地更新该消息；timeZone 同 MessageInput.TimeZone。
// 提问带图片时复用原图片（重新经 Config.FetchAttachment 读取）；当前模型不支持图片时返回 ErrImagesUnsupported。
func (l *Lingo) RetryMessage(ctx context.Context, u User, conversationID, messageID int64, timeZone string) (ConversationMessage, error) {
	return l.RetryMessageStream(ctx, u, conversationID, messageID, timeZone, nil)
}

// RetryMessageStream 同 RetryMessage，回答文本逐片段回调 onDelta。
func (l *Lingo) RetryMessageStream(ctx context.Context, u User, conversationID, messageID int64, timeZone string, onDelta func(delta string)) (ConversationMessage, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return ConversationMessage{}, err
	}
	m, err := l.assistant.Retry(withDataCaller(ctx, u), l.viewer(ctx, u), conversationID, messageID, timeZone, onDelta)
	if err != nil {
		return ConversationMessage{}, assistantErr(err)
	}
	return publicMessage(m), nil
}

// ConfirmPendingAction 确认执行待确认操作（确认按钮），成功时向会话追加结果消息。确认时再次校验权限，留言与
// AddTicketComment 行为一致；被拒（无权限、工单状态不允许）时操作置为 failed 并追加说明。对已执行的操作重复确认
// 返回原结果、不重复执行；已取消、过期或失败的返回 ErrConflict。会话中的提问还在回答时也返回 ErrConflict（稍后重试）。
func (l *Lingo) ConfirmPendingAction(ctx context.Context, u User, conversationID, actionID int64) (ActionResult, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return ActionResult{}, err
	}
	r, err := l.assistant.Confirm(ctx, l.principal(u), conversationID, actionID)
	if err != nil {
		return ActionResult{}, assistantErr(err)
	}
	return publicActionResult(r), nil
}

// CancelPendingAction 取消待确认操作（取消按钮），向会话追加说明；重复取消幂等，已执行、过期或失败的返回 ErrConflict；
// 会话中的提问还在回答时也返回 ErrConflict。
func (l *Lingo) CancelPendingAction(ctx context.Context, u User, conversationID, actionID int64) (ActionResult, error) {
	r, err := l.assistant.Cancel(ctx, l.principal(u), conversationID, actionID)
	if err != nil {
		return ActionResult{}, assistantErr(err)
	}
	return publicActionResult(r), nil
}

func assistantErr(err error) error {
	switch {
	case errors.Is(err, assistant.ErrNoModel):
		return ErrNoModel
	case errors.Is(err, assistant.ErrImagesUnsupported):
		return ErrImagesUnsupported
	}
	return publicErr(err)
}

// questionAttachments 把提问附带的图片转为内部引用：只取 ID、Name、ContentType、Size；带 URL 时拒绝（问答图片
// 只经 FetchAttachment 读取，灵工不请求地址），只读字段（CommentID、Forwarded 等）忽略。其余校验见 assistant.CleanAttachments。
func questionAttachments(in []Attachment) ([]assistant.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]assistant.Attachment, 0, len(in))
	for _, a := range in {
		if strings.TrimSpace(a.URL) != "" {
			return nil, fmt.Errorf("%w: question attachment %q must not have a url (images are read via FetchAttachment)", ErrInvalidInput, a.ID)
		}
		out = append(out, assistant.Attachment{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	return out, nil
}

// fetchQuestionImage 把 Config.FetchAttachment 适配给知识问答（Purpose 为 question，userID 为提问人）；未配置时为 nil。
func (l *Lingo) fetchQuestionImage() assistant.FetchImage {
	f := l.cfg.FetchAttachment
	if f == nil {
		return nil
	}
	return func(ctx context.Context, userID string, convID, msgID int64, a assistant.Attachment) (io.ReadCloser, error) {
		rc, err := f(ctx, userID, Attachment{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size,
			Purpose: AttachmentForQuestion, ConversationID: convID, MessageID: msgID})
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
			return nil, assistant.ErrImageDenied
		}
		if err == nil && rc == nil {
			return nil, assistant.ErrImageDenied // 实现有误：没有内容也没有错误，按拒绝处理
		}
		return rc, err
	}
}

func publicConversation(c *assistant.Conversation) Conversation {
	return Conversation{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, LastActiveAt: c.LastActiveAt, Cursor: c.Seq}
}

func publicMessage(m *assistant.Message) ConversationMessage {
	out := ConversationMessage{ID: m.ID, ConversationID: m.ConversationID, Role: m.Role, Content: m.Content, Status: m.Status,
		Error: m.Error, ReplyTo: m.ReplyTo, Citations: publicCitations(m.Citations), CreatedAt: m.CreatedAt}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	if m.Action != nil {
		a := publicAction(m.Action)
		out.PendingAction = &a
	}
	return out
}

func publicCitations(c assistant.Citations) Citations {
	out := Citations{Tickets: []TicketCitation{}, Releases: []ReleaseCitation{}, Knowledge: []KnowledgeCitation{}}
	for _, k := range c.Knowledge {
		out.Knowledge = append(out.Knowledge, KnowledgeCitation(k))
	}
	for _, t := range c.Tools {
		out.Tools = append(out.Tools, ToolCitation{Name: t.Name})
	}
	for _, t := range c.Tickets {
		out.Tickets = append(out.Tickets, TicketCitation(t))
	}
	for _, r := range c.Releases {
		out.Releases = append(out.Releases, ReleaseCitation{ID: r.ID, ProjectKey: r.Project, Repo: r.Repo, Tag: r.Tag,
			PublishedAt: r.PublishedAt, URL: r.URL})
	}
	return out
}

func publicAction(a *assistant.Action) PendingAction {
	return PendingAction{ID: a.ID, ConversationID: a.ConversationID, Kind: a.Kind, Summary: a.Summary, Payload: a.Payload,
		Status: a.Status, ExpiresAt: a.ExpiresAt, ResolvedAt: a.ResolvedAt}
}

func publicActionResult(r *assistant.ActionResult) ActionResult {
	out := ActionResult{Action: publicAction(r.Action)}
	if r.Message != nil {
		m := publicMessage(r.Message)
		out.Message = &m
	}
	if c := r.Comment; c != nil {
		out.Comment = &Comment{ID: c.ID, Source: c.Source, AuthorID: c.AuthorID, AuthorName: c.AuthorName, Body: c.Body,
			Public: c.Public, At: c.At}
	}
	return out
}

// newAssistant 装配知识问答：意图解析与确认判断用 LightModel（为空用 Model），组织回答用 Model（代码配置或数据库中的
// 连接配置）；未配置主模型时发送返回 ErrNoModel。
func (l *Lingo) newAssistant() (*assistant.Service, error) {
	o := assistant.Options{
		ModelMarked: l.modelMarked(),
		Store:       l.store,
		Tickets:     l.tickets,
		Releases:    l.releases,
		TicketsByID: l.store.GetMany,
		IssueURL:    l.issueURL,
		Projects: func(p auth.Principal) []assistant.ProjectInfo {
			var out []assistant.ProjectInfo
			for _, pr := range l.cfg.Projects {
				if !p.InProject(pr.Key) {
					continue
				}
				info := assistant.ProjectInfo{Key: pr.Key, Name: pr.Name}
				for _, r := range pr.Repos {
					info.Repos = append(info.Repos, r.Owner+"/"+r.Name)
				}
				out = append(out, info)
			}
			return out
		},
		ProjectOfRepo: func(repo string) string {
			for _, pr := range l.cfg.Projects {
				for _, r := range pr.Repos {
					if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
						return pr.Key
					}
				}
			}
			return ""
		},
	}
	o.BrainFor = l.brainFor // 每轮提问取当前的模型（连接配置可在运行时修改）
	o.FetchImage = l.fetchQuestionImage()
	o.Knowledge = l.knowledge
	if !l.data.Empty() {
		o.DataTools = assistantData{l}
	}
	s, err := assistant.New(o)
	if err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	return s, nil
}

type dataCallerKey struct{}

// withDataCaller 把提问人放进 ctx，供问答调用业务数据工具时使用（assistant 包只拿到 auth.Principal）。
func withDataCaller(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, dataCallerKey{}, u)
}

// assistantData 把业务数据工具适配给知识问答：调用人取自 ctx。
type assistantData struct{ l *Lingo }

func (a assistantData) caller(ctx context.Context) (connector.Caller, bool) {
	u, ok := ctx.Value(dataCallerKey{}).(User)
	if !ok {
		return connector.Caller{}, false
	}
	c, err := dataCaller(u)
	return c, err == nil
}

func (a assistantData) Visible(ctx context.Context) []assistant.ToolInfo {
	c, ok := a.caller(ctx)
	if !ok {
		return nil
	}
	var out []assistant.ToolInfo
	for _, t := range a.l.data.Visible(c) {
		out = append(out, assistant.ToolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

func (a assistantData) Run(ctx context.Context, name string, input json.RawMessage, convID int64) (*connector.Result, error) {
	c, ok := a.caller(ctx)
	if !ok {
		return nil, connector.ErrNotFound
	}
	return a.l.data.Run(ctx, c, name, input, connector.ChannelAssistant, convID)
}

// recordUsage 记录知识问答的模型用量（写库失败只记日志）。
func (l *Lingo) recordUsage(ctx context.Context, kind string, u llm.Usage, ok bool, latency time.Duration) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.store.RecordLLMUsage(rctx, store.LLMUsage{Kind: kind, Model: u.Model, Input: u.Input, Output: u.Output,
		Cached: u.Cached, OK: ok, Latency: latency}); err != nil {
		l.log.Warn("lingo: record llm usage failed", "err", err)
	}
}
