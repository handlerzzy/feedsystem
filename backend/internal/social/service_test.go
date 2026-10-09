package social_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/social"
	"github.com/handlerzzy/feedsystem/internal/social/mock"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

// assertErrorContract 锁定 E 类改造后的错误契约，三个维度缺一不可：
//
//  1. 状态码：服务层返回的是带码错误（CodedError），分类器据此给出 400/409。
//     改造前这三条都是裸 errors.New，分类器一律判 500。
//  2. 文案逐字不变：err.Error() 仍是改造前的那句话，调用方看到的文本无变化。
//  3. 对外可回显文本：PublicMessage 与 err.Error() 一致（4xx 原样回显），
//     即 HTTP 响应体里的 error 字段与改造前逐字相同。
func assertErrorContract(t *testing.T, err error, wantStatus int, wantMessage string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := apierror.ClassifyHTTPStatus(err); got != wantStatus {
		t.Errorf("ClassifyHTTPStatus(%v) = %d, want %d", err, got, wantStatus)
	}
	if got := err.Error(); got != wantMessage {
		t.Errorf("err.Error() = %q, want the pre-refactor wording %q", got, wantMessage)
	}
	if got := apierror.PublicMessage(err); got != wantMessage {
		t.Errorf("PublicMessage(err) = %q, want %q (client-visible text must not change)", got, wantMessage)
	}
}

// newCache 返回一个基于 miniredis 的真实 Redis 客户端（前缀 test:）。
func newCache(t *testing.T) (*rediscache.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rediscache.NewClient(rdb, "test:"), mr
}

// fixture 把 SocialService 的三条外部依赖都换成 mock/内存实现。
type fixture struct {
	store    *mock.MockSocialStore
	accounts *mock.MockAccountLookup
	pub      *mock.MockSocialPublisher
	cache    *rediscache.Client
	mr       *miniredis.Miniredis
	svc      *social.SocialService
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	cache, mr := newCache(t)
	f := &fixture{
		store:    mock.NewMockSocialStore(ctrl),
		accounts: mock.NewMockAccountLookup(ctrl),
		pub:      mock.NewMockSocialPublisher(ctrl),
		cache:    cache,
		mr:       mr,
	}
	f.svc = social.NewSocialService(f.store, f.accounts, f.pub, cache)
	return f
}

// existedAccount 让账号存在性校验通过：Follow/Unfollow 会按顺序查两个 ID。
func existedAccount(t *testing.T, f *fixture, ids ...uint) {
	t.Helper()
	for _, id := range ids {
		f.accounts.EXPECT().FindByID(gomock.Any(), id).Return(&account.Account{ID: id}, nil)
	}
}

// followingCacheKey 复刻 feed 服务里 Following 时间线的缓存键格式，
// 用于验证 social 侧只失效“该关注者本人”的缓存。
func (f *fixture) followingCacheKey(accountID uint) string {
	return f.cache.Key("feed:listByFollowing:limit=10:accountID=%d:before=0", accountID)
}

// ---------- Follow ----------

