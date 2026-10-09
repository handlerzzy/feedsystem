package feed_test

// 本文件锁定 feed 包 handler 的错误契约，覆盖两类站点：
//
//  1. A 类（repository 的裸错误）：仍然是 500，但响应体不再回显驱动原文。
//  2. 绑定失败（全部绑定站点）：状态码恒为 400，文案是 curated 的人话。
//
// 第 2 类经历过一次反复，值得记下来：这些站点最初是
// c.JSON(400, gin.H{"error": err.Error()})，理由是"gin 对截断请求体返回
// io.ErrUnexpectedEOF，分类器不认识它（默认落 500），硬编码 400 才是安全默认值"。
// 那个理由对**状态码**成立，但不该顺手把**文案**也交出去——实测回显的是
// "json: cannot unmarshal number into Go struct field .tag_name of type string"，
// 内部类型结构就这么透给了客户端。现在两者兼顾：apierror.RespondBinding 固定
// 400（不依赖分类器，新错误类型也不会退化成 500），文案由统一出口给出。
//
// 这里只驱动 handler，不重复 service_test.go 已覆盖的业务不变量。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/feed"

	"github.com/gin-gonic/gin"
	"go.uber.org/mock/gomock"
)

// listByTagRouter 组装最小的 /feed/tag 路由。ListByTag 不读 JWT
// （viewerAccountID 取不到就按 0 处理），因此这里不需要 accountID 中间件。
func listByTagRouter(f *fixture) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := feed.NewFeedHandler(f.svc)
	r.POST("/feed/tag", h.ListByTag)
	return r
}

func postTag(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/feed/tag", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// bindErrBody 用与 handler 完全相同的绑定路径复算一次错误消息：
// 它就是改造前 `gin.H{"error": err.Error()}` 会写出的那句话。
func decodeError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var parsed map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, w.Body.String())
	}
	return parsed["error"]
}

// A 类站点：仓储错误仍是 500，但原文只进日志。
func TestListByTagRepositoryErrorIs500WithoutLeaking(t *testing.T) {
	f := newFixtureWithoutCache(t)
	const raw = "Error 1045 (28000): Access denied for user 'feed'@'10.0.0.7' (using password: YES)"
	f.store.EXPECT().ListByTag(gomock.Any(), "go", 10).
		Return(nil, errors.New(raw)).
		Times(1)
	// 未设置 likes 期望 => repository 报错时不得继续去查点赞。

	w := postTag(t, listByTagRouter(f), `{"tag_name":"go"}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), raw) || strings.Contains(w.Body.String(), "1045") {
		t.Errorf("body = %s, 不得回显原始仓储错误", w.Body.String())
	}
	if got := decodeError(t, w); got != "internal server error" {
		t.Errorf(`error = %q, want "internal server error"`, got)
	}
}

// 绑定失败的契约：状态码恒为 400，文案必须是**curated 的人话**，
// 且不得回显 Go 结构体名/字段名。
//
// 改造前这些站点写的是 c.JSON(400, gin.H{"error": err.Error()})，实测回显：
//
//	json: cannot unmarshal number into Go struct field .tag_name of type string
//
// 内部类型结构就这么透给了客户端。现在走 apierror.RespondBinding：状态码固定
// 400（绑定失败按定义就是客户端错误，不依赖分类器，新错误类型也不会退化成 500），
// 文案则由 apierror 统一给出。
//
// 这里把期望文案**逐字写死**而不是复用 apierror 的映射函数：复用等于拿实现
// 验证实现，文案改了测试会跟着一起改，测不出任何东西。文案是对外契约的一部分，
// 就该在测试里显式固定下来。
func TestListByTagBindFailureStays400WithCuratedWording(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "请求体被截断（gin 返回 io.ErrUnexpectedEOF）",
			body: `{"tag_name":`,
			want: "request body is incomplete",
		},
		{
			name: "JSON 语法错误（*json.SyntaxError）",
			body: `{"tag_name": }`,
			want: "invalid request body: malformed JSON",
		},
		{
			name: "字段类型错误（*json.UnmarshalTypeError）",
			body: `{"tag_name":123}`,
			want: `invalid request body: field "tag_name" has the wrong type`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWithoutCache(t)
			// 未设置任何仓储期望 => 绑定失败必须在触达 service 之前返回。

			w := postTag(t, listByTagRouter(f), tc.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if got := decodeError(t, w); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
			// 关键回归：不得再把内部类型结构透出去。
			for _, leak := range []string{"Go struct field", "UnmarshalTypeError", "SyntaxError", "tag_name\" of type"} {
				if strings.Contains(decodeError(t, w), leak) {
					t.Errorf("文案里泄漏了内部细节 %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}

// 绑定错误一律 400，即使分类器不认识这个错误类型。
//
// 这是"硬编码 400 是刻意的安全默认值"那条理由的可执行版本：gin 将来若引入新的
// 绑定错误类型（例如 *http.MaxBytesError），走分类器会兜成 500 + 通用文案，
// 客户端会去重试而不是修请求。RespondBinding 对它仍然给 400 + 可操作文案，
// 且**不会**回显原文。
func TestRespondBindingIsAlways400AndNeverEchoesRawError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	unknown := errors.New("some future binding error containing /internal/path")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/feed/tag", nil)

	apierror.RespondBinding(c, unknown)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400（分类器不认识也要给客户端错误）", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "/internal/path") || strings.Contains(body, "some future binding error") {
		t.Errorf("不得回显原始错误，实际 body=%s", body)
	}
	if body := w.Body.String(); !strings.Contains(body, "invalid request body") {
		t.Errorf("应给出可操作的兜底文案，实际 body=%s", body)
	}
}

// 硬编码 400 的纯校验站点（tag_name 为空）同样保持原样。
func TestListByTagMissingNameStays400(t *testing.T) {
	f := newFixtureWithoutCache(t)
	// 未设置仓储期望 => 参数校验失败不得触达 service。

	w := postTag(t, listByTagRouter(f), `{"tag_name":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}
