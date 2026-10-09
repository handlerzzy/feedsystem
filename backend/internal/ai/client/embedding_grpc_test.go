package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	aiv1 "github.com/handlerzzy/feedsystem/internal/aipb/ai/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// 本文件与 grpc_test.go 用同一套做法（进程内 bufconn 假服务端），
// 覆盖 embedding 客户端的报文映射与错误翻译。
//
// 与 chat 分开测是刻意的：两条通道的**契约不同**（批量、维度、归一化），
// 复用一个假服务端会让"Embed 的字段名写错了"这类问题被 Complete 的
// 用例掩盖过去。

// fakeEmbedGateway 是可控的 ModelGateway 服务端（embedding 部分）。
type fakeEmbedGateway struct {
	aiv1.UnimplementedModelGatewayServer

	lastReq atomic.Pointer[aiv1.EmbedRequest]
	embedFn func(context.Context, *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error)
	health  *aiv1.HealthResponse
}

func (f *fakeEmbedGateway) Embed(ctx context.Context, req *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
	f.lastReq.Store(req)
	if f.embedFn != nil {
		return f.embedFn(ctx, req)
	}
	return &aiv1.EmbedResponse{
		Vectors:    []*aiv1.Embedding{{Values: []float32{1, 0, 0}}},
		Model:      "fake",
		Dim:        3,
		Normalized: true,
	}, nil
}

func (f *fakeEmbedGateway) Health(context.Context, *aiv1.HealthRequest) (*aiv1.HealthResponse, error) {
	if f.health != nil {
		return f.health, nil
	}
	return &aiv1.HealthResponse{Serving: true, Version: "test"}, nil
}

func startFakeEmbedGateway(t *testing.T, gw *fakeEmbedGateway) *EmbeddingGRPCClient {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	aiv1.RegisterModelGatewayServer(srv, gw)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("建立测试连接失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &EmbeddingGRPCClient{
		conn: conn,
		gw:   aiv1.NewModelGatewayClient(conn),
		opts: ai.EmbeddingOptions{Model: "text-embedding-3-small", Dim: 3, Normalize: true},
	}
}

func TestNewEmbeddingRejectsBadAddr(t *testing.T) {
	if _, err := NewEmbedding(ai.EmbeddingOptions{}); !errors.Is(err, ai.ErrGatewayNotConfigured) {
		t.Fatalf("空地址应返回 ErrGatewayNotConfigured，实际 %v", err)
	}
	// 与 chat 客户端共用同一份地址校验：两处各写一份必然有一处先漂移。
	for _, addr := range []string{"sidecar", "sidecar:", ":50051extra"} {
		if _, err := NewEmbedding(ai.EmbeddingOptions{GatewayAddr: addr}); err == nil {
			t.Errorf("非法地址 %q 应当被拒绝", addr)
		}
	}
}

func TestEmbedMapsProtoFields(t *testing.T) {
	gw := &fakeEmbedGateway{
		embedFn: func(_ context.Context, req *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
			out := make([]*aiv1.Embedding, 0, len(req.GetInputs()))
			for range req.GetInputs() {
				out = append(out, &aiv1.Embedding{Values: []float32{0.5, 0.5, 0.5}})
			}
			return &aiv1.EmbedResponse{
				Vectors: out, Model: "text-embedding-3-small", Dim: 3, Normalized: true,
				Usage: &aiv1.Usage{PromptTokens: 5, TotalTokens: 5},
			}, nil
		},
	}
	c := startFakeEmbedGateway(t, gw)

	res, err := c.Embed(context.Background(), ai.EmbeddingRequest{
		Inputs: []string{"甲", "乙"}, Normalize: true, RequestID: "req-9",
	})
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(res.Vectors) != 2 || res.Dim != 3 {
		t.Fatalf("结果形状不对: %d 条 dim=%d", len(res.Vectors), res.Dim)
	}
	if res.Usage.PromptTokens != 5 {
		t.Errorf("usage 必须回传，实际 %+v", res.Usage)
	}

	// 逐字断言报文映射：字段名写错时 gRPC 不会报错，只会让对端收到空值。
	got := gw.lastReq.Load()
	if got.GetModel() != "text-embedding-3-small" {
		t.Errorf("model 未按 Options 默认值回填: %q", got.GetModel())
	}
	if !got.GetNormalize() {
		t.Error("normalize 未透传：对端会返回未归一化向量，而这类偏差只在检索质量上体现")
	}
	if got.GetRequestId() != "req-9" {
		t.Errorf("request_id 未透传: %q", got.GetRequestId())
	}
	if strings.Join(got.GetInputs(), ",") != "甲,乙" {
		t.Errorf("inputs 顺序/内容不对: %v", got.GetInputs())
	}
}

func TestEmbedRejectsEmptyInputsLocally(t *testing.T) {
	gw := &fakeEmbedGateway{}
	c := startFakeEmbedGateway(t, gw)
	if _, err := c.Embed(context.Background(), ai.EmbeddingRequest{}); err == nil {
		t.Fatal("空 inputs 必须在本地被拒绝")
	}
	if gw.lastReq.Load() != nil {
		t.Error("空 inputs 不该产生任何往返：那只会白吃一次超时预算")
	}
}

