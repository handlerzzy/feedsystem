package video

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"time"
)

type LikeService struct {
	repo         LikeStore
	VideoRepo    VideoExistenceStore
	cache        *rediscache.Client
	likeMQ       LikePublisher
	popularityMQ PopularityPublisher
}

func NewLikeService(repo LikeStore, videoRepo VideoExistenceStore, cache *rediscache.Client, likeMQ LikePublisher, popularityMQ PopularityPublisher) *LikeService {
	return &LikeService{repo: repo, VideoRepo: videoRepo, cache: cache, likeMQ: likeMQ, popularityMQ: popularityMQ}
}

func (s *LikeService) Like(ctx context.Context, like *Like) error {
	if like == nil {
		return apierror.BadRequest("like is nil")
	}
	if like.VideoID == 0 || like.AccountID == 0 {
		return apierror.BadRequest("video_id and account_id are required")
	}

	if s.VideoRepo != nil {
		ok, err := s.VideoRepo.IsExist(ctx, like.VideoID)
		if err != nil {
			return err
		}
		if !ok {
			return apierror.ErrNotFound
		}
	}

	isLiked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		return err
	}
	if isLiked {
		// 已点赞：幂等返回成功，不再报错。
		// 说明：点赞是 MQ 异步落库，IsLiked 读的是最终一致的 DB 快照。
		// 快速连点时最坏会重复投递一条 like.like，但 worker 侧已经是幂等的：
		//   likeworker.applyLike   → LikeIgnoreDuplicate 返回 created=false 时
		//                            提前 return，不会重复 +likes_count/+popularity
		//   likeworker.applyUnlike → DeleteByVideoAndAccount 返回 deleted=false 时
		//                            提前 return，不会重复 -likes_count/-popularity
		// 因此本项改动不会造成计数漂移，无需额外去重。
		return nil
	}

	like.CreatedAt = time.Now()
	mysqlEnqueued := false
	redisEnqueued := false
	if s.likeMQ != nil {
		if err := s.likeMQ.Like(ctx, like.AccountID, like.VideoID); err == nil {
			mysqlEnqueued = true
		}
	}
	if s.popularityMQ != nil {
		if err := s.popularityMQ.Update(ctx, like.VideoID, 1); err == nil {
			redisEnqueued = true
		}
	}
	if mysqlEnqueued && redisEnqueued {
		return nil
	}

	// Fallback: direct MySQL write when like MQ publish fails.
	// 事务与幂等判定都在 repo 内（见 LikeWithCounts）：返回 (false, nil)
	// 表示并发下点赞行已存在，属于幂等成功，不是错误。
	if !mysqlEnqueued {
		if _, err := s.repo.LikeWithCounts(ctx, like); err != nil {
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	if !redisEnqueued {
		UpdatePopularityCache(ctx, s.cache, like.VideoID, 1)
	}
	return nil
}

func (s *LikeService) Unlike(ctx context.Context, like *Like) error {
	if like == nil {
		return apierror.BadRequest("like is nil")
	}
	if like.VideoID == 0 || like.AccountID == 0 {
		return apierror.BadRequest("video_id and account_id are required")
	}

	if s.VideoRepo != nil {
		ok, err := s.VideoRepo.IsExist(ctx, like.VideoID)
		if err != nil {
			return err
		}
		if !ok {
			return apierror.ErrNotFound
		}
	}

	isLiked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		return err
	}
	if !isLiked {
		// 未点赞：幂等返回成功（客户端重试/双击不该报错）
		return nil
	}

	mysqlEnqueued := false
	redisEnqueued := false
	if s.likeMQ != nil {
		if err := s.likeMQ.Unlike(ctx, like.AccountID, like.VideoID); err == nil {
			mysqlEnqueued = true
		}
	}
	if s.popularityMQ != nil {
		if err := s.popularityMQ.Update(ctx, like.VideoID, -1); err == nil {
			redisEnqueued = true
		}
	}
	if mysqlEnqueued && redisEnqueued {
		return nil
	}

	// Fallback: direct MySQL write when like MQ publish fails.
	// 返回 (false, nil) 表示并发下点赞行已被删除，属于幂等成功。
	if !mysqlEnqueued {
		if _, err := s.repo.UnlikeWithCounts(ctx, like.VideoID, like.AccountID); err != nil {
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	if !redisEnqueued {
		UpdatePopularityCache(ctx, s.cache, like.VideoID, -1)
	}
	return nil
}

func (s *LikeService) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	return s.repo.IsLiked(ctx, videoID, accountID)
}

func (s *LikeService) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	return s.repo.ListLikedVideos(ctx, accountID)
}
