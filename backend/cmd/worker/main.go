package main

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/aiapp"
	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/db"
	"github.com/handlerzzy/feedsystem/internal/logging"
	mqrabbit "github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/observability"
	"github.com/handlerzzy/feedsystem/internal/social"
	"github.com/handlerzzy/feedsystem/internal/video"
	"github.com/handlerzzy/feedsystem/internal/worker"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// 本进程需要的 MQ 拓扑定义只有唯一来源：internal/middleware/rabbitmq/topology.go。
// 此前这里手抄了一份 16 个常量 + 4 个 declareXxxTopology 函数（约 138 行），
// 内容与 rabbitmq 包逐字节相同，但漏掉了 DeclareDLX —— 于是 worker 先于 API
// 启动时（compose 里 worker 不依赖 api，且两边都是 restart: always，每次重启
// 都会竞争），死信交换机并不存在，被拒绝/过期的消息会被 broker 静默丢弃。

func connectWithRetry(name string, maxRetries int, fn func() error) {
	for i := 0; i < maxRetries; i++ {
		if err := fn(); err == nil {
			return
		}
		wait := time.Duration(1<<i) * time.Second
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		// 仍在退避重试路径上，故为 Warn。
		logging.L().Warn("依赖不可用，稍后重试",
			zap.String("name", name),
			zap.Int("attempt", i+1),
			zap.Int("max_retries", maxRetries),
			zap.Duration("wait", wait))
		time.Sleep(wait)
	}
	// 重试耗尽，进程无法继续（MySQL 连不上），故为 Fatal。
	logging.L().Fatal("超过最大重试次数", zap.String("name", name))
}

// runWorkerWithRetry 为每个 Worker 创建独立 Channel，断开后自动重连。
//
// 参数从 *amqp.Connection 改成 *mqrabbit.RabbitMQ 是关键：原先这里捕获的是
// 启动时那一条连接，一旦它断掉，"5 秒后重试"重试的其实是在同一条死连接上开
// Channel——永远失败，且进程健康、日志只有 Warn，故障就此静默固化。
// 现在每次重试都经由 rmq.NewChannel()，它会先确认连接可用、必要时重新拨号。
func runWorkerWithRetry(ctx context.Context, name string, rmq *mqrabbit.RabbitMQ, fn func(*amqp.Channel) error) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch, err := rmq.NewChannel()
		if err != nil {
			// 5 秒后重连，可自愈，故为 Warn。
			logging.Ctx(ctx).Warn("创建 Channel 失败，5 秒后重试",
				zap.String("name", name), zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}
		if err := ch.Qos(50, 0, false); err != nil {
			// QoS 没设上只影响预取窗口，消费仍继续，故为 Warn。
			logging.Ctx(ctx).Warn("QoS 设置失败", zap.String("name", name), zap.Error(err))
		}

		logging.Ctx(ctx).Info("worker started, consuming", zap.String("name", name))
		if err := fn(ch); err != nil {
			if ctx.Err() != nil {
				ch.Close()
				return
			}
			logging.Ctx(ctx).Warn("消费中断，5 秒后重连",
				zap.String("name", name), zap.Error(err))
		}
		ch.Close()
		time.Sleep(5 * time.Second)
	}
}

