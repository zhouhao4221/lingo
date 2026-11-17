# 产品需求文档 (PRD)

| 字段 | 内容 |
|-----|------|
| 产品名称 | 灵问 Lingo（连接 Agent AI 智能开发团队与用户的知识问答 Agent） |
| 文档版本 | v1.8 |
| 创建日期 | 2026-11-13 |
| 最后更新 | 2026-11-17 |
| 文档负责人 | haiqing |
| 状态 | 草稿 |

---

## 修订历史

| 版本 | 日期 | 修改人 | 修改内容 |
|-----|------|-------|---------|
| v1.0 | 2026-11-13 | haiqing | 初始版本（模板） |
| v1.1 | 2026-11-13 | haiqing | 按现有架构设计回填概述、用户、功能、数据、技术方案等章节 |
| v1.2 | 2026-11-13 | haiqing | 调整产品定位为 Agent 研发与用户的问答平台，重写背景与用户画像；并入数据库迁移约定 |
| v1.3 | 2026-11-13 | haiqing | 研发方明确为 Agent AI 智能开发团队；灵问定位为知识问答 Agent；新增 MCP、开放接口、固定角色多用户 |
| v1.4 | 2026-11-13 | haiqing | 新增项目与对接仓库：前后端多仓库归属一个项目，按项目授权与提问，Issue 落到项目内合适仓库；新增对接版本记录：同步 Gitea Release、工单关联修复版本 |
| v1.5 | 2026-11-13 | haiqing | 新增浏览器 JS 接入：嵌入问答组件、JS API、公开 Key 与域名白名单、身份令牌 |
| v1.6 | 2026-11-13 | haiqing | 新增基于人员权限的业务数据请求：数据接口登记、代用户调用、双重权限校验、审计 |
| v1.7 | 2026-11-13 | haiqing | 新增系统在线升级（可插拔执行器）；明确可扩展部署约定 |
| v1.8 | 2026-11-17 | haiqing | 改为 Go 库形态，去掉租户、前端与在线升级 |

---

## 1. 概述

### 1.1 背景与问题

**现状**：研发由 Agent AI 智能开发团队承担：AI Agent 在 Gitea 中领取 Issue、编写代码、提交 PR，人类工程师负责评审与把关。研发速度大幅提升，但用户侧的提问、反馈与需求仍靠群聊、私信、邮件人工传递。

**问题**：
- **用户与 AI 研发团队之间没有通道**：用户没有也不需要 Gitea 账号；AI Agent 只能处理结构化的 Issue，读不懂零散的聊天记录。
- **反馈质量决定 AI 研发效率**：描述不清、缺少复现信息的反馈，AI Agent 无法直接动手，只能反复追问或交由人工整理。
- **大量提问其实不需要研发介入**：用法、限制、已知问题等咨询占比高，却同样流向研发，挤占研发容量。
- **反馈闭环断裂**：Issue 处理完毕，用户不知道结果，需要有人手工转达。
- **知识没有沉淀**：问答散落在各处，同样的问题被反复提出、反复解答。

**机会**：做一个可嵌入业务项目的知识问答 Agent Go 库，站在用户与 AI 研发团队之间：
- 先由灵问基于知识库直接回答，能答的当场解决；
- 答不了的，整理成结构化、带上下文的 Issue 交给 AI 研发团队，处理进展自动回传用户；
- 业务项目 `import "github.com/zhouhao4221/lingo"` 即可嵌入，通过 Go 接口、HTTP Handler 与 MCP，让业务系统、用户侧的 AI 客户端和研发侧的 Agent 都能直接接入；
- 每一次问答与 Issue 处理都回流知识库，让灵问越用越聪明。

### 1.2 产品愿景

灵问是可嵌入业务项目的知识问答 Agent Go 库，连接 Agent AI 智能开发团队与用户：用户随时提问，灵问能答即答，答不了的转成 AI 研发团队能直接处理的任务，结果自动回到用户手中，每一次问答都让知识库更完整。

### 1.3 目标与成功指标

| 目标类型 | 指标 | 当前值 | 目标值 | 衡量方式 |
|---------|-----|-------|-------|---------|
| 业务目标 | 集成 lingo 的业务项目数 | 0 | 待定 | 待定 |
| 业务目标 | 问答直接解决率（无需转 Issue） | - | 待定 | 未转 Issue 且用户认可的会话 / 全部会话 |
| 业务目标 | 转出 Issue 被 AI Agent 直接领取处理的比例 | - | 待定 | 无需人工补充信息即开始处理的 Issue / 转出 Issue |
| 用户目标 | 问答首次响应时间 | - | P95 < 5s | 会话日志 |
| 用户目标 | 工单状态变更到提交人收到通知的延迟 | - | P95 < 1 分钟 | Webhook 接收时间 vs Notifier 投递时间 |
| 技术目标 | 通知投递成功率（含重试） | - | ≥ 99.9% | outbox 表状态统计 |
| 技术目标 | 工单与 Gitea 状态一致性 | - | 补偿后 100% | 同步补偿任务对账 |

### 1.4 范围定义

**包含（In Scope）**：
- 知识问答 Agent：基于知识库与 LLM 回答用户提问，答不了时引导转为工单
- 业务数据请求：以提问人员身份、基于其权限调用接入方注册的 Tool 函数查询数据或提交操作，用于回答与工单
- 公开 API：单一公开包（根包 `lingo`），提供 Go 方法、REST JSON Handler、MCP Handler 与 Gitea Webhook Handler
- 接入与配置：`lingo.Config` 声明数据库、模型、Gitea、项目、身份解析、通知与业务数据工具；迁移由接入方显式调用 `lingo.Migrate`
- 身份与固定角色：身份由接入方经 `Authenticate` 提供，固定角色 external / agent / reviewer / admin
- 项目与对接仓库：项目及其前端、后端等多个 Gitea 仓库在代码中声明；提问 / 提交时指定项目（可选仓库）
- 对接版本记录：同步 Gitea Release，版本说明入知识库，工单关联修复版本并通知提交人
- 工单提交：类型为咨询 / Bug / 需求，转出时自动整理为结构化 Issue（问题描述、复现信息、期望结果）
- Gitea 同步：机器人账号建 Issue、打类型标签；Webhook 接收 Issue 评论/状态/标签变更
- 信息反馈：工单 / 版本事件经 outbox 回调接入方 `Notifier`，失败重试
- 需求评审：内部人员评审（通过 / 驳回 / 需补充信息）与回复，结论回写 Gitea
- 知识库：结案工单与问答会话自动抽取问答对，供问答 Agent 检索

