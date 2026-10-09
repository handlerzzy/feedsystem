package client

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/ai"
	aiv1 "github.com/handlerzzy/feedsystem/internal/aipb/ai/v1"
	"fmt"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// GRPCClient 通过 gRPC 调用 sidecar 上的 ModelGateway。
//
// 与 sidecar 的关系是"两个独立部署的进程"：本客户端**不假设**对方存在。
// 因此这里没有启动期连接、没有 ping、没有后台重连——连接由 grpc-go 在首次
// 调用时惰性建立，失败就是一次普通错误，交给调用方降级。
// 这条性质直接支撑 P0 的验收：sidecar 没起，API 与 Worker 照常启动并服务。
type GRPCClient struct {
	conn *grpc.ClientConn
	gw   aiv1.ModelGatewayClient
	opts ai.ClientOptions
}

// 编译期断言：接口漂移时立刻编译失败。
var _ ai.Client = (*GRPCClient)(nil)

// New 构造 gRPC 客户端。
//
// 参数是 ai.ClientOptions 而不是 config.AIConfig：本包因此不必 import 配置包，
// 单测可以直接构造 Options，也不必为了跑一个单测去拼 yaml。
//
// 不会在此处建连：grpc.NewClient 是惰性的，地址写错也不会在这里失败，
// 而是在第一次调用时变成一个可降级的错误。
func New(opts ai.ClientOptions) (*GRPCClient, error) {
	addr := opts.GatewayAddr
	if addr == "" {
		return nil, ai.ErrGatewayNotConfigured
	}
	// 只做一次格式校验就把地址交给 grpc：写错的地址拖到第一次调用才报，
	// 而且报出来的是"连接失败"，很难定位到"配置里写漏了端口"。
	if err := validateGatewayAddr(addr); err != nil {
		return nil, err
	}

	// insecure 是刻意的：sidecar 与 API/Worker 同处一个 compose 网络，
	// 走的是回环/容器内网。这里放 TLS 会带来证书分发问题，而收益只是
	// 保护一段本来就不出宿主机的流量。
	//
	// 注意这**不**意味着可以放松"API key 不出进程"这条纪律：
	// key 只存在于 sidecar 进程，Go 侧连 key 长什么样都不知道。
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("ai: 创建到 %s 的 gRPC 客户端失败: %w", addr, err)
	}
	return &GRPCClient{conn: conn, gw: aiv1.NewModelGatewayClient(conn), opts: opts}, nil
}

// Complete 调一次非流式补全。
func (c *GRPCClient) Complete(ctx context.Context, req ai.Request) (ai.Result, error) {
	if c == nil || c.gw == nil {
		return ai.Result{}, errors.New("ai: gRPC 客户端未初始化")
	}

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	pb, err := c.gw.Complete(ctx, c.toProto(req))
	if err != nil {
		return ai.Result{}, describeCallErr("Complete", err)
	}
	if isEmptyResponse(pb) {
		// 对端返回 (nil, nil) 是协议违规。注意 gRPC 会把空消息解成一个**非 nil
		// 的零值消息**，所以不能只判 nil。
		//
		// 为什么必须报错而不是返回"空成功"：空字符串会被上游当成模型输出
		// 写进库，那是脏数据——比一次明确的失败难查得多。
		return ai.Result{}, errors.New("ai: sidecar 返回了空响应")
	}

	return ai.Result{
		Text:    pb.GetText(),
		Model:   pb.GetModel(),
		Usage:   usageFromProto(pb.GetUsage()),
		Latency: 0, // 由调用方在更外层测量端到端耗时；这里不重复计时。
	}, nil
}

// Health 探测 sidecar。
func (c *GRPCClient) Health(ctx context.Context) (ai.Health, error) {
	if c == nil || c.gw == nil {
		return ai.Health{}, errors.New("ai: gRPC 客户端未初始化")
	}

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	resp, err := c.gw.Health(ctx, &aiv1.HealthRequest{})
	if err != nil {
		return ai.Health{}, describeCallErr("Health", err)
	}
	if resp == nil {
		return ai.Health{}, errors.New("ai: sidecar 返回了空健康响应")
	}
	return ai.Health{
		Serving:   resp.GetServing(),
		Version:   resp.GetVersion(),
		Detail:    resp.GetDetail(),
		Providers: resp.GetProviders(),
	}, nil
}

// Close 关闭连接。允许重复调用：gRPC 的 Close 对已关闭连接会返回错误，
// 而装配层会无脑 defer Close，把"重复关闭"升级成日志噪音没有意义。
func (c *GRPCClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// withTimeout 在本客户端上叠一层兜底超时。
//
// 调用方 ctx 已有的 deadline 更早时，WithTimeout 会取更早的那个——
// 这正是想要的：调用方最清楚自己能等多久，客户端只负责"别比它更久"。
func (c *GRPCClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.opts.TimeoutOr())
}

