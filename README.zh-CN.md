# 灵工 Lingo

<p align="center"><img src="assets/logo/lingo-icon-color.svg" width="96" alt="灵工 Lingo logo"></p>

[English](README.md) | 简体中文

灵工是可嵌入业务项目的 Go 库，连接终端用户与在 Gitea 中工作的 AI 研发团队：用户在你的应用里提交咨询、Bug 与需求，灵工整理为结构化的 Gitea Issue，双向同步评论与状态，并把进展回传给你的应用，直到修复发布、上线。

灵工只提供公开 API（Go 方法、REST Handler、MCP Server、Gitea Webhook Handler），无租户、无前端：登录、用户管理、界面与部署由接入方负责。

> **状态：** 工单流程已端到端实现；知识问答（工单与版本问答）与知识库（后台答复与评论入库、结案工单抽取、回答反馈、审核、检索作答）已实现。首个正式版本发布前 API 仍可能调整。

## 功能

- **反馈工单**：咨询 / Bug / 需求，支持文字、表单字段、截图与附件（灵工只存附件引用，文件在接入方自己的附件系统中，可经 `Config.FetchAttachment` 读取后上传到 Gitea，读取时会传入提交人 ID 供接入方校验归属）；接口按接入方自建的弹窗组件设计
- **后台确认**：转出到 Gitea、直接答复、合并或驳回；按项目与类型的自动 / 手动开关；超时提醒
- **工单整理**：可选的轻量模型生成清晰标题、从自由描述中抽取表单字段、整理描述（按模板逐项校验，并保留原始描述）
- **Gitea 对接**：按仓库 Issue 表单模板建单、评论双向同步、识别 PR 合并、按提交包含关系关联版本、上线信号（Actions 工作流、提交状态或 `MarkDeployed`）
- **可靠同步**：Webhook 验签、去重、异步处理；定时对账补齐漏掉的投递；启动自检与可选的 Webhook 自动注册
- **进展通知**：事件投递给你的 `Notifier`（至少一次，带事件 ID），并提供变更流；被合并工单的提交人随目标工单收到进展
- **数据隔离**：一切按调用者的用户 ID、角色与项目隔离；提交人身份不会写入 Gitea
- **知识问答**：用自然语言问工单进展、修复版本、近期发版，回答附引用；对话中给工单留言须确认；会话只有本人可见，支持流式输出（见下文「知识问答」）
- **业务数据工具**：接入方注册只读 Tool，问答与 MCP 以提问人的身份代查业务数据，权限预校验、脱敏、审计（见下文「业务数据工具」）
- **MCP Server**：同样的能力以 MCP 工具提供给 AI 客户端与研发 Agent（Agent 可直接回复提交人）

## 安装

```sh
go get github.com/zhouhao4221/lingo
```

要求：Go 1.24+、PostgreSQL 13+、Gitea 1.22+（用 Gitea Actions 作上线信号需 1.25+）。

## 使用

```go
import _ "github.com/jackc/pgx/v5/stdlib" // 或任意 database/sql 的 PostgreSQL 驱动

if err := lingo.Migrate(ctx, db); err != nil { ... } // 在发布流程中执行

// 只读检查迁移是否就绪（不建表、不改数据），未就绪时可只禁用反馈入口：
st, err := lingo.CheckMigrations(ctx, db) // err != nil：检查失败、状态未知；err == nil && !st.Ready：未就绪（st.Pending / st.MissingTables …）

l, err := lingo.New(lingo.Config{
	DB:           db,       // *sql.DB（PostgreSQL）
	Model:        model,    // 可选：知识问答组织回答（不配置时知识问答不可用）
	LightModel:   light,    // 可选：便宜、快速的 lingo.Model，建 Issue 前整理工单、问答意图解析（为空用 Model）
	Gitea:        lingo.GiteaConfig{BaseURL: url, Token: botToken, WebhookSecret: secret},
	Projects:     projects, // 项目及其 Gitea 仓库
	Authenticate: authn,    // 从请求解析 lingo.User（ID、角色、项目）
	Notifier:     notifier, // 可选：接收工单进展事件
})

mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler())) // REST JSON
mux.Handle("/mcp", l.MCPHandler())                             // MCP（Streamable HTTP）
mux.Handle("/webhooks/gitea", l.WebhookHandler())              // Gitea Webhook
go l.Run(ctx)                                                  // 后台任务
```

