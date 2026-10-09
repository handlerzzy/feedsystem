package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Notification struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RecipientID uint      `gorm:"index:idx_notif_recipient_read,priority:1;not null" json:"recipient_id"`
	SenderID    uint      `gorm:"not null" json:"sender_id"`
	Type        string    `gorm:"type:varchar(50);not null" json:"type"`
	TargetID    uint      `json:"target_id"`
	Content     string    `gorm:"type:varchar(255)" json:"content"`
	IsRead      bool      `gorm:"index:idx_notif_recipient_read,priority:2;default:false" json:"is_read"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
}

type NotificationWorker struct {
	ch    *amqp.Channel
	db    *gorm.DB
	queue string
	hub   NotificationHub
}

type NotificationHub interface {
	Push(userID uint, n *Notification)
}

func NewNotificationWorker(ch *amqp.Channel, db *gorm.DB, queue string, hub NotificationHub) *NotificationWorker {
	return &NotificationWorker{ch: ch, db: db, queue: queue, hub: hub}
}

func (w *NotificationWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.db == nil {
		return errors.New("notification worker is not initialized")
	}
	if w.queue == "" {
		return errors.New("queue is required")
	}
	if err := w.db.WithContext(ctx).AutoMigrate(&Notification{}); err != nil {
		return err
	}
	deliveries, err := w.ch.Consume(w.queue, "", false, false, false, false, nil)
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

func (w *NotificationWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	const maxRetries = 3
	for i := 0; i <= maxRetries; i++ {
		select {
		case <-ctx.Done():
			_ = d.Nack(false, true)
			return
		default:
		}
		if err := w.process(ctx, d); err != nil {
			if i >= maxRetries {
				// Ack 掉即彻底丢弃这条消息，没有补偿路径，故为 Error。
				logging.Ctx(ctx).Error("notification worker: 重试次数耗尽，消息已丢弃",
					zap.Int("max_retries", maxRetries), zap.Error(err))
				_ = d.Ack(false)
				return
			}
			wait := time.Duration(1<<uint(i)) * time.Second
			// 还在退避重试路径上，故为 Warn。
			logging.Ctx(ctx).Warn("notification worker: 处理失败，稍后重试",
				zap.Duration("wait", wait), zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries), zap.Error(err))
			time.Sleep(wait)
			continue
		}
		_ = d.Ack(false)
		return
	}
}

func (w *NotificationWorker) process(ctx context.Context, d amqp.Delivery) error {
	body := d.Body
	if len(body) == 0 {
		return nil
	}
	routingKey := d.RoutingKey

	var notif *Notification

	switch {
	case routingKey == "like.like":
		var evt rabbitmq.LikeEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return nil
		}
		if evt.UserID == 0 || evt.VideoID == 0 {
			return nil
		}
		var authorID uint
		if err := w.db.WithContext(ctx).Model(&struct {
			ID       uint
			AuthorID uint
		}{}).Table("videos").Where("id = ?", evt.VideoID).Select("author_id").Scan(&authorID).Error; err != nil {
			return err
		}
		if authorID == 0 || authorID == evt.UserID {
			return nil
		}
		notif = &Notification{RecipientID: authorID, SenderID: evt.UserID, Type: "like", TargetID: evt.VideoID, Content: "点赞了你的视频"}

	case routingKey == "comment.publish":
		var evt rabbitmq.CommentEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return nil
		}
		if evt.AuthorID == 0 || evt.VideoID == 0 {
			return nil
		}
		var authorID uint
		if err := w.db.WithContext(ctx).Model(&struct {
			ID       uint
			AuthorID uint
		}{}).Table("videos").Where("id = ?", evt.VideoID).Select("author_id").Scan(&authorID).Error; err != nil {
			return err
		}
		if authorID == 0 || authorID == evt.AuthorID {
			return nil
		}
		notif = &Notification{RecipientID: authorID, SenderID: evt.AuthorID, Type: "comment", TargetID: evt.VideoID, Content: "评论了你的视频"}

	case routingKey == "social.follow":
		var evt rabbitmq.SocialEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return nil
		}
		if evt.FollowerID == 0 || evt.VloggerID == 0 {
			return nil
		}
		notif = &Notification{RecipientID: evt.VloggerID, SenderID: evt.FollowerID, Type: "follow", TargetID: evt.FollowerID, Content: "关注了你"}
	}

	if notif == nil {
		return nil
	}
	if err := w.db.WithContext(ctx).Create(notif).Error; err != nil {
		return err
	}
	if w.hub != nil {
		w.hub.Push(notif.RecipientID, notif)
	}
	return nil
}
