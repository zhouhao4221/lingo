# 项目架构

> 由 `/rd:init` 扫描生成（2026-11-13），项目处于骨架阶段，请随实现演进更新。业务流与模块说明详见 [`docs/architecture/overview.md`](../architecture/overview.md)。

## 技术栈

- 后端：Go 1.24，标准库 `net/http`（`http.ServeMux` 方法路由），入口 `cmd/server`
- 数据库：PostgreSQL（`portal.json` 的 `database_url`），迁移 SQL 位于 `migrations/`
- 外部集成：Gitea API（机器人账号 token）+ Gitea Webhook（共享密钥签名）
- 前端：Vue 3 + Vite 7（`web/`，pnpm），开发时 `/api` 代理到 `:8080`
- 配置：`portal.json`（示例 `portal.example.json`，本地文件已 gitignore），环境变量 `PORTAL_ADDR`
- API 风格：RESTful JSON；门户 API、接入系统 OpenAPI（AppKey 签名）、管理后台 API、`/webhooks/gitea`

## 目录结构

```
cmd/server/          服务入口（HTTP API + Webhook + 后台 Worker）
internal/
  config/            配置加载
  store/             数据库连接、事务、各领域仓储实现
  tenant/            接入系统：注册、凭据、回调地址、仓库映射
  user/              外部用户 / 内部人员、Gitea 账号映射、角色
  auth/              门户登录、AppKey 签名校验、角色鉴权
  ticket/            工单领域（咨询/Bug/需求）与状态机、Issue 关联
  review/            需求评审与回复，结论回写 Gitea
  gitea/             Gitea API 客户端
  webhook/           Webhook 接收：验签、去重、转工单事件
  notify/            通知渠道：回调、邮件、站内信
  knowledge/         知识库问答抽取与检索
  worker/            outbox 投递重试、同步补偿、知识库生成
  httpapi/           HTTP 路由与处理器
migrations/          数据库迁移 SQL（NNNN_name.sql）
web/src/             Vue 前端：views/ 页面、components/ 组件、api/ 请求封装
tests/               集成测试
deploy/ scripts/     部署与脚本
docs/                需求（requirements/）、架构（architecture/）、Prompt 库（prompt/）
```

## 分层架构

后端按领域分包，功能开发按以下顺序：

| 层 | 职责 | 目录 | 说明 |
|----|------|------|------|
| Migration | 表结构变更 | `migrations/` | 新增编号递增的 SQL 文件，约定见 [PRD 5.4](../requirements/PRD.md#54-数据库迁移约定) |
| Domain | 领域模型与规则 | `internal/<domain>/`（ticket、review、tenant、user…） | 结构体、状态机、业务校验，定义仓储接口 |
| Store | 数据访问 | `internal/store/` | 实现领域仓储接口，SQL、事务 |
| Integration | 外部系统 | `internal/gitea/`、`internal/notify/` | Gitea 客户端、通知渠道 |
| Async | 异步任务 | `internal/worker/`、`internal/webhook/` | outbox 投递、事件处理、补偿 |
| HTTP | 接口 | `internal/httpapi/`、`internal/auth/` | 路由、参数解析、鉴权、JSON 响应 |
| Web | 前端 | `web/src/` | api → components → views |

## 开发规范

- 包文档：每个包保留 `doc.go` 描述职责
- 领域包不依赖 `store`/`httpapi`，依赖方向：httpapi → domain ← store
- 与 Gitea 的写操作经 outbox/worker 异步并可重试；Webhook 必须验签并按事件去重
- 多租户：业务表带 `tenant_id`
- 密钥与本地配置不入库（`portal.json`、`.env`、`*.local`）

## 测试规范

- 单元测试：与源文件同目录 `*_test.go`，运行 `go test ./...`
- 集成测试：`tests/`（依赖 PostgreSQL / Gitea mock）
- 前端：暂无测试框架，构建验证 `cd web && pnpm build`
