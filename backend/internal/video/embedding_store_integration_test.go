package video_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/video"

	"gorm.io/gorm"
)

// 本文件用**真实 MySQL** 验证向量表的幂等与失效检测。
//
// 为什么必须用真库（mock 证明不了的三件事）：
//
//  1. **ON DUPLICATE KEY UPDATE 是否真的命中唯一键**。唯一键是
//     (video_id, model, dim) 三列，写错列顺序或漏一列不会有任何编译错误，
//     只会表现为"重复插入出多行"，而检索侧对此完全无感（它按 video_id 取，
//     取到哪一条看运气）。
//  2. **批量插入的 VALUES(col) 语义**。我们一次插入整批，冲突时用的是
//     MySQL 的 VALUES() 函数而不是 Go 侧的字面量；写成 Go 侧字面量会让
//     整批都写成第一行的内容——"看起来成功、数据全错"，且只在冲突路径上出现。
//  3. **LEFT JOIN 的失效检测**。`COALESCE(e.content_hash,'')` 配合
//     LEFT JOIN 才能表达"从来没有向量"，用 INNER JOIN 会把这些视频整个漏掉，
//     表现为"新视频永远拿不到向量"，而日志里一切正常。
//
// 默认跳过；设置 TEST_MYSQL_DSN 后运行（openIntegrationDB 定义在
// like_repo_integration_test.go）：
//
//	TEST_MYSQL_DSN='root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test -count=1 -run EmbeddingIntegration ./internal/video/

const itestEmbedModel = "itest-embed-model"

func itestOpenRepo(t *testing.T) (*gorm.DB, *video.VideoRepository) {
	t.Helper()
	db := openIntegrationDB(t)
	// 表由 AutoMigrate 建（API 进程启动时执行）。集成测试可能跑在
	// 一个还没起过 API 的库上，因此这里显式保证表存在。
	if err := db.AutoMigrate(&video.VideoEmbedding{}); err != nil {
		t.Fatalf("建向量表失败: %v", err)
	}
	return db, video.NewVideoRepository(db)
}

func itestEmbedCleanup(t *testing.T, db *gorm.DB, videoID uint) {
	t.Helper()
	t.Cleanup(func() {
		// video_tags 必须一起清：AttachTags 的并发用例会给这个视频挂标签，
		// 只删视频会留下**孤儿关联行**（video_id 指向不存在的视频）。
		// 它们不会被任何查询命中，因此不会让测试失败，只会一直堆在库里，
		// 让"按 tag 浏览"的统计慢慢变得不可信。
		db.Exec("DELETE FROM video_tags WHERE video_id = ?", videoID)
		db.Exec("DELETE FROM video_embeddings WHERE video_id = ?", videoID)
		db.Exec("DELETE FROM videos WHERE id = ?", videoID)
	})
}

func itestSeedEmbedVideo(t *testing.T, db *gorm.DB, title, description string) *video.Video {
	t.Helper()
	v := &video.Video{
		AuthorID:    900020,
		Username:    "itest-embed",
		Title:       title,
		Description: description,
		PlayURL:     "itest://play",
		CoverURL:    "itest://cover",
	}
	if err := db.Create(v).Error; err != nil {
		t.Fatalf("插入测试视频失败: %v", err)
	}
	itestEmbedCleanup(t, db, v.ID)
	return v
}

func itestVector(t *testing.T, dim int, fill float32) []byte {
	t.Helper()
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = fill + float32(i)/float32(dim)
	}
	b, err := video.EncodeVector(vec)
	if err != nil {
		t.Fatalf("编码向量失败: %v", err)
	}
	return b
}

