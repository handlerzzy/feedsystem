package video

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VideoAnalysisStore 是 AI 分析落库所需的全部持久化能力。
//
// 为什么在这里定义接口（而不是在 worker 包里）：本项目的惯例是
// **接口定义在消费方**（见 internal/feed/feed_store.go 的长注释）。
// 但这里的消费方其实是"这个仓库自己"——真正需要被替换的是整条
// "回查视频 -> 调模型 -> 幂等入库"的流程，所以分析器的可替换性
// 通过 worker 侧的 Analyzer 接口来提供（见 internal/worker/contentanalysisworker.go），
// 这里保留一个窄接口是为了让仓储层可以被单测直接构造与断言。
//
// *VideoRepository 隐式实现本接口。
type VideoAnalysisStore interface {
	// LoadVideoForAnalysis 回查分析所需的视频文本。
	//
	// 事件载荷里只带 video_id、不带标题与描述（避免把长文本塞进 MQ），
	// 因此这一步是必需的。视频已被删除时返回 ErrVideoNotFound。
	LoadVideoForAnalysis(ctx context.Context, videoID uint) (*AnalysisInput, error)

	// UpsertAnalysis 幂等写入一次分析结果，返回该行的 ID。
	//
	// 幂等键是 (video_id, model, prompt_version)：同一版本的重复消费
	// 收敛到同一行；换模型/改 prompt 则产生新行，从而能对比版本效果。
	UpsertAnalysis(ctx context.Context, a *VideoAIAnalysis) (uint, error)

	// AttachTags 在**同一个事务**里把标签关联到视频。
	//
	// 与 UpsertAnalysis 分开是为了让前者能被单独复用（例如将来只更新摘要），
	// 但生产路径上两者必须同事务：分析与标签不一致会让"这个视频有哪些 AI 标签"
	// 变成看运气的问题。实现内部用 db.Transaction 包住两步。
	AttachTags(ctx context.Context, videoID uint, tags []TagSuggestion) error

	// SaveAnalysisWithTags 是生产路径使用的入口：一次事务里写分析行 + 关联标签。
	SaveAnalysisWithTags(ctx context.Context, a *VideoAIAnalysis, tags []TagSuggestion) error
}

// ErrVideoNotFound 表示待分析的视频不存在（已被作者删除）。
//
// 单独一个哨兵：worker 需要把"视频没了"（无事可做，Ack 丢弃）
// 与"数据库暂时写不进去"（要重试）区分开。
var ErrVideoNotFound = errors.New("video: 待分析的视频不存在")

// AnalysisInput 是喂给模型的最小输入。
//
// 刻意不含 AuthorID / 统计数字：那些不是"内容语义"，让模型看到它们
// 只会引入无关信息，也会扩大 PII 面（作者标识不该出进程边界）。
type AnalysisInput struct {
	VideoID     uint
	Title       string
	Description string
}

// LoadVideoForAnalysis 实现 VideoAnalysisStore。
func (vr *VideoRepository) LoadVideoForAnalysis(ctx context.Context, videoID uint) (*AnalysisInput, error) {
	var v Video
	if err := vr.db.WithContext(ctx).Select("id", "title", "description").First(&v, videoID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrVideoNotFound
		}
		return nil, err
	}
	return &AnalysisInput{VideoID: v.ID, Title: v.Title, Description: v.Description}, nil
}

// UpsertAnalysis 实现 VideoAnalysisStore（幂等写入，见接口注释）。
func (vr *VideoRepository) UpsertAnalysis(ctx context.Context, a *VideoAIAnalysis) (uint, error) {
	if a == nil {
		return 0, errors.New("analysis is nil")
	}
	if a.VideoID == 0 || a.Model == "" || a.PromptVersion == "" {
		return 0, errors.New("video_id / model / prompt_version 都是幂等键的组成部分，不能为空")
	}
	if a.AnalyzedAt.IsZero() {
		a.AnalyzedAt = time.Now()
	}

	// 用 ON DUPLICATE KEY UPDATE 而不是"先查后插"：MQ 会重投，
	// 两个消费者可能同时处理同一条消息（至少一次语义），先查后插会双双查不到
	// 然后一起 INSERT，由唯一键决定谁失败——而失败会触发无意义的重试。
	//
	// 更新列刻意包含 summary/tags/status：重投时模型可能给出不同结果，
	// 保留最新一次是有意义的（旧结果没有单独留存的必要，原始产出在分析行里）。
	if err := vr.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "video_id"}, {Name: "model"}, {Name: "prompt_version"}},
		DoUpdates: clause.Assignments(map[string]any{
			"summary":     a.Summary,
			"tags_json":   a.TagsJSON,
			"status":      a.Status,
			"analyzed_at": a.AnalyzedAt,
		}),
	}).Create(a).Error; err != nil {
		return 0, err
	}

	// 命中已存在行时 Create 不会回填 ID（与 resolveTagID 的注释同一个坑），
	// 因此这里显式回查一次。回查用一致读即可：本函数不在长事务里，
	// 而且即使读到旧快照，拿到的也只是同一幂等键下的那一行。
	if a.ID != 0 {
		return a.ID, nil
	}
	var existing VideoAIAnalysis
	if err := vr.db.WithContext(ctx).
		Where("video_id = ? AND model = ? AND prompt_version = ?", a.VideoID, a.Model, a.PromptVersion).
		First(&existing).Error; err != nil {
		return 0, err
	}
	a.ID = existing.ID
	return existing.ID, nil
}

