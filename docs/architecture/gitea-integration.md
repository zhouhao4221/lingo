# Gitea 对接调研

> 调研日期：2026-11-18。依据：Gitea v1.27.3（最新稳定版）的 `templates/swagger/v1_json.tmpl`、`services/webhook/deliver.go`、`modules/webhook/type.go`、`modules/structs/hook.go`，并与 v1.22 / v1.24 / v1.25 及 main 分支对比版本差异。
> 目的：给出灵问各环节（模板提单 → 往来交流 → 开发完成 → 发版 → 上线）可用的 Gitea 接口、事件与可借力的特性，作为 `internal/gitea`、`internal/webhook`、`internal/release` 的设计输入。

## 1. 结论速览

| 环节 | 主要接口 / 事件 | 说明 |
|------|----------------|------|
| 初始化 | `GET /repos/{o}/{r}`、`GET/POST /repos/{o}/{r}/labels`、`POST /repos/{o}/{r}/hooks`（或 `/orgs/{org}/hooks`） | 校验仓库、补齐标签、自动注册 Webhook |
| 模板提单 | `GET /repos/{o}/{r}/issue_templates`、`GET /issue_config`、`POST /issues`、`POST /issues/{index}/assets` | 读取仓库自带的 Issue 表单模板，由 Agent 按字段收集信息后建 Issue |
| 往来交流 | `POST /issues/{index}/comments`、`issue_comment` 事件、`/reactions`、`/comments/{id}/assets` | 用户补充转为评论；研发回复经 Webhook 回传 |
| 状态流转 | `issues` / `issue_assign` / `issue_label` / `issue_milestone` 事件、`GET /issues/{index}/timeline` | 指派、标签、里程碑、关闭 / 重开 |
| 开发完成 | `pull_request` 事件、PR 正文 `Fixes #N` 关键词、`GET /commits/{sha}/pull`、timeline `pull_ref` | PR 合并自动关闭 Issue，定位修复 PR 与提交 |
| 发版 | `release` 事件（published/updated/deleted）、`GET /releases`、`GET /compare/{base}...{head}` | 用两个 tag 之间的提交定位已发布的 Issue，比解析版本说明可靠 |
| 上线 | `workflow_run` / `workflow_job` 事件（≥1.25）、`status` 事件、`GET /actions/runs` | Gitea 没有 Deployment API，用部署流水线结果或提交状态当作上线信号 |
| 对账补偿 | `GET /repos/{o}/{r}/issues?since=&state=all&labels=`、`GET /repos/{o}/{r}/issues/comments?since=` | Gitea 投递失败不会自动重试，需定时增量对账 |

**版本基线建议**：最低 **Gitea 1.22**（Issue / 评论 / 标签 / 模板 / 依赖 / timeline / Release / Webhook 全部可用）；上线信号用 Actions 时需 **≥1.25**（`actions/runs`、`actions/jobs` API 从 1.25 开始有，`workflow_run` / `workflow_job` 事件在 1.25 的事件表中）。项目看板 API（`/repos/{o}/{r}/projects`）**目前只在 main 分支，v1.27.3 也没有**，不要依赖。

## 2. 鉴权与调用约定

- **机器人 token**：请求头 `Authorization: token <PAT>`。最小 scope：`write:issue`（Issue、评论、标签、里程碑、附件）、`read:repository`（模板、提交、比较、Release、Actions），注册 Webhook 时还需 `write:repository`（组织级 Webhook 需 `write:organization`）；`read:user` 用于启动时 `GET /user` 拿到机器人 login。
- **机器人 login**：启动时 `GET /user` 缓存。用于识别 Webhook 回流里机器人自己发出的事件（`sender.login == bot`），避免评论回环。
- **分页**：`page` + `limit`，响应头带 `X-Total-Count` 和 `Link`；单页上限由实例 `[api] MAX_RESPONSE_ITEMS` 决定（默认 50）。
- **幂等**：`POST /issues` 不支持幂等键。在正文中嵌入隐藏标记 `<!-- lingo:ticket=<id> -->`，outbox 重试前先用 `GET /repos/issues/search?q=lingo:ticket=<id>` 或按标签列出 Issue 查重，避免重复建单。
- **错误处理**：404（仓库或 Issue 已删除）记为工单异常，不再重试；409/422 是参数问题，记录后告警；5xx / 超时由 outbox 退避重试。

