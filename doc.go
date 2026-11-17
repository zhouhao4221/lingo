// Package lingo 是可嵌入业务项目的知识问答 Agent 库。
//
// 灵问基于知识库回答用户提问，答不了的整理为结构化 Gitea Issue 交给 AI 研发团队，
// 进展经 Notifier 回传接入方，问答回流知识库。lingo 只提供公开 API：
// Go 方法、REST JSON Handler、MCP Handler 与 Gitea Webhook Handler；
// 登录、用户管理、界面、部署与升级由接入方自行处理。
//
//	l, err := lingo.New(lingo.Config{DB: db, Model: model, Gitea: gitea, Projects: projects,
//		Authenticate: authn, Notifier: notifier, Tools: tools})
//	mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler()))
//	mux.Handle("/mcp", l.MCPHandler())
//	mux.Handle("/webhooks/gitea", l.WebhookHandler())
//	go l.Run(ctx)
//
// 数据库迁移不会在 New 时自动执行，接入方在发布流程中调用 Migrate。
package lingo