// TestEmbeddingIntegrationUpsertIsIdempotent 守住"重复写入不产生重复行"。
func TestEmbeddingIntegrationUpsertIsIdempotent(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()
	v := itestSeedEmbedVideo(t, db, "幂等标题", "幂等描述")

	const dim = 8
	for i := 0; i < 3; i++ {
		err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{{
			VideoID:     v.ID,
			Model:       itestEmbedModel,
			Dim:         dim,
			ContentHash: video.ContentHash(v.Title, v.Description),
			Normalized:  true,
			Vector:      itestVector(t, dim, float32(i)),
		}})
		if err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}

	var n int64
	db.Model(&video.VideoEmbedding{}).
		Where("video_id = ? AND model = ? AND dim = ?", v.ID, itestEmbedModel, dim).
		Count(&n)
	if n != 1 {
		t.Fatalf("重复写入产生了 %d 行，want 1（幂等键没有生效）", n)
	}

	// 最后一次写入的内容必须生效（而不是停在第一次）。
	got, err := repo.LoadVectors(ctx, []uint{v.ID}, itestEmbedModel, dim)
	if err != nil {
		t.Fatalf("读取向量失败: %v", err)
	}
	vec := got[v.ID]
	if len(vec) != dim {
		t.Fatalf("读回的维度 = %d, want %d", len(vec), dim)
	}
	// 第 3 次写入的填充值是 2，第一维应当是 2。
	if vec[0] != 2 {
		t.Errorf("向量未被最后一次写入覆盖: vec[0] = %v, want 2", vec[0])
	}
}

// TestEmbeddingIntegrationBatchUpsertKeepsEachRow 守住批量冲突时的 VALUES() 语义。
//
// 这是"看起来成功、数据全错"的典型：整批都写成第一行的内容，
// 而库里行数、维度、时间戳全都正常。
func TestEmbeddingIntegrationBatchUpsertKeepsEachRow(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()

	const dim = 4
	v1 := itestSeedEmbedVideo(t, db, "批量甲", "描述甲")
	v2 := itestSeedEmbedVideo(t, db, "批量乙", "描述乙")

	rows := []video.VideoEmbedding{
		{VideoID: v1.ID, Model: itestEmbedModel, Dim: dim, Normalized: true,
			ContentHash: video.ContentHash(v1.Title, v1.Description), Vector: itestVector(t, dim, 1)},
		{VideoID: v2.ID, Model: itestEmbedModel, Dim: dim, Normalized: true,
			ContentHash: video.ContentHash(v2.Title, v2.Description), Vector: itestVector(t, dim, 9)},
	}
	if err := repo.UpsertEmbeddings(ctx, rows); err != nil {
		t.Fatalf("首次批量写入失败: %v", err)
	}
	// 第二次批量写入走的是冲突分支（每行都要写自己的值）。
	rows[0].Vector = itestVector(t, dim, 3)
	rows[1].Vector = itestVector(t, dim, 7)
	if err := repo.UpsertEmbeddings(ctx, rows); err != nil {
		t.Fatalf("冲突路径批量写入失败: %v", err)
	}

	got, err := repo.LoadVectors(ctx, []uint{v1.ID, v2.ID}, itestEmbedModel, dim)
	if err != nil {
		t.Fatalf("读取向量失败: %v", err)
	}
	if got[v1.ID][0] != 3 {
		t.Errorf("v1 的向量 = %v, want 第一维为 3（批量冲突时被写成了别的行的值？）", got[v1.ID][0])
	}
	if got[v2.ID][0] != 7 {
		t.Errorf("v2 的向量 = %v, want 第一维为 7", got[v2.ID][0])
	}
}

// TestEmbeddingIntegrationModelSwitchIsolates 守住"换模型/换维度必须可检测"。
//
// 不同模型的向量不在同一个空间里，混着读等于让随机数据参与排序。
func TestEmbeddingIntegrationModelSwitchIsolates(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()
	v := itestSeedEmbedVideo(t, db, "换模型", "描述")

	base := video.VideoEmbedding{
		VideoID: v.ID, Model: itestEmbedModel, Dim: 4, Normalized: true,
		ContentHash: video.ContentHash(v.Title, v.Description), Vector: itestVector(t, 4, 1),
	}
	if err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{base}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 换模型：同一 video_id 得到新的一行，旧行仍在（因此"哪些还没重算"可查询）。
	switched := base
	switched.Model = itestEmbedModel + "-v2"
	switched.Vector = itestVector(t, 4, 2)
	if err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{switched}); err != nil {
		t.Fatalf("换模型写入失败: %v", err)
	}
	// 换维度：同样得到新的一行。
	otherDim := base
	otherDim.Dim = 8
	otherDim.Vector = itestVector(t, 8, 3)
	if err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{otherDim}); err != nil {
		t.Fatalf("换维度写入失败: %v", err)
	}

	// 读取时按 (model, dim) 过滤：读到的一律是同一空间里的向量。
	only4, err := repo.LoadVectors(ctx, []uint{v.ID}, itestEmbedModel, 4)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(only4) != 1 || len(only4[v.ID]) != 4 {
		t.Fatalf("按 dim=4 读取得到 %d 条，want 1 条 4 维", len(only4))
	}
	if _, ok := only4[v.ID]; ok {
		if len(only4[v.ID]) != 4 {
			t.Error("维度过滤没有生效：不同维度的向量混进了结果")
		}
	}
	other, err := repo.LoadVectors(ctx, []uint{v.ID}, itestEmbedModel+"-v2", 4)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("换模型后的向量应能被单独读到，实际 %d 条", len(other))
	}
}

