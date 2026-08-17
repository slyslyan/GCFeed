package applicationinteraction

import (
	domaininteraction "GCFeed/internal/domain/interaction"
	inframetrics "GCFeed/internal/infra/metrics"
	"context"
	"time"
)

type ActionEventConsumer interface {
	ConsumeActionChanged(ctx context.Context, handler func(context.Context, *ActionChangedEvent) error) error
}

type ActionWorker struct {
	repo     domaininteraction.Repository
	consumer ActionEventConsumer
}

func NewActionWorker(repo domaininteraction.Repository, consumer ActionEventConsumer) *ActionWorker {
	return &ActionWorker{
		repo:     repo,
		consumer: consumer,
	}
}

func (w *ActionWorker) Start(ctx context.Context) error {
	if w == nil || w.consumer == nil {
		return nil
	}
	return w.consumer.ConsumeActionChanged(ctx, w.HandleActionChanged)
}

func (w *ActionWorker) HandleActionChanged(ctx context.Context, event *ActionChangedEvent) error {
	start := time.Now()
	var err error
	defer func() {
		inframetrics.ObserveWorkerJob("interaction_action_changed", time.Since(start), err)
	}()

	if event == nil {
		return nil
	}

	// 跳过过期事件：如果 DB 已有比该事件更新的记录，说明事件已乱序到达，丢弃。
	lastUpdated, lookupErr := w.repo.GetLastActionUpdateTime(ctx, event.UserID, event.VideoID, event.ActionType)
	if lookupErr != nil {
		// 查询失败时放行，让 SetAction 兜底（幂等 + 行锁仍能保证正确性）。
		_, _, _, err = w.repo.SetAction(ctx, event.UserID, event.VideoID, event.ActionType, event.Active, event.IdempotencyKey)
		return err
	}
	if !lastUpdated.IsZero() && !event.OccurredAt.After(lastUpdated) {
		// 事件时间 ≤ DB 记录时间 → 过期，丢弃。
		return nil
	}

	_, _, _, err = w.repo.SetAction(ctx, event.UserID, event.VideoID, event.ActionType, event.Active, event.IdempotencyKey)
	return err
}