## 3. 初始化：仓库、标签、Webhook

`New` 或 `Run` 启动时按 `Config.Projects` 对每个仓库执行：

1. `GET /repos/{o}/{r}`：校验仓库存在、机器人可访问，记录 `has_issues`、默认分支。
2. **标签补齐**：`GET /repos/{o}/{r}/labels`（组织仓库同时查 `GET /orgs/{org}/labels`），缺的用 `POST /repos/{o}/{r}/labels` 创建，并缓存 `name → id`（建 Issue 时 `labels` 字段传的是 **id**，不是名字）。
   - 建议用 Gitea **作用域标签**（scoped label，名称形如 `scope/name`，`exclusive: true` 时同一作用域互斥）：
     - `lingo`：灵问来源标记（对账筛选用）
     - `type/consult`、`type/bug`、`type/feature`（exclusive）
     - `review/pending`、`review/approved`、`review/rejected`、`review/need-info`（exclusive）
     - 可选 `status/in-progress`、`status/fixed` 等，供研发 Agent 显式标记进度
   - 标签前缀可配置，避免和仓库已有标签冲突。
3. **Webhook 自动注册（可选，默认关）**：`GET /repos/{o}/{r}/hooks` 查找 URL 相同的 hook，没有就 `POST`：
   ```json
   {
     "type": "gitea",
     "active": true,
     "config": { "url": "https://<接入方>/webhooks/gitea", "content_type": "json", "secret": "<WebhookSecret>" },
     "events": ["issues", "issue_comment", "issue_assign", "issue_label", "issue_milestone",
                "pull_request", "pull_request_comment", "release", "create", "workflow_run", "status"],
     "authorization_header": "Bearer <可选的额外令牌>"
   }
   ```
   同一个组织下的仓库多时，改用 `POST /orgs/{org}/hooks` 注册一次即可覆盖全部仓库（需要组织 owner 权限）。不允许机器人改 hook 的环境，由接入方手工配置，灵问只负责校验。

## 4. 模板提单

### 4.1 读取模板

- `GET /repos/{o}/{r}/issue_templates` 返回仓库 `.gitea/ISSUE_TEMPLATE/`（也兼容 `.github/ISSUE_TEMPLATE/`）下的全部模板，已经解析好：
  - 公共字段：`name`、`about`、`title`（标题前缀）、`labels`、`assignees`、`ref`、`file_name`
  - Markdown 模板：`content`
  - YAML 表单模板：`body[]`，每项 `IssueFormField{type, id, attributes, validations, visible}`，`type` 取 `markdown | textarea | input | dropdown | checkboxes`，`validations.required` 表示必填
- `GET /repos/{o}/{r}/issue_config` 返回 `blank_issues_enabled` 和 `contact_links`；`GET /issue_config/validate` 可在启动时校验模板配置是否合法。

### 4.2 推荐用法

1. 工单类型对应仓库里的模板（如 `bug` → `bug_report.yaml`），映射关系放在 `lingo.Repo` 配置里，没配就按模板 `labels` 自动匹配。
2. **问答 Agent 按表单字段追问**：把 `body[]` 中必填的 `input` / `textarea` / `dropdown` 当作要收集的槽位，和用户多轮对话把字段补齐；`dropdown` 的 `attributes.options` 直接作为候选项。研发方改了模板，灵问追问的内容会跟着变，不用改代码。
3. **按 Gitea 表单的渲染格式写正文**：Gitea 网页提交表单时会把每个字段渲染成 `### <label>\n\n<value>`。灵问按同样的格式拼正文，研发 Agent 看到的 Issue 和网页提交的一致。正文末尾追加：
   - 用户上下文（项目、版本 `reported_version`、环境），不放个人敏感信息
   - 隐藏标记 `<!-- lingo:ticket=<id> -->`
4. `POST /repos/{o}/{r}/issues`：`title`（模板标题前缀 + 摘要）、`body`、`labels`（label id 数组）、`assignees`（研发 Agent 账号，可按仓库配置）、`milestone`、`ref`、`due_date`（可做 SLA）。注意 `projects` 字段在 main 才有，稳定版不传。
5. 附件（截图、日志）：建 Issue 后 `POST /repos/{o}/{r}/issues/{index}/assets`（multipart，字段名 `attachment`），返回的 `browser_download_url` 用 Markdown 引用到正文或评论里。

