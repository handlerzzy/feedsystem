package http

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// readyzDepTimeout 是单个依赖探测的超时；父 ctx 的总预算为 2 秒，
// 子 ctx 同时受父 deadline 约束，故最坏总耗时不超过 2 秒。
const (
	readyzTotalTimeout = 2 * time.Second
	readyzDepTimeout   = 1 * time.Second
)

type depStatus struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type readyzResponse struct {
	Status   string      `json:"status"` // "ok" | "degraded" | "unavailable"
	Degraded bool        `json:"degraded"`
	Deps     []depStatus `json:"deps"`
}

// livezHandler 只表示"进程存活"，永远不查依赖。用于容器重启判据：
// 依赖故障时不应该重启一个本身健康的进程。
func livezHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyzHandler 聚合探测 DB/Redis/MQ。
//
// 状态码规则：
//   - DB 不可用 -> 503（核心依赖，没有它整个服务没有意义）
//   - Redis / MQ 不可用 -> 200 + degraded:true
//     因为本项目明确把二者当作可选依赖：不可用时降级为 nil 并继续服务。
//     这里若返回 503，编排层会反复重启一个本来能降级工作的实例。
func readyzHandler(deps Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), readyzTotalTimeout)
		defer cancel()

		statuses := []depStatus{
			checkMySQL(ctx, deps),
			checkRedis(ctx, deps),
			checkRabbitMQ(deps),
		}

		resp := readyzResponse{Status: "ok", Deps: statuses}
		code := http.StatusOK
		for _, s := range statuses {
			if s.OK {
				continue
			}
			if s.Name == "mysql" {
				resp.Status = "unavailable"
				code = http.StatusServiceUnavailable
			} else if resp.Status == "ok" {
				resp.Status = "degraded"
			}
		}
		resp.Degraded = resp.Status != "ok"
		c.JSON(code, resp)
	}
}

func checkMySQL(parent context.Context, deps Deps) depStatus {
	st := depStatus{Name: "mysql"}
	if deps.DB == nil {
		st.Message = "not initialized"
		return st
	}
	sqlDB, err := deps.DB.DB()
	if err != nil {
		st.Message = err.Error()
		return st
	}
	ctx, cancel := context.WithTimeout(parent, readyzDepTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		st.Message = err.Error()
		return st
	}
	st.OK = true
	return st
}

func checkRedis(parent context.Context, deps Deps) depStatus {
	st := depStatus{Name: "redis"}
	if deps.Cache == nil {
		// cache 为 nil 是"启动时 Redis 不可用"的正常降级状态，不能 panic。
		st.Message = "not initialized"
		return st
	}
	ctx, cancel := context.WithTimeout(parent, readyzDepTimeout)
	defer cancel()
	// Client.Ping 本身是 nil-safe 的（返回 error 而不是 panic）。
	if err := deps.Cache.Ping(ctx); err != nil {
		st.Message = err.Error()
		return st
	}
	st.OK = true
	return st
}

func checkRabbitMQ(deps Deps) depStatus {
	st := depStatus{Name: "rabbitmq"}
	if deps.RMQ == nil || deps.RMQ.Connection() == nil {
		st.Message = "not initialized"
		return st
	}
	// 注意这里只报告状态、不触发重连：/readyz 是只读探针。
	// 重连由真正要用连接的地方（NewChannel）驱动，见 RabbitMQ 的注释。
	if deps.RMQ.IsClosed() {
		st.Message = "connection closed"
		return st
	}
	st.OK = true
	return st
}
