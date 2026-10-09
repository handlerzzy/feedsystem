package evalset

import (
	"math"
	"strings"
	"testing"
)

// 本文件钉住"按用户分组"这套口径（P2 前置项 B1）。
//
// 为什么这部分必须有独立验证：全局口径下"给用户推对了多少"根本没有被度量，
// 而 P2 的全部结论都要靠这套数字。它算错的后果不是报错，而是
// "语义召回让 NDCG 提升了 0.05"这种看起来完全正常的假结论。
// 因此这里的期望值同样是**按公式手算**出来的。

// pv 构造一条带作者与标题的视频，用于按用户分组的用例。
func pv(id, author uint, createdDay int, popularity int64, title string) Video {
	return Video{
		ID:         id,
		AuthorID:   author,
		CreateTime: day(createdDay),
		Popularity: popularity,
		Title:      title,
	}
}

// popRanker 返回一个"按 popularity 降序"的全局策略，便于手算期望值。
//
// 不直接用 BuiltinRankers()：内置策略里的并列规则（create_time、id）
// 会把手算过程拖进一堆无关细节，而这个用例要验证的是**分组与平均**。
func popRanker() Ranker {
	return Ranker{
		Name: "popularity",
		Less: func(a, b Video) bool {
			if a.Popularity != b.Popularity {
				return a.Popularity > b.Popularity
			}
			return a.ID > b.ID
		},
	}
}

// TestRelevanceOfUserOnlyCountsThatUser 守住"标签只来自该用户自己"。
//
// 这是按用户口径的地基：一旦把别人的互动算进来，指标立刻退化成全局热度，
// 而数字仍然看起来很正常。
func TestRelevanceOfUserOnlyCountsThatUser(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{pv(1, 100, 1, 0, ""), pv(2, 100, 1, 0, "")},
		Interactions: []Interaction{
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(11)},
			{VideoID: 2, AccountID: 8, Kind: KindLike, At: day(11)},
		},
	}
	u7 := RelevanceOfUser(snap, 7)
	if u7[1] != RelLike {
		t.Errorf("用户 7 对 vid=1 的相关性 = %d, want %d", u7[1], RelLike)
	}
	if u7[2] != RelNone {
		t.Errorf("用户 7 不该拿到别人（用户 8）的互动，实际 %d", u7[2])
	}
	if got := RelevanceOfUser(snap, 99); len(got) != 0 {
		t.Errorf("不存在的用户不该有任何标签，实际 %v", got)
	}

	// 全局口径仍然是"所有用户的并集"，两者不能互相污染。
	all := RelevanceOf(snap)
	if all[1] != RelLike || all[2] != RelLike {
		t.Errorf("全局口径应包含两个人的互动，实际 %v", all)
	}
}

// TestRelevanceOfUserIgnoresPreSplitAndUnknownKind 复用全局口径的两条规则：
// 切分点之前的互动不算标签；未知互动类型忽略而不是猜。
func TestRelevanceOfUserIgnoresPreSplitAndUnknownKind(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Interactions: []Interaction{
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(9)},     // 切分点之前
			{VideoID: 1, AccountID: 7, Kind: "share", At: day(11)},     // 未知类型
			{VideoID: 1, AccountID: 7, Kind: KindComment, At: day(11)}, // 有效：弱相关
		},
	}
	if got := RelevanceOfUser(snap, 7)[1]; got != RelComment {
		t.Errorf("vid=1 的相关性 = %d, want %d（未来互动与未知类型都必须被忽略）", got, RelComment)
	}
}

// TestCandidatesForExcludesSelfAndFuture 守住按用户候选集的两条规则。
func TestCandidatesForExcludesSelfAndFuture(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos: []Video{
			pv(1, 100, 1, 0, ""),  // 别人的、切分点之前 → 候选
			pv(2, 7, 1, 0, ""),    // 自己发的 → 不是候选
			pv(3, 100, 10, 0, ""), // 恰好等于切分点 → 不是候选（严格小于）
			pv(4, 100, 11, 0, ""), // 切分点之后 → 不是候选
		},
	}
	got := ids(CandidatesFor(snap, 7))
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("用户 7 的候选集 = %v, want [1]", got)
	}
	// 换个用户，候选集应当不同：作者 100 看不到自己发的那几条。
	got = ids(CandidatesFor(snap, 100))
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("用户 100 的候选集 = %v, want [2]（1/3/4 都是他自己发的）", got)
	}
	// 全局候选集不排除任何人，保持与 P0 一致。
	if len(Candidates(snap)) != 2 {
		t.Errorf("全局候选集 = %v, want 2 条（1 与 2）", ids(Candidates(snap)))
	}
}