## 5. 往来交流

| 方向 | 做法 |
|------|------|
| 用户 → 研发 | `POST /issues/{index}/comments`，正文前缀标明来源（如「来自提交人的补充」），附件走 `POST /issues/comments/{id}/assets` |
| 研发 → 用户 | Webhook `issue_comment`（`action`: created / edited / deleted）→ 过滤机器人自己的评论 → 判断是否对用户可见 → 写工单事件 → Notifier |
| 已读回执 | 机器人收到用户消息后可在评论上加 reaction（`POST /issues/comments/{id}/reactions`，如 `eyes`）；用户对回复的满意度也可回写为 `+1` / `-1` |
| 唤醒研发 Agent | 用户补充信息后，在评论中 @ 研发 Agent 账号，或给 Issue 加 `review/need-info` 之外的状态标签，触发 Agent 侧的通知 |

**哪些评论转给用户**：Gitea 没有“内部评论”。研发在 Issue 下的讨论很多是内部的，全部转给用户会泄露过程信息。建议：

- 默认只转发**明确面向用户**的回复：评论以 `/reply` 开头，或含 `<!-- lingo:public -->` 标记，或通过 MCP 的回复用户工具发出（MCP 是首选通道，灵问发评论时自动加标记）；
- 可配置为“非机器人评论全部转发”（适合用户和研发直接沟通的场景）；
- 状态类变化（关闭、发版、需补充信息）不依赖评论，由事件驱动通知。

**锁定**：工单结束后可 `PUT /issues/{index}/lock`，防止关单后再出现无人跟进的评论；重开时 `DELETE` 解锁。

## 6. 状态流转与开发完成

### 6.1 事件到工单状态的映射

Webhook 头 `X-Gitea-Event` 是大类（如 `issues`），`X-Gitea-Event-Type` 是细分类型（如 `issue_label`、`issue_assign`），按后者分发。

| Gitea 事件（Event-Type / action） | 工单含义 |
|---|---|
| `issues` / opened | 已转研发（灵问自己建单，用于确认 issue number） |
| `issue_assign` / assigned | 已有人领取（处理中） |
| `issue_label` / label_updated、label_cleared | 类型 / 评审 / 状态标签变化，同步评审结论 |
| `issue_milestone` / milestoned | 已排期，可告诉用户“计划在 v1.3 发布” |
| `pull_request` / opened（正文引用本 Issue） | 开发中，已有修复 PR |
| `pull_request` / closed 且 `merged=true` | 已修复，待发版 |
| `issues` / closed | 已关闭：有关联的合并 PR 记为“已修复”，否则按标签区分“不修复 / 重复 / 已答复” |
| `issues` / reopened | 重新处理，通知提交人 |
| `issues` / deleted | 工单异常（PRD 3.2.2 边界情况） |

### 6.2 识别“开发完成”

- **关闭关键词**：PR 正文或提交信息里写 `Fixes #12`、`Closes owner/repo#12`（跨仓库），合并后 Gitea 自动关闭对应 Issue。关键词由实例 `[repository.pull-request] CLOSE_KEYWORDS` 配置，默认是 close / closes / closed / fix / fixes / fixed / resolve / resolves / resolved。要求研发 Agent 在 PR 里按这个约定写。
- **找到修复 PR / 提交**：`GET /issues/{index}/timeline` 中 `type=pull_ref`（被 PR 引用）、`commit_ref`（被提交引用，带 `ref_commit_sha`）、`close`；`GET /repos/{o}/{r}/commits/{sha}/pull` 由提交反查合并它的 PR。
- **合并提交**：`GET /repos/{o}/{r}/pulls/{index}` 的 `merge_commit_sha`、`base`，保存到工单（`fixed_commit`），发版时用来判断是否包含在内。

## 7. 发版

### 7.1 接收

- `release` 事件：`action` 为 published / updated / deleted；payload 带完整 `Release`（`tag_name`、`name`、`body`、`prerelease`、`draft`、`target_commitish`、`published_at`、`assets`）。草稿（`draft=true`）忽略。
- 只打 tag 不建 Release 的仓库：订阅 `create` 事件（`ref_type=tag`），或定时 `GET /repos/{o}/{r}/tags`。
- 兜底同步：`GET /repos/{o}/{r}/releases?draft=false` 分页；`GET /releases/latest`、`GET /releases/tags/{tag}` 查单个版本。

