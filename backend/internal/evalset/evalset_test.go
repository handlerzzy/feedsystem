package evalset

import (
	"math"
	"strings"
	"testing"
	"time"
)

// 本文件是评测工具的"标尺校准"。
//
// 为什么指标必须有手算用例：Recall/NDCG 算错不会报错，只会让所有后续比较
// 失去意义——P2 精排"变好了 0.05"可能只是指标实现有 bug。
// 所以这里的期望值是**按公式手算出来的**，并写清推导过程，
// 而不是"跑一遍看看结果是多少就填多少"（那种测试只能防住未来的改动，
// 防不住一开始就写错的公式）。

func day(n int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

func vid(id uint, createdDay int, likes, popularity int64) Video {
	return Video{ID: id, AuthorID: 1, CreateTime: day(createdDay), LikesCount: likes, Popularity: popularity}
}

// ---------------------------------------------------------------------------
// 相关性标签
// ---------------------------------------------------------------------------

// TestRelevanceIgnoresFutureAndPrefersStrongerSignal 守住三条标签规则：
//  1. 切分点**之前**发生的互动不算标签（会泄漏未来）；
//  2. 点赞(2) 强于评论(1)；
//  3. 同一视频两者都有时取更强的那个，而不是相加。
func TestRelevanceIgnoresFutureAndPrefersStrongerSignal(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{vid(1, 1, 0, 0), vid(2, 1, 0, 0), vid(3, 1, 0, 0)},
		Interactions: []Interaction{
			{VideoID: 1, Kind: "like", At: day(5)},     // 切分点之前：不算
			{VideoID: 2, Kind: "comment", At: day(11)}, // 之后：弱相关
			{VideoID: 3, Kind: "comment", At: day(11)},
			{VideoID: 3, Kind: "like", At: day(12)}, // 同一视频：取更强的点赞
		},
	}
	rel := RelevanceOf(snap)

	if rel[1] != RelNone {
		t.Errorf("切分点之前的互动必须被忽略，vid=1 得到 %d", rel[1])
	}
	if rel[2] != RelComment {
		t.Errorf("评论应为弱相关(1)，vid=2 得到 %d", rel[2])
	}
	if rel[3] != RelLike {
		t.Errorf("同时有点赞与评论时应取点赞(2)，vid=3 得到 %d", rel[3])
	}
}

