package feed

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
	redis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

type FeedService struct {
	// summaryRepo 是可选能力：为 nil 时完全不查摘要（AI 关闭的部署）。
	//
	// 刻意允许为 nil 而不是要求传一个空实现：这样"AI 关闭"这条路径在
	// 装配层就是"不调用 WithSummaryLookup"，语义最直白，也不会因为
	// 忘了注入空实现而 panic。
	summaryRepo SummaryLookup
	// reasonSignals 是 P2 推荐理由所需的真实信号（AI 标签）。为 nil 时
	// reason 只会来自语义召回这一条真实信号（或干脆为空）。
	reasonSignals ReasonSignalLookup
	repo          FeedStore
	likeRepo      LikeLookup
	rediscache    *rediscache.Client
	localcache    *cache.Cache
	cacheTTL      time.Duration
	requestGroup  singleflight.Group

	// ---- P2 语义召回（全部可选：零值时整条路一次都不执行）----

	// vectorStore 为 nil 时语义路关闭。**允许为 nil** 而不是要求注入空实现：
	// "AI 关闭 / 没配向量"是最常见的部署形态，它应当在装配层表现为
	// "不调用 WithSemanticRecall"，而不是每个请求里多一次 nil 判断。
	vectorStore VectorRecallStore
	// recallQuota / exploreQuota 是两路配额（0 = 关闭）。
	recallQuota  float64
	exploreQuota float64
	// embeddingModel / embeddingDim 决定与哪一组已入库向量比对。
	// 它们是幂等键的一部分：配错时语义路会读到 0 条候选并静默关闭
	// （而不是把两种向量混着算，那不会报错、只会让召回质量莫名其妙地差）。
	//
	// embeddingModel 是**配置值**（请求里用的名字），真正用于读取的模型名
	// 由 effectiveModel 决定：它优先取库里实际存在的空间（见
	// resolveEmbeddingModel）。两者可以不同（离线演示就是不同的），
	// 因此任何直接用 embeddingModel 去读向量表的地方都是 bug。
	embeddingModel string
	embeddingDim   int
	// modelCache 缓存"当前在用的向量空间名"，TTL 见 embeddingModelCacheTTL。
	modelCache *cache.Cache
	// semanticTimeout 是**整条语义路**（列出候选 + 读向量 + 算相似度）的预算。
	// 见 WithSemanticRecall 的注释：D1 的 50ms 是总共的，不是每一段各 50ms。
	semanticTimeout time.Duration
	// candidateFilter 是额外候选过滤器（拉黑/不可见等）。为 nil 时只做
	// 内置的三条过滤（缺失视频、作者自看、最新一批让位给时序路）。
	candidateFilter CandidateFilter

	// ---- P2 LLM 精排（默认关闭）----

	rerankClient   RerankClient
	rerankEnabled  bool
	rerankTimeout  time.Duration
	rerankTopN     int
	rerankDegraded atomic.Int64
	rerankCache    *cache.Cache
}

// CandidateFilter 是"这条候选该不该推给这个用户"的判定。
//
// 为什么留一个注入点而不是直接在语义路里查拉黑表：P2 的验收只要求
// "召回结果中不出现无效 ID"，而拉黑/可见性属于产品规则，它的数据源
// 与查询代价都与向量检索无关。留一个钩子，等于让"以后要加一条过滤"
// 不需要动召回逻辑（那是最不该被改动的部分）。
//
// 实现方必须是**纯函数式**的：同一批候选、同一个用户，多次调用结果相同，
// 且不得写入任何状态。召回路径会对它做多次调用。
type CandidateFilter func(ctx context.Context, viewerAccountID uint, v *video.Video) bool

// RerankClient 是 LLM 精排能力（接口定义在消费方）。
//
// 只收一个方法、只返回 ID 顺序：把"打分"这件事留给模型，
// 把"排序结果怎么用"留给本包。这样即使将来换成别的实现
// （本地小模型、规则重排），消费方的代码一行都不用改。
type RerankClient interface {
	// Rerank 把候选按用户相关度重排，返回**全部**候选的 ID（顺序即优先级）。
	//
	// 契约：返回的 ID 必须与输入的集合完全一致（不增、不减、不重复）。
	// 实现方负责在本地校验；本包也会再校验一次（见 rerank.go）。
	Rerank(ctx context.Context, accountID uint, candidates []RerankCandidate) ([]uint, error)
}

// RerankCandidate 是送给精排模型的一条候选。
type RerankCandidate struct {
	ID          uint
	Title       string
	Description string
	AuthorID    uint
	LikesCount  int64
	Popularity  int64
	CreateTime  time.Time
}

type CachedFeedData struct {
	PublicVideos []video.Video `json:"public_videos"`
}

func NewFeedService(repo FeedStore, likeRepo LikeLookup, rediscache *rediscache.Client) *FeedService {
	return &FeedService{repo: repo, likeRepo: likeRepo, rediscache: rediscache, localcache: cache.New(3*time.Second, 5*time.Second), cacheTTL: 24 * time.Hour}
}

// WithSummaryLookup 注入 AI 摘要查询能力，返回自身以支持链式装配。
//
// 用链式 setter 而不是改构造函数：构造函数已被大量单测使用，
// 而且"不注入"正是 AI 关闭时的默认状态，应当是改动最小的那条路径。
func (f *FeedService) WithSummaryLookup(l SummaryLookup) *FeedService {
	f.summaryRepo = l
	return f
}

// SemanticRecallOptions 是 P2 语义路的装配参数。
//
// 用一个结构体而不是继续加链式方法：这几个值必须**一起**决定
// "这条路开不开"，分成多个 setter 之后，漏调一个就会得到
// "配额开着但没有向量模型"这种半开状态——而它的现象是
// 语义路静默地永远返回空，排查起来只能靠读代码。
type SemanticRecallOptions struct {
	Store  VectorRecallStore
	Model  string
	Dim    int
	Quota  float64
	Addons SemanticAddons
}

// SemanticAddons 是语义路的两项可选扩展。
//
// 单独一个结构体是为了让"只开语义召回"这个最小场景的装配尽可能短：
// 探索配额与候选过滤器都可以不填，路径照常工作。
type SemanticAddons struct {
	// ExploreQuota 是冷启动探索配额（0 = 不做探索）。
	ExploreQuota float64
	// Timeout 是整条语义路的预算，<=0 时用 DefaultSemanticTimeout。
	Timeout time.Duration
	// Filter 是额外候选过滤器，可为 nil。
	Filter CandidateFilter
}

// DefaultSemanticTimeout 是整条语义路的默认预算（P2 前置 D1：50ms）。
//
// 为什么是**整条路**而不是每次查询：D1 的 150ms 读路径预算里，
// 语义层占 50ms。把它拆给"列候选 / 读向量 / 算相似度"三段，
// 总耗时就是三段之和，而预算的本意是给用户请求设一个上界。
const DefaultSemanticTimeout = 50 * time.Millisecond

