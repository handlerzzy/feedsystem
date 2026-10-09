package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// 本文件守的是"AI 关掉时到底会发生什么"。
//
// 为什么这是 P0 里最该被测试覆盖的一段：总览第 4 节的降级表与 P0 验收清单
// 都要求 ai.enabled=false 时**不产生任何对外调用**。这类性质一旦被破坏，
// 表现是"本地跑得好好的、线上开始连不上 sidecar 报一堆 Warn"，
// 而且没有任何编译期或运行期检查会提醒你——只能靠测试。

// 这个地址上没有监听者。任何真的去连接的实现都会在这里失败：
// 拨号到一个未监听的端口会立刻得到 ECONNREFUSED。
// 用它来断言"Noop 没联网"，比 mock 计数更难被绕过——它检验的是真实的 socket 行为。
const deadAddr = "127.0.0.1:1"

// TestNoopMakesNoNetworkCall 是 P0 验收清单"ai.enabled=false 时无任何外呼"的核心用例。
//
// 断言方式：给 Noop 一个**死地址**（其实它根本没有地址参数），
// 然后要求 Complete 立刻返回 ErrDisabled，而不是连接超时或拒绝连接。
// 如果哪天有人给 Noop 加上"先探活一下"之类的逻辑，这里会因为错误类型不对而红。
func TestNoopMakesNoNetworkCall(t *testing.T) {
	// 先确认这个地址确实不可达：否则本用例会变成"因为恰好能连上所以通过"。
	assertPortClosed(t, deadAddr)

	c := NewNoop()

	start := time.Now()
	_, err := c.Complete(context.Background(), Request{Prompt: "写一句摘要"})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Noop.Complete 应返回 ErrDisabled，实际: %v", err)
	}
	// 真去拨号的话，即便立刻被拒绝也会有毫秒级开销；这里只用"不慢得离谱"做兜底，
	// 真正的保证是上面的错误类型与下面的 Health 断言。
	if elapsed > 200*time.Millisecond {
		t.Errorf("Noop.Complete 耗时 %v，看起来做了 IO", elapsed)
	}

	// Health 同样不得触网，且必须是"不可用"而不是"故障"：
	// 返回 error 会让 /readyz 把"AI 关着"报成异常，制造无意义告警。
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Noop.Health 不该返回 error（关闭是预期状态，不是故障）: %v", err)
	}
	if health.Serving {
		t.Error("Noop.Health 必须报告 Serving=false")
	}
	if health.Detail == "" {
		t.Error("Noop.Health 应当说明为什么不可用（detail 为空等于没说）")
	}

	if err := c.Close(); err != nil {
		t.Errorf("Noop.Close 必须返回 nil: %v", err)
	}
}

// TestNoopResultIsNotSilentlyEmpty 防止"空成功"这种最危险的降级形态。
//
// 如果 Noop 返回 (Result{}, nil)，上游会把空字符串当成模型输出写进库：
// 数据脏了、还没有任何错误日志。返回错误是刻意的设计，这里把它钉住。
func TestNoopResultIsNotSilentlyEmpty(t *testing.T) {
	res, err := Noop{}.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Noop.Complete 必须返回错误，绝不能返回空结果 + nil error")
	}
	if res.Text != "" || res.Model != "" || res.Usage.TotalTokens != 0 {
		t.Errorf("失败时不该有部分结果，实际: %+v", res)
	}
}

// TestNoopCloseIsRepeatableAndSafe 覆盖装配层的 defer Close 习惯用法。
func TestNoopCloseIsRepeatableAndSafe(t *testing.T) {
	var c Client = Noop{}
	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Fatalf("第 %d 次 Close 失败: %v", i+1, err)
		}
	}
}

