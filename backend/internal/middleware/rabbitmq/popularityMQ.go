package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type PopularityMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（见 Publisher 的注释）。
	pub *Publisher
}

const (
	popularityExchange   = "video.popularity.events"
	popularityQueue      = "video.popularity.events"
	popularityBindingKey = "video.popularity.*"

	popularityUpdateRK = "video.popularity.update"
)

type PopularityEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	Change     int64     `json:"change"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewPopularityMQ(base *RabbitMQ) (*PopularityMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, popularityExchange, popularityQueue, popularityBindingKey)
	})
	// 启动期仍然要求连得上：拿不到第一条 Channel 就是启动失败，
	// 与原先的行为一致。运行期的重建交给 Publisher.Do。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &PopularityMQ{pub: pub}, nil
}

func (p *PopularityMQ) Update(ctx context.Context, videoID uint, change int64) error {
	if p == nil || p.pub == nil {
		return errors.New("popularity mq is not initialized")
	}
	if videoID == 0 || change == 0 {
		return errors.New("videoID and change are required")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	event := PopularityEvent{
		EventID:    id,
		VideoID:    videoID,
		Change:     change,
		OccurredAt: time.Now().UTC(),
	}
	return p.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, popularityExchange, popularityUpdateRK, event)
	})
}