// WithSemanticRecall 注入 P2 的语义召回能力，返回自身以支持链式装配。
//
// 装配层只在"AI 打开且配额 > 0"时调用它。不调用时：
// vectorStore 为 nil、两个配额都是 0，于是 buildQuotaPlan 给出零值计划，
// 整条语义路（画像、向量读取、相似度、探索查询）一次都不执行。
// "开关关掉 = 零开销"因此是**结构上成立**的，而不是靠早退判断。
func (f *FeedService) WithSemanticRecall(opts SemanticRecallOptions) *FeedService {
	f.vectorStore = opts.Store
	f.embeddingModel = opts.Model
	f.embeddingDim = opts.Dim
	if f.embeddingDim <= 0 {
		// 维度是幂等键的一部分，不能靠"猜"。给一个与 ai 默认值一致的值，
		// 让直接构造 options 的调用方仍然能工作（真正的取值由装配层给）。
		f.embeddingDim = defaultEmbeddingDim
	}
	if opts.Store == nil {
		// Store 为空等于这条路不存在。把配额一并清零，
		// 避免出现"配额开着但拿不到向量"的半开状态。
		f.recallQuota = 0
		f.exploreQuota = 0
		return f
	}
	f.recallQuota = clampQuota(opts.Quota)
	f.exploreQuota = clampQuota(opts.Addons.ExploreQuota)
	f.semanticTimeout = opts.Addons.Timeout
	if f.semanticTimeout <= 0 {
		f.semanticTimeout = DefaultSemanticTimeout
	}
	f.candidateFilter = opts.Addons.Filter
	if f.modelCache == nil {
		// TTL 见 embeddingModelCacheTTL；清理间隔取 4 倍 TTL，
		// 因为这里只会有一个键，清理频率无关紧要。
		f.modelCache = cache.New(embeddingModelCacheTTL, 4*embeddingModelCacheTTL)
	}
	return f
}

// RerankOptions 是 P2 §4.4 精排的装配参数。
//
// Timeout 与 TopN 都从配置来（ai.rerank_timeout / ai.rerank_candidates），
// 越界值在 config 层已经被夹住，这里只做"<=0 用默认"的兜底。
type RerankOptions struct {
	Client  RerankClient
	Enabled bool
	Timeout time.Duration
	TopN    int
}

// WithRerank 注入 LLM 精排能力，返回自身以支持链式装配。
//
// Enabled=false（默认）时**不注入也会是关闭的**，这里再判一次是为了让
// "开了开关但没注入客户端"表现为明确的关闭，而不是一个 nil 解引用。
func (f *FeedService) WithRerank(opts RerankOptions) *FeedService {
	f.rerankClient = opts.Client
	f.rerankEnabled = opts.Enabled && opts.Client != nil
	f.rerankTimeout = opts.Timeout
	if f.rerankTimeout <= 0 {
		f.rerankTimeout = defaultRerankTimeout
	}
	f.rerankTopN = opts.TopN
	if f.rerankTopN <= 0 {
		f.rerankTopN = defaultRerankCandidates
	}
	if f.rerankCache == nil {
		f.rerankCache = newRerankCache()
	}
	return f
}

// defaultRerankTimeout 与 internal/config 的 defaultRerankTimeout 同值（120ms）。
//
// 这个默认值与 20 条候选**不自洽**（真实模型不可能在 120ms 内答完 20 条），
// 而且这是刻意的：在真实 provider 延迟被实测之前，唯一安全的默认组合就是
// "关掉"，而默认值必须落在安全侧——只打开开关而不调超时的人会立刻看到
// 100% 的降级率（Warn 日志 + degraded 计数），而不是一个悄悄变慢的 Feed。
// 判定规则与回填步骤见 AI-P2-设计决定.md 第 2 节。
const defaultRerankTimeout = 120 * time.Millisecond

// WithReasonSignals 注入推荐理由所需的真实信号（P2 §4.6）。
//
// 与摘要一样是**可选**能力：不注入时 reason 只会来自语义召回
// （那也是一条真实信号），而不是被编造出来。
func (f *FeedService) WithReasonSignals(l ReasonSignalLookup) *FeedService {
	f.reasonSignals = l
	return f
}

// clampQuota 把配额夹到 [0,1]。
//
// 越界按 0（关闭）而不是按 1 处理：一个写错单位的配置（例如写了 30 表示
// 30%）不该变成"整页都是语义召回"，那会把既有排序整个换掉。
// 与 internal/config 的 RecallQuotaOr 保持同一个方向。
func clampQuota(q float64) float64 {
	if q < 0 || q > 1 {
		return 0
	}
	return q
}

// defaultEmbeddingDim 与 ai.DefaultEmbeddingDim 一致。
//
// 不 import ai 包只为一个常量：feed 包目前不依赖 internal/ai，
// 而这条依赖会把 gRPC 相关的东西间接拉进读路径的依赖图。
// 值本身由测试与配置层的用例双向钉住（两边漂移会立刻失败）。
const defaultEmbeddingDim = 1536

func (f *FeedService) GetVideoByIDs(ctx context.Context, videoIDs []uint) ([]*video.Video, error) {
	// GetVideoByIDs 批量获取视频信息
	// 采用 L1(本地缓存) -> L2(Redis) -> L3(MySQL) 三级架构
	if len(videoIDs) == 0 {
		return []*video.Video{}, nil
	}

	videoMap := make(map[uint]*video.Video)
	//L1:本地缓存
	var missedL1 []uint
	for _, id := range videoIDs {
		cacheKey := f.rediscache.Key("video:entity:%d", id)
		if f.localcache != nil {
			if v, found := f.localcache.Get(cacheKey); found {
				if data, ok := v.(video.Video); ok {
					videoMap[id] = &data
					continue
				}
			}
		}
		// 记录未命中的 ID，准备进入下一级缓存
		missedL1 = append(missedL1, id)
	}

	if len(missedL1) == 0 {
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	//L2:redis
	var missedL2 []uint
	if len(missedL1) > 0 {
		cacheKeys := make([]string, len(missedL1))
		for i, id := range missedL1 {
			cacheKeys[i] = f.rediscache.Key("video:entity:%d", id)
		}

		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		results, err := f.rediscache.MGet(cacheCtx, cacheKeys...)
		cancel()

		if err == nil {
			for i, res := range results {
				id := missedL1[i]
				if res != nil {
					if str, ok := res.(string); ok {
						var v video.Video
						if err := json.Unmarshal([]byte(str), &v); err == nil {
							videoMap[id] = &v
							// 回写更新 L1 本地缓存
							if f.localcache != nil {
								f.localcache.Set(cacheKeys[i], v, 5*time.Second)
							}
							continue
						}
					}
				}
				missedL2 = append(missedL2, id)
			}
		} else {
			// 如果 Redis 挂了或者超时了，全部降级到 L3
			missedL2 = missedL1
			// Redis 不可用即整体降级到 MySQL，请求仍然成功（只是变慢），故为 Warn。
			logging.Ctx(ctx).Warn("L2 Redis MGet 失败，全部降级到 MySQL", zap.Error(err))
		}
	}

	if len(missedL2) == 0 {
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	//L3:MySQL
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range missedL2 {
		wg.Add(1)
		go func(videoID uint) {
			defer wg.Done()
			sfKey := f.rediscache.Key("sf:entity:%d", videoID)

			v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
				videoList, err := f.repo.GetByIDs(ctx, []uint{videoID})

				if err != nil || len(videoList) == 0 {
					return nil, err
				}

				safeCopy := *videoList[0]
				cachekey := f.rediscache.Key("video:entity:%d", safeCopy.ID)
				if b, err := json.Marshal(safeCopy); err == nil {
					//异步回写redis
					go func(k string, b []byte) {
						setCtx, setCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
						defer setCancel()

						f.rediscache.SetBytes(setCtx, k, b, time.Hour)
					}(cachekey, b)
				}
				return videoList[0], err
			})

			if err == nil && v != nil {
				safeCopy := *(v.(*video.Video))
				mu.Lock()
				videoMap[id] = &safeCopy
				mu.Unlock()
				f.localcache.Set(f.rediscache.Key("video:entity:%d", safeCopy.ID), safeCopy, 5*time.Second)
			}
		}(id)
	}
	wg.Wait()
	return buildOrderedResult(videoIDs, videoMap), nil
}

