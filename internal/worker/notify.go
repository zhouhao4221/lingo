package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/store"
)

// DeliverNotifications 领取一批待投递通知并调用 Notify，返回领取到的条数；未配置通知时返回 0。
// 同一工单的通知按事件顺序投递：领取时已排除有更早未完成通知的工单，一批中同一工单至多一条。
func (w *Worker) DeliverNotifications(ctx context.Context) (int, error) {
	if w.o.Notifications == nil || w.o.Notify == nil {
		return 0, nil
	}
	claimed := time.Now()
	ns, err := w.o.Notifications.ClaimNotifications(ctx, w.o.BatchSize, w.o.Lease)
	if err != nil {
		return 0, err
	}
	budget := w.batchBudget(claimed)
	for i, n := range ns {
		switch {
		case ctx.Err() != nil:
			w.releaseNotification(ctx, n, "worker shutting down")
		case i > 0 && time.Now().After(budget):
			w.releaseNotification(ctx, n, "lease budget exhausted")
		default:
			w.deliver(ctx, n)
		}
	}
	return len(ns), nil
}

func (w *Worker) releaseNotification(ctx context.Context, n store.Notification, reason string) {
	fctx, cancel := finishContext(ctx)
	defer cancel()
	w.finishNotification(n, w.o.Notifications.ReleaseNotification(fctx, n, reason, w.o.Now()))
}

func (w *Worker) deliver(ctx context.Context, n store.Notification) {
	tctx, cancel := w.taskContext(ctx)
	err := w.callNotify(tctx, n.Event)
	cancel()
	fctx, fcancel := finishContext(ctx)
	defer fcancel()
	log := w.log.With("event", n.Event.ID, "kind", n.Event.Kind, "attempt", n.Attempts, "failures", n.Failures)
	switch {
	case err == nil:
		w.finishNotification(n, w.o.Notifications.CompleteNotification(fctx, n))
	case n.Failures+1 >= w.o.MaxAttempts:
		log.Error("notification abandoned", "err", err)
		w.finishNotification(n, w.o.Notifications.FailNotification(fctx, n, err.Error()))
	default:
		retryAt := w.o.Now().Add(backoff(n.Failures + 1))
		log.Warn("notification failed, will retry", "err", err, "retry_at", retryAt)
		w.finishNotification(n, w.o.Notifications.RetryNotification(fctx, n, err.Error(), retryAt))
	}
}

// callNotify 调用接入方的 Notifier；其中的 panic 按失败处理，不让宿主进程崩溃。
func (w *Worker) callNotify(ctx context.Context, e notify.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("notifier panic: %v", r)
		}
	}()
	return w.o.Notify(ctx, e)
}

func (w *Worker) finishNotification(n store.Notification, err error) {
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		w.log.Warn("notification lease lost", "event", n.Event.ID, "attempt", n.Attempts)
	case err != nil:
		w.log.Error("notification update failed", "event", n.Event.ID, "err", err)
	}
}