// TestRelevanceIgnoresUnknownKind 覆盖"出现未知互动类型"。
//
// 猜一个等级会把噪声写进标签，而标签错了所有指标都错却看不出来。
func TestRelevanceIgnoresUnknownKind(t *testing.T) {
	snap := &Snapshot{
		SplitAt:      day(10),
		Videos:       []Video{vid(1, 1, 0, 0)},
		Interactions: []Interaction{{VideoID: 1, Kind: "share", At: day(11)}},
	}
	if got := RelevanceOf(snap)[1]; got != RelNone {
		t.Errorf("未知互动类型不该产生相关性，实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 时间切分
// ---------------------------------------------------------------------------

// TestCandidatesUseOnlyPastVideos 守住"不使用未来信息"。
func TestCandidatesUseOnlyPastVideos(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos: []Video{
			vid(1, 5, 0, 0),  // 之前
			vid(2, 9, 0, 0),  // 之前（紧邻切分点）
			vid(3, 10, 0, 0), // 恰好等于切分点：不算候选（Before 是严格小于）
			vid(4, 11, 0, 0), // 之后
		},
	}
	cands := Candidates(snap)
	if len(cands) != 2 {
		t.Fatalf("候选集大小 = %d, want 2（只有切分点之前发布的）", len(cands))
	}
	for _, c := range cands {
		if c.ID != 1 && c.ID != 2 {
			t.Errorf("候选集里出现了不该有的视频: %d", c.ID)
		}
	}
}

// TestComputeSplitUsesQuantile 验证按分位切分而不是按固定时刻。
//
// 为什么重要：本地开发库与新库的绝对时间完全不同，固定时刻会让
// "候选集"在一台机器上为空、在另一台机器上是全部数据。
func TestComputeSplitUsesQuantile(t *testing.T) {
	videos := []Video{
		vid(1, 1, 0, 0), vid(2, 2, 0, 0), vid(3, 3, 0, 0), vid(4, 4, 0, 0),
	}
	at, err := ComputeSplit(videos, 0.5)
	if err != nil {
		t.Fatalf("ComputeSplit 失败: %v", err)
	}
	if !at.Equal(day(3)) {
		t.Errorf("中位数切分点 = %v, want %v", at, day(3))
	}
	if _, err := ComputeSplit(nil, 0.5); err == nil {
		t.Error("空数据集必须报错，而不是返回零值时间")
	}
}

// ---------------------------------------------------------------------------
// Recall@K：手算
// ---------------------------------------------------------------------------

// TestRecallAtKHandComputed 用手算例子钉住语义。
//
// 构造：5 条候选，其中 2 条相关（id=1 与 id=3），排序为 [1, 2, 3, 4, 5]。
//
//	Recall@1 = 命中 1 / 相关总数 2 = 0.5
//	Recall@2 = 0.5（第 2 位是无关的 id=2）
//	Recall@3 = 2/2 = 1.0
func TestRecallAtKHandComputed(t *testing.T) {
	ranked := []Video{vid(1, 1, 0, 0), vid(2, 1, 0, 0), vid(3, 1, 0, 0), vid(4, 1, 0, 0), vid(5, 1, 0, 0)}
	rel := map[uint]int{1: RelLike, 3: RelComment}

	cases := []struct {
		k    int
		want float64
	}{{1, 0.5}, {2, 0.5}, {3, 1.0}, {5, 1.0}}
	for _, tc := range cases {
		if got := RecallAtK(ranked, rel, tc.k); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Recall@%d = %v, want %v", tc.k, got, tc.want)
		}
	}
}

// TestRecallDenominatorExcludesNonCandidates 守住一个很容易写错的地方：
// Recall 的分母是"候选集里的相关样本"，不是"全部相关样本"。
//
// 如果分母用了全部相关视频，那么"切分点之后才发布、排序器根本看不到"的视频
// 也会被算进分母，所有策略的 Recall 都会被系统性压低，
// 且压低程度取决于数据分布——那样的数字无法用来比较策略。
func TestRecallDenominatorExcludesNonCandidates(t *testing.T) {
	// ranked 里只有 1 条相关，但 rel 里还有一条属于非候选（不在 ranked 中）。
	ranked := []Video{vid(1, 1, 0, 0), vid(2, 1, 0, 0)}
	rel := map[uint]int{1: RelLike, 99: RelLike} // 99 不在候选集里

	if got := RecallAtK(ranked, rel, 1); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("Recall@1 = %v, want 1.0（非候选视频不该进分母）", got)
	}
}

// TestRecallWithNoRelevantSamplesIsZero 覆盖"观测窗口内没有任何互动"。
//
// 返回 0（而不是 NaN 或 panic），并把"不可靠"交给 Warnings 表达——
// 让调用方有机会区分"效果为零"与"数据不足"。
func TestRecallWithNoRelevantSamplesIsZero(t *testing.T) {
	ranked := []Video{vid(1, 1, 0, 0), vid(2, 1, 0, 0)}
	if got := RecallAtK(ranked, map[uint]int{}, 1); got != 0 {
		t.Errorf("没有相关样本时 Recall 应为 0，实际 %v", got)
	}
}

// ---------------------------------------------------------------------------
// NDCG@K：手算
// ---------------------------------------------------------------------------

