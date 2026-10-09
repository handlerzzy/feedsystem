// Package aiapp 是 AI 能力的**装配层**：把配置翻译成 ai.Client。
//
// 为什么单独一个包（而不是塞进 internal/ai 或 internal/config）：
// 它是唯一同时认识"配置结构体"和"实现包"的地方。
//   - internal/ai 只放接口与 Noop，保持零重依赖（不 import grpc、不 import config）；
//   - internal/ai/client 只放 gRPC 实现，不认识配置文件；
//   - 两者之间的翻译与选择留在这里，于是"总开关的判断"在整个仓库里只有一处。
//
// 这处唯一性的价值：P0 的验收要求"ai.enabled=false 时无任何对外调用"。
// 只要开关判断只有一处，这条性质就是**结构上成立**的，而不是靠每个调用点自觉。
package aiapp

import (
	"errors"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/ai/client"
	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/feed"
)

// Options 把 config.AIConfig 翻译成实现层需要的参数。
//
// 单独抽出来是为了能被直接单测：翻译过程中的默认值填充
// （timeout<=0 用 2s 等）是最容易回归的部分，而它不需要真连 sidecar 才能验证。
func Options(cfg config.AIConfig) ai.ClientOptions {
	return ai.ClientOptions{
		GatewayAddr: cfg.GatewayAddr,
		Timeout:     cfg.TimeoutOr(),
		Model:       cfg.Model,
		MaxTokens:   cfg.MaxTokens,
		DailyBudget: cfg.DailyBudget,
	}
}

// NewClient 按总开关返回实现。这是全仓**唯一**读 cfg.Enabled 来决定
// "要不要真的发请求"的地方。
//
// 行为矩阵：
//
//	enabled=false                    → Noop（零网络调用，且不检查 gateway_addr）
//	enabled=true, gateway_addr 为空  → Noop，并返回一条说明原因（配置错误）
//	enabled=true, 地址非法           → Noop，并返回一条说明原因
//	enabled=true, 地址合法           → gRPC 客户端（惰性建连，sidecar 不在也不影响启动）
//
// 特别注意第三种与第四种：**任何一种情况下都不会返回 nil**。
// 配置写错时的正确反应是"降级 + 让运维看见"，而不是让 API/Worker 起不来——
// 那是拿承重墙给锦上添花的功能陪葬（见总览第 2 节红线 4）。
//
// 第二个返回值是"为什么没用真实客户端"的说明，调用方据此决定是否记 Warn：
// enabled=false 是预期状态，说明为 nil，不该产生日志。
func NewClient(cfg config.AIConfig) (ai.Client, error) {
	if !cfg.Enabled {
		// 关闭状态下连地址都不看：一个被关掉的功能不该因为别的配置项写错而报错。
		return ai.NewNoop(), nil
	}

	c, err := client.New(Options(cfg))
	if err != nil {
		// 配置问题导致的降级。返回 Noop 而不是 nil，调用方代码路径保持不变。
		return ai.NewNoop(), err
	}
	return c, nil
}

// EmbeddingOptions 把 config.AIConfig 翻译成 embedding 层需要的参数。
//
// 与 Options 分开而不是共用一个结构体：两条通道的默认值、超时与校验维度
// 都不一样（见 internal/ai/embedding.go 顶部的三条理由）。
// 共用一个结构体会让"给 chat 改超时"顺手改掉 embedding 的超时。
func EmbeddingOptions(cfg config.AIConfig) ai.EmbeddingOptions {
	return ai.EmbeddingOptions{
		// 与 chat 复用同一个 sidecar 地址：它们是同一个进程的两个方法。
		// 分开配置只会制造"chat 连 A、向量连 B"这种没人需要的状态。
		GatewayAddr: cfg.GatewayAddr,
		Timeout:     cfg.EmbeddingTimeoutOr(),
		Model:       cfg.EmbeddingModelOr(),
		Dim:         cfg.EmbeddingDimOr(),
		Normalize:   cfg.EmbeddingNormalizeOr(),
	}
}

// NewEmbeddingClient 按总开关返回 embedding 实现。
//
// 与 NewClient 完全同构，包括"任何情况下都不返回 nil"这一条：
// 开关判断在整个仓库里只有这两处，且都以 cfg.Enabled 开头。
// ai.enabled=false 时返回 NoopEmbedding，**零外呼**——这正是
// 「总开关关闭后不得有任何 embedding 调用」这条验收项的落地点。
//
// 第二个返回值是"为什么没用真实客户端"的说明，调用方据此决定是否记 Warn。
func NewEmbeddingClient(cfg config.AIConfig) (ai.EmbeddingClient, error) {
	if !cfg.Enabled {
		return ai.NewNoopEmbedding(), nil
	}
	c, err := client.NewEmbedding(EmbeddingOptions(cfg))
	if err != nil {
		return ai.NewNoopEmbedding(), err
	}
	return c, nil
}

// RerankClient 按配置返回一个 LLM 精排器。永远不返回 nil。
//
// 与 NewEmbeddingClient 同构，但多一层判断：**精排默认关闭**
// （ai.rerank_enabled=false），而关闭时返回 nil 而不是 Noop。
//
// 为什么这里与另外两个客户端不一致（它们都返回 Noop）：
// 精排的"关闭"不是"调用后失败"，而是"根本不该存在于读路径上"。
// 返回 Noop 会让 feed 包每次请求都走一遍"构造候选 → 拼 prompt → 调用 →
// 拿到 ErrDisabled → 降级"的完整流程——一次都不会真的外呼，但
// CPU 与日志都是白花的，而且它会掩盖"这条路上还挂着一次模型调用"。
// 返回 nil 让 feed 包在装配期就知道"这层不存在"，判断只有一次。
//
// 第二个返回值是"为什么没有精排器"的说明，调用方据此决定是否记日志：
// 开关关闭是预期状态（说明为 nil），开关打开但配置不全才是要看的。
func RerankClient(cfg config.AIConfig, client ai.Client) (feed.RerankClient, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if !cfg.RerankEnabled {
		// 这是**默认状态**：说明为 nil，不产生日志。
		return nil, nil
	}
	if client == nil {
		return nil, errors.New("ai: 精排已开启但没有可用的模型客户端")
	}
	return feed.NewAIModelReranker(client, cfg.Model), nil
}
