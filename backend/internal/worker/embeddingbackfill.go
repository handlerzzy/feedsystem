package worker

import (
	"context"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/video"

	"go.uber.org/zap"
)

// 本文件实现 P2 §4.1 里"向量生产"的**调度**部分。
//
// 为什么是一个后台循环而不是一条 MQ 消息：
//
//  1. **视频发布不直接产生向量**。发布路径只投递一条 AI 分析任务
//     （P1），而向量依赖标题与描述——它们在发布时就已确定，不需要等分析。
//     把回填挂到发布路径上会让"发布"多一次同步的向量调用，
//     那与"不得让用户请求等待模型"直接冲突。
//  2. **内容变更也要重算**。标题/描述改了之后没有事件可听（当前没有
//     这样的 outbox 事件），只有周期性扫描才能发现 content_hash 不一致。
//  3. **失败要能自愈**。一次模型超时、sidecar 重启都不该让某条视频
//     永远没有向量；"下一轮再来"是最简单也最可靠的补偿。
//
// 代价是"新视频的向量最多晚一个间隔"。这个延迟不影响正确性：
// 新内容由**探索通道**保证曝光（P2 §4.5），而语义召回用的是稍后的批次。

// EmbeddingBackfiller 是回填循环需要的能力（接口定义在消费方）。
//
// 与 worker 里其它接口同一个形状：只要一个方法，于是单测可以用一个
// 记录调用次数的假实现覆盖"立即执行 / 出错继续 / 优雅退出"这三件事，
// 不需要 MySQL、不需要 sidecar。
type EmbeddingBackfiller interface {
	BackfillOnce(ctx context.Context) (video.BackfillStats, error)
}

// 编译期断言：真正的回填器必须满足这个接口。
var _ EmbeddingBackfiller = (*video.EmbeddingBackfiller)(nil)

// EmbeddingBackfillInterval 是回填的默认轮询间隔。
//
// 30 秒的依据：
//   - 比"新视频需要多久才能被语义召回捞到"这个用户可感知的阈值短得多
//     （内容发出来一分钟后被语义召回与两分钟后被召回，没有区别）；
//   - 一轮空扫描只是一次带索引的 LIMIT 查询 + 一次 LEFT JOIN，
//     30 秒一次对数据库的压力可以忽略；
//   - 出现缺口时（新部署、换了模型）能在几分钟内补齐一个中等规模的库。
const EmbeddingBackfillInterval = 30 * time.Second

// EmbeddingBackfillLoop 周期性调用回填器。
type EmbeddingBackfillLoop struct {
	backfiller EmbeddingBackfiller
	interval   time.Duration
}

// NewEmbeddingBackfillLoop 构造回填循环。
//
// interval <= 0 时用默认值：写成一个"取值时兜底"而不是要求调用方
// 传对，是因为传 0 的后果是**忙等**（一次 100% CPU 的死循环），
// 而那种故障的现象只是"CPU 莫名其妙满了"，排查成本远高于这里多一行判断。
func NewEmbeddingBackfillLoop(b EmbeddingBackfiller, interval time.Duration) *EmbeddingBackfillLoop {
	if interval <= 0 {
		interval = EmbeddingBackfillInterval
	}
	return &EmbeddingBackfillLoop{backfiller: b, interval: interval}
}

// Run 阻塞运行直到 ctx 取消。backfiller 为 nil 时立即返回。
//
// 立即执行一次再进入 ticker：首次回填不该等一个完整的间隔
// （新部署时那意味着"启动后 30 秒内语义路一定是空的"）。
func (l *EmbeddingBackfillLoop) Run(ctx context.Context) {
	if l == nil || l.backfiller == nil {
		return
	}
	l.tick(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick 跑一轮并决定日志级别。
//
// 日志策略（与时间线自愈那条日志同一个思路）：
//   - 有变化（写入/失败/跳过）：Info，带全部计数；
//   - 没变化：Debug。**不能用 Info**——稳态下每 30 秒一条"什么都没做"
//     会很快把日志刷满，而真正要看的信息（哪里失败了）就被淹没了。
//   - 出错：Warn。它是可自愈的（下一轮重试），
//     但连续出现说明 sidecar 或数据库有问题，需要有人看见。
func (l *EmbeddingBackfillLoop) tick(ctx context.Context) {
	stats, err := l.backfiller.BackfillOnce(ctx)
	if err != nil {
		// ctx 取消导致的返回是正常退出路径，不该记 Warn。
		if ctx.Err() != nil {
			return
		}
		logging.Ctx(ctx).Warn("向量回填失败，下一轮重试（不影响 Feed 与其它 worker）",
			zap.Duration("interval", l.interval), zap.Error(err))
		return
	}
	fields := []zap.Field{
		zap.Int("scanned", stats.Scanned),
		zap.Int("stale", stats.Stale),
		zap.Int("embedded", stats.Embedded),
		zap.Int("skipped", stats.Skipped),
		zap.Int("failed", stats.Failed),
		zap.Int("pending", stats.Pending),
	}
	if stats.Progress() {
		// Pending > 0 说明这批没扫完，下一轮继续。把它当成"进行中"打出来，
		// 是为了让"向量池有多大、还差多少"变成一个不用查库就能看到的事实。
		logging.Ctx(ctx).Info("向量回填完成一轮", fields...)
		return
	}
	logging.Ctx(ctx).Debug("向量回填：无变化", fields...)
}
