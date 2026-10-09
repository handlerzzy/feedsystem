package account

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type AccountHandler struct {
	accountService *AccountService
}

func NewAccountHandler(accountService *AccountService) *AccountHandler {
	return &AccountHandler{accountService: accountService}
}
func (h *AccountHandler) CreateAccount(c *gin.Context) {
	var req CreateAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username is required"})
		return
	}
	if err := h.accountService.CreateAccount(c.Request.Context(), &Account{
		Username: req.Username,
		Password: req.Password,
	}); err != nil {
		// 用户名重复：ErrUsernameTaken 自带 409 与 "username already exists"
		// 文案，无需在此硬编码状态码。该分支现与下方兜底等价，保留以维持
		// 原有控制流不变（err 的分类结果就是 409）。
		if errors.Is(err, ErrUsernameTaken) {
			apierror.Respond(c, err)
			return
		}
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"message": "account created"})
}

func (h *AccountHandler) Rename(c *gin.Context) {
	var req RenameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	accountID, err := getAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	token, err := h.accountService.Rename(c.Request.Context(), accountID, req.NewUsername)
	if err != nil {
		if errors.Is(err, ErrNewUsernameRequired) {
			apierror.Respond(c, err)
			return
		}
		if errors.Is(err, ErrUsernameTaken) {
			apierror.Respond(c, err)
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(404, gin.H{"error": "account not found"})
			return
		}
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"token": token})
}

func (h *AccountHandler) ChangePassword(c *gin.Context) {
	accountID, err := getAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if err := h.accountService.ChangePasswordByID(c.Request.Context(), accountID, req.OldPassword, req.NewPassword); err != nil {
		c.JSON(400, gin.H{"error": "unsuccessfully password changed"})
		return
	}
	c.JSON(200, gin.H{"message": "successfully password changed"})
}

func (h *AccountHandler) FindByID(c *gin.Context) {
	var req FindByIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if account, err := h.accountService.FindByID(c.Request.Context(), req.ID); err != nil {
		apierror.Respond(c, err)
		return
	} else {
		c.JSON(200, account)
	}
}

func (h *AccountHandler) FindByUsername(c *gin.Context) {
	var req FindByUsernameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if account, err := h.accountService.FindByUsername(c.Request.Context(), req.Username); err != nil {
		apierror.Respond(c, err)
		return
	} else {
		c.JSON(200, account)
	}
}

// invalidCredentialMsg 是登录失败的唯一对外文案：
// "用户名不存在" 与 "密码错误" 必须返回完全一致的响应，否则用户名可被枚举。
const invalidCredentialMsg = "invalid username or password"

func (h *AccountHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	account, err := h.accountService.FindByUsername(c.Request.Context(), req.Username)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": invalidCredentialMsg})
		return
	}
	accessToken, refreshToken, err := h.accountService.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": invalidCredentialMsg})
		return
	}
	c.JSON(200, LoginResponse{Token: accessToken, RefreshToken: refreshToken, AccountID: account.ID, Username: account.Username})
}

func (h *AccountHandler) Logout(c *gin.Context) {
	accountID, err := getAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if err := h.accountService.Logout(c.Request.Context(), accountID); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"message": "account logged out"})
}

func (h *AccountHandler) UploadAvatar(c *gin.Context) {
	accountID, err := getAccountID(c)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp allowed"})
		return
	}
	dir := filepath.Join(".run", "uploads", "avatars", strconv.FormatUint(uint64(accountID), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		apierror.Respond(c, err)
		return
	}
	filename, err := randHex(16)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	filename = filename + ext
	absPath := filepath.Join(dir, filename)
	if err := c.SaveUploadedFile(f, absPath); err != nil {
		apierror.Respond(c, err)
		return
	}
	urlPath := path.Join("/static", "avatars", strconv.FormatUint(uint64(accountID), 10), filename)
	avatarURL := buildAbsoluteURL(c, urlPath)
	if err := h.accountService.UpdateAvatar(c.Request.Context(), accountID, avatarURL); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"avatar_url": avatarURL})
}

func (h *AccountHandler) UpdateProfile(c *gin.Context) {
	accountID, err := getAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if err := h.accountService.UpdateProfile(c.Request.Context(), accountID, &req); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "profile updated"})
}

func (h *AccountHandler) Refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	newToken, accountID, username, err := h.accountService.RefreshAccessToken(c.Request.Context(), req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}
	c.JSON(http.StatusOK, LoginResponse{Token: newToken, AccountID: accountID, Username: username})
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

// getAccountID 与 jwt.GetAccountID 同义。
//
// 这里之所以有一份副本而不是直接调 jwt.GetAccountID：jwt 包已经导入了
// internal/account（用于 AccountLookup），反向导入会成环。两边的语义必须保持
// 一致，改动时请一起改。
func getAccountID(c *gin.Context) (uint, error) {
	value, exists := c.Get("accountID")
	if !exists {
		return 0, errIdentityMissing()
	}
	id, ok := value.(uint)
	if !ok {
		return 0, errIdentityMissing()
	}
	return id, nil
}

// errIdentityMissing 见 jwt.errIdentityMissing 的说明：对外 401，对内留下线索。
func errIdentityMissing() error {
	logging.L().Warn("受保护的路由缺少身份，检查该路由是否挂了认证中间件")
	return apierror.Unauthorized("authentication required")
}
