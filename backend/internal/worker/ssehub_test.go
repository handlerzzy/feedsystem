package worker_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/auth"
	jwtmock "github.com/handlerzzy/feedsystem/internal/middleware/jwt/mock"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/worker"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

const sseTestSecret = "sse-unit-test-secret"

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	// auth 自第二批起必须显式注入密钥（不再静默随机生成）。
	if err := auth.SetSecret(sseTestSecret); err != nil {
		fmt.Fprintf(os.Stderr, "auth.SetSecret: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func sseNewCache(t *testing.T) (*rediscache.Client, *miniredis.Miniredis) {
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

// sseRequest 用给定的 token 位置发起一次请求，返回状态码、响应体与被保护
// handler 是否真的被执行到。
//
// reached 是这里最关键的一个断言维度：401 必须意味着业务 handler 没有执行。
// 只断言状态码会漏掉"既 Abort 又继续跑"这类缺陷。
func sseRequest(t *testing.T, repo *jwtmock.MockAccountLookup, cache *rediscache.Client,
	headerToken, queryToken string) (int, string, bool) {

	t.Helper()
	hub := worker.NewSSEHub(nil)
	reached := false

	r := gin.New()
	g := r.Group("/notification")
	g.Use(hub.SSERequireAuth(repo, cache))
	g.GET("/stream", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	target := "/notification/stream"
	if queryToken != "" {
		target += "?token=" + queryToken
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if headerToken != "" {
		req.Header.Set("Authorization", "Bearer "+headerToken)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	var parsed map[string]string
	if json.Unmarshal(w.Body.Bytes(), &parsed) == nil && parsed["error"] != "" {
		body = parsed["error"]
	}
	return w.Code, body, reached
}

func TestSSERequireAuthRejectsMissingOrMalformedToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := jwtmock.NewMockAccountLookup(ctrl)
	cache, _ := sseNewCache(t)

	cases := []struct {
		name        string
		headerToken string
		queryToken  string
	}{
		{name: "完全没有 token"},
		{name: "token 是随机串", headerToken: "not-a-jwt"},
		{name: "token 只有两段", headerToken: "aaa.bbb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, reached := sseRequest(t, repo, cache, tc.headerToken, tc.queryToken)
			if code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", code)
			}
			if reached {
				t.Error("被保护的 handler 不得执行")
			}
			if body == "" {
				t.Error("响应体应说明失败原因")
			}
		})
	}
}

func TestSSERequireAuthRejectsWronglySignedToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := jwtmock.NewMockAccountLookup(ctrl)
	cache, _ := sseNewCache(t)

	// 用另一把密钥签一个结构完全合法的 token。
	if err := auth.SetSecret("a-different-secret"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	foreign, err := auth.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if err := auth.SetSecret(sseTestSecret); err != nil {
		t.Fatalf("restore SetSecret: %v", err)
	}

	code, body, reached := sseRequest(t, repo, cache, foreign, "")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", code, body)
	}
	if reached {
		t.Error("被保护的 handler 不得执行")
	}
}

func TestSSERequireAuthAcceptsValidToken(t *testing.T) {
	token, err := auth.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	t.Run("Authorization 头（缓存命中且一致）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, mr := sseNewCache(t)
		mr.Set("test:account:7", token)

		code, body, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", code, body)
		}
		if !reached {
			t.Error("合法 token 必须放行到业务 handler")
		}
	})

	// EventSource 无法自定义 header，?token= 是 README 文档化的回退方式。
	// 它同样必须走完整的吊销校验，而不是降级成只验签名。
	t.Run("query 参数回退（缓存命中且一致）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, mr := sseNewCache(t)
		mr.Set("test:account:7", token)

		code, body, reached := sseRequest(t, repo, cache, "", token)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", code, body)
		}
		if !reached {
			t.Error("合法 token 必须放行到业务 handler")
		}
	})

	t.Run("缓存未命中时回退数据库并回填", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, mr := sseNewCache(t)
		repo.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7, Username: "alice", Token: token}, nil)

		code, body, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", code, body)
		}
		if !reached {
			t.Error("合法 token 必须放行到业务 handler")
		}
		if got, _ := mr.Get("test:account:7"); got != token {
			t.Errorf("缓存未回填：got %q", got)
		}
	})
}

// TestSSERequireAuthRejectsRevokedToken 是本文件的核心。
//
// 第一批修的"SSE 鉴权旁路"缺陷是：/notification/* 原先只做 ParseToken（验签名），
// 不做吊销校验，因此用户 logout 之后，其旧 token 仍能订阅 SSE 收到实时通知。
// 下面三种"签名完全合法但已被吊销"的情形必须全部 401。
func TestSSERequireAuthRejectsRevokedToken(t *testing.T) {
	token, err := auth.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	t.Run("缓存里的 token 已被替换（重新登录或登出）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, mr := sseNewCache(t)
		mr.Set("test:account:7", "a-newer-token")
		// 缓存命中且不一致时不得回查数据库。

		code, body, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%q", code, body)
		}
		if reached {
			t.Error("已吊销的 token 绝不能进入业务 handler")
		}
	})

	t.Run("缓存无键、数据库中 token 已被清空（已登出）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, _ := sseNewCache(t)
		// Token 为空即"已登出"，这是第一批 T1 修复要挡住的主路径。
		repo.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7, Username: "alice", Token: ""}, nil)

		code, body, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%q", code, body)
		}
		if reached {
			t.Error("已登出的账号不得收到 SSE 通知")
		}
	})

	t.Run("缓存无键、数据库中是新 token", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, _ := sseNewCache(t)
		repo.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7, Username: "alice", Token: "a-newer-token"}, nil)

		code, body, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%q", code, body)
		}
		if reached {
			t.Error("被替换掉的旧 token 不得进入业务 handler")
		}
	})

	t.Run("账号已不存在", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, _ := sseNewCache(t)
		repo.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(nil, errors.New("record not found"))

		code, _, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
		if reached {
			t.Error("账号不存在时不得放行")
		}
	})

	// 与 jwt.JWTAuth 保持一致：Redis 不可用时回退数据库，而不是一律拒绝。
	// SSE 是长连接，若 Redis 抖动就断开所有订阅，影响面比普通接口大得多。
	t.Run("Redis 故障时回退数据库：token 一致则放行", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := jwtmock.NewMockAccountLookup(ctrl)
		cache, mr := sseNewCache(t)
		mr.Close() // 制造 Redis 故障
		repo.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7, Username: "alice", Token: token}, nil)

		code, _, reached := sseRequest(t, repo, cache, token, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200（应回退数据库而不是拒绝）", code)
		}
		if !reached {
			t.Error("Redis 故障且 DB 中 token 有效时必须放行")
		}
	})
}
