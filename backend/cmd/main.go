package main

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/aiapp"
	"github.com/handlerzzy/feedsystem/internal/auth"
	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/db"
	apphttp "github.com/handlerzzy/feedsystem/internal/http"
	"github.com/handlerzzy/feedsystem/internal/logging"
	rabbitmq "github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/observability"
	"github.com/handlerzzy/feedsystem/internal/worker"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

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
	cfg, usedDefault, err := config.LoadLocalDev(configPath)
	if err != nil {
		logging.L().Fatal("Failed to load config", zap.Error(err))
	}

	// 尽早初始化日志：越早接管，越少日志散落在默认格式里。
	// 配置非法时直接退出而不是继续用默认值——否则运维会以为配置生效了。
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

	// JWT 密钥与双 token 有效期：配置已校验，这里注入到使用方。
	if err := auth.SetSecret(cfg.JWT.Secret); err != nil {
		logging.L().Fatal("Failed to init JWT secret", zap.Error(err))
	}
	if err := auth.SetAccessTokenTTL(cfg.JWT.AccessTokenTTL); err != nil {
		logging.L().Fatal("Failed to init JWT access ttl", zap.Error(err))
	}
	if err := account.SetRefreshTokenTTL(cfg.JWT.RefreshTokenTTL); err != nil {
		logging.L().Fatal("Failed to init refresh token ttl", zap.Error(err))
	}
	logging.L().Info("JWT configured",
		zap.Duration("access_ttl", cfg.JWT.AccessTokenTTL),
		zap.Duration("refresh_ttl", cfg.JWT.RefreshTokenTTL))

	// 连接数据库
	// 需要看数据库配置时临时打开：logging.L().Debug("database config", zap.Any("database", cfg.Database))
	sqlDB, err := db.NewDB(cfg.Database)
	if err != nil {
		logging.L().Fatal("Failed to connect database", zap.Error(err))
	}
	if err := db.AutoMigrate(sqlDB); err != nil {
		logging.L().Fatal("Failed to auto migrate database", zap.Error(err))
	}
	defer db.CloseDB(sqlDB)

	// 连接 Redis (可选，用于缓存)
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		// 缓存不可用只关闭缓存，API 继续服务，故为 Warn。
		logging.L().Warn("Redis config error (cache disabled)", zap.Error(err))
		cache = nil
	} else {
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := cache.Ping(pingCtx); err != nil {
			logging.L().Warn("Redis not available (cache disabled)", zap.Error(err))
			_ = cache.Close()
			cache = nil
		} else {
			defer cache.Close()
			logging.L().Info("Redis connected (cache enabled)")
		}
	}

	// 连接 RabbitMQ (可选，用于消息队列)
	// 启动期指数退避重试：API 与 MQ 同时启动时避免"先起者永久降级"。
	// 代价：MQ 不可用时启动最多多等约 31 秒（1+2+4+8+16）。
	// 未覆盖：运行中 channel 断开的自动重连（阶段三）。
	rmq, err := rabbitmq.NewRabbitMQWithRetry(context.Background(), &cfg.RabbitMQ, 5,
		func(attempt int, wait time.Duration, err error) {
			// 退避重试中，仍在自愈路径上，故为 Warn。
			logging.L().Warn("RabbitMQ 重试",
				zap.Int("attempt", attempt),
				zap.Int("max_retries", 5),
				zap.Duration("wait", wait),
				zap.Error(err))
		})
	if err != nil {
		logging.L().Warn("RabbitMQ 不可用（降级运行，SSE/通知关闭）", zap.Error(err))
		rmq = nil
	} else {
		defer rmq.Close()
		logging.L().Info("RabbitMQ connected")
	}
	// Pprof
	pprofServer, err := observability.NewPprofServer(
		"API",
		cfg.ObservabilityConfig.Pprof.Enabled,
		cfg.ObservabilityConfig.Pprof.ApiAddr,
	)
	if err != nil {
		// pprof 是旁路诊断端口，起不来不影响 API，故为 Warn。
		logging.L().Warn("Failed to start API pprof server", zap.Error(err))
	}
	if pprofServer != nil {
		defer pprofServer.Close()
	}

	// 设置路由
	// 启用跨副本扇出：推送经 Redis Pub/Sub 广播，每个副本各自投给自己持有的
	// SSE 连接。多副本时 notification 消费者是竞争消费的，没有这一步的话，
	// 消息落到哪个副本就只有那个副本上的客户端能收到（见 SSEHub.Push）。
	sseHub := worker.NewSSEHub(sqlDB).WithFanout(cache)
	deps := apphttp.Deps{DB: sqlDB, Cache: cache, RMQ: rmq, SSEHub: sseHub, Config: cfg}

	// AI 客户端装配：P0 只装配 + 探活，不参与任何业务（打标见 P1）。
	//
	// 为什么现在就要装配：P0 的交付标准之一是"把 AI 打开，能跑通一条最小
	// 端到端链路"。没有这一步，"开关 -> 实现 -> sidecar"这条路径在运行期
	// 从未被执行过——P1 第一次使用时才发现问题，代价要大得多。
	//
	// 注意这里**不构造 sidecar 的依赖**：不 import grpc 之外的东西、
	// 不拨号（grpc.NewClient 是惰性的），因此 sidecar 没起也不影响启动。
	// 唯一的网络动作是下面那个带 500ms 上限的探活，且失败只记日志。
	// 生命周期 ctx 提前到这里：AI 探活与下面的后台任务共用它，
	// 进程收到 SIGTERM 时探活也会被取消。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	aiClient, aiReason := aiapp.NewClient(cfg.AI)
	if aiReason != nil {
		// 配置问题导致的降级（开关开着但地址缺失/非法）。属于运维需要知道的
		// 状态转变，故为 Warn；进程继续正常服务。
		logging.L().Warn("AI 未启用（配置不完整，已降级为 noop）",
			zap.Bool("ai_enabled", cfg.AI.Enabled), zap.Error(aiReason))
	}
	defer func() { _ = aiClient.Close() }()
	if cfg.AI.Enabled && aiReason == nil {
		logStartupAIProbe(ctx, aiClient, cfg.AI)
	}

	r := apphttp.SetRouter(deps)
	g, err := apphttp.StartBackgroundTasks(ctx, deps)
	if err != nil {
		// 这里的 err 只表示启动参数不合法，即后台任务一个都没起来，故为 Error。
		logging.L().Error("后台任务启动失败", zap.Error(err))
	}

	// 刻意不设 ReadTimeout：上传接口上限 200MB，ReadTimeout 会误杀弱网上传。
	// 只用 ReadHeaderTimeout + WriteTimeout + IdleTimeout 提供基本保护。
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		// ErrServerClosed 是 Shutdown 触发的正常返回，不能当成失败。
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.L().Fatal("Failed to run server", zap.Error(err))
		}
	}()

	logging.L().Info("Server is running", zap.Int("port", cfg.Server.Port))

	// 先停止接入，再等在途请求结束；随后 main 返回，defer 链（db/cache/rmq/pprof Close）正常执行。
	<-ctx.Done()
	stop()
	logging.L().Info("收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 在途请求会被强行中断，客户端可见失败，故为 Error。
		logging.L().Error("优雅关闭未在超时内完成", zap.Error(err))
	}

	// 后台任务 join（带超时）：各 worker 的重试退避最坏 7 秒，10 秒足够。
	if g != nil {
		done := make(chan error, 1)
		go func() { done <- g.Wait() }()
		select {
		case err := <-done:
			// 级别按结果分：任务带错误收尾要能一眼看见，正常退出只是生命周期事件。
			if err != nil {
				logging.L().Error("后台任务退出时返回错误", zap.Error(err))
			} else {
				logging.L().Info("后台任务已全部退出")
			}
		case <-time.After(10 * time.Second):
			// 进程即将退出，未 join 上的消息由 outbox 僵尸回收/broker 重投兜底，故为 Warn。
			logging.L().Warn("后台任务未在 10 秒内退出，强制结束")
		}
	}

	logging.L().Info("已退出")
}

