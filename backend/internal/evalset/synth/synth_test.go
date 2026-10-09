package synth

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/evalset"
)

// 本文件是 B2 的验收测试。
//
// 核心断言只有一条：**生成的数据里存在可被语义利用的结构**，
// 也就是"全局热度排序明显不够好，而只看字面词的探针明显更好"。
// 少了这一条，P2 做完得到的结论只会是"我接了模型"——
// 不是模型没用，是数据里没有问题可问。

// fixedOptions 返回一份"生成时间也固定"的参数。
//
// 固定 GeneratedAt 是"逐字节可复现"的前提：它会被写进快照，
// 用 time.Now() 的话同一 seed 两次运行的文件必然不同。
func fixedOptions() Options {
	o := DefaultOptions()
	o.GeneratedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return o
}

func mustGenerate(t *testing.T, o Options) (*evalset.Snapshot, *Stats) {
	t.Helper()
	snap, stats, err := Generate(o)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	return snap, stats
}

// TestGenerateIsByteIdenticalForSameSeed 守住"固定 seed 产出逐字节一致的快照"。
//
// 没有这条性质，"半年后重跑核对基线"就不成立，而基线数字的全部价值
// 恰恰在于可复现。它同时能抓到两类常见缺陷：map 遍历顺序泄漏进输出、
// 以及误用了 time.Now()。
func TestGenerateIsByteIdenticalForSameSeed(t *testing.T) {
	opts := fixedOptions()
	opts.Videos = 120
	opts.Users = 12

	dir := t.TempDir()
	var first []byte
	for i := 0; i < 2; i++ {
		snap, _ := mustGenerate(t, opts)
		path := filepath.Join(dir, "snap.json")
		if err := snap.Save(path); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if i == 0 {
			first = b
			continue
		}
		if !bytes.Equal(first, b) {
			t.Fatal("同一组参数两次生成的快照不是逐字节一致的")
		}
	}
}

// TestGenerateDiffersForDifferentSeed 反向守一遍：
// 换 seed 必须真的换数据，否则"固定 seed"就成了摆设。
func TestGenerateDiffersForDifferentSeed(t *testing.T) {
	a := fixedOptions()
	a.Videos, a.Users = 120, 12
	b := a
	b.Seed = a.Seed + 1

	sa, _ := mustGenerate(t, a)
	sb, _ := mustGenerate(t, b)
	ba, _ := json.Marshal(sa)
	bb, _ := json.Marshal(sb)
	if bytes.Equal(ba, bb) {
		t.Fatal("换了 seed 却生成了一模一样的数据")
	}
}

// TestGeneratedScaleMeetsPrerequisites 守住 P2 前置条件 §2 的规模底线。
func TestGeneratedScaleMeetsPrerequisites(t *testing.T) {
	snap, stats := mustGenerate(t, fixedOptions())

	if stats.Videos < 500 {
		t.Errorf("视频数 = %d，前置条件要求 ≥ 500", stats.Videos)
	}
	if stats.Users < 50 {
		t.Errorf("用户数 = %d，前置条件要求 ≥ 50", stats.Users)
	}
	if stats.Candidates < 50 {
		t.Errorf("候选集 = %d，至少要能支撑 Recall@K", stats.Candidates)
	}
	// 每用户相关样本数：取最小值而不是均值，均值会被少数用户掩盖。
	if got := stats.LabelsPerUser[0]; got < 20 {
		t.Errorf("每用户相关样本数最少只有 %d，前置条件要求 ≥ 20", got)
	}

	for _, v := range snap.Videos {
		if strings.TrimSpace(v.Title) == "" {
			t.Fatalf("vid=%d 没有标题：没有文本就没有语义可评估（B2 要修的就是这个）", v.ID)
		}
		if strings.TrimSpace(v.Description) == "" {
			t.Fatalf("vid=%d 没有描述", v.ID)
		}
	}

	// 互动必须带 account_id，否则按用户口径无从谈起。
	for _, it := range snap.Interactions {
		if it.AccountID == 0 {
			t.Fatalf("存在没有 account_id 的互动：vid=%d at=%s", it.VideoID, it.At)
		}
	}
}