// TestEvaluatePerUserHandComputed 用手算例子钉住"每用户算、再平均"。
//
// 构造（SplitAt = day(10)）：
//
//	v1 author=100 popularity=100
//	v2 author=100 popularity= 90
//	v3 author=200 popularity= 10
//	v4 author=200 popularity=  5
//
// 用户 7：观测窗口内点赞了 v1、v2  → 相关集合 {1,2}
// 用户 8：观测窗口内点赞了 v3      → 相关集合 {3}
//
// popularity 排序恒为 [1,2,3,4]（与用户无关，这是全局策略）。
//
//	用户 7：R@1 = 1/2 = 0.5，R@3 = 2/2 = 1.0，N@1 = 1.0，N@3 = 1.0
//	用户 8：R@1 = 0，      R@3 = 1/1 = 1.0，N@1 = 0，  N@3 = 1.5/3 = 0.5
//
// 平均：R@1 = (0.5+0)/2 = 0.25；R@3 = 1.0；N@1 = (1.0+0)/2 = 0.5；N@3 = (1.0+0.5)/2 = 0.75
func TestEvaluatePerUserHandComputed(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos: []Video{
			pv(1, 100, 1, 100, ""),
			pv(2, 100, 2, 90, ""),
			pv(3, 200, 3, 10, ""),
			pv(4, 200, 4, 5, ""),
		},
		Interactions: []Interaction{
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(11)},
			{VideoID: 2, AccountID: 7, Kind: KindLike, At: day(11)},
			{VideoID: 3, AccountID: 8, Kind: KindLike, At: day(11)},
		},
	}

	got, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1, 3})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	if got == nil {
		t.Fatal("有 account_id 时必须产出按用户结果")
	}
	if got.Users != 2 || got.EvaluatedUsers != 2 {
		t.Errorf("用户数/参与评测用户数 = %d/%d, want 2/2", got.Users, got.EvaluatedUsers)
	}
	if got.SkippedNoRelevant != 0 {
		t.Errorf("不该有被剔除的用户，实际 %d", got.SkippedNoRelevant)
	}
	if got.RelevantPerUser.Min != 1 || got.RelevantPerUser.Max != 2 || got.RelevantPerUser.P50 != 2 {
		t.Errorf("每用户相关样本分布 = %+v, want min=1 p50=2 max=2", got.RelevantPerUser)
	}
	if len(got.Rankers) != 1 {
		t.Fatalf("应评测 1 个策略，实际 %d", len(got.Rankers))
	}

	want := []KResult{
		{K: 1, Recall: 0.25, NDCG: 0.5},
		{K: 3, Recall: 1.0, NDCG: 0.75},
	}
	for i, w := range want {
		g := got.Rankers[0].Results[i]
		if g.K != w.K {
			t.Fatalf("第 %d 个 K = %d, want %d", i, g.K, w.K)
		}
		if math.Abs(g.Recall-w.Recall) > 1e-9 {
			t.Errorf("K=%d Recall = %v, want %v", g.K, g.Recall, w.Recall)
		}
		if math.Abs(g.NDCG-w.NDCG) > 1e-9 {
			t.Errorf("K=%d NDCG = %v, want %v", g.K, g.NDCG, w.NDCG)
		}
	}
}

