package jwt_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/auth"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt/mock"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	jwtlib "github.com/golang-jwt/jwt/v5"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

const testSecret = "unit-test-secret"

func TestMain(m *testing.M) {
	// auth 在第二批改为必须显式注入密钥（不再静默随机生成）。
	if err := auth.SetSecret(testSecret); err != nil {
		fmt.Fprintf(os.Stderr, "auth.SetSecret: %v\n", err)
		os.Exit(1)
	}
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
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

// validToken 走被测代码自己的签发路径（HS256 + 正确密钥 + 未过期）。
func validToken(t *testing.T, accountID uint, username string) string {
	t.Helper()
	token, err := auth.GenerateToken(accountID, username)
	if err != nil {
		t.Fatalf("auth.GenerateToken: %v", err)
	}
	return token
}

// forgedToken 手工签发 token，用于构造签名/算法/过期时间异常。
func forgedToken(t *testing.T, method jwtlib.SigningMethod, key any, expiresAt time.Time) string {
	t.Helper()
	claims := auth.Claims{
		AccountID: 1,
		Username:  "alice",
		RegisteredClaims: jwtlib.RegisteredClaims{
			ExpiresAt: jwtlib.NewNumericDate(expiresAt),
			IssuedAt:  jwtlib.NewNumericDate(time.Now().Add(-2 * time.Minute)),
			NotBefore: jwtlib.NewNumericDate(time.Now().Add(-2 * time.Minute)),
		},
	}
	signed, err := jwtlib.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign forged token: %v", err)
	}
	return signed
}

// probe 记录业务 handler 是否执行、以及从 gin.Context 里读到的身份。
type probe struct {
	calls     int
	bound     bool
	accountID uint
	username  string
}

func probeHandler(p *probe) gin.HandlerFunc {
	return func(c *gin.Context) {
		p.calls++
		if id, err := jwt.GetAccountID(c); err == nil {
			p.bound = true
			p.accountID = id
		}
		if name, err := jwt.GetUsername(c); err == nil {
			p.username = name
		}
		c.Status(http.StatusOK)
	}
}

// newRouter 构造最小路由：一个中间件 + 一个业务 handler。
func newRouter(mw gin.HandlerFunc) (*gin.Engine, *probe) {
	p := &probe{}
	r := gin.New()
	r.GET("/protected", mw, probeHandler(p))
	return r, p
}

func serve(r *gin.Engine, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// tamperSignature 返回一个签名确实被破坏过的 token（其余部分保持合法）。
//
// 不要用「替换最后一个字符」来篡改：HS256 的签名是 32 字节，其 base64url
// （无填充）表示恰好 43 字符，而 43*6=258 bit 只承载 256 bit 数据——最后一个
// 字符的低 2 位会被解码器忽略。因此有 4 个不同字符解码出**完全相同**的字节，
// 末位替换约有 1/16 的概率根本没改到签名，token 依然有效，用例便偶发失败
// （本文件曾因此 flaky，实测该 43 字符串确有 3 个其他字符解码结果与之相同）。
//
// 改中间字符则 6 位全部有效，必然改变解码结果；函数末尾还会自校验一次，
// 确保"篡改"没有变成空操作。
func tamperSignature(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token 不是三段式 JWT: %q", token)
	}
	i := len(token) - 10 // 落在签名中后段，且避开位数不完整的末位字符
	if i <= len(parts[0])+len(parts[1])+1 {
		t.Fatalf("token 过短，无法在签名内部篡改: %q", token)
	}
	replacement := byte('A')
	if token[i] == 'A' {
		replacement = 'B'
	}
	out := token[:i] + string(replacement) + token[i+1:]

	// 自校验：确认签名字节真的变了。否则这个用例会以"token 被篡改"之名
	// 验证一个未被篡改的 token，无论通过还是失败都是误导。
	before, err1 := base64.RawURLEncoding.DecodeString(parts[2])
	after, err2 := base64.RawURLEncoding.DecodeString(strings.Split(out, ".")[2])
	if err1 != nil || err2 != nil {
		t.Fatalf("签名不是合法的 base64url: %v / %v", err1, err2)
	}
	if string(before) == string(after) {
		t.Fatal("篡改后签名字节未改变，用例将失去意义")
	}
	return out
}

