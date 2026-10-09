package feed_test

// 本文件只覆盖“能证明业务不变量”的场景，不追求行覆盖率。
// 被证明的不变量：
//
//	1. 分页边界：空结果、游标恰好落在冷热边界、最后一页（不足 limit）、
//	   游标边界处的数据不会在下一页重复出现。
//	2. 缓存层级：L1 命中不再下沉；L2 命中回填 L1；三层全 miss 才落 L3 并回填 L2；
//	   损坏的 L2 数据视为未命中。
//	3. 冷热分离：冷数据只查库且不污染 ZSET；热数据不足时用冷数据补齐，
//	   交给冷查询的游标严格早于最后一条热数据（因此不会重复返回同一条内容）。
//	4. 降级：Redis 故障时读路径必须整体回落 MySQL，而不是返回错误。
//
// 所有外部依赖（MySQL / Redis）都被替换：
//   - MySQL：mockgen 生成的 MockFeedStore / MockLikeLookup；
//   - Redis：miniredis 上的真实 go-redis 客户端（rediscache.NewClient(rdb, "test:")）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/feed"
	"github.com/handlerzzy/feedsystem/internal/feed/mock"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/video"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

// ---------- 测试脚手架 ----------

const redisPrefix = "test:"

type fixture struct {
	store *mock.MockFeedStore
	likes *mock.MockLikeLookup
	mr    *miniredis.Miniredis
	svc   *feed.FeedService
}

// newFixture 返回一个 MySQL 与点赞仓库全部被 mock 的 FeedService，
// Redis 换成 miniredis（前缀 test:，与 account 包测试保持一致）。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := mock.NewMockFeedStore(ctrl)
	likes := mock.NewMockLikeLookup(ctrl)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := rediscache.NewClient(rdb, redisPrefix)

	return &fixture{
		store: store,
		likes: likes,
		mr:    mr,
		svc:   feed.NewFeedService(store, likes, cache),
	}
}

// newFixtureWithoutCache 模拟未配置 Redis 的部署（rediscache == nil）。
func newFixtureWithoutCache(t *testing.T) *fixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := mock.NewMockFeedStore(ctrl)
	likes := mock.NewMockLikeLookup(ctrl)
	return &fixture{
		store: store,
		likes: likes,
		svc:   feed.NewFeedService(store, likes, nil),
	}
}

func mkVideo(id uint, createTime time.Time) *video.Video {
	return &video.Video{
		ID:          id,
		AuthorID:    100 + id,
		Username:    fmt.Sprintf("user%d", id),
		Title:       fmt.Sprintf("video-%d", id),
		Description: fmt.Sprintf("desc-%d", id),
		PlayURL:     fmt.Sprintf("https://cdn.example.com/%d.mp4", id),
		CoverURL:    fmt.Sprintf("https://cdn.example.com/%d.png", id),
		CreateTime:  createTime,
		LikesCount:  int64(id) * 10,
		Popularity:  int64(id) * 100,
	}
}

func entityKey(id uint) string { return fmt.Sprintf("%svideo:entity:%d", redisPrefix, id) }

func timelineKey() string { return redisPrefix + "feed:global_timeline" }

// seedEntity 把视频实体写进 L2（Redis），模拟“缓存已预热”。
func seedEntity(t *testing.T, f *fixture, v *video.Video) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal video: %v", err)
	}
	if err := f.mr.Set(entityKey(v.ID), string(b)); err != nil {
		t.Fatalf("seed redis: %v", err)
	}
}

