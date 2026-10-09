package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/video"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

type CommentWorker struct {
	ch       *amqp.Channel
	comments *video.CommentRepository
	videos   *video.VideoRepository
	queue    string
}

func NewCommentWorker(ch *amqp.Channel, comments *video.CommentRepository, videos *video.VideoRepository, queue string) *CommentWorker {
	return &CommentWorker{ch: ch, comments: comments, videos: videos, queue: queue}
}

func (w *CommentWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.comments == nil || w.videos == nil {
		return errors.New("comment worker is not initialized")
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

func (w *CommentWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
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
				logging.Ctx(ctx).Error("comment worker: 重试次数耗尽，消息已丢弃",
					zap.Int("max_retries", maxRetries), zap.Error(err))
				_ = d.Ack(false)
				return
			}
			wait := time.Duration(1<<uint(i)) * time.Second
			// 还在退避重试路径上，故为 Warn。
			logging.Ctx(ctx).Warn("comment worker: 处理失败，稍后重试",
				zap.Duration("wait", wait), zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries), zap.Error(err))
			time.Sleep(wait)
			continue
		}
		_ = d.Ack(false)
		return
	}
}

func (w *CommentWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.CommentEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil
	}
	switch evt.Action {
	case "publish":
		return w.applyPublish(ctx, &evt)
	case "delete":
		return w.applyDelete(ctx, &evt)
	default:
		return nil
	}
}

func (w *CommentWorker) applyPublish(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	if evt == nil || evt.VideoID == 0 || evt.AuthorID == 0 || strings.TrimSpace(evt.Content) == "" {
		return nil
	}

	ok, err := w.videos.IsExist(ctx, evt.VideoID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	c := &video.Comment{
		Username: strings.TrimSpace(evt.Username),
		VideoID:  evt.VideoID,
		AuthorID: evt.AuthorID,
		Content:  strings.TrimSpace(evt.Content),
	}
	if err := w.comments.CreateComment(ctx, c); err != nil {
		return err
	}
	return w.videos.ChangePopularity(ctx, evt.VideoID, 1)
}

func (w *CommentWorker) applyDelete(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	if evt == nil || evt.CommentID == 0 {
		return nil
	}
	c, err := w.comments.GetByID(ctx, evt.CommentID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	return w.comments.DeleteComment(ctx, c)
}
