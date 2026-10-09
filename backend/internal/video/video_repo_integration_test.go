package video_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/video"

	"gorm.io/gorm"
)

// 本文件用**真实 MySQL** 验证 PublishWithOutbox —— 也就是 outbox 模式的一致性保证。
//
// 为什么必须用真库：此前从 service 下沉到 repo 的 6 个事务里，其余 5 个都有真实
// 数据库覆盖，只有 PublishWithOutbox 仅被 mock 断言过"service 是否调用了它"。
// 而它守的恰恰是整个 outbox 模式存在的理由：视频、本地消息表、标签三者同成败。
//
// 三件 mock 证明不了的事：
//
//  1. **原子性**——任何一步失败，之前已写入的行必须一起回滚。视频写成功而
//     outbox 写失败，该视频将永远不会进入任何人的时间线（静默且永久）；反之
//     则产生指向不存在视频的消息。
//  2. **outbox 行的字段值**——worker 只抢占 status='pending' 的行、并按
//     create_time ASC 排序（见 internal/worker/outboxworker.go）。字段名或取值
//     写错不会有任何编译错误，只会让视频静静地进不了时间线。
//  3. **标签的复用与去重**——FirstOrCreate 的最终行为由数据库唯一键仲裁，
//     在内存里 mock 一个 map 无法复现。
//
// 默认跳过；设置 TEST_MYSQL_DSN 后运行（openIntegrationDB 定义在
// like_repo_integration_test.go）：
//
//	TEST_MYSQL_DSN='root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test -count=1 -run Integration ./internal/video/
//
// 关于"如何人为制造事务中途失败"：MySQL 处于 STRICT_TRANS_TABLES（本机实测
// sql_mode 含该项），因此向 tags.name（varchar(100)）插入超长值会返回
// Error 1406 而不是被静默截断。已实测确认：
//
//	INSERT INTO tags (name) VALUES (REPEAT('a',101));
//	-- ERROR 1406 (22001): Data too long for column 'name' at row 1
//
// 这让我们可以把失败精确地放在事务的**最后一步**，从而检验前两步是否回滚。

// itestPublishAuthorID 与 like_repo_integration_test.go 里的 900001 区分开，
// 便于在库里识别本文件造出的数据。
const itestPublishAuthorID = 900010

// itestTagName 生成本次测试专用的标签名，避免与真实数据或历史运行互相污染。
// 必须匹配 video.ExtractTags 的 [\p{L}\p{N}_]+，因此只用字母数字下划线。
func itestTagName(t *testing.T, suffix string) string {
	t.Helper()
	name := "itest_" + strings.ReplaceAll(t.Name(), "/", "_") + "_" + suffix
	if len(name) > 100 {
		t.Fatalf("测试标签名 %d 字符，超过 tags.name 的 varchar(100)", len(name))
	}
	return name
}

// itestRegisterPublishCleanup 注册清理。
//
// 通过闭包在测试结束时才读取 v.ID，因此即使发布在中途失败、留下"半个事务"
// 的残留行，也能被清理干净——这一点对原子性测试尤其重要：断言失败时我们
// 恰恰希望保留证据，但测试结束后不应污染数据库。
func itestRegisterPublishCleanup(t *testing.T, db *gorm.DB, v *video.Video, tagNames ...string) {
	t.Helper()
	t.Cleanup(func() {
		if v.ID != 0 {
			db.Exec("DELETE FROM video_tags WHERE video_id = ?", v.ID)
			db.Exec("DELETE FROM outbox_msgs WHERE video_id = ?", v.ID)
			db.Exec("DELETE FROM videos WHERE id = ?", v.ID)
		}
		for _, name := range tagNames {
			// 只删已经没有任何关联的标签，绝不误删真实数据。
			db.Exec("DELETE FROM tags WHERE name = ? AND id NOT IN (SELECT tag_id FROM video_tags)", name)
		}
	})
}

