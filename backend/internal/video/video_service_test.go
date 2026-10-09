package video_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"
	"github.com/handlerzzy/feedsystem/internal/video/mock"

	miniredis "github.com/alicebob/miniredis/v2"
	"go.uber.org/mock/gomock"
	"gorm.io/gorm"
)

// newCache / hotScore / hotKeys 定义在 helpers_test.go。

type videoFixture struct {
	store  *mock.MockVideoStore
	popPub *mock.MockPopularityPublisher
	cache  *rediscache.Client
	mr     *miniredis.Miniredis
	svc    *video.VideoService
}

func newVideoFixture(t *testing.T) *videoFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	cache, mr := newCache(t)
	f := &videoFixture{
		store:  mock.NewMockVideoStore(ctrl),
		popPub: mock.NewMockPopularityPublisher(ctrl),
		cache:  cache,
		mr:     mr,
	}
	f.svc = video.NewVideoService(f.store, cache, f.popPub)
	return f
}

func validVideo() *video.Video {
	return &video.Video{
		Title:    "标题",
		PlayURL:  "https://cdn.example.com/play.mp4",
		CoverURL: "https://cdn.example.com/cover.jpg",
	}
}

const videoDetailKey = "test:video:detail:id=42"

// ---------- Publish ----------

func TestVideoServicePublish(t *testing.T) {
	t.Run("字段校验失败时返回 ErrValidation(400) 且不落库", func(t *testing.T) {
		cases := []struct {
			name string
			v    *video.Video
		}{
			{"video 为 nil", nil},
			{"title 为空", &video.Video{Title: "", PlayURL: "p", CoverURL: "c"}},
			{"title 只有空白", &video.Video{Title: "   ", PlayURL: "p", CoverURL: "c"}},
			{"play_url 为空", &video.Video{Title: "t", PlayURL: "", CoverURL: "c"}},
			{"play_url 只有空白", &video.Video{Title: "t", PlayURL: "  ", CoverURL: "c"}},
			{"cover_url 为空", &video.Video{Title: "t", PlayURL: "p", CoverURL: ""}},
			{"cover_url 只有空白", &video.Video{Title: "t", PlayURL: "p", CoverURL: " "}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newVideoFixture(t)
				// 未设置 PublishWithOutbox 期望 => 校验失败绝不能落库。

				err := f.svc.Publish(context.Background(), tc.v)
				if !errors.Is(err, apierror.ErrValidation) {
					t.Fatalf("expected apierror.ErrValidation, got %v", err)
				}
				if got := apierror.ClassifyHTTPStatus(err); got != 400 {
					t.Errorf("HTTP status = %d, want 400", got)
				}
			})
		}
	})

	t.Run("落库前去除首尾空白", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, v *video.Video) error {
				if v.Title != "标题" {
					t.Errorf("Title = %q, want 去除空白后的值", v.Title)
				}
				if v.PlayURL != "https://cdn.example.com/play.mp4" {
					t.Errorf("PlayURL = %q, want 去除空白后的值", v.PlayURL)
				}
				if v.CoverURL != "https://cdn.example.com/cover.jpg" {
					t.Errorf("CoverURL = %q, want 去除空白后的值", v.CoverURL)
				}
				return nil
			})

		err := f.svc.Publish(context.Background(), &video.Video{
			Title:    "  标题 ",
			PlayURL:  " https://cdn.example.com/play.mp4",
			CoverURL: "https://cdn.example.com/cover.jpg ",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("落库失败时原样透出错误", func(t *testing.T) {
		f := newVideoFixture(t)
		dbErr := errors.New("transaction failed")
		f.store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).Return(dbErr)

		if err := f.svc.Publish(context.Background(), validVideo()); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})
}

// ---------- Delete ----------

func TestVideoServiceDelete(t *testing.T) {
	t.Run("GetByID 返回 nil,nil 时返回 ErrNotFound(404) 且不执行删除", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(nil, nil)
		// 未设置 DeleteVideo 期望 => 不存在就不能删。

		err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID)
		if !errors.Is(err, apierror.ErrNotFound) {
			t.Fatalf("expected apierror.ErrNotFound, got %v", err)
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 404 {
			t.Errorf("HTTP status = %d, want 404", got)
		}
	})

	t.Run("repo 直接返回 record not found 时也映射为 404", func(t *testing.T) {
		// VideoRepository.GetByID 走 GORM First，视频不存在时返回的是
		// gorm.ErrRecordNotFound 而不是 apierror.ErrNotFound；
		// ClassifyHTTPStatus 必须把它也映射成 404，否则删除不存在的视频会变成 500。
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(nil, gorm.ErrRecordNotFound)

		err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID)
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 404 {
			t.Errorf("HTTP status = %d, want 404", got)
		}
	})

	t.Run("非作者删除返回 ErrForbidden(403) 且不执行删除", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:       fixtureVideoID,
			AuthorID: fixtureAccountID + 1,
		}, nil)
		// 未设置 DeleteVideo 期望 => 越权绝不能落库。

		err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID)
		if !errors.Is(err, apierror.ErrForbidden) {
			t.Fatalf("expected apierror.ErrForbidden, got %v", err)
		}
		if got := apierror.ClassifyHTTPStatus(err); got != 403 {
			t.Errorf("HTTP status = %d, want 403", got)
		}
	})

	t.Run("查询失败时原样透出错误且不执行删除", func(t *testing.T) {
		f := newVideoFixture(t)
		dbErr := errors.New("db down")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(nil, dbErr)

		if err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("作者本人删除成功并失效详情缓存", func(t *testing.T) {
		f := newVideoFixture(t)
		f.mr.Set(videoDetailKey, "stale")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:       fixtureVideoID,
			AuthorID: fixtureAccountID,
		}, nil)
		f.store.EXPECT().DeleteVideo(gomock.Any(), fixtureVideoID).Return(nil)

		if err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.mr.Exists(videoDetailKey) {
			t.Error("删除成功后必须失效 video:detail 缓存")
		}
	})

	t.Run("删除失败时原样透出错误", func(t *testing.T) {
		f := newVideoFixture(t)
		dbErr := errors.New("delete failed")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:       fixtureVideoID,
			AuthorID: fixtureAccountID,
		}, nil)
		f.store.EXPECT().DeleteVideo(gomock.Any(), fixtureVideoID).Return(dbErr)

		if err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	t.Run("缓存不可用时删除仍然成功（best-effort）", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:       fixtureVideoID,
			AuthorID: fixtureAccountID,
		}, nil)
		f.store.EXPECT().DeleteVideo(gomock.Any(), fixtureVideoID).Return(nil)

		f.mr.Close() // Redis 立刻不可用

		if err := f.svc.Delete(context.Background(), fixtureVideoID, fixtureAccountID); err != nil {
			t.Fatalf("缓存失效失败不得让删除失败，got %v", err)
		}
	})
}

