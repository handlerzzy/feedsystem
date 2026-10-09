package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/handlerzzy/feedsystem/internal/ai"
	aiv1 "github.com/handlerzzy/feedsystem/internal/aipb/ai/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// EmbeddingGRPCClient 通过 gRPC 调用 sidecar 上的 ModelGateway.Embed。
//
// 与 GRPCClient 是**两个独立的连接**，不复用同一个 ClientConn：
//
//   - 它们的生命周期不同。chat 只在异步 worker 里用，embedding 还会被
//     回填任务与（P2 的）召回路径使用，任一方关闭连接都不该影响另一方；
//   - 它们的超时不同（见 ai.EmbeddingOptions 的注释）。共用一个 conn 时
//     超时仍然可以各自设置，但把两者分开能让"哪条通道被关闭了"
//     在 Close 的语义上也是清楚的。
//
// 与 sidecar 的关系同样是"两个独立部署的进程"：本客户端不假设对方存在，
// 惰性建连，失败就是一次普通错误，交给调用方降级。
type EmbeddingGRPCClient struct {
	conn *grpc.ClientConn
	gw   aiv1.ModelGatewayClient
	opts ai.EmbeddingOptions
}

// 编译期断言：接口漂移时立刻编译失败。
var _ ai.EmbeddingClient = (*EmbeddingGRPCClient)(nil)

// NewEmbedding 构造 embedding 的 gRPC 客户端。
func NewEmbedding(opts ai.EmbeddingOptions) (*EmbeddingGRPCClient, error) {
	addr := opts.GatewayAddr
	if addr == "" {
		return nil, ai.ErrGatewayNotConfigured
	}
	// 与 chat 客户端共用同一份地址校验：两处各写一份的话，
	// 一边修好了端口校验、另一边还在放 ":abc" 过去。
	if err := validateGatewayAddr(addr); err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("ai: 创建到 %s 的 embedding gRPC 客户端失败: %w", addr, err)
	}
	return &EmbeddingGRPCClient{conn: conn, gw: aiv1.NewModelGatewayClient(conn), opts: opts}, nil
}

// Embed 批量生成向量。
func (c *EmbeddingGRPCClient) Embed(ctx context.Context, req ai.EmbeddingRequest) (ai.EmbeddingResult, error) {
	if c == nil || c.gw == nil {
		return ai.EmbeddingResult{}, errors.New("ai: embedding gRPC 客户端未初始化")
	}
	if len(req.Inputs) == 0 {
		// 在本地就拒绝：一次注定被 sidecar 拒绝的往返只会白吃一次超时预算，
		// 而且错误消息会从"调用方传了空列表"变成"sidecar 报 bad_request"。
		return ai.EmbeddingResult{}, errors.New("ai: embedding 请求的 inputs 不能为空")
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.TimeoutOr())
	defer cancel()

	pb, err := c.gw.Embed(ctx, c.toProto(req))
	if err != nil {
		return ai.EmbeddingResult{}, describeCallErr("Embed", err)
	}
	res, err := embeddingFromProto(pb, req)
	if err != nil {
		return ai.EmbeddingResult{}, err
	}
	// 本地再校验一遍维度与条数。
	//
	// 为什么对端已经校验过还要再查：这一层的意义是**契约**，
	// 换实现对端（或对端降级成一个只回空壳的版本）时它仍然成立。
	// 带着错位或错维度的向量入库，只会在检索质量上体现出来。
	if err := ai.ValidateEmbeddingResult(res, len(req.Inputs), c.opts.DimOr()); err != nil {
		return ai.EmbeddingResult{}, err
	}
	return res, nil
}

// Health 探测向量通道。
//
// 复用 ModelGateway.Health：embedding 与 chat 在同一个 sidecar 进程里，
// 单独加一个 Health 方法只会让协议多一个必须同步维护的字段。
// 这里从 detail 里读 embedding 的可用性——它由 sidecar 拼在 detail 末尾。
func (c *EmbeddingGRPCClient) Health(ctx context.Context) (ai.EmbeddingHealth, error) {
	if c == nil || c.gw == nil {
		return ai.EmbeddingHealth{}, errors.New("ai: embedding gRPC 客户端未初始化")
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.TimeoutOr())
	defer cancel()

	resp, err := c.gw.Health(ctx, &aiv1.HealthRequest{})
	if err != nil {
		return ai.EmbeddingHealth{}, describeCallErr("Health", err)
	}
	if resp == nil {
		return ai.EmbeddingHealth{}, errors.New("ai: sidecar 返回了空健康响应")
	}
	detail := resp.GetDetail()
	return ai.EmbeddingHealth{
		// serving 为 false 时向量通道必然不可用（总开关关闭或没有 chat provider）。
		// 但 serving 为 true 也**不代表**向量可用：chat 与 embedding 的凭据、
		// 模型、端点都是独立的。因此这里额外看 detail 里的 embedding=[...]。
		Serving: resp.GetServing() && embeddingProvidersFromDetail(detail) != "",
		Detail:  detail,
	}, nil
}

// Close 关闭连接。允许重复调用。
func (c *EmbeddingGRPCClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *EmbeddingGRPCClient) toProto(req ai.EmbeddingRequest) *aiv1.EmbedRequest {
	model := req.Model
	if model == "" {
		model = c.opts.Model
	}
	return &aiv1.EmbedRequest{
		Inputs:    req.Inputs,
		Model:     model,
		Normalize: req.Normalize,
		RequestId: req.RequestID,
	}
}

func embeddingFromProto(pb *aiv1.EmbedResponse, req ai.EmbeddingRequest) (ai.EmbeddingResult, error) {
	if pb == nil {
		return ai.EmbeddingResult{}, errors.New("ai: sidecar 返回了空 embedding 响应")
	}
	vectors := make([][]float32, 0, len(pb.GetVectors()))
	for _, v := range pb.GetVectors() {
		if v == nil {
			// 单条为空是协议违规：返回一个空向量会让"这一条没有语义"
			// 混进库里，而它其实是传输或对端的问题。
			return ai.EmbeddingResult{}, errors.New("ai: sidecar 返回了空的向量条目")
		}
		vectors = append(vectors, v.GetValues())
	}
	if req.Normalize && !pb.GetNormalized() {
		// 请求与实现不一致时**不能静默接受**：没归一化的向量在余弦检索里
		// 会让更长的文本系统性占优，而分数看起来仍然正常。
		return ai.EmbeddingResult{}, errors.New("ai: 请求要求归一化，但 sidecar 返回了未归一化的向量")
	}
	return ai.EmbeddingResult{
		Vectors:    vectors,
		Model:      pb.GetModel(),
		Dim:        int(pb.GetDim()),
		Normalized: pb.GetNormalized(),
		Usage:      usageFromProto(pb.GetUsage()),
	}, nil
}

// embeddingProvidersFromDetail 从 Health 的 detail 里取出 embedding 的 provider 列表。
//
// 为什么靠解析文本：不改 proto 是硬约束（只增不删，而且要动就得两边同时发版）。
// detail 是给人与脚本共用的诊断字段，sidecar 侧拼的格式是稳定的
// （见 gateway.ts 的 health()），并且这里的解析失败只会让
// Health 报"不可用"——保守方向，不会误报可用。
func embeddingProvidersFromDetail(detail string) string {
	const marker = "embedding=["
	idx := strings.Index(detail, marker)
	if idx < 0 {
		return ""
	}
	rest := detail[idx+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
