package aiapp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/config"
)

// 本文件守的是 P0 验收清单里最硬的一条：
//
//	ai.enabled=false 时无任何外呼（有测试断言）
//
// "有测试断言"这四个字要求它必须是**可执行的证明**，而不是代码走查的结论。
// 下面用两种手段：注入一个"一旦被调用就失败"的消费者，以及一个真实不可达的
// 地址（证明没有发生 socket 连接）。

// fakeConsumer 模拟未来 P1/P3 的调用方：它只认识 ai.Client 接口。
//
// 这样写的意义在于：测试覆盖的是**消费方视角**，也就是真实代码的视角。
// 如果哪天有人把开关判断从 aiapp 挪到消费方，这个用例会立刻暴露——
// 因为消费方会拿到一个真的会联网的客户端。
type fakeConsumer struct {
	client ai.Client
}

func (c fakeConsumer) analyze(ctx context.Context) (string, error) {
	res, err := c.client.Complete(ctx, ai.Request{Prompt: "给这个视频打标签"})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// deadAddr 上没有监听者。用它作为 gateway_addr 来证明"没有发生连接"：
// 一旦真的拨号，会得到 ECONNREFUSED，而不是立刻返回 ErrDisabled。
const deadAddr = "127.0.0.1:1"

// TestDisabledProducesNoOutboundCall 是 P0 验收项的直接实现。
func TestDisabledProducesNoOutboundCall(t *testing.T) {
	assertPortClosed(t, deadAddr)

	// 刻意给一份"看起来完全配好了"的配置，只把开关关掉。
	// 这样测的才是开关本身，而不是"因为没配地址所以没联网"。
	cfg := config.AIConfig{
		Enabled:     false,
		GatewayAddr: deadAddr,
		Timeout:     time.Second,
		Model:       "gpt-4o-mini",
		MaxTokens:   64,
	}

	client, reason := NewClient(cfg)
	if client == nil {
		t.Fatal("NewClient 永远不该返回 nil（调用方不必写 nil 判断）")
	}
	// 关闭状态是预期状态：reason 必须为 nil，调用方据此不记任何日志。
	// 若这里返回了错误，AI 关闭的部署会周期性刷 Warn——纯噪音。
	if reason != nil {
		t.Errorf("关闭状态下不该有降级原因，实际: %v", reason)
	}

	start := time.Now()
	_, err := fakeConsumer{client: client}.analyze(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ai.ErrDisabled) {
		t.Fatalf("关闭状态应返回 ErrDisabled（而不是连接失败），实际: %v", err)
	}
	if ai.StatusFor(err) != "disabled" {
		t.Errorf("StatusFor = %q, want disabled", ai.StatusFor(err))
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("耗时 %v，看起来真的去连了 %s", elapsed, deadAddr)
	}
}

// TestDisabledIgnoresGarbageConfig 覆盖"关掉的功能不该因为别的配置项写错而报错"。
//
// 这一条比它看起来重要：运维常见做法是先关掉 AI 再排查问题，
// 如果这时因为残留的半个 gateway_addr 配置让进程起不来，就等于
// "想关掉 AI 反而把服务弄挂了"。
func TestDisabledIgnoresGarbageConfig(t *testing.T) {
	cfg := config.AIConfig{
		Enabled:     false,
		GatewayAddr: "这不是一个地址",
		Timeout:     -time.Second, // 连非法超时也不该有影响
		MaxTokens:   -1,
	}
	client, reason := NewClient(cfg)
	if reason != nil {
		t.Errorf("关闭状态下不该检查任何其他字段，实际返回: %v", reason)
	}
	if _, err := client.Complete(context.Background(), ai.Request{}); !errors.Is(err, ai.ErrDisabled) {
		t.Errorf("应返回 ErrDisabled，实际: %v", err)
	}
}

// TestEnabledWithoutAddrDegradesInsteadOfFailing 覆盖"开关开了但地址没填"。
//
// 期望行为：降级到 Noop，并返回一个**指向 gateway_addr**的原因，
// 让运维在日志里一眼看到该补哪个字段。绝不能返回 nil（调用方 panic），
// 也不能让进程起不来（那是拿承重墙给锦上添花的功能陪葬）。
func TestEnabledWithoutAddrDegradesInsteadOfFailing(t *testing.T) {
	client, reason := NewClient(config.AIConfig{Enabled: true})
	if client == nil {
		t.Fatal("降级路径也必须返回可用对象，不能是 nil")
	}
	if !errors.Is(reason, ai.ErrGatewayNotConfigured) {
		t.Fatalf("原因应指向 gateway_addr 未配置，实际: %v", reason)
	}
	if _, err := client.Complete(context.Background(), ai.Request{}); !errors.Is(err, ai.ErrDisabled) {
		t.Errorf("降级后的客户端应恒定返回 ErrDisabled，实际: %v", err)
	}
}

// TestEnabledWithBadAddrDegradesInsteadOfFailing 覆盖地址写错的场景。
//
// 这类错误很常见（"sidecar" 忘了端口、"http://" 前缀），必须在启动期
// 就被降级并报出原因，而不是等到第一次调用。
func TestEnabledWithBadAddrDegradesInsteadOfFailing(t *testing.T) {
	for _, addr := range []string{"sidecar", "http://sidecar:50051", "sidecar:abc"} {
		t.Run(addr, func(t *testing.T) {
			client, reason := NewClient(config.AIConfig{Enabled: true, GatewayAddr: addr})
			if reason == nil {
				t.Fatalf("非法地址 %q 应返回降级原因", addr)
			}
			if client == nil {
				t.Fatal("降级路径也必须返回可用对象")
			}
			if _, err := client.Complete(context.Background(), ai.Request{}); !errors.Is(err, ai.ErrDisabled) {
				t.Errorf("降级后的客户端应恒定返回 ErrDisabled，实际: %v", err)
			}
		})
	}
}

// TestEnabledWithAddrStartsWithoutDialing 守住 P0 验收项
// "sidecar 未启动时，API 与 Worker 正常启动并服务"。
//
// 装配一个指向死地址的真实客户端：装配必须成功，且只能到调用时才失败。
// 如果哪天有人在装配里加了"先探活"或"启动即建连"，这里会红。
func TestEnabledWithAddrStartsWithoutDialing(t *testing.T) {
	assertPortClosed(t, deadAddr)

	client, reason := NewClient(config.AIConfig{
		Enabled:     true,
		GatewayAddr: deadAddr,
		Timeout:     300 * time.Millisecond,
	})
	if reason != nil {
		t.Fatalf("地址合法时不该降级，实际: %v", reason)
	}
	// 证明真的走了 gRPC 实现，而不是意外退化成 Noop。
	//
	// 用类型名而不是具体类型断言：装配层的测试不该依赖实现包的内部类型名，
	// 否则"换实现"就会变成"改测试"。这里要守的性质只是"不是 Noop"。
	if _, isNoop := client.(ai.Noop); isNoop {
		t.Fatal("地址合法且开关打开时，不该退化成 Noop")
	}
	if got := fmt.Sprintf("%T", client); !strings.Contains(got, "GRPCClient") {
		t.Errorf("期望拿到 gRPC 客户端，实际 %s", got)
	}

	// 关掉时必须干净：装配期间没有建连，Close 不该报错。
	if err := client.Close(); err != nil {
		t.Errorf("未建连时 Close 应返回 nil: %v", err)
	}
}

// TestOptionsTranslatesConfigDefaults 覆盖配置 -> 实现参数的翻译。
//
// 这是最容易悄悄回归的一段：timeout 写成 0 时如果原样传下去，
// context.WithTimeout(ctx, 0) 会立刻超时，表现为"AI 永远超时"，
// 而排查方向会被引向 sidecar。
func TestOptionsTranslatesConfigDefaults(t *testing.T) {
	t.Run("零值配置填默认", func(t *testing.T) {
		opts := Options(config.AIConfig{})
		if opts.Timeout != ai.DefaultTimeout {
			t.Errorf("timeout = %v, want %v", opts.Timeout, ai.DefaultTimeout)
		}
		if opts.MaxTokens != 0 {
			// MaxTokens 走的是 MaxTokensOr() 取值兜底，这里保留 0 是刻意的：
			// 0 表示"未指定"，由实现层决定用什么默认值。
			t.Errorf("max_tokens 应保持未指定（0），实际 %d", opts.MaxTokens)
		}
		if got := opts.MaxTokensOr(); got != ai.DefaultMaxTokens {
			t.Errorf("MaxTokensOr() = %d, want %d", got, ai.DefaultMaxTokens)
		}
	})

	t.Run("显式值原样透传", func(t *testing.T) {
		opts := Options(config.AIConfig{
			GatewayAddr: "sidecar:50051",
			Timeout:     7 * time.Second,
			Model:       "m1",
			MaxTokens:   128,
			DailyBudget: 3.5,
		})
		if opts.GatewayAddr != "sidecar:50051" || opts.Timeout != 7*time.Second ||
			opts.Model != "m1" || opts.MaxTokens != 128 || opts.DailyBudget != 3.5 {
			t.Errorf("翻译丢字段: %+v", opts)
		}
	})

	t.Run("负数超时视为未设置", func(t *testing.T) {
		opts := Options(config.AIConfig{Timeout: -time.Second})
		if opts.Timeout != ai.DefaultTimeout {
			t.Errorf("负数超时应被替换为默认值，实际 %v", opts.Timeout)
		}
	})
}

// assertPortClosed 断言地址不可连接：否则"没有联网"的断言会失去意义。
func assertPortClosed(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("测试前提不成立：%s 居然可以连接", addr)
	}
}