### 7.2 关联工单（比解析版本说明可靠）

PRD 现在的做法是解析版本说明里的 `#N`，但版本说明是人写的，经常漏写。建议改成以提交为准，版本说明解析作为补充：

1. 找到上一个正式版本的 tag（同仓库、非 prerelease，按 `published_at` 排序）；
2. `GET /repos/{o}/{r}/compare/{prevTag}...{tag}` 拿到区间内的提交列表；
3. 对每个提交：命中工单保存的 `fixed_commit`，或 `GET /commits/{sha}/pull` 得到 PR 后匹配工单关联的 PR；
4. 命中的工单记 `fixed_release_id`，状态改为“已发布”，通知提交人；
5. 再解析 Release `body` 中的 `#N` / `owner/repo#N` 补漏。

版本说明 `body` 和资产列表同时写入知识库（PRD 3.2.0.x 的版本记录）。

### 7.3 里程碑当版本计划

`GET/POST /repos/{o}/{r}/milestones`：研发把 Issue 放进 `v1.3` 里程碑时（`issue_milestone` 事件），灵问可以告诉用户预计版本和截止日期（`due_on`）。

## 8. 上线

Gitea **没有** GitHub 那样的 Deployments / Environments API，“发版”和“上线”要分开判断。可选信号（按推荐顺序）：

1. **Gitea Actions 部署流水线（≥1.25）**：订阅 `workflow_run` 事件，`action=completed` 且 `conclusion=success`，工作流名或文件名匹配配置（如 `deploy-prod.yml`），用 run 的 `head_sha` / tag 对应到版本 → 该版本的工单状态改为“已上线”。补偿用 `GET /repos/{o}/{r}/actions/runs?status=success`。
2. **提交状态（commit status）**：外部 CI/CD（Jenkins、Drone、ArgoCD 等）部署成功后调用 `POST /repos/{o}/{r}/statuses/{sha}`，`context` 约定为 `deploy/production`；灵问订阅 `status` 事件识别。不用 Gitea Actions 的团队用这种方式最通用。
3. **接入方直接告诉灵问**：公开方法 `MarkDeployed(ctx, repo, tag, env)`，由接入方的部署脚本调用。最简单，也是 1、2 都不适用时的兜底。

需要灵问主动触发部署时（不建议默认开启），可用 `POST /repos/{o}/{r}/actions/workflows/{workflow_id}/dispatches`（≥1.24）。

多仓库项目（前端 + 后端）：一个工单拆到多个仓库时，**所有关联 Issue 的修复都上线**才算该工单上线。

## 9. 其他可借力的 Gitea 特性

| 特性 | 接口 | 用途 |
|------|------|------|
| Issue 依赖 / 阻塞 | `GET/POST/DELETE /issues/{index}/dependencies`、`/blocks`（支持跨仓库，body 为 `IssueMeta{owner, repo, index}`） | 一个用户工单拆成前端、后端两个 Issue，用依赖关系表达，全部关闭才算完成 |
| 跨仓库搜索 | `GET /repos/issues/search?q=&labels=&state=&owner=` | 提单前查重、找相似 Issue，回答“这个问题已经在处理中” |
| 指派 | `POST/DELETE /issues/{index}/assignees`、`GET /repos/{o}/{r}/assignees` | 按仓库把 Issue 派给指定研发 Agent 账号 |
| 截止日期 | `POST /issues/{index}/deadline` | SLA：按类型设置截止日，超时提醒 reviewer |
| 置顶 | `POST/DELETE /issues/{index}/pin` | 多人反馈的高频问题置顶 |
| 订阅 | `PUT /issues/{index}/subscriptions/{user}` | 让 reviewer 自动关注需求类 Issue |
| 工时 | `GET /issues/{index}/times`、`/stopwatch/*` | 统计处理耗时（可选） |
| Timeline | `GET /issues/{index}/timeline` | 一个接口拿到评论、标签、指派、关闭、PR / 提交引用的完整历史，做对账和工单事件回放 |
| 仓库内容 | `GET /repos/{o}/{r}/contents/{filepath}`、`/file-contents` | 把 README、`docs/`、CHANGELOG 导入知识库 |
| Wiki | `GET /repos/{o}/{r}/wiki/pages`、`/wiki/page/{pageName}` | 读取已有 Wiki 作为知识来源；也可把审核过的问答回写 Wiki |
| Markdown 渲染 | `POST /markdown`（`mode=gfm`、`context=owner/repo`） | 把研发回复渲染成 HTML 给用户看，`#12`、提交 SHA 会转成链接 |
| 通知收件箱 | `GET /notifications`、`/repos/{o}/{r}/notifications` | Webhook 配不了的环境，轮询机器人的 @ 提醒作为降级方案 |
| 组织级标签 / Webhook | `/orgs/{org}/labels`、`/orgs/{org}/hooks` | 多仓库统一维护 |
| 项目看板 | `/repos/{o}/{r}/projects` | 只在 main 分支，**稳定版不可用，暂不依赖** |

