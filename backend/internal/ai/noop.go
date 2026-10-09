package ai

import (
	"context"
	"errors"
	"fmt"
)

// Noop 是"AI 关闭"时的 Client 实现，也是 P0 里**唯一**保证被注入的实现。
//
// 它存在的全部意义：让 ai.enabled=false 在运行期等价于"这些代码不存在"。
// 因此它有两条硬性约束，改动时别破坏：
//
//  1. **不发起任何网络调用**。不拨号、不建连接、不 ping、不重试。
//     连"连一下看看"都不行——那会让关闭 AI 的部署仍然依赖 sidecar 可达性，
//     而 P0 的验收里明确要求 sidecar 起不来时 API 与 Worker 照常服务。
//  2. **不分配需要释放的资源**。Close 永远是 nil，不必被 defer 保护。
//
// 另一点容易被忽略的好处：所有调用点都走同一个接口，于是"AI 关了"这条路径
// 也是被单测真实执行过的代码路径，而不是运行期才第一次走到的分支。
type Noop struct{}

// 编译期断言：Noop 必须始终满足 Client。接口一改，这里立刻编译失败，
// 而不是等到某个 service 在关闭 AI 的部署上 panic。
var _ Client = Noop{}

// NewNoop 返回一个关闭状态的客户端。显式构造函数，便于阅读装配代码。
func NewNoop() Noop { return Noop{} }

// Complete 恒定返回 ErrDisabled，且不做任何 IO。
//
// 返回错误而不是"返回空结果 + nil error"是刻意的：空结果会被上游当成
// "模型给出了空内容"写进库，那是脏数据；错误则会被明确丢弃。
func (Noop) Complete(ctx context.Context, _ Request) (Result, error) {
	return Result{}, fmt.Errorf("%w: 不调用模型", ErrDisabled)
}

// Health 报告"不可用"，而不是返回错误。
//
// 原因：总开关关闭是**配置决定的正常状态**，不是故障。返回 error 会让
// /readyz 之类的探针把它当异常处理，整条链路开始报警——那是纯粹的噪音。
func (Noop) Health(context.Context) (Health, error) {
	return Health{Serving: false, Detail: "ai.enabled=false，AI 功能未启用"}, nil
}

// Close 什么都不做。永远返回 nil：没有资源需要释放，也就不该有失败可能。
func (Noop) Close() error { return nil }

// ErrNotImplemented 供 P0 阶段"契约已定、实现未接"的方法使用。
//
// 存在的理由：P0 只交付骨架，CompleteStream 在 Go 侧还没有消费方。
// 与其留一个 return nil 的空实现（调用方拿到零值以为成功），
// 不如显式报错——"未实现"必须响亮，不能被当成"返回了空内容"。
var ErrNotImplemented = errors.New("ai: 该能力尚未实现")