// errEmptyDatabase 是"数据库里一条视频都没有"的内部哨兵。
//
// 为什么用错误而不是"返回 nil 切片"来表达：nil 与"查到 0 条"在 Go 里
// 是同一个值，而这两者的**后续行为不同**——空库要直接给出空页且不再
// 构建响应项，冷尾翻到空则要走完整的构建流程。用一个不跨包暴露的
// 哨兵把两者分开，比在调用点反复判 len(...) == 0 更不容易写反。
var errEmptyDatabase = errors.New("feed: 数据库为空")

// 时间线相关的常量。
const (
	// timelineRebuildLimit 是一次修复最多向数据库回捞多少条。
	//
	// 它是"一次修复的步长"而不是"时间线的容量"：缺口大于它时，
	// 后续的探测会继续补，直到追上（每次最多 1000 条，代价可控）。
	timelineRebuildLimit = 1000

	// TimelineProbeSize 是落后探测一次核对多少条视频。
	//
	// 取 20（默认页大小）而不是 1：见 timelineBehind 的注释——只核对一条
	// 会被单行脏数据骗过去。也没有必要取到 limit：探测结果的缓存不该
	// 随请求参数变化，否则不同 limit 的请求会互相冲刷缓存。
	TimelineProbeSize = 20

	// timelineScoreSkew 是"时间线分数比数据库时间大多少就算脏"的容忍度。
	//
	// 为什么需要这个判据（一次真机上量出来的故障）：
	// 本机 compose 环境里，时间线的分数比数据库里的 create_time **整整大 8 小时**
	// （另一处按本地时区而非 UTC 写入分数留下的历史数据，6 条成员全部如此）。
	// 后果不是"顺序有点不对"，而是**冷热拼接算出错误的游标**：
	// 游标取的是 ZSET 尾部（最旧分数），而它比数据库里那条视频的真实时间大 8 小时，
	// 于是数据库查询 `create_time < 游标` 会把**已经返回过的三条视频再返回一遍**——
	// `/feed/listLatest` 的第一页出现重复视频（实测：6 条内容返回 9 行）。
	//
	// 为什么容忍 2 秒而不是"必须精确相等"：
	// 成员刚被异步链路写进时间线、而数据库那一条的 create_time 有极小抖动时，
	// 分数略微偏大是**正常**的中间状态。把这些正常状态判成"脏"会让每个首页
	// 请求都触发一次全量补齐，把一次读故障放大成雪崩。
	// 8 小时这种量级的偏移远远超过任何时钟抖动，2 秒足以区分两者。
	timelineScoreSkew = 2 * time.Second

	// timelineProbeTTL 是"时间线是否落后"这个探测结论的缓存时长。
	//
	// 这个缓存是**必须**的，不是优化：探测在首页读路径上，若每次都查库，
	// 就等于把"避免打库"这件事取消了。1 秒的选择依据见
	// ensureTimelineFresh 的注释（它决定"时间线落后"最长能被容忍多久）。
	timelineProbeTTL = time.Second
)

// ListLatest 查询最新视频（冷热分离 + 游标分页 + P2 多路融合）。
//
// 它是 P2 唯一被改动的读路径，改动方式是**包一层**：
//
//	基准页（listLatestVideos，本函数以下的全部逻辑一行没动）
//	  + 语义召回（recall_quota 个槽位）
//	  + 冷启动探索（explore_quota 个槽位）
//	  → 按槽位合成 → 返回 limit 条
//
// 为什么必须包一层而不是把配额逻辑插进冷热拼接里：冷热拼接那段代码
// 同时承载着时间线自愈（b9f6e3f）与既有分页契约，任何一处改动都要
// 重新论证"没有改坏分页"。包一层之后，两条性质都是**结构性**的：
//
//   - 配额全为 0 时融合函数直接返回基准页的前 limit 条（与基准线逐字节一致）；
//   - 游标（next_time）取基准页里最旧的一条，不受召回内容影响。
//
// 未登录（viewerAccountID == 0）时不走语义路：没有用户就没有兴趣向量，
// 而"全局兴趣向量"只是热榜的另一种算法（P2 §4.5 对"新用户"的要求）。
func (f *FeedService) ListLatest(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	plan := buildQuotaPlan(limit, f.recallQuota, f.exploreQuota)
	if !plan.Enabled() {
		// P2 关闭：一条额外查询都不发、一行额外代码都不执行。
		// 这个早退是"三关全闭时与基准线逐字节一致"的实现方式。
		return f.listLatestBaseline(ctx, limit, latestBefore, viewerAccountID)
	}
	return f.listLatestFused(ctx, limit, latestBefore, viewerAccountID, plan)
}

// listLatestBaseline 是 P2 之前的那条读路径（时序 + 冷热拼接 + 分页）。
func (f *FeedService) listLatestBaseline(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	baseVideos, err := f.listLatestVideos(ctx, limit, limit, latestBefore)
	if err != nil {
		if errors.Is(err, errEmptyDatabase) {
			// 空库：与改造前一致地返回一个空页，且**不再走 buildFeedVideos**。
			//
			// "不再走"这件事是被既有测试明确断言过的行为（空页不该产生
			// 额外的 likeRepo 查询）。它现在有了第二个理由：P2 之后
			// buildFeedVideos 还会去查标签与向量，空页上做这些毫无意义。
			return ListLatestResponse{VideoList: []FeedVideoItem{}}, nil
		}
		return ListLatestResponse{}, err
	}
	// cursorFrom 传零值：基准路径（配额全关）下游标就是"最后一条视频的时间"，
	// 这也正是 P2 之前的行为。latestBefore 在空页时被原样返回。
	return f.buildListLatestResponse(ctx, baseVideos, limit, time.Time{}, viewerAccountID, reasonSources{})
}

