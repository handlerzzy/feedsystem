package video

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"go.uber.org/zap"
)

// 本文件实现 P2 §4.1「embedding 生产与存储」。
//
// 三条硬要求，缺一条这条链路就不可靠：
//
//  1. **向量与内容版本绑定**：标题/描述变了必须重算。判定靠 content_hash
//     （见 ListEmbeddingTargets / NeedsEmbedding），而不是靠人记得重跑。
//  2. **必须可缺省**：AI 关闭、sidecar 不在、表为空 —— 任何一种情况下
//     Feed 都必须照常工作。这里所有错误都只**返回**，不 panic、不阻塞调用方；
//     调用方（worker 的后台循环）只记日志。
//  3. **不重试到天亮**：一次失败就翻页下一批，失败计数由调用方按轮次观察。
//     单条向量失败不该让整批卡住——那会把"少数几条算不出来"放大成
//     "整个向量池停止更新"。

// noEmbeddingError 表示"这条视频现在不需要 / 不能有向量"，不是故障。
//
// 为什么需要它：worker 与后台循环要区分"失败（要看、要计数）"与
// "跳过（正常）"。把正常跳过计入失败会让回填的失败率看起来永远很高，
// 于是真正的失败被淹没。
type noEmbeddingError struct {
	reason string
}

func (e *noEmbeddingError) Error() string { return "video: 跳过向量化: " + e.reason }

// IsNoEmbedding 报告一个错误是否属于"不需要向量"这一类。
//
// 与 IsNoAnalysis 同样的形状：调用方不需要 import 本包的错误类型细节，
// 只需要一个判定函数。
func IsNoEmbedding(err error) bool {
	var target *noEmbeddingError
	return errors.As(err, &target)
}

// EmbeddingBackfillStore 是回填所需的持久化能力（接口定义在消费方，本包）。
type EmbeddingBackfillStore interface {
	ListEmbeddingTargets(ctx context.Context, model string, dim int, limit int) ([]EmbeddingTarget, error)
	UpsertEmbeddings(ctx context.Context, rows []VideoEmbedding) error
}

// 编译期断言：GORM 实现必须始终满足这个接口。
var _ EmbeddingBackfillStore = (*VideoRepository)(nil)

// BackfillOptions 是回填器的参数。零值不可用，请用 DefaultBackfillOptions。
type BackfillOptions struct {
	// Model 是**请求里**用的模型名，也是"与哪一组已入库向量比对"的依据。
	//
	// 注意：入库时用的模型名取自**响应**（见 BackfillOnce），因为
	// "向量是谁算的"只有提供方能回答。离线演示时响应回的是
	// "faux-lexical"，而这里的配置值是 "text-embedding-3-small"；
	// 按配置值入库会让库里留下"标称真模型、实际是词法哈希"的向量，
	// 之后切到真 provider 时会读到这批假向量。
	//
	// 因此本字段的作用是：**探测哪些视频在目标空间里还没有向量**。
	Model string
	Dim   int
	// Normalize 要求端点返回归一化向量（nil = 用默认值 true）。
	//
	// 用指针而不是 bool：与 internal/config 的 EmbeddingNormalize 同一个理由——
	// 零值 false 与"显式要求不归一化"无法区分，而这两个语义的期望行为完全相反
	// （见 ai.EmbeddingRequest 的注释）。用 *bool 之后，
	// "忘了写"与"明确要 false"是两件不同的事。
	Normalize *bool
	// BatchLimit 是一次回填最多检查多少条视频。
	//
	// 它是**扫描上限**而不是"总量上限"：视频总量超过它时，靠周期性反复调用
	// 逐批推进（每次都是从 id 最小的开始扫）。因此它不需要等于视频总量，
	// 但也不能太小——太小的话向量池的增长速度会跟不上发布速度。
	BatchLimit int
	// EmbedBatch 是一次送给端点的文本条数（上限 64，见 proto 注释）。
	EmbedBatch int
	// Timeout 是整次回填的兜底超时（不是单次调用的超时，那个在客户端里）。
	Timeout time.Duration
	// SkipZero 让"计算出零向量"这件事**不**入库。
	//
	// 零向量的余弦与任何向量都是 0，看起来像"这条内容没有语义"，
	// 而真实原因往往是"标题和描述都是空的"。默认 true。
	// 允许关掉它只有一个用途：测试要构造"写入确实发生了"的场景。
	SkipZero bool
}

