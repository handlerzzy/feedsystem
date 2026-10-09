package logging_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/logging"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// capture 换入一个可观测的 logger，返回收集到的日志与恢复函数。
func capture(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	prev := logging.SetLogger(zap.New(core))
	t.Cleanup(func() { logging.SetLogger(prev) })
	return logs
}

// runRequest 走一遍给定中间件 + 业务 handler。
func runRequest(mw gin.HandlerFunc, status int) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(mw)
	r.GET("/probe", func(c *gin.Context) { c.Status(status) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return w
}

// ---------- Init ----------

func TestInitRejectsInvalidConfig(t *testing.T) {
	prev := logging.SetLogger(logging.L())
	defer logging.SetLogger(prev)

	cases := []struct {
		name string
		cfg  logging.Config
		want string // 报错里应提到哪个字段
	}{
		{"非法级别", logging.Config{Level: "verbose"}, "level"},
		{"非法格式", logging.Config{Format: "xml"}, "format"},
		{"非法输出", logging.Config{Output: "/var/log/x.log"}, "output"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := logging.Init(tc.cfg)
			if err == nil {
				t.Fatalf("期望报错，实际通过；配置错误被静默接受会让运维以为生效了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应指明是哪个字段(%q)，实际: %v", tc.want, err)
			}
		})
	}
}

func TestInitAcceptsValidAndEmptyConfig(t *testing.T) {
	prev := logging.SetLogger(logging.L())
	defer logging.SetLogger(prev)

	// 空配置必须可用：日志配置不该成为进程起不来的原因。
	for _, cfg := range []logging.Config{
		{},
		{Level: "debug", Format: "console", Output: "stdout"},
		{Level: "warn", Format: "json", Output: "stderr"},
		{Level: "INFO"}, // 大小写不敏感
	} {
		if err := logging.Init(cfg); err != nil {
			t.Errorf("Init(%+v) = %v, want nil", cfg, err)
		}
		if logging.L() == nil {
			t.Fatal("Init 之后 L() 不应为 nil")
		}
	}
}

// ---------- RequestID ----------

func TestRequestIDGeneratesAndPropagates(t *testing.T) {
	capture(t)
	w := runRequest(logging.RequestID(), http.StatusOK)

	got := w.Header().Get(logging.RequestIDHeader)
	if got == "" {
		t.Fatal("响应头应带上 X-Request-ID，否则客户端报障时无从提供")
	}
	if len(got) != 32 {
		t.Errorf("自动生成的 ID 长度 = %d, want 32（16 字节 hex）", len(got))
	}
}

func TestRequestIDHonorsValidUpstreamValue(t *testing.T) {
	capture(t)
	r := gin.New()
	r.Use(logging.RequestID())
	var seen string
	r.GET("/probe", func(c *gin.Context) {
		seen = logging.RequestIDFrom(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(logging.RequestIDHeader, "upstream-trace-42")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if seen != "upstream-trace-42" {
		t.Errorf("ctx 中的 ID = %q, want 沿用上游值", seen)
	}
	if got := w.Header().Get(logging.RequestIDHeader); got != "upstream-trace-42" {
		t.Errorf("响应头 = %q, want 沿用上游值", got)
	}
}

// TestRequestIDRejectsUnsafeValues 覆盖日志注入。
//
// 该值来自客户端且会被写进日志：若不校验就透传，攻击者可以塞入换行符伪造
// 日志行，或用超长值把日志撑爆。不合法一律重新生成——不报错，因为这属于
// 客户端噪音，不值得中断一个本来正常的请求。
func TestRequestIDRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"含空格", "evil id with spaces"},
		{"含换行（日志注入）", "evil\nINFO forged log line"},
		{"含制表符", "evil\tid"},
		{"超长", strings.Repeat("a", 200)},
		{"含引号", `evil"id`},
		{"含花括号（伪造结构化字段）", `evil","status":200,"x":"`},
		{"非 ASCII", "追踪-id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture(t)
			r := gin.New()
			r.Use(logging.RequestID())
			var seen string
			r.GET("/probe", func(c *gin.Context) {
				seen = logging.RequestIDFrom(c.Request.Context())
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set(logging.RequestIDHeader, tc.raw)
			r.ServeHTTP(httptest.NewRecorder(), req)

			if seen == tc.raw {
				t.Fatalf("危险的 X-Request-ID 被原样采用: %q", tc.raw)
			}
			if !isSafeID(seen) {
				t.Errorf("重新生成的值也应合法，实际 %q", seen)
			}
		})
	}
}

func isSafeID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9', ch == '.', ch == '_', ch == '-':
		default:
			return false
		}
	}
	return true
}

