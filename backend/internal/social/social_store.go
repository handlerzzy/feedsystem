package social

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
)

//go:generate mockgen -destination=mock/social_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/social SocialStore
//go:generate mockgen -destination=mock/account_lookup_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/social AccountLookup
//go:generate mockgen -destination=mock/social_publisher_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/social SocialPublisher

// SocialStore 是 SocialService 所需的关系持久化能力。
//
// 为什么定义接口：SocialService 的业务规则（不能关注自己、重复关注/重复取关
// 的判定、发 MQ 失败只降级不报错）全部与 GORM 无关，却因为直接依赖
// *SocialRepository 而必须连上 MySQL 才能验证。抽成接口后 service 层可用
// mock 单测，无需任何外部依赖。
//
// 接口按“消费方需要什么”来定义：SocialService 恰好用到了 SocialRepository
// 的全部 7 个方法，因此这里与 repo 的方法集相同；一旦 repo 新增方法而
// service 不用，不应加进本接口。
//
// *SocialRepository 隐式实现本接口，因此调用方无需改动。
type SocialStore interface {
	Follow(ctx context.Context, social *Social) error
	Unfollow(ctx context.Context, social *Social) error
	IsFollowed(ctx context.Context, social *Social) (bool, error)
	GetAllFollowers(ctx context.Context, VloggerID uint) ([]*account.PublicAccount, error)
	GetAllVloggers(ctx context.Context, FollowerID uint) ([]*account.PublicAccount, error)
	CountFollowers(ctx context.Context, vloggerID uint) (int64, error)
	CountVloggers(ctx context.Context, followerID uint) (int64, error)
}

// AccountLookup 是 SocialService 所需的账号存在性校验能力（跨包依赖）。
//
// 为什么定义接口：SocialService 依赖 *account.AccountRepository，但只用它
// 的 FindByID 做“关注者/被关注者必须存在”的校验。按消费方需要裁剪后只剩
// 一个方法，单测里用一个 mock 即可，不必为 social 包引入 account 的真实仓储。
//
// Go 接口是隐式实现，*account.AccountRepository 自动满足本接口，
// 因此 internal/http/router.go 无需改动。
type AccountLookup interface {
	FindByID(ctx context.Context, id uint) (*account.Account, error)
}

// SocialPublisher 是 SocialService 所需的事件发布能力。
//
// 为什么定义接口：MQ 只在关注/取关成功后用于异步通知，且失败只记日志、
// 不影响业务结果（降级路径）。抽成接口后可以在单测里直接构造“MQ 不可用”
// 的场景，而不需要一台 RabbitMQ。
//
// *rabbitmq.SocialMQ 隐式实现本接口，因此调用方无需改动。
type SocialPublisher interface {
	Follow(ctx context.Context, followerID, vloggerID uint) error
	UnFollow(ctx context.Context, followerID, vloggerID uint) error
}

// 编译期断言：真实实现必须始终满足这些接口。
// 若仓储/账号库/MQ 的方法签名漂移，这里会立刻编译失败，
// 而不是等到某个测试莫名其妙地报错。
var (
	_ SocialStore     = (*SocialRepository)(nil)
	_ AccountLookup   = (*account.AccountRepository)(nil)
	_ SocialPublisher = (*rabbitmq.SocialMQ)(nil)
)