// ---------- ListByAuthorID ----------

func TestVideoServiceListByAuthorID(t *testing.T) {
	t.Run("把作者 ID 转成 int64 传给 repo", func(t *testing.T) {
		f := newVideoFixture(t)
		want := []video.Video{{ID: 1, AuthorID: fixtureAccountID}}
		f.store.EXPECT().ListByAuthorID(gomock.Any(), int64(fixtureAccountID)).Return(want, nil)

		got, err := f.svc.ListByAuthorID(context.Background(), fixtureAccountID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ID != 1 {
			t.Errorf("ListByAuthorID = %+v, want %+v", got, want)
		}
	})

	t.Run("repo 出错时原样透出", func(t *testing.T) {
		f := newVideoFixture(t)
		dbErr := errors.New("db down")
		f.store.EXPECT().ListByAuthorID(gomock.Any(), int64(fixtureAccountID)).Return(nil, dbErr)

		if _, err := f.svc.ListByAuthorID(context.Background(), fixtureAccountID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
	})
}

// ---------- GetDetail ----------

func TestVideoServiceGetDetail(t *testing.T) {
	t.Run("缓存命中时直接返回且不回源", func(t *testing.T) {
		f := newVideoFixture(t)
		cached := &video.Video{ID: fixtureVideoID, Title: "cached-video", AuthorID: fixtureAccountID}
		b, err := json.Marshal(cached)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		f.mr.Set(videoDetailKey, string(b))
		// 未设置 GetByID 期望 => 缓存命中绝不能查库。

		got, err := f.svc.GetDetail(context.Background(), fixtureVideoID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.ID != fixtureVideoID || got.Title != "cached-video" {
			t.Fatalf("GetDetail = %+v, want the cached video", got)
		}
	})

	t.Run("缓存未命中时回源并回填缓存", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:       fixtureVideoID,
			Title:    "from-db",
			AuthorID: fixtureAccountID,
		}, nil)

		got, err := f.svc.GetDetail(context.Background(), fixtureVideoID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Title != "from-db" {
			t.Fatalf("GetDetail = %+v, want the db row", got)
		}

		raw, err := f.mr.Get(videoDetailKey)
		if err != nil {
			t.Fatalf("未命中后必须回填 video:detail 缓存: %v", err)
		}
		var backfilled video.Video
		if err := json.Unmarshal([]byte(raw), &backfilled); err != nil {
			t.Fatalf("缓存内容不是合法 JSON: %v", err)
		}
		if backfilled.ID != fixtureVideoID || backfilled.Title != "from-db" {
			t.Errorf("回填内容 = %+v, want the db row", backfilled)
		}
	})

	t.Run("回源失败时透出错误且不写缓存", func(t *testing.T) {
		f := newVideoFixture(t)
		dbErr := errors.New("db down")
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(nil, dbErr)

		if _, err := f.svc.GetDetail(context.Background(), fixtureVideoID); !errors.Is(err, dbErr) {
			t.Fatalf("expected original error, got %v", err)
		}
		if f.mr.Exists(videoDetailKey) {
			t.Error("回源失败时不得写缓存")
		}
	})

	t.Run("缓存不可用时仍然回源返回", func(t *testing.T) {
		f := newVideoFixture(t)
		f.store.EXPECT().GetByID(gomock.Any(), fixtureVideoID).Return(&video.Video{
			ID:    fixtureVideoID,
			Title: "from-db",
		}, nil)

		f.mr.Close() // Redis 立刻不可用

		got, err := f.svc.GetDetail(context.Background(), fixtureVideoID)
		if err != nil {
			t.Fatalf("Redis 故障不得让详情查询失败，got %v", err)
		}
		if got == nil || got.Title != "from-db" {
			t.Fatalf("GetDetail = %+v, want the db row", got)
		}
	})
}

// ---------- UpdateLikesCount ----------
