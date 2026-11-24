package lingo

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/mcp"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// MCPHandler 返回 MCP Server（Streamable HTTP，无状态：只接受 POST，直接返回 JSON 响应），工具与 Handler 能力对齐。
// 身份由 Config.Authenticate 解析，权限与数据隔离同 Go 方法；带 Origin 头的浏览器请求须与 Host 同源。
// 工具：list_projects、list_ticket_types、create_ticket、get_ticket、list_my_tickets、reply_ticket、add_perspective、
// list_perspectives、list_events、
// list_triage、forward_ticket、answer_ticket、merge_ticket、reject_ticket、reject_forwarded_ticket、get_issue_close、
// retry_issue_close、list_releases、get_release、get_llm_usage；
// 知识问答：ask、list_conversations、list_conversation_messages、confirm_action、cancel_action。
func (l *Lingo) MCPHandler() http.Handler { return l.mcp }

const mcpInstructions = `灵工（lingo）连接用户与研发：用户提交咨询 / Bug / 需求工单，经后台确认后由机器人在 Gitea 建 Issue；研发在 Gitea 处理，
进展回传给提交人。研发 Agent 可用 get_ticket 读取工单上下文（含提交人的补充与附件列表），用 reply_ticket 回复提交人
（会同步到 Issue，并通知提交人）。需求工单可按调用者视角（使用者 / 业务方 / 专家 / 决策者）用 add_perspective 追加补充、
list_perspectives 查看分层补充与缺失的必填视角。用 ask 以自然语言询问工单进展与发版情况（会话只有本人可见）；
要求留言时 ask 返回待确认操作，经用户同意后用 confirm_action 执行。所有数据按调用者身份隔离。`

// replyModeNote 说明 Gitea 评论是否会转给提交人，随 GiteaConfig.ReplyMode 变化。
func (l *Lingo) replyModeNote() string {
	if l.cfg.Gitea.ReplyMode == ReplyAll {
		return "\n注意：本实例把 Gitea 中所有非机器人评论都转给提交人，内部讨论不要写在工单 Issue 下。"
	}
	return "\nGitea 中的普通评论不会转给提交人；以 /reply 开头的评论会转给提交人。"
}

func (l *Lingo) newMCPHandler() http.Handler {
	return mcp.New(mcp.Options{
		Name: "lingo", Version: "0.1.0", Instructions: mcpInstructions + l.replyModeNote(),
		AllowedOrigins: l.cfg.MCPAllowedOrigins,
		Authenticate:   func(r *http.Request) (any, error) { return l.cfg.Authenticate(r) },
		ErrorText: func(err error) (string, bool) {
			if status, _ := restError(err); status != 0 {
				return err.Error(), true
			}
			return "", false
		},
		Tools:  l.mcpTools(),
		Logger: l.log,
	})
}

func mcpCtx(ctx context.Context) context.Context { return ticket.WithSource(ctx, "mcp") }

// tool 构造一个工具：A 为参数结构（按 JSON 解析），schema 为参数的 JSON Schema。
func tool[A any](name, desc, schema string, call func(ctx context.Context, u User, a A) (any, error)) mcp.Tool {
	return mcp.Tool{Name: name, Description: desc, InputSchema: json.RawMessage(schema),
		Call: func(ctx context.Context, user any, args json.RawMessage) (any, error) {
			var a A
			if err := mcp.Args(args, &a); err != nil {
				return nil, err
			}
			return call(mcpCtx(ctx), user.(User), a)
		}}
}

type none struct{}

func requireID(id int64) error {
	if id <= 0 {
		return &mcp.ArgError{Msg: "id is required"}
	}
	return nil
}

