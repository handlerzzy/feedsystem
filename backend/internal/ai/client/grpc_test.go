package client

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/ai"
	aiv1 "github.com/handlerzzy/feedsystem/internal/aipb/ai/v1"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// 本文件用一个**进程内的假 ModelGateway** 覆盖真实 gRPC 代码路径。
//
// 为什么不去拉一个真的 sidecar 或真模型：
//  - P0 验收明确要求 CI 不依赖真实模型或 API key；
//  - 单测要能在没有任何外部进程的机器上跑（CI 只跑 go test）。
//
// 用 bufconn 而不是自己起 TCP 端口：不占端口、不受防火墙影响、不会与并行
// 测试抢资源，而且走的仍是完整的 gRPC 编解码与拦截器链路——
// 换成一个"直接调用实现方法"的假实现就测不到报文映射这一层了。

const bufSize = 1024 * 1024

// fakeGateway 是可控的 ModelGateway 服务端。
type fakeGateway struct {
	aiv1.UnimplementedModelGatewayServer

	// lastReq 记录服务端收到的最后一个请求，用于断言报文映射。
	lastReq atomic.Pointer[aiv1.CompleteRequest]

	completeFn func(context.Context, *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error)
	healthFn   func(context.Context, *aiv1.HealthRequest) (*aiv1.HealthResponse, error)
}

func (f *fakeGateway) Complete(ctx context.Context, req *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
	f.lastReq.Store(req)
	if f.completeFn != nil {
		return f.completeFn(ctx, req)
	}
	return &aiv1.CompleteResponse{Text: "ok", Model: req.GetModel()}, nil
}

func (f *fakeGateway) CompleteStream(*aiv1.CompleteRequest, grpc.ServerStreamingServer[aiv1.CompleteChunk]) error {
	// P0 不对流式做业务，但契约已经生成；这里显式返回未实现，
	// 避免"看起来成功了却什么都没流出来"。
	return status.Error(codes.Unimplemented, "P0 未实现")
}

func (f *fakeGateway) Health(ctx context.Context, req *aiv1.HealthRequest) (*aiv1.HealthResponse, error) {
	if f.healthFn != nil {
		return f.healthFn(ctx, req)
	}
	return &aiv1.HealthResponse{Serving: true, Version: "test"}, nil
}

// startFakeGateway 在内存里起一个 gRPC 服务端并返回连上它的客户端。
//
// ClientOptions.GatewayAddr 在这里只用于展示与错误文案；实际连接由
// grpc.NewClient + bufconn dialer 建立。为了让 New() 里的地址校验通过，
// 这里必须给一个形如 host:port 的地址。
func startFakeGateway(t *testing.T, gw *fakeGateway) *GRPCClient {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	aiv1.RegisterModelGatewayServer(srv, gw)
	go func() {
		// Serve 在 srv.Stop 后返回；错误已被 Stop 处理，这里不再上报。
		_ = srv.Serve(lis)
	}()
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

	return &GRPCClient{conn: conn, gw: aiv1.NewModelGatewayClient(conn)}
}

// ---------------------------------------------------------------------------
// 地址校验
// ---------------------------------------------------------------------------

// TestNewRejectsMissingGatewayAddr 守住"开关开了但没给地址"这条最常见的手滑。
//
// 它必须返回 ErrGatewayNotConfigured（而不是去连空地址），
// 装配层据此记一条指向 gateway_addr 的 Warn。
func TestNewRejectsMissingGatewayAddr(t *testing.T) {
	_, err := New(ai.ClientOptions{})
	if !errors.Is(err, ai.ErrGatewayNotConfigured) {
		t.Fatalf("空地址应返回 ErrGatewayNotConfigured，实际: %v", err)
	}
}

// TestNewRejectsMalformedGatewayAddr 覆盖"写漏端口"这类配置错误。
//
// 不校验的话，错误会拖到第一次调用才以"连接失败"的形式出现，
// 而那时运维看到的是 sidecar 的问题，方向就错了。
func TestNewRejectsMalformedGatewayAddr(t *testing.T) {
	for _, addr := range []string{"sidecar", "sidecar:", ":50051extra", "http://sidecar:50051"} {
		if _, err := New(ai.ClientOptions{GatewayAddr: addr}); err == nil {
			t.Errorf("非法地址 %q 应当被拒绝", addr)
		}
	}
}

