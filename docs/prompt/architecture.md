# 项目架构

> 2026-11-17 改为 Go 库形态（无租户、无前端），请随实现演进更新。业务流与模块说明详见 [`docs/architecture/overview.md`](../architecture/overview.md)。

## 技术栈

- 形态：Go 1.24 库，module `github.com/zhouhao4221/lingo`，嵌入宿主应用进程运行；`examples/server` 为接入示例
- HTTP：标准库 `net/http`，对外暴露 `http.Handler`（REST、MCP、Webhook），路径为相对路径，挂载前缀由接入方决定
- 数据库：PostgreSQL，使用接入方传入的 `*sql.DB`（库内不引入驱动）；迁移 SQL 位于 `migrations/`，go:embed 打包，`lingo.Migrate` 执行
- 外部集成：Gitea API（机器人账号 token）+ Gitea Webhook（共享密钥签名）
- 扩展点（接入方实现）：`Model`（对话 + 向量）、`Authenticate`（请求 → `User`）、`Notifier`（进展回传）、`Tool`（业务数据）
- 配置：全部经 `lingo.Config` 在代码中传入，库不读配置文件与环境变量
- MCP：`MCPHandler`（Streamable HTTP），复用领域服务与鉴权

## 目录结构

```
*.go                 公开 API（根包 lingo）：Config、扩展点类型、New、Handler、MCPHandler、WebhookHandler、Run、Migrate
internal/
  store/             基于 *sql.DB 的仓储实现与事务
  assistant/         知识问答 Agent：会话、检索增强回答、追问、转工单
  llm/               经 lingo.Model 的对话与向量调用封装
  mcp/               MCP Server 工具定义
  project/           项目与仓库：同步 Config.Projects、默认仓库、落库仓库判定
  auth/              调用 Authenticate，固定角色与项目范围鉴权
  ticket/            工单领域（咨询/Bug/需求）与状态机、Issue 关联
  review/            需求评审与回复，结论回写 Gitea
  gitea/             Gitea API 客户端
  webhook/           Webhook 接收：验签、去重、转工单事件
  notify/            进展事件写 outbox，投递给 Notifier
  release/           版本记录：同步 Gitea Release、解析 Issue 引用、关联修复版本
  connector/         业务数据工具执行：scope 预校验、确认、脱敏与审计
  knowledge/         知识库问答抽取、审核与检索
  worker/            outbox 投递重试、同步补偿、知识库生成
  httpapi/           REST 路由与处理器
migrations/          数据库迁移 SQL（NNNN_name.sql，go:embed）
examples/server/     最小接入示例
tests/               集成测试
docs/                需求（requirements/）、架构（architecture/）、Prompt 库（prompt/）
```

## 分层架构

后端按领域分包，功能开发按以下顺序：

| 层 | 职责 | 目录 | 说明 |
|----|------|------|------|
| Migration | 表结构变更 | `migrations/` | 新增编号递增的 SQL 文件，约定见 [PRD 5.4](../requirements/PRD.md#54-数据库迁移约定) |
| Domain | 领域模型与规则 | `internal/<domain>/`（assistant、project、ticket、review、release、knowledge…） | 结构体、状态机、业务校验，定义仓储接口 |
| Store | 数据访问 | `internal/store/` | 实现领域仓储接口，SQL、事务 |
| Integration | 外部系统 | `internal/gitea/`、`internal/llm/`、`internal/notify/`、`internal/connector/` | Gitea 客户端、Model / Notifier / Tool 调用封装 |
| Async | 异步任务 | `internal/worker/`、`internal/webhook/` | outbox 投递、事件处理、补偿 |
| API | 接口 | 根包 `lingo`、`internal/httpapi/`、`internal/mcp/`、`internal/auth/` | 公开类型与方法、路由、MCP 工具、鉴权、JSON 响应 |

## 开发规范

- 包文档：每个包保留 `doc.go` 描述职责
- 公开 API 只在根包，根包只做装配与类型定义，业务逻辑放 `internal/` 领域包；对外类型与函数遵循 semver，破坏性变更升主版本
- 不引入数据库驱动、Web 框架、模型 SDK 等重依赖，优先标准库
- 领域包不依赖 `store`/`httpapi`，依赖方向：httpapi → domain ← store
- 与 Gitea 的写操作、Notifier 回调经 outbox/worker 异步并可重试；Webhook 必须验签并按事件去重
- 无租户：表不带 `tenant_id`；用户 ID 为接入方的用户 ID（文本），不建用户表
- 项目与仓库：会话、知识条目带 `project_id`，工单带 `project_id` + `repo_id`；收到的 `project` 须在 `User.Projects` 范围内（为空不限）、`repo` 须属于该项目，未指定时取默认仓库
- 角色固定为 external / agent / reviewer / admin，由接入方在 `Authenticate` 中映射；权限判断集中在 `auth`
- REST 与 MCP 能力对齐：新增对外能力时 Go 方法、REST、MCP 同步提供，Handler / MCP 工具只做参数适配
- 问答回答必须附知识来源，无可靠依据时不编造；模型调用经 `llm` 包，不在领域层直接调用 `Model`
- 业务数据请求必须以提问人员身份经 `Tool` 发起并校验 scope；业务数据不入知识库、不写日志原文，调用经 `connector` 统一审计
- 密钥与本地配置不入库（`.env`、`*.local`）

## 可扩展约定

> lingo 嵌入宿主进程，宿主可能单实例也可能多实例部署，库不做阻碍水平扩展的设计。

- **无状态**：会话、限流计数、锁等状态放 PostgreSQL，不放进程内存；附件由接入方的附件系统保存，灵工只存引用，不落本地磁盘
- **迁移单独执行**：`New` 不自动迁移，由宿主发布流程调用 `lingo.Migrate`（advisory lock 防并发）；迁移须向前兼容，保证新旧版本可同时运行
- **Handler 与 Run 可分离**：宿主可在同一进程同时挂载 Handler 与运行 `Run`，也可分进程分别扩容
- **Worker 多实例安全**：outbox 取任务用 `SELECT … FOR UPDATE SKIP LOCKED`；定时任务用数据库咨询锁（advisory lock）保证单实例执行
- **优雅退出**：`Run` 随 ctx 取消退出，处理完在途任务；HTTP 优雅关闭由宿主负责

## 测试规范

- 单元测试：与源文件同目录 `*_test.go`，运行 `go test ./...`
- 集成测试：`tests/`，独立 Go module（PostgreSQL 驱动 pgx 只作为这里的测试依赖，不进入主 module）；需设置 `LINGO_TEST_DATABASE_URL`，每个测试在临时 schema 中运行，未设置时跳过
  - 运行：`cd tests && LINGO_TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable go test ./...`