内置模型（标准库 HTTP，不引入 SDK）：`lingo.NewOpenAIModel(lingo.OpenAIConfig{...})` 对接 OpenAI 或任意 OpenAI 兼容接口（网关、vLLM、Ollama 的 `/v1` 等），`lingo.NewDeepSeekModel(apiKey, "")` 对接 DeepSeek；也可以自己实现 `lingo.Model`。

```go
light, _ := lingo.NewDeepSeekModel(os.Getenv("DEEPSEEK_API_KEY"), "")
// 或：lingo.NewOpenAIModel(lingo.OpenAIConfig{BaseURL: "https://gateway.example.com/v1", APIKey: key, Model: "your-model"})
```

完整示例见 [examples/server](examples/server/main.go)。

### 角色

`Authenticate` 把用户映射为四个固定角色之一：`external`（终端用户，只看本人工单；可用 `TicketAudience` 放宽为只读查看同事的，见下节）、`agent`（研发 Agent，看所属项目已转出的工单）、`reviewer`（后台确认，看所属项目全部工单）、`admin`。`User.Projects` 可选，用于限制可访问的项目。`User.Perspective` 可选，是与权限无关的需求视角，见下节。

### 同事可见（TicketAudience）

默认 external 只能看本人的工单。配置 `Config.TicketAudience` 后，可按接入方自己的规则（如同部门、同团队）让 external 额外看到某些提交人的工单：

```go
TicketAudience: func(ctx context.Context, u lingo.User) (lingo.TicketAudience, error) {
	return lingo.TicketAudience{UserIDs: directory.Teammates(u.ID)}, nil // 接入方的组织数据
},
```

- **只读、不超出 `User.Projects`**：查看工单与对外评论、`ListVisibleTickets`（REST `GET /tickets/visible`、MCP `list_visible_tickets`）、知识问答的查找与汇总、版本关联的工单跟随放宽；工单返回 `mine` 区分是否本人提交。
- **不跟随**：留言、补附件、补充视角、确认留言（对同事的工单返回 `not_found`），附件、`ListEvents` / `Notifier`（只对提交人本人），需求视角（未配置 `AuthorizePerspective` 时）。
- **合并工单**按提交人判断：同事的工单被合并进他人的主工单时，主工单不随之可见。
- **问答历史**：重新打开会话时，引用的工单按当前范围重新判断，已不可见的只保留编号；已生成的回答正文不做处理。
- 只对 external 调用（`User.ID` 为空不调用），每个读请求最多调用一次、不缓存；名单去重后最多取前 1000 个；回调返回错误时按未放宽处理并记 Warn 日志。
- 这是隐私变更，启用前请告知用户，例如：“你提交的工单（标题、描述、进展与公开回复）同部门成员可以查看，附件仅你和处理人员可见。”

### 需求视角

同一条需求（`feature` 工单）可以由不同身份的人分别补充：使用者 `user` 说现象与诉求、业务方 `business` 说影响范围、专家 `expert` 给方案建议、决策者 `leader` 定做不做与优先级。转出后 Issue 正文按视角分段展示（每段标注姓名与时间），并打 `<前缀>/视角/<视角名>` 标签；转出后的补充由机器人重写正文区块并发一条提示评论。视角只用于给内容归类，不授予任何权限。

接入分四步（完整示例见 [example_perspective_test.go](example_perspective_test.go)、[examples/server](examples/server/main.go)）：

