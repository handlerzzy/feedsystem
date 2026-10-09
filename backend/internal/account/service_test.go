package account_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/account/mock"
	"github.com/handlerzzy/feedsystem/internal/auth"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	// auth 在第二批改为必须显式注入密钥（不再静默随机生成）。
	if err := auth.SetSecret("unit-test-secret"); err != nil {
		fmt.Fprintf(os.Stderr, "auth.SetSecret: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// dupUsernameErr 模拟 MySQL 唯一键冲突（错误码 1062）。
func dupUsernameErr() error {
	return &mysql.MySQLError{
		Number:  1062,
		Message: "Duplicate entry 'alice' for key 'accounts.username'",
	}
}

// hashPassword 生成一个可用于测试的 bcrypt 哈希。
func hashPassword(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
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

// ---------- CreateAccount ----------

func TestCreateAccount(t *testing.T) {
	t.Run("空白用户名被拒绝且不落库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		err := svc.CreateAccount(context.Background(), &account.Account{
			Username: "   ",
			Password: "secret123",
		})
		if err == nil {
			t.Fatal("expected error for blank username, got nil")
		}
		// 未设置 CreateAccount 期望 => 一旦被调用 gomock 会让测试失败。
	})

	t.Run("密码以 bcrypt 哈希存储而非明文", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		const plain = "secret123"
		store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, a *account.Account) error {
				if a.Password == plain {
					t.Error("password stored in plaintext")
				}
				if err := bcrypt.CompareHashAndPassword([]byte(a.Password), []byte(plain)); err != nil {
					t.Errorf("stored hash does not verify against original password: %v", err)
				}
				return nil
			})

		if err := svc.CreateAccount(context.Background(), &account.Account{
			Username: "alice",
			Password: plain,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("唯一键冲突映射为 ErrUsernameTaken", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).Return(dupUsernameErr())

		err := svc.CreateAccount(context.Background(), &account.Account{
			Username: "alice",
			Password: "secret123",
		})
		if !errors.Is(err, account.ErrUsernameTaken) {
			t.Fatalf("expected ErrUsernameTaken, got %v", err)
		}
	})

	t.Run("其他数据库错误原样透出", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		dbErr := errors.New("connection refused")
		store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).Return(dbErr)

		err := svc.CreateAccount(context.Background(), &account.Account{
			Username: "alice",
			Password: "secret123",
		})
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected original db error, got %v", err)
		}
		if errors.Is(err, account.ErrUsernameTaken) {
			t.Fatal("non-1062 error must not be reported as username conflict")
		}
	})
}

// ---------- ChangePassword ----------

func TestChangePasswordByID(t *testing.T) {
	const oldPass, newPass = "old-pass-123", "new-pass-456"

	t.Run("旧密码错误时不写入且不吊销 token", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().FindByID(gomock.Any(), uint(1)).Return(&account.Account{
			ID:       1,
			Username: "alice",
			Password: hashPassword(t, oldPass),
		}, nil)
		// 未设置 ChangePassword / Logout 期望 => 被调用即失败。

		err := svc.ChangePasswordByID(context.Background(), 1, "wrong-old-pass", newPass)
		if err == nil {
			t.Fatal("expected error for wrong old password, got nil")
		}
	})

	t.Run("旧密码正确时写入新哈希并吊销 token", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		loggedIn := &account.Account{
			ID:           1,
			Username:     "alice",
			Password:     hashPassword(t, oldPass),
			Token:        "old-access-token",
			RefreshToken: "old-refresh-token",
		}
		// changePassword 内部：FindByID 取账号，成功后调用 Logout -> 再次 FindByID。
		store.EXPECT().FindByID(gomock.Any(), uint(1)).Return(loggedIn, nil).Times(2)

		store.EXPECT().ChangePassword(gomock.Any(), uint(1), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ uint, hash string) error {
				if hash == newPass {
					t.Error("new password stored in plaintext")
				}
				if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(newPass)); err != nil {
					t.Errorf("stored hash does not verify against new password: %v", err)
				}
				return nil
			})
		store.EXPECT().Logout(gomock.Any(), uint(1)).Return(nil)

		// 预置缓存，验证改密后被清除。
		mr.Set("test:account:1", "old-access-token")
		mr.Set("test:account:1:refresh", "old-refresh-token")
		mr.Set("test:refresh:old-refresh-token", "1")

		if err := svc.ChangePasswordByID(context.Background(), 1, oldPass, newPass); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, k := range []string{
			"test:account:1",
			"test:account:1:refresh",
			"test:refresh:old-refresh-token",
		} {
			if mr.Exists(k) {
				t.Errorf("cache key %q should have been deleted after password change", k)
			}
		}
	})
}

