package video_test

import (
	"context"
	"errors"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"
	"github.com/handlerzzy/feedsystem/internal/video/mock"

	miniredis "github.com/alicebob/miniredis/v2"
	"go.uber.org/mock/gomock"
)

// newCache 定义在 helpers_test.go，与 internal/account 的单测保持一致。

// likeFixture 组装一套全部由 mock 驱动的 LikeService。
// 不传任何真实数据库或消息队列。
type likeFixture struct {
	store   *mock.MockLikeStore
	videos  *mock.MockVideoExistenceStore
	likePub *mock.MockLikePublisher
	popPub  *mock.MockPopularityPublisher
	cache   *rediscache.Client
	mr      *miniredis.Miniredis
	svc     *video.LikeService
}

func newLikeFixture(t *testing.T) *likeFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	cache, mr := newCache(t)
	f := &likeFixture{
		store:   mock.NewMockLikeStore(ctrl),
		videos:  mock.NewMockVideoExistenceStore(ctrl),
		likePub: mock.NewMockLikePublisher(ctrl),
		popPub:  mock.NewMockPopularityPublisher(ctrl),
		cache:   cache,
		mr:      mr,
	}
	f.svc = video.NewLikeService(f.store, f.videos, cache, f.likePub, f.popPub)
	return f
}

func (f *likeFixture) videoExists() {
	f.videos.EXPECT().IsExist(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
}

// ---------- Like ----------

func TestLikeRejectsInvalidInput(t *testing.T) {
	ctrl := gomock.NewController(t)
	svc := video.NewLikeService(mock.NewMockLikeStore(ctrl), nil, nil, nil, nil)

	if err := svc.Like(context.Background(), nil); err == nil {
		t.Error("expected error for nil like")
	}
	// VideoID/AccountID 为 0 时必须在触碰任何依赖前就返回。
	if err := svc.Like(context.Background(), &video.Like{VideoID: 0, AccountID: 1}); err == nil {
		t.Error("expected error for zero video_id")
	}
	if err := svc.Like(context.Background(), &video.Like{VideoID: 1, AccountID: 0}); err == nil {
		t.Error("expected error for zero account_id")
	}
}

func TestLikeReturnsNotFoundForMissingVideo(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockLikeStore(ctrl)
	videos := mock.NewMockVideoExistenceStore(ctrl)
	svc := video.NewLikeService(store, videos, nil, nil, nil)

	videos.EXPECT().IsExist(gomock.Any(), uint(42)).Return(false, nil)
	// 未设置 IsLiked 期望 => 视频不存在时不得继续往下走。

	err := svc.Like(context.Background(), &video.Like{VideoID: 42, AccountID: 1})
	if !errors.Is(err, apierror.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLikeIsIdempotentWhenAlreadyLiked(t *testing.T) {
	f := newLikeFixture(t)
	f.videoExists()
	f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(true, nil)
	// 已点赞 => MQ 与回退写都不得发生（未设期望即代表"被调用则失败"）。

	if err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("repeat like must be silently idempotent, got %v", err)
	}
}

func TestLikeShortCircuitsWhenBothMQPublishSucceed(t *testing.T) {
	f := newLikeFixture(t)
	f.videoExists()
	f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
	f.likePub.EXPECT().Like(gomock.Any(), uint(1), uint(7)).Return(nil)
	f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(1)).Return(nil)
	// 两条 MQ 都成功 => 不得触碰数据库回退，也不得改 Redis。
	// 这正是异步路径的收益所在。

	if err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLikeFallsBackToDatabaseWhenLikeMQFails(t *testing.T) {
	t.Run("并发下点赞行已存在时视为幂等成功", func(t *testing.T) {
		f := newLikeFixture(t)
		f.videoExists()
		f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
		f.likePub.EXPECT().Like(gomock.Any(), uint(1), uint(7)).Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(1)).Return(nil)
		// repo 告知"行已存在"（created=false, err=nil）：这是幂等成功，不是失败。
		// 第三批加的 errSkipCountUpdate 哨兵就是为了让这种情况不报错。
		f.store.EXPECT().LikeWithCounts(gomock.Any(), gomock.Any()).Return(false, nil)

		if err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
			t.Fatalf("duplicate row must be treated as idempotent success, got %v", err)
		}
	})

	t.Run("真实数据库错误必须透出", func(t *testing.T) {
		f := newLikeFixture(t)
		f.videoExists()
		f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
		f.likePub.EXPECT().Like(gomock.Any(), uint(1), uint(7)).Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(1)).Return(nil)

		dbErr := errors.New("connection refused")
		f.store.EXPECT().LikeWithCounts(gomock.Any(), gomock.Any()).Return(false, dbErr)

		err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1})
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected the db error to surface, got %v", err)
		}
	})

	t.Run("视频不存在时透出 ErrNotFound", func(t *testing.T) {
		f := newLikeFixture(t)
		f.videoExists()
		f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
		f.likePub.EXPECT().Like(gomock.Any(), uint(1), uint(7)).Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(1)).Return(nil)
		f.store.EXPECT().LikeWithCounts(gomock.Any(), gomock.Any()).Return(false, apierror.ErrNotFound)

		err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1})
		if !errors.Is(err, apierror.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestLikeFallsBackToRedisOnlyWhenPopularityMQFails(t *testing.T) {
	f := newLikeFixture(t)
	f.videoExists()
	f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
	// like MQ 成功、popularity MQ 失败 => 走 Redis 兜底，但不该再写数据库。
	f.likePub.EXPECT().Like(gomock.Any(), uint(1), uint(7)).Return(nil)
	f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(1)).Return(errors.New("mq down"))

	// 预置 detail 缓存，验证兜底会把它失效掉。
	f.mr.Set("test:video:detail:id=7", "stale")

	if err := f.svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.mr.Exists("test:video:detail:id=7") {
		t.Error("popularity fallback should invalidate the video detail cache")
	}
}

