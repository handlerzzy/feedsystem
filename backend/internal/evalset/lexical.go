package evalset

import (
	"math"
	"unicode"
)

// LexicalProbeRanker 返回一个**纯词法**的结构探针排序器。
//
// 它存在的理由，以及它**不是**什么：
//
// P2 前置条件里最核心的一条验收是"在生成的评测数据上，popularity 的 NDCG
// 明显小于 1"。但那只能证明"热度排序不够好"，不能证明"数据里有可被
// 语义利用的结构"——一个纯粹打乱顺序的排序器同样会让 NDCG 小于 1。
//
// 这个探针补上的正是那一环：它只看标题与描述的字面重叠（字符 bigram +
// idf + 余弦），不含任何模型、不联网、不读 API key。如果它能明显跑赢
// popularity，就说明**文本里确实编码了用户偏好**，P2 的 embedding 召回
// 才有可能有效；如果它跑不赢，那 P2 做出来也不会有差别，
// 该修的是数据而不是模型。
//
// 纪律：它是**诊断工具**，不是线上排序器。
//   - 不进 BuiltinRankers()：那几个必须与线上 SQL 逐字对应，
//     混进一个"只存在于离线评测里的策略"会让基线表的含义立刻变模糊。
//   - 不需要任何外部依赖：因此 CI 与离线环境都能跑，"有假模型路径"
//     这条验收项由它兜住。
func LexicalProbeRanker(snap *Snapshot) Ranker {
	docs := newLexicalDocs(snap)

	// 用户画像按需构建并缓存。
	//
	// 为什么用 map 缓存是安全的：这里只按 key 查，不遍历 map，
	// 因此不会把 map 的随机顺序带进排序结果（那是可复现性的常见杀手）。
	profiles := make(map[uint]map[string]float64)
	scores := make(map[uint]map[uint]float64)

	return Ranker{
		Name: "lexical_probe",
		Personalized: func(accountID uint, a, b Video) bool {
			sa := scores[accountID]
			if sa == nil {
				profile := profiles[accountID]
				if profile == nil {
					profile = docs.profileFor(snap, accountID)
					profiles[accountID] = profile
				}
				sa = docs.scoresFor(profile)
				scores[accountID] = sa
			}
			va, vb := sa[a.ID], sa[b.ID]
			if va != vb {
				return va > vb
			}
			// 并列时按 id 降序：与线上 popularity/likes_count 的
			// 次级排序保持一致，也让结果与遍历顺序无关。
			return a.ID > b.ID
		},
	}
}

// lexicalDocs 是整个快照的文档向量集合。
type lexicalDocs struct {
	// vecs 是 videoID -> L2 归一化后的 tf-idf 稀疏向量。
	vecs map[uint]map[string]float64
}

// newLexicalDocs 计算所有视频的文档向量。
//
// 只用候选集（切分点之前的视频）算 idf 会更好，但那需要知道切分点；
// 而这里把全量视频纳入 idf 统计**不会引入未来信息**——
// idf 是词频统计，不含任何互动标签。把它写清楚，是因为
// "特征是否泄漏未来"是这套代码里最容易被误判的地方。
func newLexicalDocs(snap *Snapshot) *lexicalDocs {
	rawTokens := make(map[uint][]string, len(snap.Videos))
	df := make(map[string]int)
	for _, v := range snap.Videos {
		tokens := lexicalTokens(v.Title + " " + v.Description)
		rawTokens[v.ID] = tokens
		seen := make(map[string]bool, len(tokens))
		for _, t := range tokens {
			if seen[t] {
				continue
			}
			seen[t] = true
			df[t]++
		}
	}

	n := float64(len(snap.Videos))
	if n == 0 {
		n = 1
	}
	vecs := make(map[uint]map[string]float64, len(rawTokens))
	for id, tokens := range rawTokens {
		tf := make(map[string]float64, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		vec := make(map[string]float64, len(tf))
		var norm float64
		for t, c := range tf {
			// sklearn 风格的平滑 idf：常数项保证"文档里出现过"的词权重恒为正，
			// 否则一个在所有文档里都出现的词会得到 0 权重，画像会变空。
			idf := math.Log((1+n)/(1+float64(df[t]))) + 1
			w := c * idf
			vec[t] = w
			norm += w * w
		}
		if norm > 0 {
			inv := 1 / math.Sqrt(norm)
			for t := range vec {
				vec[t] *= inv
			}
		}
		vecs[id] = vec
	}
	return &lexicalDocs{vecs: vecs}
}

// profileFor 用用户在**切分点之前**的互动构建兴趣画像。
//
// 只用切分点之前的互动是硬要求：用观测窗口内的互动做画像等于
// 把答案喂给排序器，"探针跑赢热度"就变成了自证。
func (d *lexicalDocs) profileFor(snap *Snapshot, accountID uint) map[string]float64 {
	profile := make(map[string]float64)
	for _, it := range snap.Interactions {
		if it.AccountID != accountID {
			continue
		}
		if it.At.After(snap.SplitAt) {
			continue
		}
		for t, w := range d.vecs[it.VideoID] {
			profile[t] += w
		}
	}
	var norm float64
	for _, w := range profile {
		norm += w * w
	}
	if norm > 0 {
		inv := 1 / math.Sqrt(norm)
		for t := range profile {
			profile[t] *= inv
		}
	}
	return profile
}

// scoresFor 计算画像与每个视频的余弦相似度。
func (d *lexicalDocs) scoresFor(profile map[string]float64) map[uint]float64 {
	out := make(map[uint]float64, len(d.vecs))
	if len(profile) == 0 {
		// 没有历史行为（冷启动用户）：所有分数都是 0，
		// 排序退化为"按 id 降序"。这不是缺陷，而是这个探针
		// 本来就没有可用的信号——真实系统在这里必须走兜底通道。
		return out
	}
	for id, vec := range d.vecs {
		var dot float64
		// 遍历较短的一侧：画像通常远小于文档向量。
		if len(profile) < len(vec) {
			for t, w := range profile {
				dot += w * vec[t]
			}
		} else {
			for t, w := range vec {
				dot += w * profile[t]
			}
		}
		if dot > 0 {
			out[id] = dot
		}
	}
	return out
}

// lexicalTokens 把一段文本切成词法单元。
//
// 中文没有空格，因此汉字按**单字 + 相邻 bigram** 处理：
// 单字召回高但区分度低，bigram 承担主要区分度，两者都留着让 idf 自己去定权。
// 英文/数字按连续串切分并小写，这样 "goroutine" 与 "Goroutine" 是同一个词。
func lexicalTokens(text string) []string {
	units := make([]string, 0, 32)
	var word []rune
	flush := func() {
		if len(word) > 0 {
			units = append(units, string(word))
			word = word[:0]
		}
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			units = append(units, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word = append(word, unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()

	tokens := make([]string, 0, len(units)*2)
	for i, u := range units {
		tokens = append(tokens, u)
		if i > 0 {
			tokens = append(tokens, units[i-1]+u)
		}
	}
	// 刻意不排序：向量是 map，顺序与结果无关，
	// 而每次排序会让 500 条文档多出一次无意义的 log(n) 开销。
	return tokens
}
