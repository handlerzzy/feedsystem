package message_test

// 关于“service 层单测”的说明（与任务描述不符之处，详见交付报告）：
//
// 当前 internal/message 的 Service 是一个纯数据持有者，没有任何方法：
//
//	type Service struct {
//	    repo        MessageStore
//	    accountRepo AccountLookup
//	}
//
// 全部业务规则（接收者存在性校验、空内容拒绝、发送者取自 token 主体、
// 会话列表固定条数）都写在 Handler 的方法里，Service 只是把两个依赖
// 传给 Handler 的载体。因此“用 mock 替换仓储后验证 service 层规则”
// 在当前代码结构下只能通过 NewService 注入 mock、再驱动 Handler 来完成——
// 注入点正是 Service 的字段，这也是本次重构要解耦的东西。
//
// 本文件被证明的不变量：
//
//	1. 空/纯空白内容、to_id=0 在触达仓储之前就被拒绝（不产生孤儿私信）。
//	2. 接收者不存在时拒绝发送，绝不落库。
//	3. 发送者身份永远来自 token 主体（gin.Context 的 accountID），
//	   请求体里的 from_id 一律无效。
//	4. 会话列表固定 limit=50、缺少 peer_id 时拒绝、无消息时返回 []（不是 null）、
//	   仓储错误返回 5xx 而不是伪装成空会话。
//
// 依赖全部被替换：MockMessageStore / MockAccountLookup（mockgen 产物）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/message"
	"github.com/handlerzzy/feedsystem/internal/message/mock"

	"github.com/gin-gonic/gin"
	"go.uber.org/mock/gomock"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// ---------- 脚手架 ----------

// newTestRouter 组装一个最小的 /message 路由：用中间件直接写入 accountID，
// 等价于生产环境 jwt.JWTAuth 校验通过后的效果（c.Set("accountID", ...)），
// 使测试只覆盖 message 包的业务规则而不依赖 JWT 实现。
func newTestRouter(t *testing.T, store message.MessageStore, lookup message.AccountLookup, accountID uint) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := message.NewHandler(message.NewService(store, lookup))
	g := r.Group("/message")
	g.Use(func(c *gin.Context) {
		c.Set("accountID", accountID)
		c.Next()
	})
	g.POST("/send", h.Send)
	g.POST("/list", h.List)
	return r
}

// newAnonymousRouter 返回一个没有 accountID 的路由，用于验证未登录请求。
func newAnonymousRouter(t *testing.T, store message.MessageStore, lookup message.AccountLookup) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := message.NewHandler(message.NewService(store, lookup))
	g := r.Group("/message")
	g.POST("/send", h.Send)
	g.POST("/list", h.List)
	return r
}

