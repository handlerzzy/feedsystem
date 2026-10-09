package feed

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/video"

	"go.uber.org/zap"
)

// 本文件实现 P2 §4.2「向量召回通道」。
//
// 接口（VectorRecallStore）声明在 feed_store.go —— 消费方声明，实现方满足，
// 与本项目既有惯例一致。这里放的是**消费方这一侧**的逻辑：
// 用户兴趣向量怎么来、候选池怎么圈、过滤与去重怎么做、相似度怎么算。
//
// 为什么检索这一层刻意不进仓储包（P2 前置 B4 的决定）：
// 检索是"把候选向量读进内存再算余弦"，是一个纯计算 + 一次批量读。
// 把它放进仓储层，会让最需要单测覆盖的部分（过滤、去重、排序）
// 必须连上真库才能验证。

// SemanticQuota 相关的常量。
const (
	// SemanticCandidateWindow 是一次语义召回最多考虑多少条**有向量的**视频。
	//
	// 它是 P2 前置 B4「应用内暴力检索」能成立的硬前提：没有它，
	// 向量池一大，每次 Feed 请求都会把整个池子读进内存。
	// 依据与升级触发条件见 AI-P2-设计决定.md 第 1 节
	// （单次扫描量 > 5,000 条或召回段 p99 > 20ms 就必须换 ANN）。
	//
	// 取 500 而不是 200：D4 的 200 是**融合后的候选池**，而语义路需要
	// 一个比它更大的搜索空间，否则"老但相关"的内容在一开始就被截掉了。
	// 500 × 1536 维 float32 ≈ 3MB 内存、一次全量点积约 0.4 MFLOP，
	// 在 D1 的 50ms 预算里只占很小一段（实测见 README 的延迟表）。
	SemanticCandidateWindow = 500

	// SemanticWarmupExclude 是做用户画像时**排除**掉的最新视频条数。
	//
	// 为什么必须排除：最新的一批内容时序路已经保证了它们的曝光，
	// 而且它们往往是"平台上最新的热词"，会被画像当成用户兴趣，
	// 于是语义位里塞满刚发布的视频，与探索配额、时序路三重重复。
	//
	// 取 20（一页）而不是"全部"：排除太多会把真正有用的近期兴趣也排掉，
	// 那时画像只剩很久以前的偏好，反而让语义路变钝。
	SemanticWarmupExclude = 20

	// UserProfileLikes 是构建用户兴趣向量时最多取多少条最近点赞。
	//
	// 上限的意义：兴趣是**会变**的，把三年前的点赞和昨天的等权平均，
	// 画像会停在一个"平均人"上。取 50 条是"最近偏好"与"样本足够稳"
	// 之间的折中（再少则单条噪声大的行为会主导画像）。
	UserProfileLikes = 50

	// SemanticMaxCandidates 是一次召回最多返回多少个候选。
	//
	// 它只需要覆盖"融合阶段可能用到的语义位"，取 3×SemanticCandidateWindow/10
	// 太大也没有意义：真正进入最终列表的只有 recall_quota × limit 条。
	// 留出余量是为了让融合阶段在去重（与基础页重复）之后还有得选。
	SemanticMaxCandidates = 50
)

// SemanticHit 是一条语义召回结果。
type SemanticHit struct {
	Video *video.Video
	// Score 是用户兴趣向量与该视频向量的余弦相似度，范围 [-1, 1]。
	Score float64
}