## 10. Webhook 接收要点

- **请求头**：`X-Gitea-Event`、`X-Gitea-Event-Type`、`X-Gitea-Delivery`（每次投递的 UUID）、`X-Gitea-Signature`；同时带 GitHub 兼容头 `X-Hub-Signature-256: sha256=...` 和 Gogs 兼容头。
- **验签**：`X-Gitea-Signature` = `hex(HMAC-SHA256(secret, 原始请求体))`。必须先读原始字节验签再反序列化，用 `hmac.Equal` 做常量时间比较。配置了 `authorization_header` 时再校验 `Authorization` 头。
- **去重**：两层。① `X-Gitea-Delivery` 落库唯一约束，挡住同一次投递的重复请求；② 手动“重新投递”会生成**新的** delivery UUID（`ReplayHookTask` 新建 hook task），对账拉回的数据也没有 delivery ID，所以还要按业务键幂等：评论 `comment.id + 动作 + updated_at`（编辑另加正文摘要）；Issue `issue.id + 动作 + updated_at`，标签 / 指派 / 里程碑事件另加当前标签 id / 指派人 / 里程碑 id，避免同一秒内的不同变化被误判为重复；PR `pr.id + 动作 + updated_at`；Release 无更新时间，用 `release.id + 动作 + 内容摘要`；状态用 `status.id`，工作流运行用 `run.id + attempt`。
- **快速返回**：只做验签、去重、落库，立刻返回 2xx，业务处理交给 worker（Gitea 默认投递超时 `[webhook] DELIVER_TIMEOUT` 为 5 秒）。
- **不保证送达**：Gitea 投递失败只记录在 hook 历史里，**不会自动重试**。所以第 1 节的定时增量对账是必需的：
  - `GET /repos/{o}/{r}/issues?state=all&labels=lingo&since=<上次对账时间>&type=issues`
  - `GET /repos/{o}/{r}/issues/comments?since=<上次对账时间>`（仓库级评论增量，一次拿到所有 Issue 的新评论）
  - `GET /repos/{o}/{r}/releases` 与本地版本记录比对
- **防回环**：`sender.login` 是机器人自己时，只用来确认 outbox 任务已生效，不再触发通知或评论。

## 11. 已采纳的设计改动

> 已于 2026-11-18 合入 PRD v1.9、`types.go`、`lingo.go` 与 `migrations/0001_init.sql`（草案未发布，直接修改）。

1. **`GiteaConfig` 扩展**（新增字段，向后兼容）：`LabelPrefix`、`ReplyMode`（marked / all）、`AutoRegisterWebhook`、`WebhookURL`。
2. **`lingo.Repo` 扩展**：`Assignees`（默认指派的研发 Agent 账号）、`Templates`（工单类型 → 模板文件名）、`Deploy`（上线信号：Actions 工作流文件名 / 提交状态 context / 环境名；各仓库流水线不同，所以放在仓库级）。新增 `l.MarkDeployed` 与事件 `ticket.deployed`。
3. **工单表**增加 `fixed_pr`、`fixed_commit`、`deployed_at`、`gitea_updated_at`、`milestone`、`due_date`；状态为 `submitted / open / in_progress / need_info / fixed / released / deployed / closed / abnormal`；新增 `ticket_issues`（跨仓库拆单）、`deployments`、`sync_cursors`。
4. **webhook_deliveries 表**（delivery_id 唯一）加上业务键幂等（见第 10 节），对账游标（每个仓库的 `since`）单独保存。
5. **版本关联**从“解析版本说明”改为“比较两个 tag 的提交 + 解析说明补漏”（第 7.2 节）。
6. **PRD 非功能需求**中 Gitea 版本“待定”改为：最低 1.22，使用 Actions 上线信号需 1.25 及以上。
7. 和研发 Agent 约定（写进 Agent 的提示词或仓库 CONTRIBUTING）：PR 使用 `Fixes owner/repo#N`；面向用户的回复用 MCP 工具或 `/reply` 前缀；需要补充信息时加 `review/need-info` 标签。

