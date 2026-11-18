// Package worker 后台任务：消费 Webhook 投递（重新解析后同步到工单）、按工单顺序投递通知 outbox；
// 之后加入 Gitea 写操作 outbox、同步补偿（Issue、Release）与知识库生成。
//
// 领取统一走 store 的 SKIP LOCKED + 租约，多实例运行安全。失败按 10 秒起翻倍、最长 1 小时退避重试，
// 超过 MaxAttempts 或确定重试无用（载荷无法解析）时放弃，由定时对账补齐。
package worker