```go
lingo.Config{
	// 1. 在 Authenticate 中按自己的用户身份映射视角（为空视为 user）
	Authenticate: func(r *http.Request) (lingo.User, error) {
		u := currentUser(r)
		return lingo.User{ID: u.ID, Name: u.Name, Role: lingo.RoleExternal, Perspective: perspectiveOf(u)}, nil
	},
	// 2. 谁能补充（write）/ 查看哪条需求。不配置时同工单可见范围：external 只能是本人的需求，
	//    agent 只能是已转出的，reviewer / admin 为项目内全部。要让业务方等参与他人的需求，在这里放行
	AuthorizePerspective: func(ctx context.Context, u lingo.User, t lingo.Ticket, write bool) error {
		if u.Perspective != "" || t.UserID == u.ID {
			return nil
		}
		return lingo.ErrNotFound // 或 lingo.ErrForbidden
	},
	// 3. 必填视角（可选）：转出时缺失发 ticket.perspective_missing，Issue 区块标注“缺少：…”；
	//    Project.RequiredPerspectives 可覆盖，空切片表示该项目不检查
	RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveLeader},
	// 4. Notifier 按 Event.Perspectives 提醒相应的人（推给谁由你决定）
}
```

调用方式（Go / REST / MCP 同一套校验）：

| Go | REST | MCP |
|---|---|---|
| `l.AddPerspective(ctx, u, id, content)` | `POST /tickets/{id}/perspectives` `{"content":"…"}` → 201 | `add_perspective {id, content}` |
| `l.ListPerspectives(ctx, u, id)` | `GET /tickets/{id}/perspectives` → 200 | `list_perspectives {id}` |

```json
// POST 返回一段补充
{"id": 7, "perspective": "business", "author_name": "张三", "content": "影响华东 30 家门店", "at": "2026-11-21T09:30:00Z"}
// GET 返回按视角分组的补充（固定顺序，只含有内容的视角）与缺失的必填视角；首段为工单原始提交（original，id 为 0）
{"ticket_id": 42,
 "groups": [{"perspective": "user", "title": "使用者反馈", "entries": [{"id": 0, "perspective": "user", "author_name": "王五", "content": "希望支持导出 Excel", "at": "…", "original": true}]},
            {"perspective": "business", "title": "业务补充", "entries": [{"id": 7, "…": "…"}]}],
 "missing": ["leader"]}
```

错误：`invalid_input`（400，非需求工单、视角非法、内容为空或超过 10000 字）、`forbidden`（403）、`not_found`（404，不存在、项目外或无权查看）、`conflict`（409，工单已答复 / 合并 / 驳回 / 关闭）。

注意：

- 补充只能追加，不能修改或撤回；已修复、已发布、已上线的需求仍可补充。
- 补充人与首段提交人的姓名（`User.Name`）会写入 Gitea Issue，邮箱与用户 ID 不会。
- 事件 `ticket.perspective_added`（`Perspectives` 为本次视角，`Body` 为“业务方 · 张三 补充了内容”，不含补充内容）与 `ticket.perspective_missing`（`Perspectives` 为缺失视角）的可见范围同其他工单事件。
- 机器人令牌需能创建标签（`write:issue`）；视角标签在启动自检时补齐。

### 知识问答（对话窗口）

用户可以用自然语言问工单与发版，例如“我上周提的登录 bug 修了吗”“最近发版了什么”“demo 项目还有几个待确认的工单”，也可以说“在 #12 补一句：复现环境是 Chrome 120”。推荐接入方把它做成一个独立的对话窗口：左侧是会话列表，右侧是聊天区。