// ---------- AccessLog ----------

// TestAccessLogLevelFollowsStatus 锁住分级别记录：
// 5xx 需要人看、4xx 多半是客户端用法问题、其余只是流水。
func TestAccessLogLevelFollowsStatus(t *testing.T) {
	cases := []struct {
		status    int
		wantLevel zapcore.Level
	}{
		{http.StatusOK, zapcore.InfoLevel},
		{http.StatusNotFound, zapcore.WarnLevel},
		{http.StatusBadRequest, zapcore.WarnLevel},
		{http.StatusUnauthorized, zapcore.WarnLevel},
		{http.StatusInternalServerError, zapcore.ErrorLevel},
		{http.StatusBadGateway, zapcore.ErrorLevel},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			logs := capture(t)
			runRequest(logging.AccessLog(), tc.status)

			entries := logs.FilterMessage("request").All()
			if len(entries) != 1 {
				t.Fatalf("期望恰好 1 条访问日志，实际 %d 条", len(entries))
			}
			if entries[0].Level != tc.wantLevel {
				t.Errorf("status=%d 记为 %v, want %v", tc.status, entries[0].Level, tc.wantLevel)
			}
		})
	}
}

// TestAccessLogCarriesUsefulFields 确认定位问题所需的字段真的都在。
func TestAccessLogCarriesUsefulFields(t *testing.T) {
	logs := capture(t)

	r := gin.New()
	r.Use(logging.RequestID(), logging.AccessLog())
	r.GET("/video/getDetail", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/video/getDetail", nil))

	entries := logs.FilterMessage("request").All()
	if len(entries) != 1 {
		t.Fatalf("期望 1 条访问日志，实际 %d", len(entries))
	}
	ctxMap := entries[0].ContextMap()
	for _, key := range []string{
		"request_id", "method", "path", "status", "latency", "size", "client_ip",
	} {
		if _, ok := ctxMap[key]; !ok {
			t.Errorf("访问日志缺少字段 %q，实际字段: %v", key, keysOf(ctxMap))
		}
	}
	if ctxMap["path"] != "/video/getDetail" {
		t.Errorf("path = %v, want /video/getDetail", ctxMap["path"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- Ctx ----------

func TestCtxAddsRequestIDOnlyWhenPresent(t *testing.T) {
	capture(t)

	logging.L().Info("no ctx id")
	if got := logging.Ctx(nil).Check(zapcore.InfoLevel, "x"); got == nil {
		t.Fatal("Ctx(nil) 也必须返回可用 logger")
	}

	logs := capture(t)
	logging.Ctx(nil).Info("without id")
	logging.Ctx(logging.WithRequestID(t.Context(), "abc123")).Info("with id")

	all := logs.All()
	if len(all) != 2 {
		t.Fatalf("期望 2 条日志，实际 %d", len(all))
	}
	if _, ok := all[0].ContextMap()["request_id"]; ok {
		t.Error("没有 request id 时不应凭空加字段")
	}
	if got := all[1].ContextMap()["request_id"]; got != "abc123" {
		t.Errorf("request_id = %v, want abc123", got)
	}
}
