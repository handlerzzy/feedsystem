package worker

import (
	"context"
	"encoding/json"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"
	"fmt"
	"os"
	"time"

	oredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// outbox 状态机的取值。表由 GORM 依据 video.OutboxMsg 推导（实测表名 outbox_msgs）。
const (
	outboxStatusPending    = "pending"
	outboxStatusProcessing = "processing"

	outboxClaimLimit   = 100
	outboxPollInterval = 1 * time.Second
	// outboxZombieAfter：processing 超过该时长仍未被删除，视为抢占者已崩溃，回收为 pending。
	outboxZombieAfter = 5 * time.Minute
	// outboxZombieEveryRounds：每 N 轮回收一次僵尸，避免每轮都发一条 UPDATE。
	outboxZombieEveryRounds = 30
)

// outboxPoller 把 outbox_msgs 中 pending 的消息投递到 timeline MQ。
// 多副本安全：通过事务内 SELECT ... FOR UPDATE SKIP LOCKED + 置 processing 抢占，
// 副本之间不会拿到同一批消息。
type outboxPoller struct {
	db *gorm.DB
	mq *rabbitmq.TimelineMQ
}

// StartOutboxPoller 阻塞运行 outbox 轮询循环，直到 ctx 被取消。
// 调用方负责在 goroutine 中运行它（API 侧由 errgroup 托管）。
func StartOutboxPoller(ctx context.Context, db *gorm.DB, tmq *rabbitmq.TimelineMQ) {
	if db == nil || tmq == nil {
		// 依赖缺失即整体关闭轮询，进程继续运行（消息留在表里），故为 Warn。
		logging.Ctx(ctx).Warn("Outbox poller disabled: timeline mq is not initialized")
		return
	}

	p := &outboxPoller{db: db, mq: tmq}
	logging.Ctx(ctx).Info("outbox poller 启动", zap.Int("pid", os.Getpid()))

	round := 0
	for {
		select {
		case <-ctx.Done():
			logging.Ctx(ctx).Info("outbox poller 退出")
			return
		default:
		}

		round++
		if round%outboxZombieEveryRounds == 1 {
			p.requeueZombies(ctx)
		}

		messages, err := p.claim(ctx, outboxClaimLimit)
		if err != nil {
			// 抢占失败只影响本轮：消息仍是 pending，下一秒重试，不会丢，故为 Warn。
			logging.Ctx(ctx).Warn("outbox 抢占失败", zap.Error(err))
		}
		if err != nil || len(messages) == 0 {
			if !sleepCtx(ctx, outboxPollInterval) {
				logging.Ctx(ctx).Info("outbox poller 退出")
				return
			}
			continue
		}

		delivered := 0
		for i := range messages {
			if ctx.Err() != nil {
				logging.Ctx(ctx).Info("outbox poller 退出")
				return
			}
			if p.deliver(ctx, &messages[i]) {
				delivered++
			}
		}

		// 整批都失败（例如 MQ 断线）时不要立刻重新抢占，否则会形成
		// claim -> 投递失败 -> 回置 pending -> 立刻 claim 的空转循环，
		// 每轮还会多出一次事务 + N 条 UPDATE。退化为固定 1 秒轮询。
		if delivered == 0 {
			if !sleepCtx(ctx, outboxPollInterval) {
				logging.Ctx(ctx).Info("outbox poller 退出")
				return
			}
		}
	}
}

// claim 在一个事务内抢占至多 limit 条 pending 消息，并置为 processing。
// SKIP LOCKED 让并发副本跳过已被别人锁定的行，而不是阻塞等待。
func (p *outboxPoller) claim(ctx context.Context, limit int) ([]video.OutboxMsg, error) {
	var claimed []video.OutboxMsg
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var batch []video.OutboxMsg
		if err := tx.Clauses(clause.Locking{
			Strength: clause.LockingStrengthUpdate,
			Options:  clause.LockingOptionsSkipLocked,
		}).Where("status = ?", outboxStatusPending).Order("create_time ASC").Limit(limit).
			Find(&batch).Error; err != nil {
			return err
		}
		for i := range batch {
			if err := tx.Model(&video.OutboxMsg{}).Where("id = ?", batch[i].ID).
				Update("status", outboxStatusProcessing).Error; err != nil {
				return err
			}
		}
		claimed = batch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// deliver 投递单条消息：成功即删除并返回 true；失败必须回置 pending，否则会永久卡在 processing。
func (p *outboxPoller) deliver(ctx context.Context, msg *video.OutboxMsg) bool {
	if err := p.mq.PublishVideo(ctx, msg.VideoID, msg.CreateTime); err != nil {
		// 投递失败但消息会回置 pending 等待下一轮重投，不丢消息，故为 Warn。
		logging.Ctx(ctx).Warn("投递 MQ 失败",
			zap.Uint("id", msg.ID), zap.Uint("video_id", msg.VideoID), zap.Error(err))
		// 用不绑 ctx 的 db：即使处于退出流程也要把状态回置，避免消息滞留 processing。
		if err := p.db.Model(&video.OutboxMsg{}).Where("id = ?", msg.ID).
			Update("status", outboxStatusPending).Error; err != nil {
			// 回置失败会让消息卡在 processing（只能等 5 分钟后的僵尸回收兜底），故为 Error。
			logging.Ctx(ctx).Error("回置 pending 失败",
				zap.Uint("id", msg.ID), zap.Error(err))
		}
		return false
	}
	if err := p.db.Delete(msg).Error; err != nil {
		// MQ 已投递成功但删除失败：僵尸回收后同一条消息会被再次投递（重复消费），故为 Error。
		logging.Ctx(ctx).Error("删除 outbox 消息失败",
			zap.Uint("id", msg.ID), zap.Error(err))
		return false
	}
	return true
}

// requeueZombies 回收长时间停留在 processing 的消息（抢占者崩溃/被 kill 的场景）。
func (p *outboxPoller) requeueZombies(ctx context.Context) {
	res := p.db.WithContext(ctx).Model(&video.OutboxMsg{}).
		Where("status = ? AND create_time < ?", outboxStatusProcessing, time.Now().Add(-outboxZombieAfter)).
		Update("status", outboxStatusPending)
	if res.Error != nil {
		// 纯维护动作，30 轮后才再试，不影响主流程，故为 Warn。
		logging.Ctx(ctx).Warn("回收僵尸 outbox 消息失败", zap.Error(res.Error))
		return
	}
	if res.RowsAffected > 0 {
		logging.Ctx(ctx).Info("回收僵尸 outbox 消息", zap.Int64("count", res.RowsAffected))
	}
}

func StartConsumer(ctx context.Context, tmq *rabbitmq.TimelineMQ, queueName string, redisClient *rediscache.Client, rmq *rabbitmq.RabbitMQ) {
	if tmq == nil || rmq == nil || rmq.Connection() == nil {
		// 依赖缺失即不启动消费者，进程继续运行，故为 Warn。
		logging.Ctx(ctx).Warn("Timeline consumer disabled: rabbitmq is not initialized")
		return
	}
	if redisClient == nil {
		logging.Ctx(ctx).Warn("Timeline consumer disabled: redis is not initialized")
		return
	}

	// 阻塞运行消费循环，直到 ctx 被取消；调用方负责放在 goroutine 中运行。
	for {
		select {
		case <-ctx.Done():
			logging.Ctx(ctx).Info("Timeline consumer 退出")
			return
		default:
		}

		// 每次重连创建独立的 Channel，不与发布者共用
		ch, err := rmq.NewChannel()
		if err != nil {
			// 可自愈：5 秒后重连，故为 Warn。
			logging.Ctx(ctx).Warn("Timeline consumer 创建 Channel 失败，5 秒后重试", zap.Error(err))
			if !sleepCtx(ctx, 5*time.Second) {
				logging.Ctx(ctx).Info("Timeline consumer 退出")
				return
			}
			continue
		}

		if err := ch.Qos(10, 0, false); err != nil {
			// QoS 没设上只影响预取窗口，消费仍继续，故为 Warn。
			logging.Ctx(ctx).Warn("Timeline consumer QoS 设置失败", zap.Error(err))
		}

		msgs, err := ch.Consume(queueName, "", false, false, false, false, nil)
		if err != nil {
			logging.Ctx(ctx).Warn("Timeline consumer 注册消费失败，5 秒后重试",
				zap.String("queue", queueName), zap.Error(err))
			ch.Close()
			if !sleepCtx(ctx, 5*time.Second) {
				logging.Ctx(ctx).Info("Timeline consumer 退出")
				return
			}
			continue
		}

		logging.Ctx(ctx).Info("Timeline consumer 已启动", zap.String("queue", queueName))

		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				ch.Close()
				logging.Ctx(ctx).Info("Timeline consumer 退出")
				return
			case msg, ok := <-msgs:
				if !ok {
					closed = true
					continue
				}

				var event rabbitmq.TimelineEvent
				if err := json.Unmarshal(msg.Body, &event); err != nil {
					// Ack 掉即永久丢弃这条消息，没有任何自动补偿，故为 Error。
					logging.Ctx(ctx).Error("Timeline consumer 反序列化失败，消息已丢弃",
						zap.String("queue", queueName), zap.Error(err))
					msg.Ack(false)
					continue
				}

				opCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				timelineKey := redisClient.Key("feed:global_timeline")
				err = redisClient.ZAdd(opCtx, timelineKey, oredis.Z{
					Score:  float64(event.CreateTime),
					Member: fmt.Sprintf("%d", event.VideoID),
				})

				if err != nil {
					// Nack 后由 broker 重投，可自愈（Redis 恢复即追上），故为 Warn。
					logging.Ctx(ctx).Warn("Timeline consumer 写入 Zset 失败",
						zap.String("key", timelineKey), zap.Uint("video_id", event.VideoID), zap.Error(err))
					msg.Nack(false, true)
					cancel()
					continue
				}

				if err := redisClient.ZRemRangeByRank(opCtx, timelineKey, 0, -1001); err != nil {
					// 裁剪只是防止 Zset 无限增长，本条消息已成功写入并 Ack，故为 Warn。
					logging.Ctx(ctx).Warn("Timeline consumer ZRem 失败",
						zap.String("key", timelineKey), zap.Error(err))
				}

				msg.Ack(false)
				cancel()
			}
		}

		// msgs channel 关闭说明 AMQP Channel 断开，关闭并重连
		ch.Close()
		logging.Ctx(ctx).Warn("Timeline consumer Channel 断开，5 秒后重连")
		if !sleepCtx(ctx, 5*time.Second) {
			logging.Ctx(ctx).Info("Timeline consumer 退出")
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
