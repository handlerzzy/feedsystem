package video

import (
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"github.com/gin-gonic/gin"
)

const sessionTTL = 24 * time.Hour

// errChunkCacheUnavailable 表示分片上传依赖的 Redis 不可用。
//
// 它同时携带状态码与对外文案：此前它是一个裸 errors.New，handler 只能自己
// 用 gin.H{"error": err.Error()} 拼响应体——那正是"绕过统一出口"的写法。
// 现在状态码与文案都由错误自身声明，handler 只需交给 RespondWith 记录即可。
var errChunkCacheUnavailable = apierror.New(http.StatusServiceUnavailable, "chunk upload requires redis")

type ChunkUploadHandler struct {
	cache *rediscache.Client
}

func NewChunkUploadHandler(cache *rediscache.Client) *ChunkUploadHandler {
	return &ChunkUploadHandler{cache: cache}
}

func (h *ChunkUploadHandler) sessionKey(uploadID string) string {
	return h.cache.Key("chunk_upload:%s", uploadID)
}

func (h *ChunkUploadHandler) hashKey(accountID uint, fileHash string) string {
	return h.cache.Key("chunk_upload_hash:%d:%s", accountID, fileHash)
}

// getSession 读取分片上传会话。
//
// 关键在于**把"没有这个会话"和"Redis 挂了"分开**。此前两者被压成同一句话：
//
//	b, err := h.cache.GetBytes(...)
//	if err != nil {
//		return nil, fmt.Errorf("upload session not found")   // 原因被丢弃
//	}
//
// 后果有两层：Redis 一挂，所有分片上传接口都对外声称"会话不存在"，客户端据此
// 去重开上传会话、反复重试，而服务端日志里一个字都没有——故障被伪装成客户端的
// 输入问题，排查时完全没有线索。现在：
//
//   - 真正的缓存未命中 -> 404，对外文案保持 "upload session not found" 不变；
//   - Redis 报错         -> 503（复用 errChunkCacheUnavailable 的文案），
//     并携带底层原因，由 RespondWith 记进日志。
func (h *ChunkUploadHandler) getSession(ctx *gin.Context, uploadID string) (*ChunkUploadSession, error) {
	if h.cache == nil {
		return nil, errChunkCacheUnavailable
	}
	b, err := h.cache.GetBytes(ctx.Request.Context(), h.sessionKey(uploadID))
	if err != nil {
		if rediscache.IsMiss(err) {
			return nil, apierror.NotFound("upload session not found")
		}
		// %w 保留哨兵以便调用方识别，%v 把底层原因拼进文本供日志使用。
		return nil, fmt.Errorf("%w: %v", errChunkCacheUnavailable, err)
	}
	var s ChunkUploadSession
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("invalid session data")
	}
	return &s, nil
}

// respondSessionError 把 getSession 的错误落成响应。
//
// 不直接调 apierror.Respond：它对 >=500 一律回通用文案（"internal server
// error"），会把"Redis 不可用"这条对客户端与排查都有价值的信息抹掉，而这条
// 文案本来就在对外契约里（cache == nil 时用的就是同一个哨兵）。
func respondSessionError(c *gin.Context, err error) {
	if errors.Is(err, errChunkCacheUnavailable) {
		apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), err)
		return
	}
	apierror.Respond(c, err)
}

func (h *ChunkUploadHandler) saveSession(ctx *gin.Context, s *ChunkUploadSession) error {
	if h.cache == nil {
		return errChunkCacheUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return h.cache.SetBytes(ctx.Request.Context(), h.sessionKey(s.UploadID), b, sessionTTL)
}

func (h *ChunkUploadHandler) InitChunkUpload(c *gin.Context) {
	if h.cache == nil {
		apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), errChunkCacheUnavailable)
		return
	}

	var req InitChunkUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	const maxSize = 200 << 20
	if req.FileSize > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file size exceeds 200MB limit"})
		return
	}

	// Check for existing session (resume)
	hashKey := h.hashKey(accountID, req.FileHash)
	existingID, err := h.cache.GetBytes(c.Request.Context(), hashKey)
	if err == nil && len(existingID) > 0 {
		session, sessErr := h.getSession(c, string(existingID))
		if sessErr == nil {
			// Refresh TTL on resume
			_ = h.cache.SetBytes(c.Request.Context(), hashKey, existingID, sessionTTL)
			_ = h.saveSession(c, session)
			c.JSON(http.StatusOK, gin.H{
				"upload_id":       session.UploadID,
				"uploaded_chunks": session.UploadedChunks(),
			})
			return
		}
	}

	id, _ := randHex(16)
	uploadID := id + fmt.Sprintf("%d", time.Now().UnixNano())
	session := &ChunkUploadSession{
		UploadID:     uploadID,
		AccountID:    accountID,
		Filename:     req.Filename,
		FileSize:     req.FileSize,
		ChunkSize:    req.ChunkSize,
		TotalChunks:  req.TotalChunks,
		FileHash:     req.FileHash,
		UploadedBits: make([]bool, req.TotalChunks),
	}

	if err := h.saveSession(c, session); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to create session", err)
		return
	}

	if err := h.cache.SetBytes(c.Request.Context(), hashKey, []byte(uploadID), sessionTTL); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to create session", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":       uploadID,
		"uploaded_chunks": []int{},
	})
}