// allowAnyLikes 让点赞查询返回空 map（不关心点赞状态的用例使用）。
func allowAnyLikes(f *fixture) {
	f.likes.EXPECT().
		BatchGetLiked(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(map[uint]bool{}, nil).
		AnyTimes()
}

// expectTimelineProbe 让"时间线落后探测"看到一批**已经在时间线上**的视频。
//
// 首页读路径会先确认时间线有没有落后（见 FeedService.timelineBehind）：
// 它取数据库里最新的 N 条、再核对每一条在不在 ZSET 里。用例里把这一批
// 指向已经种进时间线的视频，即判定为"没有落后"，被测分支保持原样。
//
// 用 AnyTimes 而不是 Times(1)：探测结论有 1 秒缓存，一个用例里探测几次
// 取决于它自己调了几次首页接口——那是被测行为，不该由脚手架来规定。
func expectTimelineProbe(f *fixture, onTimeline ...*video.Video) {
	f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
		Return(onTimeline, nil).AnyTimes()
}

// videosByIDs 返回一个按请求 ID 列表过滤的假数据库。
func videosByIDs(all ...*video.Video) func(context.Context, []uint) ([]*video.Video, error) {
	index := make(map[uint]*video.Video, len(all))
	for _, v := range all {
		index[v.ID] = v
	}
	return func(_ context.Context, ids []uint) ([]*video.Video, error) {
		out := make([]*video.Video, 0, len(ids))
		for _, id := range ids {
			if v, ok := index[id]; ok {
				out = append(out, v)
			}
		}
		return out, nil
	}
}

func idsOfVideos(videos []*video.Video) []uint {
	out := make([]uint, 0, len(videos))
	for _, v := range videos {
		out = append(out, v.ID)
	}
	return out
}

func idsOfItems(items []feed.FeedVideoItem) []uint {
	out := make([]uint, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func assertNoDuplicateIDs(t *testing.T, ids []uint) {
	t.Helper()
	seen := make(map[uint]int, len(ids))
	for _, id := range ids {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("video %d appears %d times in one page, want at most 1 (ids=%v)", id, n, ids)
		}
	}
}

// waitForRedisKey 等待异步回填完成（L3 -> L2 的回写是 go func 异步做的）。
func waitForRedisKey(t *testing.T, mr *miniredis.Miniredis, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mr.Exists(key) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("cache key %q was not backfilled within 2s", key)
}

// ---------- GetVideoByIDs：L1 / L2 / L3 ----------

func TestGetVideoByIDs(t *testing.T) {
	base := time.Now().Add(-time.Hour)

	t.Run("空 ID 列表直接返回空结果且不触碰任何缓存与数据库", func(t *testing.T) {
		f := newFixture(t)
		// 未设置 store / likes 期望 => 一旦被调用测试即失败。

		got, err := f.svc.GetVideoByIDs(context.Background(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d videos, want 0", len(got))
		}
	})

	t.Run("L2 命中时不查 MySQL 并把实体回填到 L1", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		seedEntity(t, f, v1)
		// 未设置 store.GetByIDs 期望 => 一旦落库测试即失败。

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want [1]", ids)
		}
		if got[0].Title != "video-1" {
			t.Errorf("decoded title = %q, want \"video-1\"", got[0].Title)
		}

		// 删掉 L2 再查一次：只有第一次真的回填了 L1 才可能仍然不落库。
		f.mr.Del(entityKey(1))
		got, err = f.svc.GetVideoByIDs(context.Background(), []uint{1})
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("second call got ids %v, want [1] (L1 backfill missing?)", ids)
		}
	})

	t.Run("L1 与 L2 都未命中时回落 MySQL 并异步回填 L2", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{1}).Return([]*video.Video{v1}, nil)

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want [1]", ids)
		}

		waitForRedisKey(t, f.mr, entityKey(1))
		raw, err := f.mr.Get(entityKey(1))
		if err != nil {
			t.Fatalf("read backfilled entity: %v", err)
		}
		var cached video.Video
		if err := json.Unmarshal([]byte(raw), &cached); err != nil {
			t.Fatalf("backfilled entity is not valid JSON: %v", err)
		}
		if cached.ID != 1 || cached.Title != "video-1" {
			t.Errorf("backfilled entity = {id:%d title:%q}, want {1 \"video-1\"}", cached.ID, cached.Title)
		}
	})

	t.Run("L2 中的损坏 JSON 视为未命中并回落 MySQL", func(t *testing.T) {
		f := newFixture(t)
		v5 := mkVideo(5, base)
		if err := f.mr.Set(entityKey(5), "{not-json"); err != nil {
			t.Fatalf("seed redis: %v", err)
		}
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{5}).Return([]*video.Video{v5}, nil)

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{5})
		if err != nil {
			t.Fatalf("corrupted cache entry must not fail the request, got %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{5}) {
			t.Fatalf("got ids %v, want [5]", ids)
		}
	})

	t.Run("数据库里已不存在的 ID 被跳过而不是返回空项", func(t *testing.T) {
		f := newFixture(t)
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{9}).Return([]*video.Video{}, nil)

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{9})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d videos, want 0 (deleted video must be skipped)", len(got))
		}
	})

	t.Run("返回顺序与入参一致而不是数据库返回顺序", func(t *testing.T) {
		f := newFixture(t)
		v1, v2, v3 := mkVideo(1, base), mkVideo(2, base), mkVideo(3, base)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v1, v2, v3)).AnyTimes()

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{3, 1, 2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{3, 1, 2}) {
			t.Fatalf("got ids %v, want the request order [3 1 2]", ids)
		}
	})

	t.Run("重复 ID 只穿透到 MySQL 一次（singleflight 合并并发回源）", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, ids []uint) ([]*video.Video, error) {
				// 给后两个 goroutine 足够时间加入同一个 singleflight 航班。
				time.Sleep(100 * time.Millisecond)
				return videosByIDs(v1)(context.Background(), ids)
			}).
			Times(1)

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{1, 1, 1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d videos, want 3 (one per requested id)", len(got))
		}
	})

	t.Run("Redis 故障时全部降级到 MySQL 且结果顺序不受影响", func(t *testing.T) {
		f := newFixture(t)
		v1, v2, v3 := mkVideo(1, base), mkVideo(2, base), mkVideo(3, base)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v1, v2, v3)).AnyTimes()
		f.mr.Close() // Redis 直接挂掉

		got, err := f.svc.GetVideoByIDs(context.Background(), []uint{3, 1, 2})
		if err != nil {
			t.Fatalf("redis outage must not fail the request, got %v", err)
		}
		if ids := idsOfVideos(got); !reflect.DeepEqual(ids, []uint{3, 1, 2}) {
			t.Fatalf("got ids %v, want [3 1 2]", ids)
		}
	})
}

// ---------- ListLatest：ZSET 重建 ----------

func TestListLatestTimelineRebuild(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	t.Run("ZSET 为空且数据库也为空：返回空页且只查一次库（不无限递归）", func(t *testing.T) {
		f := newFixture(t)
		f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
			Return(nil, nil).
			Times(1)
		// 未设置 likes 期望 => 空页不应该再走 buildFeedVideos。

		resp, err := f.svc.ListLatest(context.Background(), 3, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.VideoList) != 0 || resp.HasMore || resp.NextTime != 0 {
			t.Fatalf("got %+v, want an empty last page", resp)
		}
	})

	t.Run("ZSET 为空时用最新 1000 条重建时间线并返回首页", func(t *testing.T) {
		f := newFixture(t)
		v3 := mkVideo(3, base.Add(3*time.Minute))
		v2 := mkVideo(2, base.Add(2*time.Minute))
		v1 := mkVideo(1, base.Add(1*time.Minute))
		f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
			Return([]*video.Video{v3, v2, v1}, nil).
			Times(1)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v1, v2, v3)).AnyTimes()
		f.likes.EXPECT().BatchGetLiked(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(map[uint]bool{3: true}, nil).
			Times(1)

		resp, err := f.svc.ListLatest(context.Background(), 3, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2, 1}) {
			t.Fatalf("got ids %v, want newest-first [3 2 1]", ids)
		}
		if !resp.HasMore {
			t.Error("a full page must report HasMore=true")
		}
		if want := v1.CreateTime.UnixMilli(); resp.NextTime != want {
			t.Errorf("NextTime = %d, want the oldest item of the page (%d)", resp.NextTime, want)
		}
		if !resp.VideoList[0].IsLiked || resp.VideoList[1].IsLiked {
			t.Errorf("IsLiked flags = [%v %v], want [true false]",
				resp.VideoList[0].IsLiked, resp.VideoList[1].IsLiked)
		}

		members, err := f.mr.ZMembers(timelineKey())
		if err != nil {
			t.Fatalf("read rebuilt timeline: %v", err)
		}
		sort.Strings(members)
		if !reflect.DeepEqual(members, []string{"1", "2", "3"}) {
			t.Fatalf("rebuilt ZSET members = %v, want [1 2 3]", members)
		}
	})
}

// ---------- ListLatest：冷热边界 ----------