- 灵工的做法：模型只把提问解析为查询意图；查询由灵工按调用者的角色与项目执行（可见范围同 `GetTicket`）；模型依据查询结果组织回答。回答附结构化引用（`citations`：工单 ID、标题、状态、Issue 地址；版本 tag、发布时间），可渲染为卡片或链接。
- 送给模型的内容：提问与最近 10 轮对话；工单的编号、项目、仓库、类型、标题、状态、时间、修复版本、里程碑与正文摘要；单张工单的最近 5 条评论正文（按提问人可见范围，reviewer / admin 包括研发内部讨论）；版本的名称、说明摘要、上线记录与修复的工单；知识类问题另含检索到的知识条目（问题与答案摘要）。不含提交人与评论作者的姓名、邮箱、用户 ID（例外：配置了 `TicketAudience` 时，同事工单带提交人姓名，见上文「同事可见」），也不含表单字段、提交环境与附件。模型由接入方提供，请按自己的合规要求选择。
- 写操作须确认：要求留言时，助手消息带 `pending_action`；窗口可显示“确认 / 取消”按钮（调用 confirm / cancel），用户也可以直接回复“确认”。30 分钟内有效，取消、问别的或超时后作废。
- 会话只有本人可见（admin 也不行），可续聊、删除；灵工不自动清理，保留多久由接入方决定。
- 需要 `Config.Model`（意图解析用 `LightModel`，为空时用 `Model`）；未配置时发送返回 `ErrNoModel`（REST 501）。模型实现 `lingo.StreamModel` 时逐字输出，内置模型已实现（注意 `OpenAIConfig.HTTPClient` 的超时是整个回答的时限，默认 90 秒）。
- 回答工单、版本与知识类问题（使用方法、原因、怎么办，见下节「知识库」），以及经注册的工具查询提问人本人的业务数据（见「业务数据工具」）。

```go
c, _ := l.CreateConversation(ctx, u, "")
m, err := l.SendMessageStream(ctx, u, c.ID, lingo.MessageInput{Content: "我上周提的登录 bug 修了吗", TimeZone: "Asia/Shanghai"},
	func(delta string) { fmt.Print(delta) }) // 逐片段输出；最终以 m.Content 为准
if m.PendingAction != nil { /* 显示确认按钮，确认时调用 l.ConfirmPendingAction(ctx, u, c.ID, m.PendingAction.ID) */ }
```

REST 流式：`POST /conversations/{id}/messages`，请求体 `{"content":"…","time_zone":"Asia/Shanghai","stream":true}`，响应为 SSE：

```
event: delta
data: {"text":"#12 已修复"}

event: message
data: {"id":8,"role":"assistant","content":"#12 已修复，已在 v1.3.0 发布。","status":"done","citations":{…}}
```

最后恰好一个 `message` 事件，以它的 `content` 为准；开始输出后出错发送 `event: error`（`{"code","message"}`）；开始输出前的错误（如 404、409、501）仍是普通 JSON 错误响应。模型出错时助手消息的 `status` 为 `failed`，可调用 retry 重试。用 `fetch` 读取流（`EventSource` 不支持 POST）。

### 知识库

知识类问题（“导出报表为什么是空的”“怎么配置 SSO”）由知识库回答：灵工在提问人可访问的项目里检索已生效的知识条目（最多 5 条），并检索提问人可见的同类工单，交给模型依据作答；回答在 `citations.knowledge` 中附知识条目（编号、问题；来源工单只在提问人能查看时给出），条目的引用次数加一。都没有命中时回复“知识库里还没有相关内容”，引导提交咨询工单。

