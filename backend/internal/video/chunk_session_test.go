package video

import (
	"net/http"
	"strings"
	"testing"

	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖 getSession 的两种失败语义。
//
// 改之前两者是同一句话：
//
//	b, err := h.cache.GetBytes(...)
//	if err != nil {
//		return nil, fmt.Errorf("upload session not found")   // 原因被丢弃
//	}
//
// 于是 Redis 一挂，所有分片上传接口都对客户端声称"会话不存在"，而服务端日志里
// 一个字都没有。客户端拿到 404 会去重开上传会话并反复重试，把一次基础设施故障
// 放大成持续流量；运维则完全没有线索，只能看到一堆"客户端在传不存在的会话"。
//
// 这是"把故障伪装成客户端输入错误"的典型，也是最难查的一类：接口语义看起来
// 完全正常。现在要求：缓存未命中 -> 404（文案不变），Redis 报错 -> 503 且留下原因。

// newSessionTestHandler 起一个指向 miniredis 的 handler，并返回 mr 以便制造故障。
func newSessionTestHandler(t *testing.T) (*ChunkUploadHandler, *miniredis.Miniredis) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	client := rediscache.NewClient(
		goredis.NewClient(&goredis.Options{Addr: mr.Addr()}),
		"",
	)
	t.Cleanup(func() {
		_ = client.Close()
		mr.Close()
	})
	return NewChunkUploadHandler(client), mr
}

// TestSessionCacheMissReturns404 守住既有语义：真的没有这个会话时仍然是 404，
// 且对外文案逐字不变（客户端依赖它判断"该重开上传"）。
func TestSessionCacheMissReturns404(t *testing.T) {
	h, _ := newSessionTestHandler(t)

	c, rec := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: "no-such-session"})
	h.ChunkStatus(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（缓存未命中就是没有这个会话）", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upload session not found") {
		t.Errorf("body = %s, want 含 %q（对外文案不得改变）", body, "upload session not found")
	}
}

// TestSessionRedisFailureReturns503NotGlobalNotFound 是本次修复的核心断言。
//
// 把 miniredis 关掉模拟 Redis 故障，接口必须报 503（我们的依赖挂了），
// 而不是 404（客户端拿着不存在的会话）。
func TestSessionRedisFailureReturns503NotGlobalNotFound(t *testing.T) {
	h, mr := newSessionTestHandler(t)

	// 先确认正常情况下这个会话是存在的，排除"其实是未命中"的可能。
	if err := h.cache.SetBytes(t.Context(), h.sessionKey("sess-1"),
		[]byte(`{"upload_id":"sess-1"}`), 0); err != nil {
		t.Fatalf("准备会话失败: %v", err)
	}
	c, rec := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: "sess-1"})
	h.ChunkStatus(c)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("准备阶段就报 404，测试前提不成立: %s", rec.Body.String())
	}

	// 制造 Redis 故障。
	mr.Close()

	c2, rec2 := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: "sess-1"})
	h.ChunkStatus(c2)

	if rec2.Code == http.StatusNotFound {
		t.Fatalf("Redis 故障被报成了 404：客户端会据此重开上传会话并反复重试，"+
			"而服务端没有任何线索。body=%s", rec2.Body.String())
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503（依赖不可用）; body=%s", rec2.Code, rec2.Body.String())
	}
	if body := rec2.Body.String(); !strings.Contains(body, errChunkCacheUnavailable.Error()) {
		t.Errorf("body = %s, want 含 %q", body, errChunkCacheUnavailable.Error())
	}
}

// TestSessionRedisFailureLeavesTheCauseInLogs 覆盖"必须留下线索"这一半。
//
// 仅仅把状态码从 404 改成 503 还不够：如果不把底层原因带进日志，运维依然只
// 知道"某个上传接口 503 了"，不知道是 Redis 连接被拒、超时还是认证失败。
func TestSessionRedisFailureLeavesTheCauseInLogs(t *testing.T) {
	h, mr := newSessionTestHandler(t)
	if err := h.cache.SetBytes(t.Context(), h.sessionKey("sess-2"),
		[]byte(`{"upload_id":"sess-2"}`), 0); err != nil {
		t.Fatalf("准备会话失败: %v", err)
	}
	mr.Close()

	logs, restore := captureAPILogger(t)
	defer restore()

	c, rec := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: "sess-2"})
	h.ChunkStatus(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "chunk upload requires redis") {
		t.Errorf("日志里应记录本次 5xx；实际日志:\n%s", joined)
	}
	// 关键：务必带上底层原因。连接被拒时会含 "connection refused" 或 EOF 之类，
	// 这里只断言确实带了 error 字段而非空。
	if !strings.Contains(joined, "err=") {
		t.Errorf("日志里应带上底层原因（err=...），否则排查仍无线索；实际日志:\n%s", joined)
	}
}

// TestSessionReaderReturnsSentinelWrapped 钉住 getSession 的错误形状：
// Redis 故障必须能被 errors.Is 识别为 errChunkCacheUnavailable，否则
// respondSessionError 会退回通用文案，503 的语义又丢了。
func TestSessionReaderReturnsSentinelWrapped(t *testing.T) {
	h, mr := newSessionTestHandler(t)
	mr.Close()

	c, _ := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: "x"})
	_, err := h.getSession(c, "x")
	if err == nil {
		t.Fatal("Redis 故障时 getSession 必须报错")
	}
	if !strings.Contains(err.Error(), errChunkCacheUnavailable.Error()) {
		t.Errorf("err = %v, want 含哨兵文案（供 respondSessionError 识别）", err)
	}
}