// TestEmbeddingIntegrationListModels 守住"列出库里实际存在的向量空间"这条查询。
//
// 为什么这条必须用真库：它的 SQL 里有聚合别名（COUNT(*) AS ...），
// 而聚合别名撞上 MySQL 保留字时**编译期与单测期都毫无征兆**——语句在执行时
// 直接报 1064，调用方只看到"读不到向量空间"的降级（退回配置里的模型名），
// 日志之外没有任何信号。本机实验室里 `AS rows` 正是这样漏出去的。
//
// 因此断言必须真正执行这条语句并检查它**按 dim 分组返回了内容**：
// 任何"只检查 error == nil"或走 mock 的写法都抓不到它。
func TestEmbeddingIntegrationListModels(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()
	v := itestSeedEmbedVideo(t, db, "列出向量空间", "描述")

	const dim = 8
	older := itestEmbedModel + "-lm-older"
	newer := itestEmbedModel + "-lm-newer"

	seed := func(model string, fill float32) {
		t.Helper()
		err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{{
			VideoID: v.ID, Model: model, Dim: dim, Normalized: true,
			ContentHash: video.ContentHash(v.Title, v.Description),
			Vector:      itestVector(t, dim, fill),
		}})
		if err != nil {
			t.Fatalf("写入 %s 失败: %v", model, err)
		}
	}
	seed(older, 1)
	// 让两个空间的 updated_at 明确可区分（MAX(updated_at) 是排序依据）。
	time.Sleep(20 * time.Millisecond)
	seed(newer, 2)

	stats, err := repo.ListEmbeddingModels(ctx, dim, 8)
	if err != nil {
		t.Fatalf("列出向量空间失败（聚合别名是否撞了保留字？）: %v", err)
	}

	pos := make(map[string]int, len(stats))
	for i, s := range stats {
		pos[s.Model] = i
		if s.Model == older || s.Model == newer {
			if s.Dim != dim {
				t.Errorf("%s 的维度 = %d, want %d", s.Model, s.Dim, dim)
			}
			if s.Rows < 1 {
				t.Errorf("%s 的行数 = %d, want >= 1（COUNT 别名没有被正确扫描回来）", s.Model, s.Rows)
			}
			if s.LastUpdated.IsZero() {
				t.Errorf("%s 的 last_updated 为零值（MAX(updated_at) 没有被扫描回来）", s.Model)
			}
		}
	}
	if _, ok := pos[older]; !ok {
		t.Fatalf("dim=%d 的空间列表里没有 %s，实际返回 %d 条", dim, older, len(stats))
	}
	if _, ok := pos[newer]; !ok {
		t.Fatalf("dim=%d 的空间列表里没有 %s，实际返回 %d 条", dim, newer, len(stats))
	}
	// 按最近写入倒序：后写的排在先写的之前。
	if pos[newer] >= pos[older] {
		t.Errorf("排序没有按最近写入倒序: %s 在第 %d 位, %s 在第 %d 位",
			newer, pos[newer], older, pos[older])
	}

	// dim 过滤：换一个维度查询时，这两个空间都不该出现
	// （不同维度的向量不可比较，混进来会让检索侧取到错误的空间）。
	other, err := repo.ListEmbeddingModels(ctx, dim*2, 8)
	if err != nil {
		t.Fatalf("按另一维度列出失败: %v", err)
	}
	for _, s := range other {
		if s.Model == older || s.Model == newer {
			t.Errorf("dim 过滤没有生效: dim=%d 的查询返回了 %s", dim*2, s.Model)
		}
	}
}