- **入库**：reviewer / admin 直接答复待确认工单时可勾选加入知识库（`AnswerTicketWith`，REST `POST /tickets/{id}/answer` 带 `knowledge`，MCP `answer_ticket` 的 `add_to_knowledge`），默认不入库；也可手动新建（`CreateKnowledge`）。入库即生效、所有能访问该项目的用户都可能检索到，**请把问题与答案改写为通用内容，去掉只针对提交人的信息**；邮箱与手机号会自动替换为占位。
- **维护**（reviewer / admin，限 `User.Projects`）：`ListKnowledge`（可按引用次数排序）、`UpdateKnowledge`、`DisableKnowledge` / `EnableKnowledge`、`DeleteKnowledge`、`DisableKnowledgeByTicket`（提交人要求删除个人数据时，按来源工单停用派生的条目）。
- **检索**（所有角色）：`SearchKnowledge`（REST `GET /knowledge/search`、MCP `search_knowledge`）。不需要数据库扩展：中文按二元切词、用 GIN 数组索引按重合度排序，没有命中时按关键词包含匹配兜底。
- **向量（可选）**：主模型的 `Embed` 可用时，`Run` 在后台为条目生成向量（新建、编辑后，或换了向量模型），检索时对前 50 条按相似度重排；模型实现 `lingo.EmbeddingModelNamer` 时按模型名区分向量（内置适配器已实现，DeepSeek 与未配置 `EmbeddingModel` 的 OpenAI 适配器不生成向量），否则按维度区分。查询时多一次 `Embed` 调用，出错时只用切词检索。
- **自动增长与审核**（REQ-007）：
  - 后台在工单上发表对外评论时也可勾选入库（`AddTicketCommentWith`，REST `POST /tickets/{id}/comments` 带 `knowledge`，MCP `reply_ticket` 的 `add_to_knowledge`），直接生效。
  - Bug / 需求工单首次进入已发布、已上线或已关闭（未修复）后，`Run` 用轻量模型抽取一条问答，**待审核**；每张工单最多抽取一次，未配置模型时不抽取。**升级到本版本时已结案的工单不抽取**。
  - 用户对回答反馈有帮助 / 没帮助（`RateMessage`，REST `POST /conversations/{id}/messages/{mid}/feedback`，MCP `rate_message`）：回答引用的条目累加次数；知识类问题的回答第一次被标为有帮助时，提问与回答（去掉工单号、知识编号并脱敏）作为**待审核**候选入库。
  - 审核（reviewer / admin）：`ListKnowledge`（`Status: KnowledgePending`）列出待审核条目，`ApproveKnowledge`（可带编辑后的内容）/ `RejectKnowledge`；入库时与已生效条目重合比例不低于 60% 的标出 `duplicate_of`，只提示不合并。待审核、已驳回的条目不参与检索与回答。
  - 未答上的问题：知识类问题没有命中时，在提问人可访问的项目里各记录一条脱敏后的提问，近 90 天同项目的同类提问归并计数，`ListUnansweredQuestions`（REST `GET /knowledge/unanswered`）按次数列出，供补充知识。
- 检索只返回重合词数不少于提问与条目中词数较少一方 20% 的条目，避免只因“怎么”“为什么”等常用词命中无关内容。
- 用量：作答记为 `knowledge_answer`，生成向量记为 `knowledge_embed`（只记次数），抽取记为 `knowledge_extract`。需要迁移 `0005_knowledge.sql`、`0006_knowledge_review.sql`。

### 业务数据工具（Tool）

接入方注册只读的业务数据工具后，灵工能回答"订单 1024 为什么审批失败""我的请假余额还有几天"这类问题：知识问答按需选一个工具、以**提问人的身份**调用，并据结果作答（引用 `citations.tools` 只记工具名）；MCP 客户端在 `tools/list` 中看到自己可调用的工具并直接调用；接入方界面可调用 `CallTool`。

```go
orderTool := lingo.Tool{
	Name:        "get_order",
	Description: "按订单号查询订单状态与审批记录",
	InputSchema: json.RawMessage(`{"type":"object","required":["order_id"],
		"properties":{"order_id":{"type":"string","description":"订单号"}}}`),
	Scope:     "order:read",               // User.Scopes 含它的用户才能调用
	Mode:      lingo.ToolRead,             // 本期只调用只读工具
	Sensitive: []string{"customer.phone"}, // 结果与审计参数中替换为 ***
	Timeout:   5 * time.Second,            // 0 为默认 10 秒，最多 60 秒
	Call: func(ctx context.Context, u lingo.User, in json.RawMessage) (any, error) {
		var p struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(in, &p); err != nil {
			return nil, err
		}
		o, err := orders.Get(ctx, p.OrderID) // 响应 ctx 取消
		if err != nil {
			return nil, errors.New("order lookup failed") // 错误中不要带业务数据
		}
		if o.OwnerID != u.ID && !canViewAllOrders(u) { // 必须按传入用户二次鉴权
			return nil, errors.New("order not visible")
		}
		// 可选：声明数据的实际归属项目，负责该项目的 reviewer / admin 可在审计中看到这次调用
		return lingo.ToolOutput{Project: o.ProjectKey, Data: o}, nil
	},
}

leaveTool := lingo.Tool{
	Name: "get_leave_balance", Description: "查询本人的剩余年假与调休天数",
	InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	Scope: "leave:read", Mode: lingo.ToolRead,
	Call: func(ctx context.Context, u lingo.User, _ json.RawMessage) (any, error) {
		return leave.Balance(ctx, u.ID) // 只查本人
	},
}

l, err := lingo.New(lingo.Config{ /* … */ Tools: []lingo.Tool{orderTool, leaveTool}})
```

