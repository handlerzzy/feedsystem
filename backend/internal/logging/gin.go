package logging

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// maxRequestIDLen 限制外部传入的请求 ID 长度。
const maxRequestIDLen = 64

// RequestID 为每个请求确定一个关联标识，写入：
//   - c.Request.Context()：供 Ctx(ctx) 取出，附在所有日志上；
//   - 响应头 X-Request-ID：供客户端在报障时提供。
//
// 若上游（网关/负载均衡）已设置了合法的 X-Request-ID 则沿用，这样跨服务
// 能保持同一条链路；否则自行生成。
//
// 安全考虑：该值来自客户端，且会被写进日志。若不校验就透传，攻击者可以
// 塞入换行符伪造日志行（日志注入），或用超长值把日志撑爆。因此只接受
// 受限字符集与长度，不合法一律重新生成——不报错，因为这属于客户端噪音，
// 不值得中断一个本来正常的请求。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := sanitizeRequestID(c.GetHeader(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}

		c.Request = c.Request.WithContext(WithRequestID(c.Request.Context(), id))
		c.Header(RequestIDHeader, id)

		c.Next()
	}
}

// AccessLog 记录每个请求的结果，级别按状态码区分：
// 5xx → Error（需要人看），4xx → Warn（多半是客户端用法问题），其余 Info。
//
// 关注 latency 而不是只记时间戳：定位"慢"比定位"错"更需要量化依据。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.Int("status", status),
			zap.Duration("latency", latency),
			zap.Int("size", c.Writer.Size()),
			zap.String("client_ip", c.ClientIP()),
		}
		// gin 在 handler 里 c.Error(...) 收集的错误一并带上，省得两边对不上。
		if errs := c.Errors.ByType(gin.ErrorTypePrivate).String(); errs != "" {
			fields = append(fields, zap.String("handler_errors", errs))
		}

		switch {
		case status >= 500:
			Ctx(c.Request.Context()).Error("request", fields...)
		case status >= 400:
			Ctx(c.Request.Context()).Warn("request", fields...)
		default:
			Ctx(c.Request.Context()).Info("request", fields...)
		}
	}
}

// newRequestID 生成 16 字节随机十六进制串。
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退化成时间戳：宁可关联性弱一点，也不要让请求失败。
		return "ts-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// sanitizeRequestID 只放行 [A-Za-z0-9._-] 且长度受限的值；不合法返回空串。
func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '.', ch == '_', ch == '-':
		default:
			return ""
		}
	}
	return raw
}