func itestNewPublishVideo(title, description string) *video.Video {
	return &video.Video{
		AuthorID:    itestPublishAuthorID,
		Username:    "itest-publish-user",
		Title:       title,
		Description: description,
		PlayURL:     "itest://play",
		CoverURL:    "itest://cover",
	}
}

// itestCountRows 按条件数行，专用于断言"某张表里还有没有残留"。
func itestCountRows(t *testing.T, db *gorm.DB, model any, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Where(query, args...).Count(&n).Error; err != nil {
		t.Fatalf("计数失败 (%s): %v", query, err)
	}
	return n
}

// TestPublishWithOutboxIntegration 覆盖正常路径，逐字段校验三种行。
//
// outbox 行的每个字段都是 worker 的输入契约：worker 只抢占
// status='pending' 的行，按 create_time 排序，用 video_id 去取视频、
// 用 event_type 决定怎么处理。任何一个写错都不会编译报错，
// 只会让视频静默地进不了时间线——所以这里逐字段断言。
func TestPublishWithOutboxIntegration(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	tagA := itestTagName(t, "a")
	tagB := itestTagName(t, "b")
	v := itestNewPublishVideo(
		"itest 标题 #"+tagA,
		"描述里有 #"+tagA+" 和 #"+tagB, // tagA 重复出现，用于验证去重
	)
	itestRegisterPublishCleanup(t, db, v, tagA, tagB)

	if err := repo.PublishWithOutbox(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if v.ID == 0 {
		t.Fatal("发布成功后 video.ID 应被回填，实际为 0")
	}

	// --- 1. 视频行确实落库 ---
	var got video.Video
	if err := db.First(&got, v.ID).Error; err != nil {
		t.Fatalf("视频未落库: %v", err)
	}
	if got.Title != v.Title {
		t.Errorf("title = %q, want %q", got.Title, v.Title)
	}

	// --- 2. outbox 行：worker 的输入契约 ---
	var msgs []video.OutboxMsg
	if err := db.Where("video_id = ?", v.ID).Find(&msgs).Error; err != nil {
		t.Fatalf("查询 outbox 失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox 行数 = %d, want 1（必须恰好一条：0 条视频永远进不了时间线，多条会被重复投递）", len(msgs))
	}
	msg := msgs[0]
	if msg.VideoID != v.ID {
		t.Errorf("outbox.video_id = %d, want %d（worker 靠它取视频，写错会投递错视频）", msg.VideoID, v.ID)
	}
	if msg.Status != "pending" {
		t.Errorf("outbox.status = %q, want %q（worker 只抢占 pending，其它值等于永不投递）", msg.Status, "pending")
	}
	if msg.EventType != "video_published" {
		t.Errorf("outbox.event_type = %q, want %q", msg.EventType, "video_published")
	}
	if msg.CreateTime.IsZero() {
		t.Error("outbox.create_time 为零值；worker 按 create_time ASC 排序，零值会让该行永远排在最前")
	}

	// --- 3. 标签：两个不同标签，tagA 重复出现只算一次 ---
	for _, name := range []string{tagA, tagB} {
		var tag video.Tag
		if err := db.Where("name = ?", name).First(&tag).Error; err != nil {
			t.Fatalf("标签 %q 未落库: %v", name, err)
		}
		if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ? AND tag_id = ?", v.ID, tag.ID); n != 1 {
			t.Errorf("标签 %q 的关联行数 = %d, want 1（同一标签在一篇视频里重复出现必须只关联一次）", name, n)
		}
	}
	if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ?", v.ID); n != 2 {
		t.Errorf("video_tags 总行数 = %d, want 2（标题 1 个 + 描述 2 个去重后共 2 个标签）", n)
	}
}