**不包含（Out of Scope）**：
- AI 研发 Agent 本身（领取 Issue、写代码、提 PR 由研发侧 Agent 完成，灵问只负责交付任务与回传结果）
- 替代 Gitea 的研发协作功能（看板、代码评审、CI 等）
- 支持 Gitea 以外的代码托管平台（GitHub、GitLab 等）
- 前端界面（原 Web 门户、管理后台、浏览器 JS 嵌入 SDK 均不提供），界面由接入方自行实现
- 用户登录与用户管理（lingo 不存用户表、不做登录、不发令牌）
- 多租户（不区分接入方，无租户隔离）
- 在线升级（升级即 `go get -u`）；部署方式由接入方自行决定
- 工单 SLA 计时与工时统计（后续版本再评估）
- 移动端原生 App

---

## 2. 用户研究

### 2.1 目标用户

| 用户角色 | 用户特征 | 使用场景 | 核心痛点 | 期望收益 |
|---------|---------|---------|---------|---------|
| 接入开发者 | 集成 lingo 的业务项目的开发者 | `import` lingo，配置 `lingo.Config`，挂载 Handler、运行 worker，实现 Model / Notifier / Tool | 自建反馈与问答通道成本高 | 以 Go 库方式嵌入，复用业务项目已有的登录、用户体系与界面 |
| 用户（external） | 使用研发团队交付的产品的人，无 Gitea 账号，不关心研发流程；经接入方界面或 MCP 使用 | 不会用、遇到 Bug、希望增加功能 | 不知道找谁问；问题说不清；提了之后石沉大海 | 随时提问、即时得到答复；答不了的有人跟进，进展主动告知 |
| AI 研发 Agent | Agent AI 智能开发团队中的 AI Agent，在 Gitea 领取并处理 Issue；经 MCP 使用 | 读取灵问转出的 Issue，开发修复后在 Issue 中回复；通过 MCP 查询知识库与问题上下文 | 需求描述模糊、缺少复现信息时无法直接动手 | 拿到结构化、可执行的任务；处理结果自动回传用户 |
| 研发负责人（reviewer） | 监督 AI 研发团队的人类工程师 / 产品；经接入方界面或 MCP 使用 | 评审需求、把关 AI 产出、处理 AI 无法解决的问题、审核知识库 | 需求来源杂，评审结论难留痕 | 统一评审入口，结论自动同步 Gitea 与用户 |

### 2.2 用户旅程

```mermaid
%%{init: {'theme':'neutral'}}%%
journey
  title 用户提问并获得答复
  section 提问
    在接入方界面/AI 助手(MCP)提问: 4: 用户
    灵问基于知识库直接回答: 5: 灵问
  section 转出
    答不了，确认转为工单: 3: 用户
    整理为结构化 Issue: 5: 灵问
  section 处理
    AI Agent 领取并处理: 4: AI 研发 Agent
    需求评审与把关: 3: 研发负责人
  section 反馈
    收到处理结果通知: 5: 用户
    问答回流知识库: 5: 灵问
```

### 2.3 竞品分析

| 竞品 | 优势 | 劣势 | 我们的差异化 |
|-----|-----|-----|-------------|
| 直接使用 Gitea Issue | 零开发，研发习惯 | 用户需账号，无问答、无反馈通知与知识库 | 用户免账号，先问答后转 Issue，自动反馈 |
| 通用工单系统（如 Zammad、Jira Service Management） | 功能完备 | 与 Gitea 割裂，工单不面向 AI Agent 消费 | 以 Gitea 为事实来源，Issue 为 AI 研发可执行格式 |
| 通用 AI 客服 / 知识库问答 | 即时回答 | 答不了就断了，不连接研发 | 问答与研发闭环，处理结果回流知识库 |
| 各业务系统自建反馈模块 | 贴合业务 | 重复建设，数据分散 | Go 库嵌入 + MCP 统一接入 |

---

## 3. 功能需求

### 3.1 功能概览

| 优先级 | 功能模块 | 功能描述 | 用户价值 | MVP |
|-------|---------|---------|---------|-----|
| P0 | 知识问答 Agent | 基于知识库与 LLM 回答提问，答不了时引导转工单 | 能答即答，减少研发打扰 | 是 |
| P1 | 业务数据请求 | 以人员身份、按其权限调用接入方注册的 Tool 函数，结果用于回答 | 直接回答“我的数据”类问题 | 否 |
| P0 | 公开 API（Go 接口 / HTTP Handler） | 根包 `lingo` 提供 Go 方法与 REST JSON Handler：提问、工单、评审、知识库、版本 | 业务项目以库方式嵌入 | 是 |
| P0 | MCP Server | `MCPHandler` 以 MCP 工具暴露提问、检索知识库、提交/查询工单，与 REST 对齐 | AI 客户端与研发 Agent 直接接入 | 是 |
| P0 | 接入与配置 | `lingo.Config` 声明依赖与扩展点；`lingo.Migrate` 显式迁移；挂载 Handler、运行 worker | 接入方按需组装，自行决定单体或拆分进程 | 是 |
| P0 | 项目与对接仓库 | 在代码中声明项目及其多个仓库（前后端等）；按项目提问，Issue 落到项目内合适的仓库 | 用户按产品提问，问题直达对应仓库 | 是 |
| P0 | 工单提交 | 咨询、Bug、需求；转出时整理为结构化 Issue | AI 研发可直接处理 | 是 |
| P0 | Gitea 同步 | 建 Issue + 类型标签；Webhook 回流评论、状态、标签变更 | 研发留在 Gitea 工作 | 是 |
| P0 | 信息反馈（Notifier） | 工单 / 版本事件经 outbox 回调接入方 `Notifier`，失败重试 | 提交人及时获知进度，渠道由接入方决定 | 是 |
| P1 | 需求评审 | 内部人员评审需求类工单并回复，结论回写 Gitea | 评审有入口、有记录 | 是 |
| P0 | 身份与固定角色 | 预置固定角色，身份由接入方经 `Authenticate` 提供并映射角色；按角色控制权限 | 复用接入方用户体系，权限清晰 | 是 |
| P0 | 知识库 | 结案工单与问答会话抽取问答对，供问答 Agent 检索 | 问答 Agent 的知识来源 | 是 |
| P1 | 对接版本记录 | 同步 Gitea Release，版本说明入知识库，工单关联修复版本并通知 | 用户知道问题在哪个版本解决 | 否 |

