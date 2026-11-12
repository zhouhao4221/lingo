# 灵问 Lingo

Gitea 外接工单门户：为多个内部系统提供统一的咨询 / Bug / 需求提交入口，同步为 Gitea Issue，更新后反馈提交人，支持需求评审，并把结案记录沉淀为知识库。

## 文档

- 产品需求：[docs/requirements/PRD.md](docs/requirements/PRD.md)
- 项目架构与开发规范：[docs/prompt/architecture.md](docs/prompt/architecture.md)
- 业务流与模块：[docs/architecture/overview.md](docs/architecture/overview.md)- 各流程的项目规范（代码生成、测试、评审、发版等）：`docs/prompt/`
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