func TestFollow(t *testing.T) {
	ctx := context.Background()

	t.Run("不能关注自己", func(t *testing.T) {
		f := newFixture(t)
		// 两个 ID 相同，账号校验会连着查两次。
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7}, nil).Times(2)
		// 未设置 IsFollowed / Follow / MQ 期望 => 被调用即失败。

		err := f.svc.Follow(ctx, &social.Social{FollowerID: 7, VloggerID: 7})
		if err == nil {
			t.Fatal("expected error when following self, got nil")
		}
		if !strings.Contains(err.Error(), "self") {
			t.Errorf("expected a self-follow error, got %v", err)
		}
		// 状态码与文案：改造前是 500 + 原文回显，现在必须是 400 且文案不变。
		assertErrorContract(t, err, http.StatusBadRequest, "can not follow self")
	})

	t.Run("重复关注被拒绝且不重复写库", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)
		// 未设置 Follow / MQ 期望 => 已存在的关系不得再次落库、不得重复发通知。

		err := f.svc.Follow(ctx, rel)
		if err == nil {
			t.Fatal("expected error for duplicate follow, got nil")
		}
		if !strings.Contains(err.Error(), "already followed") {
			t.Errorf("expected an already-followed error, got %v", err)
		}
		// 重复关注是状态冲突：改造前 500，现在 409，文案逐字不变。
		assertErrorContract(t, err, http.StatusConflict, "already followed")
	})

	t.Run("关注者账号不存在时报错且不写库", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(1)).Return(nil, notFound)
		// 未设置第二个 FindByID / IsFollowed / Follow 期望 => 提前返回。

		err := f.svc.Follow(ctx, &social.Social{FollowerID: 1, VloggerID: 2})
		if !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("目标用户不存在时报错且不写库", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		existedAccount(t, f, 1)
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(2)).Return(nil, notFound)
		// 未设置 IsFollowed / Follow 期望 => 目标不存在时不得建立关系。

		err := f.svc.Follow(ctx, &social.Social{FollowerID: 1, VloggerID: 2})
		if !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("关系查询失败时不写库", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		dbErr := errors.New("connection refused")
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, dbErr)
		// 未设置 Follow 期望 => 判重失败时必须放弃写入，避免产生重复关系。

		err := f.svc.Follow(ctx, rel)
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected the repo error to surface, got %v", err)
		}
	})

	t.Run("成功关注：先写库、再失效本人缓存、最后发 MQ", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)

		var order []string
		f.store.EXPECT().Follow(gomock.Any(), rel).DoAndReturn(
			func(_ context.Context, _ *social.Social) error {
				order = append(order, "db")
				return nil
			})
		f.pub.EXPECT().Follow(gomock.Any(), uint(1), uint(2)).DoAndReturn(
			func(_ context.Context, _, _ uint) error {
				order = append(order, "mq")
				return nil
			})

		// 预置两个账号的 Following 缓存：只应失效关注者本人的。
		ownKey, otherKey := f.followingCacheKey(1), f.followingCacheKey(2)
		f.mr.Set(ownKey, "cached")
		f.mr.Set(otherKey, "cached")

		if err := f.svc.Follow(ctx, rel); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(order) != 2 || order[0] != "db" || order[1] != "mq" {
			t.Errorf("副作用顺序 = %v, want [db mq]（先持久化再通知）", order)
		}
		if f.mr.Exists(ownKey) {
			t.Error("关注者本人的 Following 缓存应被失效")
		}
		if !f.mr.Exists(otherKey) {
			t.Error("其他账号的 Following 缓存不应被误删")
		}
	})

	t.Run("MQ 发布失败不影响关注成功", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
		f.store.EXPECT().Follow(gomock.Any(), rel).Return(nil)
		f.pub.EXPECT().Follow(gomock.Any(), uint(1), uint(2)).
			Return(errors.New("social mq is not initialized"))

		if err := f.svc.Follow(ctx, rel); err != nil {
			t.Fatalf("MQ 不可用时关注必须仍然成功（降级），got %v", err)
		}
	})

	t.Run("MQ 未启用（nil 发布器）时关注依然成功", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockSocialStore(ctrl)
		accounts := mock.NewMockAccountLookup(ctrl)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		accounts.EXPECT().FindByID(gomock.Any(), uint(1)).Return(&account.Account{ID: 1}, nil)
		accounts.EXPECT().FindByID(gomock.Any(), uint(2)).Return(&account.Account{ID: 2}, nil)
		store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
		store.EXPECT().Follow(gomock.Any(), rel).Return(nil)
		// cache 也为 nil：既不失效缓存也不发 MQ，业务照常完成。
		svc := social.NewSocialService(store, accounts, nil, nil)

		if err := svc.Follow(ctx, rel); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("MQ 初始化失败传入 typed-nil 时关注依然成功", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockSocialStore(ctrl)
		accounts := mock.NewMockAccountLookup(ctrl)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		accounts.EXPECT().FindByID(gomock.Any(), uint(1)).Return(&account.Account{ID: 1}, nil)
		accounts.EXPECT().FindByID(gomock.Any(), uint(2)).Return(&account.Account{ID: 2}, nil)
		store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
		store.EXPECT().Follow(gomock.Any(), rel).Return(nil)

		// router.go 在 NewSocialMQ 失败时传的是 (*rabbitmq.SocialMQ)(nil)：
		// 放进接口字段后接口值本身不为 nil，因此会真的调用一次发布，
		// publish 内部按 "social mq is not initialized" 报错，service 只记日志。
		// 本用例锁定的是：这条路不会 panic，也不会让关注失败。
		var mq *rabbitmq.SocialMQ
		svc := social.NewSocialService(store, accounts, mq, nil)

		if err := svc.Follow(ctx, rel); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("缓存失效失败不影响关注成功", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
		f.store.EXPECT().Follow(gomock.Any(), rel).Return(nil)
		f.pub.EXPECT().Follow(gomock.Any(), uint(1), uint(2)).Return(nil)
		f.mr.Close() // Redis 宕机：删缓存必然失败

		if err := f.svc.Follow(ctx, rel); err != nil {
			t.Fatalf("缓存失效失败不得影响关注结果, got %v", err)
		}
	})
}