## 12. 仓库配置与启动自检

仓库层面的设置基本都有 API（v1.27.3 核实）。灵问只**读**这些设置做自检，**写**仅限标签、Webhook，以及可选的模板初始化；其余设置留给研发方。

### 12.1 仓库设置相关接口

| 类别 | 接口 | 灵问用途 |
|------|------|---------|
| 仓库属性 | `GET / PATCH /repos/{o}/{r}`（`EditRepoOption`：`has_issues`、`has_pull_requests`、`has_releases`、`has_actions`、`has_wiki`、`internal_tracker`、`external_tracker`、`default_branch`、合并方式等） | 只读自检：Issue 已启用、没有改用外部 Issue 跟踪；`internal_tracker.enable_issue_dependencies` 决定能否跨仓库拆单 |
| 权限 | `GET /repos/{o}/{r}/collaborators/{bot}/permission`、`/teams` | 自检机器人账号在每个仓库至少有 write 权限 |
| 标签 / 里程碑 | `/labels`、`/milestones`、`/orgs/{org}/labels` | 补齐标签（第 3 节） |
| Webhook | `/hooks`、`/hooks/{id}/tests`、`/orgs/{org}/hooks` | 自动注册；注册后发一次测试投递，验证 Webhook 能连通 |
| 文件内容 | `GET /contents/{filepath}`、`POST /contents`（一次提交多个文件）、`POST /branches`、`POST /pulls` | 读 `.gitea/ISSUE_TEMPLATE/`；可选：仓库没有模板时，在新分支提交灵问推荐的模板并开 PR，由研发合并（不直接写默认分支） |
| 分支 / Tag 保护 | `/branch_protections`、`/tag_protections` | 不使用 |
| 协作者 / 团队 / 部署密钥 / 推送镜像 / 主题 / 头像 / 转移 | `/collaborators`、`/teams`、`/keys`、`/push_mirrors`、`/topics`、`/avatar`、`/transfer` | 不使用 |
| Actions 密钥与变量 | `/actions/secrets`、`/actions/variables` | 不使用（部署信号只读运行结果） |
| 组织 / 管理员 | `/orgs/{org}/...`、`/admin/...` | 不使用；灵问不需要管理员 token |

### 12.2 实例级配置（只能读，部分没有 API）

- `GET /version`：启动时检查版本 ≥ 1.22，低于 1.25 时关闭 Actions 上线信号并告警。
- `GET /settings/api`：读 `max_response_items`，分页大小以它为准。
- `GET /settings/attachment`：读附件是否启用、允许的类型、大小上限、数量上限，上传前先校验。
- `GET /settings/repository`：读 `time_tracking_disabled` 等全局开关。
- **没有 API、只能在 `app.ini` 改的**，需要写进接入文档：
  - `[security] ALLOWED_HOST_LIST`（旧版本在 `[webhook]` 下）：Webhook 允许投递的目标地址。默认只允许外网地址，灵问部署在内网时 Webhook 会被拒，要加上灵问的地址或网段。
  - `[webhook] DELIVER_TIMEOUT`：投递超时，默认 5 秒。
  - `[repository.pull-request] CLOSE_KEYWORDS` / `REOPEN_KEYWORDS`：PR 自动关闭 Issue 的关键词。
  - `[api] MAX_RESPONSE_ITEMS`：单页最大条数（可经 `/settings/api` 读取）。

### 12.3 启动自检清单（`Run` 启动时执行，结果写日志，不阻断启动）

1. `/version` 版本达标；
2. 每个仓库可访问、`has_issues=true`、未启用 `external_tracker`；
3. 机器人对每个仓库有 write 权限；
4. 标签齐全（缺的补上）；
5. Webhook 存在、处于启用状态、事件订阅完整，测试投递成功（失败时提示检查 `ALLOWED_HOST_LIST`）；
6. 配置了 `Repo.Templates` 的模板文件存在，`/issue_config/validate` 通过；
7. 配置了 `Repo.Deploy.Workflow` 时，`GET /actions/workflows` 中能找到该工作流。