### 3.2 功能详情

#### 3.2.0 知识问答 Agent

**功能描述**：灵问本身是一个知识问答 Agent。用户从任一入口提问，灵问检索知识库并结合 LLM 生成回答；无法回答或用户不满意时，引导用户补充信息并转为工单。

**用户故事**：
> 作为用户，我希望提问后立刻得到答复；答不了时，系统帮我把问题整理清楚交给研发，而不是让我重新描述一遍。

**功能流程**：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  A[用户提问] --> B[检索知识库]
  B --> C{能回答?}
  C -- 是 --> D[生成回答并附来源]
  D --> E{用户满意?}
  E -- 是 --> F[会话结束 / 回流知识库]
  E -- 否 --> G[追问补充信息]
  C -- 否 --> G
  G --> H[整理为结构化工单]
  H --> I[创建 Gitea Issue]
```

**业务规则**：
- 回答需附知识来源（知识条目 / 历史工单），无可靠依据时不编造，直接说明并引导转工单
- 转工单前由灵问追问关键信息（Bug：复现步骤、期望与实际结果、版本；需求：场景与期望），整理成 AI 研发 Agent 可执行的 Issue 模板
- 转工单需用户确认，会话记录随工单附带
- 模型服务由接入方经 `Config.Model`（`lingo.Model` 接口：Chat + Embed）提供；提示词是否可配置：待定

**验收标准**：
- [ ] 知识库已有答案的问题，灵问直接回答并给出来源
- [ ] 知识库无答案时不编造，引导转为工单
- [ ] 转出的 Issue 包含结构化的问题描述、复现信息与原始会话链接

#### 3.2.0.1 公开 API：Go 接口、HTTP Handler 与 MCP

**功能描述**：灵问只提供公开 Go API：单一公开包（根包 `lingo`），其余实现位于 `internal/`。`lingo.New` 返回的实例 `l` 提供以下接入形式，能力一致：
- **Go 方法**：`Ask`、`CreateTicket`、`GetTicket`、`ReplyTicket`、`Review`、`SearchKnowledge`、`ListReleases` 等，接入方可不走 HTTP 直接调用
- **REST JSON 接口**：`l.Handler()`，提供提问、工单、评审、知识库、版本能力
- **MCP Server**：`l.MCPHandler()`，Streamable HTTP，以 MCP 工具形式暴露能力，与 REST 对齐，供用户侧 AI 客户端（IDE、桌面助手等）和研发侧 AI Agent 直接调用
- **Gitea Webhook**：`l.WebhookHandler()`，验签 + 去重
- **后台 worker**：`l.Run(ctx)`，负责 outbox 投递、Gitea 同步补偿、知识库生成

**MCP 工具（草案）**：

| 工具 | 说明 | 主要使用方 |
|-----|-----|----------|
| `list_projects` | 列出当前身份可对接的项目及其仓库 | 两侧 |
| `ask` | 向灵问提问（指定 `project`，可选 `repo`），返回回答与来源 | 用户侧 AI 客户端 |
| `search_knowledge` | 检索知识库条目（按 `project` / `repo` 过滤） | 两侧 |
| `create_ticket` | 提交咨询 / Bug / 需求工单（指定 `project`，可选 `repo`） | 用户侧 AI 客户端 |
| `get_ticket` | 查询工单状态、回复、关联 Issue 与修复版本 | 两侧 |
| `reply_ticket` | 在工单中回复用户（同步为 Issue 评论） | 研发侧 AI Agent |

**业务规则**：
- Handler 内的路径为相对路径，挂载前缀由接入方决定（见 3.2.1）
- REST 与 MCP 的请求身份均由接入方提供的 `Authenticate` 解析（见 3.2.0.2）；Go 方法如何传入身份：待确认
- MCP 传输方式：Streamable HTTP（是否同时支持 stdio：待定）
- 研发侧工具仅对 `reviewer` / `agent` 角色开放
- Go 方法签名、REST 路径与请求格式：待设计

**验收标准**：
- [ ] 通过 Go 方法可完成提问、检索、提交与查询工单
- [ ] 通过 REST 接口与 MCP 客户端可完成同样操作
- [ ] 未授权身份无法调用研发侧工具

#### 3.2.0.2 身份与固定角色（由接入方提供）

**功能描述**：lingo 不存用户表、不做登录、不发令牌。接入方通过 `Config.Authenticate`（`func(r *http.Request) (lingo.User, error)`）解析请求身份，返回 `lingo.User{ID, Name, Email, Role, Scopes}`：`ID` 为接入方自己的用户 ID，`Role` 由接入方把自身用户映射为下列固定角色之一，可选 `Projects` 限制可访问的项目（见 3.2.0.3）。系统预置一组固定角色，不支持自定义角色或细粒度权限配置。

**固定角色**：

| 角色 | 标识 | 权限 |
|-----|-----|-----|
| 用户 | `external` | 提问、提交工单、查看本人会话与工单 |
| AI 研发 Agent | `agent` | 通过 MCP / REST 接口读取工单、回复用户、检索知识库 |
| 研发负责人 | `reviewer` | 在用户权限之外：查看全部工单、评审需求、回复用户、审核知识库 |
| 管理员 | `admin` | 全部权限 |

**用户故事**：
> 作为接入开发者，我希望沿用业务项目已有的登录与用户体系，只把当前用户映射为灵问的固定角色，不必维护第二套账号。

**业务规则**：
- 角色集合固定，权限随角色内置，不提供角色编辑
- 登录、用户管理、令牌签发由接入方负责；每个 AI Agent 的身份如何区分由接入方在 `Authenticate` 中处理
- 同一角色的多个用户权限相同，操作记录按 `User.ID` 留痕

**验收标准**：
- [ ] `Authenticate` 返回不同角色时，只能执行该角色权限内的操作
- [ ] 工单回复、评审等操作记录到具体 `User.ID`

#### 3.2.0.3 项目与对接仓库（代码声明）

**功能描述**：灵问以**项目**为对接单位。一个项目可包含多个 Gitea 仓库（如前端、后端、移动端）。项目与仓库由接入方在 `Config.Projects`（`[]lingo.Project`）中声明，`New` 时同步入库；提问或提交工单时指定项目（可进一步指定仓库），问答在整个项目范围内进行，转出的 Issue 落到项目下合适的仓库。

**用户故事**：
> 作为用户，我只关心“哪个产品出了问题”，不想分辨是前端还是后端；我希望选择项目后提问，由灵问判断问题该交给哪个仓库。
>
> 作为接入开发者，我希望在代码中把一个产品的前后端仓库声明到同一个项目下，并设置项目内的默认仓库。

**层级关系**：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  C[Config.Projects] -->|New 时同步入库| P[项目]
  P --> R1[前端仓库]
  P --> R2[后端仓库]
  P --> R3[其他仓库]
```

