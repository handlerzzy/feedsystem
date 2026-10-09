package worker_test

// C 类站点契约：MarkRead / UnreadCount 的 DB 错误。
//
// 改造前这两处是：
//
//	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
//
// 状态码本来就对（服务端失败），但驱动错误原文——表名、连接串、SQL 片段——
// 会直接回显给客户端。改造后走 apierror.Respond：状态码仍是 500，
// 响应体固定为 {"error":"internal server error"}，原文只进日志。
//
// 关于 *gorm.DB 的构造（不接真库，也没有伪造错误）：
// mysql dialector 支持 SkipInitializeWithVersion（跳过 SELECT VERSION() 探测），
// gorm.Config 支持 DisableAutomaticPing；两者一起设置后 gorm.Open 只做
// database/sql 的惰性 sql.Open，不建立任何连接。随后关闭底层 *sql.DB，
// 之后每次查询都会由 database/sql 返回真实的 "sql: database is closed" 错误。
// 这个错误是驱动真实产生的，不是测试拼出来的假 error，也不需要 MySQL 进程或网络；
// 因此可以直接驱动 handler 的 DB 错误分支。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/worker"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// closedPoolDB 返回一个"打开成功但所有查询都失败"的 *gorm.DB。
func closedPoolDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "sse_test:sse_test@tcp(127.0.0.1:1)/no_such_db",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("gorm.Open 不应建立连接，却失败了: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("take underlying *sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close underlying *sql.DB: %v", err)
	}
	return db
}

// sseCtx 构造一个注入了 accountID 的 gin 测试上下文（等价于
// SSERequireAuth 通过后的效果），这样测试只覆盖通知 handler 的错误契约。
func sseCtx(method, target, body string, accountID uint) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("accountID", accountID)
	return c, w
}

func TestMarkReadHandlerDBErrorIs500WithoutLeaking(t *testing.T) {
	db := closedPoolDB(t)

	// 先证明这条路径上确实存在 DB 错误：取出与 handler 同源的真实驱动错误，
	// 后面用它断言"原文没有出现在响应体里"。没有这一步，本测试可能因为
	// 查询意外成功而变成空转。
	probe := db.Model(&worker.Notification{}).
		Where("id = ? AND recipient_id = ?", uint(1), uint(7)).
		Update("is_read", true).Error
	if probe == nil {
		t.Fatal("expected the closed pool to produce a database error")
	}

	hub := worker.NewSSEHub(db)
	c, w := sseCtx(http.MethodPost, "/notification/markRead", `{"id":1}`, 7)
	hub.MarkReadHandler(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), probe.Error()) {
		t.Errorf("body = %s, 不得回显原始 DB 错误原文 %q", w.Body.String(), probe.Error())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"internal server error"}` {
		t.Errorf(`body = %s, want {"error":"internal server error"}`, got)
	}
}

// A 类站点：ListHandler 原先也是 ClassifyHTTPStatus + err.Error()，
// 状态码不变（500），但通知列表查询的驱动原文不再回显。
func TestListHandlerDBErrorIs500WithoutLeaking(t *testing.T) {
	db := closedPoolDB(t)

	var probeRows []worker.Notification
	probe := db.Model(&worker.Notification{}).
		Where("recipient_id = ?", uint(7)).
		Order("created_at desc").
		Limit(50).
		Find(&probeRows).Error
	if probe == nil {
		t.Fatal("expected the closed pool to produce a database error")
	}

	hub := worker.NewSSEHub(db)
	c, w := sseCtx(http.MethodPost, "/notification/list", `{}`, 7)
	hub.ListHandler(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), probe.Error()) {
		t.Errorf("body = %s, 不得回显原始 DB 错误原文 %q", w.Body.String(), probe.Error())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"internal server error"}` {
		t.Errorf(`body = %s, want {"error":"internal server error"}`, got)
	}
}

// MarkRead 的**绑定错误**站点（硬编码 http.StatusBadRequest）保持原样，理由与
// 与 feed.ListByTag 同类：gin 对截断的请求体返回 io.ErrUnexpectedEOF。
// 状态码保持 400 —— 绑定失败按定义就是客户端错误，这里走 apierror.RespondBinding
// 固定 400，不依赖分类器（否则将来出现分类器不认识的绑定错误类型时会变 500）。
// 文案则是人话："request body is incomplete"，而不是裸的 "unexpected EOF"。
func TestMarkReadHandlerTruncatedBodyStays400(t *testing.T) {
	// 绑定失败必须在触达 DB 之前返回，因此这里不需要（也不应该需要）可用的 *gorm.DB。
	hub := worker.NewSSEHub(nil)
	c, w := sseCtx(http.MethodPost, "/notification/markRead", `{"id":`, 7)
	hub.MarkReadHandler(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"request body is incomplete"}` {
		t.Errorf(`body = %s, want {"error":"request body is incomplete"}`, got)
	}
}

func TestUnreadCountHandlerDBErrorIs500WithoutLeaking(t *testing.T) {
	db := closedPoolDB(t)

	probe := db.Model(&worker.Notification{}).
		Where("recipient_id = ? AND is_read = ?", uint(7), false).
		Count(new(int64)).Error
	if probe == nil {
		t.Fatal("expected the closed pool to produce a database error")
	}

	hub := worker.NewSSEHub(db)
	c, w := sseCtx(http.MethodPost, "/notification/unreadCount", `{}`, 7)
	hub.UnreadCountHandler(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), probe.Error()) {
		t.Errorf("body = %s, 不得回显原始 DB 错误原文 %q", w.Body.String(), probe.Error())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"internal server error"}` {
		t.Errorf(`body = %s, want {"error":"internal server error"}`, got)
	}
}
