# 灵问 Lingo

[English](README.md) | 简体中文

灵问是可嵌入业务项目的知识问答 Agent Go 库：用户随时提问，灵问基于知识库能答即答；
答不了的整理成结构化 Issue，交给 AI 研发团队在 Gitea 中处理，进展回传给接入方；每一次问答都回流知识库。

lingo 只提供公开 API，不含租户与前端，登录、用户管理、界面、部署由接入方自己处理。

> **状态**：早期阶段，公开 API 设计中，多数方法尚未实现。

## 功能

- **知识问答 Agent**：检索增强回答并附来源；答不了时追问关键信息，转为工单
- **反馈工单**：咨询 / Bug / 需求，支持文字、截图与附件；接口供接入方自建的弹窗组件调用
- **后台确认**：后台人员确认后再转出到 Gitea，或直接答复、合并、驳回；按项目与工单类型设自动 / 手动开关
- **Gitea 对接**：按仓库 Issue 表单模板建单、评论双向往来、识别 PR 合并、关联发布版本与上线信号；Webhook 验签去重并定时对账
- **进展通知**：事件投递给接入方 `Notifier`（至少一次，带事件 ID），另有查询方法与变更流
- **数据隔离**：工单、会话、附件按提交人 ID、角色与项目隔离；个人信息不外传 Gitea
- **MCP Server**：同样的能力以 MCP 工具提供给 AI 客户端与 AI 研发 Agent
- **业务数据工具**：注册 Go 函数，以提问人员自身的权限代其取数

## 安装

```sh
go get github.com/zhouhao4221/lingo
```

## 接入

```go
if err := lingo.Migrate(ctx, db); err != nil { ... } // 发布流程中执行一次

l, err := lingo.New(lingo.Config{
	DB:           db,       // *sql.DB（PostgreSQL，驱动自选）
	Model:        model,    // 实现 lingo.Model：Chat + Embed
	Gitea:        lingo.GiteaConfig{BaseURL: url, Token: token, WebhookSecret: secret},
	Projects:     projects, // 项目与所含 Gitea 仓库
	Authenticate: authn,    // 从请求解析 lingo.User
	Notifier:     notifier, // 可选：接收工单进展
	Tools:        tools,    // 可选：代用户取业务数据
})

mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler())) // REST JSON
mux.Handle("/mcp", l.MCPHandler())                             // MCP
mux.Handle("/webhooks/gitea", l.WebhookHandler())              // Gitea Webhook
go l.Run(ctx)                                                  // 后台任务
```

完整示例见 [examples/server](examples/server/main.go)。

环境要求：Go 1.24+、PostgreSQL、Gitea 1.22+（用 Gitea Actions 作为上线信号需 1.25+）。

## 目录

```
*.go                 公开 API（根包 lingo）
internal/            实现（见 docs/architecture/overview.md）
migrations/          数据库迁移（go:embed 打包）
examples/            接入示例
docs/                需求与架构文档
tests/               集成测试
```

## 许可证

[Apache License 2.0](LICENSE)
