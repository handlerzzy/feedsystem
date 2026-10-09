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

// newCache / hotScore / hotKeys 定义在 helpers_test.go。

const fixtureCommentID uint = 5

type commentFixture struct {
	store      *mock.MockCommentStore
	videos     *mock.MockVideoExistenceStore
	commentPub *mock.MockCommentPublisher
	popPub     *mock.MockPopularityPublisher
	cache      *rediscache.Client
	mr         *miniredis.Miniredis
	svc        *video.CommentService
}

func newCommentFixture(t *testing.T) *commentFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	cache, mr := newCache(t)
	f := &commentFixture{
		store:      mock.NewMockCommentStore(ctrl),
		videos:     mock.NewMockVideoExistenceStore(ctrl),
		commentPub: mock.NewMockCommentPublisher(ctrl),
		popPub:     mock.NewMockPopularityPublisher(ctrl),
		cache:      cache,
		mr:         mr,
	}
	f.svc = video.NewCommentService(f.store, f.videos, cache, f.commentPub, f.popPub)
	return f
}

// serviceWith 用同一批依赖换一组 MQ 发布器（用于 nil / typed-nil 场景）。
func (f *commentFixture) serviceWith(commentMQ video.CommentPublisher, popularity video.PopularityPublisher) *video.CommentService {
	return video.NewCommentService(f.store, f.videos, f.cache, commentMQ, popularity)
}

func commentRequest() *video.Comment {
	return &video.Comment{
		ID:       fixtureCommentID,
		VideoID:  fixtureVideoID,
		AuthorID: fixtureAccountID,
		Username: "alice",
		Content:  "nice video",
	}
}

// ---------- Publish ----------

