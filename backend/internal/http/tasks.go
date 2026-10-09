package http

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/worker"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// notificationQueues 是三个 notification 消费者对应的队列。
var notificationQueues = []string{"notification.like", "notification.comment", "notification.social"}

// StartBackgroundTasks 启动 API 进程内的常驻任务，并把它们纳入返回的 errgroup。
// 返回的 error 只表示"启动参数不合法"，不表示任务运行结果。
// 所有 goroutine 都必须响应 ctx.Done()；调用方在退出时对 errgroup 做带超时的 Wait。
//
// 注意：SSEHub 由调用方通过 Deps 注入，这里不会新建实例——
// 否则会出现"一个 hub 推送、另一个 hub 里没有客户端"的问题。
func StartBackgroundTasks(ctx context.Context, deps Deps) (*errgroup.Group, error) {
	if deps.DB == nil {
		return nil, errors.New("deps.DB is required")
	}
	if deps.SSEHub == nil {
		return nil, errors.New("deps.SSEHub is required")
	}

	g, ctx := errgroup.WithContext(ctx)

	timelineMQ, err := rabbitmq.NewTimelineMQ(deps.RMQ)
	if err != nil {
		logging.Ctx(ctx).Warn("timelineMQ init failed (mq disabled)", zap.Error(err))
		timelineMQ = nil
	}
	// StartOutboxPoller / StartConsumer 内部是阻塞循环，返回即代表已响应 ctx 退出。
	g.Go(func() error {
		worker.StartOutboxPoller(ctx, deps.DB, timelineMQ)
		return nil
	})
	g.Go(func() error {
		worker.StartConsumer(ctx, timelineMQ, rabbitmq.TimelineTopology().Queue, deps.Cache, deps.RMQ)
		return nil
	})

	// SSE 跨副本扇出的订阅端：每个副本都订阅同一个频道，把别的副本消费到的
	// notification 投给本副本持有的连接。没启用扇出时它只记一行日志就返回。
	g.Go(func() error {
		deps.SSEHub.StartFanout(ctx)
		return nil
	})

	// SSE notification 拓扑声明（同步执行，与改前一致）
	if deps.RMQ != nil {
		if notifCh, err := deps.RMQ.NewChannel(); err == nil {
			if err := rabbitmq.DeclareTopic(notifCh, "like.events", "notification.like", "like.like"); err != nil {
				logging.Ctx(ctx).Warn("notification like topic init failed", zap.Error(err))
			}
			if err := rabbitmq.DeclareTopic(notifCh, "comment.events", "notification.comment", "comment.publish"); err != nil {
				logging.Ctx(ctx).Warn("notification comment topic init failed", zap.Error(err))
			}
			if err := rabbitmq.DeclareTopic(notifCh, "social.events", "notification.social", "social.follow"); err != nil {
				logging.Ctx(ctx).Warn("notification social topic init failed", zap.Error(err))
			}
			notifCh.Close()
		}
	}

	if deps.RMQ == nil {
		// 降级运行：SSE 通知整体关闭，但不影响其余功能。
		logging.Ctx(ctx).Warn("Notification SSE disabled (MQ not available)")
		return g, nil
	}

	// 每个 notification worker 独立 Channel + 自动重连
	for _, queue := range notificationQueues {
		g.Go(func() error {
			runNotificationConsumer(ctx, deps, queue)
			return nil
		})
	}
	return g, nil
}

// runNotificationConsumer 为单个 notification 队列维持消费循环，直到 ctx 被取消。
// 保留原有的"Channel 断开 → 5 秒后重连"策略，不在本项内改动重试策略。
func runNotificationConsumer(ctx context.Context, deps Deps, queue string) {
	for {
		select {
		case <-ctx.Done():
			logging.Ctx(ctx).Info("notification 消费者退出", zap.String("queue", queue))
			return
		default:
		}

		ch, err := deps.RMQ.NewChannel()
		if err != nil {
			logging.Ctx(ctx).Warn("notification 创建 Channel 失败，5 秒后重试",
				zap.String("queue", queue), zap.Error(err))
			if !sleepCtx(ctx, 5*time.Second) {
				logging.Ctx(ctx).Info("notification 消费者退出", zap.String("queue", queue))
				return
			}
			continue
		}
		w := worker.NewNotificationWorker(ch, deps.DB, queue, deps.SSEHub)
		if err := w.Run(ctx); err != nil {
			logging.Ctx(ctx).Warn("notification 消费中断，5 秒后重连",
				zap.String("queue", queue), zap.Error(err))
		}
		ch.Close()

		if !sleepCtx(ctx, 5*time.Second) {
			logging.Ctx(ctx).Info("notification 消费者退出", zap.String("queue", queue))
			return
		}
	}
}

// sleepCtx 是可被 ctx 中断的等待。返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
