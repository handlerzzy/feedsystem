package video_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/video"
	"github.com/handlerzzy/feedsystem/internal/video/mock"

	"github.com/gin-gonic/gin"
	"go.uber.org/mock/gomock"
)

// 本文件钉住错误契约本身，而不是某个业务流程：
//
//   - E 类：服务层原先的裸 errors.New(...) 换成 apierror.BadRequest/NotFound 后，
//     状态码必须从"分类器不认识的 500"变成 400/404，且对外文案**逐字不变**
//     （文案是已对外承诺的契约，改了就是信息丢失）。
//   - A 类：handler 交给分类器的站点，5xx 时响应体只给通用文案，
//     原始错误（SQL、路径这类内部细节）只能进日志。
//
// newCommentFixture / newLikeFixture 定义在 comment_service_test.go、like_service_test.go。

// newHandlerContext 构造一个可直接调用 handler 的 gin 上下文。
// 外部测试包拿不到 chunk_handler_test.go（package video）里的私有 helper，
// 因此这里自带一份最小实现。
func newHandlerContext(t *testing.T, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

// captureLogger 临时把 apierror.Logger 换成内存收集器，返回取日志的函数。
// 必须在测试内 defer restore；本包测试均不并行，替换全局是安全的。
func captureLogger(t *testing.T) (*[]string, func()) {
	t.Helper()
	var lines []string
	orig := apierror.Logger
	apierror.Logger = func(format string, v ...any) {
		lines = append(lines, fmt.Sprintf(format, v...))
	}
	return &lines, func() { apierror.Logger = orig }
}

// ---------- E 类：服务层错误的「状态码 + 文案」契约 ----------

func TestServiceLayerErrorContract(t *testing.T) {
	cases := []struct {
		name        string
		wantStatus  int
		wantMessage string
		run         func(t *testing.T) error
	}{
		{
			name:        "CommentService.Publish: comment 为 nil",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "comment is nil",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				// 未设任何期望 => 校验失败不得触碰 store / MQ。
				return f.svc.Publish(context.Background(), nil)
			},
		},
		{
			name:        "CommentService.Publish: video_id 为 0",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "video_id and author_id are required",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				return f.svc.Publish(context.Background(), &video.Comment{
					VideoID: 0, AuthorID: fixtureAccountID, Content: "hi",
				})
			},
		},
		{
			name:        "CommentService.Publish: author_id 为 0",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "video_id and author_id are required",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				return f.svc.Publish(context.Background(), &video.Comment{
					VideoID: fixtureVideoID, AuthorID: 0, Content: "hi",
				})
			},
		},
		{
			name:        "CommentService.Publish: content 只有空白",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "content is required",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				return f.svc.Publish(context.Background(), &video.Comment{
					VideoID: fixtureVideoID, AuthorID: fixtureAccountID, Content: "   ",
				})
			},
		},
		{
			name:        "CommentService.Publish: 视频不存在",
			wantStatus:  http.StatusNotFound,
			wantMessage: "video not found",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, nil)
				return f.svc.Publish(context.Background(), commentRequest())
			},
		},
		{
			name:        "CommentService.Delete: 评论不存在",
			wantStatus:  http.StatusNotFound,
			wantMessage: "comment not found",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				f.store.EXPECT().GetByID(gomock.Any(), fixtureCommentID).Return(nil, nil)
				return f.svc.Delete(context.Background(), fixtureCommentID, fixtureAccountID)
			},
		},
		{
			name:        "CommentService.GetAll: 视频不存在",
			wantStatus:  http.StatusNotFound,
			wantMessage: "video not found",
			run: func(t *testing.T) error {
				f := newCommentFixture(t)
				f.videos.EXPECT().IsExist(gomock.Any(), fixtureVideoID).Return(false, nil)
				_, err := f.svc.GetAll(context.Background(), fixtureVideoID)
				return err
			},
		},
		{
			name:        "LikeService.Like: like 为 nil",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "like is nil",
			run: func(t *testing.T) error {
				f := newLikeFixture(t)
				return f.svc.Like(context.Background(), nil)
			},
		},
		{
			name:        "LikeService.Like: video_id/account_id 为 0",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "video_id and account_id are required",
			run: func(t *testing.T) error {
				f := newLikeFixture(t)
				return f.svc.Like(context.Background(), &video.Like{VideoID: fixtureVideoID, AccountID: 0})
			},
		},
		{
			name:        "LikeService.Unlike: like 为 nil",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "like is nil",
			run: func(t *testing.T) error {
				f := newLikeFixture(t)
				return f.svc.Unlike(context.Background(), nil)
			},
		},
		{
			name:        "LikeService.Unlike: video_id/account_id 为 0",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "video_id and account_id are required",
			run: func(t *testing.T) error {
				f := newLikeFixture(t)
				return f.svc.Unlike(context.Background(), &video.Like{VideoID: 0, AccountID: fixtureAccountID})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if got := apierror.ClassifyHTTPStatus(err); got != tc.wantStatus {
				t.Errorf("ClassifyHTTPStatus = %d, want %d", got, tc.wantStatus)
			}
			// 文案是已对外承诺的契约：换错误类型可以，改字不行。
			if got := err.Error(); got != tc.wantMessage {
				t.Errorf("err.Error() = %q, want %q（对外文案不得改变）", got, tc.wantMessage)
			}
			if got := apierror.PublicMessage(err); got != tc.wantMessage {
				t.Errorf("PublicMessage = %q, want %q", got, tc.wantMessage)
			}
		})
	}
}