// TestEmbeddingIntegrationStaleDetection 守住"内容变更 / 从来没有向量"两条。
func TestEmbeddingIntegrationStaleDetection(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()

	targets, err := repo.ListEmbeddingTargets(ctx, itestEmbedModel, 4, 1000)
	if err != nil {
		t.Fatalf("列出目标失败: %v", err)
	}
	byID := make(map[uint]video.EmbeddingTarget, len(targets))
	for _, tg := range targets {
		byID[tg.VideoID] = tg
	}

	fresh := itestSeedEmbedVideo(t, db, "刚写入向量", "描述")
	stale := itestSeedEmbedVideo(t, db, "内容会变", "描述")
	never := itestSeedEmbedVideo(t, db, "从来没有向量", "描述")

	if err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{
		{VideoID: fresh.ID, Model: itestEmbedModel, Dim: 4, Normalized: true,
			ContentHash: video.ContentHash(fresh.Title, fresh.Description), Vector: itestVector(t, 4, 1)},
		{VideoID: stale.ID, Model: itestEmbedModel, Dim: 4, Normalized: true,
			ContentHash: video.ContentHash(stale.Title, stale.Description), Vector: itestVector(t, 4, 1)},
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 改标题模拟"内容变了"。
	if err := db.Model(&video.Video{}).Where("id = ?", stale.ID).
		Update("title", "内容已经变了").Error; err != nil {
		t.Fatalf("更新标题失败: %v", err)
	}

	targets, err = repo.ListEmbeddingTargets(ctx, itestEmbedModel, 4, 5000)
	if err != nil {
		t.Fatalf("列出目标失败: %v", err)
	}
	byID = make(map[uint]video.EmbeddingTarget, len(targets))
	for _, tg := range targets {
		byID[tg.VideoID] = tg
	}

	// 从来没有向量的视频必须出现在列表里（LEFT JOIN 写错时会整个漏掉，
	// 表现为"新视频永远拿不到向量"，而日志里一切正常）。
	tg, ok := byID[never.ID]
	if !ok {
		t.Fatal("从来没有向量的视频没有出现在目标列表里：LEFT JOIN 写错了")
	}
	if !tg.NeedsEmbedding() {
		t.Error("从来没有向量时必须判为需要向量化")
	}

	// 改了标题的视频必须判为需要重算。
	if tg, ok := byID[stale.ID]; !ok {
		t.Fatal("内容变更的视频没有出现在目标列表里")
	} else if !tg.NeedsEmbedding() {
		t.Error("标题变了却判定为不需要重算：视频会一直用旧向量被召回")
	} else if tg.StoredHash == "" {
		t.Error("已入库的指纹没有读出来，无法判断是'内容变了'还是'从来没有向量'")
	}

	// 内容没变的视频不该被判为需要重算（否则回填任务会永远跑不完）。
	if tg, ok := byID[fresh.ID]; !ok {
		t.Fatal("已向量化的视频没出现在目标列表里")
	} else if tg.NeedsEmbedding() {
		t.Errorf("内容没变却要求重算：stored=%q current=%q", tg.StoredHash, tg.CurrentHash)
	}
}

// TestEmbeddingIntegrationRejectsBadRows 守住入库前的形状校验。
func TestEmbeddingIntegrationRejectsBadRows(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()
	v := itestSeedEmbedVideo(t, db, "坏行", "描述")

	cases := map[string]video.VideoEmbedding{
		"缺少幂等键":     {VideoID: 0, Model: itestEmbedModel, Dim: 4, ContentHash: "x", Vector: itestVector(t, 4, 1)},
		"缺少模型":      {VideoID: v.ID, Dim: 4, ContentHash: "x", Vector: itestVector(t, 4, 1)},
		"缺少内容指纹":    {VideoID: v.ID, Model: itestEmbedModel, Dim: 4, Vector: itestVector(t, 4, 1)},
		"维度声明与字节不符": {VideoID: v.ID, Model: itestEmbedModel, Dim: 4, ContentHash: "x", Vector: itestVector(t, 8, 1)},
	}
	for name, row := range cases {
		if err := repo.UpsertEmbeddings(ctx, []video.VideoEmbedding{row}); err == nil {
			t.Errorf("%s 必须被拒绝", name)
		}
	}
	var n int64
	db.Model(&video.VideoEmbedding{}).Where("video_id = ?", v.ID).Count(&n)
	if n != 0 {
		t.Errorf("被拒绝的行不该入库，实际写入 %d 行", n)
	}
	// 空切片是合法的（表示无事可做），不该报错。
	if err := repo.UpsertEmbeddings(ctx, nil); err != nil {
		t.Errorf("空批次不该报错，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// R1：标签附着的 TOCTOU 窗口
// ---------------------------------------------------------------------------

// TestAttachTagsIntegrationConcurrentSamePair 覆盖 P2 前置条件里的 R1。
//
// 背景：attachTagsTx 过去是"先 Count 再 Create"两步。单 worker 顺序消费下
// 不会触发，但在**多 worker 副本 + MQ 至少一次重投**时，两个副本可能同时
// 处理同一条消息：都 Count 到 0，然后一起 INSERT —— 唯一键在这里救不了我们，
// 因为 video_tags 上没有 (video_id, tag_id) 唯一索引（加它会先在历史重复行上
// 迁移失败，而 P1 的硬约束是"只新增，不改既有结构"）。
//
// 这条用例用真库把并发场景跑出来：
//   - N 个 goroutine 过 barrier 后同时给**同一个视频**附着**同一批标签**；
//   - 断言 (video_id, tag_id) 关联行数恰好是 1，且标签行本身只有一行。
//
// 它同时是对既有实现的**事实核查**：resolveTagID 会对 tags 行加写锁，
// 这层锁在本仓库里恰好把同标签的事务串行化了。断言仍然要留着——
// 那条锁是 resolveTagID 的实现细节，一旦它改成"只读不写"，这里的竞态
// 会立刻变成真的，而那时的表现只是"标签重复"（读路径用 DISTINCT，
// 完全看不出来）。
func TestAttachTagsIntegrationConcurrentSamePair(t *testing.T) {
	db, repo := itestOpenRepo(t)
	ctx := context.Background()
	v := itestSeedEmbedVideo(t, db, "并发标签", "描述")
	tagNames := []string{
		itestTagName(t, "race1"), itestTagName(t, "race2"), itestTagName(t, "race3"),
	}
	t.Cleanup(func() {
		for _, name := range tagNames {
			// 只删已经没有任何关联的标签，绝不误删真实数据。
			db.Exec("DELETE FROM tags WHERE name = ? AND id NOT IN (SELECT tag_id FROM video_tags)", name)
		}
	})

	suggestions := make([]video.TagSuggestion, 0, len(tagNames))
	for _, n := range tagNames {
		suggestions = append(suggestions, video.TagSuggestion{Name: n, Confidence: 0.9})
	}

	const goroutines = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := repo.AttachTags(ctx, v.ID, suggestions); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		// 死锁（1213）在这条路径上是可接受的：事务被回滚、消息会重投。
		// 不可接受的是"两个事务都成功却留下两行"。
		t.Logf("第 %d 个并发附着返回错误（可能死锁，可被重投修复）: %v", i+1, err)
	}

	for _, name := range tagNames {
		var tag video.Tag
		if err := db.Where("name = ?", name).First(&tag).Error; err != nil {
			t.Fatalf("标签 %q 未落库: %v", name, err)
		}
		if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ? AND tag_id = ?", v.ID, tag.ID); n != 1 {
			t.Errorf("(%d,%d) 的关联行数 = %d, want 1：attachTagsTx 存在 TOCTOU 窗口"+
				"（两个事务同时查不到、一起插入）", v.ID, tag.ID, n)
		}
	}
}