// TestGeneratedDataIsNotDegenerate 是 B2 的**核心验收项**。
//
// 三个断言缺一不可：
//  1. 按用户口径下，popularity 的 NDCG 明显小于 1 —— 数据不是"热度即答案"；
//  2. 只看标题/描述字面的词法探针明显更好 —— 文本里确实编码了用户偏好，
//     也就是说 P2 的 embedding 召回**有可能**有效；
//  3. 按用户口径的样本量达标（reliable=true）—— 结论不是噪声。
//
// 阈值写成 0.5 而不是"小于 1"：一个恰好 0.98 的数字在字面上满足
// "小于 1"，但它说明数据仍然是退化的。差多少才算"明显"必须给出具体数，
// 否则这条验收项等于没有（前置条件文件自己的要求）。
func TestGeneratedDataIsNotDegenerate(t *testing.T) {
	snap, _ := mustGenerate(t, fixedOptions())
	ks := []int{20}
	rankers := append(evalset.BuiltinRankers(), evalset.LexicalProbeRanker(snap))

	res, err := evalset.Evaluate(snap, rankers, ks)
	if err != nil {
		t.Fatalf("Evaluate 失败: %v", err)
	}
	if res.PerUser == nil {
		t.Fatal("合成数据必须有用户维度")
	}
	if !res.PerUser.Reliable {
		t.Fatalf("合成数据的规模必须足以支撑结论，实际 warnings=%v", res.PerUser.Warnings)
	}

	ndcg := map[string]float64{}
	for _, r := range res.PerUser.Rankers {
		ndcg[r.Name] = r.Results[0].NDCG
	}
	pop, ok := ndcg["popularity"]
	if !ok {
		t.Fatal("按用户结果里缺少 popularity 策略")
	}
	probe, ok := ndcg["lexical_probe"]
	if !ok {
		t.Fatal("按用户结果里缺少 lexical_probe 策略")
	}

	if pop >= 0.5 {
		t.Errorf("popularity 的按用户 NDCG@20 = %.4f，必须明显小于 1（阈值 0.5）：数据仍然被热度主导", pop)
	}
	if probe <= pop*1.5 {
		t.Errorf("词法探针 NDCG@20 = %.4f 相对 popularity %.4f 的优势不足：文本里没有可利用的语义结构", probe, pop)
	}
	t.Logf("按用户 NDCG@20：popularity=%.4f lexical_probe=%.4f（提升 %.1f 倍）",
		pop, probe, probe/pop)
}

// TestInteractionsFollowUserPreference 直接验证"互动由偏好驱动"。
//
// TestGeneratedDataIsNotDegenerate 是聚合证据，这条是**机制证据**：
// 如果每个用户被互动的视频并不集中在少数话题上，那么"词法探针更好"
// 可能是别的原因（比如模板词），P2 做完仍然没有可解释的语义信号。
//
// 判定方式：从标题前缀还原话题，统计每个用户被标记的视频里
// 落在其最热门 3 个话题上的比例，要求平均 ≥ 0.7。
// 随机基线的期望是 3/20 = 0.15，差距非常明显。
func TestInteractionsFollowUserPreference(t *testing.T) {
	snap, _ := mustGenerate(t, fixedOptions())

	topicOfVideo := make(map[uint]string, len(snap.Videos))
	for _, v := range snap.Videos {
		name := topicNameFromTitle(v.Title)
		if name == "" {
			t.Fatalf("vid=%d 的标题 %q 无法还原话题：模板与话题表脱节了", v.ID, v.Title)
		}
		topicOfVideo[v.ID] = name
	}

	perUser := make(map[uint][]string)
	for _, it := range snap.Interactions {
		if !it.At.After(snap.SplitAt) {
			continue
		}
		perUser[it.AccountID] = append(perUser[it.AccountID], topicOfVideo[it.VideoID])
	}
	if len(perUser) == 0 {
		t.Fatal("没有任何观测窗口内的互动")
	}

	total := 0.0
	for _, topics := range perUser {
		counts := make(map[string]int)
		for _, tp := range topics {
			counts[tp]++
		}
		hit := 0
		for _, c := range topN(counts, 3) {
			hit += c
		}
		total += float64(hit) / float64(len(topics))
	}
	avg := total / float64(len(perUser))
	if avg < 0.7 {
		t.Errorf("用户被互动视频的话题集中度平均只有 %.2f（随机基线约 0.15），互动看起来不是偏好驱动的", avg)
	}
	t.Logf("话题集中度平均 %.2f（随机基线约 %.2f）", avg, 3.0/float64(len(topicTable)))
}

// TestAsOfKeepsFeaturesFreeOfFutureInteractions 守住"排序特征不含未来信息"。
//
// 合成器自己算特征，因此它同样可能犯"用导出时刻的值当特征"的错——
// 而那种错只会让基线数字虚高，在报告里完全看不出来。
func TestAsOfKeepsFeaturesFreeOfFutureInteractions(t *testing.T) {
	snap, _ := mustGenerate(t, fixedOptions())

	// 手工统计每个视频在切分点之前的点赞数，与快照里的 likes_count 对照。
	preLikes := make(map[uint]int64)
	for _, it := range snap.Interactions {
		if it.Kind != evalset.KindLike || it.At.After(snap.SplitAt) {
			continue
		}
		preLikes[it.VideoID]++
	}
	for _, v := range snap.Videos {
		if v.LikesCount != preLikes[v.ID] {
			t.Fatalf("vid=%d 的 likes_count=%d，切分点之前的点赞数是 %d：特征里混进了未来互动",
				v.ID, v.LikesCount, preLikes[v.ID])
		}
	}
}

