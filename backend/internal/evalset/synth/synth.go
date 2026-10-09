// Package synth 生成**带话题结构**的合成评测集。
//
// 为什么需要它（P2 前置项 B2）：真实开发库几乎是空的，而且永远不会自然积累
// ——`./e2e_regression.sh` 每次跑完都会自清理。用真实数据只能得到
// "候选集 2 条、相关样本 2 条"，任何排序策略的数字都是噪声。
//
// 更要紧的是**旧版合成器是退化的**：视频零文本、互动量由 popularity 代理驱动，
// 于是"按热度排序"与"按真实相关性排序"是同一件事，popularity 的 NDCG 恒等于 1。
// 用那种数据评估语义召回，结论必然是"没差别"——不是模型没用，是数据里
// 根本没有可被语义利用的结构。
//
// 本包生成的每一份数据都满足三条性质，缺一条就退化：
//
//  1. **视频有真实文本**：标题与描述由话题词表组成，话题之间词面几乎不重叠，
//     因此向量/词法模型能从中恢复出"这条视频讲什么"。
//  2. **用户有话题偏好分布**：每个用户对少数几个话题有明显偏好，
//     其余话题只有很低的背景权重。
//  3. **互动按用户偏好采样**，而不是按全局热度：热度是偏好的**结果**，
//     不是生成依据。这正是"全局热度排序无法满足单个用户"的来源。
//
// 由此得到可验证的结论：`popularity` 排序的 NDCG 明显小于 1，
// 而只看文本词法的探针排序器明显更好——数据里有真实结构。
package synth

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/handlerzzy/feedsystem/internal/evalset"
)

// Options 是生成参数。零值不可用，请用 DefaultOptions() 再改。
type Options struct {
	// Seed 是随机种子。固定种子必须产出逐字节一致的数据。
	Seed int64
	// Videos / Users / Topics 是三个规模参数。
	Videos int
	Users  int
	Topics int
	// Days 是发布时间跨度（天）。
	Days int
	// Authors 是作者池大小（作者只发某个话题的内容，模拟垂类创作者）。
	Authors int
	// TrainPerUser 是每个用户在观测窗口之前的互动条数（决定排序特征）。
	TrainPerUser int
	// LabelPerUser 是每个用户在观测窗口之内的互动条数（决定相关性标签）。
	//
	// 它直接决定"每用户相关样本数"。P2 前置条件要求 ≥ 20，
	// 所以默认值必须高于它——这是**不能调低**的，调低等于把验收项做假。
	LabelPerUser int
	// LabelWindowDays 是观测窗口长度（天）。
	LabelWindowDays int
	// SplitRatio 是时间切分比例（按发布时间分位）。
	SplitRatio float64
	// Start 是数据集的起始时间。
	Start time.Time
	// GeneratedAt 写进快照。要保持逐字节可复现就必须固定它（见 cmd/evalseed）。
	GeneratedAt time.Time
}

// DefaultOptions 返回 P2 前置条件里那组规模参数。
//
// 规模取自 docs/AI-P2-前置条件.md §2：视频 ≥ 500、用户 ≥ 50、每用户相关样本 ≥ 20。
// 这里刻意取到刚好达标而不是"越大越好"：快照要进 git，
// 一个几 MB 的 JSON 会让每次 review 都在翻数据而不是看逻辑。
func DefaultOptions() Options {
	return Options{
		Seed:            20261008,
		Videos:          500,
		Users:           60,
		Topics:          20,
		Days:            60,
		Authors:         40,
		TrainPerUser:    30,
		LabelPerUser:    24,
		LabelWindowDays: 14,
		SplitRatio:      0.5,
		Start:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		GeneratedAt:     time.Now().UTC(),
	}
}

// Stats 是生成结果的规模摘要，供命令行打印与测试断言。
type Stats struct {
	Videos       int
	Candidates   int
	Users        int
	Topics       int
	Interactions int
	// LabelsPerUser 是每个用户实际得到的相关候选视频数（升序）。
	LabelsPerUser []int
}