// TestPublishWithOutboxRollsBackVideoAndOutboxOnTagFailure 是本文件最重要的用例。
//
// 把失败点放在事务的**最后一步**（写标签），然后断言前两步写入的视频行与
// outbox 行都已回滚。这是唯一能证明"三者同成败"的方向：如果反过来把失败放在
// 第一步，后面的步骤根本不会执行，测不出任何回滚行为。
//
// 若此用例失败，意味着存在"视频已入库、outbox 未入库"的窗口——该视频不会
// 报错、不会被删除，只是永远不出现在任何人的时间线里。
func TestPublishWithOutboxRollsBackVideoAndOutboxOnTagFailure(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	// tags.name 是 varchar(100)；101 个字符在 STRICT_TRANS_TABLES 下必然报
	// Error 1406，从而让事务在写完视频与 outbox 之后失败。
	overlong := strings.Repeat("x", 101)
	v := itestNewPublishVideo("itest 超长标签 #"+overlong, "")
	itestRegisterPublishCleanup(t, db, v)

	err := repo.PublishWithOutbox(context.Background(), v)
	if err == nil {
		t.Fatal("标签名超长时发布必须失败，实际返回 nil（说明超长值被静默截断，本用例的失败注入失效）")
	}

	// 注意：GORM 在 INSERT 后会把自增 ID 回填到结构体，即使事务已回滚。
	// 所以下面用这个 ID 去查库，查不到才是正确结果。
	if v.ID == 0 {
		t.Fatal("即使回滚，GORM 也应已把自增 ID 回填到结构体——为 0 说明失败发生在 INSERT 之前，本用例没测到回滚")
	}

	if n := itestCountRows(t, db, &video.Video{}, "id = ?", v.ID); n != 0 {
		t.Errorf("视频行残留 %d 条（id=%d）；事务未回滚：该视频永远不会进入任何人的时间线", n, v.ID)
	}
	if n := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", v.ID); n != 0 {
		t.Errorf("outbox 行残留 %d 条（video_id=%d）；会出现指向不存在视频的消息", n, v.ID)
	}
	if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ?", v.ID); n != 0 {
		t.Errorf("video_tags 关联行残留 %d 条（video_id=%d）", n, v.ID)
	}
}

// TestPublishWithOutboxNoOutboxRowWhenVideoInsertFails 覆盖反方向：
// 第一条 INSERT 就失败时，不得留下任何 outbox 行。
//
// 用一个已存在的 video.ID 触发主键冲突。除了证明"不会凭空多出 outbox 行"，
// 它还验证了即使 service 层误传了带 ID 的结构体，也不会污染 outbox 表。
func TestPublishWithOutboxNoOutboxRowWhenVideoInsertFails(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)
	ctx := context.Background()

	existing := itestNewPublishVideo("itest 已存在的视频 #"+itestTagName(t, "x"), "")
	itestRegisterPublishCleanup(t, db, existing, itestTagName(t, "x"))
	if err := repo.PublishWithOutbox(ctx, existing); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	before := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", existing.ID)

	// 复制一份并复用同一个主键：视频 INSERT 必然冲突。
	dup := itestNewPublishVideo("itest 主键冲突 #"+itestTagName(t, "y"), "")
	dup.ID = existing.ID
	itestRegisterPublishCleanup(t, db, dup, itestTagName(t, "y"))

	if err := repo.PublishWithOutbox(ctx, dup); err == nil {
		t.Fatal("主键冲突时发布必须失败，实际返回 nil")
	}

	after := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", existing.ID)
	if after != before {
		t.Errorf("outbox 行数从 %d 变成 %d；失败的发布不得写入 outbox 行", before, after)
	}
	if n := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", existing.ID); n != 1 {
		t.Errorf("outbox 行数 = %d, want 1（原视频的那一条）", n)
	}
}

