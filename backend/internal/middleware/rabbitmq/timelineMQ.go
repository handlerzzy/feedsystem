package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type TimelineMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（见 Publisher 的注释）。
	pub *Publisher
}

const (
	timelineExchange   = "video.timeline.events"
	timelineQueue      = "video.timeline.update.queue"
	timelineBindingKey = "video.timeline.*"
	timelinePublishRK  = "video.timeline.publish"
)

type TimelineEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	CreateTime int64     `json:"create_time"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewTimelineMQ(base *RabbitMQ) (*TimelineMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, timelineExchange, timelineQueue, timelineBindingKey)
	})
	// 启动期仍然要求连得上：拿不到第一条 Channel 就是启动失败，
	// 与原先的行为一致。运行期的重建交给 Publisher.Do。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &TimelineMQ{pub: pub}, nil
}

func (t *TimelineMQ) PublishVideo(ctx context.Context, videoID uint, createTime time.Time) error {
	if t == nil || t.pub == nil {
		return errors.New("timeline mq is not initialized")
	}
	if videoID == 0 {
		return errors.New("videoID are required")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	timeline := TimelineEvent{
		EventID:    id,
		VideoID:    videoID,
		CreateTime: createTime.UnixMilli(),
		OccurredAt: time.Now(),
	}
	return t.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, timelineExchange, timelinePublishRK, timeline)
	})
}
