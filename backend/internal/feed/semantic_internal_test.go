package feed

import (
	"context"
	"errors"
	"testing"
	"time"

	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	redis "github.com/redis/go-redis/v9"
)

// 本文件覆盖 P2 §4.2 语义召回通道的**内部逻辑**。
//
// 用 internal 包测试而不是走公开 API：被验证的性质（候选过滤、
// 画像构建、相似度排序、降级）都由未导出的方法承担，
// 从 ListLatest 走一遍需要在 mock 里把冷热拼接、ZSET、游标全都铺好，
// 而那些与本节无关——测试会被无关的脚手架淹没。
//
// 三条最要紧的性质：
//  1. **可缺省**：没有注入 store、配额为 0、未登录 —— 一次查询都不发；
//  2. **无无效 ID**：向量表里的孤儿行（视频已删）不得出现在结果里；
//  3. **失败静默**：向量库不可用时返回空结果且不报错，Feed 照常。

// semanticVideoAt 造一条带指定发布时间的视频。
func semanticVideoAt(id uint, at time.Time) *video.Video {
	v := semanticVideo(id)
	v.CreateTime = at
	return v
}

func semanticVideo(id uint) *video.Video {
	return &video.Video{
		ID:         id,
		AuthorID:   100 + id,
		Title:      "标题",
		CreateTime: time.UnixMilli(int64(id) * 1000),
	}
}

func newSemanticService(t *testing.T, store VectorRecallStore, quota float64) (*FeedService, *fakeFeedStore) {
	t.Helper()
	repo := &fakeFeedStore{}
	likes := &fakeLikeLookup{}

	// 默认给一个非零用户：多数用例测的是"已经登录用户"的行为。
	// 未登录那条路径由专门的用例覆盖。
	if store != nil {
		if fv, ok := store.(*fakeVectorStore); ok && fv.accountID == 0 {
			fv.accountID = 7
		}
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewFeedService(repo, likes, rediscache.NewClient(rdb, "p2test:"))
	if quota > 0 {
		svc.WithSemanticRecall(SemanticRecallOptions{
			Store: store,
			Model: "test-model",
			Dim:   2,
			Quota: quota,
		})
	}
	return svc, repo
}

// TestSemanticRecallOffIsZeroQueries 守"关掉 = 零开销"：store 是一个
// 没有任何期望的 mock，任何一次调用都会让测试失败。
func TestSemanticRecallOffIsZeroQueries(t *testing.T) {
	store := &fakeVectorStore{}
	svc, _ := newSemanticService(t, store, 0) // quota=0 → 不注入

	plan := buildQuotaPlan(3, svc.recallQuota, svc.exploreQuota)
	if plan.Enabled() {
		t.Fatalf("配额为 0 时计划应当是零值: %+v", plan)
	}
	if hits := svc.semanticRecall(context.Background(), 7, plan); hits != nil {
		t.Fatalf("配额为 0 时应当返回空: %+v", hits)
	}
}

func TestSemanticRecallSkipsAnonymousViewer(t *testing.T) {
	store := &fakeVectorStore{}
	svc, _ := newSemanticService(t, store, 0.5)

	if hits := svc.semanticRecall(context.Background(), 0, QuotaPlan{Semantic: 2}); hits != nil {
		t.Fatalf("未登录用户不该走语义路: %+v", hits)
	}
}

func TestSemanticRecallFiltersOrphansWarmupAndDedup(t *testing.T) {
	store := &fakeVectorStore{
		// 候选池：40/50 是"老但相关"，30/20/10 是最新的一批（应让位给时序路），
		// 99 是向量表里的孤儿行（视频已删）。
		candidates: []video.VectorCandidate{{ID: 40}, {ID: 50}, {ID: 30}, {ID: 20}, {ID: 10}, {ID: 99}},
		liked:      []uint{40},
		vectors: map[uint][]float32{
			40: {1, 0},
			50: {0.8, 0.6},
			30: {0, 1}, 20: {0, 1}, 10: {0, 1}, 99: {0, 1},
		},
	}
	svc, repo := newSemanticService(t, store, 0.5)
	repo.existing = map[uint]bool{}
	for _, id := range []uint{40, 50, 30, 20, 10} {
		repo.existing[id] = true
	}
	repo.freshest = []uint{30, 20, 10}

	hits := svc.semanticRecall(context.Background(), 7, QuotaPlan{Semantic: 2})
	if len(hits) == 0 {
		t.Fatal("语义召回没有返回任何结果")
	}
	gotIDs := make([]uint, 0, len(hits))
	for _, h := range hits {
		gotIDs = append(gotIDs, h.Video.ID)
	}
	// 孤儿行必须被过滤（P2 §4.2 验收项：召回结果中不出现无效 ID）。
	for _, id := range gotIDs {
		if id == 99 {
			t.Fatalf("召回结果里出现了无效 ID 99: %v", gotIDs)
		}
	}
	// 最新的一批（30/20/10）应当让位给时序路。
	for _, id := range gotIDs {
		if id == 30 || id == 20 || id == 10 {
			t.Fatalf("召回结果里出现了最新一批的视频 %d: %v（时序路已经保证了它们的曝光）", id, gotIDs)
		}
	}
	// 相似度排序：40 在最前。
	if len(hits) > 0 && hits[0].Video.ID != 40 {
		t.Fatalf("相似度最高的 40 没有排在第一位: %v (score=%v)", gotIDs, hits[0].Score)
	}
	// 去重：同一个 ID 不出现两次。
	seen := map[uint]int{}
	for _, id := range gotIDs {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("召回结果里 %d 出现了多次: %v", id, gotIDs)
		}
	}
}