// TestEvaluatePerUserDiffersBetweenUsers 是 B1 验收清单里的第一条：
// **对同一个候选集、两个不同用户，排序结果不同，指标也不同。**
//
// 这里用一个人为的个性化策略（按"用户偏好集合"排序）来证明这条性质
// 在评测框架里真的成立——P2 的语义精排就是这样一个策略。
func TestEvaluatePerUserDiffersBetweenUsers(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos: []Video{
			pv(1, 100, 1, 0, "go"), pv(2, 100, 2, 0, "cat"),
			pv(3, 200, 3, 0, "go"), pv(4, 200, 4, 0, "cat"),
		},
		Interactions: []Interaction{
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(11)},
			{VideoID: 3, AccountID: 7, Kind: KindLike, At: day(11)},
			{VideoID: 2, AccountID: 8, Kind: KindLike, At: day(11)},
			{VideoID: 4, AccountID: 8, Kind: KindLike, At: day(11)},
		},
	}
	// 用户 7 喜欢 go 主题，用户 8 喜欢 cat 主题。
	prefersGo := func(accountID uint, a, b Video) bool {
		if accountID == 7 {
			return a.Title == "go" && b.Title != "go"
		}
		return a.Title == "cat" && b.Title != "cat"
	}
	r := Ranker{Name: "semantic", Personalized: prefersGo}

	cands := CandidatesFor(snap, 7)
	rank7 := RankFor(7, cands, r)
	rank8 := RankFor(8, cands, r)
	if equalIDs(rank7, ids(rank8)) {
		t.Fatal("同一个候选集在两个用户下必须排出不同顺序")
	}
	if !equalIDs(rank7, []uint{1, 3, 2, 4}) {
		t.Errorf("用户 7 的排序 = %v, want [1 3 2 4]（go 优先，同类保持稳定顺序）", ids(rank7))
	}
	if !equalIDs(rank8, []uint{2, 4, 1, 3}) {
		t.Errorf("用户 8 的排序 = %v, want [2 4 1 3]", ids(rank8))
	}

	res, err := EvaluatePerUser(snap, []Ranker{r}, []int{2})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	// 两个用户各自的相关视频都被个性化策略排到最前，因此都是满分；
	// 换成全局的 popularity 策略则不可能同时对两个人都成立。
	if res.Rankers[0].Results[0].NDCG != 1 {
		t.Errorf("个性化策略对两个用户都应满分，实际 %v", res.Rankers[0].Results[0].NDCG)
	}

	// 同一个候选集、同一个策略，两个用户的**单用户**指标必须能分别算出来，
	// 且在这个例子里是不同的（用户 7 的两条相关视频都能进前 2，用户 8 也是，
	// 但一旦把 K 收紧到 1，两者的差异才会显现）。
	one, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	// popularity 全为 0（并列按 id 降序 → [4,3,2,1]），每个用户有 2 条相关样本：
	// 用户 7 的相关是 {1,3} → 前 1 只命中 v4 这一条无关的 → 0/2 = 0；
	// 用户 8 的相关是 {2,4} → 前 1 命中 v4 → 1/2 = 0.5。
	// 平均 = 0.25。注意这个 0.25 只在按用户分组时才有意义：
	// 全局口径下 v4 是"被互动过"的，会得到另一个完全不同的数字。
	if got := one.Rankers[0].Results[0].Recall; math.Abs(got-0.25) > 1e-9 {
		t.Errorf("平均 Recall@1 = %v, want 0.25", got)
	}
}

// TestEvaluatePerUserSkipsUsersWithoutRelevant 守住"无法度量 != 得 0 分"。
//
// 没有相关样本的用户若被算成 0，指标会随着"有多少沉默用户"漂移，
// 而沉默用户的比例完全是数据采集的产物，不是推荐质量。
func TestEvaluatePerUserSkipsUsersWithoutRelevant(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{pv(1, 100, 1, 10, ""), pv(2, 100, 2, 5, "")},
		Interactions: []Interaction{
			// 用户 7 只有切分点**之前**的互动 → 没有标签
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(5)},
			// 用户 8 有观测窗口内的互动
			{VideoID: 1, AccountID: 8, Kind: KindLike, At: day(11)},
		},
	}
	got, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	if got.Users != 2 {
		t.Errorf("用户总数 = %d, want 2", got.Users)
	}
	if got.EvaluatedUsers != 1 || got.SkippedNoRelevant != 1 {
		t.Errorf("参与评测/剔除 = %d/%d, want 1/1", got.EvaluatedUsers, got.SkippedNoRelevant)
	}
	if got.Rankers[0].Results[0].Recall != 1 {
		t.Errorf("只按用户 8 计算应为 1.0，实际 %v", got.Rankers[0].Results[0].Recall)
	}
	if len(got.Notes) == 0 {
		t.Error("剔除用户必须留下说明，否则报告读者不知道平均值覆盖了谁")
	}
}

