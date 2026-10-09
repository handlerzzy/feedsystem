package http

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/aiapp"
	"github.com/handlerzzy/feedsystem/internal/feed"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/message"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/middleware/ratelimit"
	"github.com/handlerzzy/feedsystem/internal/social"
	"github.com/handlerzzy/feedsystem/internal/video"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"go.uber.org/zap"
)

// registerJSONFieldNames 让 validator 报出的字段名用 json tag（客户端自己写的键），
// 而不是 Go 结构体字段名。
//
// 默认情况下 validator.FieldError.Field() 返回的是 Go 字段名（如 FileSize），
// 拼进 4xx 文案就等于把内部类型结构透给客户端。实测改造前接口会回显：
//
//	Key: 'InitChunkUploadRequest.FileSize' Error:Field validation for
//	    'FileSize' failed on the 'required' tag
//
// 注册之后同一处变成 invalid request body: field "file_size" is required ——
// 既准确（用的是客户端自己的键名）又不含内部信息。
//
// 必须在注册路由之前调用：它是全局 validator 的设置。
func registerJSONFieldNames() {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		// 引擎类型不符只影响文案精度，不影响校验本身，故不致命。
		logging.L().Warn("validator engine type mismatch; binding error messages will use Go field names")
		return
	}
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			// 明确不参与 JSON 的字段不给名字（validator 会跳过它）。
			return ""
		}
		return name
	})
}