func (c *GRPCClient) toProto(req ai.Request) *aiv1.CompleteRequest {
	model := req.Model
	if model == "" {
		model = c.opts.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.opts.MaxTokensOr()
	}
	format := ""
	if req.JSON {
		format = "json"
	}
	return &aiv1.CompleteRequest{
		Model:          model,
		System:         req.System,
		Prompt:         req.Prompt,
		Temperature:    req.Temperature,
		MaxTokens:      int32(maxTokens),
		ResponseFormat: format,
		RequestId:      req.RequestID,
	}
}

func usageFromProto(u *aiv1.Usage) ai.Usage {
	if u == nil {
		// 老 sidecar 不上报 usage 是允许的（proto 里 usage 是可缺省字段）。
		// 这里按 0 记账，绝不因此报错——计量缺失不该让功能失败。
		return ai.Usage{}
	}
	return ai.Usage{
		PromptTokens:     int(u.GetPromptTokens()),
		CompletionTokens: int(u.GetCompletionTokens()),
		TotalTokens:      int(u.GetTotalTokens()),
		CostUSD:          u.GetCostUsd(),
	}
}

// describeCallErr 把 gRPC 状态码翻译成接口层的哨兵错误。
//
// 为什么必须做这层翻译（而不是把 status.Error 直接往上传）：
// grpc-go 的超时错误是 `status.Error(codes.DeadlineExceeded, "context deadline
// exceeded")`，它**不 wrap** context.DeadlineExceeded。调用点若按
// errors.Is(err, context.DeadlineExceeded) 判断"模型慢，静默降级"，
// 会永远判不出来，于是把所有超时都当成未知故障——日志噪音与误报紧随其后。
//
// 翻译规则：
//   - DeadlineExceeded / Canceled → 接口层的 ai.ErrTimeout，并用 %w 包住原始
//     status.Error，desc 里的细节不丢。
//   - Unavailable → ai.ErrUnavailable，文案点明 sidecar：这是 AI 最常见的状态
//     （sidecar 没部署），运维要能一眼知道该查什么。
//   - 其余原样返回：编造解释只会把排查引向错误方向。
//
// 所有分支都用 %w，调用点既能 errors.Is 判定哨兵，也能拿到底层错误。
func describeCallErr(op string, err error) error {
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w（%s）: %w", ai.ErrTimeout, op, err)
	case codes.Canceled:
		// 取消与超时必须分开：grpc-go 对两者都返回 status.Error，
		// 而 status.FromContextError 不 wrap context 错误，所以这里漏判的话，
		// "调用方取消"会被记成"模型超时"（review 抓到的真实误报）。
		return fmt.Errorf("%w（%s）: %w", ai.ErrCanceled, op, err)
	case codes.Unavailable:
		return fmt.Errorf("%w（%s）: %w", ai.ErrUnavailable, op, err)
	default:
		return err
	}
}

// validateGatewayAddr 校验 host:port 形式的地址。
//
// 为什么要自己再查一遍端口：net.SplitHostPort 认为 "sidecar:" 和 ":abc" 都是
// 合法输入（它只管冒号的位置）。放过去的话，错误会在第一次调用时以
// "连接失败"的形式出现，而真正的问题是配置里端口写漏了或写错了。
func validateGatewayAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ai: gateway_addr %q 非法（应形如 host:port）: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("ai: gateway_addr %q 缺少主机名", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("ai: gateway_addr %q 的端口不是数字: %w", addr, err)
	}
	if n <= 0 || n > 65535 {
		return fmt.Errorf("ai: gateway_addr %q 的端口超出 1-65535", addr)
	}
	return nil
}

// isEmptyResponse 判断一条响应是不是"没有可用内容"。
//
// 判据只有一条：**文本为空**。不是"所有字段都为空"。
//
// 为什么（review 抓到的真实缺陷）：sidecar 在"被 max_tokens 截断且内容为空"
// 时曾经返回 text="" 但 model 与 usage 都有值的响应，而旧的"三字段全空"判据
// 恰好放过了它——Go 侧于是拿到 (Result{Text:""}, nil)，那正是注释里承诺
// "绝不发生"的脏数据路径（空字符串被当成模型输出写进库）。
// 现在的判据与"调用方有没有可用内容"严格等价：空文本就是没有内容。
func isEmptyResponse(pb *aiv1.CompleteResponse) bool {
	return pb == nil || strings.TrimSpace(pb.GetText()) == ""
}