**指定方式**：

| 入口 | 方式 |
|-----|-----|
| Go 方法 / REST 接口 | 参数 `project`（项目标识），可选 `repo`（`owner/name`，须属于该项目） |
| MCP | 工具参数 `project`、可选 `repo`；可先调用 `list_projects` 获取可选项目及其仓库 |

**业务规则**：
- 项目在代码中声明（标识、名称、说明及其仓库）；仓库归属一个项目，并标注类型（前端 / 后端 / 移动端 / 文档 / 其他）；一个仓库只属于一个项目；`lingo.Project` 具体字段：待确认
- 每个项目设一个默认仓库；机器人账号需对项目下所有仓库有 Issue 读写权限
- 项目授权（谁能访问哪个项目）由接入方在 `Authenticate` / 业务中处理；`User.Projects` 为可选限制，设置后只能指定其中的项目
- 未指定 `project` 时取 `Config.Projects` 中第一个项目（须在 `User.Projects` 范围内，否则报错要求指定）
- 问答范围：知识库检索覆盖项目下所有仓库的知识与版本说明，回答时注明来源仓库
- Issue 落库仓库的确定顺序：用户指定的 `repo` → 问答 Agent 按问题内容判断（如界面 / 交互类 → 前端仓库，接口 / 数据类 → 后端仓库），转出前请用户确认 → 项目默认仓库
- 跨仓库问题（前后端都要改）：在主要仓库创建 Issue，并在其他相关仓库创建关联 Issue 互相引用（是否自动创建：待定）
- 工单创建后仓库调整由研发负责人操作（是否支持跨仓库迁移 Issue：待定）

**验收标准**：
- [ ] 在 `Config.Projects` 中声明项目、多个仓库与默认仓库后，`New` 时同步入库
- [ ] 设置 `User.Projects` 时，指定其外的项目被拒绝
- [ ] 指定项目提问时，回答可引用该项目下任一仓库的知识，并注明来源仓库
- [ ] 未指定仓库时，问答 Agent 给出建议仓库，用户确认后 Issue 创建在该仓库
- [ ] `list_projects` 仅返回当前身份可对接的项目及其仓库

#### 3.2.0.4 对接版本记录

**功能描述**：灵问同步各对接仓库在 Gitea 中的版本记录（Release / Tag 及版本说明），用于三件事：
- **问答**：版本说明入知识库，回答“最新版本有什么变化”“这个问题在哪个版本修复了”“某功能从哪个版本开始支持”
- **关联**：解析版本说明中的 Issue 引用（如 `closes #12`、`#12`、`REQ-001`），把工单与修复 / 上线版本关联
- **通知**：包含某工单的版本发布后，通知提交人“已在 vX.Y.Z 发布”

**用户故事**：
> 作为用户，我希望知道我反馈的问题在哪个版本修复、我现在用的版本有哪些已知问题，以便决定是否升级。
>
> 作为 AI 研发 Agent，我希望发版后关联的工单自动更新并通知用户，不需要逐个回复。

**功能流程**：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  A[Gitea 发布 Release] --> B[Webhook / 定时同步]
  B --> C[保存版本记录]
  C --> D[版本说明入知识库]
  C --> E[解析 Issue 引用]
  E --> F[工单关联修复版本]
  F --> G[经 Notifier 通知提交人]
```

**业务规则**：
- 来源：Gitea Release（标签、标题、说明、发布时间、是否预发布）；通过 `release` Webhook 实时接收，并定时全量同步兜底
- 同步项目下各仓库的版本；草稿（draft）不同步，预发布（prerelease）同步但单独标记，是否通知：待定
- 关联规则：版本说明中引用的 Issue 编号 → 对应工单标记“修复版本”；同一工单出现在多个版本时取最早的正式版本
- 提交 Bug 时可填写当前使用版本（Go 方法 / HTTP 接口 / MCP 参数 `version`），问答 Agent 据此判断问题是否已在更新版本修复，并提示升级
- 版本记录按仓库保存、按项目汇总：按项目查询时按时间合并前后端各仓库的发布；问答时引用所选项目下的版本记录并注明仓库
- 项目是否需要统一的产品版本号（映射到各仓库的版本组合）：待定
- 版本格式遵循 semver（`vX.Y.Z`）；非 semver 标签按发布时间排序

**MCP 工具（新增）**：

| 工具 | 说明 |
|-----|-----|
| `list_releases` | 列出指定项目（或其下某仓库）的版本记录（可分页、可过滤预发布） |
| `get_release` | 查看某版本的说明与关联工单 |

**验收标准**：
- [ ] Gitea 发布 Release 后，灵问在 1 分钟内同步该版本记录
- [ ] 版本说明中引用的 Issue 对应的工单显示修复版本，提交人收到发布通知
- [ ] 用户问“某问题在哪个版本修复”时，问答 Agent 能给出版本号并附版本说明来源
- [ ] 提交 Bug 并填写旧版本号，若该问题已在新版本修复，问答 Agent 提示升级

#### 3.2.0.5 基于人员权限的业务数据请求（Tool 函数）

**功能描述**：问答过程中，灵问可以**以提问人员的身份**调用接入方注册的业务数据工具（`lingo.Tool`，Go 函数），查询数据或提交业务操作，把结果用于回答或附到工单中。是否能取到数据、能取到哪些数据，由该人员的权限决定，灵问不持有超越用户本人的权限。

**用户故事**：
> 作为用户，我问“我昨天提交的订单 1024 为什么审批失败”，希望灵问直接查到这张订单的状态和失败原因来回答，而不是让我截图。
>
> 作为接入方，我希望灵问只能看到当前用户本人有权看到的数据，并且每一次数据请求都有记录可查。

**人员信息**：

| 来源 | 内容 |
|-----|-----|
| `Authenticate` 返回的 `lingo.User` | `ID`（接入方用户 ID）、`Name`、`Email`、`Role` |
| `User.Scopes` | 用户可用的数据权限，如 `order:read`、`order:read:dept`、`leave:apply` |

> 人员信息与权限由接入方提供，灵问只校验和使用，不自行修改。

**工具注册**：接入方在 `Config.Tools`（`[]lingo.Tool`）中注册可供灵问调用的业务数据工具：

| 字段 | 说明 |
|-----|-----|
| `Name` / `Description` | 供问答 Agent 理解何时调用，如“查询订单详情” |
| `InputSchema` | 入参定义（JSON Schema），如 `order_id` |
| `Scope` | 调用前用户必须具备的 scope，如 `order:read` |
| `Mode` | `read`（查询） / `write`（操作，如提交申请、重试任务） |
| `Sensitive` | 返回结果中需脱敏或不得展示的字段（`[]string`） |
| `Call` | `func(ctx, lingo.User, json.RawMessage) (any, error)`，业务侧实现 |

**功能流程**：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  A[用户提问] --> B[问答 Agent 判断需要业务数据]
  B --> C{User.Scopes 含<br/>Tool.Scope?}
  C -- 否 --> D[告知无权限 / 建议转工单]
  C -- 是 --> E{Mode 为 write?}
  E -- 是 --> F[展示操作内容，用户确认]
  E -- 否 --> G[调用 Tool.Call，传入当前 User]
  F --> G
  G --> H[业务侧在 Call 内二次校验并返回]
  H --> I[脱敏后用于回答 / 附到工单]
  G --> J[写入 data_requests 审计（摘要）]
```