// ---------- JWTAuth：格式/签名/过期 ----------

func TestJWTAuthRejectsUntrustedTokens(t *testing.T) {
	t.Run("无 token、格式错误、签名错误、过期一律 401 且不进业务逻辑", func(t *testing.T) {
		cache, _ := newCache(t)
		valid := validToken(t, 1, "alice")
		tampered := tamperSignature(t, valid)

		cases := []struct {
			name   string
			header string
		}{
			{"缺少 Authorization 头", ""},
			{"Authorization 不是 Bearer 方案", "Basic " + valid},
			{"Bearer 后 token 为空", "Bearer "},
			{"token 被篡改（签名不匹配）", "Bearer " + tampered},
			{"使用错误密钥签名", "Bearer " + forgedToken(t, jwtlib.SigningMethodHS256, []byte("wrong-secret"), time.Now().Add(time.Hour))},
			{"算法混淆：alg=none", "Bearer " + forgedToken(t, jwtlib.SigningMethodNone, jwtlib.UnsafeAllowNoneSignatureType, time.Now().Add(time.Hour))},
			{"算法混淆：HS512 签名", "Bearer " + forgedToken(t, jwtlib.SigningMethodHS512, []byte(testSecret), time.Now().Add(time.Hour))},
			{"已过期的 token", "Bearer " + forgedToken(t, jwtlib.SigningMethodHS256, []byte(testSecret), time.Now().Add(-time.Minute))},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctrl := gomock.NewController(t)
				lookup := mock.NewMockAccountLookup(ctrl)
				// 未设置 FindByID 期望 => 这一类请求压根不该走到吊销校验。

				r, p := newRouter(jwt.JWTAuth(lookup, cache))
				w := serve(r, "/protected", tc.header)

				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", w.Code)
				}
				if p.calls != 0 {
					t.Errorf("业务 handler 被执行了 %d 次，未通过鉴权的请求必须被拦截", p.calls)
				}
			})
		}
	})
}

// ---------- JWTAuth：吊销校验（第一批 T1 的核心缺陷） ----------

func TestJWTAuthTokenRevocation(t *testing.T) {
	t.Run("签名有效但账号已登出（缓存无键、DB token 为空）→ 401", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		token := validToken(t, 1, "alice")
		// account.Logout 会先删缓存键、再把 DB 里的 token 置空。
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).
			Return(&account.Account{ID: 1, Username: "alice", Token: ""}, nil)

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（已登出的 token 不得再被接受）", w.Code)
		}
		if !strings.Contains(w.Body.String(), "revoked") {
			t.Errorf("body = %q, want a token-revoked error", w.Body.String())
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次，已吊销的 token 必须被拦截", p.calls)
		}
	})

	t.Run("缓存中的 token 与请求 token 不一致 → 401 且不回查数据库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		old := validToken(t, 1, "alice")
		// 该账号已重新登录/改名，缓存里是更新的 token。
		mr.Set("test:account:1", "a-newer-token")
		// 未设置 FindByID 期望 => 缓存命中且不一致时必须直接拒绝。

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+old)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("缓存未命中但 DB 中已是更新的 token（改名/重新登录）→ 401", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		old := validToken(t, 1, "alice")
		// 改名 / 重新登录会写入新 token；缓存若被清空（Redis 重启、
		// key 过期、多副本未同步），只剩 DB 兜底，旧 token 必须被拒。
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).
			Return(&account.Account{ID: 1, Username: "alice", Token: "a-newer-token"}, nil)

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+old)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（DB 中 token 已更新，旧 token 必须失效）", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("缓存未命中但 DB token 一致 → 200 并回填缓存", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 7, "alice")
		lookup.EXPECT().FindByID(gomock.Any(), uint(7)).
			Return(&account.Account{ID: 7, Username: "alice", Token: token}, nil)

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（Redis 未命中但 DB 中 token 仍然有效）", w.Code)
		}
		if p.calls != 1 || !p.bound || p.accountID != 7 || p.username != "alice" {
			t.Errorf("bound = %+v, want accountID=7 username=alice 且 handler 执行一次", p)
		}
		if got, _ := mr.Get("test:account:7"); got != token {
			t.Errorf("cache = %q, want 回填请求所用的 token", got)
		}
	})

	t.Run("缓存命中且一致 → 200 且不回查数据库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 7, "alice")
		mr.Set("test:account:7", token)
		// 未设置 FindByID 期望 => 缓存命中就该短路返回。

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if p.calls != 1 || p.accountID != 7 || p.username != "alice" {
			t.Errorf("bound = %+v, want accountID=7 username=alice", p)
		}
	})

	t.Run("DB 中账号已不存在 → 401", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		token := validToken(t, 1, "alice")
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).Return(nil, fmt.Errorf("record not found"))

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("Redis 故障时回退 DB：DB token 已清空仍 401", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 1, "alice")
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).
			Return(&account.Account{ID: 1, Username: "alice", Token: ""}, nil)
		mr.Close() // Redis 整体不可用

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（Redis 故障不得绕过吊销校验）", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("Redis 故障时回退 DB：DB token 一致 → 200", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 1, "alice")
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).
			Return(&account.Account{ID: 1, Username: "alice", Token: token}, nil)
		mr.Close()

		r, p := newRouter(jwt.JWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（Redis 故障时 DB 兜底仍可放行有效 token）", w.Code)
		}
		if p.calls != 1 || p.accountID != 1 {
			t.Errorf("bound = %+v, want accountID=1", p)
		}
	})
}

