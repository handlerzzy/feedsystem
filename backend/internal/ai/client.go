// Package ai 定义 AI 能力的**消费方接口**，以及"AI 关掉时"使用的实现。
//
// 为什么接口定义在这里（而不是定义在某个实现包里）：
// 这是本项目明确的惯例（见 internal/feed/feed_store.go 的长注释）。
// 消费方声明"我需要什么"，实现方去满足它。好处在 AI 这个场景里尤其明显——
//
//   - 单测不需要 sidecar、不需要 API key、不需要网络：注入 Noop 或 mockgen
//     生成的 mock 就能覆盖全部逻辑。
//   - P1~P4 的每个新场景（打标、审核、Agent）都只依赖这个窄接口，
//     换 provider、换 SDK、把 sidecar 整个删掉，业务代码一行都不用改。
//
// 本包**不 import internal/config，也不 import grpc**：
//   - 配置结构体的细节属于装配层，渗进接口层后每次配置字段调整都会波及所有消费方；
//   - grpc 是"真实实现"的依赖，接口层保持零重依赖，任何只做编排的逻辑
//     （未来的打标 pipeline 等）才能在没有 sidecar 的环境里被单测覆盖。
//
// 装配放在 internal/aiapp：它同时认识本包与 internal/config，
// 是"总开关 -> 用哪个实现"这段逻辑的唯一落地点。
package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

//go:generate mockgen -destination=mock/client_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/ai Client

// ErrDisabled 表示 AI 总开关关闭（ai.enabled=false）。
//
// 为什么要有一个**可判定**的错误，而不是让调用方自己 if cfg.AI.Enabled：
// 调用点分散在 service / worker / 未来的 handler 里，"每个地方自己判断一次"
// 必然会漏掉一处，而漏掉的那一处就是一次不该发生的模型调用。
// 统一由 Noop 返回这个错误，调用方只需 errors.Is(err, ai.ErrDisabled)
// 就能把"AI 关了"与"模型真的失败了"分开处理（前者不该记 Warn，后者要）。
var ErrDisabled = errors.New("ai: 功能已关闭（ai.enabled=false）")

// ErrGatewayNotConfigured 表示开关是开的但没给 sidecar 地址。
// 与 ErrDisabled 区分开：这是**配置错误**，值得记一条 Warn 让运维看见；
// 而 ErrDisabled 是预期状态，不该产生任何日志噪音。
var ErrGatewayNotConfigured = errors.New("ai: gateway_addr 未配置")

// ErrTimeout 表示一次调用在超时预算内没有拿到结果。
//
// 为什么接口层要定义自己的超时哨兵，而不是复用 context.DeadlineExceeded：
// 真实链路是 grpc-go 把 deadline 变成 `status.Error(codes.DeadlineExceeded, ...)`，
// 而**那个错误不 wrap context.DeadlineExceeded**——errors.Is 判定不出来。
//
// 于是有两种做法：让接口层 import grpc 去判 status code，或者由实现层把状态码
// 翻译成这个哨兵。选后者：接口层保持零重依赖、调用点不必知道传输协议，
// 这两条都比省一个变量重要。实现层仍会保留原始错误（%w），排查信息不丢。
var ErrTimeout = errors.New("ai: 调用超时")

// ErrCanceled 表示调用方主动取消（进程退出、上游请求被取消）。
//
// 与 ErrTimeout 分开的理由：它们的处置完全不同。超时说明"模型太慢"，
// 是要看的信号；取消是预期内的生命周期事件，不该刷告警。
// 而 grpc-go 的取消错误同样不 wrap context.Canceled，所以必须由实现层翻译。
var ErrCanceled = errors.New("ai: 调用已取消")

// ErrUnavailable 表示 sidecar 不可达（没启动、已退出、或网络不通）。
//
// 单独一个哨兵的价值在于分级：这是 AI 最常见的状态，日志里应当一眼看出
// "sidecar 不在，业务已按预期降级"，而不是被归进"未知错误"里天天排查。
var ErrUnavailable = errors.New("ai: sidecar 不可达")

// 默认值。与 internal/config 里的默认值保持一致：
// 配置层负责"yaml 里没写"，这里负责"调用方直接构造 Options 时忘了写"。
const (
	DefaultTimeout   = 2 * time.Second
	DefaultMaxTokens = 512
)

// Usage 是一次调用的计量结果。字段缺失（老 sidecar）时按 0 处理，不得报错。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
}

// Request 是一次补全请求。
//
// 刻意与 aipb.CompleteRequest 分开：pipeline 层不应该知道 protobuf 的存在，
// 否则有一天换掉传输协议（比如换成 HTTP）会波及所有业务代码。
type Request struct {
	// System / Prompt 分开传递：多数 provider 把它们当不同角色的消息，
	// 合并成一条字符串之后就再也拆不回来了。
	System string
	Prompt string
	// Temperature / MaxTokens 为 0 表示"用默认值"，不是"显式指定 0"。
	Temperature float64
	MaxTokens   int
	// JSON 为 true 时要求模型返回合法 JSON 对象。
	JSON bool
	// Model 留空时用 Options.Model（再留空则由 sidecar 决定）。
	Model string
	// RequestID 用于把 Go 日志与 sidecar 日志对上，由调用方生成。
	RequestID string
}