// SetRouter 只负责注册路由，不负责启动后台任务（见 tasks.go）。
func SetRouter(deps Deps) *gin.Engine {
	db := deps.DB
	cache := deps.Cache
	rmq := deps.RMQ
	cfg := deps.Config

	// 不用 gin.Default()：它自带 Logger 与 Recovery，但 Logger 直接写 stdout、
	// 格式与项目其余日志不一致，也没有级别与请求关联。
	// 这里换成 gin.New() + 自己的两个中间件 + gin.Recovery()：
	//   RequestID  → 每个请求一个关联标识，写进 ctx 与响应头
	//   AccessLog  → 结构化访问日志，级别按状态码区分（5xx=Error, 4xx=Warn）
	// Recovery 放在 AccessLog 之后，panic 会被记成 500 并出现在访问日志里。
	registerJSONFieldNames()

	r := gin.New()
	r.Use(gin.Recovery(), logging.RequestID(), logging.AccessLog())
	if err := r.SetTrustedProxies(nil); err != nil {
		logging.L().Error("SetTrustedProxies failed", zap.Error(err))
	}
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	// /livez：进程存活，不查依赖（编排层用它做重启判据）
	// /readyz：依赖就绪探测，DB 不可用 503，Redis/MQ 不可用 200 + degraded
	r.GET("/livez", livezHandler)
	r.GET("/readyz", readyzHandler(deps))
	r.Static("/static", "./.run/uploads")
	// rate_limit
	loginLimiter := ratelimit.Limit(cache, "account_login", 10, time.Minute, ratelimit.KeyByIP)
	registerLimiter := ratelimit.Limit(cache, "account_register", 5, time.Hour, ratelimit.KeyByIP)
	changePwdLimiter := ratelimit.Limit(cache, "account_change_pwd", 5, time.Hour, ratelimit.KeyByAccount)

	likeLimiter := ratelimit.Limit(cache, "like_write", 30, time.Minute, ratelimit.KeyByAccount)
	commentLimiter := ratelimit.Limit(cache, "comment_write", 10, time.Minute, ratelimit.KeyByAccount)
	socialLimiter := ratelimit.Limit(cache, "social_write", 20, time.Minute, ratelimit.KeyByAccount)

	// account
	accountRepository := account.NewAccountRepository(db)
	accountService := account.NewAccountService(accountRepository, cache)
	accountHandler := account.NewAccountHandler(accountService)
	accountGroup := r.Group("/account")
	{
		accountGroup.POST("/register", registerLimiter, accountHandler.CreateAccount)
		accountGroup.POST("/login", loginLimiter, accountHandler.Login)
		accountGroup.POST("/findByID", accountHandler.FindByID)
		accountGroup.POST("/findByUsername", accountHandler.FindByUsername)
		accountGroup.POST("/refresh", accountHandler.Refresh)
	}
	protectedAccountGroup := accountGroup.Group("")
	protectedAccountGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedAccountGroup.POST("/changePassword", changePwdLimiter, accountHandler.ChangePassword)
		protectedAccountGroup.POST("/logout", accountHandler.Logout)
		protectedAccountGroup.POST("/rename", accountHandler.Rename)
		protectedAccountGroup.POST("/uploadAvatar", accountHandler.UploadAvatar)
		protectedAccountGroup.POST("/updateProfile", accountHandler.UpdateProfile)
	}
	// video
	videoRepository := video.NewVideoRepository(db)
	// 以下四处都用**接口类型**的变量声明再赋值。
	//
	// 不要改回 `popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)` 再在失败分支
	// 里写 `popularityMQ = nil`：那样得到的是 (*rabbitmq.PopularityMQ)(nil)，
	// 一旦存进接口字段，service 里的 `if s.popularityMQ != nil` 就为真，
	// 于是真的会去调用 nil 接收者的方法。目前不出事只是因为 MQ 方法自带
	// `if m == nil` 保护并返回 error——那是巧合，不是契约；谁把它删掉，
	// MQ 不可用的部署就会 panic。
	var popularityMQ video.PopularityPublisher
	if mq, err := rabbitmq.NewPopularityMQ(rmq); err != nil {
		// 路由装配期没有请求 ctx，用全局 logger；失败即降级运行，故为 Warn。
		logging.L().Warn("PopularityMQ init failed (mq disabled)", zap.Error(err))
	} else {
		popularityMQ = mq
	}
	videoService := video.NewVideoService(videoRepository, cache, popularityMQ)
	// AI 分析任务的投递器（P1）。装配条件与别的可选依赖一致：
	// 拿不到就跳过注入并记 Warn，绝不让"AI 可选功能不可用"影响 API 启动。
	//
	// 注意它**不检查 ai.enabled**：开关决定的是"要不要真的调模型"，
	// 而这里只是"要不要投递一条 MQ 消息"。让投递也受开关控制会多一处判断，
	// 而 worker 侧拿到消息后同样会因为 AI 关闭而静默跳过（见 worker）。
	// 一处判断（worker 侧）比两处更容易保证一致。
	if mq, err := rabbitmq.NewContentAnalysisMQ(rmq); err != nil {
		// 装配期没有请求 ctx，用全局 logger；失败即降级（发布不打标），故为 Warn。
		logging.L().Warn("ContentAnalysisMQ init failed (AI tagging disabled)", zap.Error(err))
	} else {
		videoService.WithContentAnalysis(mq)
	}
	videoHandler := video.NewVideoHandler(videoService, accountService)
	chunkHandler := video.NewChunkUploadHandler(cache)
	videoGroup := r.Group("/video")
	{
		videoGroup.POST("/listByAuthorID", videoHandler.ListByAuthorID)
		videoGroup.POST("/getDetail", videoHandler.GetDetail)
	}
	protectedVideoGroup := videoGroup.Group("")
	protectedVideoGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedVideoGroup.POST("/uploadVideo", videoHandler.UploadVideo)
		protectedVideoGroup.POST("/uploadCover", videoHandler.UploadCover)
		protectedVideoGroup.POST("/publish", videoHandler.PublishVideo)
		protectedVideoGroup.POST("/chunk/init", chunkHandler.InitChunkUpload)
		protectedVideoGroup.POST("/chunk/upload", chunkHandler.UploadChunk)
		protectedVideoGroup.POST("/chunk/status", chunkHandler.ChunkStatus)
		protectedVideoGroup.POST("/chunk/complete", chunkHandler.CompleteChunkUpload)
	}
	// like
	var likeMQ video.LikePublisher
	if mq, err := rabbitmq.NewLikeMQ(rmq); err != nil {
		logging.L().Warn("LikeMQ init failed (mq disabled)", zap.Error(err))
	} else {
		likeMQ = mq
	}
	likeRepository := video.NewLikeRepository(db)
	likeService := video.NewLikeService(likeRepository, videoRepository, cache, likeMQ, popularityMQ)
	likeHandler := video.NewLikeHandler(likeService)
	likeGroup := r.Group("/like")
	protectedLikeGroup := likeGroup.Group("")
	protectedLikeGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedLikeGroup.POST("/like", likeLimiter, likeHandler.Like)
		protectedLikeGroup.POST("/unlike", likeLimiter, likeHandler.Unlike)
		protectedLikeGroup.POST("/isLiked", likeHandler.IsLiked)
		protectedLikeGroup.POST("/listMyLikedVideos", likeHandler.ListMyLikedVideos)
	}
	// comment
	commentRepository := video.NewCommentRepository(db)
	var commentMQ video.CommentPublisher
	if mq, err := rabbitmq.NewCommentMQ(rmq); err != nil {
		logging.L().Warn("CommentMQ init failed (mq disabled)", zap.Error(err))
	} else {
		commentMQ = mq
	}
	commentService := video.NewCommentService(commentRepository, videoRepository, cache, commentMQ, popularityMQ)
	commentHandler := video.NewCommentHandler(commentService, accountService)
	commentGroup := r.Group("/comment")
	{
		commentGroup.POST("/listAll", commentHandler.GetAllComments)
	}
	protectedCommentGroup := commentGroup.Group("")
	protectedCommentGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedCommentGroup.POST("/publish", commentLimiter, commentHandler.PublishComment)
		protectedCommentGroup.POST("/delete", commentLimiter, commentHandler.DeleteComment)
	}
	// social
	var socialMQ social.SocialPublisher
	if mq, err := rabbitmq.NewSocialMQ(rmq); err != nil {
		logging.L().Warn("SocialMQ init failed (mq disabled)", zap.Error(err))
	} else {
		socialMQ = mq
	}
	socialRepository := social.NewSocialRepository(db)
	socialService := social.NewSocialService(socialRepository, accountRepository, socialMQ, cache)
	socialHandler := social.NewSocialHandler(socialService)
	socialGroup := r.Group("/social")
	protectedSocialGroup := socialGroup.Group("")
	protectedSocialGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedSocialGroup.POST("/follow", socialLimiter, socialHandler.Follow)
		protectedSocialGroup.POST("/unfollow", socialLimiter, socialHandler.Unfollow)
		protectedSocialGroup.POST("/getAllFollowers", socialHandler.GetAllFollowers)
		protectedSocialGroup.POST("/getAllVloggers", socialHandler.GetAllVloggers)
		protectedSocialGroup.POST("/getCounts", socialHandler.GetCounts)
	}

	// feed
	feedRepository := feed.NewFeedRepository(db)
	// 注入 AI 摘要查询能力（P1）。它是**可选**依赖：
	// 表为空、AI 关闭、甚至将来这张表被整体删除，Feed 都照常返回，
	// 只是不携带 summary 字段（见 feed.FeedVideoItem.Summary 的 omitempty）。
	// AI 通道（P0/P1/P2）。**两个客户端都只做本地装配**：gRPC 连接是惰性的，
	// sidecar 不在也不影响 API 启动（见 internal/ai/client 的注释）。
	//
	// 为什么把这段放在 feed 之前：P2 的语义路与精排都要用它们，
	// 而装配失败的降级策略必须与 P1 的打标完全一致（Warn + 继续）。
	aiClient, aiReason := aiapp.NewClient(cfg.AI)
	if aiReason != nil {
		// 配置不完整导致的降级（开关开着但地址缺失/非法）。运维需要知道，故为 Warn。
		logging.L().Warn("AI 未启用（配置不完整，已降级为 noop）",
			zap.Bool("ai_enabled", cfg.AI.Enabled), zap.Error(aiReason))
	}
	embeddingClient, embedReason := aiapp.NewEmbeddingClient(cfg.AI)
	if embedReason != nil {
		logging.L().Warn("向量通道未启用（配置不完整，已降级为 noop）",
			zap.Bool("ai_enabled", cfg.AI.Enabled), zap.Error(embedReason))
	}
	defer func() {
		// 关闭连接。允许重复调用：Noop 的 Close 也是空实现。
		_ = aiClient.Close()
		_ = embeddingClient.Close()
	}()

	feedService := feed.NewFeedService(feedRepository, likeRepository, cache).
		WithSummaryLookup(videoRepository).
		WithReasonSignals(videoRepository)
	// P2 语义召回：**三关都在这里判**（总开关 / 语义配额 / 向量客户端可用）。
	//
	// 只在真的会用到时才注入 store：不注入时整条语义路（用户画像、读向量、
	// 算相似度、探索查询）一次都不执行，"关掉 = 零开销"因此是结构性的。
	// 装配信息用 Info 打出来——"语义路到底开没开"是排查 Feed 行为时
	// 第一个要确认的事实，而它不能靠读配置推断（配额越界会被夹成 0、
	// 向量客户端可能降级成 Noop）。
	recallQuota := cfg.AI.RecallQuotaOr()
	exploreQuota := cfg.AI.ExploreQuotaOr()
	if cfg.AI.Enabled && (recallQuota > 0 || exploreQuota > 0) {
		feedService.WithSemanticRecall(feed.SemanticRecallOptions{
			Store: &feedSemanticStore{videos: videoRepository, likes: likeRepository},
			Model: cfg.AI.EmbeddingModelOr(),
			Dim:   cfg.AI.EmbeddingDimOr(),
			Quota: recallQuota,
			Addons: feed.SemanticAddons{
				ExploreQuota: exploreQuota,
			},
		})
		logging.L().Info("P2 语义召回已启用",
			zap.Float64("recall_quota", recallQuota),
			zap.Float64("explore_quota", exploreQuota),
			zap.String("embedding_model", cfg.AI.EmbeddingModelOr()),
			zap.Int("embedding_dim", cfg.AI.EmbeddingDimOr()),
			zap.Duration("semantic_timeout", feed.DefaultSemanticTimeout))
	} else {
		// 刻意把"配置里的配额"与"实际生效的配额"分开打：
		// 总开关关闭时配置里仍写着 0.3/0.1，而实际生效是 0——
		// 只看一个数字会让人误以为语义路是开着的（排查 Feed 行为时
		// 第一个要确认的事实就是这个，而它不能靠读配置推断）。
		logging.L().Info("P2 语义召回未启用",
			zap.Bool("ai_enabled", cfg.AI.Enabled),
			zap.Float64("configured_recall_quota", recallQuota),
			zap.Float64("configured_explore_quota", exploreQuota),
			zap.String("effective", "recall_quota=0 explore_quota=0（语义路与探索路都不存在，零额外查询）"))
	}
	// P2 LLM 精排（默认关闭）。aiReason 非 nil 时模型客户端是 Noop，
	// 但这里仍然允许装配：Noop 会返回 ErrDisabled，feed 包会把它记成
	// "ai_disabled" 降级。**不**在这里替它判断，是为了让"降级率"这个
	// 指标反映真实的调用结果，而不是被两处判断各解释一遍。
	if reranker, err := aiapp.RerankClient(cfg.AI, aiClient); err != nil {
		logging.L().Warn("LLM 精排未启用（配置不完整）", zap.Error(err))
	} else if reranker != nil {
		feedService.WithRerank(feed.RerankOptions{
			Client:  reranker,
			Enabled: true,
			Timeout: cfg.AI.RerankTimeoutOr(),
			TopN:    cfg.AI.RerankCandidatesOr(),
		})
		logging.L().Info("P2 LLM 精排已启用",
			zap.Duration("timeout", cfg.AI.RerankTimeoutOr()),
			zap.Int("candidates", cfg.AI.RerankCandidatesOr()),
			zap.String("model", cfg.AI.Model))
	}
	feedHandler := feed.NewFeedHandler(feedService)
	feedGroup := r.Group("/feed")
	feedGroup.Use(jwt.SoftJWTAuth(accountRepository, cache))
	{
		feedGroup.POST("/listLatest", feedHandler.ListLatest)
		feedGroup.POST("/listLikesCount", feedHandler.ListLikesCount)
		feedGroup.POST("/listByPopularity", feedHandler.ListByPopularity)
		feedGroup.POST("/listByTag", feedHandler.ListByTag)
	}
	protectedFeedGroup := feedGroup.Group("")
	protectedFeedGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedFeedGroup.POST("/listByFollowing", feedHandler.ListByFollowing)
	}
	// message
	messageRepo := message.NewRepository(db)
	messageService := message.NewService(messageRepo, accountRepository)
	messageHandler := message.NewHandler(messageService)
	messageGroup := r.Group("/message")
	protectedMessageGroup := messageGroup.Group("")
	protectedMessageGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedMessageGroup.POST("/send", messageHandler.Send)
		protectedMessageGroup.POST("/list", messageHandler.List)
	}
	// SSE notification
	// 拓扑声明与消费者启动已移至 tasks.go；这里的 hub 由 cmd/main.go 注入，保证全局唯一。
	sseHub := deps.SSEHub
	notifGroup := r.Group("/notification")
	notifGroup.Use(sseHub.SSERequireAuth(accountRepository, cache))
	sseHub.RegisterRoutes(r, notifGroup)

	return r
}

