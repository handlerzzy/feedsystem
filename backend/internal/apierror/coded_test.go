package apierror_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
)

// TestCodedErrorCarriesStatusAndMessage 覆盖"状态码正确、文案逐字不变"这一核心诉求。
//
// 背景：服务层里 `errors.New("video not found")` 语义上是 404，但分类器不认识，
// 最终落成 500 并把原文回显。简单换成 apierror.ErrNotFound 会让状态码正确、
// 文案却从 "video not found" 退化成 "not found"——那是信息丢失与契约变更。
// CodedError 让两者同时成立。
func TestCodedErrorCarriesStatusAndMessage(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{"400", apierror.BadRequest("content is required"), http.StatusBadRequest, "content is required"},
		{"401", apierror.Unauthorized("invalid refresh token"), http.StatusUnauthorized, "invalid refresh token"},
		{"403", apierror.Forbidden("not the author"), http.StatusForbidden, "not the author"},
		{"404", apierror.NotFound("video not found"), http.StatusNotFound, "video not found"},
		{"409", apierror.Conflict("already followed"), http.StatusConflict, "already followed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apierror.ClassifyHTTPStatus(tc.err); got != tc.wantStatus {
				t.Errorf("ClassifyHTTPStatus = %d, want %d", got, tc.wantStatus)
			}
			// 文案必须逐字保留：这是"修状态码但不破坏契约"的关键。
			if got := apierror.PublicMessage(tc.err); got != tc.wantMsg {
				t.Errorf("PublicMessage = %q, want %q", got, tc.wantMsg)
			}
			if tc.err.Error() != tc.wantMsg {
				t.Errorf("Error() = %q, want %q", tc.err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestCodedErrorWithServerStatusDoesNotEcho 确认这条新路径没有开一个后门：
// 显式声明为 5xx 的 CodedError 仍然不得回显自己的文案。
func TestCodedErrorWithServerStatusDoesNotEcho(t *testing.T) {
	leaky := apierror.New(http.StatusInternalServerError,
		`Error 1045: Access denied for user 'root'@'10.0.0.7'`)

	if got := apierror.ClassifyHTTPStatus(leaky); got != http.StatusInternalServerError {
		t.Errorf("ClassifyHTTPStatus = %d, want 500", got)
	}
	if got := apierror.PublicMessage(leaky); got != "internal server error" {
		t.Errorf("5xx 的 CodedError 不得回显原文，得到 %q", got)
	}
}

// TestCodedErrorComposesWithSentinels 确认它能和哨兵错误组合：
// 既保留"是哪一类问题"（errors.Is），又携带具体状态与文案。
func TestCodedErrorComposesWithSentinels(t *testing.T) {
	wrapped := apierror.Wrap(apierror.ErrNotFound, http.StatusNotFound, "video not found")

	if !errors.Is(wrapped, apierror.ErrNotFound) {
		t.Error("errors.Is 应能穿透到哨兵错误，供上层按类别判断")
	}
	if got := apierror.ClassifyHTTPStatus(wrapped); got != http.StatusNotFound {
		t.Errorf("ClassifyHTTPStatus = %d, want 404", got)
	}
	if got := apierror.PublicMessage(wrapped); got != "video not found" {
		t.Errorf("PublicMessage = %q, want %q", got, "video not found")
	}

	// fmt.Errorf 的 %w 链上也应可用。
	chained := fmt.Errorf("publish comment: %w", wrapped)
	if !errors.Is(chained, apierror.ErrNotFound) {
		t.Error("errors.Is 应能穿透多层包装")
	}
	if got := apierror.ClassifyHTTPStatus(chained); got != http.StatusNotFound {
		t.Errorf("多层包装后 ClassifyHTTPStatus = %d, want 404", got)
	}
}

// TestCodedStatusIgnoresPlainErrors 确认它不会误判普通错误——
// 否则任何错误都可能被当成已分类，分类器就失去意义了。
func TestCodedStatusIgnoresPlainErrors(t *testing.T) {
	plain := errors.New("some error")

	if code, ok := apierror.CodedStatus(plain); ok {
		t.Errorf("普通错误不应被判为已分类，得到 (%d, true)", code)
	}
	if got := apierror.ClassifyHTTPStatus(plain); got != http.StatusInternalServerError {
		t.Errorf("ClassifyHTTPStatus = %d, want 500", got)
	}
	if got := apierror.PublicMessage(plain); got != "internal server error" {
		t.Errorf("PublicMessage = %q, want 通用文案", got)
	}
}

// TestCodedErrorTakesPrecedenceOverSentinels 确认显式状态码优先于按类别推断。
// 这很重要：服务层若明确知道该错误对应哪个状态，就不该被宽泛的哨兵覆盖。
func TestCodedErrorTakesPrecedenceOverSentinels(t *testing.T) {
	// 同时满足"是哨兵 ErrNotFound"（会推 404）与"显式声明 409"。
	both := apierror.Wrap(apierror.ErrNotFound, http.StatusConflict, "version conflict")

	if got := apierror.ClassifyHTTPStatus(both); got != http.StatusConflict {
		t.Errorf("ClassifyHTTPStatus = %d, want 409（显式状态码应优先）", got)
	}
}