// TestNDCGAtKHandComputed 用完整的推导钉住公式。
//
// 构造：3 条候选，相关性 [2, 1, 0]，按此顺序排列（即理想排序）。
//
//	gain(2) = 2^2-1 = 3，gain(1) = 2^1-1 = 1，gain(0) = 0
//	discount(i) = log2(i+2)，所以第 1 位折扣 1、第 2 位 log2(3)、第 3 位 2
//
//	DCG  = 3/1 + 1/log2(3) + 0/2
//	IDCG = 与 DCG 相同（已经是理想顺序）
//	NDCG = 1.0
//
// 再把它倒过来排 [0, 1, 2]：
//
//	DCG  = 0/1 + 1/log2(3) + 3/2
//	两者相除即得期望值。
func TestNDCGAtKHandComputed(t *testing.T) {
	rel := map[uint]int{1: RelLike, 2: RelComment}

	log23 := math.Log2(3)

	t.Run("理想排序为 1.0", func(t *testing.T) {
		ranked := []Video{vid(1, 1, 0, 0), vid(2, 1, 0, 0), vid(3, 1, 0, 0)}
		if got := NDCGAtK(ranked, rel, 3); math.Abs(got-1.0) > 1e-9 {
			t.Errorf("NDCG@3 = %v, want 1.0", got)
		}
	})

	t.Run("相关视频被排到最后", func(t *testing.T) {
		ranked := []Video{vid(3, 1, 0, 0), vid(2, 1, 0, 0), vid(1, 1, 0, 0)}
		dcg := 0/math.Log2(2) + 1/log23 + 3/math.Log2(4)
		idcg := 3/math.Log2(2) + 1/log23 + 0/math.Log2(4)
		want := dcg / idcg
		if got := NDCGAtK(ranked, rel, 3); math.Abs(got-want) > 1e-9 {
			t.Errorf("NDCG@3 = %v, want %v", got, want)
		}
		if want >= 1.0 {
			t.Fatal("测试构造有误：把相关视频排到最后不该得到满分")
		}
	})

	t.Run("K 截断到 1 时只看首位", func(t *testing.T) {
		// 首位是无关视频 → DCG@1 = 0，理想首位是 rel=2 → IDCG@1 = 3。
		ranked := []Video{vid(3, 1, 0, 0), vid(1, 1, 0, 0), vid(2, 1, 0, 0)}
		if got := NDCGAtK(ranked, rel, 1); math.Abs(got-0) > 1e-9 {
			t.Errorf("NDCG@1 = %v, want 0（首位无关）", got)
		}
		// 首位是强相关 → 1.0
		ranked2 := []Video{vid(1, 1, 0, 0), vid(3, 1, 0, 0)}
		if got := NDCGAtK(ranked2, rel, 1); math.Abs(got-1.0) > 1e-9 {
			t.Errorf("NDCG@1 = %v, want 1.0", got)
		}
	})
}

// TestNDCGIsMonotonicInRank 验证"相关视频排得越靠前，分数越高"。
//
// 这是 NDCG 最核心的性质，也是 P2 判断"精排是否真的更好"的依据。
func TestNDCGIsMonotonicInRank(t *testing.T) {
	rel := map[uint]int{1: RelLike}
	prev := -1.0
	for pos := 0; pos < 5; pos++ {
		ranked := make([]Video, 0, 5)
		for i := 0; i < 5; i++ {
			if i == pos {
				ranked = append(ranked, vid(1, 1, 0, 0))
			} else {
				ranked = append(ranked, vid(uint(100+i), 1, 0, 0))
			}
		}
		got := NDCGAtK(ranked, rel, 5)
		if prev >= 0 && got > prev+1e-9 {
			t.Errorf("相关视频排到第 %d 位时 NDCG 反而变高了: %v > %v", pos+1, got, prev)
		}
		prev = got
	}
}

// ---------------------------------------------------------------------------
// 排序策略
// ---------------------------------------------------------------------------

// TestBuiltinRankersMatchProductionSQL 钉住三个内置排序与线上 SQL 的对应关系。
//
// 逐一对照 internal/feed/repo.go：
//
//	timeline    → ORDER BY create_time DESC
//	popularity  → ORDER BY popularity DESC, create_time DESC, id DESC
//	likes_count → ORDER BY likes_count DESC, id DESC
//
// 这几条规则一旦与线上不一致，基线数字就与线上表现无关，
// 而报告里完全看不出来——所以必须有断言。
func TestBuiltinRankersMatchProductionSQL(t *testing.T) {
	rankers := BuiltinRankers()
	byName := map[string]Ranker{}
	for _, r := range rankers {
		byName[r.Name] = r
	}

	t.Run("timeline 按发布时间倒序", func(t *testing.T) {
		r := byName["timeline"]
		got := Rank([]Video{vid(1, 1, 0, 0), vid(2, 3, 0, 0), vid(3, 2, 0, 0)}, r)
		if got[0].ID != 2 || got[1].ID != 3 || got[2].ID != 1 {
			t.Errorf("顺序 = %v, want [2 3 1]", ids(got))
		}
	})

	t.Run("popularity 优先热度，其次时间，最后 id", func(t *testing.T) {
		r := byName["popularity"]
		videos := []Video{
			vid(1, 5, 0, 100),
			vid(2, 9, 0, 100), // 热度相同、时间更新 → 应排在 1 前面
			vid(3, 1, 0, 200), // 热度最高
			vid(4, 9, 0, 100), // 与 2 完全同热度同时间 → id 大的在前
		}
		got := Rank(videos, r)
		want := []uint{3, 4, 2, 1}
		if !equalIDs(got, want) {
			t.Errorf("顺序 = %v, want %v", ids(got), want)
		}
	})

	t.Run("likes_count 优先点赞数，其次 id", func(t *testing.T) {
		r := byName["likes_count"]
		videos := []Video{
			vid(1, 1, 10, 0),
			vid(2, 1, 10, 0), // 同点赞数 → id 大的在前
			vid(3, 1, 99, 0),
		}
		got := Rank(videos, r)
		want := []uint{3, 2, 1}
		if !equalIDs(got, want) {
			t.Errorf("顺序 = %v, want %v", ids(got), want)
		}
	})
}

