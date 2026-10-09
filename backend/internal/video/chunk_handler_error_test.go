package video

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖 chunk_handler.go 的两类改动：
//
//  1. D 类：原本 `c.JSON(500, gin.H{"error": "<静态文案>"})` 把底层错误整个丢掉，
//     文件里连 log 都没 import——生产上分片上传失败时没有任何诊断线索。
//     现在改为 RespondWith(..., cause)：对外文案逐字不变，cause 进日志。
//  2. 「chunk N missing」：客户端少传分片是客户端错误，状态码从 500 改为 400。
//
// 日志捕获通过临时替换 apierror.Logger 实现（apierror 只读，不修改）。
// 本包测试均不使用 t.Parallel，因此替换全局变量是安全的，用完必须还原。

func captureAPILogger(t *testing.T) (*[]string, func()) {
	t.Helper()
	var lines []string
	orig := apierror.Logger
	apierror.Logger = func(format string, v ...any) {
		lines = append(lines, fmt.Sprintf(format, v...))
	}
	return &lines, func() { apierror.Logger = orig }
}

// TestCompleteChunkUploadMissingChunkFileReturns400 钉住状态码语义修正：
// 会话认为分片齐了，但磁盘上第 1 片不见了——这属于"客户端需要补传"，
// 必须是 400 而不是 500，同时文案保持 "chunk N missing" 不变。
func TestCompleteChunkUploadMissingChunkFileReturns400(t *testing.T) {
	h, cleanup := setupTestEnv(t)
	defer cleanup()

	chunkSize := 128
	totalChunks := 2
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	uploadID := initUpload(t, h, "missing.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)
	for i := 0; i < totalChunks; i++ {
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}

	// 上传记录（Redis）里第 1 片是"已上传"，但磁盘文件被清掉了。
	missingPath := filepath.Join(".run", "uploads", "tmp", uploadID, "1")
	if err := os.Remove(missingPath); err != nil {
		t.Fatalf("remove chunk file %s: %v", missingPath, err)
	}

	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	h.CompleteChunkUpload(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（缺分片是客户端错误，不是服务端故障）; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseJSON(t, rec)
	if resp["error"] != "chunk 1 missing" {
		t.Fatalf("error = %v, want %q（对外文案不得改变）", resp["error"], "chunk 1 missing")
	}
}

// TestUploadChunkTempDirFailureLogsCause 覆盖 D 类站点 "failed to create temp dir"：
// 对外文案逐字不变（可操作提示，不退回通用文案），底层错误只进日志。
func TestUploadChunkTempDirFailureLogsCause(t *testing.T) {
	h, cleanup := setupTestEnv(t)
	defer cleanup()

	lines, restore := captureAPILogger(t)
	defer restore()

	chunkSize := 128
	chunks, chunkHashes, fileHash := makeTestChunks(t, 1, chunkSize)
	uploadID := initUpload(t, h, "log.mp4", int64(chunkSize), int64(chunkSize), 1, fileHash)

	// 把分片临时目录的父路径做成普通文件：MkdirAll 必然失败（ENOTDIR）。
	uploadsDir := filepath.Join(".run", "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", uploadsDir, err)
	}
	if err := os.WriteFile(filepath.Join(uploadsDir, "tmp"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	c, rec := newMultipartContext(t, "/video/chunk/upload", map[string]string{
		"upload_id":   uploadID,
		"chunk_index": "0",
		"chunk_hash":  chunkHashes[0],
	}, chunks[0])
	h.UploadChunk(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseJSON(t, rec)
	if resp["error"] != "failed to create temp dir" {
		t.Fatalf("error = %v, want %q（对外文案不得改变）", resp["error"], "failed to create temp dir")
	}
	// 底层错误（路径 + 系统调用原因）不得出现在响应体里。
	if body := rec.Body.String(); strings.Contains(body, "not a directory") {
		t.Fatalf("响应体泄漏了底层错误: %s", body)
	}

	// 但必须能在日志里找到它——这正是 D 类改动的全部意义。
	// 日志由 apierror.logServerError 产出，同时含对外文案与 cause：
	// "request failed: method=... path=... status=... msg=\"...\" err=<cause>"，
	// 客户端看到哪条文案、服务端发生了什么，靠这一行就能对上。
	joined := strings.Join(*lines, "\n")
	if joined == "" {
		t.Fatal("5xx 没有写任何日志，失败原因无从诊断")
	}
	for _, want := range []string{
		"status=500",
		"path=/video/chunk/upload",
		`msg="failed to create temp dir"`,
		"mkdir", "not a directory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志缺少 %q，实际日志:\n%s", want, joined)
		}
	}
}

// TestCompleteChunkUploadOutputDirFailureLogsCause 覆盖第二个 D 类站点，
// 确认 cause 的传递不是只改了单个站点。
func TestCompleteChunkUploadOutputDirFailureLogsCause(t *testing.T) {
	h, cleanup := setupTestEnv(t)
	defer cleanup()

	lines, restore := captureAPILogger(t)
	defer restore()

	chunkSize := 128
	chunks, chunkHashes, fileHash := makeTestChunks(t, 1, chunkSize)
	uploadID := initUpload(t, h, "outdir.mp4", int64(chunkSize), int64(chunkSize), 1, fileHash)
	uploadChunk(t, h, uploadID, 0, chunkHashes[0], chunks[0])

	// 让输出目录的父路径成为普通文件：合并阶段的 MkdirAll 必然失败。
	uploadsDir := filepath.Join(".run", "uploads")
	if err := os.WriteFile(filepath.Join(uploadsDir, "videos"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	h.CompleteChunkUpload(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseJSON(t, rec)
	if resp["error"] != "failed to create output dir" {
		t.Fatalf("error = %v, want %q（对外文案不得改变）", resp["error"], "failed to create output dir")
	}
	if body := rec.Body.String(); strings.Contains(body, "not a directory") {
		t.Fatalf("响应体泄漏了底层错误: %s", body)
	}

	joined := strings.Join(*lines, "\n")
	for _, want := range []string{
		"status=500",
		"path=/video/chunk/complete",
		`msg="failed to create output dir"`,
		"mkdir", "not a directory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志缺少 %q，实际日志:\n%s", want, joined)
		}
	}
}

// TestChunkCacheUnavailableStaysServiceUnavailable 是"不要动"站点的回归护栏：
// errChunkCacheUnavailable 由 handler 显式映射为 503，本次改动没有把它卷进分类器
// （否则会落成 500）。哨兵自身现在携带状态码，因此分类器也能识别它。
func TestChunkCacheUnavailableStaysServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	if got := apierror.ClassifyHTTPStatus(errChunkCacheUnavailable); got != http.StatusServiceUnavailable {
		t.Fatalf("哨兵自身状态码 = %d, want 503", got)
	}

	handler := NewChunkUploadHandler(nil)

	c, rec := newJSONContext(t, "/video/chunk/init", InitChunkUploadRequest{
		Filename: "x.mp4", FileSize: 1, ChunkSize: 1, TotalChunks: 1, FileHash: "h",
	})
	handler.InitChunkUpload(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503（既有显式映射，不得改变）", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, errChunkCacheUnavailable.Error()) {
		t.Fatalf("body = %s, want 含 %q", body, errChunkCacheUnavailable.Error())
	}
}
