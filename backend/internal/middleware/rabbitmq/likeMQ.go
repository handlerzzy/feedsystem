package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type LikeMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（见 Publisher 的注释）。
	pub *Publisher
}

const (
	likeExchange   = "like.events"
	likeQueue      = "like.events"
	likeBindingKey = "like.*"

	likeLikeRK   = "like.like"
	likeUnlikeRK = "like.unlike"
)

type LikeEvent struct {
	EventID    string    `json:"event_id"`
	Action     string    `json:"action"`
	UserID     uint      `json:"user_id"`
	VideoID    uint      `json:"video_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewLikeMQ(base *RabbitMQ) (*LikeMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, likeExchange, likeQueue, likeBindingKey)
	})
	// 启动期仍然要求连得上：拿不到第一条 Channel 就是启动失败，
	// 与原先的行为一致。运行期的重建交给 Publisher.Do。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &LikeMQ{pub: pub}, nil
}

func (l *LikeMQ) Like(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, "like", likeLikeRK, userID, videoID)
}

func (l *LikeMQ) Unlike(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, "unlike", likeUnlikeRK, userID, videoID)
}

func (l *LikeMQ) publish(ctx context.Context, action, routingKey string, userID, videoID uint) error {
	if l == nil || l.pub == nil {
		return errors.New("like mq is not initialized")
	}
	if userID == 0 || videoID == 0 {
		return errors.New("userID and videoID are required")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	event := LikeEvent{
		EventID:    id,
		Action:     action,
		UserID:     userID,
		VideoID:    videoID,
		OccurredAt: time.Now(),
	}
	return l.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, likeExchange, routingKey, event)
	})
}