- **权限**：灵工只做第一道防线——`User.Scopes` 含 `Tool.Scope` 才可见、可调用（问答中模型也看不到无权限的工具）。**`Call` 必须按传入的 `lingo.User` 再次鉴权与过滤数据**，灵工不使用超级账号。无权调用、工具不存在、结果声明的归属项目调用人无权访问时，三者返回相同的 `ErrNotFound`（MCP 为 `unknown tool`）。
- **注册校验**：`Name` 只含字母、数字、下划线（不超过 64 个字符），`Mode` 为 `ToolRead`，`Scope` 非空，`InputSchema` 为 JSON 对象，`Call` 非空，`Timeout` 不为负且不超过 60 秒。不满足的工具**被忽略并记 Warn 日志**（`New` 不会因此失败）；与内置 MCP 工具或先注册的工具同名（不区分大小写）时忽略后者，不覆盖已有工具。升级后请检查启动日志中的 `Config.Tools entry ignored`。
- **参数**：只校验 `required` 与 `properties` 的基本类型；不合法时问答按参数的 `description` 追问，MCP / Go 返回参数错误（只含参数名与期望类型）。
- **结果**：按 `Sensitive` 路径（`phone`、`customer.phone`、`items.*.price`）脱敏，超过 8KB 截断（`truncated`）；**原始结果不写入会话、知识库、审计与日志**，回答正文照常作为会话消息保存。业务数据类回答被标为有帮助时不进入知识库候选。
- **超时、并发与熔断**：默认 10 秒；超时后仍未返回的 `Call` 继续占用并发名额（每个工具 8 个、合计 64 个），名额满时返回 `ErrToolUnavailable`；同一工具 5 分钟内连续失败 5 次暂停 1 分钟（只在当前实例内）；暂停前已在途、之后才返回的调用不会关闭或重开熔断。时限到达后才取到的结果按超时丢弃。结果编码或脱敏失败按失败计入，记录不含错误原文。
- **审计**：每次调用先写一条待完成记录再执行（写不进就不调用），记录人员、工具、入口、参数摘要（只含 `InputSchema` 声明的参数：参数名与截断、脱敏后的值；拒绝记录只有参数名；未声明的字段不记录）、状态与原因、耗时，不含结果与错误原文，保留 180 天。`ListDataRequests`（REST `GET /data-requests`）：每个人都能看到本人的记录；reviewer / admin 另可看到归属项目在其 `User.Projects` 中**明确列出**的记录——`User.Projects` 为空**不**授予跨用户审计，未声明归属的记录只有本人可见。
- **合规提醒**：业务数据会发给你配置的模型组织回答，请按数据合规要求选择模型服务，并用 `Sensitive` 去掉不应外发的字段。工具结果被当作数据而不是指令，但仍应避免让工具返回不可信的长文本。
- 错误：`ErrToolUnavailable`（REST 503 `tool_unavailable`）、`ErrToolFailed`（超时、`Call` 出错或 panic；REST 502 `tool_failed`），都不含错误原文。
- 需要迁移 `0007_data_requests.sql`；即使没有注册工具，`Run` 也会把租约过期的待完成记录标为中断、清理 180 天前的记录。只读，`write` 类工具与转工单时附带业务数据留待后续版本。
- **升级后的行为变化**：
  1. 以前写在 `Config.Tools` 中、从未被调用的工具，升级后会被问答、MCP 与 `CallTool` **真实调用**（产生真实的业务查询与审计记录），前提是提问人的 `User.Scopes` 含其 `Scope`。
  2. 不满足注册校验的工具（含 `write` 类、`Scope` 为空、`Timeout` 超过 60 秒）**被忽略并记 Warn 日志**，不再保留在配置中生效；`New` 照常成功。请检查启动日志中的 `Config.Tools entry ignored`。

