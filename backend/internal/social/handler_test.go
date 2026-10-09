package social_test

// 本文件锁定 E 类改造的**端到端**契约：服务层的带码错误经 handler 的
// apierror.Respond 之后，HTTP 状态码与响应体文案必须同时正确。
//
// 为什么在 service_test.go 之外单独放一层：service 层断言只能证明"错误带了码"，
// 证明不了 handler 真的用了分类器。改造前这三个站点都是
// `c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})`，
// 状态码由分类器决定、文案硬取原文，因此 500 + 原文回显；现在必须是
// 400/409 + 同一句话。只有走完 HTTP 这一层才能锁住这个结论。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/social"

	"github.com/gin-gonic/gin"
	"go.uber.org/mock/gomock"
)

// socialPost 组装一个最小的 /social 路由并发起一次请求，返回状态码与
// 完整的响应体（已 TrimSpace）。
//
// accountID 通过中间件写入 gin.Context，等价于 jwt.JWTAuth 校验通过后的效果：
// 这里要覆盖的是服务层错误契约，不是 JWT 实现。
func socialPost(t *testing.T, f *fixture, path, body string, accountID uint) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := social.NewSocialHandler(f.svc)
	g := r.Group("/social")
	g.Use(func(c *gin.Context) {
		c.Set("accountID", accountID)
		c.Next()
	})
	g.POST("/follow", h.Follow)
	g.POST("/unfollow", h.Unfollow)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w.Code, strings.TrimSpace(w.Body.String())
}

func assertErrorResponse(t *testing.T, code int, body string, wantCode int, wantError string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("status = %d, want %d (body=%s)", code, wantCode, body)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, body)
	}
	if parsed["error"] != wantError {
		t.Errorf(`error = %q, want the pre-refactor wording %q`, parsed["error"], wantError)
	}
	if want := `{"error":"` + wantError + `"}`; body != want {
		t.Errorf("body = %s, want %s (status code and wording must both be stable)", body, want)
	}
}

// 改造前：500 {"error":"can not follow self"}；改造后：400，文案不变。
func TestFollowHandlerSelfFollowIs400(t *testing.T) {
	f := newFixture(t)
	// 自关注时服务层会连着查两次同一个账号。
	f.accounts.EXPECT().FindByID(gomock.Any(), uint(7)).
		Return(&account.Account{ID: 7}, nil).Times(2)
	// 未设置 IsFollowed / Follow 期望 => 自关注必须在写库之前被拒绝。

	code, body := socialPost(t, f, "/social/follow", `{"vlogger_id":7}`, 7)
	assertErrorResponse(t, code, body, http.StatusBadRequest, "can not follow self")
}

// 改造前：500 {"error":"already followed"}；改造后：409，文案不变。
func TestFollowHandlerDuplicateFollowIs409(t *testing.T) {
	f := newFixture(t)
	rel := &social.Social{FollowerID: 1, VloggerID: 2}
	existedAccount(t, f, 1, 2)
	f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(true, nil)
	// 未设置 Follow / MQ 期望 => 重复关注不得再次落库、不得重复通知。

	code, body := socialPost(t, f, "/social/follow", `{"vlogger_id":2}`, 1)
	assertErrorResponse(t, code, body, http.StatusConflict, "already followed")
}

// 改造前：500 {"error":"not followed"}；改造后：409，文案不变。
func TestUnfollowHandlerNotFollowedIs409(t *testing.T) {
	f := newFixture(t)
	rel := &social.Social{FollowerID: 1, VloggerID: 2}
	existedAccount(t, f, 1, 2)
	f.store.EXPECT().IsFollowed(gomock.Any(), rel).Return(false, nil)
	// 未设置 Unfollow / MQ 期望 => 不存在的关系不产生任何副作用。

	code, body := socialPost(t, f, "/social/unfollow", `{"vlogger_id":2}`, 1)
	assertErrorResponse(t, code, body, http.StatusConflict, "not followed")
}
