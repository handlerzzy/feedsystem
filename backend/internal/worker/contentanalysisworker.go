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
)

// Analyzer 是 worker 需要的分析能力，**接口定义在消费方**（本项目惯例）。
//
// 为什么不让 worker 直接依赖 *video.VideoAIAnalyzer：
// worker 的职责只有"消费、重试、丢弃"这套与业务无关的骨架，
// 而分析需要模型客户端与数据库。依赖具体类型会让 worker 的单测
// 必须构造 MySQL 与 sidecar；依赖一个单方法接口，测试里传一个函数就够了。
//
// *video.VideoAIAnalyzer 隐式实现本接口（那边有编译期断言）。
type Analyzer interface {
	Analyze(ctx context.Context, videoID uint) error
}

// NoAnalysisReporter 让 worker 能区分"没有结果但也不是故障"与"真的失败"。
//
// 单独一个接口而不是让 worker import video 包：worker 不需要知道
// 视频分析的任何细节，只需要一个判定函数。
type NoAnalysisReporter func(err error) bool

// ContentAnalysisWorker 消费 AI 内容分析任务。
//
// 结构照抄 popularityworker.go：3 次重试 + 指数退避 + 耗尽后 Ack 丢弃 + 分级日志。
// 这不是巧合——重试与丢弃的语义必须与既有 worker 完全一致，
// 否则"AI 任务为什么消失了"会变成一个只有 AI 才有的特例问题。
//
// P1 起它真正干活了：analyzer 非 nil 时调用模型并幂等入库；
// 为 nil 时退化成 P0 的行为（只解析并记日志），用于"AI 关闭"的部署。
type ContentAnalysisWorker struct {
	ch    *amqp.Channel
	queue string
	// sleep 是退避等待，默认 time.Sleep。
	//
	// 允许注入的唯一目的是测试：真实退避是 1s+2s+4s，跑满一次要 7 秒，
	// 而"重试耗尽后到底 Ack 还是 Nack"这条最容易写反的逻辑必须被覆盖。
	// 生产路径永远走默认值。
	sleep func(time.Duration)
	// handle 是实际的处理逻辑，由调用方注入。
	//
	// 为什么用函数字段而不是在 process 里写死：P1 的打标逻辑需要模型客户端、
	// 需要数据库，这些依赖的构造属于装配层（cmd/worker/main.go）。
	// 注入后，单测可以在不连 MySQL、不连 MQ 的情况下覆盖
	// "重试与丢弃"这段最容易写错的逻辑。
	handle HandlerFunc
	// analyzer 是 P1 的分析能力；为 nil 表示 AI 关闭，只记日志不落库。
	analyzer Analyzer
	// isNoAnalysis 判定"没有结果但也不是故障"（AI 关闭、视频已删、无文本）。
	// 为 nil 时按"一律是失败"处理，即退化到 P0 的语义。
	isNoAnalysis NoAnalysisReporter
}

// HandlerFunc 处理一条已解析的分析事件。
// 返回 nil 表示成功（消息会被 Ack），返回 error 表示进入重试。
type HandlerFunc func(ctx context.Context, evt rabbitmq.ContentAnalysisEvent) error

func NewContentAnalysisWorker(ch *amqp.Channel, queue string, handle HandlerFunc) *ContentAnalysisWorker {
	return &ContentAnalysisWorker{ch: ch, queue: queue, handle: handle, sleep: time.Sleep}
}

// WithAnalyzer 注入 P1 的分析能力，返回自身以支持链式装配。
//
// isNoAnalysis 允许为 nil：那时所有错误都按"失败"处理（会重试），
// 对 P0 的既有行为完全兼容。
func (w *ContentAnalysisWorker) WithAnalyzer(a Analyzer, isNoAnalysis NoAnalysisReporter) *ContentAnalysisWorker {
	w.analyzer = a
	w.isNoAnalysis = isNoAnalysis
	return w
}

// wait 是 handleDelivery 里的退避等待，走可注入的 sleep（见字段注释）。
func (w *ContentAnalysisWorker) wait(d time.Duration) {
	if w.sleep == nil {
		time.Sleep(d)
		return
	}
	w.sleep(d)
}

