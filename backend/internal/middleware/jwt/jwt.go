package jwt

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/auth"
	"github.com/handlerzzy/feedsystem/internal/logging"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// JWTAuth check jwt token and ensure it matches the currently stored token.
func JWTAuth(accountRepo AccountLookup, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			return
		}

		tokenString := parts[1]

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		if err := CheckAndBind(c, claims, tokenString, accountRepo, cache); err != nil {
			apierror.Respond(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

func SoftJWTAuth(accountRepo AccountLookup, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			return
		}

		tokenString := parts[1]

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		if err := CheckAndBind(c, claims, tokenString, accountRepo, cache); err != nil {
			apierror.Respond(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

// CheckAndBind 校验 token 是否仍然有效（Redis 比对 + DB 兜底），
// 通过后把 accountID / username 写入 gin.Context。
// 注意：本函数不负责调用 c.Next()，由调用方决定。
func CheckAndBind(c *gin.Context, claims *auth.Claims, tokenString string,
	accountRepo AccountLookup, cache *rediscache.Client) error {
	key := cache.Key("account:%d", claims.AccountID)

	// 先查 Redis
	if cache != nil {
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		defer cancel()

		b, err := cache.GetBytes(cacheCtx, key)
		if err == nil {
			if string(b) != tokenString {
				return apierror.Unauthorized("token has been revoked")
			}
			c.Set("accountID", claims.AccountID)
			c.Set("username", claims.Username)
			return nil
		}
	}

	// Redis 故障/未启用：查 DB 兜底
	accountInfo, err := accountRepo.FindByID(c.Request.Context(), claims.AccountID)
	if err != nil || accountInfo.Token == "" || accountInfo.Token != tokenString {
		return apierror.Unauthorized("token has been revoked")
	}

	if cache != nil {
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		defer cancel()

		if err := cache.SetBytes(cacheCtx, key, []byte(tokenString), 24*time.Hour); err != nil {
			// 回填缓存失败不影响本次鉴权（DB 已校验通过），故为 Warn。
			// key 只含 accountID，可安全入日志；token 本身不记。
			logging.Ctx(c.Request.Context()).Warn("failed to set cache",
				zap.Uint("account_id", claims.AccountID), zap.Error(err))
		}
	}

	c.Set("accountID", claims.AccountID)
	c.Set("username", claims.Username)
	return nil
}

// GetAccountID 取出 JWTAuth/SoftJWTAuth 写入 gin.Context 的账号 ID。
//
// 取不到时返回 401 类型的错误，而不是裸 errors.New：调用点统一交给
// apierror.Respond 之后，裸错误会被分类成 500（分类器不认识它），
// 客户端会以为"服务端挂了"从而重试，而正确语义是"你没有通过认证"。
//
// 走到这里其实意味着**路由没挂认证中间件**，是服务端接线错误；但它对外表现
// 为"调用方没有身份"，所以对外给 401，同时留一条 Warn 让接线错误可见——
// 否则这条路径一旦真的被走到，日志里不会有任何痕迹。
func GetAccountID(c *gin.Context) (uint, error) {
	uidValue, exists := c.Get("accountID")
	if !exists {
		return 0, errIdentityMissing("accountID")
	}

	accountID, ok := uidValue.(uint)
	if !ok {
		return 0, errIdentityMissing("accountID")
	}

	return accountID, nil
}

func GetUsername(c *gin.Context) (string, error) {
	val, exists := c.Get("username")
	if !exists {
		return "", errIdentityMissing("username")
	}

	username, ok := val.(string)
	if !ok {
		return "", errIdentityMissing("username")
	}

	return username, nil
}

// errIdentityMissing 报告"受保护路由上没有身份"。
// 对外是 401（调用方未通过认证），对内留一条 Warn（接线错误）。
func errIdentityMissing(key string) error {
	logging.L().Warn("受保护的路由缺少身份，检查该路由是否挂了认证中间件",
		zap.String("missing_key", key))
	return apierror.Unauthorized("authentication required")
}