// semanticRecall 执行一次语义召回。任何失败都返回空结果 + nil 错误。
//
// 为什么**不向上返回错误**：语义路是可缺省的一层（P2 §4.1 的硬要求）。
// 向量表被删、某个 model 的向量还没回填、Redis/DB 抖动——这些都不该
// 让一次 Feed 请求变成 5xx，也不该让调用方去区分"哪种失败"。
// 失败信息通过日志暴露，调用方只看到"这一路这次没有产出"。
//
// 用户维度缺失时（viewerAccountID == 0，即未登录）直接返回空：
// 没有用户就没有兴趣向量，全局兴趣向量会让"个性化"变成一个谎言
// （那只是热榜的另一种算法）。未登录用户退回原有的时序/热榜排序，
// 正是 P2 §4.5 对"新用户"的要求。
func (f *FeedService) semanticRecall(ctx context.Context, viewerAccountID uint, quota QuotaPlan) []SemanticHit {
	if f.vectorStore == nil || quota.Semantic <= 0 || viewerAccountID == 0 {
		return nil
	}
	// 整段共用一个预算：向量读取与相似度计算都在这条路径上，
	// 分开设超时会让总耗时变成两段之和（D1 的 50ms 是**总共**的预算）。
	opCtx, cancel := context.WithTimeout(ctx, f.semanticTimeout)
	defer cancel()

	// 1. 候选池：最近的、有向量的视频。
	//
	// 模型名走 effectiveModel：库里实际存的是哪个空间由**提供方**决定
	// （离线演示时是 faux-lexical），按配置名去读会一条都读不到。
	model := f.effectiveModel(opCtx)
	candidates, err := f.vectorStore.ListVectorCandidates(opCtx, model, f.embeddingDim, SemanticCandidateWindow)
	if err != nil {
		f.logSemanticDegrade(ctx, "列出向量候选失败，语义路本次关闭", err)
		return nil
	}
	if len(candidates) == 0 {
		// 池子为空有两种可能：回填还没跑（正常），或者库里的向量空间刚变了
		// 而我们还在用旧名字读（换模型后的回填刚写完）。第二种只需清一次
		// 缓存就能自愈，因此这里主动失效并**再试一次**——
		// 不重试的话语义路会一直瞎到缓存 TTL 结束，表现为
		// "换完模型要等半分钟才好"，而没人会想到那是缓存。
		f.invalidateEffectiveModel()
		model = f.effectiveModel(opCtx)
		candidates, err = f.vectorStore.ListVectorCandidates(opCtx, model, f.embeddingDim, SemanticCandidateWindow)
		if err != nil || len(candidates) == 0 {
			// 仍然是空：这是**正常状态**（回填还没跑、或 AI 刚打开）。
			// 不记日志：它会每秒钟重复一次，把真正的故障淹掉。
			return nil
		}
	}

	// 2. 用户兴趣向量：最近点赞视频向量的加权平均。
	profile, err := f.userInterestVector(opCtx, viewerAccountID)
	if err != nil {
		f.logSemanticDegrade(ctx, "构建用户兴趣向量失败，语义路本次关闭", err)
		return nil
	}
	if len(profile) == 0 {
		// 新用户 / 没有点赞：没有可用的个性化信号。这是 P2 §4.5 明确
		// 描述的冷启动状态，退回原有排序即可，不是故障。
		return nil
	}

	// 3. 批量读取候选向量。
	ids := make([]uint, 0, len(candidates))
	byID := make(map[uint]video.VectorCandidate, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
		byID[c.ID] = c
	}
	vectors, err := f.vectorStore.LoadVectors(opCtx, ids, model, f.embeddingDim)
	if err != nil {
		f.logSemanticDegrade(ctx, "读取候选向量失败，语义路本次关闭", err)
		return nil
	}
	if len(vectors) == 0 {
		return nil
	}

	// 4. 相似度打分。
	scored := make([]scoredCandidate, 0, len(vectors))
	for id, vec := range vectors {
		if c, ok := byID[id]; ok {
			scored = append(scored, scoredCandidate{id: id, createdAt: c.CreateTime, score: cosine(profile, vec)})
		}
	}
	sortCandidates(scored)

	if len(scored) > SemanticMaxCandidates {
		scored = scored[:SemanticMaxCandidates]
	}

	// 5. 取回完整视频对象（过滤与去重需要它）。
	topIDs := make([]uint, 0, len(scored))
	for _, s := range scored {
		topIDs = append(topIDs, s.id)
	}
	videos, err := f.repo.GetByIDs(ctx, topIDs)
	if err != nil {
		f.logSemanticDegrade(ctx, "读取召回视频失败，语义路本次关闭", err)
		return nil
	}
	videoByID := make(map[uint]*video.Video, len(videos))
	for _, v := range videos {
		videoByID[v.ID] = v
	}

	// 6. 过滤：无效 ID、缺失视频、作者自看。顺序保持打分顺序。
	excluded := f.warmupExclusionSet(opCtx)
	hits := make([]SemanticHit, 0, len(scored))
	for _, s := range scored {
		v := videoByID[s.id]
		if v == nil {
			// 向量表里有、视频表里没有：说明这条视频已被删除（或向量行是
			// 孤儿）。P2 §4.2 的验收项"召回结果中不出现无效 ID"就是这一条。
			continue
		}
		if excluded[v.ID] {
			// 最新的一批：时序路已经保证它们的曝光，语义位留给别的。
			continue
		}
		if f.candidateFilter != nil && !f.candidateFilter(ctx, viewerAccountID, v) {
			continue
		}
		hits = append(hits, SemanticHit{Video: v, Score: s.score})
	}
	return hits
}