func TestEmbedRejectsUnnormalizedResponse(t *testing.T) {
	// 请求要归一化、对端说没归一化 —— 必须失败。
	// 静默接受会让更长的文本在余弦检索里系统性占优，而分数看起来正常。
	gw := &fakeEmbedGateway{
		embedFn: func(context.Context, *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
			return &aiv1.EmbedResponse{
				Vectors: []*aiv1.Embedding{{Values: []float32{3, 4, 0}}},
				Dim:     3, Normalized: false,
			}, nil
		},
	}
	c := startFakeEmbedGateway(t, gw)
	_, err := c.Embed(context.Background(), ai.EmbeddingRequest{Inputs: []string{"甲"}, Normalize: true})
	if err == nil || !strings.Contains(err.Error(), "归一化") {
		t.Fatalf("必须因为未归一化而失败，实际 %v", err)
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	gw := &fakeEmbedGateway{
		embedFn: func(context.Context, *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
			return &aiv1.EmbedResponse{
				Vectors: []*aiv1.Embedding{{Values: []float32{1, 2}}},
				Dim:     2, Normalized: true,
			}, nil
		},
	}
	c := startFakeEmbedGateway(t, gw)
	if _, err := c.Embed(context.Background(), ai.EmbeddingRequest{Inputs: []string{"甲"}, Normalize: true}); err == nil {
		t.Fatal("维度与期望不符时必须失败：换模型的检测点就在这")
	}
}

func TestEmbedRejectsNilVectorEntry(t *testing.T) {
	gw := &fakeEmbedGateway{
		embedFn: func(context.Context, *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
			return &aiv1.EmbedResponse{Vectors: []*aiv1.Embedding{nil}, Dim: 3, Normalized: true}, nil
		},
	}
	c := startFakeEmbedGateway(t, gw)
	if _, err := c.Embed(context.Background(), ai.EmbeddingRequest{Inputs: []string{"甲"}, Normalize: true}); err == nil {
		t.Fatal("空向量条目是协议违规，必须失败")
	}
}

// TestEmbedTranslatesGRPCStatus 守住错误翻译：调用点靠它分级。
func TestEmbedTranslatesGRPCStatus(t *testing.T) {
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.DeadlineExceeded, ai.ErrTimeout},
		{codes.Canceled, ai.ErrCanceled},
		{codes.Unavailable, ai.ErrUnavailable},
	}
	for _, c := range cases {
		gw := &fakeEmbedGateway{
			embedFn: func(context.Context, *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
				return nil, status.Error(c.code, "boom")
			},
		}
		cl := startFakeEmbedGateway(t, gw)
		_, err := cl.Embed(context.Background(), ai.EmbeddingRequest{Inputs: []string{"甲"}, Normalize: true})
		if !errors.Is(err, c.want) {
			t.Errorf("%v 应被翻译成 %v，实际 %v", c.code, c.want, err)
		}
	}
}

// TestEmbedHealthReadsEmbeddingAvailability 守住"chat 可用 ≠ 向量可用"。
//
// 两者是独立的凭据与模型，Health 必须把它们分开报告，
// 否则运维会看到 serving=true 却一直拿不到向量。
func TestEmbedHealthReadsEmbeddingAvailability(t *testing.T) {
	gw := &fakeEmbedGateway{
		health: &aiv1.HealthResponse{
			Serving: true, Version: "test",
			Detail: "calls=1 failures=0 tokens=2 cost_usd=0.000000 embedding=[openai-compatible]",
		},
	}
	c := startFakeEmbedGateway(t, gw)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if !h.Serving {
		t.Error("detail 里报告了 embedding provider，serving 应为 true")
	}

	// chat provider 有、embedding 一个都没有 → 必须报不可用。
	gw.health = &aiv1.HealthResponse{
		Serving: true, Version: "test",
		Detail: "calls=1 failures=0 tokens=2 cost_usd=0.000000 embedding=[]",
	}
	h2, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if h2.Serving {
		t.Error("embedding 列表为空时必须报不可用")
	}
}

// TestEmbedRespectsClientTimeout 守住"客户端自己的兜底超时"。
//
// 调用方给的 ctx 更早时以调用方为准；没给时用 Options.Timeout。
// 漏掉这一条会让一次卡住的调用把上游请求一直挂住。
func TestEmbedRespectsClientTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	gw := &fakeEmbedGateway{
		embedFn: func(ctx context.Context, _ *aiv1.EmbedRequest) (*aiv1.EmbedResponse, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, ctx.Err()
		},
	}
	c := startFakeEmbedGateway(t, gw)
	c.opts.Timeout = 30 * time.Millisecond

	start := time.Now()
	_, err := c.Embed(context.Background(), ai.EmbeddingRequest{Inputs: []string{"甲"}, Normalize: true})
	if !errors.Is(err, ai.ErrTimeout) {
		t.Fatalf("应因客户端兜底超时而失败，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("应在兜底超时附近返回，实际用了 %v", elapsed)
	}
}
