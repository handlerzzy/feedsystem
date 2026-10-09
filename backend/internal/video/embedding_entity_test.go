package video

import (
	"math"
	"testing"
)

// 本文件守住向量的**编解码与版本标记**。
//
// 为什么这部分值得单独测：向量是 BLOB，写错了不会报错，
// 只会在检索时表现为"结果时好时坏"。而内容指纹错了更隐蔽——
// 它决定"要不要重算"，错了就是"永远用旧向量"或者"每次都重算"。

func TestEncodeDecodeVectorRoundTrip(t *testing.T) {
	vec := []float32{0, 1, -1, 0.5, 1e-8, 12345.678}
	b, err := EncodeVector(vec)
	if err != nil {
		t.Fatalf("EncodeVector 失败: %v", err)
	}
	if len(b) != len(vec)*4 {
		t.Fatalf("字节数 = %d, want %d", len(b), len(vec)*4)
	}
	if got := EmbeddingDimOf(b); got != len(vec) {
		t.Errorf("EmbeddingDimOf = %d, want %d", got, len(vec))
	}
	back, err := DecodeVector(b)
	if err != nil {
		t.Fatalf("DecodeVector 失败: %v", err)
	}
	for i := range vec {
		if back[i] != vec[i] {
			t.Errorf("第 %d 维不一致: %v vs %v", i, back[i], vec[i])
		}
	}
}

// TestEncodeVectorRejectsNaN 守住"NaN 不得入库"。
//
// NaN 与任何向量的点积都是 NaN，于是排序比较全部返回 false，
// 现象是"召回结果时有时无"，极难定位到是某一条向量坏了。
func TestEncodeVectorRejectsNaN(t *testing.T) {
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if _, err := EncodeVector([]float32{1, bad}); err == nil {
			t.Errorf("%v 必须被拒绝", bad)
		}
	}
}

// TestDecodeVectorRejectsBadLength 守住"不截断、不 panic"。
//
// 截断会让一条坏数据静默地变成一条"看起来正常但短了一截"的向量，
// 而它与其它向量的点积会算在错误的位置上。
func TestDecodeVectorRejectsBadLength(t *testing.T) {
	for _, b := range [][]byte{nil, {}, {1, 2, 3}, make([]byte, 5)} {
		_, err := DecodeVector(b)
		if err == nil {
			t.Fatalf("长度 %d 的字节串必须被拒绝", len(b))
		}
		var enc *VectorEncodingError
		if !asEncodingError(err, &enc) {
			t.Errorf("应返回 VectorEncodingError（调用方据此删掉重算而不是重试查询），实际 %T", err)
		}
	}
}

func asEncodingError(err error, target **VectorEncodingError) bool {
	e, ok := err.(*VectorEncodingError)
	if ok {
		*target = e
	}
	return ok
}

// TestContentHashTracksContent 守住"内容版本"的语义。
func TestContentHashTracksContent(t *testing.T) {
	base := ContentHash("标题", "描述")
	if base == "" {
		t.Fatal("指纹不能为空")
	}
	if ContentHash("标题", "描述") != base {
		t.Error("同样的内容必须得到同样的指纹（否则会反复重算）")
	}
	if ContentHash("标题2", "描述") == base {
		t.Error("标题变了必须换指纹（否则永远用旧向量）")
	}
	if ContentHash("标题", "描述2") == base {
		t.Error("描述变了必须换指纹")
	}
	// 不用分隔符时 ("ab","c") 与 ("a","bc") 会撞成同一个指纹。
	if ContentHash("ab", "c") == ContentHash("a", "bc") {
		t.Error("字段拼接必须用不可能出现在内容里的分隔符，否则会出现假相等")
	}
}

func TestL2Normalize(t *testing.T) {
	v := L2Normalize([]float32{3, 4})
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Errorf("归一化结果 = %v, want [0.6 0.8]", v)
	}
	// 零向量原样返回：除零会得到 NaN，而 NaN 入库会污染整列检索。
	z := L2Normalize([]float32{0, 0})
	if z[0] != 0 || z[1] != 0 {
		t.Errorf("零向量不该被改成 %v", z)
	}
}

func TestCosineSimilarity(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Errorf("同向 = %v, want 1", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Errorf("正交 = %v, want 0", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{-1, 0}); math.Abs(got+1) > 1e-9 {
		t.Errorf("反向 = %v, want -1", got)
	}
	// 未归一化时也必须给出**正确**的余弦，而不是"假设已归一化"的点积：
	// 库里混进少量未归一化向量是完全可能发生的。
	if got := CosineSimilarity([]float32{10, 0}, []float32{0.1, 0}); math.Abs(got-1) > 1e-9 {
		t.Errorf("未归一化同向 = %v, want 1", got)
	}
	// 零向量：任意相似度都会是 NaN，必须显式返回 0。
	if got := CosineSimilarity([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Errorf("零向量 = %v, want 0", got)
	}
	if got := CosineSimilarity(nil, nil); got != 0 {
		t.Errorf("空向量 = %v, want 0", got)
	}
}

func TestEmbeddingTextIsStableAndConsistentWithHash(t *testing.T) {
	if got := EmbeddingText("标题", "描述"); got != "标题。描述" {
		t.Errorf("EmbeddingText = %q", got)
	}
	// 空字段不该留下多余的分隔符（否则同一内容会有两种文本表示）。
	if got := EmbeddingText("  标题  ", ""); got != "标题" {
		t.Errorf("只有标题时 = %q", got)
	}
	if got := EmbeddingText("", " 描述 "); got != "描述" {
		t.Errorf("只有描述时 = %q", got)
	}
	if got := EmbeddingText("  ", "  "); got != "" {
		t.Errorf("全空时 = %q, want 空串", got)
	}
}

func TestEmbeddingTargetNeedsEmbedding(t *testing.T) {
	h := ContentHash("标题", "描述")
	cases := []struct {
		name string
		t    EmbeddingTarget
		want bool
	}{
		{"从来没有向量", EmbeddingTarget{StoredHash: "", CurrentHash: h}, true},
		{"内容变了", EmbeddingTarget{StoredHash: "old", CurrentHash: h}, true},
		{"内容没变", EmbeddingTarget{StoredHash: h, CurrentHash: h}, false},
	}
	for _, c := range cases {
		if got := c.t.NeedsEmbedding(); got != c.want {
			t.Errorf("%s: NeedsEmbedding = %v, want %v", c.name, got, c.want)
		}
	}
}