// startupAIProbeTimeout 是启动期探活的硬上限。
//
// 为什么必须这么短：探活发生在进程启动路径上，而 sidecar 只是可选的旁路依赖。
// 一个连不上的 sidecar 绝不能拖慢 API 的启动——500ms 足够覆盖同机/同 compose
// 网络的连通性判断，又短到"就算对端黑洞也感觉不到"。
const startupAIProbeTimeout = 500 * time.Millisecond

// logStartupAIProbe 在开启 AI 时做**一次**健康探活，把结果写进启动日志。
//
// 它的价值不在于"检查"，而在于把三件在运行期才知道的事情变得可见：
//  1. sidecar 到底在不在（不在是正常的，但要能从日志里看出来）；
//  2. sidecar 是否真的提供服务（活着但没配 key 时 serving=false，
//     这种情况必须显式区分——否则会以为是网络问题）；
//  3. 一次真实 gRPC 往返是否成功（协议、字段、端口都通了）。
//
// 刻意只探一次、不轮询：AI 是旁路，周期性探活会制造噪音；
// 真正的健康判断应该由使用它的功能在调用时自然得到。
func logStartupAIProbe(ctx context.Context, c ai.Client, cfg config.AIConfig) {
	probeCtx, cancel := context.WithTimeout(ctx, startupAIProbeTimeout)
	defer cancel()

	h, err := c.Health(probeCtx)
	switch {
	case err != nil:
		// sidecar 不可达是**预期内**的状态（P0 的验收要求就是"sidecar 不在时
		// API 照常服务"），故为 Warn 而不是 Error：Error 会让人以为服务坏了。
		logging.L().Warn("AI sidecar 不可达，AI 功能将静默降级",
			zap.String("gateway_addr", cfg.GatewayAddr), zap.String("status", ai.StatusFor(err)))
	case !h.Serving:
		// 连上了但对方声明不提供服务（总开关关着 / 没配 provider key）。
		// 这与"连不上"是两件事，日志必须能区分，故单独一条。
		logging.L().Warn("AI sidecar 已连接但不提供服务",
			zap.String("gateway_addr", cfg.GatewayAddr),
			zap.String("version", h.Version), zap.String("detail", h.Detail))
	default:
		// 走到这里说明整条链路（开关 -> 客户端 -> 协议 -> sidecar -> provider 配置）
		// 都是通的，属于启动期值得记录的状态，故为 Info。
		logging.L().Info("AI sidecar 就绪",
			zap.String("gateway_addr", cfg.GatewayAddr),
			zap.String("version", h.Version), zap.Strings("providers", h.Providers))
	}
}
