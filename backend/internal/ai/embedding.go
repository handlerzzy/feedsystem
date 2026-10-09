package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 本文件定义 **embedding 通道**的消费方接口。
//
// 为什么它与 Client 分开，而不是往 Client 上加一个 Embed 方法：
//
//  1. **语义完全不同**。chat 是"生成"（temperature、max_tokens、流式），
//     embedding 是"编码"（批量、维度、归一化）。混在一起之后，
//     每个消费方都要被迫依赖它根本用不到的那一半。
//  2. **超时/重试/配额/成本的口径不同**。一次 embedding 通常 50~200ms、
//     按输入 token 计费、天然适合批量；chat 可能几秒、按输入输出分别计费。
//     放进同一个接口之后，为 chat 定的超时会去掐断 embedding 的批量请求，
//     或者反过来让 embedding 的宽松超时把 chat 的失败拖长——
//     这种"互相污染"在监控上表现为两类调用的错误率一起抖。
//  3. **失败处置不同**。chat 失败要重试（打标是异步的）；embedding 失败
//     直接关掉语义路即可（P2 的降级设计），重试只会把一次故障放大。
//
// 接口定义在消费方（本包），与本项目既有惯例一致（见 internal/feed/feed_store.go）。
//
//go:generate mockgen -destination=mock/embedding_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/ai EmbeddingClient

// ErrEmbeddingUnavailable 表示 embedding 服务不可用。
//
// 单独一个哨兵的理由：P2 的语义路只需要区分"能用"和"不能用"，
// 不能用就静默关掉整条路（Feed 退回原有排序）。把它与 chat 的错误混在
// 一起会让"向量服务没配 key"这条最常见的状态淹没在通用错误里。
var ErrEmbeddingUnavailable = errors.New("ai: embedding 服务不可用")

// DefaultEmbeddingTimeout 是单次 embedding 调用的兜底超时。
//
// 比 chat 的 2 秒宽：embedding 是**批量**的（一次最多 64 条），
// 而且它只在异步路径（回填）与带硬预算的读路径（精排前的召回）上使用。
// 取值依据见 docs/AI-P2-前置条件.md 的 D1：精排总预算 200ms 以内，
// 而向量召回要么走缓存、要么用更小的超时，不会吃掉全部预算。
const DefaultEmbeddingTimeout = 3 * time.Second

// DefaultEmbeddingModel / DefaultEmbeddingDim 是**定死**的默认值与维度。
//
// 它们必须与 proto 注释、sidecar 配置、以及向量表里记录的 model/dim 一致。
// 换模型时这三处必须同时改，并且所有已入库的向量都要重算——
// 这就是为什么向量表要存 model 与 dim（见 video.VideoEmbedding）。
const (
	DefaultEmbeddingModel = "text-embedding-3-small"
	DefaultEmbeddingDim   = 1536
)

// EmbeddingRequest 是一次向量化请求。
//
// 与 aipb.EmbedRequest 分开的理由同 Request：pipeline 层不应该知道 protobuf 的存在。
type EmbeddingRequest struct {
	// Inputs 是待向量化的文本，一次最多 64 条（sidecar 会拒绝超限请求）。
	//
	// 只要有一条为空或全空白，整批就应该被判为失败——
	// 这不是接口层的判断，而是调用方的责任：空文本会得到零向量，
	// 零向量与任何东西的余弦都是 0，看起来像"这条内容没有语义"。
	Inputs []string
	// Model 留空时用 Options.Model（再留空则由 sidecar 决定）。
	Model string
	// Normalize 要求返回 L2 归一化后的向量。
	//
	// 默认应为 true：没归一化的向量在余弦检索里会让**更长的文本**系统性占优，
	// 而这种偏差在结果里看起来像"模型偏好长内容"，几乎不可能反查到。
	Normalize bool
	// RequestID 用于把 Go 日志与 sidecar 日志对上。
	RequestID string
}

// EmbeddingResult 是一次向量化的结果。
type EmbeddingResult struct {
	// Vectors 与 EmbeddingRequest.Inputs **一一对应且顺序一致**。
	//
	// 契约：len(Vectors) == len(Inputs) 且每条长度 == Dim。
	// 实现层必须校验这两条，不满足时返回错误而不是"按能对上的部分返回"——
	// 文本与向量错配会让检索结果永久变差，且完全看不出来。
	Vectors [][]float32
	// Model 是实际使用的模型（由 sidecar 回填，可能是它的默认值）。
	Model string
	// Dim 是所有向量的维度。
	Dim int
	// Normalized 是 sidecar **实际**做的处理（不是回显请求）。
	Normalized bool
	// Usage 用于成本核算；缺失时按 0 记账，不得因此报错。
	Usage Usage
	// Latency 由调用方在更外层测量。
	Latency time.Duration
}

// EmbeddingHealth 描述向量通道的可用性。
//
// 与 Health 分开：chat 有 key 不代表向量可用（它们是两套凭据与两个模型）。
type EmbeddingHealth struct {
	Serving bool
	Detail  string
}

