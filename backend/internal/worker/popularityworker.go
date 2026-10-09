package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

type PopularityWorker struct {
	ch    *amqp.Channel
	cache *rediscache.Client
	queue string
}

func NewPopularityWorker(ch *amqp.Channel, cache *rediscache.Client, queue string) *PopularityWorker {
	return &PopularityWorker{ch: ch, cache: cache, queue: queue}
}

func (w *PopularityWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.cache == nil {
		return errors.New("popularity worker is not initialized")
	}
	if w.queue == "" {
		return errors.New("queue is required")
	}

	deliveries, err := w.ch.Consume(
		w.queue,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("deliveries channel closed")
			}
			w.handleDelivery(ctx, d)
		}
	}
}

func (w *PopularityWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	const maxRetries = 3
	for i := 0; i <= maxRetries; i++ {
		select {
		case <-ctx.Done():
			_ = d.Nack(false, true)
			return
		default:
		}
		if err := w.process(ctx, d.Body); err != nil {
			if i >= maxRetries {
				// Ack 掉即彻底丢弃这条消息，没有补偿路径，故为 Error。
				logging.Ctx(ctx).Error("popularity worker: 重试次数耗尽，消息已丢弃",
					zap.Int("max_retries", maxRetries), zap.Error(err))
				_ = d.Ack(false)
				return
			}
			wait := time.Duration(1<<uint(i)) * time.Second
			// 还在退避重试路径上，故为 Warn。
			logging.Ctx(ctx).Warn("popularity worker: 处理失败，稍后重试",
				zap.Duration("wait", wait), zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries), zap.Error(err))
			time.Sleep(wait)
			continue
		}
		_ = d.Ack(false)
		return
	}
}

func (w *PopularityWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.PopularityEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil
	}
	if evt.VideoID == 0 || evt.Change == 0 {
		return nil
	}
	video.UpdatePopularityCache(ctx, w.cache, evt.VideoID, evt.Change)
	return nil
}
