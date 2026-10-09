package embed

import (
	"math"
	"reflect"
	"testing"
)

// 本文件的核心是一条**跨语言一致性**断言：本包与 sidecar/src/embeddings.ts
// 必须对同一段文本给出逐位相同的向量。
//
// 为什么值得为它写死测试数据：两边的分叉不会报错、不会崩，只会让
// "离线评测说 P2 变好了"与"演示链路看起来还行"指向两个不同的函数——
// 而这两个结论本来是要互相印证的。下面每一条期望值都是从 JS 侧跑出来
// 之后**粘贴**进来的（生成脚本见文件末尾注释），不是手算的。
func TestLexicalUnitsMatchesSidecar(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{
			text: "Go 并发编程：goroutine 与 channel",
			want: []string{"go", "并", "go并", "发", "并发", "编", "发编", "程", "编程", "goroutine", "程goroutine", "与", "goroutine与", "channel", "与channel"},
		},
		{
			text: "猫咪日常：橘猫的一天",
			want: []string{"猫", "咪", "猫咪", "日", "咪日", "常", "日常", "橘", "常橘", "猫", "橘猫", "的", "猫的", "一", "的一", "天", "一天"},
		},
		{
			text: "a1b2 中文 mixed 文本 123",
			want: []string{"a1b2", "中", "a1b2中", "文", "中文", "mixed", "文mixed", "文", "mixed文", "本", "文本", "123", "本123"},
		},
		{
			// 空白与非字母数字（含全角标点）都只是分隔符。
			text: "   ",
			want: []string{},
		},
	}
	for _, tc := range cases {
		got := LexicalUnits(tc.text)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("LexicalUnits(%q)\n got %q\nwant %q", tc.text, got, tc.want)
		}
	}
}

func TestFNV1aMatchesSidecar(t *testing.T) {
	// JS: fnv1a(...) —— 见文件末尾生成脚本。
	cases := []struct {
		text string
		want uint32
	}{
		{"Go 并发编程：goroutine 与 channel", 2249936062},
		{"猫咪日常：橘猫的一天", 4224635923},
		{"分布式系统的一致性", 2009757344},
		{"Golang 高性能服务优化", 426104743},
		{"", 2166136261}, // FNV offset basis
		{"   ", 2268612183},
		{"a1b2 中文 mixed 文本 123", 2043059749},
	}
	for _, tc := range cases {
		if got := FNV1a(tc.text); got != tc.want {
			t.Errorf("FNV1a(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestHashedVectorMatchesSidecar(t *testing.T) {
	// 8 维以方便把期望值写进代码；1536 维下的一致性由"同一套单元 + 同一套
	// 取模"保证，而取模/符号这两件事在这里已经被覆盖（dim 变了会改
	// idx 与 sign 的取值，所以这里必须用真实的 dim=8 跑）。
	cases := []struct {
		text string
		want []float32
	}{
		{"Go 并发编程：goroutine 与 channel", []float32{-0.727607, -0.485071, -0.242536, -0.242536, 0, 0.242536, 0, 0.242536}},
		{"猫咪日常：橘猫的一天", []float32{0, -0.258199, -0.516398, 0.258199, 0.516398, -0.516398, 0, 0.258199}},
		{"分布式系统的一致性", []float32{0, -0.229416, 0, 0.458831, -0.229416, 0, -0.688247, -0.458831}},
		{"Golang 高性能服务优化", []float32{-0.333333, 0.333333, 0, 0.333333, -0.666667, 0, 0.333333, 0.333333}},
		{"", []float32{0, 0, 0, 0, 0, 0, 0, 0}},
		{"a1b2 中文 mixed 文本 123", []float32{-0.603023, 0.301511, 0, -0.603023, 0, -0.301511, 0, 0.301511}},
	}
	const eps = 1e-6
	for _, tc := range cases {
		got := float64s(L2Normalize(HashedVector(tc.text, 8)))
		if len(got) != len(tc.want) {
			t.Fatalf("HashedVector(%q) 维度 %d, want %d", tc.text, len(got), len(tc.want))
		}
		for i := range got {
			if math.Abs(got[i]-float64(tc.want[i])) > eps {
				t.Errorf("HashedVector(%q)[%d] = %.6f, want %.6f\n got %v\nwant %v",
					tc.text, i, got[i], tc.want[i], got, tc.want)
				break
			}
		}
	}
}

func float64s(vec []float32) []float64 {
	out := make([]float64, len(vec))
	for i, v := range vec {
		out[i] = float64(v)
	}
	return out
}

func TestEncodeDimensionAndDeterminism(t *testing.T) {
	if got := len(Encode("任意文本")); got != LexicalDim {
		t.Fatalf("Encode 维度 = %d, want %d", got, LexicalDim)
	}
	a := Encode("同一段文本必须得到同一个向量")
	b := Encode("同一段文本必须得到同一个向量")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Encode 对同一输入返回了不同结果：离线评测的可复现性会失效")
	}
	// 归一化之后模长必须为 1（零向量除外）。
	var sum float64
	for _, v := range L2Normalize(Encode("归一化检查")) {
		sum += float64(v) * float64(v)
	}
	if math.Abs(sum-1) > 1e-5 {
		t.Fatalf("归一化后模长平方 = %v, want 1", sum)
	}
	// 空文本与纯标点都是零向量——调用方必须据此拒绝入库。
	for _, s := range []string{"", "   ", "：，。！"} {
		for _, v := range L2Normalize(Encode(s)) {
			if v != 0 {
				t.Fatalf("%q 应当得到零向量", s)
			}
		}
	}
}

// 期望值生成方式（需要 sidecar 的 node_modules）：
//
//	cd sidecar && cat > /tmp/genvec.mts <<'EOF'
//	import { fnv1a, lexicalUnits, hashedLexicalVector, l2Normalize } from "<abs>/sidecar/src/embeddings.ts";
//	for (const s of [...]) console.log(JSON.stringify({ text: s, fnv: fnv1a(s), units: lexicalUnits(s), vec8: l2Normalize(hashedLexicalVector(s, 8)) }));
//	EOF
//	node_modules/.bin/tsx /tmp/genvec.mts
//
// 改了 sidecar 的切分或哈希规则时，这个测试会先失败——那是**预期**行为：
// 两边的向量空间必须同时迁移，否则线上（sidecar 算的向量）与离线
// （本包算的向量）会落进两个不同的空间。
