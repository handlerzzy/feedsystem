// Package embed 提供**离线、确定性、零依赖**的向量编码器。
//
// 为什么需要它（P2 §4.1 的"离线可复现"要求）：
// P2 的效果结论来自离线评测（Recall@K / NDCG），而真实 embedding 端点需要
// API key、需要网络、并且**同一个输入在不同时间可能给出不同的向量**
// （provider 换代、量化策略调整）。用真实端点做评测，"半年后重跑基线"
// 这件事就不成立，而基线数字的全部价值就在于可复现。
//
// 本包的做法：把 sidecar 在 `AI_PROVIDER=faux`（离线演示）下使用的
// 词法向量编码器**在 Go 里实现一份**，两边逐位一致（由 lexical_test.go
// 里的固定向量锁住）。于是：
//
//   - 端到端链路（发布 -> 入库 -> 召回 -> 融合 -> 展示）可以在没有 key
//     的环境里被真实验证；
//   - 离线评测用的向量与演示链路是**同一个函数**，评测结论不是另一套代码
//     的结论；
//   - 生产环境换真实模型时，只需改配置（AI_EMBEDDING_MODEL / _DIM），
//     消费方接口与存储格式一个字节都不用动。
//
// 纪律：本包**不做 HTTP、不读环境变量、不 import 项目其它包**。
// 它只是"文本 -> []float32"的一个纯函数集合，任何地方都能安全调用。
package embed

import (
	"math"
	"regexp"
	"unicode"
)

// LexicalDim 是词法向量的默认维度。
//
// 取 1536 是为了与 ai.DefaultEmbeddingDim（真实模型的维度）**对齐**：
// 离线演示与真实模型共用同一份表结构、同一份校验、同一段召回代码，
// 于是"演示能跑通、上线换模型就崩"这类问题在演示阶段就会暴露。
const LexicalDim = 1536

// LexicalModel 是词法向量在 video_embeddings.model 列里的名字。
//
// 与真实模型名分开是必须的：向量表按 (video_id, model, dim) 做幂等键，
// 用不同的名字才能让"哪些向量是真实模型算的、哪些是演示算的"变成一个
// 可查询的事实，而不是靠人记着——混在同一个 model 名下会让一次模型切换
// 变成静默的向量空间污染（新旧向量不可混用，却谁也看不出来）。
const LexicalModel = "faux-lexical"

// Encode 返回一段文本的词法向量（带符号的特征哈希 + L2 归一化）。
//
// 返回值恒为 LexicalDim 维；文本为空（或全是标点）时返回零向量——
// 调用方应当**拒绝**入库零向量：它与任何东西的余弦都是 0，
// 看起来像"这条内容没有语义"，而实际原因是"输入的文本是空的"。
// 这个判断留在调用方是因为本包不认识 ai 包的哨兵，也不想认识。
func Encode(text string) []float32 {
	return HashedVector(text, LexicalDim)
}

// HashedVector 把文本编码成 dim 维的带符号哈希向量（未归一化）。
//
// 为什么用"带符号"的哈希：不做符号时任何两个词的贡献都是正的，
// 任意两段文本的余弦都会明显偏高（所有文本都"有点像"），
// 召回结果会退化成"谁更长谁占优"。符号让不相关的词相互抵消。
//
// 与 sidecar/src/embeddings.ts 的 hashedLexicalVector 逐位一致：
// 同样的 lexicalUnits 切分、同样的 FNV-1a、同样的取模与符号位。
// 两边不一致的后果不会报错，只会让"离线评测说变好了"与"演示看起来还行"
// 指向两个不同的函数——所以 lexical_test.go 里有跨语言的固定向量断言。
func HashedVector(text string, dim int) []float32 {
	if dim <= 0 {
		return nil
	}
	vec := make([]float32, dim)
	for _, token := range LexicalUnits(text) {
		h := FNV1a(token)
		idx := int(h % uint32(dim))
		// 第二个哈希位决定符号（见上面的说明）。
		sign := float32(1)
		if (h/uint32(dim))&1 == 1 {
			sign = -1
		}
		vec[idx] += sign
	}
	return vec
}