// listLatestFused 在基准页之外叠加两路召回，并按配额槽位合成。
func (f *FeedService) listLatestFused(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint, plan QuotaPlan) (ListLatestResponse, error) {
	// 基础页按**融合池**大小取（D4 的 200 条），而不是只取 limit 条。
	//
	// 为什么需要更大的池子：配额位会把基础页里的若干条挤到后面去，
	// 手上只有 limit 条时，"被挤掉的那几条"没有替补，结果就是
	// 一次开启召回反而让页面变短，表现为"开了语义召回，首页从 20 条变成 17 条"。
	poolLimit := FusionPoolSize
	if poolLimit < limit {
		poolLimit = limit
	}
	// 池子取 poolLimit 条，但"要不要冷热拼接"按这一页需要的 limit 条判断
	// （见 listLatestVideos 的注释：混用两者会让首页变短）。
	// 池子取 poolLimit 条，但"要不要冷热拼接"按这一页需要的 limit 条判断
	// （见 listLatestVideos 的注释：混用两者会让拼接量按池子大小算）。
	pool, err := f.listLatestVideos(ctx, poolLimit, limit, latestBefore)
	if err != nil {
		if errors.Is(err, errEmptyDatabase) {
			return ListLatestResponse{VideoList: []FeedVideoItem{}}, nil
		}
		return ListLatestResponse{}, err
	}
	if len(pool) > limit {
		// 池子的前 limit 条才是"这一页"，其余只是安全余量。
		//
		// 替补刻意**不参与**最终列表的填充：它们比这一页更旧，放进来会让
		// next_time 越过它们，于是翻页时中间那段视频被静默跳过。
		// 宁可页面少一条（fuseChannels 会在候选不够时退回基础页），
		// 也不要让用户看到"翻页跳过了几条"。
		pool = pool[:limit]
	}

	semantic := f.semanticRecall(ctx, viewerAccountID, plan)
	explore := f.exploreRecall(ctx, viewerAccountID, plan)
	final, oldestBase := fuseChannels(fuseInput{
		Base:     pool,
		Semantic: semantic,
		Explore:  explore,
		Plan:     plan,
	})
	if len(final) == 0 {
		// 融合后为空（基础页本身为空）：与基准路径一样返回空页。
		return f.buildListLatestResponse(ctx, nil, limit, time.Time{}, viewerAccountID,
			reasonSources{semantic: semantic, explore: explore})
	}
	// LLM 精排（默认关闭）。它作用在**最终列表**上：融合分已经把三路的
	// 优先级表达完了，精排只改变列表内部的次序，不改变集合
	// （validateRerankResult 强制这一点）。
	//
	// 顺序：融合 → 精排 → 配额槽位允许被"重排"而不是"重新分配"。
	// 之所以不在精排之后再分配配额：那会让"配额"失去确定性——
	// 模型给出的顺序一旦偏好某一类内容，配额位就会被同一路占满。
	if final = f.applyRerank(ctx, viewerAccountID, final); len(final) == 0 {
		return f.buildListLatestResponse(ctx, nil, limit, time.Time{}, viewerAccountID,
			reasonSources{semantic: semantic, explore: explore})
	}
	return f.buildListLatestResponse(ctx, final, limit, oldestBase, viewerAccountID,
		reasonSources{semantic: semantic, explore: explore})
}

// listLatestVideos 取基础页（P2 之前 ListLatest 的全部检索逻辑）。
//
// 两个数量参数必须分开，这一点是一次**真实回归**的证据：
//
//	limit     = 这一次最多取多少条（"池子多大"）
//	stitchTo  = 至少凑到多少条才不必再从数据库回捞冷的（"这一页要几条"）
//
// P2 引入融合池之后，limit 会变成 200（为了给配额位留替补），
// 而"要不要冷热拼接"仍然必须按**这一页需要几条**来判断。
// 一开始把两者混用，效果是：ZSET 里只有 6 条成员、其中 2 条视频已被删除时，
// 旧实现会去数据库把那几条冷数据补上（凑够 10 条），
// 新实现却认为"6 < 200，我要凑 200 条"然后……什么都补不上，
// 于是首页从 10 条缩到了 6 条。真机对比 old/new 两个进程时抓到的。
func (f *FeedService) listLatestVideos(ctx context.Context, limit int, stitchTo int, latestBefore time.Time) ([]*video.Video, error) {
	if stitchTo <= 0 || stitchTo > limit {
		stitchTo = limit
	}
	if f.rediscache == nil {
		return f.repo.ListLatest(ctx, limit, latestBefore)
	}

	// 获取 ZSET 中最老的一条数据
	zsetTail, err := f.rediscache.ZRangeWithScores(ctx, f.rediscache.Key("feed:global_timeline"), 0, 0)

	if err != nil {
		return f.repo.ListLatest(ctx, limit, latestBefore)
	}

	// 时间线需要修复的两种情况：
	//
	// 时间线需要修复的两种情况：
	//
	//	1. **空**：从没建过（新部署 / Redis 被清）；
	//	2. **落后**：数据库里最新的一条视频还不在时间线上。
	//
	// 第二种曾经被漏掉，是一次真实的缺陷：原实现只在 ZSET **完全为空**时
	// 才回退数据库，而"落后"时读路径会一直从 ZSET 取，永远看不到新视频。
	// 后果不是"慢一点"，而是**只要异步消费者慢下来或挂掉，全局最新流就
	// 静默冻结在过去，且永远不会自愈**——因为补齐逻辑（下面的冷热拼接）
	// 只会向更旧的方向翻页，结构上不可能发现更新的内容。
	//
	// 触发条件写在一起的另一个好处：修复动作只有一份实现（refreshTimeline），
	// "空"与"落后"不会各自演化出一套补数据的逻辑。
	if len(zsetTail) == 0 || f.timelineBehind(ctx, latestBefore) {
		repaired, err := f.refreshTimeline(ctx)
		switch {
		case err != nil:
			// 修复失败时**不返回错误**：
			//   - 时间线空 → 只能查库（返回错误会让一次可降级的故障变成 5xx）；
			//   - 时间线落后 → 手上这页是旧的但不是错的，给出去好过一个错误，
			//     而且下一次请求会再试（探测缓存过期后）。
			// 用 Warn：这是"异步链路没跟上"的运维信号，但本次请求仍然成功。
			logging.Ctx(ctx).Warn("时间线刷新失败，本次继续使用现有时间线", zap.Error(err))
			if len(zsetTail) == 0 {
				return f.repo.ListLatest(ctx, limit, latestBefore)
			}
		case !repaired:
			// 数据库也是空的：没有基础页可给。
			//
			// 这条分支的存在是为了**防无限递归**——原实现用递归重查实现
			// "重建后再读一次"，库为空时会自己调用自己。
			return nil, errEmptyDatabase
		default:
			// 补齐后必须重读一次尾巴：watermark 取的是**最低分**，
			// 而补进来的成员完全可能比原来的最低分更旧（时间戳异常的历史
			// 成员、或跨时区写入的数据），于是冷热边界会跟着变。
			//
			// 这里曾经只在"原本为空"时重读，理由是"补进来的都是更新的数据"——
			// 那是错的，单测 TestListLatestDetectsStaleTimelineDespiteFutureScores
			// 直接构造了反例（时间线里只有一个未来时间的成员）。
			// 重读一次只是 O(log N) 的 ZRANGE，不值得为省它去赌一个假设。
			zsetTail, err = f.rediscache.ZRangeWithScores(ctx, f.rediscache.Key("feed:global_timeline"), 0, 0)
			if err != nil || len(zsetTail) == 0 {
				return f.repo.ListLatest(ctx, limit, latestBefore)
			}
		}
	}

	watermark := int64(zsetTail[0].Score)
	reqTime := time.Now().UnixMilli()
	if !latestBefore.IsZero() {
		reqTime = latestBefore.UnixMilli()
	}

	var baseVideos []*video.Video

	if reqTime <= watermark {
		//冷数据降级查库

		// 针对个别用户的防并发（此时可以用时间戳做锁，因为冷尾流量极小）
		sfKey := f.rediscache.Key("sf:cold:listLatest:%d:%d", limit, reqTime)
		v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
			return f.repo.ListLatest(ctx, limit, latestBefore)
		})
		if err != nil {
			return nil, err
		}
		baseVideos = v.([]*video.Video)
		// 不回写 ZSET，防止冷数据污染热点时间线

	} else {
		// 热数据直接查redis
		maxScore := "+inf"
		if !latestBefore.IsZero() {
			maxScore = fmt.Sprintf("%d", reqTime-1) // 防重复
		}

		videoIDsStr, err := f.rediscache.ZRevRangeByScore(ctx, f.rediscache.Key("feed:global_timeline"), maxScore, "-inf", 0, int64(limit))
		if err != nil {
			return nil, err
		}

		var videoIDs []uint
		for _, idStr := range videoIDsStr {
			if id, err := strconv.ParseUint(idStr, 10, 64); err == nil {
				videoIDs = append(videoIDs, uint(id))
			}
		}

		if len(videoIDs) > 0 {
			baseVideos, err = f.GetVideoByIDs(ctx, videoIDs)
			if err != nil {
				return nil, err
			}
		}

		// 刚好击穿了冷热边界
		if len(baseVideos) < stitchTo {
			remainLimit := stitchTo - len(baseVideos) // 计算还差几个

			var coldCursor time.Time
			if len(baseVideos) > 0 {
				coldCursor = baseVideos[len(baseVideos)-1].CreateTime
			} else {
				coldCursor = latestBefore
			}

			sfKey := f.rediscache.Key("sf:stitch:listLatest:%d:%d", remainLimit, coldCursor.UnixMilli())
			v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
				return f.repo.ListLatest(ctx, remainLimit, coldCursor)
			})

			if err == nil {
				coldVideos := v.([]*video.Video)
				baseVideos = append(baseVideos, coldVideos...)
			}
		}
	}

	return baseVideos, nil
}

