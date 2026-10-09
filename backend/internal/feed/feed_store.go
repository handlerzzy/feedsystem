package feed

import (
	"context"
	"time"

	"github.com/handlerzzy/feedsystem/internal/video"
)

//go:generate mockgen -destination=mock/feed_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/feed FeedStore
//go:generate mockgen -destination=mock/like_lookup_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/feed LikeLookup
//go:generate mockgen -destination=mock/summary_lookup_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/feed SummaryLookup
//go:generate mockgen -destination=mock/vector_recall_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/feed VectorRecallStore

// FeedStore 是 FeedService 所需的视频持久化能力。
//
// 为什么定义接口：FeedService 是缓存层级最复杂的 service（L1 本地缓存 /
// L2 Redis / L3 MySQL + 冷热双写 + 游标分页 + singleflight），这些编排逻辑
// 与 GORM 完全无关，却因为直接依赖 *FeedRepository 而必须连上 MySQL 才能验证。
// 抽成接口后 service 层可用 mock 单测：可以精确断言“某个 ID 是否落到了 L3”
// “冷热拼接时给数据库的游标是什么”，这些断言在真实 MySQL 上无法稳定构造。
//
// 接口按“消费方需要什么”定义。这里恰好与 FeedRepository 的方法集合一致：
// ListLatest、ListLikesCountWithCursor、ListByFollowing、ListByPopularity、
// GetByIDs、ListByTag 全部被 FeedService 用到，没有可以裁剪的冗余方法。
//
// *FeedRepository 隐式实现本接口，因此 internal/http/router.go 无需改动。
type FeedStore interface {
	ListLatest(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error)
	ListLikesCountWithCursor(ctx context.Context, limit int, cursor *LikesCountCursor) ([]*video.Video, error)
	ListByFollowing(ctx context.Context, limit int, viewerAccountID uint, latestBefore time.Time) ([]*video.Video, error)
	ListByPopularity(ctx context.Context, limit int, popularityBefore int64, timeBefore time.Time, idBefore uint) ([]*video.Video, error)
	GetByIDs(ctx context.Context, ids []uint) ([]*video.Video, error)
	ListByTag(ctx context.Context, tagName string, limit int) ([]*video.Video, error)
	// ListFreshLowEngagement 是 P2 冷启动探索通道的数据来源（新且冷）。
	//
	// 为什么单独一个方法而不是复用 ListLatest：判据不同。ListLatest 是
	// "最新的"，探索要的是"新的**且**还没什么人互动过的"——后者在时间段内
	// 要按互动量过滤，是一个不同的查询，硬塞进 ListLatest 会让它的语义变模糊
	// （而 ListLatest 的语义是红线，见 P2 §3）。
	ListFreshLowEngagement(ctx context.Context, limit int, freshness time.Duration, maxPopularity int64, excludeAuthorID uint) ([]*video.Video, error)
}

// FreshIDLister 是"取最新一批视频 ID"的能力，P2 用它把语义位让给时序路。
//
// 单独一个小接口而不是塞进 VectorRecallStore：它既不是向量能力
// （不读向量），也不在语义路的必需项里——拿不到它时语义路照常工作，
// 只是不再排除新内容。**允许缺省的东西必须能被单独判断**，
// 否则"要不要注入"就变成全有或全无。
type FreshIDLister interface {
	NewestVideoIDs(ctx context.Context, limit int) ([]uint, error)
}

// VectorRecallStore 是 P2 语义召回所需的向量能力（**可选**接口）。
//
// 为什么单独一个接口、而不是往 FeedStore 上加方法：
//
//  1. **它是可缺省的**。P2 的验收里有一条硬要求——向量不可用时 Feed 必须
//     与今天逐字节一致。把"必需能力"与"可选能力"放在同一个接口里，
//     会让"没配向量库"变成"FeedService 构造不出来"，也就是把锦上添花
//     变成承重墙（这是本项目最优先的一条纪律）。
//  2. **它的实现方只需要三个方法**，而 FeedStore 有七个。分开之后，
//     给语义路写假实现的成本很低——这是它被单测覆盖的前提。
//  3. 装配层只在"AI 打开且语义配额 > 0"时才注入它。不注入时，
//     整个语义路（含用户画像、向量读取、相似度计算）一次都不会执行，
//     "开关关掉 = 零开销"因此是**结构上成立**的，而不是靠早退判断。
//
// *video.VideoRepository 与 *video.LikeRepository **共同**满足它，
// 因此装配层需要传两个对象（见 WithVectorRecall 的注释）。
type VectorRecallStore interface {
	// ListVectorCandidates 列出最近的、且已入库向量的候选视频。
	ListVectorCandidates(ctx context.Context, model string, dim int, limit int) ([]video.VectorCandidate, error)
	// LoadVectors 读回指定视频的向量（按 model/dim 过滤）。
	LoadVectors(ctx context.Context, videoIDs []uint, model string, dim int) (map[uint][]float32, error)
	// RecentLikedVideoIDs 返回某账号最近点赞的视频 ID——用户兴趣向量的来源。
	RecentLikedVideoIDs(ctx context.Context, accountID uint, limit int) ([]uint, error)
	// ListEmbeddingModels 列出库里**实际存在**的向量空间（按最近写入倒序）。
	//
	// 为什么读路径需要它：向量的 model 是**提供方**决定的，不是配置决定的
	// （离线演示时 sidecar 用的是词法编码器，它回的是 faux-lexical，
	// 而配置里写的是 text-embedding-3-small）。按配置名去读会一条都读不到，
	// 表现为"语义路静默失效"——最难查的一类故障。
	ListEmbeddingModels(ctx context.Context, dim int, limit int) ([]video.EmbeddingModelStat, error)
}