### 连接配置（存数据库）

模型与 Gitea 的连接配置（地址、令牌 / API Key、Webhook 密钥、自动注册 Webhook、拒绝重定向）可以不写在代码里，存入数据库，由有权限的人在接入方的配置页里修改，**不用重启**：

- `ConnectionSettings` / `UpdateConnectionSettings` / `TestConnectionSettings`（REST `GET` / `PUT /settings/connections`、`POST /settings/connections/test`），不提供 MCP。权限由 `Config.AuthorizeSettings` 决定，不配置时只允许 admin。
- **优先级**：代码中 `Config` 给了值的项优先，查看时来源为 `config`，修改返回 409；没给的读数据库。模型按槽位判断（`Config.Model` / `LightModel` 设置了就整体以代码为准）；布尔项代码中为 `true` 才锁定。`LabelPrefix`、`ReplyMode` 只能在代码中配置。
- **生效**：保存后当前实例立即生效，其他实例 30 秒内生效（`Run` 定时检查，只挂载 Handler 的进程在收到请求时检查）；进行中的请求使用开始时的配置。
- **密钥明文存储**：数据库中的 `lingo_settings` 表保存明文密钥，能读取该表（含备份、只读副本）的人都能拿到密钥，请收紧数据库账号与备份的访问。接口只返回掩码（如 `sk-****abcd`），日志只记修改人与字段名。
- **改地址须重填密钥**：Gitea 或模型的地址换了主机，必须同时提交令牌 / API Key，避免把已保存的密钥发往新地址。换成另一个 Gitea 实例会使已有工单的 Issue 关联失效，队列中的任务与对账还可能操作到新实例上同编号的无关 Issue，不要这样做（只用于同一实例换域名或协议）。
- **未配置状态**：没有 Gitea 地址或令牌时也能启动：Webhook 返回 503，建单等任务暂不执行（不计失败），自检只报 `config` 警告；配置好后自动恢复。未配置期间 Gitea 推送失败的 Webhook 不会重投，由定时对账补齐；关闭对账（`ReconcileInterval < 0`）时补不回。
- **改 Webhook 密钥**：灵工立即按新密钥验签。开启自动注册时会补跑一次自检，把新密钥同步到 Gitea；未开启或 `SkipSelfCheck` 时需在 Gitea 中手动同步。
- 数据库里的模型只支持内置的 OpenAI 兼容接口（`openai`）与 DeepSeek（`deepseek`），不含向量模型；自定义的 `lingo.Model` 仍通过 `Config` 传入。需要执行迁移 0004。

### REST 接口

