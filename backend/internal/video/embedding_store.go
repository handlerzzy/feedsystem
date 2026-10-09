package video

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EmbeddingStore 是向量落库与读取所需的持久化能力。
//
// 与 VideoAnalysisStore 同样的定位：接口留窄，方便仓储层被单测直接构造与断言。
// *VideoRepository 隐式实现它。
//
// 注意本接口**不做检索**：P2 前置 B4 选定的是"应用内暴力检索"，
// 也就是说召回是"把候选向量读进内存再算余弦"。把检索放进仓储层
// 会把一个纯计算的事情耦合到数据库连接上，而它恰恰是最需要被单测覆盖的部分。
type EmbeddingStore interface {
	// UpsertEmbeddings 幂等写入一批向量。空切片是合法的（表示无事可做）。
	UpsertEmbeddings(ctx context.Context, rows []VideoEmbedding) error

	// LoadVectors 读回指定视频的向量，按 (model, dim) 过滤。
	//
	// 过滤而不是"读出来再筛"：不同 model/dim 的向量在同一个空间里没有意义，
	// 让它们进入计算就等于让一份随机数据参与排序。
	LoadVectors(ctx context.Context, videoIDs []uint, model string, dim int) (map[uint][]float32, error)

	// ListEmbeddingTargets 列出待（重新）向量化的视频。
	//
	// 返回的每一行都带上它的**内容指纹**与已入库向量的指纹（没有则为空），
	// 调用方据此判断"该不该重算"。这个判断刻意留在 Go 里而不是 SQL 里：
	// 指纹算法一旦要改（比如把标签也算进内容版本），SQL 里的表达式
	// 会与 Go 里的实现悄悄漂移，而漂移的后果是"该重算的没重算"。
	ListEmbeddingTargets(ctx context.Context, model string, dim int, limit int) ([]EmbeddingTarget, error)
}

// EmbeddingTarget 是一条"可能需要向量"的视频及其当前版本状态。
type EmbeddingTarget struct {
	VideoID     uint
	Title       string
	Description string
	// CurrentHash 是这条视频此刻的内容指纹（由 v.Title/v.Description 算出）。
	CurrentHash string
	// StoredHash 是已入库向量的内容指纹；没有向量时为空串。
	StoredHash string
}

// NeedsEmbedding 报告这条视频是否需要（重新）生成向量。
//
// 两种情况都需要：从来没有向量，或者内容变了（指纹不一致）。
func (t EmbeddingTarget) NeedsEmbedding() bool {
	return t.StoredHash == "" || t.StoredHash != t.CurrentHash
}

// EmbeddingModelStat 是一条"库里有哪些向量空间"的事实。
//
// 存在的理由（一个真实的坑）：**向量的 model 是提供方决定的，不是配置决定的**。
// 离线演示时 sidecar 用的是词法编码器，它在响应里回的是 "faux-lexical"，
// 而配置里的 ai.embedding_model 是 "text-embedding-3-small"。若按配置的
// 名字入库，库里就会留下"标称 text-embedding-3-small、实际是词法哈希"的向量——
// 之后有人切到真实 provider 时，它会读到这批假向量并混进检索结果，
// 而现象只是"召回质量莫名其妙地差"。因此：
//
//	入库用**响应里的 model**，读取用**库里实际存在的 model**（本结构体）。
type EmbeddingModelStat struct {
	Model string
	Dim   int
	Rows  int64
	// LastUpdated 是这批向量最后一次写入的时间，用于挑"当前在用的"空间。
	LastUpdated time.Time
}

// 编译期断言：GORM 实现必须始终满足这些接口。
var (
	_ EmbeddingStore      = (*VideoRepository)(nil)
	_ EmbeddingSpaceStore = (*VideoRepository)(nil)
)

// EmbeddingSpaceStore 是"发现库里实际有哪些向量空间"的能力。
//
// 接口定义在消费方（本包），实现是 *VideoRepository。
type EmbeddingSpaceStore interface {
	// ListEmbeddingModels 列出库里已入库的向量空间（按最近写入时间倒序）。
	ListEmbeddingModels(ctx context.Context, dim int, limit int) ([]EmbeddingModelStat, error)
}