func main() {
	// 加载 .env（本地开发）
	if err := godotenv.Load(); err != nil {
		// 这几条早于 logging.Init：L() 未 Init 时返回默认 logger，同样不会 nil。
		logging.L().Info(".env not found; continuing")
	}
	// 加载配置
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/config.yaml"
	}
	logging.L().Info("loading config", zap.String("path", configPath))
	// worker 不使用 JWT，按 RoleWorker 校验：不要求 jwt.secret。
	cfg, usedDefault, err := config.LoadLocalDevForWorker(configPath)
	if err != nil {
		logging.L().Fatal("Failed to load config", zap.Error(err))
	}

	// 尽早初始化日志：越早接管，越少日志散落在默认格式里。
	if err := logging.Init(logging.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	}); err != nil {
		logging.L().Fatal("Failed to init logging", zap.Error(err))
	}
	defer logging.Sync()

	if usedDefault {
		logging.L().Warn("config file not found, using default local config",
			zap.String("path", configPath))
	} else {
		logging.L().Info("config loaded", zap.String("path", configPath))
	}
	// 连接数据库（带重试）
	var sqlDB *gorm.DB
	connectWithRetry("MySQL", 10, func() error {
		var err error
		sqlDB, err = db.NewDB(cfg.Database)
		return err
	})
	defer db.CloseDB(sqlDB)

	// 连接 Redis（用于流行度更新）
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		// 缓存不可用只是关掉 PopularityWorker，进程继续运行，故为 Warn。
		logging.L().Warn("Redis config error (popularity worker disabled)", zap.Error(err))
		cache = nil
	} else {
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := cache.Ping(pingCtx); err != nil {
			logging.L().Warn("Redis not available (popularity worker disabled)", zap.Error(err))
			_ = cache.Close()
			cache = nil
		} else {
			defer cache.Close()
			logging.L().Info("Redis connected (popularity worker enabled)")
		}
	}
	// 生命周期 ctx 提前到这里：MQ 建连现在复用 API 侧同一条路径
	// （NewRabbitMQWithRetry），它的退避等待是可被 ctx 中断的。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 连接 RabbitMQ：复用 API 侧的建连路径，URL 拼接与退避策略不再有第二份实现。
	// 重试次数保持原有的 10 次。
	rmq, err := mqrabbit.NewRabbitMQWithRetry(ctx, &cfg.RabbitMQ, 10,
		func(attempt int, wait time.Duration, err error) {
			// 退避重试中，仍在自愈路径上，故为 Warn。
			logging.L().Warn("RabbitMQ 重试",
				zap.Int("attempt", attempt),
				zap.Int("max_retries", 10),
				zap.Duration("wait", wait),
				zap.Error(err))
		})
	if err != nil {
		// worker 没有 MQ 就没有任何可消费的东西，属启动期不可恢复，故为 Fatal。
		logging.L().Fatal("RabbitMQ 不可用", zap.Error(err))
	}
	defer rmq.Close()

	// 声明拓扑（持久化队列，幂等，声明一次即可）。
	// 与下面"是否启动 PopularityWorker"保持一致：没有 Redis 时该 worker 不会
	// 运行，此时声明 popularity 队列只会让消息堆积而无人消费。
	topologies := []mqrabbit.Topology{
		mqrabbit.SocialTopology(),
		mqrabbit.LikeTopology(),
		mqrabbit.CommentTopology(),
		// AI 内容分析拓扑（P0 新增的第 6 个）。无条件声明：
		// 它既不依赖 Redis 也不依赖 sidecar，handler 没注入时 worker 只记日志。
		// 声明一个暂时没人发消息的队列没有副作用（消费者空转不消耗资源）。
		mqrabbit.ContentAnalysisTopology(),
	}
	if cache != nil {
		topologies = append(topologies, mqrabbit.PopularityTopology())
	}

	// 拓扑声明同样走 NewChannel：它会在连接不可用时重连，而不是拿着启动时
	// 捕获的那条连接碰运气。
	topoCh, err := rmq.NewChannel()
	if err != nil {
		logging.L().Fatal("Failed to open topology channel", zap.Error(err))
	}
	for _, topo := range topologies {
		if err := topo.Declare(topoCh); err != nil {
			logging.L().Fatal("Failed to declare topology",
				zap.String("queue", topo.Queue), zap.Error(err))
		}
	}
	topoCh.Close()

	// 准备 repo
	socialRepo := social.NewSocialRepository(sqlDB)
	videoRepo := video.NewVideoRepository(sqlDB)
	// AI 分析器（P1）。这里的装配遵守两条纪律：
	//
	//  1. **AI 关闭时一切照旧**：开关关闭时 aiapp.NewClient 返回 Noop，
	//     分析器对每次调用都拿到 ErrDisabled，worker 侧把它归为
	//     "没有结果但也不是故障" 并 Ack 跳过——不重试、不写任何数据。
	//  2. **sidecar 不在也不影响 worker 启动**：NewClient 只做本地装配，
	//     gRPC 连接是惰性的（见 internal/ai/client 的注释）。
	//
	// contentAnalyzer 保持**接口类型**的声明：这样当装配失败时它是真正的 nil，
	// 而不是"持有 (*T)(nil) 的非 nil 接口"（router.go 里有同样的教训）。
	var contentAnalyzer worker.Analyzer
	aiClient, aiReason := aiapp.NewClient(cfg.AI)
	// normalize 是 embedding 请求的归一化要求（缺省 true）。
	// 单独取一个局部变量是因为 BackfillOptions 用的是 *bool（见那里的注释）。
	normalize := cfg.AI.EmbeddingNormalizeOr()
	if aiReason != nil {
		// 配置不完整导致的降级（开关开着但地址缺失/非法）。属于运维需要知道的
		// 状态，故为 Warn；worker 继续正常消费，只是不产生分析结果。
		logging.L().Warn("AI 未启用（配置不完整，已降级为 noop）",
			zap.Bool("ai_enabled", cfg.AI.Enabled), zap.Error(aiReason))
	}
	if cfg.AI.Enabled && aiReason == nil {
		contentAnalyzer = video.NewVideoAIAnalyzer(video.AnalyzerOptions{
			Store:         videoRepo,
			Client:        aiClient,
			Model:         cfg.AI.Model,
			PromptVersion: video.PromptVersion,
			MinConfidence: cfg.AI.TagMinConfidence,
			Timeout:       cfg.AI.TimeoutOr(),
		})
		logging.L().Info("AI 内容分析已启用",
			zap.String("model", cfg.AI.Model),
			zap.String("prompt_version", video.PromptVersion),
			zap.Duration("timeout", cfg.AI.TimeoutOr()))
	} else if !cfg.AI.Enabled {
		logging.L().Info("AI 内容分析未启用（ai.enabled=false）")
	}

	// P2 向量回填（§4.1）。三件事一起在这里决定：用哪个模型、什么维度、
	// 以及"要不要跑"。三个条件缺一不可：
	//
	//	ai.enabled=false                → 不跑（零外呼）
	//	没配 embedding 通道（降级 noop） → 不跑（否则每轮都在写一条失败日志）
	//	两者都正常                      → 每 30 秒一轮
	//
	// 注意它**不受 recall_quota 影响**：配额管的是"读路径要不要用向量"，
	// 回填管的是"向量池里有没有东西"。有人把配额临时调成 0 做排查时，
	// 向量池不该跟着停止更新——否则排查结束、配额调回来，还要再等一轮
	// 回填才能恢复语义召回。
	var embedBackfiller worker.EmbeddingBackfiller
	embeddingClient, embedReason := aiapp.NewEmbeddingClient(cfg.AI)
	if embedReason != nil {
		// 配置不完整导致的降级（开关开着但地址缺失/非法），故为 Warn。
		logging.L().Warn("向量回填未启用（embedding 通道配置不完整）", zap.Error(embedReason))
	}
	if cfg.AI.Enabled && embedReason == nil {
		backfillOpts := video.DefaultBackfillOptions()
		backfillOpts.Model = cfg.AI.EmbeddingModelOr()
		backfillOpts.Dim = cfg.AI.EmbeddingDimOr()
		backfillOpts.Normalize = &normalize
		embedBackfiller = video.NewEmbeddingBackfiller(videoRepo, embeddingClient, backfillOpts)
		logging.L().Info("向量回填已启用",
			zap.String("model", backfillOpts.Model),
			zap.Int("dim", backfillOpts.Dim),
			zap.Duration("interval", worker.EmbeddingBackfillInterval),
			zap.Int("batch_limit", backfillOpts.BatchLimit))
	} else {
		logging.L().Info("向量回填未启用",
			zap.Bool("ai_enabled", cfg.AI.Enabled),
			zap.String("reason", ai.StatusFor(embedReason)))
	}

	likeRepo := video.NewLikeRepository(sqlDB)
	commentRepo := video.NewCommentRepository(sqlDB)

	pprofServer, err := observability.NewPprofServer(
		"Worker",
		cfg.ObservabilityConfig.Pprof.Enabled,
		cfg.ObservabilityConfig.Pprof.WorkerAddr,
	)
	if err != nil {
		// pprof 是旁路诊断端口，起不来不影响消费，故为 Warn。
		logging.L().Warn("Failed to start worker pprof server", zap.Error(err))
	}
	if pprofServer != nil {
		defer pprofServer.Close()
	}

	// 每个 Worker 独立 Channel + 自动重连。
	// 用 WaitGroup 做真正的 join：各 worker 的 Run(ctx) 在 ctx 取消时立即返回
	// （只有单条消息的重试退避会延迟退出，最坏 7 秒）。
	var wg sync.WaitGroup
	startWorker := func(name string, fn func(*amqp.Channel) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorkerWithRetry(ctx, name, rmq, fn)
		}()
	}
	startWorker("SocialWorker", func(ch *amqp.Channel) error {
		return worker.NewSocialWorker(ch, socialRepo, mqrabbit.SocialTopology().Queue).Run(ctx)
	})
	startWorker("LikeWorker", func(ch *amqp.Channel) error {
		return worker.NewLikeWorker(ch, likeRepo, videoRepo, mqrabbit.LikeTopology().Queue).Run(ctx)
	})
	startWorker("CommentWorker", func(ch *amqp.Channel) error {
		return worker.NewCommentWorker(ch, commentRepo, videoRepo, mqrabbit.CommentTopology().Queue).Run(ctx)
	})
	// AI 内容分析 worker。P0 注入 nil handler：worker 只解析并记录载荷，
	// 不调用模型、不写库。P1 会在这里传入一个持有 ai.Client 的 handler。
	//
	// 注意它**没有**被 cfg.AI.Enabled 包起来：消费一个空队列不产生任何外呼，
	// 而让拓扑与消费者保持常驻可以让 P1 只改一行（换 handler），
	// 而不必同时改动"worker 是否启动"这个更容易出错的判断。
	// 向量回填循环。它不消费 MQ（没有事件可听，见 worker/embeddingbackfill.go
	// 的说明），因此用 startWorker 的同一套"可重入 + ctx 退出"骨架包一层，
	// 保证进程退出时它能被 join 到。
	if embedBackfiller != nil {
		startWorker("EmbeddingBackfill", func(*amqp.Channel) error {
			worker.NewEmbeddingBackfillLoop(embedBackfiller, worker.EmbeddingBackfillInterval).Run(ctx)
			return nil
		})
		// 进程退出时关闭向量客户端连接（与 chat 客户端分开的两条连接）。
		defer func() { _ = embeddingClient.Close() }()
	}

	startWorker("ContentAnalysisWorker", func(ch *amqp.Channel) error {
		return worker.NewContentAnalysisWorker(
			ch, mqrabbit.ContentAnalysisTopology().Queue, nil).
			WithAnalyzer(contentAnalyzer, video.IsNoAnalysis).
			Run(ctx)
	})
	if cache != nil {
		startWorker("PopularityWorker", func(ch *amqp.Channel) error {
			return worker.NewPopularityWorker(ch, cache, mqrabbit.PopularityTopology().Queue).Run(ctx)
		})
	}

	// 等待退出信号
	<-ctx.Done()
	logging.L().Info("Worker shutting down...")

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
		logging.L().Info("所有 worker 已退出")
	case <-time.After(10 * time.Second):
		// 进程即将退出，未消费完的消息由 broker 重投兜底，故为 Warn。
		logging.L().Warn("worker 未在 10 秒内退出，强制结束")
	}
	logging.L().Info("Worker stopped")
}