// ---------- Unfollow ----------

func TestUnfollow(t *testing.T) {
	ctx := context.Background()

	t.Run("取消未关注的关系是幂等的：不删库也不发 MQ", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
		// 未设置 Unfollow / MQ 期望 => 幂等：不存在的关系不产生任何副作用。

		err := f.svc.Unfollow(ctx, rel)
		if err == nil {
			t.Fatal("expected error when unfollowing a non-existent relation, got nil")
		}
		if !strings.Contains(err.Error(), "not followed") {
			t.Errorf("expected a not-followed error, got %v", err)
		}
		// 取关不存在的关系同样是状态冲突：改造前 500，现在 409，文案逐字不变。
		assertErrorContract(t, err, http.StatusConflict, "not followed")
	})

	t.Run("重复取消关注：第二次不再产生任何副作用", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)
		f.store.EXPECT().Unfollow(gomock.Any(), rel).Return(nil)
		f.pub.EXPECT().UnFollow(gomock.Any(), uint(1), uint(2)).Return(nil)
		// 第二次调用：关系已不存在。
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)

		if err := f.svc.Unfollow(ctx, rel); err != nil {
			t.Fatalf("first unfollow: %v", err)
		}
		if err := f.svc.Unfollow(ctx, rel); err == nil {
			t.Fatal("second unfollow should report the relation no longer exists")
		}
		// 未给第二次 Unfollow / MQ 设置期望 => 重复删除与重复通知都会让测试失败。
	})

	t.Run("关注者账号不存在时报错且不删库", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(1)).Return(nil, notFound)

		err := f.svc.Unfollow(ctx, &social.Social{FollowerID: 1, VloggerID: 2})
		if !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("成功取关：先删库、再失效缓存、最后发 MQ", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)

		var order []string
		f.store.EXPECT().Unfollow(gomock.Any(), rel).DoAndReturn(
			func(_ context.Context, _ *social.Social) error {
				order = append(order, "db")
				return nil
			})
		f.pub.EXPECT().UnFollow(gomock.Any(), uint(1), uint(2)).DoAndReturn(
			func(_ context.Context, _, _ uint) error {
				order = append(order, "mq")
				return nil
			})

		ownKey := f.followingCacheKey(1)
		f.mr.Set(ownKey, "cached")

		if err := f.svc.Unfollow(ctx, rel); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(order) != 2 || order[0] != "db" || order[1] != "mq" {
			t.Errorf("副作用顺序 = %v, want [db mq]", order)
		}
		if f.mr.Exists(ownKey) {
			t.Error("取关后关注者本人的 Following 缓存应被失效")
		}
	})

	t.Run("MQ 发布失败不影响取关成功", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)
		f.store.EXPECT().Unfollow(gomock.Any(), rel).Return(nil)
		f.pub.EXPECT().UnFollow(gomock.Any(), uint(1), uint(2)).
			Return(errors.New("social mq is not initialized"))

		if err := f.svc.Unfollow(ctx, rel); err != nil {
			t.Fatalf("MQ 不可用时取关必须仍然成功（降级），got %v", err)
		}
	})

	t.Run("取关不校验自我关系（与 Follow 不对称）", func(t *testing.T) {
		// 现状锁定：Follow 有 FollowerID == VloggerID 的显式拒绝，
		// Unfollow 没有；数据库中若存在自关注关系，取关会照常执行。
		f := newFixture(t)
		rel := &social.Social{FollowerID: 7, VloggerID: 7}
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7}, nil).Times(2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)
		f.store.EXPECT().Unfollow(gomock.Any(), rel).Return(nil)
		f.pub.EXPECT().UnFollow(gomock.Any(), uint(7), uint(7)).Return(nil)

		if err := f.svc.Unfollow(ctx, rel); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------- IsFollowed ----------

func TestIsFollowed(t *testing.T) {
	ctx := context.Background()

	t.Run("被关注者账号不存在时报错且不查关系", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		existedAccount(t, f, 1)
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(2)).Return(nil, notFound)

		if _, err := f.svc.IsFollowed(ctx, &social.Social{FollowerID: 1, VloggerID: 2}); !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("双方账号都存在时透出仓储结果", func(t *testing.T) {
		f := newFixture(t)
		rel := &social.Social{FollowerID: 1, VloggerID: 2}
		existedAccount(t, f, 1, 2)
		f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)

		got, err := f.svc.IsFollowed(ctx, rel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Error("expected IsFollowed = true")
		}
	})
}