// ---------- Login ----------

func TestLogin(t *testing.T) {
	const pass = "correct-horse"

	setup := func(t *testing.T) (*mock.MockAccountStore, *miniredis.Miniredis, *account.AccountService) {
		t.Helper()
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		return store, mr, account.NewAccountService(store, cache)
	}

	t.Run("密码错误时不签发 token 也不写缓存", func(t *testing.T) {
		store, mr, svc := setup(t)
		store.EXPECT().FindByUsername(gomock.Any(), "alice").Return(&account.Account{
			ID:       1,
			Username: "alice",
			Password: hashPassword(t, pass),
		}, nil)
		// 未设置 Login 期望 => 被调用即失败。

		if _, _, err := svc.Login(context.Background(), "alice", "wrong-password"); err == nil {
			t.Fatal("expected error for wrong password, got nil")
		}
		if keys := mr.Keys(); len(keys) != 0 {
			t.Errorf("no cache key should be written on failed login, got %v", keys)
		}
	})

	t.Run("用户不存在时报错", func(t *testing.T) {
		store, _, svc := setup(t)
		store.EXPECT().FindByUsername(gomock.Any(), "ghost").
			Return(nil, errors.New("record not found"))

		if _, _, err := svc.Login(context.Background(), "ghost", pass); err == nil {
			t.Fatal("expected error for unknown user, got nil")
		}
	})

	t.Run("成功后签发可解析的 token 并回填缓存", func(t *testing.T) {
		store, mr, svc := setup(t)
		store.EXPECT().FindByUsername(gomock.Any(), "alice").Return(&account.Account{
			ID:       7,
			Username: "alice",
			Password: hashPassword(t, pass),
		}, nil)
		store.EXPECT().Login(gomock.Any(), uint(7), gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ uint, token, refresh string) error {
				if token == "" || refresh == "" {
					t.Error("repo.Login received empty token")
				}
				return nil
			})

		access, refresh, err := svc.Login(context.Background(), "alice", pass)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		claims, err := auth.ParseToken(access)
		if err != nil {
			t.Fatalf("issued access token does not parse: %v", err)
		}
		if claims.AccountID != 7 || claims.Username != "alice" {
			t.Errorf("access token claims = {%d %q}, want {7 \"alice\"}", claims.AccountID, claims.Username)
		}

		for _, k := range []string{
			"test:account:7",
			"test:account:7:refresh",
			"test:refresh:" + refresh,
		} {
			if !mr.Exists(k) {
				t.Errorf("expected cache key %q to be populated after login", k)
			}
		}
	})
}

// ---------- Logout ----------

func TestLogout(t *testing.T) {
	t.Run("已登出账号直接返回且不触碰数据库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().FindByID(gomock.Any(), uint(1)).Return(&account.Account{
			ID:    1,
			Token: "",
		}, nil)
		// 未设置 Logout 期望 => 被调用即失败。

		if err := svc.Logout(context.Background(), 1); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("正常登出清除三处缓存并清空 token", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		store.EXPECT().FindByID(gomock.Any(), uint(1)).Return(&account.Account{
			ID:           1,
			Token:        "access-1",
			RefreshToken: "refresh-1",
		}, nil)
		store.EXPECT().Logout(gomock.Any(), uint(1)).Return(nil)

		mr.Set("test:account:1", "access-1")
		mr.Set("test:account:1:refresh", "refresh-1")
		mr.Set("test:refresh:refresh-1", "1")

		if err := svc.Logout(context.Background(), 1); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, k := range []string{
			"test:account:1",
			"test:account:1:refresh",
			"test:refresh:refresh-1",
		} {
			if mr.Exists(k) {
				t.Errorf("cache key %q should have been deleted on logout", k)
			}
		}
	})
}

// ---------- RefreshAccessToken ----------

