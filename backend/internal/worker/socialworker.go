package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/social"
	"time"

	"github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

type SocialWorker struct {
	ch    *amqp.Channel
	repo  *social.SocialRepository
	queue string
}

func NewSocialWorker(ch *amqp.Channel, repo *social.SocialRepository, queue string) *SocialWorker {
	return &SocialWorker{ch: ch, repo: repo, queue: queue}
}

func (w *SocialWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.repo == nil {
		return errors.New("social worker is not initialized")
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

func (w *SocialWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
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
				logging.Ctx(ctx).Error("social worker: 重试次数耗尽，消息已丢弃",
					zap.Int("max_retries", maxRetries), zap.Error(err))
				_ = d.Ack(false)
				return
			}
			wait := time.Duration(1<<uint(i)) * time.Second
			// 还在退避重试路径上，故为 Warn。
			logging.Ctx(ctx).Warn("social worker: 处理失败，稍后重试",
				zap.Duration("wait", wait), zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries), zap.Error(err))
			time.Sleep(wait)
			continue
		}
		_ = d.Ack(false)
		return
	}
}

func (w *SocialWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.SocialEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		// 解析事件失败，直接丢弃
		return nil
	}
	if evt.FollowerID == 0 || evt.VloggerID == 0 {
		return nil
	}

	switch evt.Action {
	case "follow":
		err := w.repo.Follow(ctx, &social.Social{
			FollowerID: evt.FollowerID,
			VloggerID:  evt.VloggerID,
		})
		if err == nil {
			return nil
		}
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return nil
		}
		return err
	case "unfollow":
		return w.repo.Unfollow(ctx, &social.Social{
			FollowerID: evt.FollowerID,
			VloggerID:  evt.VloggerID,
		})
	default:
		return nil
	}
}