// TestRankDoesNotMutateInput 覆盖一个隐蔽的坑：
// 评测多个策略时会共用同一份候选集，若 Rank 原地排序，
// 第二个策略拿到的就是"已经被第一个策略排过序"的数据——
// 结果看起来正常，但所有策略的数字都会错。
func TestRankDoesNotMutateInput(t *testing.T) {
	original := []Video{vid(1, 3, 0, 0), vid(2, 1, 0, 0), vid(3, 2, 0, 0)}
	before := ids(original)

	_ = Rank(original, BuiltinRankers()[0])

	if !equalIDs(original, before) {
		t.Errorf("Rank 改动了入参顺序: %v → %v", before, ids(original))
	}
}

// ---------------------------------------------------------------------------
// 整体评测与可靠性标注
// ---------------------------------------------------------------------------

// TestEvaluateFlagsUnreliableSmallSample 守住"数据不足必须显式说出来"。
//
// 这是本文件里最重要的一条产品性断言：小样本也会算出看起来正常的数字，
// 如果报告不标注不可靠，它会被当成结论使用——那比没有报告更危险。
func TestEvaluateFlagsUnreliableSmallSample(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{vid(1, 1, 0, 0), vid(2, 2, 0, 0), vid(3, 20, 0, 0)},
		Interactions: []Interaction{
			{VideoID: 1, Kind: "like", At: day(11)},
		},
		SplitRatio: 0.5,
	}
	res, err := Evaluate(snap, BuiltinRankers(), []int{5, 10})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	if res.Reliable {
		t.Error("候选集 2 条、相关样本 1 条时必须标注不可靠")
	}
	if len(res.Warnings) == 0 {
		t.Error("不可靠时必须给出具体原因，否则报告无法自证")
	}
	if res.Candidates != 2 {
		t.Errorf("候选集 = %d, want 2", res.Candidates)
	}
	if res.Relevant != 1 {
		t.Errorf("相关样本 = %d, want 1", res.Relevant)
	}
	if len(res.Rankers) != 3 {
		t.Errorf("应评测 3 个内置策略，实际 %d", len(res.Rankers))
	}
}

// TestEvaluateRejectsEmptyInput 覆盖参数错误路径：
// 空候选集/空排序器/非法 K 都必须报错，而不是返回一堆 0。
func TestEvaluateRejectsEmptyInput(t *testing.T) {
	empty := &Snapshot{SplitAt: day(10), Videos: []Video{vid(1, 20, 0, 0)}}
	if _, err := Evaluate(empty, BuiltinRankers(), []int{5}); err == nil {
		t.Error("候选集为空时必须报错")
	}
	ok := &Snapshot{SplitAt: day(10), Videos: []Video{vid(1, 1, 0, 0)}}
	if _, err := Evaluate(ok, nil, []int{5}); err == nil {
		t.Error("没有排序器时必须报错")
	}
	if _, err := Evaluate(ok, BuiltinRankers(), []int{0}); err == nil {
		t.Error("K=0 时必须报错")
	}
	if _, err := Evaluate(ok, BuiltinRankers(), nil); err == nil {
		t.Error("K 为空时必须报错")
	}
}