// TestEvaluatePerUserReliableThresholds 守住"用户数/每用户样本数都要纳入判定"。
//
// 前置条件 §1 明确要求阈值随口径更新：只看全局相关样本数会漏掉
// "1 个用户贡献了全部标签"这种数据。
func TestEvaluatePerUserReliableThresholds(t *testing.T) {
	// 造 minUsers 个用户，每个用户只有 1 条相关样本 → 用户数达标、每用户样本数不达标。
	snap := &Snapshot{SplitAt: day(10)}
	snap.Videos = append(snap.Videos, pv(1, 999, 1, 0, ""))
	snap.Videos = append(snap.Videos, pv(2, 999, 2, 0, ""))
	for i := 0; i < minUsers; i++ {
		account := uint(1000 + i)
		video := uint(1)
		if i%2 == 0 {
			video = 2
		}
		snap.Interactions = append(snap.Interactions, Interaction{
			VideoID: video, AccountID: account, Kind: KindLike, At: day(11),
		})
	}
	got, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	if got.EvaluatedUsers != minUsers {
		t.Fatalf("参与评测用户数 = %d, want %d", got.EvaluatedUsers, minUsers)
	}
	if got.Reliable {
		t.Error("每用户相关样本只有 1 条时必须判为不可靠")
	}
	if !hasWarningContaining(got.Warnings, "每用户相关样本") {
		t.Errorf("不可靠的原因必须指向每用户样本数，实际 %v", got.Warnings)
	}

	// 每用户样本数达标但用户数不达标 → 仍然不可靠。
	small := &Snapshot{SplitAt: day(10)}
	for i := 0; i < 10; i++ {
		small.Videos = append(small.Videos, pv(uint(i+1), 999, 1, 0, ""))
	}
	for i := 0; i < minRelevantPerUser; i++ {
		small.Interactions = append(small.Interactions, Interaction{
			VideoID: uint(i%10 + 1), AccountID: 7, Kind: KindLike, At: day(11),
		})
	}
	got2, err := EvaluatePerUser(small, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	if got2.Reliable {
		t.Error("只有 1 个用户时必须判为不可靠")
	}
	if !hasWarningContaining(got2.Warnings, "参与评测的用户") {
		t.Errorf("不可靠的原因必须指向用户数，实际 %v", got2.Warnings)
	}
}

// TestEvaluatePerUserNilWithoutAccounts 守住旧快照的兼容路径。
//
// P0 导出的 snapshot.json 没有 account_id。读它时不能报错，
// 但必须让渲染层能说出"这份数据不能评估 P2"。
func TestEvaluatePerUserNilWithoutAccounts(t *testing.T) {
	snap := &Snapshot{
		SplitAt:      day(10),
		Videos:       []Video{pv(1, 100, 1, 0, "")},
		Interactions: []Interaction{{VideoID: 1, Kind: KindLike, At: day(11)}},
	}
	got, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("旧快照不该让评测失败: %v", err)
	}
	if got != nil {
		t.Fatalf("没有 account_id 时应返回 nil，实际 %+v", got)
	}

	res, err := Evaluate(snap, []Ranker{popRanker()}, []int{1})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	if res.PerUser != nil {
		t.Fatal("Evaluate 也不该凭空造出按用户结果")
	}
	if !strings.Contains(RenderText(res), "无法做按用户分组的评测") {
		t.Errorf("终端输出必须显式说明这份数据不能评估 P2：\n%s", RenderText(res))
	}
	if !strings.Contains(RenderMarkdown(res, snap), "没有用户维度") {
		t.Errorf("Markdown 必须显式说明这份数据不能评估 P2：\n%s", RenderMarkdown(res, snap))
	}
}

