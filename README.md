# 灵问 Lingo

灵问是连接 Agent AI 智能开发团队与用户的知识问答 Agent：用户随时提问，灵问基于知识库能答即答；
答不了的整理成结构化 Issue，交给 AI 研发团队在 Gitea 中处理，进展自动回传用户；
每一次问答都回流知识库。支持 Web 门户、浏览器 JS 嵌入、开放接口（OpenAPI）与 MCP 接入，固定角色多用户协作。

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
