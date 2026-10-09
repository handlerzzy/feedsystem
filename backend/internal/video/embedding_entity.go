package video

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

// VideoEmbedding 是一条视频内容的向量（P2 的语义召回输入）。
//
// 幂等键是 **(video_id, model, dim)**，三个字段都是键的一部分，各有明确理由：
//
//   - video_id：一条视频一条向量；
//   - model：换模型意味着向量空间变了，新旧向量**不可混用**。保留旧行而不是
//     覆盖，是为了让"换模型后哪些还没重算"变成一个可查询的事实，
//     而不是靠人记着；
//   - dim：维度变化必须可检测（P2 前置 B3 的验收项）。维度不同的向量
//     放进同一个索引里做点积，结果不是"差一点"而是完全无意义，
//     而且不会报错——只会让召回质量莫名其妙地差。
//
// 内容版本绑定在 content_hash 上：标题或描述变了，hash 就不一致，
// 这一行即视为**过期**，必须重算。没有这一条，改了标题的视频会一直
// 用旧向量被召回，表现为"推荐理由与视频内容对不上"。
type VideoEmbedding struct {
	ID      uint `gorm:"primaryKey" json:"id"`
	VideoID uint `gorm:"not null;uniqueIndex:uk_video_embedding,priority:1" json:"video_id"`
	// Model / Dim 见上面的说明：它们是幂等键的一部分，不是普通属性。
	Model string `gorm:"type:varchar(64);not null;uniqueIndex:uk_video_embedding,priority:2" json:"model"`
	Dim   int    `gorm:"not null;uniqueIndex:uk_video_embedding,priority:3" json:"dim"`
	// ContentHash 是生成这条向量时的标题+描述指纹。
	ContentHash string `gorm:"type:varchar(64);not null" json:"content_hash"`
	// Normalized 记录入库时向量是否已 L2 归一化。
	//
	// 必须记录而不是"约定都归一化"：余弦与点积在归一化与否时不等价，
	// 混着用会让部分结果系统性偏移，而这种偏移看起来像"模型不稳定"。
	Normalized bool `gorm:"not null;default:1" json:"normalized"`
	// Vector 是 float32 小端序列化的向量。
	//
	// 用 BLOB 而不是 JSON 数组：1536 维的 JSON 文本约 20KB，
	// 而二进制只有 6KB；P2 的暴力检索要在内存里扫全表，
	// 解析开销会直接体现在每次 Feed 请求的延迟上。
	//
	// 列名刻意叫 vec 而不是 values：values 是 MySQL 的保留字，
	// 任何一句 `INSERT ... ON DUPLICATE KEY UPDATE values=...` 都要加反引号，
	// 而漏掉反引号是一个只在运行期报 1064 的语法错误（集成测试里抓到过）。
	// 换一个不冲突的名字，比要求每个后来者都记得加引号可靠。
	Vector []byte `gorm:"column:vec;type:mediumblob;not null" json:"-"`
	// CreatedAt / UpdatedAt 由 GORM 维护。UpdatedAt 是"这条向量多久没重算"的依据。
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 显式指定表名：GORM 的复数化对 VideoEmbedding 会给出
// video_embeddings（正确），但写死可以避免将来改结构体名时表名悄悄变化
// （那等于"建新表 + 遗留旧表"，是红线 3 明确禁止的数据孤立）。
func (VideoEmbedding) TableName() string { return "video_embeddings" }

// ContentHash 计算"内容版本"指纹。
//
// 用标题+描述而不是视频 ID：ID 不变不代表内容不变，而向量必须跟着内容走。
// 用 \x00 分隔两个字段：不用分隔符时 ("ab","c") 与 ("a","bc") 会得到
// 同一个 hash，于是"改了标题但描述也改了"的某一种组合不会被判为过期。
func ContentHash(title, description string) string {
	sum := sha256.Sum256([]byte(title + "\x00" + description))
	// 取前 32 个十六进制字符（128 位）足够：这不是防碰撞的安全边界，
	// 而是"内容变了要能看出来"的版本标记。短一点也让它在日志里可读。
	return hex.EncodeToString(sum[:16])
}

// VectorEncodingError 表示入库的字节串无法解成向量。
//
// 单独一个错误类型（而不是笼统的 error）：调用方需要把"这条向量的字节坏了"
// 与"查库失败"分开处置——前者应当删掉重算，后者要重试。
type VectorEncodingError struct {
	Reason string
}

func (e *VectorEncodingError) Error() string { return "video: 向量编码非法: " + e.Reason }

// EncodeVector 把向量序列化成入库用的字节串（float32 小端）。
//
// 拒绝 NaN / Inf：它们入库之后与任何向量的点积都是 NaN，
// 于是**整条召回路**的结果都会被污染（排序比较全都返回 false），
// 而现象是"召回结果时有时无"，极难定位。
func EncodeVector(vec []float32) ([]byte, error) {
	out := make([]byte, len(vec)*4)
	for i, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("video: 第 %d 维不是有限数（NaN/Inf 会污染整列检索）", i)
		}
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out, nil
}

// DecodeVector 把入库的字节串还原成向量。
//
// 长度不是 4 的倍数时返回 VectorEncodingError 而不是 panic 或截断：
// 截断会让一条坏数据静默地变成一条"看起来正常但短了一截"的向量，
// 而它与其它向量的点积会算在错误的位置上。
func DecodeVector(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, &VectorEncodingError{Reason: "字节串为空"}
	}
	if len(b)%4 != 0 {
		return nil, &VectorEncodingError{Reason: fmt.Sprintf("字节长度 %d 不是 4 的倍数", len(b))}
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// EmbeddingDimOf 从字节串长度推出维度，用于不解析即校验。
func EmbeddingDimOf(b []byte) int { return len(b) / 4 }

// L2Normalize 就地归一化一个向量。
//
// 零向量原样返回：除以 0 会得到 NaN，而 NaN 入库会污染整列检索
// （与任何向量的点积都是 NaN）。零向量本身是合法的（模型对空输入
// 可能给出零向量），只是它不携带任何信息。
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

// CosineSimilarity 计算两个向量的余弦相似度。
//
// 它假设两个向量**都已归一化**（那时等价于点积且更快）；
// 未归一化时退回完整公式而不是给出错误答案——P2 的暴力检索会用它，
// 而"少数几条向量没归一化"是完全可能发生的（历史数据、坏实现）。
func CosineSimilarity(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na <= 0 || nb <= 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
