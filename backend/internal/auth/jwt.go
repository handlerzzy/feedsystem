// internal/auth/jwt.go
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	cachedSecret   []byte
	accessTokenTTL = 15 * time.Minute
)

// SetSecret 由启动流程调用；未调用或传入空串时 GenerateToken/ParseToken 返回错误。
// 不再"未设置就随机生成"：那样多副本互不认 token，重启后全部失效。
func SetSecret(secret string) error {
	if secret == "" {
		return errors.New("jwt secret is empty")
	}
	cachedSecret = []byte(secret)
	return nil
}

// SetAccessTokenTTL 由启动流程调用，覆盖 access token 的默认有效期（15 分钟）。
func SetAccessTokenTTL(d time.Duration) error {
	if d <= 0 {
		return errors.New("access token ttl must be positive")
	}
	accessTokenTTL = d
	return nil
}

func jwtSecret() ([]byte, error) {
	if len(cachedSecret) == 0 {
		return nil, errors.New("jwt secret is not configured (call auth.SetSecret at startup)")
	}
	return cachedSecret, nil
}

type Claims struct {
	AccountID uint   `json:"account_id"`
	Username  string `json:"username"`
	jwt.RegisteredClaims
}

func GenerateToken(accountID uint, username string) (string, error) {
	secret, err := jwtSecret()
	if err != nil {
		return "", err
	}
	now := time.Now()

	claims := Claims{
		AccountID: accountID,
		Username:  username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	return token.SignedString(secret)
}

func GenerateRefreshToken(accountID uint) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ParseToken(tokenString string) (*Claims, error) {
	secret, err := jwtSecret()
	if err != nil {
		return nil, err
	}
	token, err := jwt.ParseWithClaims(
		tokenString,
		&Claims{},
		func(token *jwt.Token) (interface{}, error) {
			if token.Method == nil || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errors.New("unexpected signing method")
			}
			return secret, nil
		},
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, jwt.ErrTokenInvalidClaims
	}

	return claims, nil
}