func TestCommentServicePublish(t *testing.T) {
	t.Run("入参非法时直接报错且不查视频、不投递 MQ", func(t *testing.T) {
		cases := []struct {
			name    string
			comment *video.Comment
		}{
			{"comment 为 nil", nil},
			{"video_id 为 0", &video.Comment{VideoID: 0, AuthorID: fixtureAccountID, Content: "hi"}},
			{"author_id 为 0", &video.Comment{VideoID: fixtureVideoID, AuthorID: 0, Content: "hi"}},
			{"content 为空", &video.Comment{VideoID: fixtureVideoID, AuthorID: fixtureAccountID, Content: ""}},
			{"content 只有空白", &video.Comment{VideoID: fixtureVideoID, AuthorID: fixtureAccountID, Content: "   "}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newCommentFixture(t)
				// 未设置任何期望 => 校验失败时不得触碰 store/videoStore/MQ。

				if err := f.svc.Publish(context.Background(), tc.comment); err == nil {
					t.Fatal("expected error, got nil")
				}
			})
		}
	})

	t.Run("视频不存在时返回 404 且文案逐字不变", func(t *testing.T) {
		// 此前这里是 errors.New("video not found")：语义上是 404，
		// 但分类器不认识它，最终落成 500 并把原文回显。现在改成
		// apierror.NotFound("video not found")：状态码对了，对外文案不变。
		f := newCommentFixture(t)
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, nil)
		// 未设置 MQ / repo 期望 => 视频不存在就不能继续写。

		err := f.svc.Publish(context.Background(), commentRequest())
		if err == nil {
			t.Fatal("expected error for missing video, got nil")
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 404 {
			t.Errorf("HTTP status = %d, want 404", got)
		}
		if err.Error() != "video not found" {
			t.Errorf("error message = %q, want %q（对外文案不得改变）", err.Error(), "video not found")
		}
	})

	t.Run("视频存在性查询失败时原样透出错误", func(t *testing.T) {
		f := newCommentFixture(t)
		dbErr := errors.New("db down")
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, dbErr)

		if err := f.svc.Publish(context.Background(), commentRequest()); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("两条 MQ 都发布成功时短路，不写库也不碰 Redis", func(t *testing.T) {
		f := newCommentFixture(t)
		f.mr.Set(videoDetailKey, "cached")
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		// 未设置 PublishWithPopularity 期望 => MQ 成功就不得同步写库。

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.mr.Exists(videoDetailKey) {
			t.Error("两条 MQ 都成功时不应同步失效 detail 缓存")
		}
		if keys := hotKeys(f.mr); len(keys) != 0 {
			t.Errorf("两条 MQ 都成功时不应写热榜 ZSET，got %v", keys)
		}
	})

	t.Run("评论 MQ 发布失败时回退到同步写", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).
			Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		f.store.EXPECT().PublishWithPopularity(gomock.Any(), comment).Return(nil)

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("MQ 失败必须回退同步写而不是报错，got %v", err)
		}
	})

	t.Run("同步写失败时原样透出错误", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		dbErr := errors.New("insert failed")
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).
			Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		f.store.EXPECT().PublishWithPopularity(gomock.Any(), comment).Return(dbErr)

		if err := f.svc.Publish(context.Background(), comment); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("热度 MQ 失败时用 Redis 兜底", func(t *testing.T) {
		f := newCommentFixture(t)
		f.mr.Set(videoDetailKey, "cached")
		f.mr.Set("test:video:entity:42", "cached")
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(errors.New("mq down"))
		// 评论 MQ 成功 => 不写库；热度 MQ 失败 => 直接改 Redis。

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("热度 MQ 失败不得让发布失败，got %v", err)
		}
		if score, ok := hotScore(t, f.mr, fixtureVideoID); !ok || score != 1 {
			t.Errorf("热榜分数 = %v(ok=%v), want 1", score, ok)
		}
		for _, k := range []string{videoDetailKey, "test:video:entity:42"} {
			if f.mr.Exists(k) {
				t.Errorf("缓存 key %q 应被失效", k)
			}
		}
	})

	t.Run("Redis 兜底失败不影响发布成功", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(errors.New("mq down"))

		f.mr.Close() // Redis 立刻不可用

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("Redis 故障不得让评论发布失败（best-effort），got %v", err)
		}
	})

	t.Run("MQ 未启用时直接回退到同步写", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.store.EXPECT().PublishWithPopularity(gomock.Any(), comment).Return(nil)
		svc := f.serviceWith(nil, nil)

		if err := svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if score, ok := hotScore(t, f.mr, fixtureVideoID); !ok || score != 1 {
			t.Errorf("热榜分数 = %v(ok=%v), want 1", score, ok)
		}
	})

	t.Run("typed-nil 的具体 MQ 指针等价于 MQ 未启用", func(t *testing.T) {
		// router.go 在 MQ 初始化失败时传入的是 (*rabbitmq.CommentMQ)(nil)；
		// 装进接口后 != nil 为真，靠 CommentMQ 方法自带的 nil 接收者保护
		// 返回 error，从而仍然落到同步回退。
		f := newCommentFixture(t)
		comment := commentRequest()
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.store.EXPECT().PublishWithPopularity(gomock.Any(), comment).Return(nil)

		var commentMQ *rabbitmq.CommentMQ
		var popularityMQ *rabbitmq.PopularityMQ
		svc := f.serviceWith(commentMQ, popularityMQ)

		if err := svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("typed-nil MQ 必须回退同步写而不是 panic/报错，got %v", err)
		}
		if score, ok := hotScore(t, f.mr, fixtureVideoID); !ok || score != 1 {
			t.Errorf("热榜分数 = %v(ok=%v), want 1", score, ok)
		}
	})
}

// ---------- Publish: @提及 ----------

