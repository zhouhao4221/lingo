# 灵问 Lingo

可嵌入业务项目的知识问答 Agent Go 库：基于知识库回答用户提问，答不了的转为结构化 Gitea Issue 交给 AI 研发团队，进展经 `Notifier` 回传接入方，问答回流知识库。只提供公开 API（根包 `lingo` 的 Go 方法、REST `Handler`、`MCPHandler`、`WebhookHandler`、`Run`、`Migrate`）；无租户、无前端，登录、用户管理、界面、部署与升级由接入者自己处理。固定角色（external / agent / reviewer / admin）由接入方映射；按项目对接（一个项目含前端、后端等多个仓库），同步各仓库的 Gitea 版本记录；可通过接入方注册的 `Tool` 以人员权限代用户取数。

## 文档

- 代码仓库：`https://github.com/zhouhao4221/lingo.git`（GitHub 开源，module `github.com/zhouhao4221/lingo`）
- 产品需求：[docs/requirements/PRD.md](docs/requirements/PRD.md)
- 项目架构与开发规范：[docs/prompt/architecture.md](docs/prompt/architecture.md)
- 业务流与模块：[docs/architecture/overview.md](docs/architecture/overview.md)
- 各流程的项目规范（代码生成、测试、评审、发版等）：`docs/prompt/`
- 需求文档：`docs/requirements/`（active / completed / modules），由 DevFlow `/rd:*` 命令管理

## 常用命令

```sh
go build ./... && go vet ./...       # 构建与静态检查
go test ./...                        # 测试
go run ./examples/server             # 最小接入示例（需自行引入 PostgreSQL 驱动并实现 Model）
```

## 关键约定

- 公开 API 只在根包 `lingo`，实现放 `internal/`；根包对外类型与函数即兼容承诺，破坏性变更需升主版本
- 不引入数据库驱动、Web 框架或模型 SDK 依赖；数据库用接入方传入的 `*sql.DB`，模型走 `lingo.Model` 接口
- 依赖方向：httpapi → domain ← store；领域包不依赖 `store` / `httpapi`
- 对 Gitea 的写操作与 Notifier 回调经 outbox + worker 异步、可重试；Webhook 必须验签并按事件去重
- 无租户；迁移文件 `migrations/NNNN_name.sql` 经 go:embed 打包、由接入方调用 `lingo.Migrate` 执行，编号递增，已发布的不得修改（详见 PRD 5.4）
- 每个包保留 `doc.go` 说明职责
- 密钥与本地配置不入库（`.env`、`*.local`）