// Generate 按参数生成一份评测快照。
//
// 它是**纯函数式**的：同一组参数（含 GeneratedAt）必定产出逐字节一致的快照。
// 这条性质由 synth_test.go 直接断言——没有它，"半年后重跑核对基线"
// 这件事就不成立，而基线数字的全部价值就在于可复现。
func Generate(opts Options) (*evalset.Snapshot, *Stats, error) {
	if err := opts.validate(); err != nil {
		return nil, nil, err
	}
	rnd := rand.New(rand.NewSource(opts.Seed))

	topics := topicTable[:opts.Topics]

	// 话题的"全局吸引力"：决定这个题材总体上被多少人看。
	// 它是真实存在的现象（有些题材天然受众广），也是**热度排序唯一的优势来源**。
	// 用 [0.4, 2.0] 而不是均匀分布：差距太小的话热度排序会与随机排序无异，
	// 那同样是一种退化（评测看不出任何策略的差别）。
	appeal := make([]float64, len(topics))
	for i := range appeal {
		appeal[i] = 0.4 + rnd.Float64()*1.6
	}

	// ---- 作者：每个作者只发一个话题的内容 ----
	authorsPerTopic := max(1, opts.Authors/len(topics))
	authors := make([][]uint, len(topics))
	for i := range authors {
		for j := 0; j < authorsPerTopic; j++ {
			authors[i] = append(authors[i], uint(i*authorsPerTopic+j+1))
		}
	}
	nextAuthor := uint(len(topics)*authorsPerTopic + 1)

	// ---- 视频：话题轮流分配，保证每个话题在时间轴上均匀铺开 ----
	//
	// 逐轮打乱话题顺序再分配，而不是"前 25 条全是话题 0"：
	// 若某个话题集中在时间轴某一段，它就会整体落进候选集或整体落进观测窗口，
	// 于是"该话题的相关视频"在多用户之间分布极不均匀，指标随切分点剧烈跳动。
	videos := make([]evalset.Video, 0, opts.Videos)
	topicOf := make([]int, 0, opts.Videos)
	quality := make([]float64, 0, opts.Videos)
	for len(videos) < opts.Videos {
		for _, t := range rnd.Perm(len(topics)) {
			if len(videos) >= opts.Videos {
				break
			}
			tp := topics[t]
			offset := time.Duration(rnd.Intn(max(1, opts.Days*24))) * time.Hour
			offset += time.Duration(rnd.Intn(60)) * time.Minute

			// 话题数多于作者数时按需补作者。
			// 不补的话 authors[t] 会是空切片，rnd.Intn(0) 直接 panic——
			// 而这正是 "-topics 调大一点就崩" 这类只在特定参数下出现的坑。
			if len(authors[t]) == 0 {
				authors[t] = append(authors[t], nextAuthor)
				nextAuthor++
			}
			authorID := authors[t][rnd.Intn(len(authors[t]))]

			videos = append(videos, evalset.Video{
				ID:          uint(len(videos) + 1),
				AuthorID:    authorID,
				CreateTime:  opts.Start.Add(offset),
				Title:       tp.title(rnd),
				Description: tp.description(rnd),
			})
			topicOf = append(topicOf, t)
			quality = append(quality, 0.7+rnd.Float64()*0.6)
		}
	}

	splitAt, err := evalset.ComputeSplit(videos, opts.SplitRatio)
	if err != nil {
		return nil, nil, err
	}

	// ---- 用户：每个用户对少数几个话题有明显偏好 ----
	prefs := make([][]float64, opts.Users)
	for u := 0; u < opts.Users; u++ {
		prefs[u] = userPreference(rnd, appeal)
	}

	// ---- 互动：按用户偏好采样 ----
	//
	// 训练期互动只发生在切分点之前发布的视频上（否则会用到"当时还不存在"的内容）；
	// 标签互动只发生在**候选视频**上（切分点之后发布的视频排序器看不到，
	// 给它打标签等于考一道没教过的题）。
	var interactions []evalset.Interaction
	labelsPerUser := make([]int, 0, opts.Users)
	preSplit := make([]int, 0, len(videos)) // 候选视频下标（切分点之前发布）
	for i, v := range videos {
		if v.CreateTime.Before(splitAt) {
			preSplit = append(preSplit, i)
		}
	}
	if len(preSplit) == 0 {
		return nil, nil, fmt.Errorf("切分点之前没有任何视频：调大 -days 或调小 -split")
	}

	labelWindow := time.Duration(opts.LabelWindowDays) * 24 * time.Hour

	for u := 0; u < opts.Users; u++ {
		account := uint(u + 1)

		// 训练期：按"用户偏好 × 视频质量"加权采样。
		// 这一步决定了 likes_count/popularity 这两个排序特征，
		// 因此它必须是**偏好驱动的**，而不是先有热度再倒推偏好。
		//
		// 权重为 0 表示"这件事不可能发生"：用户不会给自己发的视频点赞，
		// 而且自己发的视频本来也不会出现在自己的推荐流里
		// （见 evalset.CandidatesFor）。若不过滤，生成的标签里会混进
		// "用户与自己的视频互动"，而这类样本在评测时会被静默剔除，
		// 表现为每用户相关样本数莫名其妙地少于设定值。
		trainWeights := make([]float64, len(preSplit))
		for k, idx := range preSplit {
			if videos[idx].AuthorID == account {
				continue
			}
			trainWeights[k] = prefs[u][topicOf[idx]] * quality[idx]
		}
		for _, k := range weightedSampleWithoutReplacement(rnd, trainWeights, opts.TrainPerUser) {
			v := videos[preSplit[k]]
			span := splitAt.Sub(v.CreateTime)
			if span <= 0 {
				continue
			}
			at := v.CreateTime.Add(time.Duration(rnd.Int63n(int64(span))) + 1)
			interactions = append(interactions, evalset.Interaction{
				VideoID: v.ID, AccountID: account, Kind: evalset.KindLike, At: at,
			})
		}

		// 观测窗口：同样按偏好采样，但取不同的加权（避免与训练期完全同一批视频）。
		labelWeights := make([]float64, len(preSplit))
		for k, idx := range preSplit {
			if videos[idx].AuthorID == account {
				continue
			}
			// 质量的作用略小：标签反映的是"这个人会不会喜欢"，
			// 质量只影响曝光概率。全用质量会重新把热度引回标签里。
			labelWeights[k] = prefs[u][topicOf[idx]] * math.Sqrt(quality[idx])
		}
		chosen := weightedSampleWithoutReplacement(rnd, labelWeights, opts.LabelPerUser)
		relevant := 0
		for _, k := range chosen {
			v := videos[preSplit[k]]
			at := splitAt.Add(time.Duration(rnd.Int63n(int64(labelWindow))) + 1)
			kind := evalset.KindLike
			if rnd.Float64() < 0.3 {
				kind = evalset.KindComment
			}
			interactions = append(interactions, evalset.Interaction{
				VideoID: v.ID, AccountID: account, Kind: kind, At: at,
			})
			relevant++
		}
		labelsPerUser = append(labelsPerUser, relevant)
	}

	// 特征只能在切分点之前累积。
	//
	// 与 cmd/evalbaseline 走同一条逻辑（evalset.AsOf），而不是各写一份：
	// 两份实现里必然有一份会忘了过滤未来互动，而那种错的后果是
	// "基线数字虚高但完全看不出来"。
	likes, comments := splitByKind(interactions)
	features, note := evalset.AsOf(videos, likes, comments, splitAt)
	sortInteractions(interactions)

	sort.Ints(labelsPerUser)
	stats := &Stats{
		Videos:        len(videos),
		Candidates:    len(preSplit),
		Users:         opts.Users,
		Topics:        len(topics),
		Interactions:  len(interactions),
		LabelsPerUser: labelsPerUser,
	}

	snap := &evalset.Snapshot{
		GeneratedAt: opts.GeneratedAt.UTC(),
		// Source 前缀必须是 "synthetic"：评测渲染层据此打上
		// "不能作为结论"的横幅（见 evalset.IsSynthetic）。
		Source: fmt.Sprintf("synthetic(seed=%d,videos=%d,users=%d,topics=%d,days=%d)",
			opts.Seed, opts.Videos, opts.Users, opts.Topics, opts.Days),
		SplitAt:      splitAt,
		SplitRatio:   opts.SplitRatio,
		FeatureNote:  note + "；本快照由 internal/evalset/synth 合成，标签按用户话题偏好采样生成",
		Videos:       features,
		Interactions: interactions,
	}
	return snap, stats, nil
}