// buildListLatestResponse 把最终列表组装成响应。
//
// 两个参数刻意分开：videos 是**要展示的**，cursorFrom 是**算游标用的**。
// 融合之后这两者不再是同一个东西——语义召回会带来比基础页更旧的内容，
// 若游标跟着它走，翻页会跳过基础页里介于两者之间的视频（见 fuseChannels）。
//
// cursorFrom 为零值时退回"最后一条的时间"：那条路径只在配额全关时走到
// （也就是 P2 之前的行为），以及基础页为空时（此时游标本来就是 0）。
func (f *FeedService) buildListLatestResponse(ctx context.Context, videos []*video.Video, limit int, cursorFrom time.Time, viewerAccountID uint, recall reasonSources) (ListLatestResponse, error) {
	// 冷尾整页为空：返回非 nil 的空切片与 next_time=0。
	//
	// 三条都是既有契约：[] 与 null 对前端是两回事；空页之后没有下一页可翻；
	// 而 buildFeedVideos 仍然会被调用一次（likeRepo 收到一个空列表）——
	// 那是既有测试断言过的调用次数。这里刻意不做"顺手省掉一次空查询"的优化。
	if len(videos) == 0 {
		if _, err := f.buildFeedVideos(ctx, nil, viewerAccountID); err != nil {
			return ListLatestResponse{}, err
		}
		return ListLatestResponse{VideoList: []FeedVideoItem{}}, nil
	}

	// 游标的两条分支，与 P2 之前一致：
	//
	//  1. 融合给出了基准页里最旧的一条（cursorFrom）——P2 新增的那条路径；
	//  2. 配额全关时的基准路径：取最后一条视频的时间。
	var nextTime int64
	if !cursorFrom.IsZero() {
		nextTime = cursorFrom.UnixMilli()
	} else {
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}

	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID, recall)
	if err != nil {
		return ListLatestResponse{}, err
	}

	return ListLatestResponse{
		VideoList: feedVideos,
		NextTime:  nextTime,
		HasMore:   len(videos) == limit,
	}, nil
}

// timelineBehind 报告时间线是否**落后于数据库**。
//
// 判据：**这一页本该展示的那批视频（数据库里最新的 N 条），是不是每一条都在时间线上**。
//
// 为什么不是"最新那一条在不在"：那依赖"数据库里最新的一条就是真正最新的"
// 这个假设，而它对**单行脏数据**毫无抵抗力。本机开发库里就有这样的行
// （video 795，时间戳按另一种时区约定写入，于是在排序里永远排第一），
// 它在时间线上，于是"最新一条在不在"永远回答"在"——**探测被一行数据废掉了**。
// 按一批来判断，单个异常行就无法代表整体。
//
// 为什么不是"数据库最新时间 > ZSET 最高分"：那依赖两边时钟可比，
// 而时间线里允许存在时间戳异常的成员（同一个 795 的 ZSET 分数就比它在
// 数据库里的时间大 8 小时），一个未来时间的成员会让比较永远为假。
// 按成员是否存在来判断，与时钟无关。
//
// 为什么这个判断不能省（这是一个真实缺陷的修复）：
// 时间线 ZSET 由异步链路（outbox -> MQ -> 消费者）写入，而发布接口在事务
// 提交后就返回了。所以"刚发布的视频还不在 ZSET 里"是一个正常存在的中间状态。
// 原实现只在 ZSET **完全为空**时回退数据库，于是：
//   - 消费者正常时这个窗口只有几十毫秒（实测约 75ms），没人察觉；
//   - 消费者一旦变慢或挂掉，ZSET 就永远停在过去，而 /feed/listLatest 会一直
//     从它取——**全局最新流静默冻结**，没有错误、没有日志、不会自愈。
//
// 代价与频率：探测 = 一次 `LIMIT N` 的索引查询 + 一次 ZMSCORE（各一次往返），
// 结论缓存 timelineProbeTTL（1 秒）并走 singleflight，因此稳态下
// 每个进程每秒最多一次。「落后」最长被容忍 1 秒——远小于页面刷新间隔，
// 却足以把上面说的"永久冻结"变成"最多慢一秒"。
//
// 只对**首页**（latestBefore 为零）判断：翻旧页时"有没有更新的视频"
// 与本次结果无关，探测只会白白多一次查询。
func (f *FeedService) timelineBehind(ctx context.Context, latestBefore time.Time) bool {
	if !latestBefore.IsZero() {
		return false
	}

	// 时间线的"最新成员"参与缓存键（分数参与保鲜期判断）：成员一变
	// （新视频被异步写进来、或我们刚补齐过），指纹就变，探测结论自然失效。
	// 这是**不用查库**就能发现"时间线动了"的那条信号。
	headMember, headScore := f.timelineHead(ctx)
	probeKey := "feed:timeline_probe:behind:" + headMember
	if f.localcache != nil {
		if entry, found := f.localcache.Get(probeKey); found {
			if p, ok := entry.(timelineProbeEntry); ok {
				// 关键的一条判断（**不是**可有可无的优化）：
				//
				// 时间线上最新的那条视频的时间 >= 我们上次探测时数据库
				// 最新的那条的时间 → 说明这期间数据库里没有出现更新的视频，
				// 上次的结论依然成立，可以**不打库**直接复用。
				//
				// 反过来：时间线的最新时间落后于数据库最新时间，说明
				// 可能有一条刚发布的视频还没进时间线——这时必须重新探测。
				// 没有这一条的话，"刚好在某次探测之后发布"的视频会被
				// 缓存挡住整整一个 TTL：发布者刷新首页看不到自己的视频，
				// 而端到端回归里的"发布后立刻读最新流"就会随机失败
				// （实测 6 次里挂 1 次，正是这条路径）。
				// 缓存能不能复用，判据集中在 reuseProbe 里（那里的注释
				// 解释了每条判断对应哪个真实故障）。
				if verdict, ok := f.reuseProbe(ctx, p, headScore); ok {
					return verdict
				}
			}
		}
	}

	v, err, _ := f.requestGroup.Do(probeKey, func() (interface{}, error) {
		newest, err := f.repo.ListLatest(ctx, TimelineProbeSize, time.Time{})
		if err != nil {
			return nil, err
		}
		if len(newest) == 0 {
			// 空库是一个**有效结论**（永远不落后），必须缓存住，
			// 否则空库上的每一次首页请求都会去打一次库。
			// newestDB 取零值：它比任何时间线分数都"旧"，
			// 因此空库上的缓存永远不会命中上面那条提前返回——
			// 空库本来就该走冷路径查库，缓存只是省掉探测。
			return timelineProbeEntry{behind: false, at: time.Now()}, nil
		}

		entry := timelineProbeEntry{
			behind:   false,
			newestDB: newest[0].CreateTime,
			at:       time.Now(),
		}
		members := make([]string, 0, len(newest))
		for _, v := range newest {
			members = append(members, strconv.FormatUint(uint64(v.ID), 10))
		}
		scores, err := f.rediscache.ZMScore(ctx, f.rediscache.Key("feed:global_timeline"), members...)
		if err != nil {
			return nil, err
		}
		if len(scores) != len(members) {
			// 理论上不会发生（ZMSCORE 保证等长）。真发生时按"落后"处理：
			// 宁可多做一次幂等的补齐，也不要因为一次协议异常让时间线
			// 永远停在过去——后者是静默的，前者只是多一次 Redis 往返。
			entry.behind = true
			return entry, nil
		}
		for i, score := range scores {
			// ZMSCORE 对**不存在的成员**返回 0（不是 redis.Nil）：命令语义如此，
			// 它返回的是一个"分数数组"，缺席者补 0。
			// 时间线里的分数是毫秒时间戳，恒大于 0，因此 0 可以安全地当作
			// "不在时间线上"。（这一点与单成员版 ZSCORE 不同，那里返回 redis.Nil，
			// 写代码时容易想当然地按 Nil 处理，于是永远判不出缺席。）
			if score == 0 {
				entry.behind = true
				break
			}
			// 分数**明显偏大**时同样视为需要修复（见 timelineScoreSkew 的注释）。
			if int64(score)-newest[i].CreateTime.UnixMilli() > timelineScoreSkew.Milliseconds() {
				entry.behind = true
				break
			}
		}
		return entry, nil
	})
	if err != nil {
		// 探测失败按"没有落后"处理：它是自愈机制，不是正确性前提。
		// 返回 true 会让每次请求都触发一次全量补齐，把一次读故障放大成雪崩。
		return false
	}
	entry, _ := v.(timelineProbeEntry)
	if f.localcache != nil {
		f.localcache.Set(probeKey, entry, timelineProbeTTL)
	}
	return entry.behind
}