// ---------- GetAllFollowers / GetAllVloggers ----------

func TestGetAllFollowers(t *testing.T) {
	ctx := context.Background()

	t.Run("账号不存在时报错且不查关系", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(2)).Return(nil, notFound)

		if _, err := f.svc.GetAllFollowers(ctx, 2); !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("账号存在时返回关注者列表", func(t *testing.T) {
		f := newFixture(t)
		followers := []*account.PublicAccount{{ID: 1, Username: "alice"}}
		existedAccount(t, f, 2)
		f.store.EXPECT().GetAllFollowers(gomock.Any(), uint(2)).Return(followers, nil)

		got, err := f.svc.GetAllFollowers(ctx, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Username != "alice" {
			t.Errorf("got %+v, want one follower alice", got)
		}
	})
}

func TestGetAllVloggers(t *testing.T) {
	ctx := context.Background()

	t.Run("账号不存在时报错且不查关系", func(t *testing.T) {
		f := newFixture(t)
		notFound := errors.New("record not found")
		f.accounts.EXPECT().FindByID(gomock.Any(), uint(1)).Return(nil, notFound)

		if _, err := f.svc.GetAllVloggers(ctx, 1); !errors.Is(err, notFound) {
			t.Fatalf("expected the lookup error to surface, got %v", err)
		}
	})

	t.Run("账号存在时返回关注列表", func(t *testing.T) {
		f := newFixture(t)
		vloggers := []*account.PublicAccount{{ID: 2, Username: "bob"}}
		existedAccount(t, f, 1)
		f.store.EXPECT().GetAllVloggers(gomock.Any(), uint(1)).Return(vloggers, nil)

		got, err := f.svc.GetAllVloggers(ctx, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Username != "bob" {
			t.Errorf("got %+v, want one vlogger bob", got)
		}
	})
}

// ---------- CountFollowers / CountVloggers ----------

func TestCounts(t *testing.T) {
	ctx := context.Background()

	t.Run("计数不校验账号是否存在，直接查仓储", func(t *testing.T) {
		f := newFixture(t)
		// 未设置 FindByID 期望 => handler 已从 token 拿到 accountID，
		// 计数路径不得为了计数再查一次账号。
		f.store.EXPECT().CountFollowers(gomock.Any(), uint(9)).Return(int64(3), nil)
		f.store.EXPECT().CountVloggers(gomock.Any(), uint(9)).Return(int64(5), nil)

		followers, err := f.svc.CountFollowers(ctx, 9)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		vloggers, err := f.svc.CountVloggers(ctx, 9)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if followers != 3 || vloggers != 5 {
			t.Errorf("got (followers=%d vloggers=%d), want (3, 5)", followers, vloggers)
		}
	})

	t.Run("MQ 不可用不影响计数", func(t *testing.T) {
		// CountFollowers / CountVloggers 不经过 MQ，也不经过账号查询：
		// 即便 MQ 未初始化，计数仍应正常返回。
		ctrl := gomock.NewController(t)
		store := mock.NewMockSocialStore(ctrl)
		store.EXPECT().CountFollowers(gomock.Any(), uint(9)).Return(int64(1), nil)
		store.EXPECT().CountVloggers(gomock.Any(), uint(9)).Return(int64(2), nil)
		svc := social.NewSocialService(store, nil, nil, nil)

		if n, err := svc.CountFollowers(ctx, 9); err != nil || n != 1 {
			t.Fatalf("CountFollowers = (%d, %v), want (1, nil)", n, err)
		}
		if n, err := svc.CountVloggers(ctx, 9); err != nil || n != 2 {
			t.Fatalf("CountVloggers = (%d, %v), want (2, nil)", n, err)
		}
	})

	t.Run("仓储错误原样透出", func(t *testing.T) {
		f := newFixture(t)
		dbErr := errors.New("connection refused")
		f.store.EXPECT().CountFollowers(gomock.Any(), uint(9)).Return(int64(0), dbErr)
		f.store.EXPECT().CountVloggers(gomock.Any(), uint(9)).Return(int64(0), dbErr)

		if _, err := f.svc.CountFollowers(ctx, 9); !errors.Is(err, dbErr) {
			t.Fatalf("CountFollowers: expected the repo error to surface, got %v", err)
		}
		if _, err := f.svc.CountVloggers(ctx, 9); !errors.Is(err, dbErr) {
			t.Fatalf("CountVloggers: expected the repo error to surface, got %v", err)
		}
	})
}