| 接口 | 说明 |
|---|---|
| `GET /projects`、`GET /ticket-types` | 项目与仓库；工单类型及 Issue 模板表单字段 |
| `POST /tickets`、`GET /tickets/mine`、`GET /tickets/{id}`、`POST /tickets/{id}/comments` | 提交、列表、详情（含评论与附件）、补充 |
| `GET /knowledge/search?q=&project=&limit=`；`GET` / `POST /knowledge`、`PUT` / `DELETE /knowledge/{id}`、`POST /knowledge/{id}/disable\|enable`、`POST /knowledge/disable-by-ticket`、`POST /knowledge/{id}/approve\|reject`、`GET /knowledge/unanswered` | 知识库：检索（所有角色）；维护与审核（reviewer / admin） |
| `GET /data-requests?tool=&user_id=&status=&since=&until=&before=&limit=` | 业务数据工具调用审计：本人的，以及 reviewer / admin 明确负责的项目内的 |
| `GET /tickets/visible?project=&status=&before=` | 可查看的工单：本人的 + `TicketAudience` 放宽可见的同事的（只读） |
| `POST` / `GET /tickets/{id}/perspectives` | 需求视角补充：追加、按视角分组查看（见上文「需求视角」） |
| `GET /triage`、`GET /triage/summary`、`POST /tickets/{id}/forward\|answer\|merge\|reject` | 后台确认（reviewer / admin） |
| `POST /tickets/{id}/reject-forwarded`、`GET /tickets/{id}/issue-close`、`POST /tickets/{id}/issue-close/retry` | 驳回已转研发工单（只关闭关联 Issue，异步可重试）及关闭进度 / 重试（reviewer / admin） |
| `GET /events?after=` | 变更流（与 `Notifier` 同源） |
| `GET /releases`、`GET /releases/{id}` | 版本记录、关联工单与上线记录 |
| `GET` / `PUT /settings/auto-forward` | 自动转出开关（修改需 admin） |
| `GET /usage?from=&to=` | 模型 token 用量，按天、用途、模型汇总，含前缀缓存命中率（admin） |
| `POST` / `GET /conversations`、`DELETE /conversations/{id}` | 知识问答会话：新建、列表（按最近活动，`before` 为上一页的 `cursor`）、删除 |
| `GET` / `POST /conversations/{id}/messages`、`POST /conversations/{id}/messages/{mid}/retry`、`POST /conversations/{id}/messages/{mid}/feedback` | 历史消息；提问（`stream` 为真时 SSE）；重试失败的回答；回答反馈 |
| `POST /conversations/{id}/actions/{aid}/confirm\|cancel` | 确认 / 取消待确认操作（如留言） |
| `GET` / `PUT /settings/connections`、`POST /settings/connections/test` | 连接配置（模型与 Gitea）：查看（密钥为掩码）、按字段修改、连通测试（权限见 `Config.AuthorizeSettings`） |

写操作须使用 `Content-Type: application/json`（防 CSRF）；错误响应为 `{"error":{"code","message"}}`。

### MCP 工具

`list_projects`、`list_ticket_types`、`create_ticket`、`get_ticket`、`list_my_tickets`、`list_visible_tickets`、`reply_ticket`、`add_perspective`、`list_perspectives`、`list_events`、`list_triage`、`forward_ticket`、`answer_ticket`、`merge_ticket`、`reject_ticket`、`reject_forwarded_ticket`、`get_issue_close`、`retry_issue_close`、`list_releases`、`get_release`、`get_llm_usage`、`ask`、`list_conversations`、`list_conversation_messages`、`confirm_action`、`cancel_action`、`rate_message`、`search_knowledge`；另列出调用者可调用的业务数据工具（`Config.Tools`，按 `User.Scopes` 过滤）。

### 事件

`ticket.created`、`ticket.status`、`ticket.comment`、`ticket.scheduled`、`ticket.released`、`ticket.deployed`、`triage.required`、`triage.overdue`、`triage.resolved`、`ticket.perspective_added`、`ticket.perspective_missing`。至少一次投递、同一工单按序，接入方按 `Event.ID` 去重。

### Gitea 配置

- 创建机器人账号与令牌，scope：`write:issue`、`read:repository`、`read:user`（开启 `AutoRegisterWebhook` 另需 `write:repository`）。
- 把 Webhook 指向 `WebhookHandler` 并设置共享密钥，或配置 `AutoRegisterWebhook` 与 `WebhookURL` 自动注册；灵工部署在内网时，需在 Gitea 的 `[security] ALLOWED_HOST_LIST` 中放行。
- 研发以 `/reply` 开头的评论（或 MCP 的 `reply_ticket`）回复提交人；PR 中写 `Fixes #N` 关闭 Issue。
- `l.SelfCheck(ctx)`（启动时也会执行）报告版本、权限、标签、Webhook、模板与工作流问题。
- 令牌只应发往 `BaseURL`：设置 `GiteaConfig.RejectRedirects: true` 后，所有 Gitea 请求都不跟随 HTTP 重定向（同主机、子域、跨域、https→http 降级一律拒绝），按不可重试的配置错误报告；默认关闭，行为不变。

## 目录

```
*.go                 公开 API（根包 lingo）
internal/            实现（见 docs/architecture/overview.md）
migrations/          数据库迁移（go:embed 打包）
examples/            接入示例
docs/                需求与架构文档
tests/               集成测试（独立 module，需要 PostgreSQL）
```

## 许可

[Apache License 2.0](LICENSE)
