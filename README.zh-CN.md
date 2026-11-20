# 灵问 Lingo

[English](README.md) | 简体中文

灵问是可嵌入业务项目的 Go 库，连接终端用户与在 Gitea 中工作的 AI 研发团队：用户在你的应用里提交咨询、Bug 与需求，灵问整理为结构化的 Gitea Issue，双向同步评论与状态，并把进展回传给你的应用，直到修复发布、上线。

灵问只提供公开 API（Go 方法、REST Handler、MCP Server、Gitea Webhook Handler），无租户、无前端：登录、用户管理、界面与部署由接入方负责。

> **状态：** 工单流程已端到端实现；知识问答（`Ask`、知识库）尚未实现。首个正式版本发布前 API 仍可能调整。

## 功能

- **反馈工单**：咨询 / Bug / 需求，支持文字、表单字段、截图与附件；接口按接入方自建的弹窗组件设计
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

完整示例见 [examples/server](examples/server/main.go)。

### 角色

`Authenticate` 把用户映射为四个固定角色之一：`external`（终端用户，只看本人工单）、`agent`（研发 Agent，看所属项目已转出的工单）、`reviewer`（后台确认，看所属项目全部工单）、`admin`。`User.Projects` 可选，用于限制可访问的项目。

### REST 接口

| 接口 | 说明 |
|---|---|
| `GET /projects`、`GET /ticket-types` | 项目与仓库；工单类型及 Issue 模板表单字段 |
| `POST /attachments`（multipart，字段 `file`）、`GET /attachments/{id}` | 上传与下载附件 |
| `POST /tickets`、`GET /tickets/mine`、`GET /tickets/{id}`、`POST /tickets/{id}/comments` | 提交、列表、详情（含评论与附件）、补充 |
| `GET /triage`、`GET /triage/summary`、`POST /tickets/{id}/forward\|answer\|merge\|reject` | 后台确认（reviewer / admin） |
| `GET /events?after=` | 变更流（与 `Notifier` 同源） |
| `GET /releases`、`GET /releases/{id}` | 版本记录、关联工单与上线记录 |
| `GET` / `PUT /settings/auto-forward` | 自动转出开关（修改需 admin） |

写操作须使用 `Content-Type: application/json`（防 CSRF）；错误响应为 `{"error":{"code","message"}}`。

### MCP 工具

`list_projects`、`list_ticket_types`、`create_ticket`、`get_ticket`、`list_my_tickets`、`reply_ticket`、`list_events`、`list_triage`、`forward_ticket`、`answer_ticket`、`merge_ticket`、`reject_ticket`、`list_releases`、`get_release`。

### 事件

`ticket.created`、`ticket.status`、`ticket.comment`、`ticket.scheduled`、`ticket.released`、`ticket.deployed`、`triage.required`、`triage.overdue`、`triage.resolved`。至少一次投递、同一工单按序，接入方按 `Event.ID` 去重。

### Gitea 配置

- 创建机器人账号与令牌，scope：`write:issue`、`read:repository`、`read:user`（开启 `AutoRegisterWebhook` 另需 `write:repository`）。
- 把 Webhook 指向 `WebhookHandler` 并设置共享密钥，或配置 `AutoRegisterWebhook` 与 `WebhookURL` 自动注册；灵问部署在内网时，需在 Gitea 的 `[security] ALLOWED_HOST_LIST` 中放行。
- 研发以 `/reply` 开头的评论（或 MCP 的 `reply_ticket`）回复提交人；PR 中写 `Fixes #N` 关闭 Issue。
- `l.SelfCheck(ctx)`（启动时也会执行）报告版本、权限、标签、Webhook、模板与工作流问题。

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