// TestLikeWithTypedNilPublisherStillFallsBack 锁住一个容易被接口化破坏的性质。
//
// 背景：一旦构造函数参数从具体指针改成接口，传入 (*rabbitmq.LikeMQ)(nil)
// 得到的就是"装着空指针的接口"，`!= nil` 为真，于是 `if s.likeMQ != nil`
// 成立并调用方法。当前之所以不出事，只因 LikeMQ.Like 内部的 publish 自带
// nil 接收者保护并返回 error，使 mysqlEnqueued 保持 false、仍走同步回退。
//
// internal/http/router.go 现已改为用接口类型变量声明，初始化失败时留下真正的
// nil 接口，因此生产路径不再产生 typed-nil。本用例是**纵深防御**：即使将来
// 有人改回旧写法、或从别处传进 typed-nil，也必须继续安全降级而不是 panic
// 或静默丢掉写路径。
func TestLikeWithTypedNilPublisherStillFallsBack(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockLikeStore(ctrl)
	videos := mock.NewMockVideoExistenceStore(ctrl)
	videos.EXPECT().IsExist(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)

	var nilLikeMQ *rabbitmq.LikeMQ
	var nilPopMQ *rabbitmq.PopularityMQ
	svc := video.NewLikeService(store, videos, nil, nilLikeMQ, nilPopMQ)

	// typed-nil 仍会走到同步回退：这是必须保住的行为。
	store.EXPECT().LikeWithCounts(gomock.Any(), gomock.Any()).Return(true, nil)

	if err := svc.Like(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("typed-nil MQ must degrade to the synchronous path, got %v", err)
	}
}

// ---------- Unlike ----------

func TestUnlikeIsIdempotentWhenNotLiked(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockLikeStore(ctrl)
	videos := mock.NewMockVideoExistenceStore(ctrl)
	svc := video.NewLikeService(store, videos, nil, nil, nil)

	videos.EXPECT().IsExist(gomock.Any(), uint(7)).Return(true, nil)
	store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, nil)
	// 未点赞 => 不得发 MQ、不得写库。

	if err := svc.Unlike(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("repeat unlike must be silently idempotent, got %v", err)
	}
}

func TestUnlikeRejectsMissingVideo(t *testing.T) {
	ctrl := gomock.NewController(t)
	videos := mock.NewMockVideoExistenceStore(ctrl)
	svc := video.NewLikeService(mock.NewMockLikeStore(ctrl), videos, nil, nil, nil)

	videos.EXPECT().IsExist(gomock.Any(), uint(42)).Return(false, nil)

	err := svc.Unlike(context.Background(), &video.Like{VideoID: 42, AccountID: 1})
	if !errors.Is(err, apierror.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUnlikeShortCircuitsWhenBothMQPublishSucceed(t *testing.T) {
	f := newLikeFixture(t)
	f.videoExists()
	f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(true, nil)
	f.likePub.EXPECT().Unlike(gomock.Any(), uint(1), uint(7)).Return(nil)
	f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(-1)).Return(nil)

	if err := f.svc.Unlike(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnlikeFallsBackToDatabaseWhenLikeMQFails(t *testing.T) {
	t.Run("并发下已被删除时视为幂等成功", func(t *testing.T) {
		f := newLikeFixture(t)
		f.videoExists()
		f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(true, nil)
		f.likePub.EXPECT().Unlike(gomock.Any(), uint(1), uint(7)).Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(-1)).Return(nil)
		// deleted=false：行的确已经没了，目标状态达成，不该报错。
		f.store.EXPECT().UnlikeWithCounts(gomock.Any(), uint(7), uint(1)).Return(false, nil)

		if err := f.svc.Unlike(context.Background(), &video.Like{VideoID: 7, AccountID: 1}); err != nil {
			t.Fatalf("already-deleted row must be idempotent success, got %v", err)
		}
	})

	t.Run("数据库错误必须透出", func(t *testing.T) {
		f := newLikeFixture(t)
		f.videoExists()
		f.store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(true, nil)
		f.likePub.EXPECT().Unlike(gomock.Any(), uint(1), uint(7)).Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), uint(7), int64(-1)).Return(nil)

		dbErr := errors.New("deadlock")
		f.store.EXPECT().UnlikeWithCounts(gomock.Any(), uint(7), uint(1)).Return(false, dbErr)

		err := f.svc.Unlike(context.Background(), &video.Like{VideoID: 7, AccountID: 1})
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected the db error to surface, got %v", err)
		}
	})
}

// ---------- 委托方法 ----------

func TestLikeServiceDelegatesReads(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockLikeStore(ctrl)
	svc := video.NewLikeService(store, nil, nil, nil, nil)

	store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(true, nil)
	liked, err := svc.IsLiked(context.Background(), 7, 1)
	if err != nil || !liked {
		t.Fatalf("IsLiked = (%v, %v), want (true, nil)", liked, err)
	}

	want := []video.Video{{ID: 3}, {ID: 5}}
	store.EXPECT().ListLikedVideos(gomock.Any(), uint(1)).Return(want, nil)
	got, err := svc.ListLikedVideos(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListLikedVideos returned %d items, want %d", len(got), len(want))
	}
}
