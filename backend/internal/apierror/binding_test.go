package apierror_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"github.com/gin-gonic/gin/binding"
)

func newRequestWithBody(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestEmptyOrTruncatedBodyIsClientError 覆盖分类器的一个盲区。
//
// gin 的绑定在客户端发了**空请求体**或**被截断的请求体**时，返回的不是
// json.SyntaxError，而是 io.EOF / io.ErrUnexpectedEOF。这两个都不实现 Is()、
// 也不属于 validator.ValidationErrors，因此原先会一路落进 default 变成 **500**。
//
// 后果是客户端有明确的错误（body 没发全），却被告知"服务端出错"——既误导调用方
// （会去重试而不是修请求），也让服务端日志被无意义的 500 淹没。
//
// 这里刻意用**真实的 gin binding** 产生错误，而不是手写 io.EOF：
// 只有这样才能保证"gin 实际返回什么"与本测试的假设一致。
func TestEmptyOrTruncatedBodyIsClientError(t *testing.T) {
	type payload struct {
		Name string `json:"name" binding:"required"`
	}

	cases := []struct {
		name     string
		body     string
		wantHint string
	}{
		{"完全空的请求体", "", "request body is required"},
		{"被截断的 JSON", `{"name": "abc`, "request body is incomplete"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p payload
			err := binding.JSON.Bind(newRequestWithBody(tc.body), &p)
			if err == nil {
				t.Fatal("期望绑定失败，实际没有错误")
			}
			t.Logf("gin 返回的错误: %T %v", err, err)

			if got := apierror.ClassifyHTTPStatus(err); got != http.StatusBadRequest {
				t.Errorf("ClassifyHTTPStatus = %d, want 400（客户端发错请求，不是服务端故障）", got)
			}
			// 对外文案必须是可操作的。原始的 "EOF" / "unexpected EOF"
			// 对调用方毫无意义——他们无从知道该补什么。
			if got := apierror.PublicMessage(err); got != tc.wantHint {
				t.Errorf("PublicMessage = %q, want %q", got, tc.wantHint)
			}
		})
	}
}

// TestEOFStillRecognizedWhenWrapped 确认 errors.Is 能穿透包装：
// 上层用 fmt.Errorf("%w") 包装后仍应被识别为客户端错误。
func TestEOFStillRecognizedWhenWrapped(t *testing.T) {
	wrapped := fmt.Errorf("bind request body: %w", io.ErrUnexpectedEOF)

	if got := apierror.ClassifyHTTPStatus(wrapped); got != http.StatusBadRequest {
		t.Errorf("包装后的 io.ErrUnexpectedEOF 分类 = %d, want 400", got)
	}
}
