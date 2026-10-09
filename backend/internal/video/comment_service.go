package video

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

type CommentService struct {
	repo            CommentStore
	VideoRepository VideoExistenceStore
	cache           *rediscache.Client
	commentMQ       CommentPublisher
	popularityMQ    PopularityPublisher
}

func NewCommentService(repo CommentStore, videoRepo VideoExistenceStore, cache *rediscache.Client, commentMQ CommentPublisher, popularityMQ PopularityPublisher) *CommentService {
	return &CommentService{repo: repo, VideoRepository: videoRepo, cache: cache, commentMQ: commentMQ, popularityMQ: popularityMQ}
}

func (s *CommentService) Publish(ctx context.Context, comment *Comment) error {
	if comment == nil {
		return apierror.BadRequest("comment is nil")
	}
	comment.Username = strings.TrimSpace(comment.Username)
	comment.Content = strings.TrimSpace(comment.Content)
	if comment.VideoID == 0 || comment.AuthorID == 0 {
		return apierror.BadRequest("video_id and author_id are required")
	}
	if comment.Content == "" {
		return apierror.BadRequest("content is required")
	}

	exists, err := s.VideoRepository.IsExist(ctx, comment.VideoID)
	if err != nil {
		return err
	}
	if !exists {
		return apierror.NotFound("video not found")
	}

	mysqlEnqueued := false
	redisEnqueued := false
	if s.commentMQ != nil {
		if err := s.commentMQ.Publish(ctx, comment.Username, comment.VideoID, comment.AuthorID, comment.Content); err == nil {
			mysqlEnqueued = true
		}
	}
	if s.popularityMQ != nil {
		if err := s.popularityMQ.Update(ctx, comment.VideoID, 1); err == nil {
			redisEnqueued = true
		}
	}
	if mysqlEnqueued && redisEnqueued {
		s.notifyMentions(ctx, comment)
		return nil
	}

	// Fallback: direct MySQL write when comment MQ publish fails.
	// 事务已下沉到 repo（见 PublishWithPopularity）。
	if !mysqlEnqueued {
		if err := s.repo.PublishWithPopularity(ctx, comment); err != nil {
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	if !redisEnqueued {
		UpdatePopularityCache(ctx, s.cache, comment.VideoID, 1)
	}
	s.notifyMentions(ctx, comment)
	return nil
}

func (s *CommentService) Delete(ctx context.Context, commentID uint, accountID uint) error {
	comment, err := s.repo.GetByID(ctx, commentID)
	if err != nil {
		return err
	}
	if comment == nil {
		return apierror.NotFound("comment not found")
	}
	if comment.AuthorID != accountID {
		return apierror.ErrForbidden
	}

	// 记录 videoID，供删除成功后更新热度使用
	videoID := comment.VideoID

	if s.commentMQ != nil {
		if err := s.commentMQ.Delete(ctx, commentID); err == nil {
			// 与 applyPublish 的 ChangePopularity(+1) 对称：删除时热度 -1，
			// 并失效 video:detail / video:entity 缓存
			UpdatePopularityCache(ctx, s.cache, videoID, -1)
			return nil
		}
	}
	if err := s.repo.DeleteComment(ctx, comment); err != nil {
		return err
	}
	UpdatePopularityCache(ctx, s.cache, videoID, -1)
	return nil
}

func (s *CommentService) GetAll(ctx context.Context, videoID uint) ([]Comment, error) {
	exists, err := s.VideoRepository.IsExist(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, apierror.NotFound("video not found")
	}
	return s.repo.GetAllComments(ctx, videoID)
}

var mentionRegex = regexp.MustCompile(`@(\w+)`)

func (s *CommentService) notifyMentions(ctx context.Context, comment *Comment) {
	matches := mentionRegex.FindAllStringSubmatch(comment.Content, -1)
	if len(matches) == 0 {
		return
	}
	seen := make(map[string]bool)
	for _, m := range matches {
		username := m[1]
		if seen[username] || username == comment.Username {
			continue
		}
		seen[username] = true
		accID, err := s.repo.AccountIDByUsername(ctx, username)
		if err != nil || accID == 0 {
			continue
		}
		notif := &MentionNotification{
			RecipientID: accID,
			SenderID:    comment.AuthorID,
			Type:        "mention",
			TargetID:    comment.VideoID,
			Content:     comment.Username + " 在评论中提到了你",
		}
		if err := s.repo.CreateMentionNotification(ctx, notif); err != nil {
			// @ 通知是评论的附属产物：评论已落库，调用方随后仍 return nil，故为 Warn。
			logging.Ctx(ctx).Warn("create mention notification failed",
				zap.Uint("recipient_id", accID),
				zap.Uint("sender_id", comment.AuthorID),
				zap.Uint("video_id", comment.VideoID),
				zap.Error(err))
		}
	}
}