// UpsertEmbeddings 幂等写入一批向量。
//
// 用 ON DUPLICATE KEY UPDATE 而不是"先查后插"：回填任务与内容更新
// 可能并发处理同一条视频，先查后插会双双查不到然后一起 INSERT，
// 由唯一键决定谁失败——而失败会触发无意义的重试。
//
// 一次 Create 传整批而不是逐条：回填 500 条视频逐条往返会产生 500 次
// 网络往返，把一次几秒的任务变成几分钟。GORM 的批量插入会拼成
// 单条多值 INSERT，代价是**必须**在冲突时用 UPDATE 语义（否则整批失败）。
func (vr *VideoRepository) UpsertEmbeddings(ctx context.Context, rows []VideoEmbedding) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.VideoID == 0 || r.Model == "" || r.Dim <= 0 {
			return errors.New("video: video_id / model / dim 都是幂等键的组成部分，不能为空")
		}
		if len(r.Vector) != r.Dim*4 {
			// 维度声明与实际字节数不一致时拒绝整批：放进去的会是一条
			// "声称 1536 维、实际 512 维"的向量，而它在检索时
			// 要么被静默截断、要么与其它向量错位相乘。
			return fmt.Errorf("video: vid=%d 声明 %d 维但字节数为 %d（应为 %d）",
				r.VideoID, r.Dim, len(r.Vector), r.Dim*4)
		}
		if r.ContentHash == "" {
			return fmt.Errorf("video: vid=%d 缺少 content_hash（没有它就检测不出内容变更导致的失效）", r.VideoID)
		}
	}

	// 冲突时更新向量本体与指纹；created_at 不动（它记录"第一次向量化"），
	// updated_at 显式写当前时间。
	//
	// 四个更新值全部用 `VALUES(col)` 而不是 Go 侧的 rows[0].xxx：
	// 批量插入是**一条**多值 INSERT，用 Go 侧的字面量会让整批都写成第一行的
	// 内容——那是一个"看起来成功、数据全错"的经典坑，而且只在冲突路径上出现。
	return vr.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "video_id"}, {Name: "model"}, {Name: "dim"}},
		DoUpdates: clause.Assignments(map[string]any{
			"content_hash": gorm.Expr("VALUES(content_hash)"),
			"normalized":   gorm.Expr("VALUES(normalized)"),
			"vec":          gorm.Expr("VALUES(vec)"),
			"updated_at":   time.Now(),
		}),
	}).Create(&rows).Error
}

// LoadVectors 读回指定视频的向量。
//
// 只取 id 与 values 两列：BLOB 很大，把整行（含 model 等字符串）拉回来
// 在 500 条视频的规模下会多出可观的分配，而调用方只需要向量本身。
func (vr *VideoRepository) LoadVectors(ctx context.Context, videoIDs []uint, model string, dim int) (map[uint][]float32, error) {
	out := make(map[uint][]float32, len(videoIDs))
	if len(videoIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		VideoID uint
		// 列名是 vec，而字段名是 Vector：没有这个 tag 时 GORM 会去找
		// 名为 "vector" 的列，扫描静默返回空字节串（查询本身不报错），
		// 表现为"向量读回来全是空的"——集成测试抓到的就是这个。
		Vector []byte `gorm:"column:vec"`
	}
	if err := vr.db.WithContext(ctx).
		Model(&VideoEmbedding{}).
		Select("video_id", "vec").
		Where("model = ? AND dim = ? AND video_id IN ?", model, dim, videoIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		vec, err := DecodeVector(r.Vector)
		if err != nil {
			// 单条坏数据不该让整次读取失败：跳过它，让这条视频走"没有向量"
			// 的分支（在 P2 里就是退回其它召回通道），而不是让整个 Feed 报错。
			// 用 continue 而不是 return：一列坏数据不该拖垮另外 499 条。
			continue
		}
		out[r.VideoID] = vec
	}
	return out, nil
}

