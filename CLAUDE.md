# 灵问 Lingo

连接 Agent AI 智能开发团队与用户的知识问答 Agent：基于知识库回答用户提问，答不了的转为结构化 Gitea Issue 交给 AI 研发团队，进展自动回传用户，问答回流知识库。接入方式：Web 门户、浏览器 JS 嵌入、开放接口（OpenAPI）、MCP Server；固定角色（external / agent / reviewer / admin）多用户；按项目对接（一个项目含前端、后端等多个仓库），同步各仓库的 Gitea 版本记录；可基于人员权限代用户向业务系统请求数据；支持后台在线升级（执行器可插拔），部署遵循 architecture.md 的可扩展部署约定。

## 文档

- 代码仓库：`https://git.kanhushi.com/smartlab/lingo.git`（Gitea，`smartlab/lingo`）

- 产品需求：[docs/requirements/PRD.md](docs/requirements/PRD.md)
- 项目架构与开发规范：[docs/prompt/architecture.md](docs/prompt/architecture.md)
- 业务流与模块：[docs/architecture/overview.md](docs/architecture/overview.md)
- 各流程的项目规范（代码生成、测试、评审、发版等）：`docs/prompt/`
- 需求文档：`docs/requirements/`（active / completed / modules），由 DevFlow `/rd:*` 命令管理

## 常用命令

```sh
go run ./cmd/server                  # 后端，默认 :8080（PORTAL_ADDR 覆盖）
go test ./...                        # 后端测试
cd web && pnpm install && pnpm dev   # 前端开发，/api 代理到 :8080
cd web && pnpm build                 # 前端构建验证
```

本地配置复制 `portal.example.json` 为 `portal.json`（已 gitignore）。

## 关键约定

- 依赖方向：httpapi → domain ← store；领域包不依赖 `store` / `httpapi`
- 对 Gitea 的写操作经 outbox + worker 异步、可重试；Webhook 必须验签并按事件去重
- 业务表带 `tenant_id`；迁移文件 `migrations/NNNN_name.sql`，编号递增，已发布的不得修改（详见 PRD 5.4）
- 每个包保留 `doc.go` 说明职责
- 密钥与本地配置不入库（`portal.json`、`.env`、`*.local`）
