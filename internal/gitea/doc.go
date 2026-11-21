// Package gitea 是灵工使用的 Gitea API 客户端与 Webhook 协议层。
//
// 覆盖灵工需要的接口：仓库与实例自检、标签、Issue（建单、编辑、搜索、锁定、依赖、timeline）、
// 评论与 reaction、附件上传、Issue 表单模板读取与渲染、仓库 Webhook 注册、Release / Tag / 提交比较、
// PR 反查、Actions 工作流运行；以及 Webhook 请求读取、HMAC-SHA256 验签与事件载荷类型。
//
// 客户端只做单次请求，不重试：对 Gitea 的写操作由 outbox + worker 负责重试，
// 调用方用 IsRetryable 判断错误是否值得重试，用 errors.Is(err, ErrNotFound) 识别资源不存在。
// 接口与字段对照 Gitea v1.27.3 swagger，最低兼容 Gitea 1.22（Actions 相关接口需 1.25）。
// 详见 docs/architecture/gitea-integration.md。
package gitea
