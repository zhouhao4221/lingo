// Package notify 进展回传：把工单、评审、版本事件写入 outbox，由 worker 投递给接入方 lingo.Notifier，失败重试。
package notify