func doPost(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- Send ----------

func TestSendMessage(t *testing.T) {
	const senderID = uint(1)

	t.Run("空白内容在触达仓储之前就被拒绝", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		// 未设置任何期望 => 一旦查账号或落库测试即失败。

		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"to_id":2,"content":"   "}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("to_id 为 0 时被拒绝", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)

		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"to_id":0,"content":"hi"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("接收者不存在时拒绝发送且绝不落库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		lookup.EXPECT().FindByID(gomock.Any(), uint(2)).
			Return(nil, errors.New("record not found")).
			Times(1)
		// 未设置 store.Send 期望 => 一旦落库测试即失败。

		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"to_id":2,"content":"hi"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "recipient not found") {
			t.Errorf("body = %s, want it to mention the missing recipient", w.Body.String())
		}
	})

	t.Run("接收者存在时落库，且发送者取自 token 主体而不是请求体", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		lookup.EXPECT().FindByID(gomock.Any(), uint(2)).
			Return(&account.Account{ID: 2, Username: "bob"}, nil).
			Times(1)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, m *message.Message) error {
				if m.FromID != senderID {
					t.Errorf("FromID = %d, want the token subject %d (request body must not decide it)",
						m.FromID, senderID)
				}
				if m.ToID != 2 {
					t.Errorf("ToID = %d, want 2", m.ToID)
				}
				if m.Content != "hi" {
					t.Errorf("Content = %q, want %q", m.Content, "hi")
				}
				return nil
			}).
			Times(1)

		// 请求体里塞一个伪造的 from_id，它必须被忽略（SendRequest 没有该字段）。
		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"from_id":999,"to_id":2,"content":"hi"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
		var got message.Message
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("response is not a message: %v (%s)", err, w.Body.String())
		}
		if got.FromID != senderID || got.ToID != 2 {
			t.Errorf("echoed message = (from=%d,to=%d), want (%d,2)", got.FromID, got.ToID, senderID)
		}
	})

	t.Run("内容原样交给仓储（首尾空白由 Repository.Send 负责裁剪）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		lookup.EXPECT().FindByID(gomock.Any(), uint(2)).
			Return(&account.Account{ID: 2}, nil).
			Times(1)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, m *message.Message) error {
				if m.Content != "  hi  " {
					t.Errorf("Content = %q, want the raw %q (trimming belongs to the repository)",
						m.Content, "  hi  ")
				}
				return nil
			}).
			Times(1)

		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"to_id":2,"content":"  hi  "}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("未登录时返回 401，不会退化成 0 号账号发送", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

		w := doPost(t, newAnonymousRouter(t, store, nil), "/message/send",
			`{"to_id":2,"content":"hi"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("未注入账号仓库时跳过接收者校验", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, m *message.Message) error {
				if m.ToID != 2 {
					t.Errorf("ToID = %d, want 2", m.ToID)
				}
				return nil
			}).
			Times(1)

		w := doPost(t, newTestRouter(t, store, nil, senderID), "/message/send",
			`{"to_id":2,"content":"hi"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 when no account repo is configured (body=%s)",
				w.Code, w.Body.String())
		}
	})

	// 以下两个用例是“行为记录”（characterization），不是对现状的认可：
	// 实现里既没有自环校验，也没有内容长度上限。它们锁住当前契约，
	// 一旦将来补上校验，这两个用例会失败并提醒修改方同步更新文档/前端。
	t.Run("行为记录：自己给自己发私信当前会被正常持久化（无自环校验）", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		lookup.EXPECT().FindByID(gomock.Any(), senderID).
			Return(&account.Account{ID: senderID, Username: "alice"}, nil).
			Times(1)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, m *message.Message) error {
				if m.FromID != senderID || m.ToID != senderID {
					t.Errorf("got (from=%d,to=%d), want a self-addressed message (%d,%d)",
						m.FromID, m.ToID, senderID, senderID)
				}
				return nil
			}).
			Times(1)

		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send",
			`{"to_id":1,"content":"note to self"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want the current behaviour 200 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("行为记录：超长内容当前没有长度上限，会原样落库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		lookup := mock.NewMockAccountLookup(ctrl)
		long := strings.Repeat("x", 4096)
		lookup.EXPECT().FindByID(gomock.Any(), uint(2)).
			Return(&account.Account{ID: 2}, nil).
			Times(1)
		store.EXPECT().Send(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, m *message.Message) error {
				if len(m.Content) != len(long) {
					t.Errorf("stored content length = %d, want the full %d bytes", len(m.Content), len(long))
				}
				return nil
			}).
			Times(1)

		body, err := json.Marshal(message.SendRequest{ToID: 2, Content: long})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		w := doPost(t, newTestRouter(t, store, lookup, senderID), "/message/send", string(body))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want the current behaviour 200 (body=%s)", w.Code, w.Body.String())
		}
	})
}

// ---------- List ----------

func TestListMessages(t *testing.T) {
	const userID = uint(1)

	t.Run("缺少 peer_id 时拒绝且不查库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		// 未设置 store.List 期望 => 一旦查库测试即失败。

		w := doPost(t, newTestRouter(t, store, nil, userID), "/message/list", `{"peer_id":0}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("查询会话时固定 limit=50 并按仓储返回顺序输出", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		msgs := []message.Message{
			{ID: 2, FromID: 2, ToID: userID, Content: "newer"},
			{ID: 1, FromID: userID, ToID: 2, Content: "older"},
		}
		// 关键断言：分页窗口硬编码为 50，调用方无法放大或缩小。
		store.EXPECT().List(gomock.Any(), userID, uint(2), 50).
			Return(msgs, nil).
			Times(1)

		w := doPost(t, newTestRouter(t, store, nil, userID), "/message/list", `{"peer_id":2}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
		var resp message.ListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response is not a ListResponse: %v (%s)", err, w.Body.String())
		}
		if len(resp.Messages) != 2 {
			t.Fatalf("got %d messages, want 2", len(resp.Messages))
		}
		if resp.Messages[0].ID != 2 || resp.Messages[1].ID != 1 {
			t.Errorf("message order = [%d %d], want the repository order [2 1]",
				resp.Messages[0].ID, resp.Messages[1].ID)
		}
		if resp.Messages[0].Content != "newer" {
			t.Errorf("content = %q, want \"newer\"", resp.Messages[0].Content)
		}
	})

	t.Run("没有消息时返回空数组而不是 null", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		store.EXPECT().List(gomock.Any(), userID, uint(2), 50).
			Return(nil, nil).
			Times(1)

		w := doPost(t, newTestRouter(t, store, nil, userID), "/message/list", `{"peer_id":2}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
		if got := strings.TrimSpace(w.Body.String()); got != `{"messages":[]}` {
			t.Errorf("body = %s, want {\"messages\":[]} (clients must not receive null)", got)
		}
	})

	t.Run("仓储错误时返回 5xx 而不是伪装成空会话", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		store.EXPECT().List(gomock.Any(), userID, uint(2), 50).
			Return(nil, errors.New("db is down")).
			Times(1)

		w := doPost(t, newTestRouter(t, store, nil, userID), "/message/list", `{"peer_id":2}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "messages") {
			t.Errorf("body = %s, want an error payload, not a message list", w.Body.String())
		}
	})

	t.Run("未登录时返回 401 且不查库", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := mock.NewMockMessageStore(ctrl)
		store.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

		w := doPost(t, newAnonymousRouter(t, store, nil), "/message/list", `{"peer_id":2}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body=%s)", w.Code, w.Body.String())
		}
	})
}