// TestPublishWithOutboxReusesExistingTag 证明同一标签被多篇视频复用：
// tags 表只应有一行，video_tags 各一行。
//
// 这是 FirstOrCreate 的核心价值。若它退化成无条件 Create，tags 表的唯一键会
// 让第二篇视频的发布**整体失败**——用户发布带重复话题的视频会直接报错。
func TestPublishWithOutboxReusesExistingTag(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)
	ctx := context.Background()

	shared := itestTagName(t, "shared")
	first := itestNewPublishVideo("itest 第一篇 #"+shared, "")
	second := itestNewPublishVideo("itest 第二篇 #"+shared, "")
	itestRegisterPublishCleanup(t, db, first, shared)
	itestRegisterPublishCleanup(t, db, second, shared)

	if err := repo.PublishWithOutbox(ctx, first); err != nil {
		t.Fatalf("第一篇发布失败: %v", err)
	}
	if err := repo.PublishWithOutbox(ctx, second); err != nil {
		t.Fatalf("第二篇发布失败（复用已存在标签时不得报错）: %v", err)
	}

	var tags []video.Tag
	if err := db.Where("name = ?", shared).Find(&tags).Error; err != nil {
		t.Fatalf("查询标签失败: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("同名标签行数 = %d, want 1（FirstOrCreate 未复用）", len(tags))
	}
	for _, v := range []*video.Video{first, second} {
		if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ? AND tag_id = ?", v.ID, tags[0].ID); n != 1 {
			t.Errorf("视频 %d 的标签关联行数 = %d, want 1", v.ID, n)
		}
	}
}

// TestPublishWithOutboxWithoutTags 覆盖"没有话题"的常见路径：
// 视频与 outbox 照常写入，且不产生任何标签行。
func TestPublishWithOutboxWithoutTags(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	// "#-" 与孤立的 "#" 都匹配不上 #[\p{L}\p{N}_]+，因此一个标签都提取不出来。
	// （注意别拿 "#not-a-tag" 当反例：它其实会提取出 "not"，见下一个用例。）
	v := itestNewPublishVideo("itest 没有话题", "只有孤立的 # 和后面跟减号的 #-")
	itestRegisterPublishCleanup(t, db, v)

	if err := repo.PublishWithOutbox(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ?", v.ID); n != 0 {
		t.Errorf("video_tags 行数 = %d, want 0（无有效话题时不应产生标签）", n)
	}
	// 标签提取失败不应影响 outbox：视频照样要能进时间线。
	if n := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", v.ID); n != 1 {
		t.Errorf("outbox 行数 = %d, want 1（无话题的视频也必须能进时间线）", n)
	}
}

// TestPublishWithOutboxTagRegexBoundary 钉住 ExtractTags 的真实边界行为。
//
// 写这个用例是因为我起初想当然地以为 "#not-a-tag" 不会被识别成标签，
// 结果测试红了——实际行为是正则只吃到 "-" 之前，提取出 "not"。
// 这是**既有行为**（不是本次改动引入的），此处如实记录，免得以后有人
// 依据错误的直觉去"修"它。
func TestPublishWithOutboxTagRegexBoundary(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	// 用测试专用前缀，确保提取出的标签名不会撞上真实数据里已有的 "not"。
	prefix := itestTagName(t, "boundary")
	v := itestNewPublishVideo("itest 边界 #"+prefix+"-suffix", "")
	itestRegisterPublishCleanup(t, db, v, prefix)

	if err := repo.PublishWithOutbox(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// 期望：提取 "#" 之后连续的一段字母数字下划线，即 prefix（在 "-" 处截断）。
	var tag video.Tag
	if err := db.Where("name = ?", prefix).First(&tag).Error; err != nil {
		t.Fatalf("期望提取出标签 %q，实际未落库: %v（若正则行为确已改变，请确认是有意为之）", prefix, err)
	}
	if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ? AND tag_id = ?", v.ID, tag.ID); n != 1 {
		t.Errorf("关联行数 = %d, want 1", n)
	}
}

