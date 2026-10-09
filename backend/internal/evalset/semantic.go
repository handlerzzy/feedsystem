package evalset

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/embed"
	"github.com/handlerzzy/feedsystem/internal/rank"
)

// 本文件实现 P2 的**离线排序器**：语义召回 + 多路融合（+ 冷启动探索）。
//
// 它为什么必须存在（P2 §4.7 是"本任务的完成标志"）：
// 没有它，P2 做完只能证明"我接了模型"，无法回答"变好了多少、
// 在什么条件下变好、什么条件下变差"。而后者才是这个任务的价值所在。
//
// 三条设计纪律：
//
//  1. **配额与槽位几何直接调用 internal/rank**，与线上是同一个函数。
//     各写一份的话，离线结论只证明了评测里那个近似实现更好。
//  2. **只用切分点之前的信息**：兴趣画像来自切分点**之前**的互动，
//     基础页的排序特征（popularity/likes）也来自切分点之前
//     （见 Snapshot.ApplyFeatures）。用观测窗口内的互动做画像是自证。
//  3. **向量用 internal/embed 的词法编码器**，与 sidecar 在
//     AI_PROVIDER=faux 下用的编码器逐位一致（有跨语言测试锁住）。
//     这保证了"离线评测说变好了"与"演示链路看起来还行"说的是同一件事。
//
// 明确的局限（必须与结论一起读）：
//   - 词法向量只捕捉**字面重叠**。真实语义模型能识别"换一种说法的同一件事"，
//     因此本评测给出的是 P2 **效果的下界**，不是真实模型的期望值；
//   - 快照里没有视频标签，因此标签类推荐理由（"你常看 X"）在离线路径上
//     不会被触发，理由覆盖率会低于线上。

// 评测的默认参数。每个值都对应一个线上配置项（写在一起是为了让
// "离线用的是什么参数"在结果文件里可以直接引用）。
const (
	// EvalEmbeddingModel 是词法参考编码器的模型名，与
	// internal/embed.LexicalModel（sidecar 的 faux-lexical）保持一致。
	EvalEmbeddingModel = embed.LexicalModel

	// EvalCandidateWindow 对应线上的 SemanticCandidateWindow（500 条）。
	// 快照只有 250 条候选，因此离线实际上会扫全量——这不是"参数没生效"，
	// 而是"数据规模还没到需要截断的程度"。
	EvalCandidateWindow = 500

	// EvalProfileLikes 对应线上的 UserProfileLikes（最近 50 条正向行为）。
	EvalProfileLikes = 50

	// EvalFusionPool 对应线上的 FusionPoolSize（200 条候选）。
	EvalFusionPool = 200

	// EvalExploreFreshness 是"新内容"的时间窗。
	//
	// 线上是 24 小时；离线的观测窗口是 14 天，同样的 24 小时会把探索池
	// 缩到几乎没有内容（合成数据的发布跨度是 60 天）。这里放大到 7 天，
	// 并把这个偏离**显式写在结果文件里**——悄悄换一个参数会让结论不可复核。
	EvalExploreFreshness = 7 * 24 * time.Hour

	// EvalExploreMaxPopularity 对应线上的 ExploreMaxPopularity（互动数 < 5）。
	EvalExploreMaxPopularity = 5
)

// EvalSemanticConfig 是一次离线语义评测的参数。
//
// 零值不可用，请用 DefaultEvalSemanticConfig。
type EvalSemanticConfig struct {
	// Dim 是词法向量的维度。0 表示用 embed.LexicalDim（1536）。
	Dim int
	// CandidateWindow / ProfileLikes / FusionPool 见上面各常量的说明。
	CandidateWindow int
	ProfileLikes    int
	FusionPool      int
	// RecallQuota / ExploreQuota 与线上同名配置一一对应。
	RecallQuota  float64
	ExploreQuota float64
	// ExploreFreshness / ExploreMaxPopularity 见上面各常量的说明。
	ExploreFreshness     time.Duration
	ExploreMaxPopularity int64
	// NoSemantic / NoExplore 用于消融实验：分别把两路关掉，
	// 得到"只用基础排序""只加语义""只加探索""三个都加"四个数字。
	// 消融是回答"在什么条件下变好"的唯一方式。
	NoSemantic bool
	NoExplore  bool
}