// TestNewDoesNotDial 守住"启动期不建连"这条性质——P0 的验收
// "sidecar 未启动时 API 与 Worker 正常启动并服务"正是靠它成立的。
//
// 用一个**不可用**的地址构造客户端：如果 New 里做了拨号或探活，这里会失败。
func TestNewDoesNotDial(t *testing.T) {
	c, err := New(ai.ClientOptions{GatewayAddr: "127.0.0.1:1", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("构造客户端不该因为 sidecar 不可达而失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 构造成功还不够：Close 也必须安全（grpc-go 的惰性连接此时还没建立）。
	if err := c.Close(); err != nil {
		t.Errorf("未建连时 Close 应返回 nil: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Complete 的成功路径与报文映射
// ---------------------------------------------------------------------------

// TestCompleteMapsRequestAndResponse 是本文件里最重要的一条：
// 它同时钉住"请求怎么组装"和"响应怎么翻译"。
//
// 这两件事都是最容易悄悄错的地方（比如把 JSON 标志漏掉、把 usage 丢掉），
// 而错了之后在真实链路上表现为"模型返回的东西看起来不太对"，
// 几乎不可能靠端到端测试定位。
func TestCompleteMapsRequestAndResponse(t *testing.T) {
	gw := &fakeGateway{
		completeFn: func(_ context.Context, req *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			return &aiv1.CompleteResponse{
				Text:  `{"tags":["猫"]}`,
				Model: "resolved-" + req.GetModel(),
				Usage: &aiv1.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18, CostUsd: 0.000123},
			}, nil
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	res, err := c.Complete(context.Background(), ai.Request{
		System:      "你是标注员",
		Prompt:      "给这段视频打标签",
		Temperature: 0.2,
		JSON:        true,
		RequestID:   "req-1",
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}

	sent := gw.lastReq.Load()
	if sent == nil {
		t.Fatal("服务端没有收到请求")
	}
	if sent.GetSystem() != "你是标注员" || sent.GetPrompt() != "给这段视频打标签" {
		t.Errorf("system/prompt 映射错了: %+v", sent)
	}
	if sent.GetResponseFormat() != "json" {
		t.Errorf("JSON=true 应映射为 response_format=json，实际 %q", sent.GetResponseFormat())
	}
	if sent.GetRequestId() != "req-1" {
		t.Errorf("request_id 丢失: %q", sent.GetRequestId())
	}
	if sent.GetTemperature() != 0.2 {
		t.Errorf("temperature 丢失: %v", sent.GetTemperature())
	}
	// MaxTokens 未指定时必须填上默认值，不能是 0——0 在多数 provider 那里
	// 意味着"用服务端默认"，但我们的 proto 约定 0 = 未指定，两者语义不同，
	// 这里统一在客户端就落定，避免语义在下游分叉。
	if sent.GetMaxTokens() != int32(ai.DefaultMaxTokens) {
		t.Errorf("未指定 max_tokens 时应填默认值 %d，实际 %d", ai.DefaultMaxTokens, sent.GetMaxTokens())
	}

	if res.Text != `{"tags":["猫"]}` {
		t.Errorf("text 映射错了: %q", res.Text)
	}
	if res.Usage.TotalTokens != 18 || res.Usage.PromptTokens != 11 || res.Usage.CompletionTokens != 7 {
		t.Errorf("usage 映射错了: %+v", res.Usage)
	}
	if res.Usage.CostUSD != 0.000123 {
		t.Errorf("cost_usd 丢失: %v", res.Usage.CostUSD)
	}
}

// TestCompleteUsesConfiguredDefaults 覆盖 Options 里的默认 model / max_tokens。
//
// 这两项是"换模型只改配置、不动代码"的关键，配置写了却不生效会让人
// 以为换模型没效果，然后去改代码——那才是真正的麻烦。
func TestCompleteUsesConfiguredDefaults(t *testing.T) {
	gw := &fakeGateway{}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	c.opts = ai.ClientOptions{Model: "cfg-model", MaxTokens: 64}
	if _, err := c.Complete(context.Background(), ai.Request{Prompt: "x"}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	sent := gw.lastReq.Load()
	if sent.GetModel() != "cfg-model" {
		t.Errorf("model = %q, want cfg-model（配置的默认模型没生效）", sent.GetModel())
	}
	if sent.GetMaxTokens() != 64 {
		t.Errorf("max_tokens = %d, want 64", sent.GetMaxTokens())
	}

	// Request 级别的显式值优先于配置默认值。
	if _, err := c.Complete(context.Background(), ai.Request{Prompt: "x", Model: "per-call", MaxTokens: 8}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	sent = gw.lastReq.Load()
	if sent.GetModel() != "per-call" || sent.GetMaxTokens() != 8 {
		t.Errorf("单次请求的显式值未覆盖配置默认值: %+v", sent)
	}
}

// TestCompleteToleratesMissingUsage 覆盖"老 sidecar 不上报 usage"。
//
// proto 里 usage 是可缺省字段，缺失时按 0 记账即可，绝不能因此报错——
// 把计量缺失升级成功能失败是本末倒置。
func TestCompleteToleratesMissingUsage(t *testing.T) {
	gw := &fakeGateway{
		completeFn: func(context.Context, *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			return &aiv1.CompleteResponse{Text: "ok"}, nil // 没有 Usage
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	res, err := c.Complete(context.Background(), ai.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("usage 缺失不该导致失败: %v", err)
	}
	if res.Usage.TotalTokens != 0 || res.Usage.CostUSD != 0 {
		t.Errorf("usage 缺失时应按 0 记账，实际 %+v", res.Usage)
	}
}

// TestCompleteRejectsEmptyResponse 覆盖协议违规 (nil, nil)。
//
// 必须报错而不是返回空成功：空字符串会被上游当成模型输出写进库。
func TestCompleteRejectsEmptyResponse(t *testing.T) {
	gw := &fakeGateway{
		completeFn: func(context.Context, *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			return nil, nil
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	if _, err := c.Complete(context.Background(), ai.Request{Prompt: "x"}); err == nil {
		t.Fatal("对端返回空响应必须报错，不能当成空结果成功")
	}
}

// ---------------------------------------------------------------------------
// 超时、sidecar 不可达、错误文案
// ---------------------------------------------------------------------------

// TestCompleteHonoursTimeout 守住第 2 节红线 7：模型慢不能拖住调用方。
//
// 断言两件事：错误必须能被 errors.Is(err, ai.ErrTimeout) 判定
// （调用点靠它决定"静默降级"而不是"报故障"），且耗时要落在超时附近，
// 不能远大于它——远大于说明有人在客户端里偷偷重试。
//
// 注意这里断言的是**接口层的哨兵**而不是 context.DeadlineExceeded：
// grpc-go 的超时错误不 wrap context 错误，所以实现层有义务把它翻译过来。
// 这条断言正是"翻译真的发生了"的证明。
func TestCompleteHonoursTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	gw := &fakeGateway{
		completeFn: func(ctx context.Context, _ *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return &aiv1.CompleteResponse{Text: "太晚了"}, nil
			}
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()
	c.opts = ai.ClientOptions{Timeout: 80 * time.Millisecond}

	start := time.Now()
	_, err := c.Complete(context.Background(), ai.Request{Prompt: "慢"})
	elapsed := time.Since(start)

	if !errors.Is(err, ai.ErrTimeout) {
		t.Fatalf("超时必须返回 ai.ErrTimeout，实际: %v", err)
	}
	// 原始 gRPC 错误仍要能被翻出来：翻译不能把排查线索吃掉。
	if !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Errorf("错误里应保留原始状态码，实际: %v", err)
	}
	// 上限放到 1s：给 CI 抖动留余地，但仍然能抓住"重试 3 次"(约 240ms+)之外
	// 的明显放大（比如退避重试到秒级）。
	if elapsed > time.Second {
		t.Errorf("超时耗时 %v，远超设定的 80ms，客户端可能在内部重试", elapsed)
	}
	if ai.StatusFor(err) != "timeout" {
		t.Errorf("StatusFor = %q, want timeout（日志分级依赖它）", ai.StatusFor(err))
	}
}

// TestCallerDeadlineWins 守住"调用方的 deadline 永远优先"。
//
// 调用方只等 30ms 而客户端兜底是 10s：必须以更早的那个为准。
// 反过来（客户端超时覆盖调用方）会让"我这个请求只肯等 30ms"的意图失效。
func TestCallerDeadlineWins(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	gw := &fakeGateway{
		completeFn: func(ctx context.Context, _ *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return &aiv1.CompleteResponse{Text: "太晚了"}, nil
			}
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()
	c.opts = ai.ClientOptions{Timeout: 10 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Complete(ctx, ai.Request{Prompt: "慢"})
	if !errors.Is(err, ai.ErrTimeout) {
		t.Fatalf("应返回 ai.ErrTimeout，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("耗时 %v，调用方的 50ms 截止时间没有生效", elapsed)
	}
}

// TestCompleteOnUnreachableSidecar 模拟 P0 验收里的真实场景：
// sidecar 没起。此时必须快速失败并给出可行动的文案，而不是卡住。
func TestCompleteOnUnreachableSidecar(t *testing.T) {
	c, err := New(ai.ClientOptions{GatewayAddr: "127.0.0.1:1", Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	start := time.Now()
	_, err = c.Complete(context.Background(), ai.Request{Prompt: "x"})
	if err == nil {
		t.Fatal("sidecar 不可达时必须报错")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("失败耗时 %v，不该在不可达的地址上长时间等待", elapsed)
	}
	// 必须能判定为"sidecar 不可达"：这是 AI 最常见的状态，
	// 归进"未知错误"会让运维天天排查一个本该被静默降级的事件。
	if !errors.Is(err, ai.ErrUnavailable) {
		t.Errorf("sidecar 不可达应返回 ai.ErrUnavailable，实际: %v", err)
	}
	if ai.StatusFor(err) != "unavailable" {
		t.Errorf("StatusFor = %q, want unavailable", ai.StatusFor(err))
	}
	// 文案必须指向 sidecar：运维看到 "Unavailable" 不会知道该去看哪个服务。
	if !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("错误文案里应点明 sidecar，实际: %v", err)
	}
}

// TestErrorNeverLeaksKeyMaterial 守住"API key 不进日志/错误/响应"这条纪律。
//
// 做法：让假网关返回一个带 key 形状的 desc，然后断言客户端**不加工**它——
// 也就是说，客户端不能自作主张把请求内容（可能含 key）拼进错误里。
// 这条测试的价值不在于目前会红，而在于给未来"把 req 打进错误方便排查"的
// 改动设一道闸。
func TestErrorNeverLeaksKeyMaterial(t *testing.T) {
	const fakeKey = "sk-test-SHOULD-NOT-SURFACE"
	gw := &fakeGateway{
		completeFn: func(context.Context, *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			return nil, status.Error(codes.Internal, "provider rejected")
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	_, err := c.Complete(context.Background(), ai.Request{
		System: fakeKey,
		Prompt: fakeKey,
	})
	if err == nil {
		t.Fatal("应当报错")
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Errorf("错误信息里泄漏了请求内容（可能含密钥）: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// TestHealthReportsNotServing 覆盖"sidecar 活着但不提供服务"。
//
// 这是真实存在且很常见的状态（总开关关着、或一个 provider key 都没配）。
// Go 侧必须能区分它，否则会先去调 Complete、白等一次超时才降级。
func TestHealthReportsNotServing(t *testing.T) {
	gw := &fakeGateway{
		healthFn: func(context.Context, *aiv1.HealthRequest) (*aiv1.HealthResponse, error) {
			return &aiv1.HealthResponse{
				Serving:   false,
				Version:   "0.1.0",
				Providers: []string{"openai"},
				Detail:    "未配置任何 provider key",
			}, nil
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if h.Serving {
		t.Error("Serving 应为 false")
	}
	if h.Version != "0.1.0" || h.Detail == "" {
		t.Errorf("健康信息缺失: %+v", h)
	}
	if len(h.Providers) != 1 || h.Providers[0] != "openai" {
		t.Errorf("providers 映射错了: %+v", h.Providers)
	}
}

// TestHealthOnUnreachableSidecar 确认探活失败也是"可降级"的普通错误。
func TestHealthOnUnreachableSidecar(t *testing.T) {
	c, err := New(ai.ClientOptions{GatewayAddr: "127.0.0.1:1", Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Health(context.Background()); !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("sidecar 不可达时 Health 应返回 ai.ErrUnavailable，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// TestCloseIsRepeatable 守住装配层 defer Close 的用法。
//
// grpc-go 对已关闭的连接返回 ErrClientConnClosing，把这个升级成错误日志
// 只会制造噪音——关闭一个已经关掉的东西不是故障。
func TestCloseIsRepeatable(t *testing.T) {
	gw := &fakeGateway{}
	c := startFakeGateway(t, gw)

	for i := 0; i < 3; i++ {
		_ = c.Close() // 不断言错误：第一次可能失败，重复关闭必须不 panic
	}
}

// TestNilReceiverIsSafe 覆盖"装配失败后残留的半初始化对象"。
//
// NewClient 保证不返回 nil，但调用方仍可能构造出零值 GRPCClient
// （比如某个测试直接 var c GRPCClient）。零值上调用方法必须报错而不是 panic：
// panic 在 goroutine 里会直接带走整个进程。
func TestNilReceiverIsSafe(t *testing.T) {
	var c *GRPCClient
	if _, err := c.Complete(context.Background(), ai.Request{}); err == nil {
		t.Error("零值客户端的 Complete 应返回错误")
	}
	if _, err := c.Health(context.Background()); err == nil {
		t.Error("零值客户端的 Health 应返回错误")
	}
	if err := c.Close(); err != nil {
		t.Errorf("零值客户端的 Close 应返回 nil: %v", err)
	}
}

// TestCompleteRejectsEmptyTextWithMetadata 覆盖 review 抓到的真实脏数据路径。
//
// sidecar 在"被 max_tokens 截断且内容为空"时曾经返回 text="" 但 model 与 usage
// 都有的响应。旧的判据是"text、model、usage **全都**为空才算空响应"，
// 恰好放过了它——Go 侧于是拿到 (Result{Text:""}, nil)，
// 而那正是注释里承诺"绝不发生"的"空字符串被当成模型输出写进库"。
func TestCompleteRejectsEmptyTextWithMetadata(t *testing.T) {
	for _, text := range []string{"", "   ", "\n\t"} {
		gw := &fakeGateway{
			completeFn: func(context.Context, *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
				return &aiv1.CompleteResponse{
					Text:  text,
					Model: "gpt-4o-mini", // 有 model、有 usage，但内容为空
					Usage: &aiv1.Usage{PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10},
				}, nil
			},
		}
		c := startFakeGateway(t, gw)
		_, err := c.Complete(context.Background(), ai.Request{Prompt: "x"})
		_ = c.Close()

		if err == nil {
			t.Fatalf("空白内容 %q 必须报错，不能当成空结果成功", text)
		}
		if !strings.Contains(err.Error(), "空响应") {
			t.Errorf("错误文案应说明是空响应，实际: %v", err)
		}
	}
}

// TestCanceledIsNotReportedAsTimeout 覆盖 review 抓到的误报：
// 服务端返回 codes.Canceled 时必须翻译成 ai.ErrCanceled（而不是 ErrTimeout）。
//
// 用真实连接构造：客户端 ctx 取消会让 grpc-go 产生 codes.Canceled。
func TestCanceledIsNotReportedAsTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	gw := &fakeGateway{
		completeFn: func(ctx context.Context, _ *aiv1.CompleteRequest) (*aiv1.CompleteResponse, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return &aiv1.CompleteResponse{Text: "太晚了"}, nil
			}
		},
	}
	c := startFakeGateway(t, gw)
	defer func() { _ = c.Close() }()
	c.opts = ai.ClientOptions{Timeout: 10 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := c.Complete(ctx, ai.Request{Prompt: "x"})
	if err == nil {
		t.Fatal("取消后必须报错")
	}
	if errors.Is(err, ai.ErrTimeout) {
		t.Errorf("取消被误报成超时（会把人引向排查模型性能）: %v", err)
	}
	if !errors.Is(err, ai.ErrCanceled) {
		t.Errorf("应返回 ai.ErrCanceled，实际: %v", err)
	}
	if got := ai.StatusFor(err); got != "canceled" {
		t.Errorf("StatusFor = %q, want canceled", got)
	}
}