func TestRefreshAccessToken(t *testing.T) {
	t.Run("空 token 直接报错", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		svc := account.NewAccountService(mock.NewMockAccountStore(ctrl), nil)

		if _, _, _, err := svc.RefreshAccessToken(context.Background(), ""); err == nil {
			t.Fatal("expected error for empty refresh token, got nil")
		}
	})

	t.Run("未知 token 视为无效", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, _ := newCache(t)
		svc := account.NewAccountService(store, cache)

		if _, _, _, err := svc.RefreshAccessToken(context.Background(), "never-issued"); err == nil {
			t.Fatal("expected error for unknown refresh token, got nil")
		}
	})

	t.Run("缓存中的 account 与 token 不匹配时视为已吊销", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		mr.Set("test:refresh:stale-token", "7")
		store.EXPECT().FindByID(gomock.Any(), uint(7)).Return(&account.Account{
			ID:           7,
			Username:     "alice",
			RefreshToken: "a-newer-token",
		}, nil)
		// 未设置 UpdateToken 期望 => 被调用即失败。

		if _, _, _, err := svc.RefreshAccessToken(context.Background(), "stale-token"); err == nil {
			t.Fatal("expected error for revoked refresh token, got nil")
		}
	})

	t.Run("有效 token 换发新 access token 并落库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		mr.Set("test:refresh:good-token", "7")
		store.EXPECT().FindByID(gomock.Any(), uint(7)).Return(&account.Account{
			ID:           7,
			Username:     "alice",
			RefreshToken: "good-token",
		}, nil)
		store.EXPECT().UpdateToken(gomock.Any(), uint(7), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ uint, token string) error {
				claims, err := auth.ParseToken(token)
				if err != nil {
					t.Errorf("persisted access token does not parse: %v", err)
					return nil
				}
				if claims.AccountID != 7 || claims.Username != "alice" {
					t.Errorf("persisted token claims = {%d %q}, want {7 \"alice\"}",
						claims.AccountID, claims.Username)
				}
				return nil
			})

		newToken, id, username, err := svc.RefreshAccessToken(context.Background(), "good-token")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != 7 || username != "alice" {
			t.Errorf("got (id=%d username=%q), want (7, \"alice\")", id, username)
		}
		if newToken == "" {
			t.Error("expected a non-empty access token")
		}
		if got, _ := mr.Get("test:account:7"); got != newToken {
			t.Errorf("cache holds %q, want the newly issued token", got)
		}
	})
}

// ---------- UpdateProfile ----------

func TestUpdateProfile(t *testing.T) {
	t.Run("两个字段都为空时报错", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		// 未设置 UpdateFields 期望 => 被调用即失败。
		if err := svc.UpdateProfile(context.Background(), 1, &account.UpdateProfileRequest{}); err == nil {
			t.Fatal("expected error when nothing to update, got nil")
		}
	})

	t.Run("仅传 bio 时只更新 bio 并去除首尾空白", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().UpdateFields(gomock.Any(), uint(1),
			map[string]interface{}{"bio": "hello world"}).Return(nil)

		err := svc.UpdateProfile(context.Background(), 1, &account.UpdateProfileRequest{
			Bio: "  hello world  ",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("同时传 avatar_url 与 bio 时两个字段都更新", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().UpdateFields(gomock.Any(), uint(1), map[string]interface{}{
			"bio":        "hi",
			"avatar_url": "https://cdn.example.com/a.png",
		}).Return(nil)

		err := svc.UpdateProfile(context.Background(), 1, &account.UpdateProfileRequest{
			Bio:       "hi",
			AvatarURL: "https://cdn.example.com/a.png",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------- Rename ----------

func TestRename(t *testing.T) {
	t.Run("新用户名为空时报错", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		if _, err := svc.Rename(context.Background(), 1, ""); !errors.Is(err, account.ErrNewUsernameRequired) {
			t.Fatalf("expected ErrNewUsernameRequired, got %v", err)
		}
	})

	t.Run("新用户名冲突映射为 ErrUsernameTaken", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		svc := account.NewAccountService(store, nil)

		store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).
			Return(dupUsernameErr())

		_, err := svc.Rename(context.Background(), 1, "bob")
		if !errors.Is(err, account.ErrUsernameTaken) {
			t.Fatalf("expected ErrUsernameTaken, got %v", err)
		}
	})

	t.Run("成功后签发的新 token 携带新用户名并刷新缓存", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).
			DoAndReturn(func(_ context.Context, _ uint, _ string, token string) error {
				claims, err := auth.ParseToken(token)
				if err != nil {
					t.Errorf("token passed to repo does not parse: %v", err)
					return nil
				}
				if claims.Username != "bob" {
					t.Errorf("token carries username %q, want \"bob\"", claims.Username)
				}
				return nil
			})

		token, err := svc.Rename(context.Background(), 1, "bob")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, _ := mr.Get("test:account:1"); got != token {
			t.Errorf("cache holds %q, want the newly issued token", got)
		}
	})

	t.Run("缓存写入失败不影响改名成功", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockAccountStore(ctrl)
		cache, mr := newCache(t)
		svc := account.NewAccountService(store, cache)

		store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).Return(nil)
		// 关掉 Redis：缓存写入必然失败，但改名本身应当成功。
		mr.Close()

		if _, err := svc.Rename(context.Background(), 1, "bob"); err != nil {
			t.Fatalf("cache failure must not fail the rename, got %v", err)
		}
	})
}