**业务规则**：
- **代用户请求，双重校验**：灵问先校验 `User.Scopes` 含 `Tool.Scope`；`Call` 收到当前 `lingo.User`，业务侧必须在 `Call` 内按该用户的权限二次校验并做数据范围过滤。灵问不使用任何超级账号
- **只用于本次会话**：取到的数据只用于本次会话，不写入知识库，不用于回答其他用户
- **操作需确认**：`write` 类工具必须向用户展示将要提交的内容并经明确确认后才调用；防重复提交：调用方传入幂等键（idempotency key），同一用户 + 工具 + 幂等键只执行一次，记录于 `data_requests.idempotency_key`
- **转工单时的数据**：业务数据随工单转给 AI 研发 Agent 前需用户同意，并按 `Sensitive` 脱敏
- **审计**：每次数据请求写入 `data_requests`，只存摘要（人员、工具、参数摘要、结果状态、耗时）；研发负责人与管理员可查，用户可查看本人记录
- **失败处理**：`Call` 返回错误时如实告知用户，不编造数据；连续失败的工具自动熔断并告警（阈值：待定）
- `User.Scopes` 不含 `Tool.Scope` 时，不发起调用

**验收标准**：
- [ ] 接入方可在 `Config.Tools` 注册工具并配置 `Scope`、`Mode` 与 `Sensitive`
- [ ] 具备权限的用户提问时，灵问以其身份调用 `Call` 并基于返回数据回答
- [ ] 不具备权限的用户无法触发调用；`Call` 拒绝时灵问如实告知
- [ ] `write` 类工具未经用户确认不会被调用
- [ ] 每次请求均有审计记录；业务数据不进入知识库，转工单时按 `Sensitive` 脱敏

#### 3.2.1 接入与配置

**功能描述**：接入方在业务项目中 `import "github.com/zhouhao4221/lingo"`，通过 `lingo.New(lingo.Config{...})` 创建实例，显式执行迁移，并把 Handler 挂到自己的路由上。最小接入示例见 `examples/server/main.go`。

**接入示例（草案）**：

```go
l, err := lingo.New(lingo.Config{
    DB:           db,       // 接入方提供的 *sql.DB（PostgreSQL）
    Model:        myModel,  // lingo.Model：Chat + Embed
    Gitea:        lingo.GiteaConfig{BaseURL: ..., Token: ..., WebhookSecret: ...},
    Projects:     []lingo.Project{...},
    Authenticate: func(r *http.Request) (lingo.User, error) { ... },
    Notifier:     myNotifier,
    Tools:        []lingo.Tool{...},
})

lingo.Migrate(ctx, db) // 在发布流程中显式调用

mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler()))
mux.Handle("/mcp", l.MCPHandler())
mux.Handle("/webhooks/gitea", l.WebhookHandler())
go l.Run(ctx)
```

**Config 字段**：

| 字段 | 说明 |
|-----|-----|
| `DB` | 接入方提供的 `*sql.DB`（PostgreSQL），驱动由接入方选择，lingo 不引入数据库驱动依赖 |
| `Model` | `lingo.Model` 接口：Chat + Embed，接入方实现或用适配器 |
| `Gitea` | `lingo.GiteaConfig{BaseURL, Token, WebhookSecret}` |
| `Projects` | 项目与仓库声明，`New` 时同步入库（见 3.2.0.3） |
| `Authenticate` | 接入方解析请求身份，返回 `lingo.User`（见 3.2.0.2） |
| `Notifier` | `lingo.Notifier` 接口：工单 / 版本事件回调接入方（见 3.2.3） |
| `Tools` | 业务数据工具，代用户取数（见 3.2.0.5） |

**业务规则**：
- 迁移 SQL 经 `go:embed` 打包在库内，由接入方在发布流程中显式调用 `lingo.Migrate(ctx, db)`；`New` 时不自动执行迁移（见 5.4）
- 挂载路径由接入方决定；`l.Run(ctx)` 可与 Handler 分进程运行，接入方自行决定单体或拆分进程
- 登录、用户管理、界面、部署、升级由接入方处理；升级 lingo 即 `go get -u`

**验收标准**：
- [ ] 按 `examples/server/main.go` 完成 `New`、`Migrate`、挂载与 `Run` 后，可跑通提问、转工单与通知闭环
- [ ] `New` 不执行迁移；未调用 `Migrate` 时的行为：待确认

#### 3.2.2 工单提交与 Gitea 同步

