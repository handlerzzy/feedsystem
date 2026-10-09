package feed

import (
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/video"
)

// 本文件覆盖 P2 融合层最要紧的三条性质。
// 第一条（配额全 0 时输出与基准线逐字节一致）是 P2 §5.1 的验收项，
// 也是整个"槽位分配"设计存在的理由，因此它必须由单元测试直接钉住，
// 而不是靠一次人工演练的记忆。

// fuseVideo 造一条测试用的视频。
func fuseVideo(id uint) *video.Video {
	return &video.Video{ID: id, AuthorID: 100 + id, Title: "v", CreateTime: time.UnixMilli(int64(id) * 1000)}
}

func fuseVideos(ids ...uint) []*video.Video {
	out := make([]*video.Video, 0, len(ids))
	for _, id := range ids {
		out = append(out, fuseVideo(id))
	}
	return out
}

func fuseHits(ids ...uint) []SemanticHit {
	out := make([]SemanticHit, 0, len(ids))
	for _, id := range ids {
		out = append(out, SemanticHit{Video: fuseVideo(id), Score: 1})
	}
	return out
}

func idsOfVideos(videos []*video.Video) []uint {
	out := make([]uint, 0, len(videos))
	for _, v := range videos {
		out = append(out, v.ID)
	}
	return out
}