// timelineNegativeFreshFor 是"没有落后"这个结论的**最短保鲜期**。
//
// 为什么需要它（一个真实的产品要求，也是一次随机失败的根因）：
// 发布路径只把视频写进数据库，时间线由异步消费者补写（实测 ~75ms）。
// 如果探测结论在这段时间里被复用了，那么"发布→立刻刷新首页"就会
// 看不到自己的视频——端到端回归里"最新流（公开）包含该视频"这条断言
// 实测 6 次挂 1 次，正是这条路径。
//
// 100ms 的依据：它明显长于"同一次发布后的两次请求"之间的间隔（毫秒级，
// 因此第二次请求会等到保鲜期结束、重新探测并看到新视频），
// 又远小于"用户感知得到卡顿"的量级（首屏可交互阈值是 300ms）。
// **只对"没有落后"生效**：落后的结论在补齐之前一直成立，没有这个窗口问题。
const timelineNegativeFreshFor = 100 * time.Millisecond

// timelineProbeEntry 是一次探测的结论与"它当时看到的事实"。
//
// 单独一个结构体而不是只缓存 bool：缓存能不能复用，取决于
// "这期间数据库里有没有出现更新的视频"（见 timelineBehind 里的判断）。
// 只存 bool 的话，判断依据就丢了，只能靠 TTL 兜着——
// 而 TTL 正是"发布后看不到自己的视频"那个缺陷的来源。
type timelineProbeEntry struct {
	// behind 是当时的结论。
	behind bool
	// newestDB 是当时数据库里最新一条视频的发布时间（零值表示库是空的）。
	newestDB time.Time
	// at 是得出结论的时刻，用于判断"没有落后"这个结论是否还足够新。
	at time.Time
}

// reuseProbe 判断一次缓存的探测结论能否复用。
//
// 返回 (verdict, true) 表示可以复用；返回 (_, false) 表示必须重新探测。
//
// 三条判断，每一条都对应一个真实故障或一条真实性能要求：
//
//  1. **落后的结论**（p.behind）在补齐之前一直成立 —— 直接复用。
//     这一条是"每秒钟最多打一次库"的来源（稳态下消费者跟得上时，
//     探测结论是"不落后"，见第 2 条）。
//  2. **时间线的最新时间 >= 上次探测时数据库最新那条的时间**：
//     这期间没有"尚未进时间线"的新视频，结论仍然成立 —— 复用。
//     这是稳态下的**零查询**快速路径。
//  3. 否则（时间线没追上数据库）：可能有一条刚发布的视频。
//     - 结论已经超过保鲜期（timelineNegativeFreshFor）：必须重新探测；
//     - 结论还很新：等到保鲜期结束再探测（发布到写入时间线实测约 75ms，
//     因此等待很短且只发生在"可能有写入"的瞬间）。
//
// 为什么第 3 条不能省（这是一次真实的随机失败）：一次探测给出的 newestDB
// 是**它那一刻**看到的事实，"恰好在那次探测之后发布"的视频永远无法让
// newestDB 变大。只靠第 2 条会让缓存自我印证下去（"上次也说没落后，
// 所以这次也没落后"），而发布者刷新首页就是看不到自己的视频——
// 端到端回归里"发布→立刻读最新流"实测 6 次挂 1 次，正是这条路径。
func (f *FeedService) reuseProbe(ctx context.Context, p timelineProbeEntry, headScore int64) (bool, bool) {
	// 落后的结论在补齐之前一直成立。（它同样受下面的保鲜期约束：
	// 补齐动作本身会改变时间线头部，缓存的键随之改变。）
	if p.behind {
		return true, true
	}
	age := time.Since(p.at)
	if age >= timelineNegativeFreshFor {
		// "没有落后"这个结论超过保鲜期就**必须重新探测**，不管时间线
		// 头部看起来追上了没有：数据库里发生了什么，只有查库才知道。
		// 一次探测给出的 newestDB 是它那一刻看到的事实，而"恰好在那次
		// 探测之后发布"的视频永远无法让 newestDB 变大——只靠
		// "时间线是否追上"判断，缓存会一直自我印证下去。
		return false, false
	}
	if headScore >= p.newestDB.UnixMilli() {
		// 保鲜期内且时间线已追上数据库：这期间没有"尚未进时间线"的新视频，
		// 结论成立 —— 这是稳态下的零查询快速路径。
		return false, true
	}
	// 结论还很新、时间线又没追上：可能挡住的正是刚发布的那条视频
	// （发布 -> 数据库已写 -> 消费者还没写时间线，实测约 75ms）。
	// 等到保鲜期结束再重新探测 —— 等待有界，且只发生在"可能有写入"的瞬间。
	if wait := timelineNegativeFreshFor - age; wait > 0 {
		if !sleepCtx(ctx, wait) {
			// 请求被取消：返回旧结论，本次照常返回（不自愈）。
			// 这是**有意的降级**——请求已经没人要了，不该为了自愈
			// 再去打一次库。
			return false, true
		}
	}
	return false, false
}