// ---------- SoftJWTAuth：匿名放行 + 带 token 时同样严格 ----------

func TestSoftJWTAuth(t *testing.T) {
	t.Run("无 Authorization 头必须放行并调用 c.Next", func(t *testing.T) {
		cache, _ := newCache(t)
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		// 未设置 FindByID 期望 => 匿名请求不得触发任何账号查询。

		r, p := newRouter(jwt.SoftJWTAuth(lookup, cache))
		w := serve(r, "/protected", "")

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（SoftJWTAuth 在无 token 时必须放行匿名访问）", w.Code)
		}
		if p.calls != 1 {
			t.Fatalf("业务 handler 执行了 %d 次, want 1（c.Next 必须被调用）", p.calls)
		}
		if p.bound {
			t.Error("匿名请求不应绑定任何身份")
		}
	})

	t.Run("带无效 token → 401 而不是静默降级为匿名", func(t *testing.T) {
		cache, _ := newCache(t)
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		valid := validToken(t, 1, "alice")
		tampered := tamperSignature(t, valid)

		r, p := newRouter(jwt.SoftJWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+tampered)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("带已吊销 token → 401", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		token := validToken(t, 1, "alice")
		lookup.EXPECT().FindByID(gomock.Any(), uint(1)).
			Return(&account.Account{ID: 1, Token: ""}, nil)

		r, p := newRouter(jwt.SoftJWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（SoftJWTAuth 也必须做吊销校验）", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("业务 handler 被执行了 %d 次", p.calls)
		}
	})

	t.Run("带有效 token → 200 并绑定身份", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 3, "carol")
		mr.Set("test:account:3", token)

		r, p := newRouter(jwt.SoftJWTAuth(lookup, cache))
		w := serve(r, "/protected", "Bearer "+token)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if p.calls != 1 || !p.bound || p.accountID != 3 || p.username != "carol" {
			t.Errorf("bound = %+v, want accountID=3 username=carol", p)
		}
	})
}

// ---------- CheckAndBind：只校验+绑定，不负责 c.Next ----------

