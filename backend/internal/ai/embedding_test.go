package ai

import (
	"context"
	"errors"
	"testing"
)

// 本文件守住 embedding 通道的两条**不可协商**的性质：
//
//  1. 总开关关闭时一次外呼都没有（P0/P1/P2 的共同验收项）；
//  2. 结果的契约校验（条数、维度）不能被绕过。
//
// 第 2 条尤其重要：维度或条数不对的向量入库之后**不会报错**，
// 只会在检索质量上体现出来，而那是最难反查的一类问题。

// TestNoopEmbeddingMakesNoNetworkCall 与 P0 的 TestNoopMakesNoNetworkCall 同构。
//
// 做法：把 ctx 的 Deadline 设成一个**已经过期**的时间。
// 只要实现里真的发起了网络调用，它就必然因为 ctx 已取消而失败；
// 而 Noop 根本不用 ctx，所以照样返回 ErrDisabled。
func TestNoopEmbeddingMakesNoNetworkCall(t *testing.T) {
	noop := NewNoopEmbedding()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消：任何真实调用都会以 context.Canceled 失败

	res, err := noop.Embed(ctx, EmbeddingRequest{Inputs: []string{"测试文本"}, Normalize: true})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("关闭时必须返回 ErrDisabled，实际 %v", err)
	}
	if len(res.Vectors) != 0 {
		t.Errorf("关闭时不该返回任何向量，实际 %d 条", len(res.Vectors))
	}
	if err := noop.Close(); err != nil {
		t.Errorf("Close 必须允许在未连接时调用，实际 %v", err)
	}
	if err := noop.Close(); err != nil {
		t.Errorf("Close 必须允许重复调用，实际 %v", err)
	}
}

// TestNoopEmbeddingHealthIsNotAnError 守住"AI 关了不是故障"。
//
// Health 返回 error 会让每个调用点都要写一次 errors.Is(err, ErrDisabled)，
// 而漏掉一处就会在日志里刷出与业务无关的告警。
func TestNoopEmbeddingHealthIsNotAnError(t *testing.T) {
	h, err := NewNoopEmbedding().Health(context.Background())
	if err != nil {
		t.Fatalf("关闭状态的 Health 不该返回错误，实际 %v", err)
	}
	if h.Serving {
		t.Error("总开关关闭时 serving 必须为 false")
	}
	if h.Detail == "" {
		t.Error("必须说明为什么不可用，否则调用点只能猜")
	}
}

// TestEmbeddingOptionsDefaults 钉住"零值也有安全默认"。
//
// 直接构造 EmbeddingOptions{} 的单测与工具不该出现
// "0 超时 = 立即超时"或"空模型名"这类只在特定调用路径上暴露的坑。
func TestEmbeddingOptionsDefaults(t *testing.T) {
	o := EmbeddingOptions{}
	if o.TimeoutOr() != DefaultEmbeddingTimeout {
		t.Errorf("超时兜底 = %v, want %v", o.TimeoutOr(), DefaultEmbeddingTimeout)
	}
	if o.ModelOr() != DefaultEmbeddingModel {
		t.Errorf("模型兜底 = %q, want %q", o.ModelOr(), DefaultEmbeddingModel)
	}
	// Dim <= 0 表示"不校验"而不是"期望 0 维"，否则会拒绝一切真实响应。
	if o.DimOr() != 0 {
		t.Errorf("未配置维度时应表示不校验，实际 %d", o.DimOr())
	}
	if o2 := (EmbeddingOptions{Dim: 1024}); o2.DimOr() != 1024 {
		t.Errorf("配置了维度时必须原样返回，实际 %d", o2.DimOr())
	}
}

// TestValidateEmbeddingResult 覆盖契约校验的每一条分支。
//
// 这些分支对应的都是"看起来成功、实际有毒"的响应。
func TestValidateEmbeddingResult(t *testing.T) {
	vec := func(n int) []float32 { return make([]float32, n) }

	cases := []struct {
		name    string
		res     EmbeddingResult
		inputs  int
		wantDim int
		wantErr bool
	}{
		{"正常", EmbeddingResult{Vectors: [][]float32{vec(4), vec(4)}, Dim: 4}, 2, 4, false},
		{"条数少了一条", EmbeddingResult{Vectors: [][]float32{vec(4)}, Dim: 4}, 2, 4, true},
		{"维度声明为 0", EmbeddingResult{Vectors: [][]float32{vec(4)}, Dim: 0}, 1, 4, true},
		{"维度与期望不符", EmbeddingResult{Vectors: [][]float32{vec(3)}, Dim: 3}, 1, 4, true},
		{"某一条的维度与声明不符", EmbeddingResult{Vectors: [][]float32{vec(4), vec(3)}, Dim: 4}, 2, 4, true},
		{"不校验维度时放行", EmbeddingResult{Vectors: [][]float32{vec(7)}, Dim: 7}, 1, 0, false},
	}
	for _, c := range cases {
		err := ValidateEmbeddingResult(c.res, c.inputs, c.wantDim)
		if c.wantErr && err == nil {
			t.Errorf("%s：必须报错", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：不该报错，实际 %v", c.name, err)
		}
	}
}
