# 架构概览

## 业务流

1. 接入系统（内部业务系统）通过 AppKey 调用门户 OpenAPI，或用户在门户页面直接提交。
2. 门户按类型（咨询 / Bug / 需求）创建工单，并由机器人账号在映射的 Gitea 仓库创建 Issue，
   打上类型标签，保存 `repo + issue number` 关联。
3. Gitea 中 Issue 被评论、改状态、改标签时，Webhook 推送到 `/webhooks/gitea`；
   门户校验签名、去重后更新工单，并写入通知 outbox。
4. Worker 投递通知：回调接入系统 `callback_url`、邮件或站内信，失败重试。
5. 需求类工单进入评审：内部人员在门户评审或回复，结论同步为 Gitea 评论与标签。
6. 工单结案后，Worker 从工单正文、评论与评审记录抽取问答对写入知识库；
   用户提交咨询前可先检索知识库。

## 模块

| 目录 | 职责 |
| --- | --- |
| `internal/tenant` | 接入系统注册、凭据、回调地址、仓库映射 |
| `internal/user` | 外部用户与内部人员、Gitea 账号映射、角色 |
| `internal/auth` | 门户登录、AppKey 签名校验、角色鉴权 |
| `internal/ticket` | 工单领域与状态机 |
| `internal/review` | 需求评审与回复 |
| `internal/gitea` | Gitea API 客户端 |
| `internal/webhook` | Gitea Webhook 接收与事件转换 |
| `internal/notify` | 信息反馈渠道 |
| `internal/knowledge` | 知识库问答生成与检索 |
| `internal/worker` | outbox 投递、同步补偿、知识库生成 |
| `internal/store` | 数据库与仓储 |
| `internal/httpapi` | HTTP 路由 |
| `web/` | Vue 3 门户与管理后台 |
