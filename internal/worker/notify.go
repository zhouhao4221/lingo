package worker

import (
	"context"
	"errors"
	"time"

	"github.com/zhouhao4221/lingo/internal/store"
)

// DeliverNotifications 领取一批待投递通知并调用 Notify，返回领取到的条数；未配置通知时返回 0。
// 同一工单的通知按事件顺序投递：某条失败后，本批中同一工单的后续通知一并推迟到它的重试时间之后。
func (w *Worker) DeliverNotifications(ctx context.Context) (int, error) {
	if w.o.Notifications == nil || w.o.Notify == nil {
		return 0, nil
	}
	ns, err := w.o.Notifications.ClaimNotifications(ctx, w.o.BatchSize, w.o.Lease)
	if err != nil {
		return 0, err
	}
	blocked := map[int64]time.Time{} // 工单 ID → 本批中失败通知的重试时间
	for _, n := range ns {
		release := func(reason string, at time.Time) {
			w.finishNotification(n, w.o.Notifications.RetryNotification(context.WithoutCancel(ctx), n, reason, at))
		}
		if ctx.Err() != nil {
			release("worker shutting down", w.o.Now())
			continue
		}
		if at, ok := blocked[n.Event.TicketID]; ok {
			release("earlier notification of the ticket failed", at)
			continue
		}
		if retryAt, failed := w.deliver(ctx, n); failed && n.Event.TicketID != 0 {
			blocked[n.Event.TicketID] = retryAt
		}
	}
	return len(ns), nil
}

// deliver 投递一条通知；失败且将重试时返回重试时间与 failed=true。
func (w *Worker) deliver(ctx context.Context, n store.Notification) (retryAt time.Time, failed bool) {
	tctx, cancel := w.taskContext(ctx)
	defer cancel()
	log := w.log.With("event", n.Event.ID, "kind", n.Event.Kind, "attempt", n.Attempts)
	err := w.o.Notify(tctx, n.Event)
	switch {
	case err == nil:
		w.finishNotification(n, w.o.Notifications.CompleteNotification(tctx, n))
		return time.Time{}, false
	case n.Attempts >= w.o.MaxAttempts:
		log.Error("notification abandoned", "err", err)
		w.finishNotification(n, w.o.Notifications.FailNotification(tctx, n, err.Error()))
		return time.Time{}, false
	default:
		retryAt = w.o.Now().Add(backoff(n.Attempts))
		log.Warn("notification failed, will retry", "err", err, "retry_at", retryAt)
		w.finishNotification(n, w.o.Notifications.RetryNotification(tctx, n, err.Error(), retryAt))
		return retryAt, true
	}
}

func (w *Worker) finishNotification(n store.Notification, err error) {
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		w.log.Warn("notification lease lost", "event", n.Event.ID, "attempt", n.Attempts)
	case err != nil:
		w.log.Error("notification update failed", "event", n.Event.ID, "err", err)
	}
}