// TestPublishWithOutboxConcurrentSameNewTag 并发发布同一个**全新**话题。
//
// 这是本文件发现真实缺陷的地方，也是 resolveTagID 存在的原因。
//
// 旧实现（GORM 的 FirstOrCreate）是"先 SELECT，查不到再 INSERT"，两步之间毫
// 无保护。N 个事务同时发布同一个新话题时会一起查不到、一起去 INSERT，由
// tags.idx_tags_name 这个唯一键决定谁活下来，其余全撞 Error 1062。该错误冒泡
// 出事务、让整个发布回滚——用户提交的视频完全合法，失败却与输入无关，对外
// 只表现为一个 500。
//
// 用真库实测（8 个 goroutine 过 barrier 后同时发布，重复 5 次）：
//
//	修复前：7/8、7/8、7/8、7/8、7/8 个发布失败
//	修复后：本用例通过（0/8 失败）
//
// 竞态窗口比"同时按下发布"宽得多：SELECT 与 INSERT 之间还夹着视频行、outbox
// 行的两次写入往返，因此几百毫秒内发布同一新话题的人都会互相踩到。
//
// 这个竞态无法用 mock 复现，只能靠真库。
func TestPublishWithOutboxConcurrentSameNewTag(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	const n = 8
	shared := itestTagName(t, "race")
	videos := make([]*video.Video, n)
	for i := range videos {
		videos[i] = itestNewPublishVideo("itest 并发 #"+shared, "")
		itestRegisterPublishCleanup(t, db, videos[i], shared)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(v *video.Video) {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时进入事务
			if err := repo.PublishWithOutbox(context.Background(), v); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(videos[i])
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		for i, err := range errs {
			t.Errorf("第 %d 个并发发布失败: %v", i+1, err)
		}
		t.Errorf("%d/%d 个并发发布同一个新话题时失败；这些用户明明提交了合法的视频，"+
			"却因为一个与他们的输入无关的竞态而发布失败（FirstOrCreate 的 SELECT-then-INSERT 竞态）",
			len(errs), n)
		return
	}

	// 全部成功时，标签必须只有一行，且每个视频都有且仅有一条关联。
	var tags []video.Tag
	if err := db.Where("name = ?", shared).Find(&tags).Error; err != nil {
		t.Fatalf("查询标签失败: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("同名标签行数 = %d, want 1", len(tags))
	}
	// 必须按 tag_id 精确计数，不能只数 video_id：
	// 命中冲突的那 7 个事务走的是"upsert 后回查 ID"的分支，如果回查写错、
	// 让 tag.ID 停在 0，这里就会插出 tag_id=0 的关联行。而只数 video_id
	// 的话它照样等于 1，测试会假绿。
	tagID := tags[0].ID
	if tagID == 0 {
		t.Fatal("标签 ID 为 0，后续断言失去意义")
	}
	for _, v := range videos {
		if n := itestCountRows(t, db, &video.VideoTag{}, "video_id = ? AND tag_id = ?", v.ID, tagID); n != 1 {
			t.Errorf("视频 %d 指向标签 %d 的关联行数 = %d, want 1"+
				"（若为 0，检查 resolveTagID 在 upsert 命中已存在行时是否正确回查了 ID）",
				v.ID, tagID, n)
		}
	}
	// 反向确认：没有任何关联行指向 tag_id=0。
	if n := itestCountRows(t, db, &video.VideoTag{}, "tag_id = 0"); n != 0 {
		t.Errorf("存在 %d 条 tag_id=0 的关联行：resolveTagID 未正确回填已存在标签的 ID", n)
	}
}

// 断言清理辅助函数自身不会掩盖问题：itestCountRows 若写错表名会静默返回 0，
// 让所有"不得残留"的断言变成永远通过。这里用一个已知存在的行反向验证它。
func TestItestCountRowsHelperSanity(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewVideoRepository(db)

	v := itestNewPublishVideo("itest 辅助函数自检 #"+itestTagName(t, "sanity"), "")
	itestRegisterPublishCleanup(t, db, v, itestTagName(t, "sanity"))
	if err := repo.PublishWithOutbox(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	if n := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", v.ID); n != 1 {
		t.Fatalf("itestCountRows 对一个已知存在的 outbox 行返回 %d, want 1——"+
			"它若失效，所有'不得残留'的断言都会变成永远通过", n)
	}
	if n := itestCountRows(t, db, &video.OutboxMsg{}, "video_id = ?", 999999999); n != 0 {
		t.Errorf("itestCountRows 对不存在的 video_id 返回 %d, want 0", n)
	}
}
