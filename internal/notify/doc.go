// Package notify 进展回传：定义对外事件 Event。领域服务在业务事务内写入 events 表与通知 outbox，
// 由 worker 按工单顺序投递给接入方 lingo.Notifier，失败重试；events 表同时是 ListEvents 变更流的数据源。
package notify