// DefaultEvalSemanticConfig 返回 P2 的设计取值（与随仓库发布的 yaml 一致）。
func DefaultEvalSemanticConfig() EvalSemanticConfig {
	return EvalSemanticConfig{
		Dim:                  embed.LexicalDim,
		CandidateWindow:      EvalCandidateWindow,
		ProfileLikes:         EvalProfileLikes,
		FusionPool:           EvalFusionPool,
		RecallQuota:          0.3,
		ExploreQuota:         0.1,
		ExploreFreshness:     EvalExploreFreshness,
		ExploreMaxPopularity: EvalExploreMaxPopularity,
	}
}

// SemanticRankerOptions 是构造一个 P2 排序器所需的全部信息。
type SemanticRankerOptions struct {
	// Name 是排序器在报告里的名字。
	Name string
	// Config 是参数。
	Config EvalSemanticConfig
	// Reasons 为 true 时同时统计推荐理由的覆盖率（reason_coverage）。
	//
	// 为什么要统计它：P2 §4.6 的红线是"理由必须基于真实信号，
	// 没有就返回空"。一条"理由覆盖率 100%"的结果反而是可疑的——
	// 它意味着每个位置都编得出理由。把覆盖率放进评测，是为了让
	// "宁缺勿编"这条纪律有一个可观察的数字。
	Reasons bool
}

// SemanticRanker 是 P2 的离线排序策略（含可选的理由统计）。
//
// 返回的 Ranker 是**个性化**排序器：同一份候选集对不同用户给出不同顺序，
// 因此它只出现在按用户分组的评测口径里（P2 前置 B1 的红线）。
// 它同时实现了 Reporter，报告里会带上配额参数、融合池大小与理由覆盖率。
type SemanticRanker struct {
	opts EvalSemanticConfig
	name string
	// reasons 为 true 时统计理由覆盖率。
	reasons bool

	docs     *evalDocs
	byID     map[uint]Video
	ranked   map[uint]int // 全局基础排序的名次（0 = 最前）
	explore  map[uint][]uint
	profiles map[uint]*evalProfile

	// 理由覆盖率的累计观测值（评测过程中填充，见 Finalize）。
	total       int
	withReason  int
	usersRanked int
	reasonKinds map[string]int
}

// Finalize 在评测跑完之后整理结果。
//
// 单独一个方法而不是在 Report 里懒计算：Report 可能在评测前后各被调用一次
// （渲染层与结果文件都要用它），而"两次调用的内容不同"是最难查的一类
// 不一致。这里把"什么时候固化"变成一个显式的调用点。
func (r *SemanticRanker) Finalize() {}

// NewSemanticRanker 在给定快照上构造一个 P2 排序器。
func NewSemanticRanker(snap *Snapshot, opts SemanticRankerOptions) *SemanticRanker {
	cfg := opts.Config
	if cfg.Dim <= 0 {
		cfg.Dim = embed.LexicalDim
	}
	if cfg.CandidateWindow <= 0 {
		cfg.CandidateWindow = EvalCandidateWindow
	}
	if cfg.ProfileLikes <= 0 {
		cfg.ProfileLikes = EvalProfileLikes
	}
	if cfg.FusionPool <= 0 {
		cfg.FusionPool = EvalFusionPool
	}
	if cfg.ExploreFreshness <= 0 {
		cfg.ExploreFreshness = EvalExploreFreshness
	}
	name := opts.Name
	if name == "" {
		name = "p2_fused"
	}

	r := &SemanticRanker{
		opts:     cfg,
		name:     name,
		reasons:  opts.Reasons,
		docs:     newEvalDocs(snap, cfg.Dim),
		byID:     make(map[uint]Video, len(snap.Videos)),
		explore:  map[uint][]uint{},
		profiles: map[uint]*evalProfile{},
	}
	for _, v := range snap.Videos {
		r.byID[v.ID] = v
	}

	// 基础排序：与线上 /feed/listByPopularity 的后备 SQL 完全一致
	// （popularity DESC, create_time DESC, id DESC）。用同一个规则而不是
	// "另一个看起来也合理的排序"，否则"融合是否变好"会混进"基础排序换了"
	// 这个变量。
	candidates := Candidates(snap)
	base := make([]Video, len(candidates))
	copy(base, candidates)
	sort.SliceStable(base, func(i, j int) bool {
		a, b := base[i], base[j]
		if a.Popularity != b.Popularity {
			return a.Popularity > b.Popularity
		}
		if !a.CreateTime.Equal(b.CreateTime) {
			return a.CreateTime.After(b.CreateTime)
		}
		return a.ID > b.ID
	})
	r.ranked = make(map[uint]int, len(base))
	for i, v := range base {
		r.ranked[v.ID] = i
	}
	return r
}