func (o Options) validate() error {
	if o.Videos < 10 {
		return fmt.Errorf("-videos 太小（%d）：生成的数据不会有意义", o.Videos)
	}
	if o.Users < 1 {
		return fmt.Errorf("-users 必须为正，实际 %d", o.Users)
	}
	if o.Topics < 2 || o.Topics > len(topicTable) {
		return fmt.Errorf("-topics 必须在 2~%d 之间，实际 %d", len(topicTable), o.Topics)
	}
	if o.SplitRatio <= 0 || o.SplitRatio >= 1 {
		return fmt.Errorf("-split 必须在 (0,1) 之间，实际 %v", o.SplitRatio)
	}
	if o.Days < 2 {
		return fmt.Errorf("-days 太小（%d）：时间切分不会产生两个有意义的窗口", o.Days)
	}
	if o.Start.IsZero() {
		return fmt.Errorf("Start 不能为零值：零值时间会被渲染成 0001-01-01")
	}
	if o.GeneratedAt.IsZero() {
		return fmt.Errorf("GeneratedAt 不能为零值（它的用途是记录生成时刻，见 cmd/evalseed 的说明）")
	}
	return nil
}

// userPreference 生成一个用户的"话题偏好分布"。
//
// 形状：少数几个核心话题占掉绝大部分权重（0.8），其余话题按全局吸引力
// 分掉剩下的 0.2。为什么保留背景权重而不是"只喜欢核心话题"：
// 真实用户会被热门内容吸引到核心兴趣之外，完全没有背景权重会让
// 热度排序的分数被压得极低——那同样是失真，只是方向相反。
func userPreference(rnd *rand.Rand, appeal []float64) []float64 {
	w := make([]float64, len(appeal))

	coreCount := 1 + rnd.Intn(3)
	if coreCount > len(w) {
		coreCount = len(w)
	}
	perm := rnd.Perm(len(w))
	core := make(map[int]bool, coreCount)
	for i := 0; i < coreCount; i++ {
		core[perm[i]] = true
	}

	// 核心权重随机拆分：不平均分配，否则"偏好强度"这个维度就没了。
	raw := make([]float64, coreCount)
	var rawSum float64
	for i := range raw {
		raw[i] = 0.3 + rnd.Float64()
		rawSum += raw[i]
	}

	background := 0.0
	for i := range w {
		if !core[i] {
			background += appeal[i]
		}
	}
	for i := range w {
		if core[i] {
			continue
		}
		if background > 0 {
			w[i] = 0.2 * appeal[i] / background
		}
	}
	for i, idx := range perm[:coreCount] {
		w[idx] = 0.8 * raw[i] / rawSum
	}
	return w
}