func (w *ContentAnalysisWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil {
		return errors.New("content analysis worker is not initialized")
	}
	if w.queue == "" {
		return errors.New("queue is required")
	}

	deliveries, err := w.ch.Consume(
		w.queue,
		"",
		false, // autoAck=false：处理成功才 Ack，失败可以被 broker 重投
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,
	)
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

// handleDelivery 与 popularityworker 采用同一套重试策略。
//
// 为什么是 3 次 + 指数退避（1s/2s/4s）而不是无限重试：模型调用失败大多是
// 瞬时问题（超时、限流），退避几次就能覆盖；而无限重试会在模型侧故障时
// 把队列变成堆积的雪球，最终拖垮 worker 本身——一个可选功能不该有能力
// 影响主流程的健康。
func (w *ContentAnalysisWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	const maxRetries = 3
	for i := 0; i <= maxRetries; i++ {
		select {
		case <-ctx.Done():
			// 进程正在退出：Nack 并 requeue，让消息回到队列交给下一个实例，
			// 而不是丢掉（丢一条 AI 任务本身不致命，但白白丢掉没必要）。
			_ = d.Nack(false, true)
			return
		default:
		}

		if err := w.process(ctx, d.Body); err != nil {
			if i >= maxRetries {
				// Ack 掉即彻底丢弃这条消息（**不会**进死信队列，见下），故为 Error。
				//
				// 为什么是 Ack 而不是 Nack(requeue=false)：后者才走 DLX。
				// 这里的判断是"重试已耗尽"，说明消息本身有问题或上游持续故障，
				// 留在队列里只会无限循环；把它扔进 DLX 也一样没人处理，
				// 只是换个地方堆积。补偿路径是事后的批量补跑脚本，
				// 这也是为什么这条日志必须是 Error 级别——它是唯一的事故痕迹。
				logging.Ctx(ctx).Error("content analysis worker: 重试次数耗尽，消息已丢弃",
					zap.Int("max_retries", maxRetries), zap.Error(err))
				_ = d.Ack(false)
				return
			}
			wait := time.Duration(1<<uint(i)) * time.Second
			// 还在退避重试路径上，故为 Warn。
			logging.Ctx(ctx).Warn("content analysis worker: 处理失败，稍后重试",
				zap.Duration("wait", wait), zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries), zap.Error(err))
			w.wait(wait)
			continue
		}
		_ = d.Ack(false)
		return
	}
}