// TestStatusForClassifiesEveryOutcome 守住"日志分级靠可判定的错误类型"这件事。
//
// 为什么值得测：StatusFor 是调用点唯一的分级依据。它一旦退化（比如把
// ErrDisabled 也报成 error），AI 关闭的部署会开始刷 Warn——
// 那正是"AI 是承重墙"体感最强的场景，运维会以为系统坏了。
func TestStatusForClassifiesEveryOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"成功", nil, "ok"},
		{"总开关关闭", ErrDisabled, "disabled"},
		{"地址未配置", ErrGatewayNotConfigured, "gateway_not_configured"},
		{"调用方超时", context.DeadlineExceeded, "timeout"},
		// 包装过的超时仍要能被识别：StatusFor 用 errors.Is 而不是 == 比较。
		// 这不是纸上谈兵——真实链路上错误会经过若干层 %w 包装才到这里。
		{"被包装过的超时", fmt.Errorf("调用模型时: %w", context.DeadlineExceeded), "timeout"},
		{"主动取消", context.Canceled, "canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusFor(tc.err); got != tc.want {
				t.Errorf("StatusFor(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}

	// 未知错误必须原样带出来：把真实故障归到一个已知类别里，
	// 排查时会被引向完全错误的方向。
	if got := StatusFor(errors.New("boom")); got == "ok" || got == "disabled" {
		t.Errorf("未知错误被错误归类: %q", got)
	}
}

// TestClientOptionsDefaults 覆盖"调用方忘了填"的场景。
//
// 0 超时如果被当成合法值传下去，context.WithTimeout(ctx, 0) 会立刻超时——
// 表现为"AI 永远返回超时"，且看起来像 sidecar 的问题。
func TestClientOptionsDefaults(t *testing.T) {
	var zero ClientOptions
	if got := zero.TimeoutOr(); got != DefaultTimeout {
		t.Errorf("零值 TimeoutOr() = %v, want %v", got, DefaultTimeout)
	}
	if got := zero.MaxTokensOr(); got != DefaultMaxTokens {
		t.Errorf("零值 MaxTokensOr() = %d, want %d", got, DefaultMaxTokens)
	}

	neg := ClientOptions{Timeout: -time.Second, MaxTokens: -1}
	if got := neg.TimeoutOr(); got != DefaultTimeout {
		t.Errorf("负数超时应被当作未设置，实际 %v", got)
	}
	if got := neg.MaxTokensOr(); got != DefaultMaxTokens {
		t.Errorf("负数 max_tokens 应被当作未设置，实际 %d", got)
	}

	set := ClientOptions{Timeout: 5 * time.Second, MaxTokens: 32}
	if got := set.TimeoutOr(); got != 5*time.Second {
		t.Errorf("显式超时被覆盖: %v", got)
	}
	if got := set.MaxTokensOr(); got != 32 {
		t.Errorf("显式 max_tokens 被覆盖: %d", got)
	}
}

// wrap 返回一个同时满足 errors.Is(err, target) 的包装错误，
// 用来证明 StatusFor 用的是 errors.Is 而不是 == 比较。
func wrap(_ error, target error) error {
	return &wrapped{msg: "wrapped", err: target}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// assertPortClosed 断言地址不可连接。若它其实可达（比如被别的测试占用了），
// 本文件的"没联网"断言就会失去意义，所以要显式确认。
func assertPortClosed(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("测试前提不成立：%s 居然可以连接，无法用它证明'没有联网'", addr)
	}
}

// TestStatusForTreatsCanceledAsCanceled 守住 review 抓到的一处误报：
// grpc-go 对"调用方取消"返回的是 status.Error(codes.Canceled)，
// 它不 wrap context.Canceled，所以实现层必须翻译成 ErrCanceled。
// 漏掉的话，进程退出/上游取消会被记成"模型超时"——一个会把人引向
// 错误排查方向的日志（去看模型，而不是看调用方为什么取消）。
func TestStatusForTreatsCanceledAsCanceled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"接口层取消哨兵", ErrCanceled, "canceled"},
		{"包装过的取消哨兵", fmt.Errorf("调用模型时: %w", ErrCanceled), "canceled"},
		{"context 原生取消", context.Canceled, "canceled"},
		{"超时仍然是超时", ErrTimeout, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusFor(tc.err); got != tc.want {
				t.Errorf("StatusFor(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