// DefaultBackfillOptions 返回生产环境的默认参数。
func DefaultBackfillOptions() BackfillOptions {
	return BackfillOptions{
		Model:      ai.DefaultEmbeddingModel,
		Dim:        ai.DefaultEmbeddingDim,
		Normalize:  boolPtr(true),
		BatchLimit: 500,
		EmbedBatch: 64,
		Timeout:    30 * time.Second,
		SkipZero:   true,
	}
}

// BackfillStats 是一次回填的结果摘要。
//
// 它存在的理由是**可观测**：回填是一条后台链路，没有这些数字就只能靠
// "Feed 里有没有语义召回"来反推它是否在工作——而那要等到出问题之后。
type BackfillStats struct {
	// Scanned 是本次检查过的视频数（含已是最新、无需重算的）。
	Scanned int
	// Embedded 是真正写入（或更新）了向量的视频数。
	Embedded int
	// Stale 是内容指纹与已入库向量不一致的视频数（含从未有过向量的）。
	Stale int
	// Skipped 是因为没有可向量化的文本、或算出零向量而跳过的条数。
	Skipped int
	// Failed 是调用端点或写库失败的条数。
	Failed int
	// Pending 是检查完之后仍然需要向量化的条数（>0 表示还没扫完）。
	Pending int
}

// Progress 报告本次回填是否有任何变化，供后台循环决定要不要打日志。
func (s BackfillStats) Progress() bool {
	return s.Embedded > 0 || s.Failed > 0 || s.Skipped > 0
}

// EmbeddingBackfiller 把"视频的标题+描述"变成 video_embeddings 里的一行。
//
// 它不持有 *gorm.DB、不持有 config，只认识两个窄接口：
// 因此单测可以完整覆盖"内容变更 -> 重算"这条最容易错的逻辑，
// 而不需要 MySQL、不需要 sidecar。
type EmbeddingBackfiller struct {
	store  EmbeddingBackfillStore
	client ai.EmbeddingClient
	opts   BackfillOptions
	// reqSeq 只用来生成可读的 request_id（把 Go 日志与 sidecar 日志对上）。
	reqSeq atomic.Uint64
}

// NewEmbeddingBackfiller 构造回填器。client 为 nil 时退化为 Noop 客户端，
// 于是"AI 关闭"的部署不需要在调用点判断 nil。
func NewEmbeddingBackfiller(store EmbeddingBackfillStore, client ai.EmbeddingClient, opts BackfillOptions) *EmbeddingBackfiller {
	opts = opts.withDefaults()
	if client == nil {
		client = ai.NewNoopEmbedding()
	}
	return &EmbeddingBackfiller{store: store, client: client, opts: opts}
}

// withDefaults 把零值参数补成安全值。
//
// 做成"取值时兜底"而不是"构造时报错"：直接构造 BackfillOptions{} 的测试
// 或未来的调用方也应该拿到一个可用（而不是会 panic、会打爆端点）的配置。
func (o BackfillOptions) withDefaults() BackfillOptions {
	d := DefaultBackfillOptions()
	if strings.TrimSpace(o.Model) == "" {
		o.Model = d.Model
	}
	if o.Dim <= 0 {
		o.Dim = d.Dim
	}
	if o.BatchLimit <= 0 {
		o.BatchLimit = d.BatchLimit
	}
	if o.EmbedBatch <= 0 {
		o.EmbedBatch = d.EmbedBatch
	}
	// 硬上限对齐 proto 契约（一次最多 64 条），超限会被 sidecar 拒绝整批，
	// 表现为"回填全失败"，而原因只是一个没人记得的常量。
	if o.EmbedBatch > MaxEmbedBatch {
		o.EmbedBatch = MaxEmbedBatch
	}
	if o.Timeout <= 0 {
		o.Timeout = d.Timeout
	}
	return o
}