// sleepCtx 等待一段时间，ctx 取消时返回 false。
//
// 用 time.NewTimer 而不是 time.Sleep：Sleep 在请求被取消后依然会把
// goroutine 挂着（最长 timelineNegativeFreshFor），而读路径上的
// goroutine 应当随请求结束立刻释放。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// timelineHead 返回时间线上最新成员的 ID 与分数。
//
// 失败时返回空串与 0：调用方（timelineBehind）会把空串当缓存键的一部分，
// 也就是退化成"不区分时间线状态"的缓存；分数 0 会让上面那条
// "时间线是否已经追上数据库"的判断恒为假，从而每次都重新探测——
// 两个方向都是**安全**的（多打一次库，而不是漏掉一条视频）。
func (f *FeedService) timelineHead(ctx context.Context) (string, int64) {
	head, err := f.rediscache.ZRangeWithScores(ctx, f.rediscache.Key("feed:global_timeline"), -1, -1)
	if err != nil || len(head) == 0 {
		return "", 0
	}
	member, _ := head[0].Member.(string)
	return member, int64(head[0].Score)
}

// refreshTimeline 把数据库里最新的若干条视频**整批**写回时间线。
//
// 为什么整批重写而不是"只补比 ZSET 最高分更新的那些"：
// 后者看起来更省，但它把一个**假设**写进了正确性里——假设 ZSET 的最高分
// 一定小于缺失成员的时间。时间线里存在时间戳异常的成员时（历史脏数据、
// 跨时区写入），那个假设不成立，于是"检测到落后、却补不进任何东西"，
// 补齐动作变成空转，缺陷依旧。
//
// ZADD 是幂等的（同成员同分数重复写等于没写），所以整批重写的代价只有
// 一次 Redis 往返，而不是"数据被改坏"的风险。代价被两道闸门限制住：
// singleflight（同一时刻只有一个人在补）+ 结论缓存（最多每秒一次）。
//
// 返回值：
//
//	true,  nil → 时间线现在能用了（含"数据库为空所以没啥可补"）
//	false, nil → 数据库里一条视频都没有
//	_,     err → 数据库或 Redis 出错，调用方决定降级方式
//
// 写入用后台 context：发起修复的那个请求被取消不该让修复半途而废
// （否则下一次请求又要从头来一遍）。
func (f *FeedService) refreshTimeline(ctx context.Context) (bool, error) {
	sfKey := f.rediscache.Key("sf:fallback:global_timeline_rebuild")
	v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
		// 无视游标，直接去 MySQL 捞最新的 1000 条
		dbVideos, err := f.repo.ListLatest(ctx, timelineRebuildLimit, time.Time{})
		if err != nil {
			return false, err
		}
		if len(dbVideos) == 0 {
			return false, nil
		}

		zElements := make([]redis.Z, 0, len(dbVideos))
		for _, vid := range dbVideos {
			zElements = append(zElements, redis.Z{
				Score:  float64(vid.CreateTime.UnixMilli()),
				Member: fmt.Sprintf("%d", vid.ID),
			})
		}

		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := f.rediscache.ZAdd(bgCtx, f.rediscache.Key("feed:global_timeline"), zElements...); err != nil {
			return false, err
		}
		// 为什么是 Info：突发发布时消费者滞后一两秒是正常的，用 Warn 会让
		// 正常流量刷告警；而"消费者真的挂了"这件事看的是这条日志的**频率**，
		// 单条级别判断不出来。条数一并打出来，便于区分抖动与故障。
		logging.Ctx(ctx).Info("时间线已从数据库补齐",
			zap.Int("repaired", len(zElements)))
		return true, nil
	})
	if err != nil {
		return false, err
	}
	repaired, _ := v.(bool)
	return repaired, nil
}

func (f *FeedService) listLatestFromDB(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	videos, err := f.repo.ListLatest(ctx, limit, latestBefore)
	if err != nil {
		return ListLatestResponse{}, err
	}
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLatestResponse{}, err
	}
	var nextTime int64
	if len(videos) > 0 {
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}
	return ListLatestResponse{
		VideoList: feedVideos,
		NextTime:  nextTime,
		HasMore:   len(videos) == limit,
	}, nil
}

// 按照点赞数查询视频
func (f *FeedService) ListLikesCount(ctx context.Context, limit int, cursor *LikesCountCursor, viewerAccountID uint) (ListLikesCountResponse, error) {
	videos, err := f.repo.ListLikesCountWithCursor(ctx, limit, cursor)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	hasMore := len(videos) == limit
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	resp := ListLikesCountResponse{
		VideoList: feedVideos,
		HasMore:   hasMore,
	}
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextLikesCountBefore := last.LikesCount
		nextIDBefore := last.ID
		resp.NextLikesCountBefore = &nextLikesCountBefore
		resp.NextIDBefore = &nextIDBefore
	}
	return resp, nil
}

// 按照关注列表查询视频
func (f *FeedService) ListByFollowing(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListByFollowingResponse, error) {
	doListByFollowingFromDB := func() (ListByFollowingResponse, error) {
		videos, err := f.repo.ListByFollowing(ctx, limit, viewerAccountID, latestBefore)
		if err != nil {
			return ListByFollowingResponse{}, err
		}
		var nextTime int64
		if len(videos) > 0 {
			nextTime = videos[len(videos)-1].CreateTime.Unix()
		} else {
			nextTime = 0
		}
		hasMore := len(videos) == limit
		feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
		if err != nil {
			return ListByFollowingResponse{}, err
		}
		resp := ListByFollowingResponse{
			VideoList: feedVideos,
			NextTime:  nextTime,
			HasMore:   hasMore,
		}
		return resp, nil
	}
	var cacheKey string
	if viewerAccountID != 0 && f.rediscache != nil {
		before := int64(0)
		if !latestBefore.IsZero() {
			before = latestBefore.Unix()
		}
		cacheKey = f.rediscache.Key("feed:listByFollowing:limit=%d:accountID=%d:before=%d", limit, viewerAccountID, before)
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		b, err := f.rediscache.GetBytes(cacheCtx, cacheKey)
		if err == nil {
			var cached ListByFollowingResponse
			if err := json.Unmarshal(b, &cached); err == nil {
				return cached, nil
			}
		} else if rediscache.IsMiss(err) { // 缓存未命中
			lockKey := "lock:" + cacheKey
			// 缓存未命中，尝试加锁
			token, locked, _ := f.rediscache.Lock(cacheCtx, lockKey, 500*time.Millisecond)
			if locked {
				defer func() { _ = f.rediscache.Unlock(context.Background(), lockKey, token) }()
				if b, err := f.rediscache.GetBytes(cacheCtx, cacheKey); err == nil {
					var cached ListByFollowingResponse
					if err := json.Unmarshal(b, &cached); err == nil {
						return cached, nil
					}
				} else { // 缓存未命中，从数据库中查询
					resp, err := doListByFollowingFromDB()
					if err != nil {
						return ListByFollowingResponse{}, err
					}
					if b, err := json.Marshal(resp); err == nil {
						_ = f.rediscache.SetBytes(cacheCtx, cacheKey, b, f.cacheTTL)
					}
					return resp, nil
				}
			} else {
				for i := 0; i < 5; i++ {
					time.Sleep(20 * time.Millisecond)
					if b, err := f.rediscache.GetBytes(cacheCtx, cacheKey); err == nil {
						var cached ListByFollowingResponse
						if err := json.Unmarshal(b, &cached); err == nil {
							return cached, nil
						}
					}
				}
			}
		}
	}

	resp, err := doListByFollowingFromDB()
	if err != nil {
		return ListByFollowingResponse{}, err
	}
	if cacheKey != "" {
		if b, err := json.Marshal(resp); err == nil {
			cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			_ = f.rediscache.SetBytes(cacheCtx, cacheKey, b, f.cacheTTL)
		}
	}
	return resp, nil
}

