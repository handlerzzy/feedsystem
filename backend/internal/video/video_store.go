package video

import (
	"context"

	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
)

//go:generate mockgen -destination=mock/video_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video VideoStore
//go:generate mockgen -destination=mock/video_existence_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video VideoExistenceStore
//go:generate mockgen -destination=mock/popularity_publisher_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video PopularityPublisher
//go:generate mockgen -destination=mock/content_analysis_publisher_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video ContentAnalysisPublisher

// VideoStore 是 VideoService 通过 repo 调用的视频持久化能力。
//
// 为什么定义接口：VideoService 的业务逻辑（发布前的字段校验、删除时的
// 作者归属校验、详情缓存的读写与回填、热度更新后的缓存处理）与 GORM
// 无关，却因为直接依赖 *VideoRepository 而必须连上 MySQL 才能验证。
// 抽成接口后 service 层可用 mock 单测，无需外部依赖。
//
// 接口按“消费方需要什么”来定义，不是照抄 VideoRepository 的全部方法：
// CreateVideo/CreateMsg/ChangeLikesCount/ChangePopularity/CountByAuthor/
// TotalLikesByAuthor 都不在其中——VideoService 从不调用它们
// （它们由 internal/worker、internal/feed 等消费方使用）。
//
// 注意：PublishWithOutbox 原先由 service 直接使用 *VideoRepository 的私有
// db 字段执行（video_service.go:47）——既越层又无法被 mock。事务下沉进 repo
// 后，「视频 + outbox + 标签」的一致性才可测。
//
// *VideoRepository 隐式实现本接口，因此调用方无需改动。
type VideoStore interface {
	GetByID(ctx context.Context, id uint) (*Video, error)
	DeleteVideo(ctx context.Context, id uint) error
	ListByAuthorID(ctx context.Context, authorID int64) ([]Video, error)
	PublishWithOutbox(ctx context.Context, video *Video) error
}

// VideoExistenceStore 是 LikeService / CommentService 唯一需要的视频能力：
// 确认目标视频存在。两个 service 都只调用 IsExist，因此这里刻意只声明
// 这一个方法——“按消费方需要裁剪”而不是直接复用 VideoStore。
//
// *VideoRepository 隐式实现本接口，因此 internal/http/router.go 无需改动。
type VideoExistenceStore interface {
	IsExist(ctx context.Context, id uint) (bool, error)
}

// PopularityPublisher 是 VideoService / LikeService / CommentService 共用的
// 热度事件发布能力（三个 service 都持有 *rabbitmq.PopularityMQ）。
//
// 为什么定义接口：*rabbitmq.PopularityMQ 只包着一个 *amqp.Channel，测试里
// 无法构造；而“热度 MQ 发布失败则直接改 Redis，且 Redis 失败不得导致整个
// 点赞/评论失败”这条 best-effort 路径正是最需要测试的。抽成接口后可用
// mock 精确控制发布成败。
//
// *rabbitmq.PopularityMQ 隐式实现本接口，因此 internal/http/router.go 无需改动。
type PopularityPublisher interface {
	Update(ctx context.Context, videoID uint, change int64) error
}

// ContentAnalysisPublisher 是 VideoService 发布视频后投递 AI 分析任务的能力。
//
// 为什么定义接口：*rabbitmq.ContentAnalysisMQ 只包着一个 *amqp.Channel，
// 测试里无法构造；而"投递失败只记 Warn、绝不影响发布结果"这条 best-effort
// 路径正是 P1 最需要被守住的性质（发布是承重墙，AI 是锦上添花）。
// 抽成接口后可用 mock 精确控制投递成败，断言发布仍然成功。
//
// *rabbitmq.ContentAnalysisMQ 隐式实现本接口，因此 internal/http/router.go 无需改动。
type ContentAnalysisPublisher interface {
	Request(ctx context.Context, evt rabbitmq.ContentAnalysisEvent) error
}

// 编译期断言：GORM / RabbitMQ 实现必须始终满足这些接口。
// 若方法签名漂移，这里会立刻编译失败，而不是等到某个测试莫名其妙地报错。
var (
	_ VideoStore               = (*VideoRepository)(nil)
	_ VideoExistenceStore      = (*VideoRepository)(nil)
	_ PopularityPublisher      = (*rabbitmq.PopularityMQ)(nil)
	_ ContentAnalysisPublisher = (*rabbitmq.ContentAnalysisMQ)(nil)
)