func TestListLatestColdHotBoundary(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	// 冷数据用例共用的断言：必须原样把用户的游标交给数据库、只查一次库、且不污染 ZSET。
	runColdCase := func(t *testing.T, cursor time.Time) {
		t.Helper()
		f := newFixture(t)
		tail := time.UnixMilli(base.UnixMilli())
		if _, err := f.mr.ZAdd(timelineKey(), float64(tail.UnixMilli()), "100"); err != nil {
			t.Fatalf("seed timeline: %v", err)
		}
		cold1 := mkVideo(7, base.Add(-2*time.Minute))
		cold2 := mkVideo(8, base.Add(-3*time.Minute))
		f.store.EXPECT().ListLatest(gomock.Any(), 3, cursor).
			Return([]*video.Video{cold1, cold2}, nil).
			Times(1)
		// 未设置 GetByIDs 期望 => 冷数据路径不应该再去逐个取实体。
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 3, cursor, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{7, 8}) {
			t.Fatalf("got ids %v, want the database page [7 8]", ids)
		}
		if resp.HasMore {
			t.Error("2 items for limit=3 must report HasMore=false (last page)")
		}
		if want := cold2.CreateTime.UnixMilli(); resp.NextTime != want {
			t.Errorf("NextTime = %d, want %d", resp.NextTime, want)
		}
		members, err := f.mr.ZMembers(timelineKey())
		if err != nil {
			t.Fatalf("read timeline: %v", err)
		}
		if !reflect.DeepEqual(members, []string{"100"}) {
			t.Fatalf("cold path must not write the ZSET, members = %v", members)
		}
	}

	t.Run("游标恰好等于 ZSET 最老时间时按冷数据处理", func(t *testing.T) {
		runColdCase(t, time.UnixMilli(base.UnixMilli()))
	})

	t.Run("游标早于 ZSET 最老时间时同样走冷数据路径", func(t *testing.T) {
		runColdCase(t, time.UnixMilli(base.Add(-30*time.Minute).UnixMilli()))
	})
}

// ---------- ListLatest：冷热拼接 ----------

func TestListLatestHotColdStitching(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())
	t1 := time.UnixMilli(now.Add(-3 * time.Minute).UnixMilli())

	seedTimeline := func(t *testing.T, f *fixture, videos ...*video.Video) {
		t.Helper()
		for _, v := range videos {
			if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
				t.Fatalf("seed timeline: %v", err)
			}
		}
	}

	t.Run("热数据不足 limit 时用冷数据补齐，且冷查询游标严格早于最后一条热数据", func(t *testing.T) {
		f := newFixture(t)
		v3, v2, v1 := mkVideo(3, t3), mkVideo(2, t2), mkVideo(1, t1)
		seedTimeline(t, f, v3, v2)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v1, v2, v3)).AnyTimes()
		// 数据库最新的两条都在时间线上 => 没落后，不触发补齐。
		expectTimelineProbe(f, v3, v2)
		// 关键断言：拼接冷数据时必须用“最后一条热数据的时间”做排他游标（create_time < t2），
		// 否则 v2 会在同一页里出现两次。
		f.store.EXPECT().ListLatest(gomock.Any(), 1, t2).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 3, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ids := idsOfItems(resp.VideoList)
		if !reflect.DeepEqual(ids, []uint{3, 2, 1}) {
			t.Fatalf("got ids %v, want hot-then-cold [3 2 1]", ids)
		}
		assertNoDuplicateIDs(t, ids)
		if !resp.HasMore {
			t.Error("exactly limit items must report HasMore=true")
		}
		if want := v1.CreateTime.UnixMilli(); resp.NextTime != want {
			t.Errorf("NextTime = %d, want the page tail %d", resp.NextTime, want)
		}
	})

	t.Run("冷热拼接后仍不足 limit：HasMore=false 表示已到最后一页", func(t *testing.T) {
		f := newFixture(t)
		v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
		seedTimeline(t, f, v3, v2)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
		expectTimelineProbe(f, v3, v2)
		f.store.EXPECT().ListLatest(gomock.Any(), 1, t2).
			Return([]*video.Video{}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 3, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2}) {
			t.Fatalf("got ids %v, want [3 2]", ids)
		}
		if resp.HasMore {
			t.Error("2 items for limit=3 must report HasMore=false")
		}
		if want := v2.CreateTime.UnixMilli(); resp.NextTime != want {
			t.Errorf("NextTime = %d, want %d", resp.NextTime, want)
		}
	})

	t.Run("游标边界处的热数据不会在下一页重复出现", func(t *testing.T) {
		f := newFixture(t)
		v2, v1 := mkVideo(2, t2), mkVideo(1, t1)
		seedTimeline(t, f, v2, v1)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v1, v2)).AnyTimes()
		// 上一页的最后一条是 v2(t2)；本页必须排除它（maxScore = t2-1）。
		f.store.EXPECT().ListLatest(gomock.Any(), 1, t1).
			Return([]*video.Video{}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 2, t2, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ids := idsOfItems(resp.VideoList)
		if !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want only [1] — the cursor item must not repeat", ids)
		}
		if resp.HasMore {
			t.Error("1 item for limit=2 must report HasMore=false")
		}
	})

	t.Run("冷页整页为空时返回空页而不是错误", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.mr.ZAdd(timelineKey(), float64(t3.UnixMilli()), "3"); err != nil {
			t.Fatalf("seed timeline: %v", err)
		}
		cursor := time.UnixMilli(t1.Add(-time.Hour).UnixMilli())
		f.store.EXPECT().ListLatest(gomock.Any(), 2, cursor).
			Return([]*video.Video{}, nil).
			Times(1)
		f.likes.EXPECT().BatchGetLiked(gomock.Any(), gomock.Len(0), uint(0)).
			Return(map[uint]bool{}, nil).
			Times(1)

		resp, err := f.svc.ListLatest(context.Background(), 2, cursor, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.VideoList == nil {
			t.Error("empty page must return an empty slice, not nil")
		}
		if len(resp.VideoList) != 0 || resp.HasMore || resp.NextTime != 0 {
			t.Fatalf("got %+v, want an empty last page", resp)
		}
	})
}

// ---------- ListLatest：时间线落后（自愈） ----------
//
// 这一节守的是一个**真实缺陷**：原实现只在 ZSET 完全为空时回退数据库，
// 于是"ZSET 落后"（异步消费者慢/挂掉，数据库已有更新的视频）会导致
// /feed/listLatest 永远服务旧数据——不报错、不打日志、不会自愈。
//
// 为什么必须有单测而不是只靠 e2e：e2e 只能在"刚发布完"这一个时间点上
// 观察，而这个缺陷的形态是"持续返回旧内容"。要稳定构造它，必须直接摆出
// "ZSET 有内容但比数据库旧"这个状态。