func (f *FeedService) ListByPopularity(ctx context.Context, limit int, reqAsOf int64, offset int, viewerAccountID uint, latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error) {
	// Redis 热榜（稳定分页：as_of + offset）
	if f.rediscache != nil {
		asOf := time.Now().UTC().Truncate(time.Minute)
		if reqAsOf > 0 {
			asOf = time.Unix(reqAsOf, 0).UTC().Truncate(time.Minute)
		}

		const win = 60
		keys := make([]string, 0, win)
		for i := 0; i < win; i++ {
			keys = append(keys, f.rediscache.Key("hot:video:1m:%s", asOf.Add(-time.Duration(i)*time.Minute).Format("200601021504")))
		}

		dest := f.rediscache.Key("hot:video:merge:1m:%s", asOf.Format("200601021504")) // 快照key：同一个as_of页内复用
		opCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		defer cancel()

		exists, _ := f.rediscache.Exists(opCtx, dest)
		if !exists {
			_ = f.rediscache.ZUnionStore(opCtx, dest, keys, "SUM")
			_ = f.rediscache.Expire(opCtx, dest, 2*time.Minute) // 给翻页留时间
		}

		start := int64(offset)
		stop := start + int64(limit) - 1
		members, err := f.rediscache.ZRevRange(opCtx, dest, start, stop)
		if err == nil && len(members) == 0 {
			if offset > 0 {
				return ListByPopularityResponse{
					VideoList:  []FeedVideoItem{},
					AsOf:       asOf.Unix(),
					NextOffset: offset,
					HasMore:    false,
				}, nil
			}
		}
		if err == nil && len(members) > 0 {
			ids := make([]uint, 0, len(members))
			for _, m := range members {
				u, err := strconv.ParseUint(m, 10, 64)
				if err == nil && u > 0 {
					ids = append(ids, uint(u))
				}
			}

			videos, err := f.repo.GetByIDs(ctx, ids)
			if err == nil {
				byID := make(map[uint]*video.Video, len(videos))
				for _, v := range videos {
					byID[v.ID] = v
				}
				ordered := make([]*video.Video, 0, len(ids))
				for _, id := range ids {
					if v := byID[id]; v != nil {
						ordered = append(ordered, v)
					}
				}
				items, err := f.buildFeedVideos(ctx, ordered, viewerAccountID)
				if err != nil {
					return ListByPopularityResponse{}, err
				}
				resp := ListByPopularityResponse{
					VideoList:  items,
					AsOf:       asOf.Unix(),
					NextOffset: offset + len(items),
					HasMore:    len(items) == limit,
				}
				if len(ordered) > 0 {
					last := ordered[len(ordered)-1]
					nextPopularity := last.Popularity
					nextBefore := last.CreateTime
					nextID := last.ID
					resp.NextLatestPopularity = &nextPopularity
					resp.NextLatestBefore = &nextBefore
					resp.NextLatestIDBefore = &nextID
				}
				return resp, nil
			}
		}
	}

	videos, err := f.repo.ListByPopularity(ctx, limit, latestPopularity, latestBefore, latestIDBefore)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	items, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	resp := ListByPopularityResponse{
		VideoList:  items,
		AsOf:       0,
		NextOffset: 0,
		HasMore:    len(items) == limit,
	}
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextPopularity := last.Popularity
		nextBefore := last.CreateTime
		nextID := last.ID
		resp.NextLatestPopularity = &nextPopularity
		resp.NextLatestBefore = &nextBefore
		resp.NextLatestIDBefore = &nextID
	}
	return resp, nil
}

// reasonSources 是本页的两路召回候选，只用于生成推荐理由。
//
// 为什么显式传进来而不是在 buildFeedVideos 里重算：重算意味着再读一次
// 向量、再算一次相似度——而理由只是把已经算出来的事实翻译成一句话。
// 两者用同一批数据，也从结构上保证了"理由说的"与"实际发生的召回"一致
// （否则会出现"理由是语义相似，但这条其实是探索位"这种自相矛盾）。
type reasonSources struct {
	semantic []SemanticHit
	explore  []*video.Video
}

func (f *FeedService) buildFeedVideos(ctx context.Context, videos []*video.Video, viewerAccountID uint, recall ...reasonSources) ([]FeedVideoItem, error) {
	feedVideos := make([]FeedVideoItem, 0, len(videos))
	videoIDs := make([]uint, len(videos))
	for i, v := range videos {
		videoIDs[i] = v.ID
	}
	likedMap, err := f.likeRepo.BatchGetLiked(ctx, videoIDs, viewerAccountID)
	if err != nil {
		return nil, err
	}
	summaries := f.latestSummaries(ctx, videoIDs)
	var src reasonSources
	if len(recall) > 0 {
		src = recall[0]
	}
	reasons := f.latestReasons(ctx, videos, viewerAccountID, src.semantic, src.explore)
	for _, video := range videos {
		feedVideos = append(feedVideos, FeedVideoItem{
			ID:          video.ID,
			Author:      FeedAuthor{ID: video.AuthorID, Username: video.Username},
			Title:       video.Title,
			Description: video.Description,
			PlayURL:     video.PlayURL,
			CoverURL:    video.CoverURL,
			CreateTime:  video.CreateTime.Unix(),
			LikesCount:  video.LikesCount,
			IsLiked:     likedMap[video.ID],
			Summary:     summaries[video.ID],
			Reason:      reasons[video.ID],
		})
	}
	return feedVideos, nil
}

// latestSummaries 批量取 AI 摘要。
//
// 三条刻意的设计：
//
//  1. **批量查一次**，不是每个视频查一次：Feed 一次最多返回 50 条，
//     逐条查询会把一次请求变成 50 次往返。
//  2. **失败只降级不报错**：摘要属于锦上添花，查不到就不填。
//     让整条 Feed 因为一张可选表查不动而 500，是拿承重墙给可选功能陪葬
//     （总览第 1 节的三条裁决里最直接的一条）。
//  3. **未注入时直接返回 nil**：AI 关闭的部署连这一次查询都不发。
func (f *FeedService) latestSummaries(ctx context.Context, videoIDs []uint) map[uint]string {
	if f.summaryRepo == nil || len(videoIDs) == 0 {
		return nil
	}
	// 单独给一个短超时：摘要查询是可选路径，不该占用整个 Feed 请求的预算。
	// 50ms 足够覆盖一次带索引的主键查询；超时就当作"没有摘要"。
	opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	summaries, err := f.summaryRepo.LatestAISummaries(opCtx, videoIDs)
	if err != nil {
		// 可选能力失败：不记 Warn 而是记 Debug？
		// 这里选择 Warn 但只记一次摘要级别的原因：它确实代表"AI 摘要这条链路
		// 出了问题"（表被删、库压力大），运维需要知道；而它不影响 Feed 结果，
		// 所以不该是 Error。频率由 50ms 超时与索引保证，不会刷屏。
		logging.Ctx(ctx).Warn("查询 AI 摘要失败，本次 Feed 不返回摘要（不影响 Feed 结果）",
			zap.Int("videos", len(videoIDs)), zap.Error(err))
		return nil
	}
	return summaries
}

func buildOrderedResult(orderedIDs []uint, dataMap map[uint]*video.Video) []*video.Video {
	res := make([]*video.Video, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		if v, exits := dataMap[id]; exits && v != nil {
			res = append(res, v)
		}
	}
	return res
}

func (f *FeedService) ListByTag(ctx context.Context, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.ListByTag(ctx, tagName, limit)
	if err != nil {
		return nil, err
	}
	return f.buildFeedVideos(ctx, videos, viewerAccountID)
}