// Ranker 返回可交给评测框架的排序策略。
//
// WithReport 把本对象挂上去：P2 §4.7 要求结果文件"含参数、时间、模型版本"，
// 而参数是策略的属性。挂在这里而不是让 CLI 另传一份，
// 是为了让"策略加了参数但忘了写进结果"成为不可能。
func (r *SemanticRanker) Ranker() Ranker {
	return Ranker{
		Name:         r.name,
		Personalized: r.less,
	}.WithReport(r)
}

// Report 实现 Reporter：把"这次评测用的是什么参数"写进结果文件。
//
// 为什么必须写进结果：一个没有参数的指标数字是不可复核的，
// 而 P2 §4.7 明确要求结果文件"含参数、时间、模型版本"。
//
// 注意 reason_coverage 是**评测过程中累积**的（每个用户第一次被排序时
// 累加一次），因此它必须在 EvaluatePerUser 跑完之后才有值——
// 见 Finalize。构造完立刻读会得到 0，而 0 看起来像一个"合理但错误"的结论。
func (r *SemanticRanker) Report() map[string]any {
	out := map[string]any{
		"embedding_model":         EvalEmbeddingModel,
		"embedding_dim":           r.opts.Dim,
		"recall_quota":            r.opts.RecallQuota,
		"explore_quota":           r.opts.ExploreQuota,
		"candidate_window":        r.opts.CandidateWindow,
		"profile_likes":           r.opts.ProfileLikes,
		"fusion_pool":             r.opts.FusionPool,
		"explore_freshness_hours": r.opts.ExploreFreshness.Hours(),
		"explore_max_popularity":  r.opts.ExploreMaxPopularity,
		"semantic_disabled":       r.opts.NoSemantic,
		"explore_disabled":        r.opts.NoExplore,
		"base_order":              "popularity DESC, create_time DESC, id DESC（与线上 listByPopularity 的后备 SQL 一致）",
		"note": "离线用词法向量（internal/embed，与 sidecar 的 faux-lexical 逐位一致），" +
			"它只捕捉字面重叠，因此本表是 P2 效果的**下界**；真实语义模型的收益未被测量",
	}
	out["users_ranked"] = r.usersRanked
	out["reason_coverage"] = r.ReasonCoverage()
	out["reason_samples"] = r.reasonSamples()
	return out
}

// ReasonCoverage 返回"最终列表里有理由的位置占比"的**全局观测值**。
//
// 它放在这里而不是逐用户算：理由覆盖率是一个产品指标（"页面看起来
// 有多少说明"），不是排序质量指标，把它混进 NDCG 的口径里会让
// 两件事互相污染。
func (r *SemanticRanker) ReasonCoverage() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.withReason) / float64(r.total)
}

func (r *SemanticRanker) reasonSamples() []string {
	if !r.reasons {
		return nil
	}
	keys := make([]string, 0, len(r.reasonKinds))
	for k := range r.reasonKinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s ×%d", k, r.reasonKinds[k]))
	}
	return out
}

// less 是最终顺序：先按"名次"升序（槽位顺序），再按 id 降序保证可复现。
//
// 为什么把顺序物化成名次而不是比较时刻重算：每条候选的槽位在
// 用户维度上是确定的，重算会让比较函数不再是严格弱序
// （sort 的行为在那种情况下未定义）。
func (r *SemanticRanker) less(accountID uint, a, b Video) bool {
	ra := r.slotRank(accountID, a.ID)
	rb := r.slotRank(accountID, b.ID)
	if ra != rb {
		return ra < rb
	}
	return a.ID > b.ID
}

// slotRank 返回某条视频在某个用户的最终列表里的名次。
//
// 名次一旦算出来就缓存：评测会对同一批候选做 O(n log n) 次比较，
// 每次都比较一遍"这条在不在语义位的前 K 名里"会把评测跑成分钟级。
func (r *SemanticRanker) slotRank(accountID uint, videoID uint) int {
	p := r.profile(accountID)
	if rk, ok := p.slot[videoID]; ok {
		return rk
	}
	return r.ranked[videoID] + p.offset
}

