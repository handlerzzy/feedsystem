package redis

import (
	"context"
	"errors"
	"time"
)

var (
	errNotInitialized = errors.New("redis client not initialized")
	errNilHandler     = errors.New("pubsub handler is nil")
)

// Pub/Sub 封装。
//
// 存在的意义是让调用方不必 import go-redis：*redis.PubSub 的生命周期很容易用错
// （没 Close 会一直占着一条连接；直接读 Channel() 而不处理重连会静默丢消息），
// 这里把正确用法固定下来。

// Publish 往频道发一条消息。
//
// 注意它**不保证送达**：Pub/Sub 是即发即忘的，没有订阅者时消息直接丢弃，也不会
// 重投。调用方必须把它当成"尽力而为的加速手段"，而不是可靠投递——真实的持久化
// 交给数据库（通知本来就先落库再推送）。
func (c *Client) Publish(ctx context.Context, channel string, payload []byte) error {
	if c == nil || c.rdb == nil {
		return errNotInitialized
	}
	return c.rdb.Publish(ctx, channel, payload).Err()
}

// SubscribeLoop 阻塞订阅 channel，把每条消息交给 handle，直到 ctx 被取消。
//
// go-redis 的 PubSub 在上层调用 ReceiveMessage 时会自行重连并重新订阅，所以这里
// 不额外做重连——重连逻辑写在外面反而容易和库内部的重订阅打架，导致重复订阅。
//
// ctx 取消后返回 nil（正常退出，不是错误）；其它错误原样返回，由调用方决定是
// 记日志后继续还是退出。
func (c *Client) SubscribeLoop(ctx context.Context, channel string, handle func(payload []byte)) error {
	if c == nil || c.rdb == nil {
		return errNotInitialized
	}
	if handle == nil {
		return errNilHandler
	}

	sub := c.rdb.Subscribe(ctx, channel)
	defer func() { _ = sub.Close() }()

	// 先确认订阅真的建立了，否则 ReceiveMessage 的第一次失败会被误当成"消息有问题"。
	if _, err := sub.Receive(ctx); err != nil {
		return err
	}

	msgCh := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				// 通道被关闭：订阅已失效且库不再重连（ctx 取消时也会走到这里）。
				return nil
			}
			handle([]byte(msg.Payload))
		}
	}
}

// PublishTimeout 是 Publish 的常用超时。Pub/Sub 只用于实时推送，不值得为它拖慢
// 请求；Redis 抖动时宁可丢掉这次实时推送（通知已经落库，客户端轮询仍能拿到）。
const PublishTimeout = 200 * time.Millisecond