func TestListLatestRepairsStaleTimeline(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())

	f := newFixture(t)
	v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
	// 时间线里只有 v2：v3 已经发布并落库，但异步消费者还没把它写进 ZSET。
	if _, err := f.mr.ZAdd(timelineKey(), float64(v2.CreateTime.UnixMilli()), "2"); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}

	// 探测：数据库最新的一批里包含 v3，而 v3 不在时间线上 => 落后。
	f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
		Return([]*video.Video{v3, v2}, nil).Times(1)
	// 补齐：回捞最新 1000 条，只把比 ZSET 最高分更新的补进去。
	f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
		Return([]*video.Video{v3, v2}, nil).Times(1)
	f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
		DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
	allowAnyLikes(f)

	resp, err := f.svc.ListLatest(context.Background(), 2, time.Time{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2}) {
		t.Fatalf("got ids %v, want [3 2] —— 落后的时间线必须被补齐，最新视频不能缺席", ids)
	}

	members, err := f.mr.ZMembers(timelineKey())
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	sort.Strings(members)
	if !reflect.DeepEqual(members, []string{"2", "3"}) {
		t.Fatalf("ZSET members = %v, want [2 3]（缺口的 v3 应被补进来）", members)
	}
	score, err := f.mr.ZScore(timelineKey(), "3")
	if err != nil {
		t.Fatalf("ZScore: %v", err)
	}
	if want := float64(v3.CreateTime.UnixMilli()); score != want {
		t.Errorf("v3 的分数 = %v, want %v（分数必须是发布时间，否则分页会错乱）", score, want)
	}
}

// TestListLatestDetectsStaleTimelineDespiteFutureScores 守住"落后"的判据本身。
//
// 场景：时间线里有一个**时间戳在未来**的成员（历史脏数据、跨时区写入都会
// 造成这种成员），而数据库里最新的一条视频并不在时间线上。
//
// 用"数据库最新时间 > ZSET 最高分"来判断落后时，这个场景**检测不出来**：
// 未来成员的分数比任何新视频都大，比较永远为假，于是补齐永远不触发。
// 这不是假想——本机开发库的视频 795 就是这种数据（create_time 被按 UTC 读，
// 结果落在 5 小时之后），实测让基于时间戳比较的版本完全失效。
//
// 按"最新一条在不在时间线上"判断则与时钟无关，因此这条用例会通过。
func TestListLatestDetectsStaleTimelineDespiteFutureScores(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	future := time.UnixMilli(now.Add(5 * time.Hour).UnixMilli()) // 时间戳在未来的脏成员
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())

	f := newFixture(t)
	v100, v3 := mkVideo(100, future), mkVideo(3, t3)
	if _, err := f.mr.ZAdd(timelineKey(), float64(v100.CreateTime.UnixMilli()), "100"); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}

	f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
		Return([]*video.Video{v100, v3}, nil).Times(1)
	f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
		Return([]*video.Video{v3, v100}, nil).Times(1)
	f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
		DoAndReturn(videosByIDs(v3, v100)).AnyTimes()
	allowAnyLikes(f)

	resp, err := f.svc.ListLatest(context.Background(), 2, time.Time{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{100, 3}) {
		t.Fatalf("got ids %v, want [100 3] —— 未来时间戳的脏成员不该掩盖\"最新视频缺席\"这件事", ids)
	}
	// 最关键的一条：v3 必须真的被写进时间线，而不是靠某条降级路径蒙对。
	if _, err := f.mr.ZScore(timelineKey(), "3"); err != nil {
		t.Fatalf("v3 没有被补进时间线: %v", err)
	}
}

func TestListLatestProbeIsCachedAndOnlyOnFirstPage(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())

	t.Run("连续两次首页请求只探测一次数据库", func(t *testing.T) {
		f := newFixture(t)
		v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
		for _, v := range []*video.Video{v3, v2} {
			if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
				t.Fatalf("seed timeline: %v", err)
			}
		}
		// Times(1)：第二次请求必须命中探测缓存（否则首页读路径每次都要打库）。
		f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
			Return([]*video.Video{v3, v2}, nil).Times(1)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
		allowAnyLikes(f)

		for i := 0; i < 2; i++ {
			if _, err := f.svc.ListLatest(context.Background(), 2, time.Time{}, 0); err != nil {
				t.Fatalf("第 %d 次请求失败: %v", i+1, err)
			}
		}
	})

	t.Run("翻旧页不探测（探测只对首页有意义）", func(t *testing.T) {
		f := newFixture(t)
		v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
		for _, v := range []*video.Video{v3, v2} {
			if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
				t.Fatalf("seed timeline: %v", err)
			}
		}
		// 只期望冷路径那一次查询：若实现去探测，gomock 会因为"意外的调用"失败。
		f.store.EXPECT().ListLatest(gomock.Any(), 2, t2).
			Return([]*video.Video{}, nil).Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 2, t2, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.VideoList) != 0 {
			t.Fatalf("got %d items, want an empty page", len(resp.VideoList))
		}
	})
}

func TestListLatestProbeFailureIsNotFatal(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())

	t.Run("探测查库失败时继续用现有时间线，不返回错误", func(t *testing.T) {
		f := newFixture(t)
		v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
		for _, v := range []*video.Video{v3, v2} {
			if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
				t.Fatalf("seed timeline: %v", err)
			}
		}
		f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
			Return(nil, errors.New("db down")).Times(1)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 2, time.Time{}, 0)
		if err != nil {
			t.Fatalf("探测失败不该让读路径报错: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2}) {
			t.Fatalf("got ids %v, want [3 2]（探测是自愈机制，不是正确性前提）", ids)
		}
	})

	t.Run("补齐失败时退化为继续用旧时间线，而不是 5xx", func(t *testing.T) {
		f := newFixture(t)
		v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
		if _, err := f.mr.ZAdd(timelineKey(), float64(v2.CreateTime.UnixMilli()), "2"); err != nil {
			t.Fatalf("seed timeline: %v", err)
		}
		f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
			Return([]*video.Video{v3, v2}, nil).Times(1)
		f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
			Return(nil, errors.New("db down")).Times(1)
		f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
			DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 1, time.Time{}, 0)
		if err != nil {
			t.Fatalf("补齐失败不该让读路径报错: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{2}) {
			t.Fatalf("got ids %v, want [2]（宁可给一页稍旧的内容，也不要给错误）", ids)
		}
	})
}

// ---------- ListLatest：Redis 故障 / 未配置 ----------

