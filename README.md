# 灵问 Lingo

灵问是一个 Gitea 外接工单门户：为多个内部系统提供统一的咨询、Bug 上报与需求提交入口，
将其创建为 Gitea 工单（Issue），在工单更新后反馈给提交人，支持内部人员评审/回复需求，
并把处理记录沉淀为知识库问答。

## 目录

```
cmd/server/          服务入口
internal/            后端模块（见 docs/architecture/overview.md）
migrations/          数据库迁移
web/                 Vue 3 + Vite 前端
docs/                需求与架构文档
deploy/ scripts/     部署与脚本
tests/               集成测试
```

## 开发

```sh
go run ./cmd/server            # 后端，默认 :8080
cd web && pnpm install && pnpm dev   # 前端，/api 代理到 :8080
```
