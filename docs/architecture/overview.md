# 架构概览

灵问是可嵌入业务项目的知识问答 Agent Go 库，连接用户与 Agent AI 智能开发团队。研发由 AI Agent 在 Gitea 中领取 Issue 完成，
人类研发负责人评审把关；灵问站在用户与研发之间，能答即答，答不了的转为 AI 研发可直接处理的 Issue。

lingo 运行在接入方（宿主应用）进程内，不含租户与前端；身份、界面、部署与升级由接入方负责。

## 公开 API

| 入口 | 使用方 | 鉴权 |
| --- | --- | --- |
| Go 方法（`l.Ask`、`l.CreateTicket` 等） | 宿主应用代码 | 调用方传入 `lingo.User` |
| `l.Handler()` REST JSON，挂载前缀由接入方决定 | 宿主应用前端、其他系统 | `Config.Authenticate` |
| `l.MCPHandler()` MCP Streamable HTTP | 用户侧 AI 客户端、研发侧 AI Agent | `Config.Authenticate` |
| `l.WebhookHandler()` | Gitea | 共享密钥签名 |
| `l.Run(ctx)` | 宿主进程 | — |
| `lingo.Migrate(ctx, db)` | 宿主发布流程 | — |

接入方提供的扩展点：`*sql.DB`（PostgreSQL）、`Model`（对话 + 向量）、`Authenticate`（请求 → `User`）、
`Notifier`（进展回传）、`Tools`（业务数据工具）、`Projects`（项目与仓库声明）。

灵问以**项目**为对接单位：一个项目包含多个 Gitea 仓库（前端、后端等），在 `Config.Projects` 中声明，`New` 时同步入库。
提问与提交工单时通过 `project`（可选 `repo`）指定；问答检索覆盖项目下所有仓库，
转出 Issue 的仓库按“用户指定 → 问答 Agent 按内容判断并确认 → 项目默认仓库”确定。
`User.Projects` 非空时限制可访问的项目。

各入口复用同一套领域服务与固定角色鉴权（external / agent / reviewer / admin，由接入方映射）。

## 业务流

1. **提问**：用户经宿主应用（Go 方法或 REST）或 MCP 向灵问提问（指定项目，可选仓库），创建问答会话。
2. **问答**：问答 Agent 检索知识库，结合 `Model` 生成回答并附来源；无可靠依据时不编造。
   涉及用户本人业务数据时，调用接入方注册的 `Tool`：灵问预校验 `User.Scopes`，`Tool.Call` 内业务侧二次校验，
   结果仅用于本次会话，操作类工具需用户确认，全部写审计。
3. **转出**：答不了或用户不满意时，问答 Agent 追问关键信息，用户确认后整理为结构化工单
   （咨询 / Bug / 需求）；用户也可经接入方弹窗直接提交（文字 + 截图 / 附件）。追问槽位取自仓库的 Issue 表单模板。
   按项目与类型的开关自动转出，或进入待确认由后台人员转出 / 答复 / 合并 / 驳回；转出时经 outbox 由机器人账号在项目内确定的仓库创建 Issue，
   打作用域标签并保存关联。
4. **处理**：AI 研发 Agent 在 Gitea 领取 Issue 处理，可通过 MCP 查询知识库与工单上下文、回复用户；
   需求类工单由研发负责人评审，结论同步为 Gitea 评论与标签。
5. **回流**：Issue 被评论、指派、改标签、排期、关闭 / 重开，或修复 PR 合并时 Webhook 推送，验签、去重后更新工单并写 outbox；
   Gitea 不重投失败的 Webhook，由 Worker 按游标定时对账补齐。用户补充信息经机器人评论回写 Issue。
6. **反馈**：Worker 把事件投递给接入方 `Notifier`，失败重试；邮件、站内信等由接入方实现。
7. **版本**：Gitea 发布 Release 时 Webhook 推送（并定时同步兜底），保存版本记录，按上一 tag 到本 tag 的提交匹配
   工单的修复提交（版本说明中的 Issue 引用补漏），关联修复版本并通知提交人，版本说明同时入知识库。
   部署工作流成功、提交状态成功或接入方调用 `MarkDeployed` 后，工单标记已上线并通知。Gitea 接口细节见
   [gitea-integration.md](gitea-integration.md)。
8. **沉淀**：工单结案或会话被用户认可后，Worker 抽取问答对写入知识库（可设审核），供后续问答检索。

## 模块

| 目录 | 职责 |
| --- | --- |
| 根包 `lingo` | 公开 API：`Config`、扩展点接口、`New`、Handler、`Run`、`Migrate` |
| `internal/assistant` | 知识问答 Agent：会话、检索增强回答、追问、转工单 |
| `internal/llm` | 经 `lingo.Model` 的对话与向量调用封装 |
| `internal/mcp` | MCP Server：工具定义（list_projects、ask、search_knowledge、create_ticket、get_ticket、reply_ticket、add_perspective、list_perspectives、list_releases、get_release） |
| `internal/project` | 项目与仓库：同步声明、仓库归属与类型、默认仓库、Issue 落库仓库判定 |
| `internal/auth` | 调用 `Authenticate`，按固定角色与项目范围鉴权 |
| `internal/ticket` | 工单领域与状态机 |
| `internal/review` | 需求评审与回复 |
| `internal/gitea` | Gitea API 客户端 |
| `internal/webhook` | Gitea Webhook 接收与事件转换（Issue、Release） |
| `internal/notify` | 进展事件写 outbox，投递给 `Notifier` |
| `internal/release` | 版本记录：同步 Gitea Release、解析 Issue 引用、工单关联修复版本 |
| `internal/connector` | 业务数据工具执行：scope 预校验、写操作确认、脱敏、审计、熔断 |
| `internal/knowledge` | 知识库问答抽取、审核与检索 |
| `internal/selfcheck` | 启动自检：Gitea 版本、仓库与权限、标签补齐、Webhook 注册与测试投递、模板与部署工作流 |
| `internal/worker` | 消费 Webhook 投递并同步工单、按工单顺序投递通知 outbox；同步补偿、知识库生成 |
| `internal/store` | 基于 `*sql.DB` 的仓储实现 |
| `internal/httpapi` | REST 路由与处理器 |
| `migrations/` | 迁移 SQL，go:embed 打包 |
| `examples/server` | 最小接入示例 |
