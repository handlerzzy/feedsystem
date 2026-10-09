package message_test

// 本文件锁定 message 包的 E 类 / A 类错误契约。
//
// E 类：Repository.Send 的空内容拒绝从裸 errors.New 换成 apierror.BadRequest 之后，
//  1. 分类器给出的状态码从 500 变成 400；
//  2. 对外文案与改造前逐字一致（"content is required"）；
//  3. 经 Handler.Send 走到 HTTP 层时响应体仍是同一句话（4xx 原样回显）。
//
// A 类：列表等站点的 c.JSON(ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
// 换成 apierror.Respond 之后，5xx 只回通用文案，驱动/SQL 细节只进日志。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/message"
	"github.com/handlerzzy/feedsystem/internal/message/mock"

	"go.uber.org/mock/gomock"
)

// 为什么可以传 nil 构造 Repository：Send 的空白校验在触达 r.db 之前就返回，
// 因此不需要真库、也不需要任何数据库驱动。这里刻意用 nil —— 若将来有人把
// 落库挪到校验之前，本测试会 panic（而不是静默通过），从而暴露顺序变化。
func blankContentError(t *testing.T) error {
	t.Helper()
	err := message.NewRepository(nil).Send(context.Background(), &message.Message{Content: "   "})
	if err == nil {
		t.Fatal("expected blank content to be rejected before hitting the database")
	}
	return err
}

func TestRepositorySendBlankContentIsBadRequest(t *testing.T) {
	err := blankContentError(t)

	if got, want := err.Error(), "content is required"; got != want {
		t.Errorf("err.Error() = %q, want the pre-refactor wording %q", got, want)
	}
	if got := apierror.ClassifyHTTPStatus(err); got != http.StatusBadRequest {
		t.Errorf("ClassifyHTTPStatus(%v) = %d, want 400 (before the change a bare errors.New landed on 500)", err, got)
	}
	if got := apierror.PublicMessage(err); got != "content is required" {
		t.Errorf("PublicMessage(err) = %q, want %q (client-visible text must not change)", got, "content is required")
	}
}

// 把 Repository 真实产出的错误喂回 Handler.Send：这条 A 类站点
// （原 c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})）
// 现在必须给出 400 + 原文，而不是 500 + 原文。
func TestHandlerSendSurfacesBlankContentAs400(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockMessageStore(ctrl)
	store.EXPECT().Send(gomock.Any(), gomock.Any()).
		Return(blankContentError(t)).
		Times(1)

	w := doPost(t, newTestRouter(t, store, nil, 1), "/message/send",
		`{"to_id":2,"content":"hi"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"content is required"}` {
		t.Errorf("body = %s, want {\"error\":\"content is required\"}", got)
	}
}

// A 类站点的 5xx 行为：状态码不变（500），但驱动/SQL 原文不得再出现在响应体里。
func TestHandlerListHidesRawRepositoryError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockMessageStore(ctrl)
	const raw = "Error 1146 (42S02): Table 'feedflow.messages' doesn't exist"
	store.EXPECT().List(gomock.Any(), uint(1), uint(2), 50).
		Return(nil, errors.New(raw)).
		Times(1)

	w := doPost(t, newTestRouter(t, store, nil, 1), "/message/list", `{"peer_id":2}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), raw) || strings.Contains(w.Body.String(), "1146") {
		t.Errorf("body = %s, 不得回显原始仓储错误", w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"internal server error"}` {
		t.Errorf("body = %s, want {\"error\":\"internal server error\"}", got)
	}
}