// scoredCandidate 是打分阶段的中间结果。
type scoredCandidate struct {
	id        uint
	createdAt time.Time
	score     float64
}

// sortCandidates 按分数降序、同分按 ID 降序。
//
// 为什么必须显式定死同分的次序：map 遍历顺序随机，同分不排序会让同一份
// 数据两次请求返回不同的列表——可复现性是这套东西能被测试的前提
// （与 evalset 里"按 id 降序保证可复现"是同一条纪律）。
// 正相关（score > 0）才保留：余弦为负表示方向相反，把它排进推荐位
// 等于"按用户最不喜欢的主题推荐"，而过零点附近的噪声又会随机换人。
func sortCandidates(items []scoredCandidate) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt)
		}
		return a.id > b.id
	})
}

// warmupExclusionSet 返回"最新的一批视频 ID"，语义路会把它们让给时序路。
//
// 失败时返回空集合而不是错误：排除集是**优化**而不是正确性前提，
// 拿不到它最多是语义位里多几条新视频，不该因此关掉整条语义路。
func (f *FeedService) warmupExclusionSet(ctx context.Context) map[uint]bool {
	fresh, ok := f.repo.(FreshIDLister)
	if !ok {
		return nil
	}
	ids, err := fresh.NewestVideoIDs(ctx, SemanticWarmupExclude)
	if err != nil {
		logging.Ctx(ctx).Warn("读取最新视频 ID 失败，语义路不再排除新内容（不影响 Feed 结果）",
			zap.Error(err))
		return nil
	}
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// userInterestVector 由用户最近点赞的视频向量加权平均得到。
//
// 权重：越近的点赞权重越高（线性衰减 1.0 → 1/n）。
// 为什么不是等权：兴趣是会漂的，等权会让"三年前点赞过 50 条宠物视频"
// 永远压过"昨天点赞了 3 条 Go 视频"，而后者才是当前画像。
// 为什么不是指数衰减：指数衰减下最近一条几乎独裁画像，单条行为的噪声
// （误点、好奇点开）会被放大成整个兴趣向量。
//
// 返回零长切片表示"没有可用的兴趣向量"（没有点赞、或点赞的视频都没有向量）。
func (f *FeedService) userInterestVector(ctx context.Context, accountID uint) ([]float32, error) {
	likedIDs, err := f.vectorStore.RecentLikedVideoIDs(ctx, accountID, UserProfileLikes)
	if err != nil {
		return nil, err
	}
	if len(likedIDs) == 0 {
		return nil, nil
	}
	vectors, err := f.vectorStore.LoadVectors(ctx, likedIDs, f.effectiveModel(ctx), f.embeddingDim)
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		// 点赞过的视频还没有向量（回填没跑到）：语义路无信号。
		return nil, nil
	}

	// 维度在累加前定死：不同维度的向量相加会在 Go 里 panic
	// （索引越界），而"少数几条维度不对"是完全可能的历史数据。
	dim := f.embeddingDim
	acc := make([]float64, dim)
	var total float64
	for rank, id := range likedIDs {
		vec := vectors[id]
		if len(vec) != dim {
			continue
		}
		weight := 1 / float64(rank+1)
		for i, v := range vec {
			acc[i] += float64(v) * weight
		}
		total += weight
	}
	if total == 0 {
		return nil, nil
	}
	out := make([]float32, dim)
	for i := range acc {
		out[i] = float32(acc[i] / total)
	}
	return video.L2Normalize(out), nil
}

// cosine 计算两个向量的余弦相似度。
//
// 长度不一致时按较短的一侧计算并返回 0 之外的**未定义**结果——这里选择
// 直接返回 0：维度不一致说明两边不在同一个向量空间，任何数值都没有意义，
// 而返回 0 会让这条候选自然沉底（而不是被 panic 打断整次请求）。
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na <= 0 || nb <= 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// logSemanticDegrade 记录一次语义路降级。
//
// 用 Info 而不是 Warn：语义路可缺省是**设计**，不是异常。
// "向量还没回填完"在新部署上会持续出现，用 Warn 会让它在告警里
// 与真正的故障无法区分（与时间线补齐那条日志同一个理由）。
// 需要人介入的情形是"持续降级"，那要看的是这条日志的**频率**。
func (f *FeedService) logSemanticDegrade(ctx context.Context, msg string, err error) {
	logging.Ctx(ctx).Info(msg,
		zap.String("model", f.embeddingModel),
		zap.Int("dim", f.embeddingDim),
		zap.Error(err))
}

