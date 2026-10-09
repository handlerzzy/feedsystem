package apierror_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// captureLog 替换 apierror.Logger，返回一个收集到的日志行切片指针。
func captureLog(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	old := apierror.Logger
	apierror.Logger = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { apierror.Logger = old })
	return &lines
}

// respond 在真实的 gin.Context 上调用 apierror.Respond 并回传状态码与响应体。
func respond(t *testing.T, err error) (int, map[string]string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/account/register", nil)

	apierror.Respond(c, err)

	var body map[string]string
	if w.Body.Len() > 0 {
		if uerr := json.Unmarshal(w.Body.Bytes(), &body); uerr != nil {
			t.Fatalf("response body is not a JSON object: %v (raw=%q)", uerr, w.Body.String())
		}
	}
	return w.Code, body
}

// secretErr 模拟一个携带内部细节的服务端错误。
func secretErr() error {
	return errors.New(
		`Error 1045: Access denied for user 'root'@'10.0.0.7' ` +
			`(using password: YES) at /app/backend/internal/db/db.go:41`)
}

func TestRespondDoesNotLeakInternalErrors(t *testing.T) {
	raw := secretErr()

	logs := captureLog(t)
	code, body := respond(t, raw)

	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
	msg := body["error"]
	if msg != "internal server error" {
		t.Errorf("message = %q, want %q", msg, "internal server error")
	}

	// 这是本测试的核心：响应体里不得出现原始错误的任何片段。
	for _, needle := range []string{
		"Access denied", "root", "10.0.0.7", "password", "db.go",
	} {
		if strings.Contains(msg, needle) {
			t.Errorf("response leaks internal detail %q: %q", needle, msg)
		}
	}

	// 但原始错误必须留在服务端日志里，否则无法排查。
	if len(*logs) == 0 {
		t.Fatal("internal error was not logged")
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "Access denied") {
		t.Errorf("log does not contain the original error: %q", joined)
	}
	if !strings.Contains(joined, "/account/register") {
		t.Errorf("log does not contain the request path: %q", joined)
	}
	// 日志必须同时含"客户端看到了什么"与"底层是什么"：
	// 排查时最常见的问题是两者对不上却无从关联。
	if !strings.Contains(joined, msg) {
		t.Errorf("log does not contain the public message %q that the client saw: %q", msg, joined)
	}
}

func TestRespondPreservesClientErrorContract(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "越权返回 forbidden",
			err:        fmt.Errorf("ownership check failed: %w", apierror.ErrForbidden),
			wantStatus: http.StatusForbidden,
			wantMsg:    "ownership check failed: forbidden",
		},
		{
			name:       "资源不存在返回 404",
			err:        apierror.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantMsg:    "not found",
		},
		{
			name:       "gorm 记录不存在映射为 404",
			err:        fmt.Errorf("query account: %w", gorm.ErrRecordNotFound),
			wantStatus: http.StatusNotFound,
			wantMsg:    "query account: record not found",
		},
		{
			name:       "未认证返回 401",
			err:        apierror.ErrUnauthorized,
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "unauthorized",
		},
		{
			name:       "冲突返回 409",
			err:        apierror.ErrConflict,
			wantStatus: http.StatusConflict,
			wantMsg:    "conflict",
		},
		{
			name:       "校验失败返回 400",
			err:        apierror.ErrValidation,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "validation error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLog(t)
			code, body := respond(t, tc.err)
			if code != tc.wantStatus {
				t.Errorf("status = %d, want %d", code, tc.wantStatus)
			}
			if body["error"] != tc.wantMsg {
				t.Errorf("message = %q, want %q", body["error"], tc.wantMsg)
			}
		})
	}
}

func TestRespondTreatsUnknownBusinessErrorAsServerError(t *testing.T) {
	// 这正是 comment_service.go 里 errors.New("comment not found") 的处境：
	// 没有哨兵 => 分类为 500。本次改造只保证它不再泄漏原文，
	// 把它变成 404 需要在 service 层换用 apierror.ErrNotFound（另行处理）。
	logs := captureLog(t)
	code, body := respond(t, errors.New("comment not found"))

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if body["error"] != "internal server error" {
		t.Errorf("message = %q, want the generic message", body["error"])
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "comment not found") {
		t.Error("the original business error should still be logged for diagnosis")
	}
}

