package video

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

type VideoHandler struct {
	service        *VideoService
	accountService *account.AccountService
}

func NewVideoHandler(service *VideoService, accountService *account.AccountService) *VideoHandler {
	return &VideoHandler{service: service, accountService: accountService}
}

func (vh *VideoHandler) PublishVideo(c *gin.Context) {
	var req PublishVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}

	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	username, err := jwt.GetUsername(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	video := &Video{
		AuthorID:    authorId,
		Username:    username,
		Title:       req.Title,
		Description: req.Description,
		PlayURL:     req.PlayURL,
		CoverURL:    req.CoverURL,
		CreateTime:  time.Now(),
	}
	if err := vh.service.Publish(c.Request.Context(), video); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, video)
}

func (vh *VideoHandler) UploadVideo(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	const maxSize = 200 << 20
	if f.Size <= 0 || f.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	if ext != ".mp4" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .mp4 is allowed"})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", authorId), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		apierror.Respond(c, err)
		return
	}

	filename, err := randHex(16)
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to generate filename", err)
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		apierror.Respond(c, err)
		return
	}

	urlPath := path.Join("/static", "videos", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":      buildAbsoluteURL(c, urlPath),
		"play_url": buildAbsoluteURL(c, urlPath),
	})
}

func (vh *VideoHandler) UploadCover(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	const maxSize = 10 << 20
	if f.Size <= 0 || f.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp is allowed"})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("covers", fmt.Sprintf("%d", authorId), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		apierror.Respond(c, err)
		return
	}

	filename, err := randHex(16)
	if err != nil {
		apierror.RespondWith(c, http.StatusInternalServerError, "failed to generate filename", err)
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		apierror.Respond(c, err)
		return
	}

	urlPath := path.Join("/static", "covers", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":       buildAbsoluteURL(c, urlPath),
		"cover_url": buildAbsoluteURL(c, urlPath),
	})
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func buildAbsoluteURL(c *gin.Context, p string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, p)
}

func (vh *VideoHandler) ListByAuthorID(c *gin.Context) {
	var req ListByAuthorIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	videos, err := vh.service.ListByAuthorID(c.Request.Context(), req.AuthorID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if videos == nil {
		videos = []Video{}
	}
	c.JSON(200, videos)
}

func (vh *VideoHandler) GetDetail(c *gin.Context) {
	var req GetDetailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	video, err := vh.service.GetDetail(c.Request.Context(), req.ID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, video)
}