// TestEvaluateIsDeterministic 守住可复现性：
// 同一份快照跑两次必须逐位相同（否则"重跑一遍核对"这件事就不成立）。
func TestEvaluateIsDeterministic(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{vid(1, 1, 5, 10), vid(2, 2, 3, 30), vid(3, 3, 9, 20), vid(4, 20, 1, 1)},
		Interactions: []Interaction{
			{VideoID: 1, Kind: "like", At: day(11)},
			{VideoID: 2, Kind: "comment", At: day(12)},
			{VideoID: 3, Kind: "like", At: day(13)},
		},
		SplitRatio: 0.5,
	}
	first, err := Evaluate(snap, BuiltinRankers(), []int{1, 2, 3})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	second, err := Evaluate(snap, BuiltinRankers(), []int{1, 2, 3})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	for i := range first.Rankers {
		for j := range first.Rankers[i].Results {
			a := first.Rankers[i].Results[j]
			b := second.Rankers[i].Results[j]
			if a != b {
				t.Errorf("第 %d 个策略 K=%d 两次结果不同: %+v vs %+v", i, a.K, a, b)
			}
		}
	}
}

func ids(vs []Video) []uint {
	out := make([]uint, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.ID)
	}
	return out
}

func equalIDs(vs []Video, want []uint) bool {
	got := ids(vs)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 特征不含未来信息（review 抓到的真实缺陷）
// ---------------------------------------------------------------------------

// TestAsOfUsesOnlyPreSplitInteractions 是本文件最重要的一条断言。
//
// 缺陷背景：videos.likes_count / videos.popularity 是会被持续累加的列，
// 导出时的值里已经包含了标签窗口内的互动。直接拿它当排序特征就等于
// "用答案排序"——基线数字虚高，而报告里完全看不出来（review 实测：
// 候选视频的 likes_count 恰好等于它之后获得的点赞数）。
func TestAsOfUsesOnlyPreSplitInteractions(t *testing.T) {
	videos := []Video{vid(1, 1, 999, 999), vid(2, 1, 999, 999), vid(3, 1, 999, 999)}
	likes := []Interaction{
		{VideoID: 1, Kind: KindLike, At: day(5)},  // 切分点之前
		{VideoID: 1, Kind: KindLike, At: day(6)},  // 切分点之前
		{VideoID: 1, Kind: KindLike, At: day(11)}, // 之后：绝不能计入
		{VideoID: 2, Kind: KindLike, At: day(12)}, // 之后：vid=2 训练期点赞数为 0
	}
	comments := []Interaction{
		{VideoID: 1, Kind: KindComment, At: day(7)},  // 之前
		{VideoID: 3, Kind: KindComment, At: day(11)}, // 之后
	}

	got, note := AsOf(videos, likes, comments, day(10))

	if got[0].LikesCount != 2 {
		t.Errorf("vid=1 的切分点前点赞数 = %d, want 2（导出时的 999 与之后的点赞都不能算）",
			got[0].LikesCount)
	}
	if got[1].LikesCount != 0 {
		t.Errorf("vid=2 在切分点之前没有点赞，应得 0，实际 %d", got[1].LikesCount)
	}
	if got[2].LikesCount != 0 || got[2].Popularity != 0 {
		t.Errorf("vid=3 的互动都在切分点之后，特征应为 0，实际 likes=%d pop=%d",
			got[2].LikesCount, got[2].Popularity)
	}
	// 代理热度必须显式说明，否则读者会以为是线上那个 popularity。
	if note == "" {
		t.Error("必须给出特征口径说明（popularity 是代理值）")
	}
}

// TestApplyFeaturesOverwritesStaleValues 守住"从文件读"这条路径也不会漏。
//
// 快照文件是会被长期保存的，手写或旧版本的快照里可能就是导出时刻的特征值。
// 加载时统一重算，等于把"特征不含未来信息"变成文件加载的必然结果。
func TestApplyFeaturesOverwritesStaleValues(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{vid(1, 1, 999, 999)},
		Interactions: []Interaction{
			{VideoID: 1, Kind: KindLike, At: day(11)}, // 之后 → 不能计入
		},
	}
	snap.ApplyFeatures()
	if snap.Videos[0].LikesCount != 0 {
		t.Errorf("陈旧的特征值必须被重算覆盖，实际 %d", snap.Videos[0].LikesCount)
	}
	if snap.FeatureNote == "" {
		t.Error("重算后必须带上口径说明")
	}
}

