# 灵工 Lingo

<p align="center"><img src="assets/logo/lingo-icon-color.svg" width="96" alt="灵工 Lingo logo"></p>

[English](README.md) | 简体中文

灵工是可嵌入业务项目的 Go 库，连接终端用户与在 Gitea 中工作的 AI 研发团队：用户在你的应用里提交咨询、Bug 与需求，灵工整理为结构化的 Gitea Issue，双向同步评论与状态，并把进展回传给你的应用，直到修复发布、上线。

灵工只提供公开 API（Go 方法、REST Handler、MCP Server、Gitea Webhook Handler），无租户、无前端：登录、用户管理、界面与部署由接入方负责。

> **状态：** 工单流程已端到端实现；知识问答（`Ask`、知识库）尚未实现。首个正式版本发布前 API 仍可能调整。

## 功能

- **反馈工单**：咨询 / Bug / 需求，支持文字、表单字段、截图与附件（灵工只存附件引用，文件在接入方自己的附件系统中，可经 `Config.FetchAttachment` 读取后上传到 Gitea，读取时会传入提交人 ID 供接入方校验归属）；接口按接入方自建的弹窗组件设计
- **后台确认**：转出到 Gitea、直接答复、合并或驳回；按项目与类型的自动 / 手动开关；超时提醒
- **工单整理**：可选的轻量模型生成清晰标题、从自由描述中抽取表单字段、整理描述（按模板逐项校验，并保留原始描述）
- **Gitea 对接**：按仓库 Issue 表单模板建单、评论双向同步、识别 PR 合并、按提交包含关系关联版本、上线信号（Actions 工作流、提交状态或 `MarkDeployed`）
- **可靠同步**：Webhook 验签、去重、异步处理；定时对账补齐漏掉的投递；启动自检与可选的 Webhook 自动注册
- **进展通知**：事件投递给你的 `Notifier`（至少一次，带事件 ID），并提供变更流；被合并工单的提交人随目标工单收到进展
- **数据隔离**：一切按调用者的用户 ID、角色与项目隔离；提交人身份不会写入 Gitea
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
	LightModel:   light,    // 可选：便宜、快速的 lingo.Model，建 Issue 前整理工单
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

`Authenticate` 把用户映射为四个固定角色之一：`external`（终端用户，只看本人工单）、`agent`（研发 Agent，看所属项目已转出的工单）、`reviewer`（后台确认，看所属项目全部工单）、`admin`。`User.Projects` 可选，用于限制可访问的项目。`User.Perspective` 可选，是与权限无关的需求视角，见下节。

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

### REST 接口

| 接口 | 说明 |
|---|---|
| `GET /projects`、`GET /ticket-types` | 项目与仓库；工单类型及 Issue 模板表单字段 |
| `POST /tickets`、`GET /tickets/mine`、`GET /tickets/{id}`、`POST /tickets/{id}/comments` | 提交、列表、详情（含评论与附件）、补充 |
| `POST` / `GET /tickets/{id}/perspectives` | 需求视角补充：追加、按视角分组查看（见上文「需求视角」） |
| `GET /triage`、`GET /triage/summary`、`POST /tickets/{id}/forward\|answer\|merge\|reject` | 后台确认（reviewer / admin） |
| `POST /tickets/{id}/reject-forwarded`、`GET /tickets/{id}/issue-close`、`POST /tickets/{id}/issue-close/retry` | 驳回已转研发工单（只关闭关联 Issue，异步可重试）及关闭进度 / 重试（reviewer / admin） |
| `GET /events?after=` | 变更流（与 `Notifier` 同源） |
| `GET /releases`、`GET /releases/{id}` | 版本记录、关联工单与上线记录 |
| `GET` / `PUT /settings/auto-forward` | 自动转出开关（修改需 admin） |
| `GET /usage?from=&to=` | 模型 token 用量，按天、用途、模型汇总，含前缀缓存命中率（admin） |

写操作须使用 `Content-Type: application/json`（防 CSRF）；错误响应为 `{"error":{"code","message"}}`。

### MCP 工具

`list_projects`、`list_ticket_types`、`create_ticket`、`get_ticket`、`list_my_tickets`、`reply_ticket`、`add_perspective`、`list_perspectives`、`list_events`、`list_triage`、`forward_ticket`、`answer_ticket`、`merge_ticket`、`reject_ticket`、`reject_forwarded_ticket`、`get_issue_close`、`retry_issue_close`、`list_releases`、`get_release`、`get_llm_usage`。

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
