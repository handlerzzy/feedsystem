package video_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/db"
	"github.com/handlerzzy/feedsystem/internal/video"

	"gorm.io/gorm"
)

// 本文件用**真实 MySQL** 验证 P1 的幂等性。
//
// 为什么必须连真库：幂等性靠的是"唯一键 + ON DUPLICATE KEY UPDATE"和
// "先查再插"这两件**数据库行为**，mock 里证明不了。而重复行/重复标签
// 正是 P1 验收清单里明确要求的两项（"重复消费不产生重复数据"）。
//
// 默认跳过；设置 TEST_MYSQL_DSN 后运行：
//
//	TEST_MYSQL_DSN='root:123456@tcp(127.0.0.1:3307)/feedflow_test?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test -count=1 -v ./internal/video/ -run TestAnalysisIdempotency

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过依赖真实 MySQL 的集成测试")
	}
	cfg, err := parseDSN(dsn)
	if err != nil {
		t.Fatalf("TEST_MYSQL_DSN 解析失败: %v", err)
	}
	gdb, err := db.NewDB(cfg)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("迁移失败（P1 的新表/新列没登记进 AutoMigrate？）: %v", err)
	}
	t.Cleanup(func() { _ = db.CloseDB(gdb) })
	return gdb
}

// parseDSN 是测试专用的极简 DSN 解析（与 cmd/evalbaseline 的同名函数同源）。
func parseDSN(dsn string) (config.DatabaseConfig, error) {
	// 形如 user:pass@tcp(host:port)/dbname?...
	var cfg config.DatabaseConfig
	at := -1
	for i := 0; i+5 <= len(dsn); i++ {
		if dsn[i:i+5] == "@tcp(" {
			at = i
		}
	}
	if at < 0 {
		return cfg, errInvalidDSN
	}
	userInfo := dsn[:at]
	rest := dsn[at+5:]
	closeIdx := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == ')' {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return cfg, errInvalidDSN
	}
	hostPort := rest[:closeIdx]
	after := rest[closeIdx+1:]
	if len(after) > 0 && after[0] == '/' {
		after = after[1:]
	}
	for i := 0; i < len(after); i++ {
		if after[i] == '?' {
			after = after[:i]
			break
		}
	}
	if after == "" {
		return cfg, errInvalidDSN
	}
	cfg.DBName = after

	host := hostPort
	port := 3306
	for i := len(hostPort) - 1; i >= 0; i-- {
		if hostPort[i] == ':' {
			host = hostPort[:i]
			p := 0
			for _, c := range hostPort[i+1:] {
				if c < '0' || c > '9' {
					return cfg, errInvalidDSN
				}
				p = p*10 + int(c-'0')
			}
			port = p
			break
		}
	}
	cfg.Host = host
	cfg.Port = port

	user := userInfo
	for i := 0; i < len(userInfo); i++ {
		if userInfo[i] == ':' {
			user = userInfo[:i]
			cfg.Password = userInfo[i+1:]
			break
		}
	}
	cfg.User = user
	return cfg, nil
}

var errInvalidDSN = &dsnError{}

type dsnError struct{}

func (*dsnError) Error() string { return "TEST_MYSQL_DSN 格式应为 user:pass@tcp(host:port)/dbname" }

// seedVideo 造一条测试视频并返回 ID。
func seedVideo(t *testing.T, gdb *gorm.DB, title, desc string) uint {
	t.Helper()
	v := &video.Video{
		AuthorID: 1, Username: "tester", Title: title, Description: desc,
		PlayURL: "http://example.com/p.mp4", CoverURL: "http://example.com/c.jpg",
		CreateTime: time.Now(),
	}
	if err := gdb.Create(v).Error; err != nil {
		t.Fatalf("造测试视频失败: %v", err)
	}
	t.Cleanup(func() {
		// 清理顺序：先关联、再标签、最后视频。
		// 不清理会污染后续用例的统计（例如 tags 表行数断言）。
		gdb.Exec("DELETE FROM video_tags WHERE video_id = ?", v.ID)
		gdb.Exec("DELETE FROM video_ai_analyses WHERE video_id = ?", v.ID)
		gdb.Exec("DELETE FROM videos WHERE id = ?", v.ID)
	})
	return v.ID
}

// TestAnalysisIdempotencyIsReal 是 P1 验收清单"重复消费不产生重复数据"的实现。
//
// 同一份模型结果写入三次，断言：
//   - video_ai_analyses 只有一行；
//   - video_tags 只有 N 行（不是 3N）；
//   - tags 表里没有语义重复的行。
func TestAnalysisIdempotencyIsReal(t *testing.T) {
	gdb := testDB(t)
	ctx := context.Background()
	repo := video.NewVideoRepository(gdb)
	videoID := seedVideo(t, gdb, "猫的一天", "记录家里的猫")

	tags := []video.TagSuggestion{
		{Name: "猫", Confidence: 0.9},
		{Name: "宠物", Confidence: 0.8},
	}
	mk := func() *video.VideoAIAnalysis {
		return &video.VideoAIAnalysis{
			VideoID: videoID, Model: "test-model", PromptVersion: "v-test",
			Summary: "一只猫在打盹", Status: video.AnalysisStatusOK, AnalyzedAt: time.Now(),
		}
	}

	for i := 0; i < 3; i++ {
		if err := repo.SaveAnalysisWithTags(ctx, mk(), tags); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}

	var analysisRows int64
	if err := gdb.Model(&video.VideoAIAnalysis{}).Where("video_id = ?", videoID).Count(&analysisRows).Error; err != nil {
		t.Fatalf("统计分析行失败: %v", err)
	}
	if analysisRows != 1 {
		t.Errorf("video_ai_analyses 行数 = %d, want 1（幂等键没生效）", analysisRows)
	}

	var tagRows int64
	if err := gdb.Table("video_tags").Where("video_id = ?", videoID).Count(&tagRows).Error; err != nil {
		t.Fatalf("统计关联行失败: %v", err)
	}
	if tagRows != int64(len(tags)) {
		t.Errorf("video_tags 行数 = %d, want %d（重复插入没被挡住）", tagRows, len(tags))
	}

	// tags 表里不该出现同名两行（唯一索引 + 复用逻辑）。
	var nameRows int64
	if err := gdb.Table("tags").Where("name IN ?", []string{"猫", "宠物"}).Count(&nameRows).Error; err != nil {
		t.Fatalf("统计标签行失败: %v", err)
	}
	if nameRows != 2 {
		t.Errorf("tags 行数 = %d, want 2（同名标签应复用同一行）", nameRows)
	}
}