func TestCommentServicePublishMentions(t *testing.T) {
	t.Run("去重、跳过自己与不存在的用户，只给存在的他人写一条通知", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		comment.Content = "hi @bob @bob @alice @ghost @carol"

		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)

		// bob：存在 -> 只写一条（重复 @ 命中 seen 去重）
		f.store.EXPECT().AccountIDByUsername(gomock.Any(), "bob").Return(uint(9), nil)
		f.store.EXPECT().CreateMentionNotification(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, n *video.MentionNotification) error {
				if n.RecipientID != 9 {
					t.Errorf("RecipientID = %d, want 9", n.RecipientID)
				}
				if n.SenderID != fixtureAccountID {
					t.Errorf("SenderID = %d, want %d", n.SenderID, fixtureAccountID)
				}
				if n.Type != "mention" {
					t.Errorf("Type = %q, want \"mention\"", n.Type)
				}
				if n.TargetID != fixtureVideoID {
					t.Errorf("TargetID = %d, want %d", n.TargetID, fixtureVideoID)
				}
				if n.Content != "alice 在评论中提到了你" {
					t.Errorf("Content = %q", n.Content)
				}
				return nil
			})
		// ghost：查不到 (0, nil) -> 跳过
		f.store.EXPECT().AccountIDByUsername(gomock.Any(), "ghost").Return(uint(0), nil)
		// carol：查询报错 -> 跳过
		f.store.EXPECT().AccountIDByUsername(gomock.Any(), "carol").Return(uint(0), errors.New("db down"))
		// alice 是评论作者本人：不得查询、不得通知（未设期望）。

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("同步回退路径同样处理 @提及", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		comment.Content = "cc @bob"

		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).
			Return(errors.New("mq down"))
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		f.store.EXPECT().PublishWithPopularity(gomock.Any(), comment).Return(nil)
		f.store.EXPECT().AccountIDByUsername(gomock.Any(), "bob").Return(uint(9), nil)
		f.store.EXPECT().CreateMentionNotification(gomock.Any(), gomock.Any()).Return(nil)

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("通知写入失败不影响评论发布成功", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		comment.Content = "cc @bob"

		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		f.store.EXPECT().AccountIDByUsername(gomock.Any(), "bob").Return(uint(9), nil)
		f.store.EXPECT().CreateMentionNotification(gomock.Any(), gomock.Any()).Return(errors.New("write failed"))

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("通知写失败不得让评论发布失败，got %v", err)
		}
	})

	t.Run("没有 @ 时不查询账号也不写通知", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := commentRequest()
		comment.Content = "no mention here"

		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.commentPub.EXPECT().Publish(gomock.Any(), "alice", fixtureVideoID, fixtureAccountID, comment.Content).Return(nil)
		f.popPub.EXPECT().Update(gomock.Any(), fixtureVideoID, int64(1)).Return(nil)
		// 未设置 AccountIDByUsername 期望 => 无 @ 就不得查账号表。

		if err := f.svc.Publish(context.Background(), comment); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------- Delete ----------

func TestCommentServiceDelete(t *testing.T) {
	t.Run("评论不存在时返回 404 且文案逐字不变", func(t *testing.T) {
		f := newCommentFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(nil, nil)
		// 未设置 DeleteComment 期望 => 不存在就不能删。

		err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID)
		if err == nil {
			t.Fatal("expected error for missing comment, got nil")
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 404 {
			t.Errorf("HTTP status = %d, want 404", got)
		}
		if err.Error() != "comment not found" {
			t.Errorf("error message = %q, want %q（对外文案不得改变）", err.Error(), "comment not found")
		}
	})

	t.Run("GetByID 出错时原样透出", func(t *testing.T) {
		f := newCommentFixture(t)
		dbErr := errors.New("db down")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(nil, dbErr)

		if err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("非作者删除返回 ErrForbidden(403) 且不删库、不发 MQ", func(t *testing.T) {
		f := newCommentFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(&video.Comment{
			ID:       fixtureCommentID,
			VideoID:  fixtureVideoID,
			AuthorID: fixtureAccountID + 1, // 别人写的评论
		}, nil)
		// 未设置 DeleteComment / commentPub 期望 => 越权绝不能产生任何副作用。

		err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID)
		if !errors.Is(err, apierror.ErrForbidden) {
			t.Fatalf("expected apierror.ErrForbidden, got %v", err)
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 403 {
			t.Errorf("HTTP status = %d, want 403", got)
		}
	})

	t.Run("作者删除且评论 MQ 可用时走 MQ 且不直接删库", func(t *testing.T) {
		f := newCommentFixture(t)
		f.mr.Set(videoDetailKey, "cached")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(&video.Comment{
			ID:       fixtureCommentID,
			VideoID:  fixtureVideoID,
			AuthorID: fixtureAccountID,
		}, nil)
		f.commentPub.EXPECT().Delete(gomock.Any(), fixtureCommentID).Return(nil)
		// 未设置 DeleteComment 期望 => 交给 MQ 后不得再同步删库。

		if err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if score, ok := hotScore(t, f.mr, fixtureVideoID); !ok || score != -1 {
			t.Errorf("热榜分数 = %v(ok=%v), want -1", score, ok)
		}
		if f.mr.Exists(videoDetailKey) {
			t.Error("删除成功后必须失效 video:detail 缓存")
		}
	})

	t.Run("评论 MQ 失败时回退到直接删库", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := &video.Comment{ID: fixtureCommentID, VideoID: fixtureVideoID, AuthorID: fixtureAccountID}
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(comment, nil)
		f.commentPub.EXPECT().Delete(gomock.Any(), fixtureCommentID).Return(errors.New("mq down"))
		f.store.EXPECT().DeleteComment(gomock.Any(), comment).Return(nil)

		if err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); err != nil {
			t.Fatalf("MQ 失败必须回退删库而不是报错，got %v", err)
		}
		if score, ok := hotScore(t, f.mr, fixtureVideoID); !ok || score != -1 {
			t.Errorf("热榜分数 = %v(ok=%v), want -1", score, ok)
		}
	})

	t.Run("回退删库失败时原样透出错误", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := &video.Comment{ID: fixtureCommentID, VideoID: fixtureVideoID, AuthorID: fixtureAccountID}
		dbErr := errors.New("delete failed")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(comment, nil)
		f.commentPub.EXPECT().Delete(gomock.Any(), fixtureCommentID).Return(errors.New("mq down"))
		f.store.EXPECT().DeleteComment(gomock.Any(), comment).Return(dbErr)

		if err := f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("MQ 未启用时直接删库", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := &video.Comment{ID: fixtureCommentID, VideoID: fixtureVideoID, AuthorID: fixtureAccountID}
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(comment, nil)
		f.store.EXPECT().DeleteComment(gomock.Any(), comment).Return(nil)
		svc := f.serviceWith(nil, nil)

		if err := svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Redis 不可用时删除仍然成功（best-effort）", func(t *testing.T) {
		f := newCommentFixture(t)
		comment := &video.Comment{ID: fixtureCommentID, VideoID: fixtureVideoID, AuthorID: fixtureAccountID}
		f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(comment, nil)
		f.store.EXPECT().DeleteComment(gomock.Any(), comment).Return(nil)
		svc := f.serviceWith(nil, nil)

		f.mr.Close()

		if err := svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID); err != nil {
			t.Fatalf("Redis 故障不得让删除失败，got %v", err)
		}
	})
}

