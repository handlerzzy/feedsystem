package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type CommentMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（见 Publisher 的注释）。
	pub *Publisher
}

const (
	commentExchange   = "comment.events"
	commentQueue      = "comment.events"
	commentBindingKey = "comment.*"

	commentPublishRK = "comment.publish"
	commentDeleteRK  = "comment.delete"
)

type CommentEvent struct {
	EventID    string    `json:"event_id"`
	Action     string    `json:"action"`
	CommentID  uint      `json:"comment_id,omitempty"`
	Username   string    `json:"username,omitempty"`
	VideoID    uint      `json:"video_id,omitempty"`
	AuthorID   uint      `json:"author_id,omitempty"`
	Content    string    `json:"content,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewCommentMQ(base *RabbitMQ) (*CommentMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, commentExchange, commentQueue, commentBindingKey)
	})
	// 启动期仍然要求连得上：拿不到第一条 Channel 就是启动失败，
	// 与原先的行为一致。运行期的重建交给 Publisher.Do。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &CommentMQ{pub: pub}, nil
}

func (c *CommentMQ) Publish(ctx context.Context, username string, videoID, authorID uint, content string) error {
	return c.publish(ctx, "publish", commentPublishRK, CommentEvent{
		Username: username,
		VideoID:  videoID,
		AuthorID: authorID,
		Content:  content,
	})
}

func (c *CommentMQ) Delete(ctx context.Context, commentID uint) error {
	return c.publish(ctx, "delete", commentDeleteRK, CommentEvent{
		CommentID: commentID,
	})
}

func (c *CommentMQ) publish(ctx context.Context, action, routingKey string, evt CommentEvent) error {
	if c == nil || c.pub == nil {
		return errors.New("comment mq is not initialized")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	evt.EventID = id
	evt.Action = action
	evt.OccurredAt = time.Now().UTC()
	return c.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, commentExchange, routingKey, evt)
	})
}