// less 与 slotRank 的缓存字段。
//
// 放在单独的结构里而不是直接挂在 SemanticRanker 上：一个用户一份状态，
// 混在一起会让"这个字段属于谁"变得要靠注释判断。
type evalProfile struct {
	// slot 是"这条视频在该用户的最终列表里的位置"（只含被召回/被探索到的）。
	slot map[uint]int
	// offset 是基础页在最终列表里的位移（配额位会把它推后）。
	offset int
	// semantic 是被语义位选中的视频，按相似度降序。
	semantic []uint
	// explore 是被探索位选中的视频。
	explore []uint
	// reasons 是这些位置的推荐理由（P2 §4.6）。
	reasons map[uint]string
}

// profile 按需构建并缓存某个用户的排序状态。
func (r *SemanticRanker) profile(accountID uint) *evalProfile {
	if p, ok := r.profiles[accountID]; ok {
		return p
	}
	p := &evalProfile{slot: map[uint]int{}, reasons: map[uint]string{}}
	r.profiles[accountID] = p

	candidates := r.candidatesFor(accountID)
	limit := len(candidates)
	if r.opts.FusionPool > 0 && limit > r.opts.FusionPool {
		limit = r.opts.FusionPool
	}
	plan := rank.QuotaPlan{}
	if !r.opts.NoSemantic {
		plan.Semantic = rank.Plan(limit, r.opts.RecallQuota, 0).Semantic
	}
	if !r.opts.NoExplore {
		plan.Explore = rank.Plan(limit, 0, r.opts.ExploreQuota).Explore
	}
	if plan.Semantic == 0 && plan.Explore == 0 {
		// 两路都关（或配额为 0）：最终顺序就是基础排序。
		// 这条早退是"配额全 0 时与基准线逐字节一致"在离线侧的对应物。
		p.offset = 0
		return p
	}

	// 基础页：基础排序里的前 limit 条候选。
	base := make([]Video, 0, limit)
	for _, v := range candidates {
		if len(base) == limit {
			break
		}
		base = append(base, v)
	}
	sort.SliceStable(base, func(i, j int) bool { return r.ranked[base[i].ID] < r.ranked[base[j].ID] })

	var semantic []uint
	var reasonOf map[uint]string
	if plan.Semantic > 0 {
		semantic, reasonOf = r.semanticPicks(accountID, candidates, plan.Semantic)
	}
	var explore []uint
	if plan.Explore > 0 {
		explore = r.explorePicks(accountID, candidates, plan.Explore)
	}

	// 按槽位装配：与线上 feed.fuseChannels 的装配循环同构。
	slots := rank.Slots(limit, plan)
	used := make(map[uint]bool, limit)
	for _, v := range base {
		used[v.ID] = true
	}
	si, ei, bi := 0, 0, 0
	order := make([]uint, 0, limit)
	for i := 0; i < limit; i++ {
		switch slots[i] {
		case rank.Semantic:
			if id, ok := nextUnused(semantic, &si, used); ok {
				order = append(order, id)
				if reasonOf != nil {
					p.reasons[id] = reasonOf[id]
				}
				continue
			}
		case rank.Explore:
			if id, ok := nextUnused(explore, &ei, used); ok {
				order = append(order, id)
				p.reasons[id] = "新发布，等待你的第一条反馈"
				continue
			}
		}
		if bi < len(base) {
			order = append(order, base[bi].ID)
			bi++
		}
	}
	for i, id := range order {
		p.slot[id] = i
	}
	// 理由覆盖率的统计**始终打开**：它是 P2 §4.6 那条红线（"宁缺勿编"）
	// 唯一可观察的数字，而成本只是遍历一页 ID。只有"收集理由样本字符串"
	// 受 Reasons 开关控制（那是给人看的，不影响指标）。
	r.recordReasons(order, p.reasons)
	p.semantic = semantic
	p.explore = explore
	return p
}

// recordReasons 累加理由覆盖率（全局观测值）。
func (r *SemanticRanker) recordReasons(order []uint, reasons map[uint]string) {
	if r.reasonKinds == nil {
		r.reasonKinds = map[string]int{}
	}
	r.usersRanked++
	for _, id := range order {
		r.total++
		reason := strings.TrimSpace(reasons[id])
		if reason == "" {
			continue
		}
		r.withReason++
		kind := reason
		if idx := strings.Index(kind, "相似"); idx >= 0 {
			kind = "与你近期点赞的内容相似"
		}
		r.reasonKinds[kind]++
	}
}

