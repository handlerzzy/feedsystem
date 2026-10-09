package rabbitmq

import amqp "github.com/rabbitmq/amqp091-go"

// Topology 描述一组 topic 拓扑：exchange + queue + binding key。
//
// 存在的理由：这组三元组此前在项目里存在两份——一份在 internal/middleware/rabbitmq
// 的各 NewXxxMQ 里，另一份是 cmd/worker/main.go 手抄的 16 个常量加 4 个
// declareXxxTopology 函数（约 138 行）。两份内容逐字节相同，但**行为不同**：
//
//	DeclareTopic（API 侧用）会顺带调用 DeclareDLX
//	worker 手抄的那 4 个函数不会
//
// 于是 worker 先于 API 启动时（compose 里 worker 不依赖 api，且两边都是
// restart: always，每次重启都会竞争），死信交换机 dlx.events 并不存在；
// 队列虽然声明了 x-dead-letter-exchange，但消息被拒绝/过期时会被 broker
// 静默丢弃。声明拓扑只应有唯一来源。
type Topology struct {
	Exchange   string
	Queue      string
	BindingKey string
}

// Declare 幂等地声明该拓扑（含死信交换机与死信队列）。
func (t Topology) Declare(ch *amqp.Channel) error {
	return DeclareTopic(ch, t.Exchange, t.Queue, t.BindingKey)
}

// 各事件流的拓扑。API 侧由对应的 NewXxxMQ 使用，worker 侧直接取用。

func LikeTopology() Topology {
	return Topology{likeExchange, likeQueue, likeBindingKey}
}

func CommentTopology() Topology {
	return Topology{commentExchange, commentQueue, commentBindingKey}
}

func SocialTopology() Topology {
	return Topology{socialExchange, socialQueue, socialBindingKey}
}

func PopularityTopology() Topology {
	return Topology{popularityExchange, popularityQueue, popularityBindingKey}
}

func TimelineTopology() Topology {
	return Topology{timelineExchange, timelineQueue, timelineBindingKey}
}

// ContentAnalysisTopology 是 P0 新增的第 6 个拓扑（AI 内容分析任务）。
//
// 与上面 5 个既有拓扑的关系：**只新增，不改动**。既有的 exchange / queue /
// binding key 常量一个字节都没动，因此旧版本进程与新版本进程可以同时运行
// （滚动升级期间两边声明的是各自那套拓扑，互不影响）。
//
// 形状照抄 PopularityTopology：同样通过 Declare 走 DeclareTopic，
// 因此自动带上持久化、DLX 与死信队列。
//
// 关于 DLX 的实际作用范围（别把它当成"处理失败就会进死信"）：
// worker 在重试耗尽时是 **Ack 丢弃**，不会死信；只有被显式拒绝
// （Nack requeue=false）、消息过期或队列超长时 broker 才会投进 DLX。
// 这里保留 DLX 是为了与其余 5 个拓扑保持一致、并在将来引入 TTL/长度上限时
// 有地方可落，而不是当前就有一条"失败进死信"的路径。
func ContentAnalysisTopology() Topology {
	return Topology{contentAnalysisExchange, contentAnalysisQueue, contentAnalysisBindingKey}
}