// NormalizeOr 返回生效的归一化要求，缺省为 true。
func (o BackfillOptions) NormalizeOr() bool {
	if o.Normalize == nil {
		return true
	}
	return *o.Normalize
}

// boolPtr 返回指向 b 的指针（局部辅助函数，避免引入第三方库）。
func boolPtr(b bool) *bool { return &b }

// MaxEmbedBatch 是一次 embedding 请求允许的最大文本条数，与 proto 注释一致。
const MaxEmbedBatch = 64

// BackfillOnce 跑一轮回填，返回结果摘要。
//
// 它**不返回错误**：一轮回填里某几条失败不该让整个循环退出，
// 也不该让调用方以为"什么都没做"。失败信息通过 stats.Failed 与日志暴露。
// 唯一的例外是"连候选都查不出来"——那时返回错误，因为它说明数据库不可用，
// 而这与"这次没什么可做"是完全不同的状态。
func (b *EmbeddingBackfiller) BackfillOnce(ctx context.Context) (BackfillStats, error) {
	var stats BackfillStats
	if b == nil || b.store == nil {
		return stats, errors.New("video: 回填器未初始化（store 为空）")
	}

	ctx, cancel := context.WithTimeout(ctx, b.opts.Timeout)
	defer cancel()

	targets, err := b.store.ListEmbeddingTargets(ctx, b.opts.Model, b.opts.Dim, b.opts.BatchLimit)
	if err != nil {
		return stats, fmt.Errorf("video: 列出待向量化视频失败: %w", err)
	}
	stats.Scanned = len(targets)

	// 只保留真正需要重算的：从来没有向量，或内容指纹变了。
	//
	// 这一步刻意放在 Go 里而不是 SQL 里（见 ListEmbeddingTargets 的注释）：
	// 指纹算法是 Go 侧的 ContentHash，SQL 里再写一份表达式必然会漂移，
	// 而漂移的后果是"该重算的没重算"——完全静默。
	stale := make([]EmbeddingTarget, 0, len(targets))
	for _, t := range targets {
		if t.NeedsEmbedding() {
			stale = append(stale, t)
		}
	}
	stats.Stale = len(stale)

	for start := 0; start < len(stale); start += b.opts.EmbedBatch {
		end := start + b.opts.EmbedBatch
		if end > len(stale) {
			end = len(stale)
		}
		batch := stale[start:end]

		texts := make([]string, 0, len(batch))
		kept := make([]EmbeddingTarget, 0, len(batch))
		for _, t := range batch {
			text := strings.TrimSpace(EmbeddingText(t.Title, t.Description))
			if text == "" {
				// 没有文本就没有向量。这不是失败：一条只有标题为空、
				// 描述也为空的视频（历史数据、或被清空过）本来就没有语义可言。
				stats.Skipped++
				continue
			}
			texts = append(texts, text)
			kept = append(kept, t)
		}
		if len(texts) == 0 {
			continue
		}

		res, err := b.client.Embed(ctx, ai.EmbeddingRequest{
			Inputs:    texts,
			Model:     b.opts.Model,
			Normalize: b.opts.NormalizeOr(),
			RequestID: b.nextRequestID(),
		})
		if err != nil {
			// 整批失败：不重试（重试由周期循环的下一次调用兜底）。
			// 这里刻意不把上下文取消当成"失败"之外的信号：调用方
			// （后台循环）在进程退出时同样会看到它，日志里区分得出来。
			stats.Failed += len(texts)
			b.logWarn(ctx, "向量化失败，本批跳过（下一轮会重试）", zap.Int("batch", len(texts)), zap.Error(err))
			continue
		}
		if err := ai.ValidateEmbeddingResult(res, len(texts), b.opts.Dim); err != nil {
			// 契约不满足（条数错位、维度不符）：**整批丢弃**。
			// 按能对上的部分入库会造成文本与向量永久错配，
			// 而那种错配在检索结果里只表现为"不够准"，几乎无法反查。
			stats.Failed += len(texts)
			b.logWarn(ctx, "向量结果不符合契约，整批丢弃", zap.Int("batch", len(texts)), zap.Error(err))
			continue
		}

		// 入库用的模型名取自响应：只有提供方知道这批向量是谁算的。
		// 为空时退回请求里的名字（老版本 sidecar 不回填 model 的情况）。
		storedModel := strings.TrimSpace(res.Model)
		if storedModel == "" {
			storedModel = b.opts.Model
		}
		if storedModel != b.opts.Model {
			// 这不是错误，但必须让人看得见：库里的空间名与配置名不同，
			// 而读路径按"实际空间"取（见 ListEmbeddingModels），
			// 因此功能正常——但读者会疑惑"为什么库里是 faux-lexical"。
			logging.Ctx(ctx).Info("向量入库使用提供方回报的模型名（与配置不同）",
				zap.String("configured_model", b.opts.Model),
				zap.String("provider_model", storedModel))
		}

		rows := make([]VideoEmbedding, 0, len(kept))
		for i, t := range kept {
			vec := res.Vectors[i]
			if b.opts.SkipZero && isZeroVector(vec) {
				// 零向量不入库：它与任何向量的余弦都是 0，会让这条视频
				// 永久占据召回池里的一个位置却永远排不上来。
				stats.Skipped++
				continue
			}
			blob, err := EncodeVector(vec)
			if err != nil {
				stats.Failed++
				b.logWarn(ctx, "向量编码失败，跳过该条", zap.Uint("video_id", t.VideoID), zap.Error(err))
				continue
			}
			rows = append(rows, VideoEmbedding{
				VideoID:     t.VideoID,
				Model:       storedModel,
				Dim:         len(vec),
				ContentHash: t.CurrentHash,
				Normalized:  b.opts.NormalizeOr() && res.Normalized,
				Vector:      blob,
			})
		}
		if len(rows) == 0 {
			continue
		}
		if err := b.store.UpsertEmbeddings(ctx, rows); err != nil {
			stats.Failed += len(rows)
			b.logWarn(ctx, "向量入库失败", zap.Int("rows", len(rows)), zap.Error(err))
			continue
		}
		stats.Embedded += len(rows)
	}

	// 仍然需要向量化的条数：本次扫描范围内没做完的部分。
	stats.Pending = stats.Stale - stats.Embedded - stats.Skipped
	if stats.Pending < 0 {
		stats.Pending = 0
	}
	return stats, nil
}

func (b *EmbeddingBackfiller) nextRequestID() string {
	return fmt.Sprintf("embed-backfill-%d", b.reqSeq.Add(1))
}

// logWarn 统一回填链路的日志级别与字段。
//
// 一律 Warn 而不是 Error：回填是可重入的后台任务，失败会在下一轮重试；
// 用 Error 会让"sidecar 暂时不在"与"数据写坏了"在告警里同样刺眼。
func (b *EmbeddingBackfiller) logWarn(ctx context.Context, msg string, fields ...zap.Field) {
	logging.Ctx(ctx).Warn(msg, append(fields, zap.String("model", b.opts.Model), zap.Int("dim", b.opts.Dim))...)
}

func isZeroVector(vec []float32) bool {
	for _, v := range vec {
		if v != 0 {
			return false
		}
	}
	return true
}