func (h *ChunkUploadHandler) UploadChunk(c *gin.Context) {
	if h.cache == nil {
		apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), errChunkCacheUnavailable)
		return
	}

	var req UploadChunkRequest
	if err := c.ShouldBind(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}

	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		respondSessionError(c, err)
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	if req.ChunkIndex < 0 || req.ChunkIndex >= session.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk_index"})
		return
	}

	if session.UploadedBits[req.ChunkIndex] {
		c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	chunkFile, err := f.Open()
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to read chunk", err)
		return
	}
	defer chunkFile.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, chunkFile); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to hash chunk", err)
		return
	}
	actualHash := fmt.Sprintf("%x", hash.Sum(nil))

	if actualHash != req.ChunkHash {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk hash mismatch", "expected": req.ChunkHash, "actual": actualHash})
		return
	}

	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to create temp dir", err)
		return
	}

	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", req.ChunkIndex))
	if _, seekErr := chunkFile.Seek(0, io.SeekStart); seekErr != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to read chunk", seekErr)
		return
	}

	dst, err := os.Create(chunkPath)
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to save chunk", err)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, chunkFile); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to save chunk", err)
		return
	}

	session.UploadedBits[req.ChunkIndex] = true
	if err := h.saveSession(c, session); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to update session", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
}

func (h *ChunkUploadHandler) ChunkStatus(c *gin.Context) {
	if h.cache == nil {
		apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), errChunkCacheUnavailable)
		return
	}

	var req ChunkStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}

	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		respondSessionError(c, err)
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":       session.UploadID,
		"uploaded_chunks": session.UploadedChunks(),
		"total_chunks":    session.TotalChunks,
	})
}

func (h *ChunkUploadHandler) CompleteChunkUpload(c *gin.Context) {
	if h.cache == nil {
		apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), errChunkCacheUnavailable)
		return
	}

	var req CompleteChunkUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}

	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		respondSessionError(c, err)
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	if !session.IsComplete() {
		missing := 0
		for _, uploaded := range session.UploadedBits {
			if !uploaded {
				missing++
				if missing > 5 {
					missing = 5
					break
				}
			}
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":     "not all chunks uploaded",
			"missing":   missing,
			"completed": len(session.UploadedChunks()),
			"total":     session.TotalChunks,
		})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", accountID), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to create output dir", err)
		return
	}

	filename, err := randHex(16)
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to generate filename", err)
		return
	}
	finalPath := filepath.Join(absDir, filename+".mp4")

	finalFile, err := os.Create(finalPath)
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to create final file", err)
		return
	}
	defer finalFile.Close()

	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	for i := 0; i < session.TotalChunks; i++ {
		chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", i))
		cf, err := os.Open(chunkPath)
		if err != nil {
			finalFile.Close()
			os.Remove(finalPath)
			// 「客户端少传了第 i 个分片」是客户端错误，不是服务端故障：
			// 报 500 会让客户端无法区分"补传这个分片"和"服务端坏了"。
			apierror.Respond(c, apierror.BadRequest(fmt.Sprintf("chunk %d missing", i)))
			return
		}
		_, err = io.Copy(finalFile, cf)
		cf.Close()
		if err != nil {
			finalFile.Close()
			os.Remove(finalPath)
			apierror.RespondWith(c, http.StatusInternalServerError, "failed to merge chunks", err)
			return
		}
	}
	finalFile.Close()

	// Clean up temp chunks
	_ = os.RemoveAll(tmpDir)

	// Clean up Redis session
	_ = h.cache.Del(c.Request.Context(), h.sessionKey(req.UploadID))
	_ = h.cache.Del(c.Request.Context(), h.hashKey(accountID, session.FileHash))

	urlPath := fmt.Sprintf("/static/videos/%d/%s/%s.mp4", accountID, date, filename)
	playURL := buildAbsoluteURL(c, urlPath)

	c.JSON(http.StatusOK, gin.H{
		"url":      playURL,
		"play_url": playURL,
	})
}