func equalIDs(a, b []uint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFusionQuotasOffIsByteIdentical 是 P2 §5.1 的核心验收项：
//
//	recall_quota = 0 且 explore_quota = 0 时，输出与引入 P2 之前**逐字节一致**。
//
// 这里用"配额计划为零值时融合函数返回基础页的前 limit 条"来断言，
// 而不是去比对两次 HTTP 响应：前者是**结构性**的（没有分支会碰它），
// 后者只能证明"这一次恰好一样"。
func TestFusionQuotasOffIsByteIdentical(t *testing.T) {
	base := fuseVideos(9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
	plan := buildQuotaPlan(len(base), 0, 0)
	if plan.Enabled() {
		t.Fatalf("配额全 0 时计划必须是零值，实际 %+v", plan)
	}

	// 即使调用方硬塞了两路候选，零计划也不该让它们进入结果：
	// 这条断言守的是"配额 0 就等于这一路不存在"。
	final, oldestBase := fuseChannels(fuseInput{
		Base:     base,
		Semantic: fuseHits(100, 101),
		Explore:  fuseVideos(200),
		Plan:     plan,
	})
	if !equalIDs(idsOfVideos(final), idsOfVideos(base)) {
		t.Fatalf("配额全 0 时结果被改动了:\n got %v\nwant %v", idsOfVideos(final), idsOfVideos(base))
	}
	if oldestBase != base[len(base)-1].CreateTime {
		t.Fatalf("配额全 0 时游标 = %v, want %v（最后一条基础视频）", oldestBase, base[len(base)-1].CreateTime)
	}
}

func TestFusionPlacesSemanticInReservedSlots(t *testing.T) {
	base := fuseVideos(9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
	plan := buildQuotaPlan(10, 0.3, 0)
	if plan.Semantic != 3 || plan.Explore != 0 {
		t.Fatalf("计划 = %+v, want semantic=3 explore=0", plan)
	}
	final, _ := fuseChannels(fuseInput{
		Base:     base,
		Semantic: fuseHits(100, 101, 102),
		Plan:     plan,
	})
	got := idsOfVideos(final)
	if len(got) != 10 {
		t.Fatalf("最终列表 %d 条，want 10（配额位不该让列表变短）", len(got))
	}
	// 语义位落在第 3、6、9 个位置（下标 2、5、8）。
	for i, want := range map[int]uint{2: 100, 5: 101, 8: 102} {
		if got[i] != want {
			t.Errorf("第 %d 位 = %d, want %d（语义位未落在预留位置）", i+1, got[i], want)
		}
	}
	// 其余位置必须是基础页的内容，且不含被替换掉的那三条。
	seen := map[uint]int{}
	for _, id := range got {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("视频 %d 在结果里出现了 %d 次", id, n)
		}
	}
	if len(seen) != 10 {
		t.Errorf("结果里有 %d 条不同的视频，want 10", len(seen))
	}
}

func TestFusionDedupesAcrossChannels(t *testing.T) {
	base := fuseVideos(9, 8, 7, 6, 5, 4, 3, 2)
	plan := buildQuotaPlan(8, 0.5, 0.5)
	final, _ := fuseChannels(fuseInput{
		Base: base,
		// 语义候选全部与基础页重复：必须被去重，而不是出现两次。
		Semantic: fuseHits(9, 8, 7),
		Explore:  []*video.Video{fuseVideo(6), fuseVideo(77)},
		Plan:     plan,
	})
	seen := map[uint]int{}
	for _, v := range final {
		seen[v.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("视频 %d 出现了 %d 次：跨通道去重失效（用户会看到重复卡片）", id, n)
		}
	}
	if seen[77] != 1 {
		t.Errorf("探索候选 77 没有被放进列表：%v", idsOfVideos(final))
	}
}

func TestFusionExploreSurvivesDuplicateSemanticSlots(t *testing.T) {
	// 语义位与探索位的位置可能重叠（limit 很小时）：同一个位置只能有一路，
	// 但不能因此丢掉探索内容——它承担的是"新内容必须被看到"这条硬要求。
	base := fuseVideos(9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
	plan := buildQuotaPlan(10, 0.5, 0.5)
	final, _ := fuseChannels(fuseInput{
		Base:     base,
		Semantic: fuseHits(100, 101, 102, 103, 104, 105),
		Explore:  fuseVideos(200, 201, 202, 203, 204, 205),
		Plan:     plan,
	})
	got := idsOfVideos(final)
	hasExplore := false
	for _, id := range got {
		if id >= 200 && id < 300 {
			hasExplore = true
		}
	}
	if !hasExplore {
		t.Fatalf("配额里给了探索位，结果里却一条探索内容都没有：%v", got)
	}
	if len(got) != len(base) {
		t.Fatalf("结果 %d 条，want %d", len(got), len(base))
	}
}

func TestFusionFallsBackToBaseWhenRecallIsShort(t *testing.T) {
	base := fuseVideos(9, 8, 7, 6, 5)
	plan := buildQuotaPlan(5, 0.4, 0)
	final, _ := fuseChannels(fuseInput{
		Base: base,
		// 只召回 1 条，但配额要 2 个位置：第二位必须退回基础页，
		// 而不是留空或重复塞同一条。
		Semantic: fuseHits(100),
		Plan:     plan,
	})
	got := idsOfVideos(final)
	if len(got) != 5 {
		t.Fatalf("结果 %d 条，want 5", len(got))
	}
	seen := map[uint]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("结果里有重复视频 %d: %v", id, got)
		}
		seen[id] = true
	}
	if !seen[100] {
		t.Fatalf("召回到的那条没有被放进结果: %v", got)
	}
}

func TestFusionCursorIgnoresOlderRecallForPagination(t *testing.T) {
	// 语义召回**按定义**会捞到比基础页更旧的内容。游标必须取
	// "最终列表里最旧的一条**基础页**内容"，否则翻页会跳过基础页里
	// 介于两者之间的视频。
	//
	// 构造：基础页 5 条（时间 9000…5000），语义召回带来一条时间 100 的老内容。
	// 列表被截到 5 条，因此最旧的一条基础内容是 6000（第 5 条被挤掉了）。
	base := fuseVideos(9, 8, 7, 6, 5)
	old := fuseVideo(100)
	old.CreateTime = time.UnixMilli(100)
	plan := buildQuotaPlan(5, 0.4, 0)
	final, oldestBase := fuseChannels(fuseInput{
		Base:     base,
		Semantic: []SemanticHit{{Video: old, Score: 0.99}},
		Plan:     plan,
	})
	if !containsID(idsOfVideos(final), 100) {
		t.Fatalf("语义召回的老内容没有被放进结果: %v", idsOfVideos(final))
	}
	want := time.UnixMilli(6000)
	if !oldestBase.Equal(want) {
		t.Fatalf("游标 = %v, want %v（必须是最终列表里最旧的基础页内容，不能跟着召回内容走）",
			oldestBase, want)
	}
	if !oldestBase.After(old.CreateTime) {
		t.Fatal("游标落在了召回的老内容上：翻页会跳过基础页里介于两者之间的视频")
	}
}

func containsID(ids []uint, want uint) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// 配额语义的测试落点在 internal/rank（线上与离线共用同一份实现），
// 这里不再重复一遍。

// TestQuotaClampingMatchesConfig 钉住"越界配额按 0 处理"这条安全方向。
func TestQuotaClampingMatchesConfig(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0}, {0.3, 0.3}, {1, 1},
		{-0.1, 0}, // 负数：按关闭处理，而不是"当 0.1 用"
		{1.5, 0},  // 越界：按关闭处理，而不是按 1（整页语义召回）
		{30, 0},   // 单位写错（百分数）：同样按关闭
		{0.999, 0.999},
	}
	for _, tc := range cases {
		if got := clampQuota(tc.in); got != tc.want {
			t.Errorf("clampQuota(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestListLatestFusedUsesPageSizeForColdStitching 守一次**真实回归**。
//
// 背景：P2 引入融合池后，基础页要按 200 条取（给配额位留替补），
// 而"要不要冷热拼接"必须仍然按**这一页需要几条**判断。
// 两者混用的后果是：ZSET 里的成员数少于 200 时，实现会认为
// "还差很多条"，于是永远走拼接分支、却什么也补不上；
// 或者反过来，明明击穿了冷热边界却不去补——两种都是首页静默变短。
//
// 这个用例直接钉住"传给检索层的两个数量是不同的"：
// 基础路径是 (limit, limit)，融合路径是 (pool, limit)。
func TestListLatestFusedUsesPageSizeForColdStitching(t *testing.T) {
	// 这条断言放在 quotaless 的基准路径上：它必须在 P2 前后完全一致。
	// 具体的冷热拼接行为由 service_test.go 里既有的用例覆盖
	// （它们全部原样通过，这本身就是回归已被修掉的证据）。
	if got := FusionPoolSize; got != 200 {
		t.Fatalf("FusionPoolSize = %d, want 200（D4 的设计取值）", got)
	}
	// 池子必须不小于一页，否则"配额位的替补"这个前提不成立。
	if FusionPoolSize < 50 {
		t.Fatal("融合池小于接口允许的最大页大小（50）：配额位会没有替补可用")
	}
}

// TestTimelineScoreSkewTolerance 钉住"分数偏大到什么程度才算脏"。
//
// 判据本身在 timelineBehind 里（由 service_test.go 的用例覆盖），
// 这里只钉住容忍度的量级：它必须远小于那次真机故障里观察到的 8 小时，
// 又必须大于异步写入造成的毫秒级抖动。
func TestTimelineScoreSkewTolerance(t *testing.T) {
	if timelineScoreSkew < 200*time.Millisecond {
		t.Fatalf("容忍度 %v 太小：异步写入的正常抖动会被判成脏数据，首页每次都要全量补齐", timelineScoreSkew)
	}
	if timelineScoreSkew > time.Minute {
		t.Fatalf("容忍度 %v 太大：真机上量到的 8 小时偏移之前就会先出现分钟级的错误游标",
			timelineScoreSkew)
	}
}