// AttachTags 实现 VideoAnalysisStore。
//
// 三步，全部在一个事务里：
//  1. 解析标签行（存在则复用，不存在则新建）——与用户手写标签共用同一张表；
//  2. 插入 video_tags 关联，**已存在则跳过**；
//  3. 返回。
//
// 为什么第 2 步是"先查再插"而不是依赖唯一索引：video_tags 上只有普通索引
// (idx_video_tag_tag_video)，加唯一索引会在已存在重复行的库上迁移失败，
// 而 P1 的硬约束是"只新增，不改既有结构"。因此在代码层保证幂等：
// 同一 (video_id, tag_id) 已有关联时不再插入。
//
// 代价是并发下理论上仍可能插出重复行（两个事务同时查不到）。这个风险
// 是可接受的，因为：worker 是单消费者语义（同一条消息只会被一个实例处理），
// 而重复行对读路径无害（ListByTag 用 JOIN + DISTINCT 语义取视频，
// 同一视频不会因为两条关联而重复出现）。真正的唯一性由 tags.name 上的
// 唯一索引保证——那才是"标签不重复"这条验收指标的关键。
func (vr *VideoRepository) AttachTags(ctx context.Context, videoID uint, tags []TagSuggestion) error {
	if videoID == 0 {
		return errors.New("video_id is required")
	}
	if len(tags) == 0 {
		return nil
	}
	return vr.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return attachTagsTx(tx, videoID, tags, TagSourceAI)
	})
}

// videoTagTable 是 video_tags 的表名，用一个显式常量而不是在 SQL 里写死字符串。
//
// 这样"哪些地方依赖这个表名"是可搜索的；将来真要改表名时（那属于
// 红线 3 禁止的操作，这里只是把风险收敛到一处）不必去 grep 原生 SQL 字符串。
const videoTagTable = "video_tags"

// attachTagsTx 是标签落库的事务体，SaveAnalysisWithTags 也复用它。
//
// 抽出来的理由：事务体一旦有两份实现，就一定会有一份忘了判重或忘了写 source。
//
// 判重用的是**单条** `INSERT ... SELECT ... WHERE NOT EXISTS`，而不是
// "先 Count 再 Create"两步（P2 前置条件 R1 记录的就是那两步之间的 TOCTOU 窗口：
// 多 worker 副本 + MQ 至少一次重投时，两个副本可能同时查到 0 然后一起 INSERT，
// 而 video_tags 上没有 (video_id, tag_id) 唯一索引可以兜底——加唯一索引需要先
// 清理历史重复行，而"只新增、不改既有结构"是本轮的硬约束）。
//
// 为什么单条语句就没有窗口：MySQL 在 REPEATABLE READ 下会对
// `INSERT ... SELECT` 读取的行加共享 next-key 锁，并对待插入的行加排他锁。
// 并发的第二个事务要么等第一个提交（然后 NOT EXISTS 看到行、什么都不插），
// 要么在间隙锁上排队——两种情况都不会插出第二行。
// 附带好处是这条语句自身的原子性，不再依赖"resolveTagID 恰好先锁住了 tags 行"
// 这个实现细节（它一旦改成只读不写，两步写法就会立刻真的出问题，
// 而表现只是"标签重复"——ListByTag 用 DISTINCT 语义取视频，读路径完全看不出来）。
//
// 冲突时不更新 source/confidence 是刻意的：用户手写的标签（source=user）
// 不该因为 AI 又给出同一个词而被改成 ai——那会丢失"用户确实写过它"这个事实。
// 来源取"首个写入者"。`WHERE NOT EXISTS` 天然表达了这一点。
func attachTagsTx(tx *gorm.DB, videoID uint, tags []TagSuggestion, source string) error {
	if videoID == 0 {
		return errors.New("video_id is required")
	}
	for _, t := range tags {
		tagID, err := resolveTagID(tx, t.Name)
		if err != nil {
			return fmt.Errorf("解析标签 %q: %w", t.Name, err)
		}

		if err := tx.Exec(
			"INSERT INTO "+videoTagTable+
				" (video_id, tag_id, source, confidence) "+
				"SELECT ?, ?, ?, ? FROM DUAL "+
				"WHERE NOT EXISTS (SELECT 1 FROM "+videoTagTable+
				" WHERE video_id = ? AND tag_id = ?)",
			videoID, tagID, source, clamp01(t.Confidence), videoID, tagID,
		).Error; err != nil {
			return err
		}
	}
	return nil
}