// ListEmbeddingModels 列出库里已有向量的 model（按最近写入时间倒序）。
//
// 为什么需要它：读取方不能用配置里的模型名去猜"库里存的是什么"。
// 提供方可能换过（真实 provider <-> 离线演示），而配置在两种情况下
// 可以完全一样。按"最近写入的那个空间"读取，等价于"读现在正在生产的
// 那一批向量"，这才是调用方真正想表达的意思。
//
// dim 必须过滤：不同维度的向量不可比较（见 VideoEmbedding 的注释）。
// limit 只是防御性的上界（正常情况下库里只有一两个空间）。
func (vr *VideoRepository) ListEmbeddingModels(ctx context.Context, dim int, limit int) ([]EmbeddingModelStat, error) {
	if limit <= 0 {
		limit = 8
	}
	var rows []struct {
		Model       string
		Dim         int
		VecCount    int64
		LastUpdated time.Time
	}
	err := vr.db.WithContext(ctx).
		Model(&VideoEmbedding{}).
		// 别名必须避开保留字：`rows` 在 MySQL 8 里是保留字，
		// 写成 AS rows 会让整条语句报 1064（语法错误），而调用方只会
		// 看到"读不到向量空间"的降级日志——本机实验室里抓到的就是这个。
		Select("model, dim, COUNT(*) AS vec_count, MAX(updated_at) AS last_updated").
		Where("dim = ?", dim).
		Group("model, dim").
		Order("last_updated DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]EmbeddingModelStat, 0, len(rows))
	for _, r := range rows {
		out = append(out, EmbeddingModelStat{Model: r.Model, Dim: r.Dim, Rows: r.VecCount, LastUpdated: r.LastUpdated})
	}
	return out, nil
}

// ListEmbeddingTargets 列出前 limit 条视频及其版本状态。
//
// 用 LEFT JOIN 而不是"先查 videos 再逐条查 video_embeddings"：
// 后者是典型的 N+1，500 条视频会产生 501 次查询。
//
// ORDER BY v.id 是刻意的：回填任务要能**分批推进**，而按主键排序
// 让"处理到哪了"变成一个有意义的游标（LIMIT 越大越靠后）。
// 按 updated_at 排序会让顺序随写入抖动，表现为同一条视频被反复处理。
func (vr *VideoRepository) ListEmbeddingTargets(ctx context.Context, model string, dim int, limit int) ([]EmbeddingTarget, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []struct {
		ID          uint
		Title       string
		Description string
		StoredHash  string
	}
	err := vr.db.WithContext(ctx).
		Table("videos AS v").
		Select("v.id, v.title, v.description, COALESCE(e.content_hash, '') AS stored_hash").
		Joins("LEFT JOIN video_embeddings AS e ON e.video_id = v.id AND e.model = ? AND e.dim = ?", model, dim).
		Order("v.id ASC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]EmbeddingTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, EmbeddingTarget{
			VideoID:     r.ID,
			Title:       r.Title,
			Description: r.Description,
			CurrentHash: ContentHash(r.Title, r.Description),
			StoredHash:  r.StoredHash,
		})
	}
	return out, nil
}

// EmbeddingText 把标题与描述拼成喂给 embedding 模型的文本。
//
// 为什么单独一个函数而不是各处自己拼：拼法必须与 ContentHash 的输入
// **严格一致**，否则会出现"内容没变但每次都判定为过期"（反复重算）
// 或者"内容变了却判定为没过期"（用了旧向量）——两种都不会报错。
func EmbeddingText(title, description string) string {
	t := strings.TrimSpace(title)
	d := strings.TrimSpace(description)
	switch {
	case t == "":
		return d
	case d == "":
		return t
	default:
		return t + "。" + d
	}
}

// VectorCandidate 是一条"有向量、可以参与相似度计算"的视频。
//
// 只带 id 与发布时间：真正的向量在下一步用 LoadVectors 批量取。
// 把文本与其它列一起查出来会让 500 条候选的这次查询多读几十 KB，
// 而它们一条都用不上。
type VectorCandidate struct {
	ID         uint
	CreateTime time.Time
}

