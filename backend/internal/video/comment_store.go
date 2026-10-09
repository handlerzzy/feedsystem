package video

import (
	"context"

	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
)

//go:generate mockgen -destination=mock/comment_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video CommentStore
//go:generate mockgen -destination=mock/comment_publisher_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/video CommentPublisher

// CommentStore 是 CommentService 通过 repo 调用的评论持久化能力。
//
// 为什么定义接口：CommentService 的业务逻辑（发布前的归属/存在性校验、
// 删除时的作者校验、MQ 失败后的同步写回退）与 GORM 无关，却因为直接
// 依赖 *CommentRepository 而必须连上 MySQL 才能验证。抽成接口后 service
// 层可用 mock 单测，无需外部依赖。
//
// 接口按“消费方需要什么”来定义，不是照抄 CommentRepository 的全部方法：
// CreateComment 不在其中，因为 CommentService 从不用它（发布走 MQ，
// 回退路径是自己拼事务插入）；IsExist 也不在其中，因为 service 用
// VideoExistenceStore.IsExist 判断的是视频而不是评论。
//
// 注意：PublishWithPopularity / AccountIDByUsername / CreateMentionNotification
// 原先由 service 直接使用 *CommentRepository 的私有 db 字段执行
// （comment_service.go:68、:149、:165）——既越层又无法被 mock。
// 事务与跨表查询下沉进 repo 后，发布回退路径与 @提及 解析才可测。
//
// *CommentRepository 隐式实现本接口，因此调用方无需改动。
type CommentStore interface {
	GetByID(ctx context.Context, id uint) (*Comment, error)
	DeleteComment(ctx context.Context, comment *Comment) error
	GetAllComments(ctx context.Context, videoID uint) ([]Comment, error)
	PublishWithPopularity(ctx context.Context, comment *Comment) error
	AccountIDByUsername(ctx context.Context, username string) (uint, error)
	CreateMentionNotification(ctx context.Context, n *MentionNotification) error
}

// CommentPublisher 是 CommentService 所需的评论事件发布能力。
//
// 为什么定义接口：*rabbitmq.CommentMQ 只包着一个 *amqp.Channel，测试里
// 无法构造；而服务里“comment MQ 发布失败则回退同步写、popularity MQ
// 发布失败则直接改 Redis”的分支正是最需要测试的路径。抽成接口后可用
// mock 精确控制两条发布路径的成败。
//
// *rabbitmq.CommentMQ 隐式实现本接口，因此 internal/http/router.go 无需改动。
type CommentPublisher interface {
	Publish(ctx context.Context, username string, videoID, authorID uint, content string) error
	Delete(ctx context.Context, commentID uint) error
}

// 编译期断言：GORM / RabbitMQ 实现必须始终满足这些接口。
var (
	_ CommentStore     = (*CommentRepository)(nil)
	_ CommentPublisher = (*rabbitmq.CommentMQ)(nil)
)
