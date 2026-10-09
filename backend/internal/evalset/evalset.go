// Package evalset 提供**离线评测集**与排序指标：把"现在的排序到底有多好"
// 变成一个可复现的数字。
//
// 为什么需要它（这是 P0 里最容易被跳过、但对 P2 最要紧的一件事）：
// P2 要引入语义召回与精排，如果现在没有一个基线数字，P2 做完就只能说
// "我接了模型"，无法证明"变好了"。评测集是唯一能把这件事从主观判断
// 变成客观比较的东西，而且它不占开发时间——数据本来就在库里。
//
// 设计上的两个关键决定：
//
//  1. **按时间切分，不使用未来信息**。候选集是 t0 之前发布的视频，
//     标签来自 t0 **之后**发生的互动（点赞/评论）。这与线上推荐的真实处境
//     一致：排序时能看到的是视频自身的历史表现，要预测的是它接下来会不会
//     被互动。用全量互动当标签会泄漏未来，把基线数字抬得虚高。
//
//  2. **排序函数一行不改地照抄线上 SQL 的语义**（见 Ranker 的注释）。
//     评测的排序器必须与线上是同一套规则，否则基线数字与线上表现无关，
//     后面也就无法用它判断"精排是否真的更好"。
//
// 局限（写在代码里而不是只写在文档里，因为用的人多半只看代码）：
// 互动标签偏向"已经曝光过的视频"——没被推荐过的视频永远拿不到互动，
// 会被当成不相关。样本量小或曝光极不均匀时，Recall 会被系统性低估。
// 这个偏差对"改造前后对比"影响不大（两边同样偏），但不适合当成绝对质量。
package evalset

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