// SaveAnalysisWithTags 实现 VideoAnalysisStore：一个事务里写分析行 + 关联标签。
//
// 为什么必须同事务：分析行写着"这个视频分析过了、标签是这些"，
// 而标签在 video_tags 里。两者不一致时，运维看到的是"分析成功但没标签"，
// 而重跑又会被幂等键挡住——问题会永久固化。
func (vr *VideoRepository) SaveAnalysisWithTags(ctx context.Context, a *VideoAIAnalysis, tags []TagSuggestion) error {
	if a == nil {
		return errors.New("analysis is nil")
	}
	if a.VideoID == 0 || a.Model == "" || a.PromptVersion == "" {
		return errors.New("video_id / model / prompt_version 都是幂等键的组成部分，不能为空")
	}
	if a.AnalyzedAt.IsZero() {
		a.AnalyzedAt = time.Now()
	}
	if a.TagsJSON == "" {
		a.TagsJSON = AnalysisOutput{Tags: tags}.TagsJSON()
	}

	return vr.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "video_id"}, {Name: "model"}, {Name: "prompt_version"}},
			DoUpdates: clause.Assignments(map[string]any{
				"summary":     a.Summary,
				"tags_json":   a.TagsJSON,
				"status":      a.Status,
				"analyzed_at": a.AnalyzedAt,
			}),
		}).Create(a).Error; err != nil {
			return err
		}
		return attachTagsTx(tx, a.VideoID, tags, TagSourceAI)
	})
}

// clamp01 把置信度夹到 [0,1]。
//
// 模型偶尔会给出 1.5 或 -0.2。放任入库会让"置信度"这个字段失去可比性，
// 也会让阈值过滤出现反直觉的结果（-0.2 永远被过滤，1.5 永远通过）。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// 编译期断言：GORM 实现必须始终满足这个接口。
var _ VideoAnalysisStore = (*VideoRepository)(nil)

// LatestAISummaries 批量取每个视频最新一条 AI 摘要。
//
// 实现在 video 包（表的所有者是它），接口声明在 feed 包（消费方）——
// 这正是本项目"接口定义在消费方"的惯例：*VideoRepository 隐式满足
// feed.SummaryLookup，因此 internal/http/router.go 无需为它做任何适配。
//
// 三条实现要点：
//
//  1. **只取 status='ok' 的行**：invalid 行没有可用摘要，
//     把它当摘要返回等于把"分析失败"伪装成"模型没写出摘要"。
//  2. **按 video_id 取最新一条**：同一个视频可能有多行（换模型/改 prompt 后
//     各留一行），用户看到的应当是最近一次分析的结果。
//  3. **一次查询取回所有视频**：Feed 一次最多 50 条，逐条查会把一次请求
//     变成 50 次往返。这里用 IN + 窗口函数不可用时，退化为
//     "按视频分组取最大 id 的那一行"——用一条 SQL 完成。
func (vr *VideoRepository) LatestAISummaries(ctx context.Context, videoIDs []uint) (map[uint]string, error) {
	out := make(map[uint]string, len(videoIDs))
	if len(videoIDs) == 0 {
		return out, nil
	}

	var rows []struct {
		VideoID uint
		Summary string
		ID      uint
	}
	// 子查询取出每个视频的最大 id，再回表取摘要。
	//
	// 不用 GROUP BY + MAX(id) 直接取 summary：MySQL 的 ONLY_FULL_GROUP_BY
	// 下那样写是非法的（summary 不在 group by 里，且不是聚合函数），
	// 而关掉这个 sql_mode 是全局副作用，不该为了一个查询去动。
	err := vr.db.WithContext(ctx).
		Table("video_ai_analyses AS a").
		Select("a.video_id, a.summary, a.id").
		Joins("JOIN (SELECT MAX(id) AS id FROM video_ai_analyses WHERE status = ? AND video_id IN ? GROUP BY video_id) AS latest ON latest.id = a.id",
			AnalysisStatusOK, videoIDs).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Summary != "" {
			out[r.VideoID] = r.Summary
		}
	}
	return out, nil
}