// LikeLookup 是 FeedService 所需的点赞查询能力，也是本包内的“窄接口”。
//
// 为什么在 feed 包里声明描述 video 包类型的接口：FeedService 只用
// video.LikeRepository 的 BatchGetLiked 一个方法（用于填充 FeedVideoItem.IsLiked），
// 却因此把整个 video.LikeRepository（含 Like/Unlike/IsLiked/ListLikedVideos）
// 拉进了依赖面，单测必须构造带真实 gorm.DB 的仓库。
// Go 的接口是隐式实现的，所以消费方声明接口是合法且惯用的做法：
// *video.LikeRepository 自动满足 LikeLookup，internal/http/router.go 无需改动。
//
// 刻意不包含 Like / Unlike / IsLiked / ListLikedVideos：feed 的读路径
// 不应该有写点赞的能力，接口越小，mock 越难被误用。
type LikeLookup interface {
	BatchGetLiked(ctx context.Context, videoIDs []uint, accountID uint) (map[uint]bool, error)
}

// SummaryLookup 是 FeedService 所需的"AI 摘要查询"能力。
//
// 为什么单独一个接口而不是塞进 FeedStore：它查的是另一张表
// （video_ai_analyses），而且**可以缺省**——AI 关闭、表为空、
// 或将来这张表被整体删除时，Feed 必须照常工作。
// 把"可选能力"和"必需能力"混在同一个接口里，会让"AI 关掉"变成
// "FeedService 构造不出来"，那就把锦上添花变成了承重墙。
type SummaryLookup interface {
	// LatestAISummaries 返回每个视频最新一条 AI 摘要（没有则不出现在 map 里）。
	LatestAISummaries(ctx context.Context, videoIDs []uint) (map[uint]string, error)
}

// ReasonSignalLookup 是推荐理由所需的**真实信号**（P2 §4.6）。
//
// 为什么单列一个接口：理由是**最容易造假**的一块（P2 §7 的风险表里
// 明确写着"理由幻觉，用户信任受损"）。本接口提供的是数据里真实存在的
// 事实——视频被打上了哪些标签——而不是让模型生成一句话。
// 没有真实信号时宁可返回空字符串，也不生成"猜你喜欢"这种无信息量的套话。
//
// 同样允许为 nil：AI 关闭时标签表为空，理由只会来自语义召回
// （那也是一条真实信号：这条视频与用户点赞过的内容相似）。
type ReasonSignalLookup interface {
	// VideoTagNames 返回每个视频的标签名（AI 打标或用户手打的都算）。
	VideoTagNames(ctx context.Context, videoIDs []uint) (map[uint][]string, error)
}

// 编译期断言：GORM 实现必须始终满足这两个接口。
// 若 FeedRepository / video.LikeRepository 的方法签名漂移，这里会立刻编译失败，
// 而不是等到某个测试莫名其妙地报错。
var (
	_ FeedStore  = (*FeedRepository)(nil)
	_ LikeLookup = (*video.LikeRepository)(nil)
	// SummaryLookup 由 video 包实现（表的所有者是它），接口声明在消费方。
	_ SummaryLookup = (*video.VideoRepository)(nil)
)

// VectorRecallStore 的实现方是**两个**对象（视频向量 + 点赞行为），
// 因此这里按"每个实现方各自负责哪几个方法"分成两条断言。
//
// 刻意不写 `_ VectorRecallStore = (*video.VideoRepository)(nil)`：
// 那个断言永远不会成立（没有哪个类型同时持有两个仓库），写成它只是自欺。
var (
	_ interface {
		ListVectorCandidates(ctx context.Context, model string, dim int, limit int) ([]video.VectorCandidate, error)
		LoadVectors(ctx context.Context, videoIDs []uint, model string, dim int) (map[uint][]float32, error)
		ListEmbeddingModels(ctx context.Context, dim int, limit int) ([]video.EmbeddingModelStat, error)
	} = (*video.VideoRepository)(nil)
	_ interface {
		RecentLikedVideoIDs(ctx context.Context, accountID uint, limit int) ([]uint, error)
	} = (*video.LikeRepository)(nil)
	// 最新 ID 的读取能力由 video 包实现（它认识 videos 表）。
	_ FreshIDLister = (*video.VideoRepository)(nil)
)
