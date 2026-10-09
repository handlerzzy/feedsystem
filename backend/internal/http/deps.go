package http

import (
	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/worker"

	"gorm.io/gorm"
)

// Deps 是 API 进程在一次启动中构造好的依赖。
// 注意：SetRouter 只负责"注册路由"，不再负责"启动后台任务"。
type Deps struct {
	DB     *gorm.DB
	Cache  *rediscache.Client
	RMQ    *rabbitmq.RabbitMQ
	SSEHub *worker.SSEHub // 由 cmd/main.go 创建，保证全局唯一

	// Config 是启动时加载的配置。
	//
	// 为什么需要它（P2）：AI 的三个开关（总开关 / recall_quota / rerank_enabled）
	// 与两个向量参数（embedding_model / dim）决定读路径的行为，而它们
	// **必须来自同一份配置对象**。此前 router 里没有配置，AI 的装配只能
	// 通过别的方式推断（例如重新读一次配置文件）——那会让"进程实际生效的
	// 配置"与"路由装配时看到的配置"变成两份可能不同的东西。
	//
	// 零值时一切 AI 功能关闭（红线 4：新增配置项的缺省 = 全关），
	// 因此 SetRouter(Deps{}) 仍然是一个合法调用（router_binding_test.go 用它）。
	Config config.Config

	// TODO(阶段三): 以下依赖目前仍在 SetRouter 内部构造，逐步上移到 cmd/main.go。
	// AccountRepo 等字段先留空，不要在本项里填。
}