**功能描述**：用户经 Go 方法、REST 接口或 MCP 提交工单（或由问答 Agent 转出），灵问以机器人账号在项目内确定的仓库（规则见 3.2.0.3）创建 Issue，打上类型标签并保存 `repo + issue number` 关联；Gitea 侧变更经 Webhook 回流。

**用户故事**：
> 作为用户，我希望不用登录 Gitea 就能提交问题并看到处理进度。

**功能流程**：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  A[Go 方法 / REST / MCP 提交] --> B[创建工单]
  B --> C[outbox: 创建 Gitea Issue]
  C --> D[保存 Issue 关联]
  E[Gitea Webhook] --> F[验签 + 去重]
  F --> G[更新工单 / 写 ticket_events]
  G --> H[写通知 outbox]
```

**业务规则**：
- 工单类型：`consult`（咨询）/ `bug` / `feature`（需求），对应 Gitea 标签
- REST / MCP 请求身份由 `Authenticate` 解析
- 对 Gitea 的写操作经 outbox + worker 异步执行，可重试；Issue 创建失败不影响工单入库
- Webhook 必须校验共享密钥签名（`GiteaConfig.WebhookSecret`），并按事件 ID 去重
- 定期同步补偿，修复漏收 Webhook 导致的状态不一致

**验收标准**：
- [ ] 提交工单后在指定仓库生成带类型标签的 Issue
- [ ] Gitea 中评论、关闭/重开、改标签后，工单状态与事件记录同步更新
- [ ] 签名错误的 Webhook 被拒绝；重复投递的事件只处理一次

**边界情况**：
| 场景 | 处理方式 |
|-----|---------|
| Gitea 不可用 | 工单正常创建，Issue 创建任务进入 outbox 重试 |
| Issue 在 Gitea 被删除 | 工单标记异常，待定是否通知提交人 |
| Webhook 乱序到达 | 以事件时间与 Issue 当前状态为准，待定具体策略 |

#### 3.2.3 信息反馈（Notifier）

**功能描述**：工单状态变化、评审结论、Issue 评论、修复版本发布时，灵问调用接入方实现的 `Notifier.Notify(ctx, lingo.Event)`；发邮件、站内信等渠道由接入方决定。

**业务规则**：
- 事件先写 `outbox`，再由 worker 投递给 `Notifier`，失败按策略重试并记录尝试次数
- 哪些事件触发通知（内部评论是否外发）：待定

**验收标准**：
- [ ] 工单状态变更后 `Notifier` 收到对应事件
- [ ] `Notify` 返回错误时按策略重试，超过上限标记失败

#### 3.2.4 需求评审

**功能描述**：需求类工单进入评审，内部人员经 `Review` 方法或 REST 接口给出结论（`approve` 通过 / `reject` 驳回 / `need_info` 需补充）并回复，结论同步为 Gitea 评论与标签。

**用户故事**：
> 作为评审人，我希望集中查看待评审需求并给出结论，结论自动同步到 Gitea 和提交人。

**验收标准**：
- [ ] 仅 reviewer / admin 角色可评审
- [ ] 评审结论在 Gitea Issue 中生成评论并更新标签
- [ ] `need_info` 时提交人收到补充信息通知

#### 3.2.5 知识库

**功能描述**：知识库是问答 Agent 的知识来源。工单结案后，worker 从工单正文、评论与评审记录抽取问答对；被用户认可的问答会话、各仓库的版本说明同样入库。研发负责人可经知识库接口审核、编辑条目。

**业务规则**：
- 知识库条目按项目（`project_id`）归属，可关联到仓库
- 自动抽取的条目可设为“待审核”，审核通过后才用于回答（是否强制审核：待定）
- 抽取与检索方式（LLM 抽取、向量检索）：待定；Embed 由 `Config.Model` 提供

**验收标准**：
- [ ] 结案工单自动生成问答条目并关联来源工单
- [ ] 新条目入库后，问答 Agent 能检索并引用

---

## 4. 非功能需求

### 4.1 性能需求

| 场景 | 指标 | 目标值 | 测量方法 |
|-----|-----|-------|---------|
| API 响应 | P95 延迟 | < 200ms（不含异步 Gitea 调用） | 服务端日志 |
| Webhook 处理 | 接收到工单更新 | < 1s | ticket_events 时间戳 |
| 并发能力 | QPS | 待定 | 压测 |

### 4.2 可用性需求

- 系统可用性：99.9%
- Gitea 故障时 lingo 可继续受理工单，恢复后由 outbox 补齐
- 故障恢复时间（RTO）：待定
- 数据恢复点（RPO）：待定

### 4.3 安全需求

| 类别 | 需求 | 优先级 |
|-----|-----|-------|
| 认证 | lingo 不做登录、不发令牌；请求身份由接入方 `Authenticate` 解析 | P0 |
| 授权 | 固定角色 external / agent / reviewer / admin；用户仅能访问本人会话与工单；可选 `User.Projects` 限制项目 | P0 |
| 模型安全 | 回答仅基于知识库；防提示词注入；不在回答中泄露其他用户数据 | P0 |
| 业务数据权限 | 代用户调用 Tool，灵问预校验 `User.Scopes` + 业务侧在 `Call` 内二次校验；业务数据不入知识库；`write` 需确认；审计只存摘要 | P0 |
| 数据安全 | Gitea token 与 Webhook 密钥经 `Config.Gitea` 传入 | P0 |
| 集成安全 | Webhook 验签 + 按事件去重 | P0 |
| 审计日志 | 工单事件全量记录于 ticket_events | P1 |

### 4.4 兼容性需求

- Go 版本：Go 1.24
- 数据库：PostgreSQL，驱动由接入方选择
- Gitea 版本：待定（需支持 Issue / Label / Webhook API）
- 部署环境：由接入方决定

---

## 5. 数据需求

### 5.1 数据模型

> 草案见 `migrations/0001_init.sql`；不含租户字段，用户 ID 均为接入方用户 ID（文本）

| 实体 | 描述 | 关键字段 |
|-----|-----|---------|
| projects | 项目，`New` 时由 `Config.Projects` 同步 | key, name, description, default_repo_id, enabled |
| repos | Gitea 仓库，归属一个项目 | project_id, owner, name, kind(frontend/backend/mobile/docs/other), enabled |
| conversations | 问答会话 | user_id(接入方用户 ID，文本), project_id, channel(api/mcp/go), status, ticket_id |
| messages | 会话消息 | conversation_id, role(user/assistant), content, sources |
| tickets | 工单 | user_id(接入方用户 ID，文本), 提交人姓名/邮箱快照, project_id, repo_id, type(consult/bug/feature), title, body, status, gitea_issue_number, reported_version, fixed_release_id |
| releases | 版本记录 | repo_id, tag, name, body, prerelease, published_at, gitea_release_id |
| release_tickets | 版本关联工单 | release_id, ticket_id, ref(引用原文) |
| data_requests | 业务数据请求审计（只存摘要） | user_id, conversation_id, tool(Tool.Name), params_digest, status, latency_ms, idempotency_key |
| ticket_events | 工单事件 | ticket_id, source, kind, payload |
| reviews | 需求评审 | ticket_id, reviewer_id(接入方用户 ID), verdict(approve/reject/need_info), comment |
| outbox | 异步任务 outbox（原 notifications）：Gitea 写操作、Notifier 投递 | 字段待确认（至少含 payload, status, attempts） |
| kb_entries | 知识库问答 | project_id, repo_id(可空), question, answer, source_ticket_id, tags, embedding |

### 5.2 数据采集

| 数据点 | 采集时机 | 用途 | 保留期限 |
|-------|---------|-----|---------|
| 工单事件 | 接口调用 / Webhook 到达 | 审计、状态追溯 | 待定 |
| 通知投递记录 | 每次投递尝试 | 排障、成功率统计 | 待定 |
| 问答会话 | 每次提问与回答 | 评估直接解决率、回流知识库 | 待定 |
| 用户反馈（满意/不满意） | 回答后 | 评估回答质量 | 待定 |

### 5.3 数据报表

| 报表名称 | 维度 | 指标 | 更新频率 |
|---------|-----|-----|---------|
| 工单概览 | 项目、类型、状态 | 工单数、平均处理时长 | 待定 |
| 通知投递 | 事件类型 | 成功率、重试次数 | 待定 |

### 5.4 数据库迁移约定

- 迁移 SQL 目录：`migrations/`，数据库 PostgreSQL；迁移文件经 `go:embed` 打包在库内
- 执行：由接入方在发布流程中显式调用 `lingo.Migrate(ctx, db)`，`lingo.New` 时不自动执行
- 文件命名：`NNNN_<snake_case_name>.sql`，编号四位递增（现有 `0001_init.sql`），新文件取当前最大编号 + 1
- 已发布的迁移文件不得修改，结构变更一律新增文件
- 迁移须向前兼容，保证滚动发布期间新旧版本可同时运行；每个迁移需可在已有数据上执行（加列给默认值或允许 NULL，避免锁大表的操作）
- 业务表不含租户字段，时间列用 `timestamptz`
- 回滚：`/rd:release` 会为本版本迁移生成回滚 SQL，编写时保证每条变更可逆

---

## 6. 技术方案

### 6.1 系统架构

> 详见 [docs/prompt/architecture.md](../prompt/architecture.md) 与 [docs/architecture/overview.md](../architecture/overview.md)

lingo 以 Go 库形式内嵌在宿主应用（接入方业务项目）中运行：

```mermaid
%%{init: {'theme':'neutral'}}%%
flowchart LR
  subgraph Host[宿主应用（接入方业务项目）]
    Mux[接入方路由 mux] -->|/lingo/| H[l.Handler REST]
    Mux -->|/mcp| M[l.MCPHandler]
    Mux -->|/webhooks/gitea| W[l.WebhookHandler]
    Biz[业务代码] -->|Go 方法| L[lingo 实例]
    H --> L
    M --> L
    W --> L
    R[l.Run worker] --> L
    L --> X[Authenticate / Model / Notifier / Tools<br/>接入方实现]
  end
  L --> DB[(PostgreSQL<br/>接入方 *sql.DB)]
  R -->|outbox 投递 / 同步补偿| G[Gitea]
  G -->|Webhook| W
