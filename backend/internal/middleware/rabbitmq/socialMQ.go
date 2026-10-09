package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type SocialMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（见 Publisher 的注释）。
	pub *Publisher
}

const (
	socialExchange   = "social.events"
	socialQueue      = "social.events"
	socialBindingKey = "social.*"

	socialFollowRK   = "social.follow"
	socialUnfollowRK = "social.unfollow"
)

type SocialEvent struct {
	EventID    string    `json:"event_id"`
	Action     string    `json:"action"`
	FollowerID uint      `json:"follower_id"`
	VloggerID  uint      `json:"vlogger_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewSocialMQ(base *RabbitMQ) (*SocialMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, socialExchange, socialQueue, socialBindingKey)
	})
	// 启动期仍然要求连得上：拿不到第一条 Channel 就是启动失败，
	// 与原先的行为一致。运行期的重建交给 Publisher.Do。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &SocialMQ{pub: pub}, nil
}

func (s *SocialMQ) Follow(ctx context.Context, followerID, vloggerID uint) error {
	return s.publish(ctx, "follow", socialFollowRK, followerID, vloggerID)
}

func (s *SocialMQ) UnFollow(ctx context.Context, followerID, vloggerID uint) error {
	return s.publish(ctx, "unfollow", socialUnfollowRK, followerID, vloggerID)
}

func (s *SocialMQ) publish(ctx context.Context, action, routingKey string, followerID, vloggerID uint) error {
	if s == nil || s.pub == nil {
		return errors.New("social mq is not initialized")
	}
	if followerID == 0 || vloggerID == 0 {
		return errors.New("followerID and vloggerID are required")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	evt := SocialEvent{
		EventID:    id,
		Action:     action,
		FollowerID: followerID,
		VloggerID:  vloggerID,
		OccurredAt: time.Now().UTC(),
	}
	return s.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, socialExchange, routingKey, evt)
	})
}