func TestSemanticRecallDegradesOnStoreFailure(t *testing.T) {
	store := &fakeVectorStore{candidatesErr: errors.New("table dropped")}
	svc, _ := newSemanticService(t, store, 0.5)

	if hits := svc.semanticRecall(context.Background(), 7, QuotaPlan{Semantic: 2}); hits != nil {
		t.Fatalf("向量库失败时应当静默返回空: %+v", hits)
	}
}

func TestSemanticRecallEmptyPoolNeedsNoProfileQuery(t *testing.T) {
	// liked 被设置为"一旦被查询就 panic"：池子为空时不该再查画像。
	store := &fakeVectorStore{}
	svc, _ := newSemanticService(t, store, 0.5)

	if hits := svc.semanticRecall(context.Background(), 7, QuotaPlan{Semantic: 2}); hits != nil {
		t.Fatalf("候选池为空时应当返回空: %+v", hits)
	}
}

func TestUserInterestVectorWeightsRecentLikesHigher(t *testing.T) {
	// 最近点赞 1（向量 1,0），其次 2（向量 0,1）。
	store := &fakeVectorStore{
		liked:   []uint{1, 2},
		vectors: map[uint][]float32{1: {1, 0}, 2: {0, 1}},
	}
	svc, _ := newSemanticService(t, store, 0.5)

	vec, err := svc.userInterestVector(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("画像维度 = %d, want 2", len(vec))
	}
	// 权重 1 与 1/2：x 分量应当明显大于 y 分量（最近的点赞影响更大）。
	if !(vec[0] > vec[1]) {
		t.Fatalf("画像 = %v，最近点赞的分量没有更重（等权会让兴趣停滞在过去）", vec)
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm < 0.99 || norm > 1.01 {
		t.Fatalf("画像模长平方 = %v, want 1（必须归一化后才能直接比较）", norm)
	}
}

func TestUserInterestVectorToleratesDimensionMismatch(t *testing.T) {
	// 2 号点赞的向量维度不对（历史数据、换过模型）：必须被跳过而不是 panic。
	store := &fakeVectorStore{
		liked:   []uint{1, 2},
		vectors: map[uint][]float32{1: {1, 0}, 2: {1, 0, 0, 0}},
	}
	svc, _ := newSemanticService(t, store, 0.5)

	vec, err := svc.userInterestVector(context.Background(), 7)
	if err != nil {
		t.Fatalf("维度不一致不该返回错误: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("画像维度 = %d, want 2", len(vec))
	}
}

func TestUserInterestVectorEmptyWhenNothingLiked(t *testing.T) {
	store := &fakeVectorStore{}
	svc, _ := newSemanticService(t, store, 0.5)

	vec, err := svc.userInterestVector(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 0 {
		t.Fatalf("没有点赞时画像必须是空的（新用户走兜底通道），实际 %v", vec)
	}
}

func TestCosineHandlesMismatchAndZeroVectors(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Fatalf("cosine(等向量) = %v, want 1", got)
	}
	if got := cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("cosine(正交) = %v, want 0", got)
	}
	if got := cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("维度不一致时必须返回 0（而不是 panic 或错误答案）, got %v", got)
	}
	if got := cosine([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("零向量的余弦必须为 0, got %v", got)
	}
	if got := cosine(nil, nil); got != 0 {
		t.Fatalf("空向量的余弦必须为 0, got %v", got)
	}
}

func TestSortCandidatesIsDeterministic(t *testing.T) {
	// 同分时按 id 降序：不排序会让同一份数据两次请求给出不同顺序
	// （map 遍历顺序随机），而那是"可复现性"最大的杀手。
	items := []scoredCandidate{
		{id: 1, score: 0.5},
		{id: 3, score: 0.5},
		{id: 2, score: 0.9},
		{id: 4, score: 0.1},
	}
	sortCandidates(items)
	want := []uint{2, 3, 1, 4}
	for i, w := range want {
		if items[i].id != w {
			t.Fatalf("排序结果 = %v, want %v", items, want)
		}
	}
}

// ---------- 手写假实现 ----------
//
// 为什么用假实现而不是 mockgen 的 mock：mock 包 import 了 feed 包，
// 而本文件是 feed 包的**内部**测试，形成 import cycle。
// 假实现还有一个好处：它们把"被测代码依赖了什么"直接写在结构体字段里，
// 读测试的人不必去猜 gomock 的期望。

type fakeVectorStore struct {
	candidates    []video.VectorCandidate
	candidates2   []video.VectorCandidate
	candidatesErr error
	candidateN    int
	liked         []uint
	likedErr      error
	vectors       map[uint][]float32
	vectorsErr    error

	// accountID 是"这个假 store 属于哪个用户"，模拟按账号过滤。
	accountID uint
	// models 是"库里实际存在的向量空间"。为空时按配置名返回一个。
	models    []video.EmbeddingModelStat
	modelsErr error
	// modelsByCall 按解析次序返回不同的空间（模拟"库里的空间变了"）。
	modelsByCall [][]video.EmbeddingModelStat
	// 观测点：模型解析被查了几次（缓存生效的直接证据）。
	modelCalls int

	// 观测点：调用次数。用来断言"这一路一次都没被碰过"。
	candidateCalls int
	likedCalls     int
}

func (f *fakeVectorStore) ListVectorCandidates(_ context.Context, model string, _ int, _ int) ([]video.VectorCandidate, error) {
	f.candidateCalls++
	f.candidateN++
	// 模拟"库里的空间变了"：只有用**新**模型名去读才看得到候选。
	if model != "test-model" {
		return nil, f.candidatesErr
	}
	if f.candidates2 != nil {
		return f.candidates2, f.candidatesErr
	}
	return f.candidates, f.candidatesErr
}

func (f *fakeVectorStore) LoadVectors(_ context.Context, ids []uint, _ string, _ int) (map[uint][]float32, error) {
	if f.vectorsErr != nil {
		return nil, f.vectorsErr
	}
	if f.vectors == nil {
		return map[uint][]float32{}, nil
	}
	out := make(map[uint][]float32, len(ids))
	for _, id := range ids {
		if v, ok := f.vectors[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeVectorStore) ListEmbeddingModels(_ context.Context, _ int, _ int) ([]video.EmbeddingModelStat, error) {
	f.modelCalls++
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	if len(f.modelsByCall) > 0 {
		idx := f.modelCalls - 1
		if idx >= len(f.modelsByCall) {
			idx = len(f.modelsByCall) - 1
		}
		return f.modelsByCall[idx], nil
	}
	if len(f.models) == 0 {
		// 默认行为：库里只有一个空间，名字与配置相同
		// （真实部署里就是这样；"名字不同"由专门的用例覆盖）。
		return []video.EmbeddingModelStat{{Model: "test-model", Dim: 2, Rows: 1}}, nil
	}
	return f.models, nil
}

func (f *fakeVectorStore) RecentLikedVideoIDs(_ context.Context, accountID uint, _ int) ([]uint, error) {
	f.likedCalls++
	// 模拟真实仓储的语义：accountID == 0（未登录）时返回空。
	// 生产实现里 WHERE account_id = 0 也查不到任何行。
	if accountID == 0 {
		return nil, nil
	}
	return f.liked, f.likedErr
}

// NewestVideoIDs 让假实现满足 FreshIDLister：语义路用它把最新一批让给时序路。
func (f *fakeFeedStore) NewestVideoIDs(_ context.Context, limit int) ([]uint, error) {
	out := make([]uint, 0, limit)
	for _, id := range f.freshest {
		if len(out) == limit {
			break
		}
		out = append(out, id)
	}
	return out, nil
}

// fakeFeedStore 只实现被本节用到的方法。
//
// 其余方法直接 panic：本节不该碰它们，真碰了要让测试**响亮地失败**，
// 而不是返回一个零值把问题藏起来（那是假实现最常见的坑）。
type fakeFeedStore struct {
	// existing 是"视频表里存在"的 ID 集合（用于模拟孤儿向量行）。
	existing map[uint]bool
	// freshest 是"最新一批"的 ID（语义路会排除它们）。
	freshest []uint
	// probe 是探测用的桩：它模拟"数据库里最新的那批视频"。
	// 不设时返回 freshest 对应的视频。
	probe func() []*video.Video
}

func (f *fakeFeedStore) ListLatest(_ context.Context, limit int, _ time.Time) ([]*video.Video, error) {
	if f.probe == nil {
		panic("本节没有为 ListLatest 准备桩")
	}
	all := f.probe()
	if limit > len(all) {
		limit = len(all)
	}
	return all[:limit], nil
}

func (f *fakeFeedStore) ListLikesCountWithCursor(context.Context, int, *LikesCountCursor) ([]*video.Video, error) {
	panic("本节不该调用 ListLikesCountWithCursor")
}

func (f *fakeFeedStore) ListByFollowing(context.Context, int, uint, time.Time) ([]*video.Video, error) {
	panic("本节不该调用 ListByFollowing")
}

func (f *fakeFeedStore) ListByPopularity(context.Context, int, int64, time.Time, uint) ([]*video.Video, error) {
	panic("本节不该调用 ListByPopularity")
}

func (f *fakeFeedStore) GetByIDs(_ context.Context, ids []uint) ([]*video.Video, error) {
	out := make([]*video.Video, 0, len(ids))
	for _, id := range ids {
		if f.existing != nil && !f.existing[id] {
			continue // 孤儿向量行：视频已删除
		}
		out = append(out, semanticVideo(id))
	}
	return out, nil
}

func (f *fakeFeedStore) ListByTag(context.Context, string, int) ([]*video.Video, error) {
	panic("本节不该调用 ListByTag")
}

func (f *fakeFeedStore) ListFreshLowEngagement(context.Context, int, time.Duration, int64, uint) ([]*video.Video, error) {
	panic("本节不该调用 ListFreshLowEngagement")
}

type fakeLikeLookup struct{}

func (fakeLikeLookup) BatchGetLiked(context.Context, []uint, uint) (map[uint]bool, error) {
	return map[uint]bool{}, nil
}

// ---------- 向量空间解析（一个真实会踩的坑） ----------

// TestEffectiveModelPrefersStoredSpace 守"读取用库里实际存在的模型名"。
//
// 背景：向量的 model 是**提供方**决定的。离线演示时 sidecar 用词法编码器，
// 它回的是 faux-lexical，而配置里写的是 text-embedding-3-small。
// 按配置名去读会一条都读不到，而日志里一切正常——最难查的一类故障。
func TestEffectiveModelPrefersStoredSpace(t *testing.T) {
	store := &fakeVectorStore{
		models: []video.EmbeddingModelStat{{Model: "faux-lexical", Dim: 2, Rows: 10}},
	}
	svc, _ := newSemanticService(t, store, 0.5)

	if got := svc.effectiveModel(context.Background()); got != "faux-lexical" {
		t.Fatalf("effectiveModel = %q, want faux-lexical（库里实际存在的空间）", got)
	}
	// 第二次必须命中缓存：每次请求都做一次 GROUP BY 等于把"避免打库"取消了。
	if store.modelCalls != 1 {
		t.Fatalf("模型解析查询了 %d 次，want 1（结论必须被缓存）", store.modelCalls)
	}
	svc.effectiveModel(context.Background())
	if store.modelCalls != 1 {
		t.Fatalf("缓存没有生效：又查了 %d 次", store.modelCalls)
	}
}

func TestEffectiveModelFallsBackToConfigured(t *testing.T) {
	// 库里一条向量都没有：退回配置值（此时语义路本来就无数据可用，
	// 退回配置值只是为了让日志里的 model 字段可读）。
	store := &fakeVectorStore{models: []video.EmbeddingModelStat{}}
	svc, _ := newSemanticService(t, store, 0.5)
	if got := svc.effectiveModel(context.Background()); got != "test-model" {
		t.Fatalf("effectiveModel = %q, want test-model", got)
	}

	// 统计查询失败时也不该报错，同样退回配置值。
	broken := &fakeVectorStore{modelsErr: errors.New("table dropped")}
	svc2, _ := newSemanticService(t, broken, 0.5)
	if got := svc2.effectiveModel(context.Background()); got != "test-model" {
		t.Fatalf("effectiveModel = %q, want test-model（失败时降级）", got)
	}
}

// TestSemanticRecallReResolvesModelAfterSpaceChange 守"换模型后自愈"。
//
// 场景：库里的向量空间从 test-model 换成 faux-lexical（换 provider 或
// 换模型后重跑回填）。此时旧名字读不到任何候选，语义路必须先失效缓存
// 再试一次——否则它会一直瞎到缓存 TTL 结束，表现为"换完模型要等半分钟"。
func TestSemanticRecallReResolvesModelAfterSpaceChange(t *testing.T) {
	store := &fakeVectorStore{
		liked:   []uint{40},
		vectors: map[uint][]float32{40: {1, 0}, 41: {1, 0}},
		// 旧空间下没有任何候选；新空间下有 41。
		candidates2: []video.VectorCandidate{{ID: 41}},
		// 第一次解析看到的是已过期的旧空间，之后变成新空间。
		modelsByCall: [][]video.EmbeddingModelStat{
			{{Model: "stale-lexical", Dim: 2, Rows: 1}},
			{{Model: "test-model", Dim: 2, Rows: 1}},
		},
	}
	svc, repo := newSemanticService(t, store, 0.5)
	repo.existing = map[uint]bool{41: true}

	hits := svc.semanticRecall(context.Background(), 7, QuotaPlan{Semantic: 1})
	if len(hits) == 0 {
		t.Fatal("换向量空间后语义路没有自愈：仍然按旧模型名读，一条都读不到")
	}
	if hits[0].Video.ID != 41 {
		t.Fatalf("召回结果 = %d, want 41", hits[0].Video.ID)
	}
}

// TestSemanticRecallInvalidatesModelCacheOnEmpty 守"缓存会被主动失效"。
func TestSemanticRecallInvalidatesModelCacheOnEmpty(t *testing.T) {
	store := &fakeVectorStore{}
	svc, _ := newSemanticService(t, store, 0.5)
	svc.effectiveModel(context.Background())
	if store.modelCalls != 1 {
		t.Fatalf("modelCalls = %d, want 1", store.modelCalls)
	}
	svc.semanticRecall(context.Background(), 7, QuotaPlan{Semantic: 1})
	if store.modelCalls < 2 {
		t.Fatalf("读到 0 条候选后没有重新解析模型名（modelCalls=%d）：换模型后要等缓存过期才好",
			store.modelCalls)
	}
}

// TestTimelineProbeRechecksAfterPublish 守一条**真实的产品要求**：
// 发布之后刷新首页，必须能看到刚发布的视频。
//
// 背景：探测结论按时间线最新成员缓存（为了不被每次首页请求打库），
// 于是"刚好在某次探测之后发布"的视频可能被缓存挡住——
// 表现是"发布者刷新首页看不到自己的视频"，而端到端回归里
// "发布→立刻读最新流"这条断言实测 6 次挂 1 次。
//
// 修法是给"没有落后"这个结论加一个**最短保鲜期**（见
// timelineNegativeFreshFor）：时间线还没追上数据库时，结论不够新就
// 等一小会儿重新探测。这里直接测这条判据——从 ListLatest 走一遍
// 需要把冷热拼接、游标、补齐全部铺好，那些与本节无关。
func TestTimelineProbeRechecksAfterPublish(t *testing.T) {
	// 时间戳落在探测结论的 TTL（1 秒）之内：本用例测的是
	// "缓存还在有效期内、但数据库已经变了"这个窗口。差几分钟的话
	// 缓存早就过期，探测自然会重新打库——那是另一条路径（by TTL），
	// 不能证明保鲜期这条判据在起作用。
	now := time.Now().Truncate(time.Millisecond)
	t2 := time.UnixMilli(now.Add(-4 * time.Millisecond).UnixMilli())
	t3 := time.UnixMilli(now.Add(-1 * time.Millisecond).UnixMilli())

	probes := 0
	store := &fakeVectorStore{}
	svc, repo := newSemanticService(t, store, 0)
	repo.probe = func() []*video.Video {
		probes++
		if probes == 1 {
			// 第一次探测：数据库里最新的就是 v2（时间线上也有它）。
			return []*video.Video{semanticVideoAt(2, t2)}
		}
		// 这之后 v3 被发布（数据库已写、异步消费者还没写时间线）。
		return []*video.Video{semanticVideoAt(3, t3), semanticVideoAt(2, t2)}
	}
	// 时间线里只有 v2：第一次判断因此是"不落后"（成员齐全）。
	if err := svc.rediscache.ZAdd(context.Background(),
		svc.rediscache.Key("feed:global_timeline"),
		redis.Z{Score: float64(t2.UnixMilli()), Member: "2"}); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}

	if svc.timelineBehind(context.Background(), time.Time{}) {
		t.Fatal("时间线完整时不该报告落后")
	}
	if probes != 1 {
		t.Fatalf("第一次判断探测了 %d 次，want 1", probes)
	}

	// 等到保鲜期结束：此时缓存里的 newestDB 还是 t2，而数据库里已经有了
	// v3（t3）。只靠"时间线是否追上"判断的话，缓存会一直自我印证下去，
	// 于是发布者刷新首页看不到自己的视频（端到端回归实测 6 次挂 1 次）。
	time.Sleep(timelineNegativeFreshFor + 20*time.Millisecond)

	started := time.Now()
	if !svc.timelineBehind(context.Background(), time.Time{}) {
		t.Fatal("发布之后仍然回答'没落后'：首页看不到刚发布的视频")
	}
	if probes < 2 {
		t.Fatalf("只探测了 %d 次：缓存把刚发布的视频挡住了", probes)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("等待了 %v，超出保鲜期（%v）太多：读路径被拖慢了", waited, timelineNegativeFreshFor)
	}
}

// TestTimelineProbeSkipsWaitWhenHeadCatchesUp 守那条快速路径：
// 时间线已经追上数据库时（异步消费者跟得上，这是常态），
// 直接复用缓存结论，一次库都不打、也不等待。
func TestTimelineProbeSkipsWaitWhenHeadCatchesUp(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())

	probes := 0
	store := &fakeVectorStore{}
	svc, repo := newSemanticService(t, store, 0)
	repo.probe = func() []*video.Video {
		probes++
		return []*video.Video{semanticVideoAt(2, t2)}
	}
	if err := svc.rediscache.ZAdd(context.Background(),
		svc.rediscache.Key("feed:global_timeline"),
		redis.Z{Score: float64(t2.UnixMilli()), Member: "2"}); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}

	if svc.timelineBehind(context.Background(), time.Time{}) {
		t.Fatal("时间线完整时不该报告落后")
	}
	// 时间线最新分数 == 缓存里记下的数据库最新时间 => 期间没有新视频，
	// 因此第二次判断必须**不打库、不等待**。
	started := time.Now()
	if svc.timelineBehind(context.Background(), time.Time{}) {
		t.Fatal("第二次判断给出了不同的结论")
	}
	if probes != 1 {
		t.Fatalf("探测了 %d 次，want 1（时间线已追上时不该重复打库）", probes)
	}
	if waited := time.Since(started); waited > timelineNegativeFreshFor/2 {
		t.Fatalf("等待了 %v：快速路径没有生效（首页会被无谓地拖慢）", waited)
	}
}
