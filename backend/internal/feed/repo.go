package feed

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/social"
	"github.com/handlerzzy/feedsystem/internal/video"
	"time"

	"gorm.io/gorm"
)

type FeedRepository struct {
	db *gorm.DB
}

func NewFeedRepository(db *gorm.DB) *FeedRepository {
	return &FeedRepository{db: db}
}

func (repo *FeedRepository) ListLatest(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("create_time DESC")
	if !latestBefore.IsZero() {
		query = query.Where("create_time < ?", latestBefore)
	}
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (repo *FeedRepository) ListLikesCountWithCursor(ctx context.Context, limit int, cursor *LikesCountCursor) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("likes_count DESC, id DESC")

	if cursor != nil {
		query = query.Where(
			"(likes_count < ?) OR (likes_count = ? AND id < ?)",
			cursor.LikesCount,
			cursor.LikesCount, cursor.ID,
		)
	}

	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (repo *FeedRepository) ListByFollowing(ctx context.Context, limit int, viewerAccountID uint, latestBefore time.Time) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("create_time DESC")
	if viewerAccountID > 0 {
		followingSubQuery := repo.db.WithContext(ctx).
			Model(&social.Social{}).
			Select("vlogger_id").
			Where("follower_id = ?", viewerAccountID)
		query = query.Where("author_id IN (?)", followingSubQuery)
	}
	if !latestBefore.IsZero() {
		query = query.Where("create_time < ?", latestBefore)
	}
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (repo *FeedRepository) ListByPopularity(ctx context.Context, limit int, popularityBefore int64, timeBefore time.Time, idBefore uint) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("popularity DESC, create_time DESC, id DESC")

	// 只有当游标完整提供时才加过滤（popularity 允许为 0）
	if !timeBefore.IsZero() && idBefore > 0 {
		query = query.Where(
			"(popularity < ?) OR (popularity = ? AND create_time < ?) OR (popularity = ? AND create_time = ? AND id < ?)",
			popularityBefore,
			popularityBefore, timeBefore,
			popularityBefore, timeBefore, idBefore,
		)
	}

	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (repo *FeedRepository) GetByIDs(ctx context.Context, ids []uint) ([]*video.Video, error) {
	var videos []*video.Video
	if len(ids) == 0 {
		return videos, nil
	}
	if err := repo.db.WithContext(ctx).Model(&video.Video{}).
		Where("id IN ?", ids).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (repo *FeedRepository) ListByTag(ctx context.Context, tagName string, limit int) ([]*video.Video, error) {
	var videos []*video.Video
	err := repo.db.WithContext(ctx).Model(&video.Video{}).Table("videos").
		Joins("JOIN video_tags ON video_tags.video_id = videos.id").
		Joins("JOIN tags ON tags.id = video_tags.tag_id").
		Where("tags.name = ?", tagName).
		Order("videos.create_time desc").
		Limit(limit).
		Find(&videos).Error
	return videos, err
}

// ListFreshLowEngagement 返回"新且冷"的视频（P2 冷启动探索通道的数据来源）。
//
// 判据三条，每一条都对应一种"新内容永远没人看得到"的成因：
//
//  1. **够新**：create_time >= now - freshness。发布久了还没互动的内容
//     不是"新内容曝光不足"，而是"内容本身没吸引力"，给它探索位是浪费。
//  2. **互动少**：popularity < maxPopularity。popularity 在点赞与评论时
//     都会自增（见 LikeRepository.LikeWithCounts），因此它比 likes_count
//     更接近"有多少人真的参与过"。
//  3. **不是自己发的**：author_id <> excludeAuthorID。推荐流不会把自己发
//     的内容推给自己——那不是隐私问题而是产品问题（首屏出现自己的视频
//     会让"推荐"立刻露馅）。
//
// 为什么用 `now` 而不是"最新一条视频的时间"当基准：**跨时区写入的脏数据**
// 会让"最新一条"落在未来（本机开发库里就有这样一行），于是时间窗整个
// 漂到未来、一条都选不出来，而现象是"探索配额开了但没有任何效果"。
// 用服务器当前时间，最坏情况只是漏掉一条脏数据。
//
// ORDER BY create_time DESC, id DESC：与 ListLatest 的次序一致，
// 于是"更新的内容排在前面"这条直觉在两条路里都成立。
func (repo *FeedRepository) ListFreshLowEngagement(ctx context.Context, limit int, freshness time.Duration, maxPopularity int64, excludeAuthorID uint) ([]*video.Video, error) {
	var videos []*video.Video
	if limit <= 0 {
		return videos, nil
	}
	since := time.Now().Add(-freshness)
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Where("create_time >= ?", since).
		Where("popularity < ?", maxPopularity).
		Order("create_time DESC, id DESC")
	if excludeAuthorID > 0 {
		query = query.Where("author_id <> ?", excludeAuthorID)
	}
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}