// feedSemanticStore 把两个仓储拼成 feed.VectorRecallStore。
//
// 为什么需要这个适配器：语义召回需要"视频向量"（VideoRepository）
// 与"用户最近点赞"（LikeRepository）两种能力，而它们分属两个类型。
// 让其中一个去持有另一个会制造一条没有必要的依赖（LikeRepository
// 不该认识向量表），所以在装配层拼起来——这里正是唯一同时认识两者的地方。
//
// 它是纯转发的薄壳：没有逻辑，因此不需要为它写单测，
// 真正的逻辑（召回、过滤、打分）都在 feed 包里被覆盖。
type feedSemanticStore struct {
	videos *video.VideoRepository
	likes  *video.LikeRepository
}

func (s *feedSemanticStore) ListVectorCandidates(ctx context.Context, model string, dim int, limit int) ([]video.VectorCandidate, error) {
	return s.videos.ListVectorCandidates(ctx, model, dim, limit)
}

func (s *feedSemanticStore) LoadVectors(ctx context.Context, videoIDs []uint, model string, dim int) (map[uint][]float32, error) {
	return s.videos.LoadVectors(ctx, videoIDs, model, dim)
}

func (s *feedSemanticStore) RecentLikedVideoIDs(ctx context.Context, accountID uint, limit int) ([]uint, error) {
	return s.likes.RecentLikedVideoIDs(ctx, accountID, limit)
}

func (s *feedSemanticStore) ListEmbeddingModels(ctx context.Context, dim int, limit int) ([]video.EmbeddingModelStat, error) {
	return s.videos.ListEmbeddingModels(ctx, dim, limit)
}

// 编译期断言：适配器必须满足消费方声明的接口。
var _ feed.VectorRecallStore = (*feedSemanticStore)(nil)