func TestListLatestDegradation(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	t.Run("Redis 故障时降级到 MySQL 且不返回错误", func(t *testing.T) {
		f := newFixture(t)
		v2, v1 := mkVideo(2, base.Add(2*time.Minute)), mkVideo(1, base.Add(time.Minute))
		cursor := time.UnixMilli(base.Add(-time.Hour).UnixMilli())
		f.store.EXPECT().ListLatest(gomock.Any(), 2, cursor).
			Return([]*video.Video{v2, v1}, nil).
			Times(1)
		allowAnyLikes(f)
		f.mr.Close()

		resp, err := f.svc.ListLatest(context.Background(), 2, cursor, 0)
		if err != nil {
			t.Fatalf("redis outage must not fail the timeline, got %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{2, 1}) {
			t.Fatalf("got ids %v, want [2 1]", ids)
		}
		if !resp.HasMore {
			t.Error("full page from the fallback must report HasMore=true")
		}
	})

	t.Run("未配置 Redis 时直接走 MySQL", func(t *testing.T) {
		f := newFixtureWithoutCache(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().ListLatest(gomock.Any(), 5, time.Time{}).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListLatest(context.Background(), 5, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want [1]", ids)
		}
		if resp.HasMore {
			t.Error("1 item for limit=5 must report HasMore=false")
		}
	})
}

// ---------- ListLikesCount：游标分页 ----------

func TestListLikesCountCursorPagination(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	v3, v2, v1 := mkVideo(3, base.Add(3*time.Minute)), mkVideo(2, base.Add(2*time.Minute)), mkVideo(1, base.Add(time.Minute))

	cursorMatcher := func(c *feed.LikesCountCursor) gomock.Matcher {
		if c == nil {
			return gomock.Nil()
		}
		return gomock.Eq(c)
	}

	tests := []struct {
		name       string
		limit      int
		cursor     *feed.LikesCountCursor
		dbVideos   []*video.Video
		dbErr      error
		wantIDs    []uint
		wantMore   bool
		wantNext   *feed.LikesCountCursor
		wantErr    bool
		wantLikeDB bool
	}{
		{
			name:       "首页无游标：仓库收到 nil 游标且以最后一条生成下一页游标",
			limit:      2,
			cursor:     nil,
			dbVideos:   []*video.Video{v3, v2},
			wantIDs:    []uint{3, 2},
			wantMore:   true,
			wantNext:   &feed.LikesCountCursor{LikesCount: v2.LikesCount, ID: v2.ID},
			wantLikeDB: true,
		},
		{
			name:       "最后一页不足 limit：HasMore=false 但仍给出可续传游标",
			limit:      3,
			cursor:     nil,
			dbVideos:   []*video.Video{v3, v2},
			wantIDs:    []uint{3, 2},
			wantMore:   false,
			wantNext:   &feed.LikesCountCursor{LikesCount: v2.LikesCount, ID: v2.ID},
			wantLikeDB: true,
		},
		{
			name:       "空结果：返回空切片且没有任何游标",
			limit:      5,
			cursor:     nil,
			dbVideos:   nil,
			wantIDs:    []uint{},
			wantMore:   false,
			wantNext:   nil,
			wantLikeDB: true,
		},
		{
			name:       "翻页：上一页的游标原样透传给仓库",
			limit:      2,
			cursor:     &feed.LikesCountCursor{LikesCount: 20, ID: 2},
			dbVideos:   []*video.Video{v1},
			wantIDs:    []uint{1},
			wantMore:   false,
			wantNext:   &feed.LikesCountCursor{LikesCount: v1.LikesCount, ID: v1.ID},
			wantLikeDB: true,
		},
		{
			name:     "数据库错误：原样返回且不查询点赞状态",
			limit:    2,
			cursor:   nil,
			dbErr:    errors.New("db is down"),
			wantErr:  true,
			wantMore: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.store.EXPECT().
				ListLikesCountWithCursor(gomock.Any(), tc.limit, cursorMatcher(tc.cursor)).
				Return(tc.dbVideos, tc.dbErr).
				Times(1)
			if tc.wantLikeDB {
				f.likes.EXPECT().
					BatchGetLiked(gomock.Any(), gomock.Any(), uint(7)).
					Return(map[uint]bool{1: true}, nil).
					Times(1)
			}

			resp, err := f.svc.ListLikesCount(context.Background(), tc.limit, tc.cursor, 7)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.VideoList == nil {
				t.Error("VideoList must never be nil")
			}
			if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Errorf("got ids %v, want %v", ids, tc.wantIDs)
			}
			if resp.HasMore != tc.wantMore {
				t.Errorf("HasMore = %v, want %v", resp.HasMore, tc.wantMore)
			}
			if tc.wantNext == nil {
				if resp.NextLikesCountBefore != nil || resp.NextIDBefore != nil {
					t.Errorf("got next cursor (%v,%v), want none",
						resp.NextLikesCountBefore, resp.NextIDBefore)
				}
			} else {
				if resp.NextLikesCountBefore == nil || resp.NextIDBefore == nil {
					t.Fatalf("missing next cursor, want (%d,%d)",
						tc.wantNext.LikesCount, tc.wantNext.ID)
				}
				if *resp.NextLikesCountBefore != tc.wantNext.LikesCount || *resp.NextIDBefore != tc.wantNext.ID {
					t.Errorf("next cursor = (%d,%d), want (%d,%d)",
						*resp.NextLikesCountBefore, *resp.NextIDBefore,
						tc.wantNext.LikesCount, tc.wantNext.ID)
				}
			}
			if len(tc.wantIDs) > 0 && resp.VideoList[0].IsLiked != (tc.wantIDs[0] == 1) {
				t.Errorf("IsLiked of first item = %v, want %v",
					resp.VideoList[0].IsLiked, tc.wantIDs[0] == 1)
			}
		})
	}
}

// ---------- ListByFollowing：读缓存 + 回填 + 降级 ----------