func TestRespondWithNilErrorIsServerError(t *testing.T) {
	logs := captureLog(t)
	code, body := respond(t, nil)

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (nil is a caller bug, not a success)", code)
	}
	if body["error"] != "internal server error" {
		t.Errorf("message = %q, want the generic message", body["error"])
	}
	if len(*logs) == 0 {
		t.Error("calling Respond with nil should leave a diagnostic log line")
	}
}

func TestPublicMessage(t *testing.T) {
	if got := apierror.PublicMessage(nil); got != "" {
		t.Errorf("PublicMessage(nil) = %q, want empty", got)
	}
	if got := apierror.PublicMessage(apierror.ErrForbidden); got != "forbidden" {
		t.Errorf("PublicMessage(ErrForbidden) = %q, want %q", got, "forbidden")
	}

	leaky := secretErr()
	if got := apierror.PublicMessage(leaky); got != "internal server error" {
		t.Errorf("PublicMessage(internal) = %q, want the generic message", got)
	}
}

// respondWith 在真实的 gin.Context 上调用 apierror.RespondWith。
func respondWith(t *testing.T, status int, msg string, cause error) (int, map[string]string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/account/register", nil)

	apierror.RespondWith(c, status, msg, cause)

	var body map[string]string
	if w.Body.Len() > 0 {
		if uerr := json.Unmarshal(w.Body.Bytes(), &body); uerr != nil {
			t.Fatalf("response body is not a JSON object: %v (raw=%q)", uerr, w.Body.String())
		}
	}
	return w.Code, body
}

func TestRespondWithKeepsHardcodedStatus(t *testing.T) {
	// 这是 Respond 覆盖不了的场景：account.ErrUsernameTaken 不是 apierror 哨兵，
	// 分类器会把它判成 500，但 handler 明确知道应返回 409。
	// 第三批验收时实测过 {"error":"username already exists"} + 409，
	// 本次改造不得改变它。
	logs := captureLog(t)
	code, body := respondWith(t, http.StatusConflict, "username already exists",
		errors.New("username already exists"))

	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (hardcoded status must win over the classifier)", code)
	}
	if body["error"] != "username already exists" {
		t.Errorf("message = %q, want %q", body["error"], "username already exists")
	}
	if len(*logs) != 0 {
		t.Errorf("4xx must not be logged as a server error, got %v", *logs)
	}
}

func TestRespondWithLogsCauseButKeepsPublicMessage(t *testing.T) {
	// chunk_handler 的情形：对外文案是固定的可操作提示，
	// 但底层错误此前被整个丢弃，线上没有任何诊断线索。
	raw := secretErr()
	logs := captureLog(t)

	code, body := respondWith(t, http.StatusInternalServerError, "failed to save chunk", raw)

	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
	if body["error"] != "failed to save chunk" {
		t.Errorf("message = %q, want the informative static message", body["error"])
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "Access denied") {
		t.Errorf("cause was discarded instead of logged: %q", joined)
	}
	// 显式文案也必须进日志，否则"客户端看到 failed to save chunk"
	// 与"日志里只有 mkdir 错误"无法关联。
	if !strings.Contains(joined, "failed to save chunk") {
		t.Errorf("log does not contain the public message the client saw: %q", joined)
	}
}

func TestRespondWithLogsEvenWithoutCause(t *testing.T) {
	// 有 5xx 站点确实没有可附的原因；即便如此也要留下"哪个入口失败了"。
	logs := captureLog(t)
	code, _ := respondWith(t, http.StatusInternalServerError, "failed to create output dir", nil)

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "/account/register") || !strings.Contains(joined, "failed to create output dir") {
		t.Errorf("a 5xx without cause should still log entry point and message, got %q", joined)
	}
}