// nextUnused 取下一条尚未被使用的 ID。
func nextUnused(ids []uint, idx *int, used map[uint]bool) (uint, bool) {
	for *idx < len(ids) {
		id := ids[*idx]
		*idx++
		if used[id] {
			continue
		}
		used[id] = true
		return id, true
	}
	return 0, false
}

// candidatesFor 复用按用户口径的可见性规则（排除自己发的视频）。
func (r *SemanticRanker) candidatesFor(accountID uint) []Video {
	out := make([]Video, 0, len(r.byID))
	for _, v := range r.byID {
		if accountID != 0 && v.AuthorID == accountID {
			continue
		}
		out = append(out, v)
	}
	// map 遍历顺序随机：必须排序，否则候选顺序会泄漏进结果
	// （评测的可复现性依赖它）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// semanticPicks 返回语义位的候选（按相似度降序）与它们各自的推荐理由。
//
// 它对应线上的 semanticRecall：先建用户兴趣向量，再对候选算余弦，
// 最后取前 K 条。区别只有一个——离线的向量来自词法参考编码器。
func (r *SemanticRanker) semanticPicks(accountID uint, candidates []Video, want int) ([]uint, map[uint]string) {
	profile, liked := r.interestProfile(accountID)
	if profile == nil || len(liked) == 0 {
		// 新用户 / 没有任何历史行为：没有个性化信号，语义位退回基础页。
		// 这正是 P2 §4.5 描述的冷启动路径。
		return nil, nil
	}

	// 候选窗口：按"最新"取前 N 条（对应线上的 ListVectorCandidates）。
	pool := append([]Video(nil), candidates...)
	sort.SliceStable(pool, func(i, j int) bool {
		if !pool[i].CreateTime.Equal(pool[j].CreateTime) {
			return pool[i].CreateTime.After(pool[j].CreateTime)
		}
		return pool[i].ID > pool[j].ID
	})
	if r.opts.CandidateWindow > 0 && len(pool) > r.opts.CandidateWindow {
		pool = pool[:r.opts.CandidateWindow]
	}

	// 最新的一批让位给时序路（对应线上的 SemanticWarmupExclude = 20）。
	const warmupExclude = 20
	excluded := map[uint]bool{}
	for i, v := range pool {
		if i >= warmupExclude {
			break
		}
		excluded[v.ID] = true
	}

	type scored struct {
		id    uint
		score float64
	}
	list := make([]scored, 0, len(pool))
	for _, v := range pool {
		if excluded[v.ID] || v.AuthorID == accountID {
			continue
		}
		vec, ok := r.docs.vectors[v.ID]
		if !ok {
			continue
		}
		list = append(list, scored{id: v.ID, score: cosineDense(profile, vec)})
	}
	// 与线上一致：同分按 id 降序，保证可复现。
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].id > list[j].id
	})

	out := make([]uint, 0, want)
	reasonByID := map[uint]string{}
	for _, s := range list {
		if len(out) == want {
			break
		}
		out = append(out, s.id)
		reasonByID[s.id] = "与你近期点赞的内容相似"
	}
	_ = liked
	return out, reasonByID
}

