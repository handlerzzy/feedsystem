package evalset

import (
	"fmt"
	"sort"
)

// 本文件实现"**按用户分组**"的评测口径（P2 前置项 B1）。
//
// 为什么必须有这一套，而不是继续用 evalset.go 里的全局口径：
// 全局口径衡量的是"切分点之后哪些视频被互动过"，本质是**全局热度预测**。
// 它没有用户维度，于是"给某个用户推对他有用的内容"这件事根本没有被度量——
// 用它评估 P2 的语义召回，唯一的结论只能是"我接了模型"。
// 用户维度必须在写召回**之前**建好，否则后面所有调参都是在迎合一个错的口径
// （见 docs/AI-P2-前置条件.md 第 1 节与第 10 节）。

// Dist 是一个整数样本的分布摘要。
//
// 为什么报分布而不是只报均值：均值会把"50 个用户里 3 个有 200 条相关样本、
// 其余几乎为 0"与"每个用户都有 20 条"压成同一个数字，而这两种数据的
// 可信度完全不同。P2 前置条件明确要求输出"每用户相关样本的分布"。
type Dist struct {
	Min  int     `json:"min"`
	P50  int     `json:"p50"`
	P90  int     `json:"p90"`
	Max  int     `json:"max"`
	Mean float64 `json:"mean"`
}

// SummarizeDist 汇总一组整数样本。空输入返回零值（而不是 NaN）。
func SummarizeDist(values []int) Dist {
	if len(values) == 0 {
		return Dist{}
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)

	var sum int
	for _, v := range sorted {
		sum += v
	}
	return Dist{
		Min: sorted[0],
		// 用最近秩（nearest-rank）而不是插值：这些量是"条数"，
		// 报一个 20.5 条的分位数没有任何意义。
		P50:  nearestRank(sorted, 0.5),
		P90:  nearestRank(sorted, 0.9),
		Max:  sorted[len(sorted)-1],
		Mean: float64(sum) / float64(len(sorted)),
	}
}

// nearestRank 取分位数，入参必须已排序。
func nearestRank(sorted []int, q float64) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * q)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// UserEvalResult 是"每用户算指标、再对用户取平均"的结果。
type UserEvalResult struct {
	// Users 是快照里**出现过互动的用户总数**（含只在训练期出现过的用户）。
	Users int `json:"users"`
	// EvaluatedUsers 是真正进入平均的用户数。
	//
	// 只有"至少有一条相关候选视频"的用户才有 Recall 的分母——
	// 参与度为零的用户不是"效果差"，而是**无法度量**，把它们算成 0
	// 会让指标随"有多少沉默用户"漂移。
	EvaluatedUsers int `json:"evaluated_users"`
	// SkippedNoRelevant 是因为没有相关候选样本而被剔除的用户数。
	SkippedNoRelevant int `json:"skipped_no_relevant"`

	// CandidatesPerUser / RelevantPerUser 是每个用户的候选集大小与相关样本数分布。
	CandidatesPerUser Dist `json:"candidates_per_user"`
	RelevantPerUser   Dist `json:"relevant_per_user"`
	// UsersBelowMinRelevant 是相关样本数低于阈值的用户数。
	// 它是"这个平均被少数几个用户主导"的直接证据。
	UsersBelowMinRelevant int `json:"users_below_min_relevant"`

	// Reliable 表示样本量是否足以支撑"P2 让排序变好了"这类结论。
	Reliable bool `json:"reliable"`
	// Warnings 影响 Reliable；Notes 只是提示，不影响判定。
	//
	// 分开的理由：把"信息"塞进 Warnings 会让任何一句提示都把整份报告
	// 判成不可信，久而久之所有人都会忽略那个红色横幅——那比不标更糟。
	Warnings []string `json:"warnings,omitempty"`
	Notes    []string `json:"notes,omitempty"`

	// Rankers 里每个策略的指标是**对用户取平均**之后的值。
	Rankers []RankerResult `json:"rankers"`
}

// P2 前置条件里写明的规模底线，也是 Reliable 的判定依据。
//
// 为什么是这两个数：P2 的结论形式是"新排序比旧排序好多少"。
// 用户数决定这个结论能不能跨人群成立（用户太少时，一个重度用户就能
// 左右全部指标）；每用户相关样本数决定单个用户的 Recall@K 有多少个
// 可能取值（相关样本 < 20 时 Recall@10 只能落在少数离散点上）。
const (
	minUsers           = 50
	minRelevantPerUser = 20
)

// CandidatesFor 返回某个用户**可见的、切分点之前发布的**候选视频。
//
// "可见"目前表达为两条，且只表达两条：
//  1. 发布时间早于切分点（不使用未来信息）；
//  2. 不是该用户自己发布的（推荐流不会把自己发的内容推给自己）。
//
// 关注/拉黑等关系维度**故意没有**在这里猜：快照里没有这些数据，
// 用默认值编一个出来会让"可见性"变成一个看起来精确、实际无据可依的规则。
// P2 接入真实召回时再按真实数据补，接口形状不变。
func CandidatesFor(snap *Snapshot, accountID uint) []Video {
	out := make([]Video, 0, len(snap.Videos))
	for _, v := range snap.Videos {
		if !v.CreateTime.Before(snap.SplitAt) {
			continue
		}
		if accountID != 0 && v.AuthorID == accountID {
			continue
		}
		out = append(out, v)
	}
	return out
}

