package video

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"go.uber.org/zap"
)

type VideoService struct {
	repo         VideoStore
	cache        *rediscache.Client
	cacheTTL     time.Duration
	popularityMQ PopularityPublisher
	// analysisMQ 是 P1 新增的 AI 分析任务投递器，为 nil 表示不投递
	// （AI 关闭、或 MQ 不可用）。用接口类型而不是具体类型：见 video_store.go。
	analysisMQ ContentAnalysisPublisher
}

func NewVideoService(repo VideoStore, cache *rediscache.Client, popularityMQ PopularityPublisher) *VideoService {
	return &VideoService{repo: repo, cache: cache, cacheTTL: 5 * time.Minute, popularityMQ: popularityMQ}
}

// WithContentAnalysis 注入 AI 分析投递器，返回自身以支持链式装配。
//
// 为什么用链式 setter 而不是改构造函数签名：构造函数已被大量单测与装配代码
// 使用，加参数会让"AI 关闭"这个默认状态变成每个调用点都要显式传 nil——
// 而默认状态恰恰应当是最省事的那条路径。装配层只在 AI 真的打开时才调用它。
func (vs *VideoService) WithContentAnalysis(p ContentAnalysisPublisher) *VideoService {
	vs.analysisMQ = p
	return vs
}

func (vs *VideoService) Publish(ctx context.Context, video *Video) error {
	if video == nil {
		return apierror.ErrValidation
	}
	video.Title = strings.TrimSpace(video.Title)
	video.PlayURL = strings.TrimSpace(video.PlayURL)
	video.CoverURL = strings.TrimSpace(video.CoverURL)

	if video.Title == "" {
		return apierror.ErrValidation
	}
	if video.PlayURL == "" {
		return apierror.ErrValidation
	}
	if video.CoverURL == "" {
		return apierror.ErrValidation
	}

	// 事务保证视频写入库和消息写入本地消息表的一致性。
	// 事务已下沉到 repo（见 VideoRepository.PublishWithOutbox）：
	// service 层不再持有 *gorm.DB。
	if err := vs.repo.PublishWithOutbox(ctx, video); err != nil {
		return err
	}

	// DB 写入**成功之后**才投递 AI 分析任务，best-effort。
	//
	// 为什么不走 outbox（本项目最重要的一条约束，改动前务必读
	// docs/AI-00-总览与全局约束.md 第 6.1 节）：
	// outbox 的 claim() 只按 status=pending 过滤、没有 event_type 过滤，
	// deliver() 会无条件把行当 timeline 事件投递并删除。
	// 往里写 AI 任务 = 被 timeline poller 抢走 -> 错误投递 -> 行被删除，
	// 任务永远丢失且全程无任何报错。
	//
	// 代价是 MQ 不可用时这次分析任务就丢了。这是正确的取舍：
	// 打标丢了可以事后批量补跑，视频本身完全正常；
	// 而 timeline 事件丢了，视频就永远不出现在任何人的时间线里。
	vs.requestAnalysis(ctx, video)
	return nil
}

// requestAnalysis 投递一次 AI 分析任务。任何失败都只记 Warn，不影响发布结果。
//
// 为什么失败只记 Warn 而不是 Error：AI 是可选功能，MQ 不可用时发布仍然成功，
// 系统处于"已按预期降级"的状态而非故障。用 Error 会让"AI 没打上标"
// 和"视频发布失败"在日志里同样刺眼，稀释真正需要响应的告警。
func (vs *VideoService) requestAnalysis(ctx context.Context, video *Video) {
	if vs.analysisMQ == nil || video == nil || video.ID == 0 {
		// 未装配（AI 关闭）或视频 ID 异常：静默跳过。
		// 这里**不能**记日志：AI 关闭是默认状态，每条发布都记一条就是刷屏。
		return
	}
	err := vs.analysisMQ.Request(ctx, rabbitmq.ContentAnalysisEvent{
		VideoID:      video.ID,
		AuthorID:     video.AuthorID,
		AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err == nil {
		return
	}
	// 投递失败：放弃本次分析（不降级直写、不重试到阻塞请求）。故为 Warn。
	logging.Ctx(ctx).Warn("AI 分析任务投递失败，本次打标跳过（不影响视频发布）",
		zap.Uint("video_id", video.ID), zap.Error(err))
}

func (vs *VideoService) Delete(ctx context.Context, id uint, authorID uint) error {
	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if video == nil {
		return apierror.ErrNotFound
	}
	if video.AuthorID != authorID {
		return apierror.ErrForbidden
	}
	if err := vs.repo.DeleteVideo(ctx, id); err != nil {
		return err
	}
	if vs.cache != nil {
		cacheKey := vs.cache.Key("video:detail:id=%d", id)
		_ = vs.cache.Del(context.Background(), cacheKey)
	}
	return nil
}

func (vs *VideoService) ListByAuthorID(ctx context.Context, authorID uint) ([]Video, error) {
	videos, err := vs.repo.ListByAuthorID(ctx, int64(authorID))
	if err != nil {
		return nil, err
	}
	return videos, nil
}

func (vs *VideoService) GetDetail(ctx context.Context, id uint) (*Video, error) {
	cacheKey := vs.cache.Key("video:detail:id=%d", id)

	getCached := func() (*Video, bool) {
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		b, err := vs.cache.GetBytes(opCtx, cacheKey)
		if err != nil {
			return nil, false
		}
		var cached Video
		if err := json.Unmarshal(b, &cached); err != nil {
			return nil, false
		}
		return &cached, true
	}

	setCached := func(video *Video) {
		b, err := json.Marshal(video)
		if err != nil {
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_ = vs.cache.SetBytes(opCtx, cacheKey, b, vs.cacheTTL)
	}

	if vs.cache != nil {
		if v, ok := getCached(); ok {
			return v, nil
		}

		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		b, err := vs.cache.GetBytes(opCtx, cacheKey)
		cancel()
		if err == nil {
			var cached Video
			if err := json.Unmarshal(b, &cached); err == nil {
				return &cached, nil
			}
		} else if rediscache.IsMiss(err) {
			lockKey := "lock:" + cacheKey

			lockCtx, lockCancel := context.WithTimeout(ctx, 50*time.Millisecond)
			token, locked, lockErr := vs.cache.Lock(lockCtx, lockKey, 2*time.Second)
			lockCancel()

			if lockErr == nil && locked {
				defer func() { _ = vs.cache.Unlock(context.Background(), lockKey, token) }()

				if v, ok := getCached(); ok {
					return v, nil
				}

				video, err := vs.repo.GetByID(ctx, id)
				if err != nil {
					return nil, err
				}
				setCached(video)
				return video, nil
			}

			// 没拿到锁：等待别人回填缓存
			for i := 0; i < 5; i++ {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
				if v, ok := getCached(); ok {
					return v, nil
				}
			}
		}
	}

	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if vs.cache != nil {
		setCached(video)
	}
	return video, nil
}