func TestListByFollowing(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	cacheKey := func(limit int, accountID uint, before int64) string {
		return fmt.Sprintf("%sfeed:listByFollowing:limit=%d:accountID=%d:before=%d",
			redisPrefix, limit, accountID, before)
	}

	t.Run("缓存命中时直接返回缓存且不查数据库", func(t *testing.T) {
		f := newFixture(t)
		want := feed.ListByFollowingResponse{
			VideoList: []feed.FeedVideoItem{{ID: 9, Title: "cached", CreateTime: base.Unix()}},
			NextTime:  base.Unix(),
			HasMore:   false,
		}
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal cached response: %v", err)
		}
		if err := f.mr.Set(cacheKey(2, 7, 0), string(b)); err != nil {
			t.Fatalf("seed redis: %v", err)
		}
		// 未设置 store 期望 => 一旦查库测试即失败。

		got, err := f.svc.ListByFollowing(context.Background(), 2, time.Time{}, 7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want the cached %+v", got, want)
		}
	})

	t.Run("缓存未命中时回源数据库并回填缓存（含释放锁）", func(t *testing.T) {
		f := newFixture(t)
		v2, v1 := mkVideo(2, base.Add(2*time.Second)), mkVideo(1, base.Add(time.Second))
		f.store.EXPECT().ListByFollowing(gomock.Any(), 2, uint(7), time.Time{}).
			Return([]*video.Video{v2, v1}, nil).
			Times(1)
		allowAnyLikes(f)

		got, err := f.svc.ListByFollowing(context.Background(), 2, time.Time{}, 7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(got.VideoList); !reflect.DeepEqual(ids, []uint{2, 1}) {
			t.Fatalf("got ids %v, want [2 1]", ids)
		}
		if !got.HasMore {
			t.Error("full page must report HasMore=true")
		}
		if want := v1.CreateTime.Unix(); got.NextTime != want {
			t.Errorf("NextTime = %d, want %d", got.NextTime, want)
		}

		raw, err := f.mr.Get(cacheKey(2, 7, 0))
		if err != nil {
			t.Fatalf("response was not written back to Redis: %v", err)
		}
		var cached feed.ListByFollowingResponse
		if err := json.Unmarshal([]byte(raw), &cached); err != nil {
			t.Fatalf("cached response is not valid JSON: %v", err)
		}
		if !reflect.DeepEqual(cached, got) {
			t.Errorf("cached %+v, want %+v", cached, got)
		}
		if f.mr.Exists("lock:" + cacheKey(2, 7, 0)) {
			t.Error("the miss-path lock must be released before returning")
		}
	})

	t.Run("带游标时游标进入缓存键与数据库条件", func(t *testing.T) {
		f := newFixture(t)
		cursor := base.Add(30 * time.Second)
		v1 := mkVideo(1, base.Add(time.Second))
		f.store.EXPECT().ListByFollowing(gomock.Any(), 1, uint(7), cursor).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)

		if _, err := f.svc.ListByFollowing(context.Background(), 1, cursor, 7); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.mr.Exists(cacheKey(1, 7, cursor.Unix())) {
			t.Errorf("page cursor must be part of the cache key, keys=%v", f.mr.Keys())
		}
	})

	t.Run("缓存内容损坏时回源数据库", func(t *testing.T) {
		f := newFixture(t)
		if err := f.mr.Set(cacheKey(1, 7, 0), "{broken"); err != nil {
			t.Fatalf("seed redis: %v", err)
		}
		v1 := mkVideo(1, base)
		f.store.EXPECT().ListByFollowing(gomock.Any(), 1, uint(7), time.Time{}).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)

		got, err := f.svc.ListByFollowing(context.Background(), 1, time.Time{}, 7)
		if err != nil {
			t.Fatalf("corrupted cache entry must not fail the request, got %v", err)
		}
		if ids := idsOfItems(got.VideoList); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want [1]", ids)
		}
	})

	t.Run("Redis 故障时仍能从数据库返回且缓存失败不影响响应", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().ListByFollowing(gomock.Any(), 1, uint(7), time.Time{}).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)
		f.mr.Close()

		got, err := f.svc.ListByFollowing(context.Background(), 1, time.Time{}, 7)
		if err != nil {
			t.Fatalf("redis outage must not fail the follow feed, got %v", err)
		}
		if ids := idsOfItems(got.VideoList); !reflect.DeepEqual(ids, []uint{1}) {
			t.Fatalf("got ids %v, want [1]", ids)
		}
	})

	t.Run("匿名访问（accountID=0）不使用缓存", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().ListByFollowing(gomock.Any(), 1, uint(0), time.Time{}).
			Return([]*video.Video{v1}, nil).
			Times(1)
		allowAnyLikes(f)

		if _, err := f.svc.ListByFollowing(context.Background(), 1, time.Time{}, 0); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, k := range f.mr.Keys() {
			if strings.Contains(k, "listByFollowing") {
				t.Errorf("anonymous request must not touch the per-user cache, found key %q", k)
			}
		}
	})

	t.Run("数据库错误原样返回", func(t *testing.T) {
		f := newFixture(t)
		dbErr := errors.New("db is down")
		f.store.EXPECT().ListByFollowing(gomock.Any(), 1, uint(7), time.Time{}).
			Return(nil, dbErr).
			Times(1)

		if _, err := f.svc.ListByFollowing(context.Background(), 1, time.Time{}, 7); !errors.Is(err, dbErr) {
			t.Fatalf("got %v, want the database error", err)
		}
	})
}

// ---------- ListByPopularity：Redis 热榜 + 数据库兜底 ----------