// ---------- A 类：handler 的 5xx 不再回显原始错误 ----------

func TestLikeHandlerServerErrorHidesRawErrorAndLogsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	lines, restore := captureLogger(t)
	defer restore()

	ctrl := gomock.NewController(t)
	store := mock.NewMockLikeStore(ctrl)
	videos := mock.NewMockVideoExistenceStore(ctrl)
	videos.EXPECT().IsExist(gomock.Any(), uint(7)).Return(true, nil)
	// 典型的内部细节：真实表名。它绝不能出现在响应体里。
	dbErr := errors.New(`Error 1146: Table 'feedflow.likes' doesn't exist`)
	store.EXPECT().IsLiked(gomock.Any(), uint(7), uint(1)).Return(false, dbErr)

	svc := video.NewLikeService(store, videos, nil, nil, nil)
	h := video.NewLikeHandler(svc)

	c, rec := newHandlerContext(t, http.MethodPost, "/video/like", `{"video_id":7}`)
	c.Set("accountID", uint(1))
	h.Like(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（服务端故障仍应是 500）", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "doesn't exist") {
		t.Fatalf("响应体泄漏了原始错误: %s", body)
	}
	if !strings.Contains(body, apierror.PublicMessage(dbErr)) {
		t.Errorf("响应体 = %s, 期望包含通用文案 %q", body, apierror.PublicMessage(dbErr))
	}
	// 原始错误必须留下线索，否则线上无从诊断。
	if len(*lines) == 0 {
		t.Fatal("5xx 必须写日志，实际没有任何日志")
	}
	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "doesn't exist") {
		t.Errorf("日志未包含底层错误，实际日志:\n%s", joined)
	}
	// 日志同时要含对外文案：客户端报的文案与日志里的原因必须能对上。
	if want := fmt.Sprintf("msg=%q", apierror.PublicMessage(dbErr)); !strings.Contains(joined, want) {
		t.Errorf("日志缺少 %s，实际日志:\n%s", want, joined)
	}
}

// ---------- A 类：4xx 站点的文案必须保持原样 ----------

// TestLikeHandlerBindingErrorKeepsDetail 覆盖 A 类替换后的 4xx 分支：
// 交给分类器后仍是 400，且 binding 的细节文案照旧回显（对调用方可操作）。
func TestLikeHandlerBindingErrorKeepsDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	svc := video.NewLikeService(mock.NewMockLikeStore(ctrl), nil, nil, nil, nil)
	h := video.NewLikeHandler(svc)

	c, rec := newHandlerContext(t, http.MethodPost, "/video/like", `{"video_id":"abc"}`)
	c.Set("accountID", uint(1))
	h.Like(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "video_id") {
		t.Errorf("响应体 = %s, 期望保留 binding 细节（含 video_id）", body)
	}
}

// TestLikeHandlerExplicitBadRequestKeepsMessage 钉住 handler 里写死的 4xx 站点：
// 本次改动没有把它们也改成通用文案。
func TestLikeHandlerExplicitBadRequestKeepsMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	svc := video.NewLikeService(mock.NewMockLikeStore(ctrl), nil, nil, nil, nil)
	h := video.NewLikeHandler(svc)

	c, rec := newHandlerContext(t, http.MethodPost, "/video/like", `{"video_id":0}`)
	h.Like(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "video_id is required") {
		t.Errorf("响应体 = %s, want 含 %q", body, "video_id is required")
	}
}