// TestValidateRejectsBadOptions 覆盖参数校验。
func TestValidateRejectsBadOptions(t *testing.T) {
	base := fixedOptions()
	cases := map[string]func(*Options){
		"视频太少":    func(o *Options) { o.Videos = 3 },
		"用户为零":    func(o *Options) { o.Users = 0 },
		"话题越界":    func(o *Options) { o.Topics = len(topicTable) + 1 },
		"切分比例越界":  func(o *Options) { o.SplitRatio = 1 },
		"天数太少":    func(o *Options) { o.Days = 1 },
		"起始时间为零值": func(o *Options) { o.Start = time.Time{} },
		"生成时间为零值": func(o *Options) { o.GeneratedAt = time.Time{} },
	}
	for name, mutate := range cases {
		o := base
		mutate(&o)
		if _, _, err := Generate(o); err == nil {
			t.Errorf("%s 时必须报错而不是静默生成一份无意义的数据", name)
		}
	}
}

// TestWeightedSampleWithoutReplacement 守住抽样的两条性质：
// 不重复、且只取决于种子（与遍历顺序无关）。
func TestWeightedSampleWithoutReplacement(t *testing.T) {
	weights := []float64{1, 2, 3, 4, 5, 6, 0, 0}
	s1 := weightedSampleWithoutReplacement(rand.New(rand.NewSource(7)), weights, 4)
	s2 := weightedSampleWithoutReplacement(rand.New(rand.NewSource(7)), weights, 4)
	if len(s1) != 4 {
		t.Fatalf("抽到的数量 = %d, want 4", len(s1))
	}
	seen := make(map[int]bool, len(s1))
	for _, idx := range s1 {
		if seen[idx] {
			t.Fatalf("抽到了重复下标 %d：%v", idx, s1)
		}
		seen[idx] = true
		if weights[idx] <= 0 {
			t.Errorf("抽到了权重为 0 的下标 %d", idx)
		}
	}
	if len(s2) != 4 || s1[0] != s2[0] || s1[1] != s2[1] || s1[2] != s2[2] || s1[3] != s2[3] {
		t.Errorf("同一种子两次抽样结果不同: %v vs %v", s1, s2)
	}
	// 要求数量超过可用权重时，返回全部可用项而不是 panic。
	if got := weightedSampleWithoutReplacement(rand.New(rand.NewSource(1)), weights, 99); len(got) != 6 {
		t.Errorf("应返回全部 6 个正权重项，实际 %d", len(got))
	}
	if got := weightedSampleWithoutReplacement(rand.New(rand.NewSource(1)), nil, 3); got != nil {
		t.Errorf("空权重应返回 nil，实际 %v", got)
	}
}

// TestSnapshotRoundTripKeepsText 守住"快照能被读回来"。
//
// 标题与描述是 B2 新增的字段，如果 JSON tag 写错（或加了 omitempty
// 却忘了它在导出侧的语义），读回来会变空——而下游只会看到
// "所有视频都没有文本"，排查方向完全被带偏。
func TestSnapshotRoundTripKeepsText(t *testing.T) {
	snap, _ := mustGenerate(t, fixedOptions())
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if err := snap.Save(path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	back, err := evalset.Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(back.Videos) != len(snap.Videos) {
		t.Fatalf("视频数 %d != %d", len(back.Videos), len(snap.Videos))
	}
	for i := range back.Videos {
		if back.Videos[i].Title != snap.Videos[i].Title {
			t.Fatalf("vid=%d 的标题读回来变了: %q vs %q", i+1, back.Videos[i].Title, snap.Videos[i].Title)
		}
	}
	for i := range back.Interactions {
		if back.Interactions[i].AccountID != snap.Interactions[i].AccountID {
			t.Fatalf("第 %d 条互动的 account_id 丢了", i)
		}
	}
}

// topicNameFromTitle 从标题里还原话题名。
//
// 三种句式都包含完整话题名（见 topics.go 的 title()），因此用 Contains。
// 这里刻意不做模糊匹配——还原不出来就说明模板与话题表脱节了，
// 那本身就该让测试失败，而不是被一个宽松的匹配掩盖过去。
func topicNameFromTitle(title string) string {
	for _, tp := range topicTable {
		if strings.Contains(title, tp.Name) {
			return tp.Name
		}
	}
	return ""
}

// topN 返回计数里最大的 n 个值（降序）。n 很小时用选择排序足够。
func topN(counts map[string]int, n int) []int {
	vals := make([]int, 0, len(counts))
	for _, c := range counts {
		vals = append(vals, c)
	}
	if n > len(vals) {
		n = len(vals)
	}
	for i := 0; i < n; i++ {
		maxIdx := i
		for j := i + 1; j < len(vals); j++ {
			if vals[j] > vals[maxIdx] {
				maxIdx = j
			}
		}
		vals[i], vals[maxIdx] = vals[maxIdx], vals[i]
	}
	return vals[:n]
}