func TestCheckAndBind(t *testing.T) {
	t.Run("校验成功时绑定身份且不查数据库（缓存命中）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, mr := newCache(t)
		token := validToken(t, 5, "erin")
		mr.Set("test:account:5", token)
		claims, err := auth.ParseToken(token)
		if err != nil {
			t.Fatalf("ParseToken: %v", err)
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/probe", nil)

		if err := jwt.CheckAndBind(c, claims, token, lookup, cache); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id, err := jwt.GetAccountID(c); err != nil || id != 5 {
			t.Errorf("accountID = (%d, %v), want (5, nil)", id, err)
		}
		if name, err := jwt.GetUsername(c); err != nil || name != "erin" {
			t.Errorf("username = (%q, %v), want (\"erin\", nil)", name, err)
		}
	})

	t.Run("校验失败时不绑定身份并返回吊销错误", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		token := validToken(t, 5, "erin")
		lookup.EXPECT().FindByID(gomock.Any(), uint(5)).
			Return(&account.Account{ID: 5, Token: ""}, nil)
		claims, err := auth.ParseToken(token)
		if err != nil {
			t.Fatalf("ParseToken: %v", err)
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/probe", nil)

		err = jwt.CheckAndBind(c, claims, token, lookup, cache)
		if err == nil {
			t.Fatal("expected an error for a revoked token, got nil")
		}
		if !strings.Contains(err.Error(), "revoked") {
			t.Errorf("error = %v, want a revoked-token error", err)
		}
		if _, err := jwt.GetAccountID(c); err == nil {
			t.Error("校验失败时不得绑定 accountID")
		}
		if _, err := jwt.GetUsername(c); err == nil {
			t.Error("校验失败时不得绑定 username")
		}
	})

	t.Run("不调用 c.Next：业务 handler 必须在绑定返回之后才执行", func(t *testing.T) {
		// CheckAndBind 有两条成功返回路径（缓存命中 / DB 兜底），
		// 两条都必须只校验+绑定；任何一条里加了 c.Next() 都会被下面抓到。
		cases := []struct {
			name      string
			warmCache bool
		}{
			{"缓存命中路径", true},
			{"DB 兜底路径（Redis 未命中）", false},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctrl := gomock.NewController(t)
				lookup := mock.NewMockAccountLookup(ctrl)
				cache, mr := newCache(t)
				token := validToken(t, 5, "erin")
				if tc.warmCache {
					mr.Set("test:account:5", token)
				} else {
					lookup.EXPECT().FindByID(gomock.Any(), uint(5)).
						Return(&account.Account{ID: 5, Username: "erin", Token: token}, nil)
				}
				claims, err := auth.ParseToken(token)
				if err != nil {
					t.Fatalf("ParseToken: %v", err)
				}

				var order []string
				r := gin.New()
				r.GET("/probe", func(c *gin.Context) {
					// 模拟调用方：CheckAndBind 只负责校验+绑定，c.Next() 由调用方决定。
					if err := jwt.CheckAndBind(c, claims, token, lookup, cache); err != nil {
						c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
						return
					}
					order = append(order, "CheckAndBind 返回")
				}, func(c *gin.Context) {
					order = append(order, "业务 handler")
					c.Status(http.StatusOK)
				})

				w := serve(r, "/probe", "")
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", w.Code)
				}
				if len(order) != 2 || order[0] != "CheckAndBind 返回" || order[1] != "业务 handler" {
					t.Fatalf("执行顺序 = %v, want [CheckAndBind 返回 业务 handler]；"+
						"若 CheckAndBind 内部调用了 c.Next，业务 handler 会提前执行", order)
				}
			})
		}
	})

	t.Run("校验失败后调用方 Abort，业务 handler 不执行", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lookup := mock.NewMockAccountLookup(ctrl)
		cache, _ := newCache(t)
		token := validToken(t, 5, "erin")
		lookup.EXPECT().FindByID(gomock.Any(), uint(5)).
			Return(&account.Account{ID: 5, Token: ""}, nil)
		claims, err := auth.ParseToken(token)
		if err != nil {
			t.Fatalf("ParseToken: %v", err)
		}

		p := &probe{}
		r := gin.New()
		r.GET("/probe", func(c *gin.Context) {
			if err := jwt.CheckAndBind(c, claims, token, lookup, cache); err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
		}, probeHandler(p))

		w := serve(r, "/probe", "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if p.calls != 0 {
			t.Errorf("校验失败后业务 handler 仍执行了 %d 次", p.calls)
		}
	})
}