func (l *Lingo) mcpTools() []mcp.Tool {
	const typeEnum = `{"type":"string","enum":["consult","bug","feature"]}`
	return []mcp.Tool{
		tool("list_projects", "列出可对接的项目及其仓库。提交工单、查询版本时用 project（项目 key）与 repo（owner/name）指定范围。",
			`{"type":"object","properties":{}}`,
			func(ctx context.Context, u User, _ none) (any, error) { return list(l.ListProjects(ctx, u)) }),
		tool("list_ticket_types", "列出可提交的工单类型与各自的表单字段（取自仓库 Issue 模板）。create_ticket 的 fields 按字段 id 填写。",
			`{"type":"object","properties":{"project":{"type":"string"},"repo":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct{ Project, Repo string }) (any, error) {
				return list(l.TicketTypes(ctx, u, a.Project, a.Repo))
			}),
		tool("create_ticket", "以当前用户身份提交工单（咨询 / Bug / 需求）。按项目设置自动转出到 Gitea，或进入待后台确认。",
			`{"type":"object","required":["type"],"properties":{
				"project":{"type":"string","description":"项目 key，缺省为默认项目"},
				"repo":{"type":"string","description":"owner/name，缺省为项目默认仓库"},
				"type":`+typeEnum+`,
				"title":{"type":"string","description":"缺省取正文首行"},
				"content":{"type":"string","description":"问题描述"},
				"fields":{"type":"object","additionalProperties":{"type":"array","items":{"type":"string"}},"description":"表单字段 id → 值"},
				"context":{"type":"object","additionalProperties":{"type":"string"},"description":"页面、浏览器等环境；不要放个人信息"},
				"version":{"type":"string","description":"正在使用的版本"}}}`,
			func(ctx context.Context, u User, a struct {
				Project, Repo string
				Type          TicketType
				Title         string
				Content       string
				Fields        map[string][]string
				Context       map[string]string
				Version       string
			}) (any, error) {
				return l.CreateTicket(ctx, u, TicketInput{Project: a.Project, Repo: a.Repo, Type: a.Type, Title: a.Title,
					Content: a.Content, Fields: a.Fields, Context: a.Context, Version: a.Version})
			}),
		tool("get_ticket", "查看工单：状态、正文、对调用者可见的评论（提交人补充、后台答复、研发回复）与附件列表。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct{ ID int64 }) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				t, cs, as, err := l.ticketDetail(ctx, u, a.ID)
				if err != nil {
					return nil, err
				}
				return struct {
					Ticket
					Comments    []Comment    `json:"comments"`
					Attachments []Attachment `json:"attachments"`
				}{t, cs, as}, nil
			}),
		tool("list_my_tickets", "列出当前用户提交的工单（新的在前），可按状态过滤；before 为上一页最后一条的 id。",
			`{"type":"object","properties":{"status":{"type":"string"},"before":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				Status string
				Before int64
				Limit  int
			}) (any, error) {
				return list(l.ListMyTickets(ctx, u, a.Status, a.Before, a.Limit))
			}),
		tool("reply_ticket", "在工单上留言，对提交人可见：提交人补充说明、后台人员答复，或研发 Agent 回复提交人（同步到 Gitea Issue 并通知提交人）。",
			`{"type":"object","required":["id","body"],"properties":{"id":{"type":"integer"},"body":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID   int64
				Body string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.AddTicketComment(ctx, u, a.ID, a.Body, nil)
			}),
		tool("add_perspective", "以调用者的视角（使用者 / 业务方 / 专家 / 决策者，由身份决定）给需求工单追加一段补充，只追加不可修改；已转出时同步到 Issue 的视角区块。",
			`{"type":"object","required":["id","content"],"properties":{"id":{"type":"integer"},"content":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID      int64
				Content string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.AddPerspective(ctx, u, a.ID, a.Content)
			}),
		tool("list_perspectives", "查看需求工单按视角分组的补充（使用者反馈 / 业务补充 / 专家意见 / 决策意见）与缺失的必填视角。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				ID int64
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.ListPerspectives(ctx, u, a.ID)
			}),
		tool("list_events", "按 id 增量拉取工单进展事件（与推送同源），用返回的最后一个 id 作下次的 after。",
			`{"type":"object","properties":{"after":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				After int64
				Limit int
			}) (any, error) {
				return list(l.ListEvents(ctx, u, a.After, a.Limit))
			}),
		tool("list_triage", "列出待后台确认的工单（reviewer / admin），按进入待确认的先后。",
			`{"type":"object","properties":{"project":{"type":"string"},"type":`+typeEnum+`,"after":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				Project string
				Type    TicketType
				After   int64
				Limit   int
			}) (any, error) {
				return list(l.ListTriage(ctx, u, TriageFilter{ProjectKey: a.Project, Type: a.Type, AfterID: a.After, Limit: a.Limit}))
			}),
		tool("forward_ticket", "把待确认工单转出到 Gitea（reviewer / admin），可修改标题、类型、仓库并附说明；attachment_ids 省略表示附件全部转出。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"},"title":{"type":"string"},"type":`+typeEnum+`,
				"repo":{"type":"string"},"note":{"type":"string"},"attachment_ids":{"type":"array","items":{"type":"string"}}}}`,
			func(ctx context.Context, u User, a struct {
				ID            int64
				Title         string
				Type          TicketType
				Repo, Note    string
				AttachmentIDs *[]string `json:"attachment_ids"`
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.ForwardTicket(ctx, u, a.ID, ForwardOptions{Title: a.Title, Type: a.Type, Repo: a.Repo, Note: a.Note, AttachmentIDs: a.AttachmentIDs})
			}),
		tool("answer_ticket", "直接答复待确认工单并结束，不转出（reviewer / admin）。",
			`{"type":"object","required":["id","answer"],"properties":{"id":{"type":"integer"},"answer":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID     int64
				Answer string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.AnswerTicket(ctx, u, a.ID, a.Answer)
			}),
		tool("merge_ticket", "把待确认工单合并到同项目的另一工单（reviewer / admin）。",
			`{"type":"object","required":["id","target_ticket_id"],"properties":{"id":{"type":"integer"},"target_ticket_id":{"type":"integer"},"note":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID     int64
				Target int64 `json:"target_ticket_id"`
				Note   string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.MergeTicket(ctx, u, a.ID, a.Target, a.Note)
			}),
		tool("reject_ticket", "驳回待确认工单（reviewer / admin），原因对提交人可见。",
			`{"type":"object","required":["id","reason"],"properties":{"id":{"type":"integer"},"reason":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID     int64
				Reason string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.RejectTicket(ctx, u, a.ID, a.Reason)
			}),
		tool("reject_forwarded_ticket", "驳回已转研发、尚未修复的工单（reviewer / admin），原因对提交人可见；保留工单、评论与历史，只关闭工单关联的主 Issue（异步、可重试）。",
			`{"type":"object","required":["id","reason"],"properties":{"id":{"type":"integer"},"reason":{"type":"string"}}}`,
			func(ctx context.Context, u User, a struct {
				ID     int64
				Reason string
			}) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.RejectForwardedTicket(ctx, u, a.ID, a.Reason)
			}),
		tool("get_issue_close", "查看驳回后关闭关联 Issue 的进度（reviewer / admin）：state 为 none / pending / done / failed。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct{ ID int64 }) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.TicketIssueClose(ctx, u, a.ID)
			}),
		tool("retry_issue_close", "关闭关联 Issue 失败（state=failed）后重新入队（reviewer / admin）；排队中或已完成时不重复入队。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct{ ID int64 }) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.RetryTicketIssueClose(ctx, u, a.ID)
			}),
		tool("list_releases", "列出项目（或其下某仓库）的版本记录，新的在前；before 为上一页最后一条的 id。",
			`{"type":"object","properties":{"project":{"type":"string"},"repo":{"type":"string"},"before":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				Project, Repo string
				Before        int64
				Limit         int
			}) (any, error) {
				return list(l.ListReleases(ctx, u, a.Project, a.Repo, a.Before, a.Limit))
			}),
		tool("get_llm_usage", "模型用量（仅 admin）：按天、用途、模型汇总调用次数、token 与前缀缓存命中率；from / to 为 RFC 3339 时间，缺省为最近 30 天。",
			`{"type":"object","properties":{"from":{"type":"string","format":"date-time"},"to":{"type":"string","format":"date-time"}}}`,
			func(ctx context.Context, u User, a struct{ From, To time.Time }) (any, error) {
				return list(l.LLMUsage(ctx, u, a.From, a.To))
			}),
		tool("ask", "以自然语言询问工单进展、修复版本、上线情况与近期发版，回答附引用的工单与版本。conversation_id 为空时新建会话，"+
			"返回值含 conversation_id，追问时传入。要求给工单留言时返回 pending_action，须经用户同意后用 confirm_action 执行。",
			`{"type":"object","required":["question"],"properties":{"question":{"type":"string"},
				"conversation_id":{"type":"integer"},"time_zone":{"type":"string","description":"IANA 时区，如 Asia/Shanghai"}}}`,
			func(ctx context.Context, u User, a struct {
				Question       string
				ConversationID int64  `json:"conversation_id"`
				TimeZone       string `json:"time_zone"`
			}) (any, error) {
				if strings.TrimSpace(a.Question) == "" {
					return nil, &mcp.ArgError{Msg: "question is required"}
				}
				id := a.ConversationID
				if id == 0 {
					if isNil(l.cfg.Model) { // 先校验，避免失败的调用留下空会话
						return nil, ErrNoModel
					}
					c, err := l.CreateConversation(ctx, u, "")
					if err != nil {
						return nil, err
					}
					id = c.ID
				}
				m, err := l.SendMessage(ctx, u, id, MessageInput{Content: a.Question, TimeZone: a.TimeZone})
				if err != nil {
					return nil, err
				}
				return struct {
					ConversationID int64               `json:"conversation_id"`
					Message        ConversationMessage `json:"message"`
				}{id, m}, nil
			}),
		tool("list_conversations", "列出当前用户的问答会话，按最近活动倒序；before 为上一页最后一条的 cursor。",
			`{"type":"object","properties":{"before":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				Before int64
				Limit  int
			}) (any, error) {
				return list(l.ListConversations(ctx, u, a.Before, a.Limit))
			}),
		tool("list_conversation_messages", "查看问答会话的消息（页内按 id 升序）；before 为本页第一条的 id，用于向前翻页。",
			`{"type":"object","required":["conversation_id"],"properties":{"conversation_id":{"type":"integer"},"before":{"type":"integer"},"limit":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				ConversationID int64 `json:"conversation_id"`
				Before         int64
				Limit          int
			}) (any, error) {
				if err := requireID(a.ConversationID); err != nil {
					return nil, err
				}
				return list(l.ListConversationMessages(ctx, u, a.ConversationID, a.Before, a.Limit))
			}),
		tool("confirm_action", "执行 ask 返回的待确认操作（如给工单留言）。只在用户明确同意后调用。",
			`{"type":"object","required":["conversation_id","action_id"],"properties":{"conversation_id":{"type":"integer"},"action_id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				ConversationID int64 `json:"conversation_id"`
				ActionID       int64 `json:"action_id"`
			}) (any, error) {
				if err := requireID(a.ConversationID); err != nil {
					return nil, err
				}
				if err := requireID(a.ActionID); err != nil {
					return nil, err
				}
				return l.ConfirmPendingAction(ctx, u, a.ConversationID, a.ActionID)
			}),
		tool("cancel_action", "取消 ask 返回的待确认操作。",
			`{"type":"object","required":["conversation_id","action_id"],"properties":{"conversation_id":{"type":"integer"},"action_id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct {
				ConversationID int64 `json:"conversation_id"`
				ActionID       int64 `json:"action_id"`
			}) (any, error) {
				if err := requireID(a.ConversationID); err != nil {
					return nil, err
				}
				if err := requireID(a.ActionID); err != nil {
					return nil, err
				}
				return l.CancelPendingAction(ctx, u, a.ConversationID, a.ActionID)
			}),
		tool("get_release", "查看版本：说明、关联的工单（按调用者可见性过滤）与上线记录。",
			`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`,
			func(ctx context.Context, u User, a struct{ ID int64 }) (any, error) {
				if err := requireID(a.ID); err != nil {
					return nil, err
				}
				return l.GetRelease(ctx, u, a.ID)
			}),
	}
}