// ListVectorCandidates 列出最近的 limit 条**已入库向量**的候选视频。
//
// 它同时解决两件事，而这两件事都不该由调用方拼 SQL：
//
//  1. **语法过滤**：只保留 model/dim 与当前配置一致、且向量非空的视频。
//     INNER JOIN 是刻意的——LEFT JOIN 会把没有向量的视频也带出来，
//     然后在计算相似度时得到 0 分，表现为"老内容永远排在后面"，
//     而真实原因是"它们根本不该进这个池子"。
//  2. **候选窗口上限**：LIMIT 是 P2 前置 B4 的"应用内暴力检索"能成立的
//     前提。没有它，向量池一大，每次 Feed 请求都会把整个池子读进内存。
//
// 窗口取**最近发布的**而不是"全部"或"随机采样"：新增视频必须有进入
// 语义池的机会，否则新内容永远只能靠探索配额曝光；而按热度取会让
// 语义路退化成"热榜的第二份拷贝"，正是 P2 要避免的事。
//
// ORDER BY v.create_time DESC, v.id DESC：与 ListLatest 的排序一致，
// 于是"同一批候选"在两条路里的含义相同，也让结果与扫描顺序无关（可复现）。
func (vr *VideoRepository) ListVectorCandidates(ctx context.Context, model string, dim int, limit int) ([]VectorCandidate, error) {
	if limit <= 0 || model == "" || dim <= 0 {
		return nil, nil
	}
	var rows []struct {
		ID         uint
		CreateTime time.Time
	}
	err := vr.db.WithContext(ctx).
		Table("video_embeddings AS e").
		Select("v.id, v.create_time").
		Joins("JOIN videos AS v ON v.id = e.video_id").
		// OCTET_LENGTH > 0：一条零字节的向量行（坏数据、或早期版本的残留）
		// 会让 DecodeVector 报错，而它在 SQL 里过滤掉的成本几乎为零。
		Where("e.model = ? AND e.dim = ? AND OCTET_LENGTH(e.vec) > 0", model, dim).
		Order("v.create_time DESC, v.id DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]VectorCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, VectorCandidate{ID: r.ID, CreateTime: r.CreateTime})
	}
	return out, nil
}

// NewestVideoIDs 返回当前最新的 limit 条视频 ID（不要求有向量）。
//
// 为什么需要它：语义召回要做的第一件事是**把"最新的一批"排除掉**——
// 那批内容时序路已经保证了。没有它，语义路会把自己算出来的"热词"
// （最近内容里最常见的词）当成用户兴趣，结果是语义位里塞满刚发布的视频，
// 与探索配额、时序路三重重复。
func (vr *VideoRepository) NewestVideoIDs(ctx context.Context, limit int) ([]uint, error) {
	if limit <= 0 {
		return nil, nil
	}
	ids := make([]uint, 0, limit)
	if err := vr.db.WithContext(ctx).
		Model(&Video{}).
		Order("create_time DESC, id DESC").
		Limit(limit).
		Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// RecentLikedVideoIDs 返回某账号最近点赞的 limit 条视频 ID（新的在前）。
//
// 语义召回的"用户兴趣向量"就是这批视频向量的加权平均，因此这个方法的
// 排序语义直接决定兴趣向量的含义：按点赞时间倒序 = "最近喜欢什么"。
//
// 用 likes.created_at（而不是 videos.create_time）排序：用户兴趣跟着
// **行为时间**走。按视频发布时间排会让"刚点赞了一条老视频"几乎不影响画像，
// 而现实中这恰恰是最强的兴趣信号。
func (lr *LikeRepository) RecentLikedVideoIDs(ctx context.Context, accountID uint, limit int) ([]uint, error) {
	if accountID == 0 || limit <= 0 {
		return nil, nil
	}
	ids := make([]uint, 0, limit)
	if err := lr.db.WithContext(ctx).
		Model(&Like{}).
		Where("account_id = ?", accountID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Pluck("video_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