// L2Normalize 就地归一化并返回同一个切片。
//
// 零向量原样返回（除零会得到 NaN，而 NaN 入库会污染整列检索）。
func L2Normalize(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum <= 0 {
		return vec
	}
	inv := 1 / math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * inv)
	}
	return vec
}

// hans 判定一个 rune 是否属于汉字。
//
// 用显式区间而不是 regexp：这个判定在热路径上（每段文本、每个字符一次），
// 而离线的 20 个话题词表里汉字占比极高。区间与 JS 侧
// `/\p{Script=Han}/u` 的覆盖面在**本项目会遇到的字符**上一致（CJK 统一表意
// 文字 + 扩展 A）。扩展 B 及以上的字符在 Go 里是单个 rune、在 JS 里是
// 代理对：那种字符上两边会切出不同的单元，但聚类结果仍然确定——
// 受影响的是"两个代理对算不算相邻 bigram"，而不是"会不会崩"。
func hans(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意文字
		(r >= 0x3400 && r <= 0x4DBF) || // 扩展 A
		(r >= 0xF900 && r <= 0xFAFF) // 兼容表意文字
}

// wordRE 匹配 JS 侧 `/\p{L}\p{N}/u` 的**等价集合**：字母或数字。
//
// 这里用 regexp 而不是 unicode.IsLetter/IsDigit：Go 的 unicode.IsDigit
// 只认 Nd（十进制数字），而 \p{N} 还包含 Nl/No。差异只影响极少数符号
// （如 Ⅷ、²），但它们会成为"同一段文本在两边切出不同单元"的来源，
// 而两边一致正是本包存在的理由。
var wordRE = regexp.MustCompile(`^[\p{L}\p{N}]$`)

// LexicalUnits 把一段文本切成词法单元。
//
// 规则与 sidecar 完全一致：
//   - 汉字按单字成单元；
//   - 其它字母/数字按连续串小写成一个单元；
//   - 其余字符是分隔符；
//   - 最后再补上相邻单元的 bigram。
//
// 为什么中文必须补 bigram：单字召回高但区分度低（"的""了"到处都是），
// bigram 才承担主要区分度。英文不加 bigram——英文的词本身已经是语义单元，
// 再加字符 bigram 只会放大拼写噪声。
func LexicalUnits(text string) []string {
	units := make([]string, 0, 32)
	var word []rune
	flush := func() {
		if len(word) > 0 {
			units = append(units, string(word))
			word = word[:0]
		}
	}
	for _, r := range text {
		switch {
		case hans(r):
			flush()
			units = append(units, string(r))
		case wordRE.MatchString(string(r)):
			word = append(word, toLowerRune(r))
		default:
			flush()
		}
	}
	flush()

	tokens := make([]string, 0, len(units)*2)
	for i, u := range units {
		tokens = append(tokens, u)
		if i > 0 {
			tokens = append(tokens, units[i-1]+u)
		}
	}
	return tokens
}

// toLowerRune 小写化单个 rune。
//
// 单独一个函数只为了说明一件事：JS 的 toLowerCase 与 Go 的 unicode.ToLower
// 在极少数字符上不一致（如 'İ'），而那些字符不会出现在本项目的内容里。
// 真出现时表现为"两边切出不同单元"，不会崩、不会错位。
func toLowerRune(r rune) rune { return unicode.ToLower(r) }

// FNV1a 是 32 位 FNV-1a 哈希，与 sidecar 的 fnv1a 逐位一致。
//
// 按**UTF-16 码元**迭代而不是按 UTF-8 字节：JS 的 charCodeAt 给的是码元，
// 两者对基本平面字符（本项目全部内容）结果相同，但写成字节迭代会让
// 含代理对的文本与 JS 侧分叉。这里显式按 rune 处理基本平面、
// 按 UTF-16 语义处理增补平面，与 JS 对齐。
func FNV1a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for _, r := range s {
		if r <= 0xFFFF {
			h ^= uint32(r)
			h *= prime32
			continue
		}
		// 增补平面：JS 会看到两个代理码元，因此这里也要按码元喂两次，
		// 否则同一个 emoji 在两边得到不同的哈希。
		r -= 0x10000
		hi := 0xD800 + (r >> 10)
		lo := 0xDC00 + (r & 0x3FF)
		h ^= uint32(hi)
		h *= prime32
		h ^= uint32(lo)
		h *= prime32
	}
	return h
}