// RelevanceOfUser 与 RelevanceOf 同规则，但只统计**该用户自己**的互动。
//
// 单独一个函数而不是给 RelevanceOf 加参数：全局口径仍在用（作为历史参考），
// 两者的调用点语义不同，合并成一个带可选参数的函数只会让两边都难读。
func RelevanceOfUser(snap *Snapshot, accountID uint) map[uint]int {
	rel := make(map[uint]int)
	for _, it := range snap.Interactions {
		if it.AccountID != accountID {
			continue
		}
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

// gradeOf 是互动类型 -> 相关性等级的唯一映射。
//
// 抽成函数是为了让全局口径与按用户口径**共用同一条规则**：
// 两处各写一份评分标准，就一定会有一处先漂移，而漂移之后
// 新旧口径的数字再也没有可比性。
func gradeOf(kind string) (int, bool) {
	switch kind {
	case KindLike:
		return RelLike, true
	case KindComment:
		return RelComment, true
	default:
		// 未知互动类型：忽略而不是猜。猜错会污染标签，
		// 而标签错了所有指标都会错，却看不出错在哪里。
		return RelNone, false
	}
}

// UserIDs 返回快照里出现过互动的用户，按升序排列。
//
// 排序是为了让评测**可复现**：map 遍历顺序随机，不排序的话同一份快照
// 两次运行会得到逐字节不同的 JSON（浮点求和的结合顺序不同），
// 而"基线可复现"正是这套工具存在的理由。
// AccountID == 0 视为"未知用户"（旧快照、或导出时漏了该列），不参与分组。
func UserIDs(snap *Snapshot) []uint {
	seen := make(map[uint]bool)
	for _, it := range snap.Interactions {
		if it.AccountID != 0 {
			seen[it.AccountID] = true
		}
	}
	out := make([]uint, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// EvaluatePerUser 对每个用户单独算 Recall@K / NDCG@K，再对用户取平均。
//
// 返回值在"快照里没有任何带 account_id 的互动"时为 (nil, nil)：
// 这是**旧快照**的正常状态（P0 导出的快照没有用户维度），
// 不该让整个评测失败——但调用方必须把它当成"这份数据不能评估 P2"，
// 渲染层会显式打印这一点。
func EvaluatePerUser(snap *Snapshot, rankers []Ranker, ks []int) (*UserEvalResult, error) {
	if len(rankers) == 0 {
		return nil, fmt.Errorf("没有可评测的排序策略")
	}
	if len(ks) == 0 {
		return nil, fmt.Errorf("没有指定 K")
	}
	users := UserIDs(snap)
	if len(users) == 0 {
		return nil, nil
	}

	res := &UserEvalResult{Users: len(users)}

	// perUser[rankerIdx][kIdx] 累加各用户的指标；分母是"真正参与平均的用户数"。
	sums := make([][]KResult, len(rankers))
	for i := range sums {
		sums[i] = make([]KResult, len(ks))
		for j, k := range ks {
			sums[i][j].K = k
		}
	}

	candSizes := make([]int, 0, len(users))
	relSizes := make([]int, 0, len(users))

	for _, u := range users {
		candidates := CandidatesFor(snap, u)
		rel := RelevanceOfUser(snap, u)

		relevant := 0
		for _, v := range candidates {
			if rel[v.ID] > RelNone {
				relevant++
			}
		}
		if relevant == 0 {
			// 没有分母：这个用户对任何策略都是"无法度量"，
			// 而不是"所有策略都得 0 分"。剔除以避免指标随沉默用户比例漂移。
			res.SkippedNoRelevant++
			continue
		}

		res.EvaluatedUsers++
		candSizes = append(candSizes, len(candidates))
		relSizes = append(relSizes, relevant)
		if relevant < minRelevantPerUser {
			res.UsersBelowMinRelevant++
		}

		for i, r := range rankers {
			ranked := RankFor(u, candidates, r)
			for j, k := range ks {
				sums[i][j].Recall += RecallAtK(ranked, rel, k)
				sums[i][j].NDCG += NDCGAtK(ranked, rel, k)
			}
		}
	}

	res.CandidatesPerUser = SummarizeDist(candSizes)
	res.RelevantPerUser = SummarizeDist(relSizes)

	if res.EvaluatedUsers == 0 {
		res.Warnings = append(res.Warnings,
			"没有任何用户拥有相关候选样本：无法计算按用户分组的指标")
		res.Reliable = false
		return res, nil
	}

	for i, r := range rankers {
		rr := RankerResult{Name: r.Name}
		for j := range ks {
			rr.Results = append(rr.Results, KResult{
				K:      ks[j],
				Recall: sums[i][j].Recall / float64(res.EvaluatedUsers),
				NDCG:   sums[i][j].NDCG / float64(res.EvaluatedUsers),
			})
		}
		res.Rankers = append(res.Rankers, rr)
	}

	if res.EvaluatedUsers < minUsers {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"参与评测的用户只有 %d 个（< %d）：结论无法跨人群成立，单个重度用户就能左右指标",
			res.EvaluatedUsers, minUsers))
	}
	if res.RelevantPerUser.P50 < minRelevantPerUser {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"每用户相关样本数的中位数只有 %d 条（< %d）：Recall@K 的取值只能落在少数离散点上，差异基本是噪声",
			res.RelevantPerUser.P50, minRelevantPerUser))
	}
	if res.UsersBelowMinRelevant > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d/%d 个用户的相关样本数低于 %d 条，平均值由其余用户主导",
			res.UsersBelowMinRelevant, res.EvaluatedUsers, minRelevantPerUser))
	}
	if res.SkippedNoRelevant > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d 个用户在观测窗口内没有任何相关候选样本，已从平均中剔除（它们不是得 0 分，而是无法度量）",
			res.SkippedNoRelevant))
	}
	res.Reliable = len(res.Warnings) == 0

	return res, nil
}