// Result 是一次补全的结果。
type Result struct {
	Text  string
	Model string
	Usage Usage
	// Latency 由客户端测量并填上：模型侧的耗时含排队，不测这里就没有任何
	// 可用于容量判断的数字。用本地时钟测，不信任对端上报。
	Latency time.Duration
}

// Health 描述 sidecar 的可用性。
type Health struct {
	Serving bool
	Version string
	Detail  string
	// Providers 是已配置 key 的 provider 名，仅用于诊断展示。
	// 契约上禁止包含 key 本身（见 model_gateway.proto 的注释）。
	Providers []string
}

// Client 是所有 AI 能力的唯一入口。
//
// 纪律（三件必须同时成立的事，缺一个就会破坏"AI 不是承重墙"的前提）：
//  1. **每个方法都必须接受 ctx 并遵守超时**：调用方给的 deadline 永远优先，
//     客户端自己的超时只是兜底。没有 ctx 的方法一旦被放进请求路径，
//     模型慢下来就等于用户请求慢下来。
//  2. **实现不得在内部无限重试**：重试要由调用方决定（异步 worker 有自己的
//     退避策略，同步读路径则应该直接放弃）。藏在客户端里的重试会把一次
//     2 秒超时悄悄变成 10 秒。
//  3. **不得在错误信息里带 API key**：错误会被写进日志与响应体。
type Client interface {
	// Complete 非流式补全。失败时返回错误，由调用方决定降级行为。
	Complete(ctx context.Context, req Request) (Result, error)

	// Health 探测 sidecar 是否可用。
	// 注意：Health 失败**不是**错误路径上的意外，而是常态（sidecar 可能压根
	// 没部署），所以它返回的 error 只用于诊断，调用方应当照常降级。
	Health(ctx context.Context) (Health, error)

	// Close 释放连接。允许重复调用，也允许在未连接时调用。
	Close() error
}

// ClientOptions 是装配一个真实客户端所需的全部参数。
//
// 放在接口层而不是实现包：装配逻辑（internal/aiapp）需要构造它，
// 而装配层为了不 import 具体实现，必须有一个双方都认识的类型。
// 它刻意只包含"连接与默认值"，不含任何配置文件的形状。
type ClientOptions struct {
	// GatewayAddr 是 sidecar 的 host:port。为空视为未配置（等价于关闭）。
	GatewayAddr string
	// Timeout 是单次调用的兜底超时，<=0 时用 DefaultTimeout。
	Timeout time.Duration
	// Model 是请求的默认模型，可被 Request.Model 覆盖。
	Model string
	// MaxTokens 是默认生成上限，可被 Request.MaxTokens 覆盖。
	MaxTokens int
	// DailyBudget 是预留占位，当前只做记录，不做拦截。
	DailyBudget float64
}

// TimeoutOr 返回生效的兜底超时。
//
// 做成"取值时兜底"而不是"构造时填默认值"：直接构造 ClientOptions{} 的单测
// 也拿得到安全值，不会出现"0 超时 = 立即超时"这种极难查的行为。
func (o ClientOptions) TimeoutOr() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

// MaxTokensOr 返回生效的默认生成上限。
func (o ClientOptions) MaxTokensOr() int {
	if o.MaxTokens <= 0 {
		return DefaultMaxTokens
	}
	return o.MaxTokens
}

// StatusFor 把一次调用错误翻译成日志友好的描述。
//
// 存在的理由：调用点需要区分几种结局，而它们的处理方式完全不同——
//   - ErrDisabled / ErrGatewayNotConfigured：配置或开关导致，不该刷 Warn；
//   - 超时 / 取消：模型偶尔慢，Warn 即可；
//   - sidecar 不可达：部署状态，Warn 且文案要能指向 sidecar；
//   - 其他：需要人看的信号。
//
// 把这套判断集中在这里，避免每个调用点各写一份（然后各漏一种情况）。
//
// 判定一律走 errors.Is：错误在到达调用点之前会经过若干层 %w 包装，
// 用 == 比较会在包装出现的那一天静默失效（表现为日志分级整体错位）。
func StatusFor(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrDisabled):
		return "disabled"
	case errors.Is(err, ErrGatewayNotConfigured):
		return "gateway_not_configured"
	// 超时与取消分两级匹配：既认接口层的哨兵（实现层翻译过来的），
	// 也认 context 原生错误（调用方自己传了带 deadline 的 ctx 时可能直接拿到）。
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrCanceled), errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return fmt.Sprintf("error: %v", err)
	}
}