// embeddingModelCacheTTL 是"当前在用的向量空间名"这个结论的缓存时长。
//
// 它是一次 `GROUP BY model` 查询（向量表在 (model,dim) 上有索引，
// 但 GROUP BY 仍然要扫一批行），因此**每个请求都查一次是不可接受的**：
// 那等于把"避免打库"这件事取消了。
//
// 30 秒的依据：换模型（含从真实 provider 切到离线演示）是一个运维动作，
// 半分钟内生效完全够用；而超过这个窗口还读不到向量时，
// semanticRecall 会主动失效缓存再解析一次（见该函数），
// 因此"换模型后语义路静默失效"最多持续到下一次请求。
const embeddingModelCacheTTL = 30 * time.Second

// embeddingModelCacheKey 是缓存键。用常量而不是拼 embeddingModel：
// 缓存的内容是"库里实际是什么"，与配置值无关（配置值只在没有任何
// 已入库向量时作为兜底）。
const embeddingModelCacheKey = "feed:embedding_model:effective"

// effectiveModel 返回**读取向量时应该使用的模型名**。
//
// 背景（一个真实会踩的坑）：向量的 model 由提供方决定，不是配置决定。
// 离线演示时 sidecar 用词法编码器，它回的是 faux-lexical，而配置里写的是
// text-embedding-3-small。若按配置名去读，一条向量都读不到，
// 表现为"语义路静默失效"——而日志里一切正常。
//
// 解析顺序：
//
//  1. 缓存（30 秒）；
//  2. 库里最近写入的那个向量空间（这才是"现在正在生产的那批"）；
//  3. 库里没有任何向量时退回配置值——此时语义路本来就无数据可用，
//     退回配置值只是为了让日志里的 model 字段可读。
//
// 解析失败按"没有向量"处理并返回配置值：语义路是可缺省的一层，
// 它不该因为一次统计查询失败而报错，也不该让调用方去区分失败类型。
func (f *FeedService) effectiveModel(ctx context.Context) string {
	if f.vectorStore == nil {
		return f.embeddingModel
	}
	if f.modelCache != nil {
		if v, found := f.modelCache.Get(embeddingModelCacheKey); found {
			if name, ok := v.(string); ok && name != "" {
				return name
			}
		}
	}
	models, err := f.vectorStore.ListEmbeddingModels(ctx, f.embeddingDim, 4)
	if err != nil {
		logging.Ctx(ctx).Info("读取向量空间失败，本次按配置里的模型名尝试（不影响 Feed 结果）",
			zap.String("configured_model", f.embeddingModel), zap.Error(err))
		return f.embeddingModel
	}
	name := f.embeddingModel
	for _, m := range models {
		if m.Rows > 0 && m.Model != "" {
			name = m.Model
			break
		}
	}
	if name != f.embeddingModel {
		// 只在两者不同时记日志：正常情况下（配置名就是库里的名字）
		// 每个进程启动后记一条没有信息量的日志只是噪音。
		logging.Ctx(ctx).Info("语义路使用的向量空间与配置的模型名不同（按库里的实际空间读取）",
			zap.String("configured_model", f.embeddingModel),
			zap.String("effective_model", name),
			zap.Int("dim", f.embeddingDim))
	}
	if f.modelCache != nil {
		f.modelCache.Set(embeddingModelCacheKey, name, embeddingModelCacheTTL)
	}
	return name
}

// invalidateEffectiveModel 清掉模型解析缓存。
//
// 什么时候需要它：一次读取返回了 0 条候选时，最可能的原因就是
// "库里的向量空间变了"（刚跑完一轮换模型后的回填）。
// 不主动失效的话，语义路会一直用旧名字读到 TTL 结束——表现为
// "换完模型之后要等半分钟才好"。
func (f *FeedService) invalidateEffectiveModel() {
	if f.modelCache != nil {
		f.modelCache.Delete(embeddingModelCacheKey)
	}
}