func TestListByPopularity(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	// seedHotKey 把热榜数据写进 asOf 那一分钟的桶里（asOf 必须是整分钟）。
	seedHotKey := func(t *testing.T, f *fixture, asOf time.Time, entries map[string]float64) {
		t.Helper()
		key := redisPrefix + "hot:video:1m:" + asOf.UTC().Format("200601021504")
		for member, score := range entries {
			if _, err := f.mr.ZAdd(key, score, member); err != nil {
				t.Fatalf("seed hot ranking: %v", err)
			}
		}
	}

	t.Run("热榜为空且 offset>0：返回空页且不回落数据库", func(t *testing.T) {
		f := newFixture(t)
		asOf := time.Now().UTC().Truncate(time.Minute)
		// 未设置 store 期望 => 一旦回落数据库测试即失败。

		resp, err := f.svc.ListByPopularity(context.Background(), 2, asOf.Unix(), 2, 0, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.VideoList) != 0 {
			t.Fatalf("got %d items, want 0", len(resp.VideoList))
		}
		if resp.HasMore {
			t.Error("empty hot ranking must report HasMore=false")
		}
		if resp.NextOffset != 2 {
			t.Errorf("NextOffset = %d, want the requested offset 2", resp.NextOffset)
		}
		if resp.AsOf != asOf.Unix() {
			t.Errorf("AsOf = %d, want %d", resp.AsOf, asOf.Unix())
		}
	})

	t.Run("热榜命中时按热度顺序返回，数据库乱序不影响排序", func(t *testing.T) {
		f := newFixture(t)
		asOf := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
		v3, v2 := mkVideo(3, base.Add(3*time.Minute)), mkVideo(2, base.Add(2*time.Minute))
		seedHotKey(t, f, asOf, map[string]float64{"3": 300, "2": 200})
		// 故意乱序返回，服务必须按热榜顺序重排。
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{3, 2}).
			Return([]*video.Video{v2, v3}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListByPopularity(context.Background(), 2, asOf.Unix(), 0, 7, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2}) {
			t.Fatalf("got ids %v, want the hot ranking order [3 2]", ids)
		}
		if resp.AsOf != asOf.Unix() {
			t.Errorf("AsOf = %d, want %d", resp.AsOf, asOf.Unix())
		}
		if resp.NextOffset != 2 || !resp.HasMore {
			t.Errorf("NextOffset=%d HasMore=%v, want 2 / true", resp.NextOffset, resp.HasMore)
		}
		if resp.NextLatestPopularity == nil || *resp.NextLatestPopularity != v2.Popularity ||
			resp.NextLatestIDBefore == nil || *resp.NextLatestIDBefore != v2.ID ||
			resp.NextLatestBefore == nil || !resp.NextLatestBefore.Equal(v2.CreateTime) {
			t.Errorf("next DB cursor = (%v,%v,%v), want v2's (popularity,time,id)",
				resp.NextLatestPopularity, resp.NextLatestBefore, resp.NextLatestIDBefore)
		}
	})

	t.Run("热榜翻页：offset 生效且两页不重复", func(t *testing.T) {
		f := newFixture(t)
		asOf := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Minute)
		v4, v3, v2, v1 := mkVideo(4, base.Add(4*time.Minute)), mkVideo(3, base.Add(3*time.Minute)),
			mkVideo(2, base.Add(2*time.Minute)), mkVideo(1, base.Add(time.Minute))
		seedHotKey(t, f, asOf, map[string]float64{"4": 400, "3": 300, "2": 200, "1": 100})
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{4, 3}).
			Return([]*video.Video{v4, v3}, nil).
			Times(1)
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{2, 1}).
			Return([]*video.Video{v2, v1}, nil).
			Times(1)
		allowAnyLikes(f)

		page1, err := f.svc.ListByPopularity(context.Background(), 2, asOf.Unix(), 0, 0, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		page2, err := f.svc.ListByPopularity(context.Background(), 2, asOf.Unix(), page1.NextOffset, 0, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		ids := append(idsOfItems(page1.VideoList), idsOfItems(page2.VideoList)...)
		if !reflect.DeepEqual(ids, []uint{4, 3, 2, 1}) {
			t.Fatalf("two pages gave ids %v, want [4 3 2 1]", ids)
		}
		assertNoDuplicateIDs(t, ids)
		if !page2.HasMore {
			t.Error("page 2 is a full page, so HasMore must stay true (it is a full-page flag, not an exhaustion flag)")
		}

		// 第三页：热榜已翻到底，必须返回空页而不是回落数据库。
		page3, err := f.svc.ListByPopularity(context.Background(), 2, asOf.Unix(), page2.NextOffset, 0, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("page 3: %v", err)
		}
		if len(page3.VideoList) != 0 || page3.HasMore {
			t.Fatalf("got %+v, want an empty last page", page3)
		}
		if page3.NextOffset != page2.NextOffset {
			t.Errorf("NextOffset = %d, want the requested offset %d", page3.NextOffset, page2.NextOffset)
		}
	})

	t.Run("热榜里的视频已被删除时跳过而不是返回空项", func(t *testing.T) {
		f := newFixture(t)
		asOf := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
		v3, v2 := mkVideo(3, base.Add(3*time.Minute)), mkVideo(2, base.Add(2*time.Minute))
		seedHotKey(t, f, asOf, map[string]float64{"3": 300, "2": 200, "1": 100})
		// 1 号视频已经被删除，数据库只返回两条。
		f.store.EXPECT().GetByIDs(gomock.Any(), []uint{3, 2, 1}).
			Return([]*video.Video{v3, v2}, nil).
			Times(1)
		allowAnyLikes(f)

		resp, err := f.svc.ListByPopularity(context.Background(), 3, asOf.Unix(), 0, 0, 0, time.Time{}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{3, 2}) {
			t.Fatalf("got ids %v, want [3 2]", ids)
		}
		if resp.HasMore {
			t.Error("2 items for limit=3 must report HasMore=false")
		}
		if resp.NextOffset != 2 {
			t.Errorf("NextOffset = %d, want 2 (only resolvable items count)", resp.NextOffset)
		}
	})

	t.Run("Redis 热榜不可用时回落到数据库游标分页", func(t *testing.T) {
		f := newFixture(t)
		v2, v1 := mkVideo(2, base.Add(2*time.Minute)), mkVideo(1, base.Add(time.Minute))
		cursorTime := base.Add(-time.Minute)
		f.store.EXPECT().ListByPopularity(gomock.Any(), 2, int64(77), cursorTime, uint(66)).
			Return([]*video.Video{v2, v1}, nil).
			Times(1)
		allowAnyLikes(f)
		f.mr.Close()

		resp, err := f.svc.ListByPopularity(context.Background(), 2, 0, 0, 0, 77, cursorTime, 66)
		if err != nil {
			t.Fatalf("redis outage must not fail the ranking, got %v", err)
		}
		if ids := idsOfItems(resp.VideoList); !reflect.DeepEqual(ids, []uint{2, 1}) {
			t.Fatalf("got ids %v, want [2 1]", ids)
		}
		if resp.AsOf != 0 || resp.NextOffset != 0 {
			t.Errorf("DB fallback must not advertise a hot snapshot, got AsOf=%d NextOffset=%d",
				resp.AsOf, resp.NextOffset)
		}
		if resp.NextLatestPopularity == nil || *resp.NextLatestPopularity != v1.Popularity {
			t.Errorf("next popularity cursor = %v, want %d", resp.NextLatestPopularity, v1.Popularity)
		}
	})

	t.Run("数据库错误原样返回", func(t *testing.T) {
		f := newFixture(t)
		dbErr := errors.New("db is down")
		f.store.EXPECT().ListByPopularity(gomock.Any(), 2, int64(0), time.Time{}, uint(0)).
			Return(nil, dbErr).
			Times(1)
		f.mr.Close()

		if _, err := f.svc.ListByPopularity(context.Background(), 2, 0, 0, 0, 0, time.Time{}, 0); !errors.Is(err, dbErr) {
			t.Fatalf("got %v, want the database error", err)
		}
	})
}

// ---------- ListByTag：字段映射与错误传播 ----------

