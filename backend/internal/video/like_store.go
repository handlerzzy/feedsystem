package video

import (
	"context"

	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
)

//go:generate mockgen -destination=mock/like_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video LikeStore
//go:generate mockgen -destination=mock/like_publisher_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video LikePublisher

// LikeStore 是 LikeService 通过 repo 调用的点赞持久化能力。
//
// 为什么定义接口：LikeService 的业务逻辑（幂等判定、MQ 与同步写之间
// 的取舍）与 GORM 无关，却因为直接依赖 *LikeRepository 而必须连上
// MySQL 才能验证。抽成接口后 service 层可用 mock 单测，无需外部依赖。
//
// 接口按“消费方需要什么”来定义，不是照抄 LikeRepository 的全部方法：
// Like/Unlike/LikeIgnoreDuplicate/DeleteByVideoAndAccount/BatchGetLiked
// 都不在其中——它们没有被 LikeService 调用（LikeIgnoreDuplicate /
// DeleteByVideoAndAccount 属于 internal/worker 的消费端）。
//
// 注意：LikeStore 里的 LikeWithCounts / UnlikeWithCounts 是 MQ 发布失败时的
// 同步回退路径。它们原先以 `s.repo.db.WithContext(ctx).Transaction(...)` 的形式
// 写在 LikeService 内部——service 直接访问 repo 的私有 db 字段，既越层又无法被
// mock。事务下沉进 repo 后，回退路径的幂等语义才可测。
//
// *LikeRepository 隐式实现本接口，因此调用方无需改动。
type LikeStore interface {
	IsLiked(ctx context.Context, videoID, accountID uint) (bool, error)
	ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error)
	LikeWithCounts(ctx context.Context, like *Like) (created bool, err error)
	UnlikeWithCounts(ctx context.Context, videoID, accountID uint) (deleted bool, err error)
}

// LikePublisher 是 LikeService 所需的点赞事件发布能力。
//
// 为什么定义接口：*rabbitmq.LikeMQ 只包着一个 *amqp.Channel，测试里无法
// 构造；而服务里“发布失败则回退同步写”的分支恰恰是最需要测试的路径。
// 抽成接口后可用 mock 强制发布失败，从而驱动回退分支。
//
// *rabbitmq.LikeMQ 隐式实现本接口，因此 internal/http/router.go 无需改动。
type LikePublisher interface {
	Like(ctx context.Context, userID, videoID uint) error
	Unlike(ctx context.Context, userID, videoID uint) error
}

// 编译期断言：GORM / RabbitMQ 实现必须始终满足这些接口。
// 若方法签名漂移，这里会立刻编译失败，而不是等到某个测试莫名其妙地报错。
var (
	_ LikeStore     = (*LikeRepository)(nil)
	_ LikePublisher = (*rabbitmq.LikeMQ)(nil)
)
