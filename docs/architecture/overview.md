# 架构概览

灵问是连接 Agent AI 智能开发团队与用户的知识问答 Agent。研发由 AI Agent 在 Gitea 中领取 Issue 完成，
人类研发负责人评审把关；灵问站在用户与研发之间，能答即答，答不了的转为 AI 研发可直接处理的 Issue。

## 接入方式

| 入口 | 使用方 | 鉴权 |
| --- | --- | --- |
| Web 门户 `/api` | 用户、研发负责人、管理员 | 门户登录 |
| 开放接口 `/openapi` | 业务系统 | AppKey 签名 |
| 浏览器 JS `/sdk/lingo.js` + `/sdk/api` | 接入方网页中的用户 | 公开 Key + 域名白名单 + 接入方签发的身份令牌（可匿名） |
| MCP Server `/mcp` | 用户侧 AI 客户端、研发侧 AI Agent | 接入方凭据 / 用户令牌 / 服务账号令牌 |
| Gitea Webhook `/webhooks/gitea` | Gitea | 共享密钥签名 |

灵问以**项目**为对接单位：一个项目包含多个 Gitea 仓库（前端、后端等），接入方按项目授权。
提问与提交工单时通过 `project`（可选 `repo`）指定，未指定时用接入方默认项目；问答检索覆盖项目下所有仓库，
转出 Issue 的仓库按“用户指定 → 问答 Agent 按内容判断并确认 → 项目默认仓库”确定。

三类入口复用同一套领域服务与固定角色鉴权（external / agent / reviewer / admin，每个角色可有多个用户）。

## 业务流

1. **提问**：用户经门户、开放接口或 MCP 向灵问提问（指定项目，可选仓库），创建问答会话。
2. **问答**：问答 Agent 检索知识库，结合大模型生成回答并附来源；无可靠依据时不编造。
   涉及用户本人业务数据时，以该人员身份、基于其权限调用接入方登记的业务数据接口（灵问预校验 scope，
   业务系统二次校验），结果仅用于本次会话，操作类请求需用户确认，全部写审计。
3. **转出**：答不了或用户不满意时，问答 Agent 追问关键信息，用户确认后整理为结构化工单
   （咨询 / Bug / 需求），由机器人账号在项目内确定的仓库创建 Issue，打类型标签并保存关联。
4. **处理**：AI 研发 Agent 在 Gitea 领取 Issue 处理，可通过 MCP 查询知识库与工单上下文、回复用户；
   需求类工单由研发负责人评审，结论同步为 Gitea 评论与标签。
5. **回流**：Issue 被评论、改状态、改标签时 Webhook 推送到门户，验签、去重后更新工单并写通知 outbox。
6. **反馈**：Worker 投递通知（接入方回调、邮件、站内信），失败重试。
7. **版本**：Gitea 发布 Release 时 Webhook 推送（并定时同步兜底），门户保存版本记录，解析说明中的
   Issue 引用把工单关联到修复版本并通知提交人，版本说明同时入知识库。
8. **沉淀**：工单结案或会话被用户认可后，Worker 抽取问答对写入知识库（可设审核），供后续问答检索。

## 模块

| 目录 | 职责 |
| --- | --- |
| `internal/assistant` | 知识问答 Agent：会话、检索增强回答、追问、转工单 |
| `internal/llm` | 大模型与向量服务客户端（可配置模型） |
| `internal/mcp` | MCP Server：工具定义（list_projects、ask、search_knowledge、create_ticket、get_ticket、reply_ticket、list_releases、get_release） |
| `internal/tenant` | 接入方注册、凭据、回调地址、项目授权 |
| `internal/project` | 项目与仓库：登记、仓库归属与类型、默认仓库、Issue 落库仓库判定 |
| `internal/user` | 固定角色多用户、服务账号、Gitea 账号映射 |
| `internal/auth` | 门户登录、AppKey 签名、令牌校验、角色鉴权 |
| `internal/ticket` | 工单领域与状态机 |
| `internal/review` | 需求评审与回复 |
| `internal/gitea` | Gitea API 客户端 |
| `internal/webhook` | Gitea Webhook 接收与事件转换（Issue、Release） |
| `internal/notify` | 信息反馈渠道 |
| `internal/release` | 版本记录：同步 Gitea Release、解析 Issue 引用、工单关联修复版本 |
| `internal/connector` | 业务数据接口：登记、权限预校验、代用户签名调用、脱敏、审计、熔断 |
| `internal/upgrade` | 系统在线升级：检查 lingo 版本、校验产物、可插拔执行器（binary / pipeline）、审计 |
| `internal/knowledge` | 知识库问答抽取、审核与检索 |
| `internal/worker` | outbox 投递、同步补偿、知识库生成 |
| `internal/store` | 数据库与仓储 |
| `internal/httpapi` | HTTP 路由：门户 API、开放接口、管理后台、MCP 挂载 |
| `web/` | Vue 3 门户（问答界面）与管理后台；`web/sdk/` 浏览器嵌入组件与 JS SDK |