// TestAITagReusesUserWrittenTagRow 守住 4.4 的"用户标签与 AI 标签共用同一个 tags 行"。
func TestAITagReusesUserWrittenTagRow(t *testing.T) {
	gdb := testDB(t)
	ctx := context.Background()
	repo := video.NewVideoRepository(gdb)
	videoID := seedVideo(t, gdb, "猫的一天 #猫", "记录家里的猫")

	// 模拟用户手写 #猫 产生的关联（PublishWithOutbox 会做这件事）。
	userVideo := &video.Video{
		AuthorID: 1, Username: "tester", Title: "猫的一天 #猫", Description: "记录家里的猫",
		PlayURL: "http://example.com/p.mp4", CoverURL: "http://example.com/c.jpg",
		CreateTime: time.Now(),
	}
	if err := repo.PublishWithOutbox(ctx, userVideo); err != nil {
		t.Fatalf("模拟用户发布失败: %v", err)
	}
	t.Cleanup(func() {
		gdb.Exec("DELETE FROM video_tags WHERE video_id = ?", userVideo.ID)
		gdb.Exec("DELETE FROM video_ai_analyses WHERE video_id = ?", userVideo.ID)
		gdb.Exec("DELETE FROM outbox_msgs WHERE video_id = ?", userVideo.ID)
		gdb.Exec("DELETE FROM videos WHERE id = ?", userVideo.ID)
	})

	var userTagID uint
	if err := gdb.Table("tags").Select("id").Where("name = ?", "猫").Scan(&userTagID).Error; err != nil || userTagID == 0 {
		t.Fatalf("用户标签 '猫' 应当已存在: id=%d err=%v", userTagID, err)
	}

	// AI 也给出 "猫"：必须复用同一行，且不因为已有关联而重复插入。
	if err := repo.SaveAnalysisWithTags(ctx, &video.VideoAIAnalysis{
		VideoID: userVideo.ID, Model: "test-model", PromptVersion: "v-test",
		Summary: "摘要", Status: video.AnalysisStatusOK,
	}, []video.TagSuggestion{{Name: "猫", Confidence: 0.95}}); err != nil {
		t.Fatalf("AI 写入失败: %v", err)
	}

	var catRows int64
	if err := gdb.Table("tags").Where("name = ?", "猫").Count(&catRows).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if catRows != 1 {
		t.Errorf("tags 里 '猫' 的行数 = %d, want 1（AI 与用户标签必须共用同一行）", catRows)
	}

	var linkRows int64
	if err := gdb.Table("video_tags").Where("video_id = ? AND tag_id = ?", userVideo.ID, userTagID).Count(&linkRows).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if linkRows != 1 {
		t.Errorf("video_tags 关联行数 = %d, want 1（不该因为 AI 也给了同一标签而重复）", linkRows)
	}

	_ = videoID
}

// TestLatestAISummariesPicksNewestAndSkipsInvalid 覆盖 Feed 读取路径用到的查询。
func TestLatestAISummariesPicksNewestAndSkipsInvalid(t *testing.T) {
	gdb := testDB(t)
	ctx := context.Background()
	repo := video.NewVideoRepository(gdb)
	videoID := seedVideo(t, gdb, "猫的一天", "记录家里的猫")

	// 旧版本的分析：应当被更新的那一行盖过。
	if err := repo.SaveAnalysisWithTags(ctx, &video.VideoAIAnalysis{
		VideoID: videoID, Model: "m1", PromptVersion: "v1",
		Summary: "旧摘要", Status: video.AnalysisStatusOK,
	}, nil); err != nil {
		t.Fatalf("写入旧分析失败: %v", err)
	}
	// 新版本：不同 prompt_version → 允许新增一行。
	if err := repo.SaveAnalysisWithTags(ctx, &video.VideoAIAnalysis{
		VideoID: videoID, Model: "m2", PromptVersion: "v2",
		Summary: "新摘要", Status: video.AnalysisStatusOK,
	}, nil); err != nil {
		t.Fatalf("写入新分析失败: %v", err)
	}
	// invalid 的行即使更新也不该被当成摘要返回。
	if err := repo.SaveAnalysisWithTags(ctx, &video.VideoAIAnalysis{
		VideoID: videoID, Model: "m3", PromptVersion: "v3",
		Summary: "不该被读到", Status: video.AnalysisStatusInvalid,
	}, nil); err != nil {
		t.Fatalf("写入无效分析失败: %v", err)
	}

	got, err := repo.LatestAISummaries(ctx, []uint{videoID})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got[videoID] != "新摘要" {
		t.Errorf("摘要 = %q, want 新摘要（应取最新一条 ok 的行）", got[videoID])
	}
}