func TestListByTag(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	t.Run("填充作者信息与点赞状态", func(t *testing.T) {
		f := newFixture(t)
		v2, v1 := mkVideo(2, base.Add(2*time.Second)), mkVideo(1, base.Add(time.Second))
		f.store.EXPECT().ListByTag(gomock.Any(), "golang", 5).
			Return([]*video.Video{v2, v1}, nil).
			Times(1)
		f.likes.EXPECT().BatchGetLiked(gomock.Any(), []uint{2, 1}, uint(7)).
			Return(map[uint]bool{1: true}, nil).
			Times(1)

		items, err := f.svc.ListByTag(context.Background(), "golang", 5, 7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2", len(items))
		}
		if items[0].Author.ID != v2.AuthorID || items[0].Author.Username != v2.Username {
			t.Errorf("author = %+v, want {%d %q}", items[0].Author, v2.AuthorID, v2.Username)
		}
		if items[0].IsLiked || !items[1].IsLiked {
			t.Errorf("IsLiked = [%v %v], want [false true]", items[0].IsLiked, items[1].IsLiked)
		}
		if items[0].CreateTime != v2.CreateTime.Unix() || items[0].LikesCount != v2.LikesCount {
			t.Errorf("item = %+v, want create_time=%d likes_count=%d",
				items[0], v2.CreateTime.Unix(), v2.LikesCount)
		}
	})

	t.Run("点赞状态查询失败时整个请求失败（不返回 IsLiked 全 false 的数据）", func(t *testing.T) {
		f := newFixture(t)
		v1 := mkVideo(1, base)
		f.store.EXPECT().ListByTag(gomock.Any(), "golang", 5).
			Return([]*video.Video{v1}, nil).
			Times(1)
		f.likes.EXPECT().BatchGetLiked(gomock.Any(), []uint{1}, uint(7)).
			Return(nil, errors.New("like db is down")).
			Times(1)

		if _, err := f.svc.ListByTag(context.Background(), "golang", 5, 7); err == nil {
			t.Fatal("expected an error when the like lookup fails, got nil")
		}
	})

	t.Run("数据库错误原样返回且不查询点赞状态", func(t *testing.T) {
		f := newFixture(t)
		dbErr := errors.New("db is down")
		f.store.EXPECT().ListByTag(gomock.Any(), "golang", 5).
			Return(nil, dbErr).
			Times(1)
		// 未设置 likes 期望 => 一旦调用即失败。

		if _, err := f.svc.ListByTag(context.Background(), "golang", 5, 7); !errors.Is(err, dbErr) {
			t.Fatalf("got %v, want the database error", err)
		}
	})
}

// TestListLatestRepairsSkewedTimelineScores 守一次**真机上量出来的故障**。
//
// 背景：本机 compose 环境里时间线的分数比数据库的 create_time 整整大 8 小时
// （另一处按本地时区而非 UTC 写分数留下的历史数据，6 条成员全部如此）。
// 它不是"顺序有点不对"，而是**冷热拼接算出错误的游标**：游标取 ZSET 尾部
// （最旧分数），而它比那条视频的真实时间大 8 小时，于是数据库查询
// `create_time < 游标` 会把已经返回过的视频**再返回一遍**——
// /feed/listLatest 第一页出现重复视频（实测 6 条内容返回 9 行）。
//
// 这里构造的就是那个场景：成员齐全、分数全部偏大 8 小时。
func TestListLatestRepairsSkewedTimelineScores(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	t3 := time.UnixMilli(now.Add(-1 * time.Minute).UnixMilli())
	t2 := time.UnixMilli(now.Add(-2 * time.Minute).UnixMilli())

	f := newFixture(t)
	v3, v2 := mkVideo(3, t3), mkVideo(2, t2)
	const skew = 8 * time.Hour
	for _, v := range []*video.Video{v3, v2} {
		if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.Add(skew).UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
			t.Fatalf("seed timeline: %v", err)
		}
	}

	// 探测：成员都在，但分数明显偏大 => 需要修复。
	f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
		Return([]*video.Video{v3, v2}, nil).Times(1)
	// 修复：整批重写（一次 LIMIT 1000 的回捞）。
	f.store.EXPECT().ListLatest(gomock.Any(), 1000, time.Time{}).
		Return([]*video.Video{v3, v2}, nil).Times(1)
	f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
		DoAndReturn(videosByIDs(v2, v3)).AnyTimes()
	allowAnyLikes(f)

	resp, err := f.svc.ListLatest(context.Background(), 2, time.Time{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := idsOfItems(resp.VideoList)
	if !reflect.DeepEqual(ids, []uint{3, 2}) {
		t.Fatalf("got ids %v, want [3 2]（分数偏大必须被修复，否则第一页会出现重复视频）", ids)
	}
	for _, v := range []*video.Video{v3, v2} {
		score, err := f.mr.ZScore(timelineKey(), fmt.Sprintf("%d", v.ID))
		if err != nil {
			t.Fatalf("ZScore(%d): %v", v.ID, err)
		}
		if want := float64(v.CreateTime.UnixMilli()); score != want {
			t.Errorf("v%d 的分数 = %v, want %v（分数必须是发布时间，否则分页会错乱）",
				v.ID, score, want)
		}
	}
}

// TestListLatestFusedDoesNotAskForMoreThanAPage 守一个**只有在配额开启时才可能出现**
// 的缺陷：融合路径要取 poolLimit 条候选（给配额位留替补），但"要不要冷热拼接"
// 必须按**这一页需要几条**判断。
//
// 混用两者的后果：时间线里只有 3 条成员、而数据库里有 20 条视频时，
// 实现会因为"3 < 200"去数据库补 197 条——一次只请求 5 条的调用
// 却把 200 条候选读进了内存。这里用 mock 的调用参数直接钉住拼接量。
func TestListLatestFusedDoesNotAskForMoreThanAPage(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	f := newFixture(t)
	const limit = 5

	// 时间线只有 3 条成员（并非常见，但完全合法：新部署 + 少量发布）。
	timeline := make([]*video.Video, 0, 3)
	for i := 1; i <= 3; i++ {
		v := mkVideo(uint(i), time.UnixMilli(now.Add(-time.Duration(i)*time.Minute).UnixMilli()))
		timeline = append(timeline, v)
		if _, err := f.mr.ZAdd(timelineKey(), float64(v.CreateTime.UnixMilli()), fmt.Sprintf("%d", v.ID)); err != nil {
			t.Fatalf("seed timeline: %v", err)
		}
	}

	// 探测：数据库里最新的 20 条就是这 3 条（都在时间线上 => 不落后）。
	f.store.EXPECT().ListLatest(gomock.Any(), feed.TimelineProbeSize, time.Time{}).
		Return(timeline, nil).Times(1)
	// 拼接：**只允许要 limit 条**（3 + 2），不能要 poolLimit 条。
	f.store.EXPECT().ListLatest(gomock.Any(), limit-len(timeline), gomock.Any()).
		Return([]*video.Video{}, nil).Times(1)
	f.store.EXPECT().GetByIDs(gomock.Any(), gomock.Any()).
		DoAndReturn(videosByIDs(timeline...)).AnyTimes()
	allowAnyLikes(f)

	resp, err := f.svc.ListLatest(context.Background(), limit, time.Time{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.VideoList) != len(timeline) {
		t.Fatalf("got %d items, want %d", len(resp.VideoList), len(timeline))
	}
}