// TestRebuildRecomputesSplitAndFeatures 守住 -split 的语义：
// 显式重切时必须把切分点与特征一起重算。
//
// 只改 SplitRatio 字段而留着旧 SplitAt，报告就会写着"按 80% 分位切分"
// 而实际按 50% 算——一个只会误导人的元数据（review 抓到的真实问题）。
func TestRebuildRecomputesSplitAndFeatures(t *testing.T) {
	var videos []Video
	for i := 1; i <= 10; i++ {
		videos = append(videos, vid(uint(i), i, 0, 0))
	}
	snap := &Snapshot{Videos: videos, SplitRatio: 0.5, SplitAt: day(6)}
	if err := snap.Rebuild(0.8, videos, nil, nil); err != nil {
		t.Fatalf("Rebuild 失败: %v", err)
	}
	if !snap.SplitAt.Equal(day(9)) {
		t.Errorf("切分点 = %v, want %v（按百分之八十的分位）", snap.SplitAt, day(9))
	}
	if snap.SplitRatio != 0.8 {
		t.Errorf("SplitRatio = %v, want 0.8", snap.SplitRatio)
	}
	// 重新切分后候选集应当只剩 8 条，而不是原来的 5 条。
	if got := len(Candidates(snap)); got != 8 {
		t.Errorf("候选集 = %d, want 8", got)
	}
	if err := snap.Rebuild(1.5, videos, nil, nil); err == nil {
		t.Error("非法比例必须报错")
	}
}

// ---------------------------------------------------------------------------
// 报告渲染
// ---------------------------------------------------------------------------

// TestRenderSyntheticWarningIsUnmissable 守住"合成数据不能被当成结论"。
//
// 合成数据的标签是按规则造的，分数会异常漂亮（NDCG 到 1.0）。
// 报告一旦脱离上下文流传，最容易被引用的就是那张表——
// 因此警示必须出现在终端与 Markdown 的显眼位置。
func TestRenderSyntheticWarningIsUnmissable(t *testing.T) {
	snap := &Snapshot{
		Source:     "synthetic(seed=1,videos=10,days=30)",
		SplitAt:    day(10),
		SplitRatio: 0.5,
		Videos:     []Video{vid(1, 1, 0, 0), vid(2, 20, 0, 0)},
	}
	res, err := Evaluate(snap, BuiltinRankers(), []int{5})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}

	text := RenderText(res)
	if !strings.Contains(text, "合成数据") {
		t.Errorf("终端输出必须标注合成数据：\n%s", text)
	}
	md := RenderMarkdown(res, snap)
	if !strings.Contains(md, "这是合成数据") {
		t.Errorf("Markdown 必须在标题下方标注合成数据：\n%s", md)
	}
}

// TestRenderRealSnapshotHasNoSyntheticWarning 反向守一遍：
// 真实数据不该被误标成合成（那会让真正的基线报告失去可信度）。
func TestRenderRealSnapshotHasNoSyntheticWarning(t *testing.T) {
	snap := &Snapshot{
		Source:     "mysql/feedflow",
		SplitAt:    day(10),
		SplitRatio: 0.5,
		Videos:     []Video{vid(1, 1, 0, 0), vid(2, 20, 0, 0)},
	}
	res, err := Evaluate(snap, BuiltinRankers(), []int{5})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	if strings.Contains(RenderText(res), "合成数据") {
		t.Error("真实数据不该出现合成数据警示")
	}
	if strings.Contains(RenderMarkdown(res, snap), "这是合成数据") {
		t.Error("真实数据的 Markdown 不该出现合成数据警示")
	}
}

// TestRenderHandlesEmptyRankers 覆盖"没有排序器"时不 panic。
func TestRenderHandlesEmptyRankers(t *testing.T) {
	res := &EvalResult{Source: "mysql/x", SplitAt: day(1), Candidates: 1}
	if out := RenderText(res); out == "" {
		t.Error("RenderText 不该返回空串")
	}
	if out := RenderMarkdown(res, &Snapshot{Source: "mysql/x"}); out == "" {
		t.Error("RenderMarkdown 不该返回空串")
	}
}

// TestIsSynthetic 覆盖来源判定的边界。
func TestIsSynthetic(t *testing.T) {
	cases := map[string]bool{
		"synthetic(seed=1)": true,
		"synthetic":         true,
		"mysql/feedflow":  false,
		"":                  false,
		"syntheti":          false,
	}
	for source, want := range cases {
		if got := IsSynthetic(source); got != want {
			t.Errorf("IsSynthetic(%q) = %v, want %v", source, got, want)
		}
	}
}