// EmbeddingClient 是向量化能力的唯一入口。
//
// 纪律与 Client 相同且同样不可协商：
//   - 每个方法都接受 ctx 并遵守超时；
//   - 实现不得在内部无限重试（重试由调用方决定）；
//   - 错误信息里不得出现 API key。
type EmbeddingClient interface {
	// Embed 批量生成向量。失败时返回错误，由调用方决定降级行为
	// （P2 的语义路是"直接关掉这一路"，不重试）。
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResult, error)

	// Health 探测向量通道。与 Client.Health 一样：失败是常态而不是意外。
	Health(ctx context.Context) (EmbeddingHealth, error)

	// Close 释放连接。允许重复调用，也允许在未连接时调用。
	Close() error
}

// EmbeddingOptions 是装配一个真实 embedding 客户端所需的全部参数。
type EmbeddingOptions struct {
	// GatewayAddr 是 sidecar 的 host:port，与 chat 共用同一个进程。
	GatewayAddr string
	// Timeout 是单次调用的兜底超时，<=0 时用 DefaultEmbeddingTimeout。
	Timeout time.Duration
	// Model 是请求的默认模型，可被 EmbeddingRequest.Model 覆盖。
	Model string
	// Dim 是期望的维度。客户端用它做**本地校验**：sidecar 返回的维度
	// 与它不一致时直接判为失败，而不是把一批维度不对的向量写进库。
	Dim int
	// Normalize 是默认是否要求归一化。
	Normalize bool
}

// TimeoutOr 返回生效的兜底超时。
func (o EmbeddingOptions) TimeoutOr() time.Duration {
	if o.Timeout <= 0 {
		return DefaultEmbeddingTimeout
	}
	return o.Timeout
}

// ModelOr 返回生效的默认模型。
func (o EmbeddingOptions) ModelOr() string {
	if o.Model == "" {
		return DefaultEmbeddingModel
	}
	return o.Model
}

// DimOr 返回生效的期望维度。<=0 表示"不校验"。
//
// 之所以允许关闭校验：维度是"部署时才知道"的参数（换模型就要改），
// 强制要求配置里写对会让一次模型切换变成一次"先改配置才能验证"的循环。
// 配错时会与 proto 注释、sidecar 配置不一致，那条链路有它自己的校验。
func (o EmbeddingOptions) DimOr() int {
	if o.Dim <= 0 {
		return 0
	}
	return o.Dim
}

// NoopEmbedding 是总开关关闭时使用的实现：任何调用都立刻失败，且不产生 IO。
//
// 与 Noop 同样的理由：调用点不需要自己判断 cfg.Enabled，
// "漏判一处"就等于一次不该发生的模型调用。
type NoopEmbedding struct{}

// 编译期断言：接口漂移时立刻编译失败。
var _ EmbeddingClient = (*NoopEmbedding)(nil)

// NewNoopEmbedding 构造一个不做任何事情的 embedding 客户端。
func NewNoopEmbedding() *NoopEmbedding { return &NoopEmbedding{} }

// Embed 返回 ErrDisabled。**不做任何 IO**。
func (n *NoopEmbedding) Embed(context.Context, EmbeddingRequest) (EmbeddingResult, error) {
	return EmbeddingResult{}, ErrDisabled
}

// Health 报告未提供服务。返回 nil 错误：调用方不该把"AI 关了"当故障。
func (n *NoopEmbedding) Health(context.Context) (EmbeddingHealth, error) {
	return EmbeddingHealth{Serving: false, Detail: "ai.enabled=false，向量通道关闭"}, nil
}

// Close 什么都不做。
func (n *NoopEmbedding) Close() error { return nil }

// ValidateEmbeddingResult 校验一次 embedding 结果是否自洽。
//
// 放在接口层而不是各实现里：这是**接口契约**的一部分，换实现也不该失效，
// 而每个实现各写一份必然有一份先漂移（漂移的后果是错位入库）。
func ValidateEmbeddingResult(res EmbeddingResult, wantInputs, wantDim int) error {
	if len(res.Vectors) != wantInputs {
		return fmt.Errorf("ai: embedding 返回 %d 条向量，与输入的 %d 条不一致",
			len(res.Vectors), wantInputs)
	}
	dim := res.Dim
	if dim <= 0 {
		return errors.New("ai: embedding 结果的维度为 0")
	}
	if wantDim > 0 && dim != wantDim {
		return fmt.Errorf("ai: embedding 维度为 %d，与期望的 %d 不一致（换模型时必须同步更新配置并重算全部向量）",
			dim, wantDim)
	}
	for i, v := range res.Vectors {
		if len(v) != dim {
			return fmt.Errorf("ai: embedding 第 %d 条的维度为 %d，与声明的 %d 不一致", i, len(v), dim)
		}
	}
	return nil
}