// TestEvaluateExcludesPersonalizedFromGlobalTable 守住两套口径不混用。
//
// 个性化策略在全局口径下没有定义：同一候选集对不同用户顺序不同，
// 而全局 rel 是所有人互动的并集。把它算进全局表只会得到一个
// 无法解释的数字（前置条件 §1 红线：不得混在一个指标里）。
func TestEvaluateExcludesPersonalizedFromGlobalTable(t *testing.T) {
	snap := &Snapshot{
		SplitAt: day(10),
		Videos:  []Video{pv(1, 100, 1, 10, ""), pv(2, 100, 2, 5, "")},
		Interactions: []Interaction{
			{VideoID: 1, AccountID: 7, Kind: KindLike, At: day(11)},
		},
	}
	personalized := Ranker{Name: "semantic", Personalized: func(_ uint, a, b Video) bool { return a.ID < b.ID }}
	res, err := Evaluate(snap, []Ranker{popRanker(), personalized}, []int{1})
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	if len(res.Rankers) != 1 || res.Rankers[0].Name != "popularity" {
		t.Errorf("全局表里应只有非个性化策略，实际 %+v", res.Rankers)
	}
	if res.PerUser == nil || len(res.PerUser.Rankers) != 2 {
		t.Fatalf("按用户表里应同时有两个策略，实际 %+v", res.PerUser)
	}
	if !strings.Contains(RenderText(res), "按用户分组口径") {
		t.Error("终端输出必须给出按用户分组的段落标题")
	}
	if !strings.Contains(RenderText(res), "不能用于评估 P2") {
		t.Error("全局口径必须标注它不能用于评估 P2")
	}
}

// TestRankRejectsPersonalized 守住"用错入口立刻失败"。
//
// Rank 对个性化策略会拿到 nil 的 Less，那是 panic（而且是难查的那种）。
// 与其让它以空指针的形式炸在 sort 内部，不如在入口显式拒绝。
func TestRankRejectsPersonalized(t *testing.T) {
	r := Ranker{Name: "semantic", Personalized: func(_ uint, a, b Video) bool { return a.ID < b.ID }}
	defer func() {
		if recover() == nil {
			t.Error("Rank 遇到个性化策略必须 panic，而不是静默用 nil Less")
		}
	}()
	Rank([]Video{pv(1, 1, 1, 0, "")}, r)
}

// TestSummarizeDistNearestRank 覆盖分布统计的取值规则。
func TestSummarizeDistNearestRank(t *testing.T) {
	if got := SummarizeDist(nil); got != (Dist{}) {
		t.Errorf("空输入应返回零值，实际 %+v", got)
	}
	got := SummarizeDist([]int{5, 1, 3, 2, 4})
	if got.Min != 1 || got.Max != 5 {
		t.Errorf("min/max = %d/%d, want 1/5", got.Min, got.Max)
	}
	// 最近秩：len=5 → p50 取下标 int(5*0.5)=2（已排序后是 3），p90 取下标 4（=5）。
	if got.P50 != 3 || got.P90 != 5 {
		t.Errorf("p50/p90 = %d/%d, want 3/5", got.P50, got.P90)
	}
	if math.Abs(got.Mean-3.0) > 1e-9 {
		t.Errorf("mean = %v, want 3", got.Mean)
	}

	// 输入不得被就地排序（调用方还要用原顺序）。
	in := []int{3, 1, 2}
	_ = SummarizeDist(in)
	if in[0] != 3 {
		t.Errorf("SummarizeDist 不该修改入参，实际 %v", in)
	}
}

// TestEvaluatePerUserIsDeterministic 守住可复现性：
// 用户集合用 map 收集，不排序的话两次运行的浮点求和顺序不同，
// 落盘的 JSON 就会逐字节不一样。
func TestEvaluatePerUserIsDeterministic(t *testing.T) {
	snap := &Snapshot{SplitAt: day(10)}
	for i := 1; i <= 20; i++ {
		snap.Videos = append(snap.Videos, pv(uint(i), 999, 1, int64(i), ""))
	}
	for account := uint(100); account < 140; account++ {
		for v := 1; v <= 5; v++ {
			snap.Interactions = append(snap.Interactions, Interaction{
				VideoID: uint(v), AccountID: account, Kind: KindLike, At: day(11),
			})
		}
	}
	first, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1, 3})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	second, err := EvaluatePerUser(snap, []Ranker{popRanker()}, []int{1, 3})
	if err != nil {
		t.Fatalf("EvaluatePerUser 失败: %v", err)
	}
	for i := range first.Rankers[0].Results {
		if first.Rankers[0].Results[i] != second.Rankers[0].Results[i] {
			t.Errorf("两次结果不同: %+v vs %+v", first.Rankers[0].Results[i], second.Rankers[0].Results[i])
		}
	}
}

func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