// process 解析载荷并交给注入的 handler。
//
// 返回 nil 的两种"不算失败"的情况（关键设计，改动前先想清楚）：
//
//  1. **JSON 解析失败**：重试不会让一段坏字节变好。返回 nil 让调用方 Ack，
//     消息被丢弃——这是与 popularityworker 一致的处理（那里也是
//     `if err := json.Unmarshal(...); err != nil { return nil }`）。
//  2. **载荷不完整**（没有 video_id、没有可用文本）：这是"无事可做"，
//     不是故障。让它在退避里耗 7 秒纯属浪费，且会刷出误导性的 Warn。
//
// 反过来说，handler 返回的错误**会**触发重试——那是真正的失败
// （模型超时、数据库暂时写不进去）。
func (w *ContentAnalysisWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.ContentAnalysisEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		// 坏消息无法通过重试修复，故为 Warn 而非 Error：级别要能区分
		// "上游发了坏消息"（一次性、可定位）与"系统持续故障"。
		logging.Ctx(ctx).Warn("content analysis worker: 载荷不是合法 JSON，已丢弃",
			zap.Int("body_bytes", len(body)), zap.Error(err))
		return nil
	}
	if evt.VideoID == 0 {
		// 缺少主键就无法定位任何视频，重试多少次都一样；上游发错消息是
		// 运维需要知道的信号，故为 Warn（而不是静默丢弃）。
		logging.Ctx(ctx).Warn("content analysis worker: 载荷缺少 video_id，已丢弃")
		return nil
	}
	// 只处理认识的分析类型。**这是 P0 与 P1 之间的兼容点**：新增分析类型时
	// 只新增 AnalysisType 的取值，旧 worker 收到不认识的取值时记一条日志后 Ack，
	// 而不是把它当成"处理失败"重试——那会让消息在队列里反复重投直到耗尽重试，
	// 每次都刷一条误导性的 Warn。代价是旧 worker 会丢弃新类型的任务，
	// 这是滚动升级期间可接受的（丢了可以批量补跑，见 contentAnalysisMQ.go 的说明）。
	switch evt.AnalysisType {
	case "":
		// 空类型：无法判断该做什么。属于坏载荷，丢弃并留下痕迹。
		logging.Ctx(ctx).Warn("content analysis worker: 载荷缺少 analysis_type，已丢弃",
			zap.Uint("video_id", evt.VideoID))
		return nil
	case rabbitmq.AnalysisTypeText:
		// 认识，继续走下面的处理。
	default:
		// 不认识的类型不是故障，故为 Info：一条 Warn 会在滚动升级期间被刷屏，
		// 让真正需要看的告警被淹没。
		logging.Ctx(ctx).Info("content analysis worker: 不认识的分析类型，已跳过",
			zap.String("analysis_type", evt.AnalysisType), zap.Uint("video_id", evt.VideoID))
		return nil
	}
	// 这里**曾经**有一条"载荷里没有文本 → 直接跳过"的短路，P1 把它删掉了。
	//
	// 为什么必须删（这是真实联调才发现的问题，别再写回去）：
	// P1 起载荷里**本来就不带**标题/描述（设计上要求 worker 自己回查，
	// 避免把长文本塞进 MQ），于是 `evt.Title == "" && evt.Description == ""`
	// 对**每一条**真实消息都成立。旧代码把它当成"无事可做"，
	// 结果分析器一次都不会被调用：AI 静默失效，日志上只有一句
	// "无文本内容，跳过"，看不出任何异常。
	//
	// 判据必须是"**有没有分析能力**"，而不是"载荷里有没有文本"。
	// 下面两个分支已经精确表达了这件事：
	//   - 有 handler → 交给它；
	//   - 没有 analyzer → 记一条"AI 未启用"（Info，见那里的注释）。
	//
	// 顺带说明 ContentAnalysisEvent 里的 Title/Description：它们是 P0
	// 留下的协议字段，**不再被 worker 使用**，但也不删——
	// 删字段会让"老生产者 + 新消费者"在滚动升级期间解析出错，
	// 与 proto 只增不删是同一条纪律。worker 只回查数据库，不看这两个字段。
	if w.handle != nil {
		return w.handle(ctx, evt)
	}
	if w.analyzer == nil {
		// AI 关闭时的路径：只记日志。
		//
		// 为什么用 Info 而不是 Warn：这是配置决定的正常状态
		// （ai.enabled=false），用 Warn 会让关闭 AI 的部署持续刷告警。
		logging.Ctx(ctx).Info("content analysis worker: AI 未启用，跳过分析",
			zap.String("event_id", evt.EventID),
			zap.Uint("video_id", evt.VideoID),
			zap.String("analysis_type", evt.AnalysisType),
		)
		return nil
	}

	if err := w.analyzer.Analyze(ctx, evt.VideoID); err != nil {
		if w.isNoAnalysis != nil && w.isNoAnalysis(err) {
			// "没有结果但也不是故障"：AI 关闭、视频已被删除、视频没有文本。
			// 这三种情况重试一万次也不会有不同结果，因此直接当成功处理（Ack 丢弃），
			// 并且用 Info 而不是 Warn——它不是故障，重试只会白耗 7 秒退避。
			logging.Ctx(ctx).Info("content analysis worker: 本次无可写入的分析结果，跳过",
				zap.Uint("video_id", evt.VideoID), zap.Error(err))
			return nil
		}
		// 真正的失败（模型超时、非法输出、数据库暂时写不进去）：返回错误，
		// 交给 handleDelivery 的退避重试。级别由那里统一决定。
		return err
	}
	return nil
}