// ---------- GetAll ----------

func TestCommentServiceGetAll(t *testing.T) {
	t.Run("视频不存在时返回 404 且文案逐字不变", func(t *testing.T) {
		f := newCommentFixture(t)
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, nil)
		// 未设置 GetAllComments 期望 => 视频不存在就不能查评论。

		_, err := f.svc.GetAll(context.Background(), fixtureVideoID)
		if err == nil {
			t.Fatal("expected error for missing video, got nil")
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 404 {
			t.Errorf("HTTP status = %d, want 404", got)
		}
		if err.Error() != "video not found" {
			t.Errorf("error message = %q, want %q（对外文案不得改变）", err.Error(), "video not found")
		}
	})

	t.Run("视频存在性查询失败时原样透出", func(t *testing.T) {
		f := newCommentFixture(t)
		dbErr := errors.New("db down")
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, dbErr)

		if _, err := f.svc.GetAll(context.Background(), fixtureVideoID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("视频存在时透出评论列表", func(t *testing.T) {
		f := newCommentFixture(t)
		want := []video.Comment{
			{ID: 1, VideoID: fixtureVideoID, Content: "first"},
			{ID: 2, VideoID: fixtureVideoID, Content: "second"},
		}
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.store.EXPECT().GetAllComments(gomock.Any(), fixtureVideoID).Return(want, nil)

		got, err := f.svc.GetAll(context.Background(), fixtureVideoID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != len(want) || got[0].Content != "first" || got[1].Content != "second" {
			t.Errorf("GetAll = %+v, want %+v", got, want)
		}
	})

	t.Run("repo 出错时原样透出", func(t *testing.T) {
		f := newCommentFixture(t)
		dbErr := errors.New("db down")
		f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(true, nil)
		f.store.EXPECT().GetAllComments(gomock.Any(), fixtureVideoID).Return(nil, dbErr)

		if _, err := f.svc.GetAll(context.Background(), fixtureVideoID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})
}