// weightedSampleWithoutReplacement 按权重**不放回**地抽 k 个下标。
//
// 用指数竞赛（key = -ln(U)/w，取最小的 k 个）而不是"轮盘赌逐个抽再剔除"：
// 轮盘赌在权重极不均匀时会因为浮点误差抽到重复下标，
// 而"少抽了一条"这种事在生成结果里完全看不出来。
// 附带好处是这个写法与遍历顺序无关，因此结果只取决于种子。
func weightedSampleWithoutReplacement(rnd *rand.Rand, weights []float64, k int) []int {
	if k <= 0 || len(weights) == 0 {
		return nil
	}
	if k > len(weights) {
		k = len(weights)
	}
	type keyed struct {
		key float64
		idx int
	}
	keys := make([]keyed, 0, len(weights))
	for i, w := range weights {
		if w <= 0 {
			// 权重为 0 表示"这个用户不可能看到它"。
			// 用 0 参与指数竞赛会得到 +Inf，被自然排到最后，行为正确。
			continue
		}
		u := rnd.Float64()
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		keys = append(keys, keyed{key: -math.Log(u) / w, idx: i})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key == keys[j].key {
			return keys[i].idx < keys[j].idx
		}
		return keys[i].key < keys[j].key
	})
	out := make([]int, 0, k)
	for i := 0; i < k && i < len(keys); i++ {
		out = append(out, keys[i].idx)
	}
	return out
}

func splitByKind(list []evalset.Interaction) (likes, comments []evalset.Interaction) {
	for _, it := range list {
		switch it.Kind {
		case evalset.KindLike:
			likes = append(likes, it)
		case evalset.KindComment:
			comments = append(comments, it)
		}
	}
	return likes, comments
}

// sortInteractions 让快照文件具有确定性。
//
// map 遍历顺序随机，若不排序则同一 seed 生成的文件每次都不一样，
// 快照就无法进 git 做 diff，"逐字节可复现"这条验收项也就不成立。
func sortInteractions(list []evalset.Interaction) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.VideoID != b.VideoID {
			return a.VideoID < b.VideoID
		}
		return a.Kind < b.Kind
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
