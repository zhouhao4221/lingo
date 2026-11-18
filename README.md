# 灵问 Lingo

灵问是可嵌入业务项目的知识问答 Agent Go 库：用户随时提问，灵问基于知识库能答即答；
答不了的整理成结构化 Issue，交给 AI 研发团队在 Gitea 中处理，进展回传给接入方；每一次问答都回流知识库。

lingo 只提供公开 API，不含租户与前端，登录、用户管理、界面、部署由接入方自己处理。

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