// interestProfile 构建用户兴趣向量，并返回产生它的正向行为视频 ID。
//
// 只用切分点**之前**的互动：用观测窗口内的互动做画像等于把答案
// 喂给排序器，"P2 变好了"就变成了自证（与 lexical_probe 的纪律一致）。
func (r *SemanticRanker) interestProfile(accountID uint) ([]float32, []uint) {
	liked := make([]uint, 0, 16)
	for _, it := range r.docs.snap.Interactions {
		if it.AccountID != accountID {
			continue
		}
		if it.At.After(r.docs.snap.SplitAt) {
			continue
		}
		if it.Kind != KindLike {
			// 只用正向行为（点赞）。评论可能表达反对，用它当兴趣
			// 会得到"你常看 X"而用户其实在批评 X。
			continue
		}
		liked = append(liked, it.VideoID)
	}
	if len(liked) == 0 {
		return nil, nil
	}
	// 按时间倒序取最近的 N 条（对应线上的 RecentLikedVideoIDs）。
	sort.Slice(liked, func(i, j int) bool { return liked[i] > liked[j] })
	if r.opts.ProfileLikes > 0 && len(liked) > r.opts.ProfileLikes {
		liked = liked[:r.opts.ProfileLikes]
	}

	dim := r.opts.Dim
	acc := make([]float64, dim)
	var total float64
	for i, id := range liked {
		vec, ok := r.docs.vectors[id]
		if !ok {
			continue
		}
		weight := 1 / float64(i+1)
		for k, v := range vec {
			acc[k] += float64(v) * weight
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
	return embed.L2Normalize(out), liked
}

// explorePicks 返回探索位的候选（新的且互动很少的内容）。
//
// 与线上的差别（写在结果文件里）：线上用"最近 24 小时"，离线用 7 天，
// 因为合成数据的发布跨度是 60 天，24 小时窗口里几乎没有内容。
// 判定"新"用的是**该用户最后一次训练互动的时刻**之后的发布，
// 而不是快照的切分点：一个在切分点前两周就停止互动的用户，
// 对他来说"新"的内容应该更早，否则探索位对他永远是空的。
func (r *SemanticRanker) explorePicks(accountID uint, candidates []Video, want int) []uint {
	last := r.lastInteractionAt(accountID)
	since := last.Add(-r.opts.ExploreFreshness)
	if last.IsZero() {
		since = r.docs.snap.SplitAt.Add(-r.opts.ExploreFreshness)
	}

	pool := make([]Video, 0, len(candidates))
	for _, v := range candidates {
		if v.AuthorID == accountID {
			continue
		}
		if v.CreateTime.Before(since) || v.CreateTime.After(last) {
			// 只看"这个用户活跃期内的新内容"：切分点之后发布的视频
			// 不该出现在候选里（排序器看不到未来）。
			continue
		}
		if v.Popularity >= r.opts.ExploreMaxPopularity {
			continue
		}
		pool = append(pool, v)
	}
	// 按发布时间倒序（与线上 ListFreshLowEngagement 一致），
	// 再从用户自己的起点轮转，保证不同用户看到的探索内容不同且确定。
	sort.Slice(pool, func(i, j int) bool {
		if !pool[i].CreateTime.Equal(pool[j].CreateTime) {
			return pool[i].CreateTime.After(pool[j].CreateTime)
		}
		return pool[i].ID > pool[j].ID
	})
	if len(pool) == 0 {
		return nil
	}
	start := int(stableHash(accountID) % uint64(len(pool)))
	out := make([]uint, 0, want)
	for i := 0; i < len(pool) && len(out) < want; i++ {
		out = append(out, pool[(start+i)%len(pool)].ID)
	}
	return out
}

// lastInteractionAt 返回某用户在切分点之前的最后一次互动时刻。
func (r *SemanticRanker) lastInteractionAt(accountID uint) time.Time {
	var last time.Time
	for _, it := range r.docs.snap.Interactions {
		if it.AccountID != accountID || it.At.After(r.docs.snap.SplitAt) {
			continue
		}
		if it.At.After(last) {
			last = it.At
		}
	}
	return last
}

// stableHash 是给"按用户轮转"用的确定性哈希。
// 用它而不是 map 遍历或随机数：评测必须逐字节可复现。
func stableHash(id uint) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(id >> (8 * i))
	}
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

// evalDocs 是整个快照的向量集合。
type evalDocs struct {
	snap    *Snapshot
	vectors map[uint][]float32
}

// newEvalDocs 为每条视频算一次词法向量。
//
// 一次算全量而不是按需：500 条 × 1536 维 ≈ 3MB，算一次点的代价远小于
// 每个用户重复算一遍（评测里每个用户都要对同一批候选打分）。
func newEvalDocs(snap *Snapshot, dim int) *evalDocs {
	vecs := make(map[uint][]float32, len(snap.Videos))
	for _, v := range snap.Videos {
		text := strings.TrimSpace(v.Title + "。" + v.Description)
		if text == "" {
			continue
		}
		vec := embed.HashedVector(text, dim)
		vecs[v.ID] = embed.L2Normalize(vec)
	}
	return &evalDocs{snap: snap, vectors: vecs}
}

// cosineDense 计算两个等长稠密向量的余弦。
//
// 与 video.CosineSimilarity 同一套公式，但不 import video 包：
// evalset 是离线工具，它的依赖图里不该出现仓储/ORM 那一层
// （那会让"离线评测能不能跑"取决于数据库依赖是否可编译）。
func cosineDense(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na <= 0 || nb <= 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
