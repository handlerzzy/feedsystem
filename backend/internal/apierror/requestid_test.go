package apierror_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"github.com/gin-gonic/gin"
)

// TestServerErrorLogCarriesRequestID 锁住请求关联能力。
//
// 这条性质是整个 #4 改造的核心价值：客户端报障时提供响应头里的
// X-Request-ID，运维就能直接检索出这整条请求的日志，而不必按时间戳在
// 海量行里人肉对齐。若 request_id 没进日志，这个能力就是空的。
func TestServerErrorLogCarriesRequestID(t *testing.T) {
	const rid = "trace-abc-123"

	logs := captureLog(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/video/publish", nil)
	// 模拟 logging.RequestID 中间件的效果。
	c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), rid))

	apierror.Respond(c, secretErr())

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, rid) {
		t.Errorf("服务端错误日志缺少 request_id=%q，客户端报障将无法定位：%q", rid, joined)
	}
	// 同时确认没有因为加了字段而丢掉原有信息。
	for _, want := range []string{"/video/publish", "Access denied", "internal server error"} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志缺少 %q：%q", want, joined)
		}
	}
}

// TestServerErrorWithoutRequestIDStillLogs 确认 request_id 缺失时不影响记录：
// 直接调用 Respond（例如未经过中间件的单测或内部调用）也必须留下日志。
func TestServerErrorWithoutRequestIDStillLogs(t *testing.T) {
	logs := captureLog(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/internal", nil)

	apierror.Respond(c, secretErr())

	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "Access denied") {
		t.Errorf("没有 request_id 时仍须记录底层错误：%q", joined)
	}
	if !strings.Contains(joined, "request_id=") {
		t.Errorf("字段本身应始终存在（值为空），便于日志解析：%q", joined)
	}
}

// TestRespondWithCarriesRequestID 覆盖 RespondWith 这条路径。
func TestRespondWithCarriesRequestID(t *testing.T) {
	const rid = "trace-xyz-789"
	logs := captureLog(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/video/chunk/upload", nil)
	c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), rid))

	apierror.RespondWith(c, http.StatusInternalServerError, "failed to save chunk", secretErr())

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON 对象: %v", err)
	}
	if body["error"] != "failed to save chunk" {
		t.Errorf("对外文案 = %q, want %q", body["error"], "failed to save chunk")
	}

	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, rid) {
		t.Errorf("RespondWith 的日志缺少 request_id=%q：%q", rid, joined)
	}
}