// Video 是候选集里的一条视频。
//
// 字段刻意与 video.Video 的**线上排序特征**一一对应：评测要复现线上排序，
// 就必须用线上真实拥有的那几个字段，而不是我们希望有的字段。
//
// Title / Description 是 P2 前置项 B2 新增的：语义召回与精排的输入就是
// 这两段文本，没有它们，快照在结构上就无法评估"语义"这件事——
// 无论排序器写得多好，都只能退化回热度与时间。
type Video struct {
	ID          uint      `json:"id"`
	AuthorID    uint      `json:"author_id"`
	CreateTime  time.Time `json:"create_time"`
	LikesCount  int64     `json:"likes_count"`
	Popularity  int64     `json:"popularity"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
}

// Interaction 是一条发生在观测窗口内的互动（用于生成相关性标签）。
type Interaction struct {
	VideoID uint      `json:"video_id"`
	Kind    string    `json:"kind"` // like | comment
	At      time.Time `json:"at"`
	// AccountID 是做出这次互动的用户（P2 前置项 B1 新增）。
	//
	// 为什么必须有它：没有用户维度，"相关性"只能表达成"这条视频后来
	// 被任何人互动过"，也就是全局热度。按用户分组的 Recall/NDCG 是
	// 个性化推荐唯一可用的度量口径（见 peruser.go 的注释）。
	//
	// 0 表示未知用户（旧快照或导出时缺列），这类互动不参与按用户评测。
	AccountID uint `json:"account_id,omitempty"`
}

// Snapshot 是评测集的完整快照，可直接落盘为 JSON。
//
// 为什么要快照而不是每次都查库：基线数字必须可复现。库里的数据每天都在变，
// 半年后回头看"当时基线是 0.31"必须还能验证——否则那份报告就只是一段回忆。
type Snapshot struct {
	// GeneratedAt 是导出时间，仅用于记录，不参与计算。
	GeneratedAt time.Time `json:"generated_at"`
	// Source 描述数据来源（例如 DSN 的库名），便于判断快照的新旧。
	Source string `json:"source"`
	// SplitAt 是训练/观测的时间切分点：候选集取它之前发布的视频，
	// 标签取它之后发生的互动。
	//
	// 为零时由 Load 依据 SplitRatio 计算，因此手写的小样本快照可以省略它。
	SplitAt time.Time `json:"split_at"`
	// SplitRatio 是计算 SplitAt 用的分位比例（例如 0.5 表示按发布时间中位数切分）。
	SplitRatio float64 `json:"split_ratio"`

	// FeatureNote 说明排序特征是怎么来的。
	//
	// 存在的理由：无论真实导出还是合成生成，排序特征都必须只反映切分点之前的
	// 状态；而"popularity"这个特征在离线下无法精确还原（线上是 Redis 累计量），
	// 只能给一个代理值。把这件事写在数据里，读者才不会被一个看起来精确的数字误导。
	FeatureNote string `json:"feature_note,omitempty"`

	Videos       []Video       `json:"videos"`
	Interactions []Interaction `json:"interactions"`
}

// Load 从文件读取快照，并按需补全 SplitAt。
func Load(path string) (*Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取评测快照 %s: %w", path, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("解析评测快照 %s: %w", path, err)
	}
	if snap.SplitRatio <= 0 || snap.SplitRatio >= 1 {
		snap.SplitRatio = DefaultSplitRatio
	}
	if snap.SplitAt.IsZero() {
		at, err := ComputeSplit(snap.Videos, snap.SplitRatio)
		if err != nil {
			return nil, err
		}
		snap.SplitAt = at
	}
	// 特征一律由切分点之前的互动重算。
	//
	// 这不是"可选的一致性检查"，而是必须做的：快照文件是会被长期保存的，
	// 而手写/旧版本的快照里 likes_count/popularity 可能是导出时刻的值——
	// 那个值里已经包含了观测窗口内的互动，等于"用答案排序"（review 抓到过）。
	// 在这里统一重算，等于把"特征不含未来信息"这条性质变成文件加载的**必然结果**。
	snap.ApplyFeatures()
	return &snap, nil
}

// ApplyFeatures 用切分点之前的互动重算所有排序特征（原地修改）。
//
// 单独抽出来是为了让"从文件读"与"从数据库导"两条路径共用同一段逻辑：
// 两条路径各写一遍，就一定会有一条忘了过滤未来互动。
func (s *Snapshot) ApplyFeatures() {
	likes := make([]Interaction, 0, len(s.Interactions))
	comments := make([]Interaction, 0, len(s.Interactions))
	for _, it := range s.Interactions {
		switch it.Kind {
		case KindLike:
			likes = append(likes, it)
		case KindComment:
			comments = append(comments, it)
		}
	}
	videos, note := AsOf(s.Videos, likes, comments, s.SplitAt)
	s.Videos = videos
	s.FeatureNote = note
}

// Save 把快照落盘（缩进格式：快照要能被人读，也要能进 git 做 diff）。
func (s *Snapshot) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ComputeSplit 按发布时间的分位比例算出切分点。
//
// 用分位数而不是固定时刻：不同环境的绝对时间完全不同（本地开发库可能只有
// 最近两天的数据），按比例切分才能让"半个数据集当候选、半个当标签"这个
// 语义在任何环境里都成立。
func ComputeSplit(videos []Video, ratio float64) (time.Time, error) {
	if len(videos) == 0 {
		return time.Time{}, fmt.Errorf("评测集为空：video 表里没有任何数据")
	}
	times := make([]time.Time, 0, len(videos))
	for _, v := range videos {
		times = append(times, v.CreateTime)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	idx := int(float64(len(times)) * ratio)
	if idx >= len(times) {
		idx = len(times) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return times[idx], nil
}

// Relevance 由交互计算出的相关性等级：0 = 不相关，2 = 强相关（点赞），1 = 弱相关（评论）。
//
// 为什么点赞高于评论：两者成本不同（点赞近乎零成本、评论需要输入内容），
// 在历史数据里点赞量级也远大于评论。等权会让指标被点赞数量主导，
// 于是"评论了但没点赞"的视频被系统性低估。
const (
	RelNone    = 0
	RelComment = 1
	RelLike    = 2
)

// RelevanceOf 汇总观测窗口内的互动，得到每一条视频的相关性等级。
//
// 只看 SplitAt **之后**的互动：这是"不使用未来信息"这条原则的落地点。
// 同一视频既被点赞又被评论时取点赞（更强信号），而不是相加——
// 相加会让热门视频的分数无上界，NDCG 的理想排序随之被单条视频主导。
func RelevanceOf(snap *Snapshot) map[uint]int {
	rel := make(map[uint]int, len(snap.Videos))
	for _, it := range snap.Interactions {
		if !it.At.After(snap.SplitAt) {
			continue
		}
		grade, ok := gradeOf(it.Kind)
		if !ok {
			continue
		}
		if grade > rel[it.VideoID] {
			rel[it.VideoID] = grade
		}
	}
	return rel
}

// Candidates 返回切分点之前发布的视频（即排序器可以"看到"的候选集）。
func Candidates(snap *Snapshot) []Video {
	out := make([]Video, 0, len(snap.Videos))
	for _, v := range snap.Videos {
		if v.CreateTime.Before(snap.SplitAt) {
			out = append(out, v)
		}
	}
	return out
}

// Ranker 是一个排序策略。
//
// 三个内置排序器**逐一对应线上已有的 SQL**（internal/feed/repo.go），
// 优先级顺序必须一致，否则基线数字与线上行为无关：
//
//	timeline     → ListLatest:        ORDER BY create_time DESC
//	popularity   → ListByPopularity:  ORDER BY popularity DESC, create_time DESC, id DESC
//	likes_count  → ListLikesCount:    ORDER BY likes_count DESC, id DESC
type Ranker struct {
	Name string
	// Less 返回 a 是否应排在 b 前面。全局排序器用它。
	Less func(a, b Video) bool
	// Personalized 非 nil 时改为**按用户**排序，Less 被忽略。
	//
	// 这是 P2 语义召回的落点：语义排序依赖"这个用户的兴趣向量"，
	// 同一份候选集对不同用户必须能排出不同顺序。评测里给它留好位置，
	// 否则"同一候选集、两个用户结果不同"这条验收项根本无从表达。
	Personalized func(accountID uint, a, b Video) bool

	// report 是策略自带的参数说明（见 Reporter）。
	//
	// 放在 Ranker 里而不是让 Evaluate 再收一个参数列表：调用方构造策略时
	// 顺手把参数挂上，就不会出现"策略加了参数但忘了传"的情况。
	report Reporter
}

// WithReport 把参数说明挂到排序策略上，返回自身以支持链式构造。
func (r Ranker) WithReport(rep Reporter) Ranker {
	r.report = rep
	return r
}

// BuiltinRankers 返回"今天线上真实存在的排序"。
//
// P2 的精排只需要往这个列表里加一个，就能与其它策略在同一份数据、
// 同一套指标下比较——这正是评测集存在的意义。
func BuiltinRankers() []Ranker {
	return []Ranker{
		{
			Name: "timeline",
			Less: func(a, b Video) bool {
				if a.CreateTime.Equal(b.CreateTime) {
					return a.ID > b.ID
				}
				return a.CreateTime.After(b.CreateTime)
			},
		},
		{
			Name: "popularity",
			Less: func(a, b Video) bool {
				if a.Popularity != b.Popularity {
					return a.Popularity > b.Popularity
				}
				if !a.CreateTime.Equal(b.CreateTime) {
					return a.CreateTime.After(b.CreateTime)
				}
				return a.ID > b.ID
			},
		},
		{
			Name: "likes_count",
			Less: func(a, b Video) bool {
				if a.LikesCount != b.LikesCount {
					return a.LikesCount > b.LikesCount
				}
				return a.ID > b.ID
			},
		},
	}
}

// Rank 按给定策略排序，返回排好序的副本（不改动入参，便于多次评测同一份候选集）。
//
// 只支持全局排序器。带 Personalized 的策略请用 RankFor——
// 这里对它们会退化成"Less 为 nil"，那是 panic 而不是可降级的错误，
// 所以在入口处直接拒绝。
func Rank(candidates []Video, r Ranker) []Video {
	if r.Personalized != nil {
		panic("evalset: 个性化排序器必须用 RankFor（Rank 无法表达用户维度）")
	}
	return rankWith(candidates, r.Less)
}

// RankFor 按给定策略为目标用户排序，返回排好序的副本。
//
// 全局策略与个性化策略共用同一个入口：调用方（按用户评测）不需要知道
// 哪个策略是哪种，否则每加一个策略都要去改评测主循环。
func RankFor(accountID uint, candidates []Video, r Ranker) []Video {
	if r.Personalized == nil {
		return rankWith(candidates, r.Less)
	}
	less := func(a, b Video) bool { return r.Personalized(accountID, a, b) }
	return rankWith(candidates, less)
}

func rankWith(candidates []Video, less func(a, b Video) bool) []Video {
	out := make([]Video, len(candidates))
	copy(out, candidates)
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// RecallAtK 计算 Recall@K：观测到的相关视频里，有多少落进了前 K。
//
// 分母只算**候选集里**的相关视频：如果一条视频在切分点之后才发布，
// 排序器根本不可能推荐它，把它算进分母会低估所有策略，且低估程度取决于
// 数据分布——那样的数字没法用来比较不同策略。
func RecallAtK(ranked []Video, rel map[uint]int, k int) float64 {
	total := 0
	for _, v := range ranked {
		if rel[v.ID] > RelNone {
			total++
		}
	}
	if total == 0 {
		// 没有任何相关样本时不返回 0：那会让"数据不足"看起来像"效果为零"。
		// 调用方通过 EvalResult.Reliable 判断是否可比较。
		return 0
	}
	hits := 0
	for i, v := range ranked {
		if i >= k {
			break
		}
		if rel[v.ID] > RelNone {
			hits++
		}
	}
	return float64(hits) / float64(total)
}

// NDCGAtK 计算 NDCG@K，增益用 2^rel - 1、折扣用 log2(i+2)。
//
// 为什么用指数增益：推荐场景里"强相关排在第一位"与"弱相关排在第一位"
// 的价值差距远大于线性假设，指数增益对这个差距更敏感，也更主流
// （与 sklearn 的 ndcg_score 默认口径一致，便于外部复核）。
func NDCGAtK(ranked []Video, rel map[uint]int, k int) float64 {
	dcg := 0.0
	for i, v := range ranked {
		if i >= k {
			break
		}
		dcg += gain(rel[v.ID]) / discount(i)
	}

	// 理想排序：把所有相关视频按相关性降序排在前面。
	grades := make([]int, 0, len(ranked))
	for _, v := range ranked {
		if g := rel[v.ID]; g > RelNone {
			grades = append(grades, g)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))

	idcg := 0.0
	for i, g := range grades {
		if i >= k {
			break
		}
		idcg += gain(g) / discount(i)
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// gain 是相关性的增益：2^rel - 1。
//
// 用 math.Pow 而不是位运算 (1<<rel)-1：位运算在 rel 较大时会整数溢出，
// 而相关性等级是会被扩展的（P1 之后可能有 "关注"=3 之类），
// 一个只在特定等级下才出现的溢出极难排查。
func gain(rel int) float64 {
	return math.Pow(2, float64(rel)) - 1
}

// discount 是位置折扣：log2(i+2)，第 1 位（i=0）折扣为 1。
func discount(i int) float64 {
	return math.Log2(float64(i) + 2)
}

// EvalResult.Source 里出现这个前缀时说明数据来自 cmd/evalseed 的合成生成器。
//
// 为什么要让评测代码认识"合成"这件事：合成数据的标签是按规则造的，
// 三个排序策略的分数会异常漂亮（NDCG 到 1.0）。报告一旦脱离上下文流传，
// 最容易被误引用的就是那张表——必须在生成端就带上明确的警示。
const syntheticSourcePrefix = "synthetic"

// IsSynthetic 报告一份结果的来源是否是合成数据。
func IsSynthetic(source string) bool {
	return len(source) >= len(syntheticSourcePrefix) && source[:len(syntheticSourcePrefix)] == syntheticSourcePrefix
}

// KResult 是某个 K 下的指标。
type KResult struct {
	K      int     `json:"k"`
	Recall float64 `json:"recall"`
	NDCG   float64 `json:"ndcg"`
}

// RankerResult 是一个排序策略在全部 K 上的结果。
type RankerResult struct {
	Name    string    `json:"name"`
	Results []KResult `json:"results"`
}

// Reporter 让一个排序策略把自己的**参数**带进结果文件。
//
// 为什么必须由策略自己报（而不是在 CLI 里写死）：
// P2 §4.7 要求结果"含参数、时间、模型版本"，而参数是策略的属性
// （配额、候选窗口、embedding 模型……）。写在 CLI 里意味着以后加一个
// 参数就要记得改 CLI，而"忘改"的后果是一份**看起来完整、实际缺参数**
// 的结果文件——那种文件比没有文件更危险，因为它会被当成可复核的依据。
type Reporter interface {
	// Report 返回要写进结果文件的键值对（必须是 JSON 可序列化的）。
	Report() map[string]any
}

// EvalResult 是一次完整评测的产出。
type EvalResult struct {
	// Source 与快照的来源一致，用于在报告里标注数据出处（尤其是合成数据）。
	Source string `json:"source"`
	// Producer 是"哪条命令生成了这份结果"（例如 cmd/evalbaseline / cmd/evalp2）。
	//
	// 为什么要有它：报告的最上面写着"由 XX 生成，请勿手工编辑"，
	// 而 P2 起有两个命令会产出报告（基线、P2 对比）。写死一个名字的话，
	// P2 的报告会声称自己是基线的产物——读者照着那句话去重跑，
	// 得到的会是另一份东西。
	Producer string    `json:"producer,omitempty"`
	SplitAt  time.Time `json:"split_at"`
	// Candidates 是候选集大小（排序器可选的视频数）。
	Candidates int `json:"candidates"`
	// Relevant 是候选集里有观测互动的视频数，也是 Recall 的分母。
	Relevant int `json:"relevant"`
	// HeldOutInteractions 是被用作标签的互动条数。
	HeldOutInteractions int `json:"held_out_interactions"`
	// Reliable 表示样本量是否足以支撑结论。
	//
	// 为什么要有这个字段：数据量小的时候指标仍然会算出一个数字，
	// 而那个数字看起来完全正常（比如 Recall@10 = 0.0）。如果不显式标注
	// "不可靠"，报告会被当成结论使用——这比没有报告更危险。
	Reliable bool           `json:"reliable"`
	Warnings []string       `json:"warnings,omitempty"`
	Rankers  []RankerResult `json:"rankers"`

	// RankerParams 是各排序策略自己报上来的参数（P2 §4.7 的"含参数"）。
	//
	// 键是排序策略名。策略没实现 Reporter 时不出现在这里——
	// 这与"报告里少一张表"是同一种问题，因此渲染层会显式说明。
	RankerParams map[string]map[string]any `json:"ranker_params,omitempty"`

	// PerUser 是**按用户分组**的评测结果（P2 前置项 B1）。
	//
	// 它和上面的全局字段是两套口径，刻意不混在一起（前置条件 §1 红线）：
	//   - 本结构体顶层的数字衡量"全局热度预测得准不准"，只作为历史参考；
	//   - PerUser 衡量"给每个用户推对了多少"，**P2 的结论只能用这一套**。
	//
	// 快照里没有 account_id 时它为 nil（旧快照的正常状态），
	// 渲染层会显式说明"这份数据不能评估 P2"，而不是安静地少打一张表。
	PerUser *UserEvalResult `json:"per_user,omitempty"`
}

// 判定"样本量是否够用"的阈值。
//
// 这些数字不是统计学推导，而是"低于它就别下结论"的经验底线：
// 相关样本少于 20 条时，Recall@10 的取值只能落在少数几个离散点上，
// 不同策略之间的差异基本是噪声。
const (
	minCandidates = 50
	minRelevant   = 20
)

// SetProducer 记录"这份结果由哪个命令生成"（写进报告抬头）。
//
// 单独一个 setter 而不是给 Evaluate 加参数：它只影响渲染文案，
// 与指标计算无关；加参数会让每一个既有调用点都被迫改一遍
// （而它们绝大多数是评测工具内部调用）。
func (r *EvalResult) SetProducer(name string) { r.Producer = name }

// Evaluate 在快照上跑完所有排序器，返回可落盘的结果。
func Evaluate(snap *Snapshot, rankers []Ranker, ks []int) (*EvalResult, error) {
	if len(rankers) == 0 {
		return nil, fmt.Errorf("没有可评测的排序策略")
	}
	if len(ks) == 0 {
		return nil, fmt.Errorf("没有指定 K")
	}
	for _, k := range ks {
		if k <= 0 {
			return nil, fmt.Errorf("K 必须为正整数，实际 %d", k)
		}
	}

	candidates := Candidates(snap)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("候选集为空：切分点 %s 之前没有任何视频", snap.SplitAt.Format(time.RFC3339))
	}
	rel := RelevanceOf(snap)

	res := &EvalResult{
		Source:     snap.Source,
		SplitAt:    snap.SplitAt,
		Candidates: len(candidates),
	}

	heldOut := 0
	candidateIDs := make(map[uint]bool, len(candidates))
	for _, v := range candidates {
		candidateIDs[v.ID] = true
	}
	for _, it := range snap.Interactions {
		if it.At.After(snap.SplitAt) {
			heldOut++
		}
	}
	res.HeldOutInteractions = heldOut

	for _, v := range candidates {
		if rel[v.ID] > RelNone {
			res.Relevant++
		}
	}

	if len(candidates) < minCandidates {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"候选集只有 %d 条（< %d）：指标波动大，不能作为结论", len(candidates), minCandidates))
	}
	if res.Relevant < minRelevant {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"候选集里的相关样本只有 %d 条（< %d）：Recall/NDCG 基本是噪声，请先积累数据",
			res.Relevant, minRelevant))
	}
	if heldOut == 0 {
		res.Warnings = append(res.Warnings, "观测窗口内没有任何互动：所有相关性都是 0，指标无意义")
	}
	// 饱和检测：候选集里几乎每条视频都被"某个人"互动过时，全局口径已经失去区分力。
	//
	// 为什么必须显式说出来：这种数据上任何排序的 NDCG 都接近 1
	// （因为理想排序与任意排序的差距被摊平），报告里会出现一张
	// "所有策略都是 1.000"的表——它看起来像"排序已经完美"，
	// 实际含义是"这个口径问不出问题"。这正是本项目引入按用户口径的原因，
	// 所以这里宁可把全局段判为不可靠，也不留下一张会被误读的表。
	if len(candidates) >= minCandidates && res.Relevant*10 >= len(candidates)*9 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"候选集里 %d/%d 条视频都被互动过：全局口径已饱和，任何排序都会得到接近满分的 NDCG，本表在此数据上没有区分力（请只看按用户分组口径）",
			res.Relevant, len(candidates)))
	}
	res.Reliable = len(res.Warnings) == 0

	// 全局口径只对"不依赖用户"的策略有定义。
	//
	// 个性化策略（P2 的语义精排）在同一份候选集上对不同用户顺序不同，
	// 而全局口径的 rel 是所有用户互动的并集——把它算进来只会得到一个
	// 无法解释的数字。这类策略只出现在 PerUser 里，这里显式跳过。
	globalRankers := globalOnly(rankers)
	if len(globalRankers) == 0 {
		res.Warnings = append(res.Warnings, "没有可在全局口径下评测的策略（全部依赖用户维度），全局表为空")
	}
	for _, r := range globalRankers {
		ranked := Rank(candidates, r)
		rr := RankerResult{Name: r.Name}
		for _, k := range ks {
			rr.Results = append(rr.Results, KResult{
				K:      k,
				Recall: RecallAtK(ranked, rel, k),
				NDCG:   NDCGAtK(ranked, rel, k),
			})
		}
		res.Rankers = append(res.Rankers, rr)
	}

	// 按用户口径一起算出来：CLI 与报告都要并排展示两套数字，
	// 让调用方自己记得再调一次，迟早会漏（而漏掉的那次就只剩全局口径）。
	perUser, err := EvaluatePerUser(snap, rankers, ks)
	if err != nil {
		return nil, err
	}
	res.PerUser = perUser

	// 收集各策略自报的参数（见 Reporter 的注释）。
	//
	// 刻意放在按用户评测**之后**：参数里可能含"评测过程中累积"的观测值
	// （例如理由覆盖率——它只有跑过每个用户才存在）。在评测前收集会得到
	// 一个看起来合理、实际是 0 的数字，而那比缺参数更危险。
	for _, r := range rankers {
		rep, ok := rankerReporter(r)
		if !ok {
			continue
		}
		if res.RankerParams == nil {
			res.RankerParams = map[string]map[string]any{}
		}
		res.RankerParams[r.Name] = rep.Report()
	}
	return res, nil
}

// rankerReporter 尝试把排序策略取成 Reporter。
//
// Ranker 是按值传递的（它只有函数字段），因此参数只能经由函数字段背后的
// 闭包对象拿到：这里用一个"附带报告能力的 Less 包装"约定——
// 构造方把 Reporter 挂到 Ranker 的 report 字段上（见 Ranker.WithReport）。
func rankerReporter(r Ranker) (Reporter, bool) {
	if r.report == nil {
		return nil, false
	}
	return r.report, true
}

// globalOnly 过滤掉无法在全局口径下排序的策略（见 Evaluate 里的说明）。
func globalOnly(rankers []Ranker) []Ranker {
	out := make([]Ranker, 0, len(rankers))
	for _, r := range rankers {
		if r.Personalized != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}
