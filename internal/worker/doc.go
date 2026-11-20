// Package worker 后台任务：消费 Webhook 投递（重新解析后同步到工单）、按工单顺序投递通知 outbox；
// Gitea 写操作 outbox（建 Issue、发评论、传附件）；定时对账（按游标补齐 Webhook 漏掉的 PR、Issue、评论与版本）；
// 待确认超时提醒与孤儿附件清理。
//
// 领取统一走 store 的 SKIP LOCKED + 租约，多实例运行安全。失败按 10 秒起翻倍、最长 1 小时退避重试，
// 超过 MaxAttempts 或确定重试无用（载荷无法解析）时放弃，由定时对账补齐。
package worker
