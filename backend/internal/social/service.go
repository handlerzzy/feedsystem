package social

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"go.uber.org/zap"
)

type SocialService struct {
	repo        SocialStore
	accountrepo AccountLookup
	socialMQ    SocialPublisher
	cache       *rediscache.Client
}

func NewSocialService(repo SocialStore, accountrepo AccountLookup, socialMQ SocialPublisher, cache *rediscache.Client) *SocialService {
	return &SocialService{repo: repo, accountrepo: accountrepo, socialMQ: socialMQ, cache: cache}
}

func (s *SocialService) Follow(ctx context.Context, social *Social) error {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return err
	}
	if social.FollowerID == social.VloggerID {
		// 带码错误：文案与改造前逐字一致（"can not follow self"），
		// 但分类器据此给出 400 而不是 500。
		return apierror.BadRequest("can not follow self")
	}
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if isFollowed {
		// 关系已存在是状态冲突（409），文案保持不变。
		return apierror.Conflict("already followed")
	}

	// 先写 DB，确保数据持久化
	if err := s.repo.Follow(ctx, social); err != nil {
		return err
	}

	// DB 成功后，失效该用户的关注列表缓存
	s.invalidateFollowingFeedCache(context.Background(), social.FollowerID)

	// 最后发 MQ（用于通知），失败只记日志不影响业务
	if s.socialMQ != nil {
		if err := s.socialMQ.Follow(ctx, social.FollowerID, social.VloggerID); err != nil {
			// MQ 只用于通知，发布失败不影响关注结果，故为 Warn。
			logging.Ctx(ctx).Warn("social MQ Follow 发布失败",
				zap.Uint("follower_id", social.FollowerID),
				zap.Uint("vlogger_id", social.VloggerID),
				zap.Error(err))
		}
	}
	return nil
}

func (s *SocialService) Unfollow(ctx context.Context, social *Social) error {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return err
	}
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if !isFollowed {
		// 与"重复关注"对称：关系不存在同样是状态冲突（409），文案保持不变。
		return apierror.Conflict("not followed")
	}

	// 先写 DB
	if err := s.repo.Unfollow(ctx, social); err != nil {
		return err
	}

	// 失效缓存
	s.invalidateFollowingFeedCache(context.Background(), social.FollowerID)

	// 最后发 MQ
	if s.socialMQ != nil {
		if err := s.socialMQ.UnFollow(ctx, social.FollowerID, social.VloggerID); err != nil {
			// 同 Follow：只影响通知，故为 Warn。
			logging.Ctx(ctx).Warn("social MQ UnFollow 发布失败",
				zap.Uint("follower_id", social.FollowerID),
				zap.Uint("vlogger_id", social.VloggerID),
				zap.Error(err))
		}
	}
	return nil
}

func (s *SocialService) invalidateFollowingFeedCache(ctx context.Context, accountID uint) {
	if s.cache == nil {
		return
	}
	pattern := s.cache.Key("feed:listByFollowing:*:accountID=%d:*", accountID)
	if err := s.cache.DelByPattern(ctx, pattern); err != nil {
		// best-effort 失效：删不掉只会让关注流多缓存一会儿（TTL 兜底），故为 Warn。
		// 注意：两个调用点传的是 context.Background()，故此处拿不到 request_id。
		logging.Ctx(ctx).Warn("失效 Following 缓存失败",
			zap.Uint("account_id", accountID), zap.Error(err))
	}
}

func (s *SocialService) GetAllFollowers(ctx context.Context, VloggerID uint) ([]*account.PublicAccount, error) {
	_, err := s.accountrepo.FindByID(ctx, VloggerID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAllFollowers(ctx, VloggerID)
}

func (s *SocialService) GetAllVloggers(ctx context.Context, FollowerID uint) ([]*account.PublicAccount, error) {
	_, err := s.accountrepo.FindByID(ctx, FollowerID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAllVloggers(ctx, FollowerID)
}

func (s *SocialService) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	return s.repo.CountFollowers(ctx, vloggerID)
}

func (s *SocialService) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	return s.repo.CountVloggers(ctx, followerID)
}

func (s *SocialService) IsFollowed(ctx context.Context, social *Social) (bool, error) {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return false, err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return false, err
	}
	return s.repo.IsFollowed(ctx, social)
}