```

**库**：
- 语言/框架：Go 1.24，标准库 `net/http`；单一公开包（根包 `lingo`），其余实现在 `internal/`
- 数据库：PostgreSQL，经接入方提供的 `*sql.DB` 访问，lingo 不引入数据库驱动依赖
- 缓存：暂无
- 消息队列：不引入，使用数据库 outbox + 后台 worker（`l.Run`）
- 问答 Agent：LLM + 知识库检索（RAG），模型经 `lingo.Model` 接口由接入方提供
- MCP Server：`l.MCPHandler()`，复用 `Authenticate` 与领域服务，工具与 REST 对齐

**部署**：
- 由接入方决定：单体（Handler 与 `l.Run` 同进程）或拆分进程（`l.Run` 单独运行）
- 可扩展约定：无状态；Worker 多实例安全（`SKIP LOCKED` + advisory lock）；迁移向前兼容且不在 `New` 时自动执行（详见 [architecture.md](../prompt/architecture.md#可扩展部署约定)）
- 监控告警：待定
- 日志系统：待定

### 6.2 第三方依赖

| 服务类型 | 服务商 | 用途 | 备选方案 |
|---------|-------|-----|---------|
| 代码托管 / Issue | Gitea（自建） | 工单的事实来源，机器人账号读写 | - |
| 通知渠道 | 接入方 | 经 `Notifier` 实现邮件、站内信等 | - |
| 大模型服务 | 接入方选择 | 问答生成、追问、Issue 整理、知识抽取；经 `lingo.Model`（Chat + Embed）接入 | 接入方实现或用适配器 |
| 向量检索 | 待定（如 PostgreSQL pgvector） | 知识库语义检索 | 关键词检索 |
| MCP | MCP 协议（Streamable HTTP） | 对 AI 客户端与研发 Agent 暴露工具 | - |

### 6.3 接口设计

> Handler 内路径均为相对路径，挂载前缀由接入方决定；下表 HTTP 路径为示例挂载点

| 接口 | 方法 | 描述 |
|-----|-----|-----|
| `lingo.New(cfg)` | Go | 创建实例，同步 `Config.Projects` 入库 |
| `lingo.Migrate(ctx, db)` | Go | 执行库内 `go:embed` 的迁移 |
| `l.Ask` / `l.CreateTicket` / `l.GetTicket` / `l.ReplyTicket` / `l.Review` / `l.SearchKnowledge` / `l.ListReleases` 等 | Go | 不走 HTTP 直接调用，签名待设计 |
| `l.Run(ctx)` | Go | 后台 worker：outbox 投递、Gitea 同步补偿、知识库生成 |
| `l.Handler()`（示例挂载 `/lingo/`） | - | REST JSON：提问、工单、评审、知识库、版本，路径待设计 |
| `l.MCPHandler()`（示例挂载 `/mcp`） | POST/GET | MCP Server（Streamable HTTP），工具见 3.2.0.1、3.2.0.4；提问 / 工单接口支持 `project`、`repo`、`version` 参数 |
| `l.WebhookHandler()`（示例挂载 `/webhooks/gitea`） | POST | Gitea Webhook 接收，验签 + 去重 |
| 健康检查 | - | 不由 lingo 提供，由接入方在宿主应用中实现（可用 `DB.PingContext` 判断数据库就绪） |

---

## 7. 发布计划

### 7.1 版本规划

> lingo 以 Go module semver tag（`vX.Y.Z`）发布，接入方通过 `go get -u` 升级

| 版本 | 目标 | 核心功能 | 预计时间 |
|-----|-----|---------|---------|
| MVP | 跑通“问答 → 转 Issue → 回传”闭环 | 知识问答 Agent、知识库、公开 API（Go 接口 / HTTP Handler）、MCP Server、身份与固定角色、项目与对接仓库、工单与 Gitea 双向同步、Notifier 通知、`examples/server` 最小接入示例 | 待定 |
| v1.0 | 正式上线 | 需求评审、同步补偿、知识库审核、对接版本记录、业务数据请求 | 待定 |
| v1.x | 持续迭代 | 报表、回答质量评估、更多 MCP 工具 | 待定 |

### 7.2 里程碑

| 里程碑 | 计划时间 | 实际时间 | 状态 | 备注 |
|-------|---------|---------|-----|-----|
| 项目骨架搭建 | 2026-11-13 | 2026-11-13 | 已完成 | 目录、包职责、迁移草案 |
| 需求评审通过 | | | 待开始 | |
| 技术方案确定 | | | 待开始 | |
| 开发完成 | | | 待开始 | |
| 测试通过 | | | 待开始 | |
| 上线发布 | | | 待开始 | |

### 7.3 灰度策略

先在 1 个内部业务项目中集成试运行，验证同步与通知闭环后再逐步推广到其他业务项目。

---

## 8. 风险管理

### 8.1 风险识别

| 风险 | 类型 | 可能性 | 影响 | 应对措施 | 负责人 |
|-----|-----|-------|-----|---------|-------|
| Webhook 丢失或乱序导致状态不一致 | 技术 | 中 | 高 | 事件去重 + 定期同步补偿 | |
| Gitea API 限流或不可用 | 技术 | 低 | 中 | outbox 异步重试 | |
| 机器人 token 泄露 | 技术 | 低 | 高 | 由接入方经 `Config.Gitea` 传入，不入代码仓库，最小权限，定期轮换 | |
| 内部评论误外发给用户 | 业务 | 中 | 中 | 明确外发规则（待定） | |
| 知识库抽取质量差 | 技术 | 中 | 低 | 人工审核或可编辑条目 | |

### 8.2 依赖项

| 依赖项 | 类型 | 状态 | 风险 |
|-------|-----|-----|-----|
| Gitea 实例与机器人账号 | 内部 | 待确认 | 权限范围需覆盖所有项目下的仓库 |
| PostgreSQL（接入方提供 `*sql.DB`） | 内部 | 待确认 | - |
| 模型/向量服务（接入方实现 `lingo.Model`） | 内部/外部 | 待确认 | 仅影响知识库 |

### 8.3 假设与约束

**假设**：
- 研发团队以 Gitea 为日常工作平台，愿意在 Issue 中回复
- 接入方能提供稳定的用户标识（`User.ID`）

**约束**：
- Gitea 为工单事实来源，lingo 不承载研发协作
- 不引入独立消息队列，异步依赖数据库 outbox
- lingo 不引入数据库驱动依赖；迁移不在 `New` 时自动执行

---

## 9. 需求追踪

> 本章节由 `/rd:new` 和 `/rd:done` 命令自动维护，记录所有从本 PRD 派生的需求。

| 编号 | 标题 | 模块 | 状态 | 创建日期 | 完成日期 |
|-----|------|------|------|---------|---------|
| - | 暂无需求 | - | - | - | - |

---

## 10. 附录

### 10.1 术语表

| 术语 | 定义 |
|-----|-----|
| 接入方 | 以 Go 库方式集成 lingo 的业务项目 |
| Agent AI 智能开发团队 | 由 AI Agent 承担开发、人类工程师评审把关的研发团队 |
| 知识问答 Agent | 灵问自身：基于知识库回答提问，答不了时转为工单 |
| MCP | Model Context Protocol，AI 客户端调用外部工具的开放协议 |
| 工单（ticket） | 灵问中的咨询 / Bug / 需求记录，与一个 Gitea Issue 关联 |
| 机器人账号 | 灵问用于调用 Gitea API 的专用账号 |
| outbox | 先写库再由 worker 异步投递的任务表，保证可重试 |
| 同步补偿 | 定期比对 Gitea 与灵问的工单状态，修复不一致 |
| Notifier | 接入方实现的 `lingo.Notifier` 接口，接收工单 / 版本事件回调 |
| Tool | 接入方注册的业务数据工具（Go 函数），代用户取数 |

### 10.2 参考资料

- Gitea API 文档：https://docs.gitea.com/api
- Gitea Webhook 文档：https://docs.gitea.com/usage/webhooks

### 10.3 相关文档

| 文档 | 链接 |
|-----|-----|
| 代码仓库 | `https://github.com/zhouhao4221/lingo.git` |
| 最小接入示例 | `examples/server/main.go` |
| 架构概览 | [docs/architecture/overview.md](../architecture/overview.md) |
| 项目架构（开发规范） | [docs/prompt/architecture.md](../prompt/architecture.md) |
| 数据库草案 | [migrations/0001_init.sql](../../migrations/0001_init.sql) |
| 原型设计 | 待补充 |
| 测试用例 | 待补充 |

---

## 审批记录

| 角色 | 姓名 | 意见 | 日期 |
|-----|-----|-----|-----|
| 产品负责人 | | | |
| 技术负责人 | | | |
| 业务负责人 | | | |

---

> 下一步：完善 PRD 后，可使用 `/rd:new <标题>` 创建具体需求
