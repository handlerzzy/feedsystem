package video_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/video"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件用**真实 MySQL** 验证回退事务的语义。
//
// 为什么必须用真库：mock 只能证明"service 是否调用了 repo"，证明不了
// "重复点赞时计数不会漂移"。而后者正是第三批加 errSkipCountUpdate 哨兵
// 要解决的问题，也是本次事务下沉最容易改坏的地方——事务边界、回滚时机、
// GREATEST 兜底都只有在真实数据库上才成立。
//
// 默认跳过；设 TEST_MYSQL_DSN 后运行，例如：
//
//	TEST_MYSQL_DSN='root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test -count=1 -run Integration ./internal/video/
func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过依赖真实数据库的集成测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试数据库失败: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("获取 sql.DB 失败: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("数据库不可达: %v", err)
	}
	return db
}

// itestSeedVideo 插入一条专用测试视频，并注册清理。
// author_id 用固定的大数值，便于识别与批量清理，不会与真实数据混淆。
func itestSeedVideo(t *testing.T, db *gorm.DB) *video.Video {
	t.Helper()
	const itestAuthorID = 900001
	v := &video.Video{
		AuthorID: itestAuthorID,
		Username: "itest-user",
		Title:    "itest-" + time.Now().Format("20060102150405.000000"),
		PlayURL:  "itest://play",
		CoverURL: "itest://cover",
	}
	if err := db.Create(v).Error; err != nil {
		t.Fatalf("插入测试视频失败: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM likes WHERE video_id = ?", v.ID)
		db.Exec("DELETE FROM videos WHERE id = ?", v.ID)
	})
	return v
}

func itestReload(t *testing.T, db *gorm.DB, id uint) (likesCount, popularity int64) {
	t.Helper()
	var v video.Video
	if err := db.First(&v, id).Error; err != nil {
		t.Fatalf("重新读取视频失败: %v", err)
	}
	return v.LikesCount, v.Popularity
}

// TestLikeWithCountsIntegration 证明：重复点赞被视为幂等成功，
// 且**计数不会漂移**（第二次必须回滚，而不是再加一次）。
func TestLikeWithCountsIntegration(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewLikeRepository(db)
	ctx := context.Background()

	v := itestSeedVideo(t, db)
	like := &video.Like{VideoID: v.ID, AccountID: 900002, CreatedAt: time.Now()}

	created, err := repo.LikeWithCounts(ctx, like)
	if err != nil {
		t.Fatalf("首次点赞失败: %v", err)
	}
	if !created {
		t.Error("首次点赞应返回 created=true")
	}
	if lc, pop := itestReload(t, db, v.ID); lc != 1 || pop != 1 {
		t.Fatalf("首次点赞后 likes_count=%d popularity=%d，期望 1/1", lc, pop)
	}

	// 第二次：唯一键冲突 => 事务回滚 => 幂等成功且计数不变。
	dup := &video.Like{VideoID: v.ID, AccountID: 900002, CreatedAt: time.Now()}
	created, err = repo.LikeWithCounts(ctx, dup)
	if err != nil {
		t.Fatalf("重复点赞必须返回 nil 错误（幂等），实际: %v", err)
	}
	if created {
		t.Error("重复点赞应返回 created=false")
	}
	if lc, pop := itestReload(t, db, v.ID); lc != 1 || pop != 1 {
		t.Fatalf("重复点赞后计数漂移: likes_count=%d popularity=%d，期望仍为 1/1", lc, pop)
	}
}

// TestUnlikeWithCountsIntegration 证明：重复取消点赞幂等，且计数不会减成负数。
func TestUnlikeWithCountsIntegration(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewLikeRepository(db)
	ctx := context.Background()

	v := itestSeedVideo(t, db)
	if _, err := repo.LikeWithCounts(ctx, &video.Like{
		VideoID: v.ID, AccountID: 900003, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("准备点赞失败: %v", err)
	}

	deleted, err := repo.UnlikeWithCounts(ctx, v.ID, 900003)
	if err != nil {
		t.Fatalf("取消点赞失败: %v", err)
	}
	if !deleted {
		t.Error("首次取消点赞应返回 deleted=true")
	}
	if lc, pop := itestReload(t, db, v.ID); lc != 0 || pop != 0 {
		t.Fatalf("取消点赞后 likes_count=%d popularity=%d，期望 0/0", lc, pop)
	}

	// 第二次：没有行可删 => 幂等成功且不得再减。
	deleted, err = repo.UnlikeWithCounts(ctx, v.ID, 900003)
	if err != nil {
		t.Fatalf("重复取消点赞必须返回 nil 错误（幂等），实际: %v", err)
	}
	if deleted {
		t.Error("重复取消点赞应返回 deleted=false")
	}
	if lc, pop := itestReload(t, db, v.ID); lc != 0 || pop != 0 {
		t.Fatalf("重复取消点赞后计数异常: likes_count=%d popularity=%d，期望 0/0", lc, pop)
	}
}

// TestUnlikeWithCountsNeverGoesNegative 覆盖 GREATEST(x-1, 0) 兜底：
// 计数为 0 时仍能安全删除点赞行，不会减成负数。
func TestUnlikeWithCountsNeverGoesNegative(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewLikeRepository(db)
	ctx := context.Background()

	v := itestSeedVideo(t, db)
	// 人为把计数压到 0，制造"有行但计数为 0"的不一致状态。
	if err := db.Model(&video.Video{}).Where("id = ?", v.ID).
		Updates(map[string]any{"likes_count": 0, "popularity": 0}).Error; err != nil {
		t.Fatalf("重置计数失败: %v", err)
	}
	if err := db.Create(&video.Like{VideoID: v.ID, AccountID: 900004, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("插入点赞行失败: %v", err)
	}

	if _, err := repo.UnlikeWithCounts(ctx, v.ID, 900004); err != nil {
		t.Fatalf("取消点赞失败: %v", err)
	}
	if lc, pop := itestReload(t, db, v.ID); lc < 0 || pop < 0 {
		t.Fatalf("计数被减成负数: likes_count=%d popularity=%d", lc, pop)
	}
}

// TestLikeWithCountsMissingVideo 证明视频不存在时返回 ErrNotFound 且不写点赞行。
func TestLikeWithCountsMissingVideo(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewLikeRepository(db)
	ctx := context.Background()

	const missingID = 999999999
	like := &video.Like{VideoID: missingID, AccountID: 900005, CreatedAt: time.Now()}

	created, err := repo.LikeWithCounts(ctx, like)
	if !errors.Is(err, apierror.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际: %v", err)
	}
	if created {
		t.Error("视频不存在时不得报告 created=true")
	}

	var n int64
	db.Model(&video.Like{}).Where("video_id = ? AND account_id = ?", missingID, 900005).Count(&n)
	if n != 0 {
		t.Errorf("视频不存在时不应写入点赞行，实际有 %d 行", n)
	}
}
