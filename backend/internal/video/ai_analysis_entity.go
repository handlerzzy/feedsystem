package video

import (
	"encoding/json"
	"time"
)

// 本文件是 P1 新增的持久化结构。
//
// 两条硬约束（总览第 2 节红线 3）：
//   - 只允许**新增表**与**新增带默认值的列**，不得删列或改名；
//   - 新模型必须登记进 internal/db/db.go 的 AutoMigrate 白名单，
//     否则表不会被创建（AutoMigrate 是硬编码列表，不是自动发现）。

// 分析状态。取值刻意只有两个：
//
//	ok      —— 拿到了可用的标签/摘要
//	invalid —— 模型返回了非法结果（JSON 不合法、字段越界、一个有效标签都没有）
//
// 为什么不把"失败"也记成一行：失败（超时、sidecar 不可达）会走 worker 的重试，
// 重试期间没有值得落库的结果；而**重试耗尽后**连这一行都不会被写入，
// 因为写入本身也要经过同一个客户端。把失败写成 db_failed 之类只会在表里
// 堆积一批"永远不会有结果"的行，让"这个视频到底分析过没有"变得需要额外判断。
const (
	AnalysisStatusOK      = "ok"
	AnalysisStatusInvalid = "invalid"
)

// VideoTag 的来源。**这是 P1 最关键的区分**：用户手写的 #话题 与模型产出的
// 标签共用同一张 tags 表，靠这一列分辨来源。
const (
	TagSourceUser = "user"
	TagSourceAI   = "ai"
)

// VideoAIAnalysis 记录一次 AI 分析的结果。
//
// 为什么必须保留 Model 与 PromptVersion：
// 改了 prompt 之后要能回答"这批标签是旧 prompt 产出的，需要重跑"，
// 也要能对比两版 prompt 的效果。没有这两个字段，重跑就只能全量盲跑，
// 而且新旧结果混在一张表里无法区分。
type VideoAIAnalysis struct {
	ID      uint `gorm:"primaryKey" json:"id"`
	VideoID uint `gorm:"not null;index:idx_ai_analyses_video;uniqueIndex:idx_ai_analyses_identity,priority:1" json:"video_id"`

	// Model / PromptVersion 与 VideoID 一起构成幂等键。
	//
	// 为什么用 (video_id, model, prompt_version) 而不是只按 video_id 去重：
	// 换模型或改 prompt 之后**必须**能重新分析同一视频并留下新的一行
	// （否则无法对比版本效果）；而同一版本的重复消费则必须收敛到同一行。
	// 只按 video_id 去重会永久锁死第一次的结果，改 prompt 就再也跑不动。
	Model         string `gorm:"type:varchar(100);not null;uniqueIndex:idx_ai_analyses_identity,priority:2" json:"model"`
	PromptVersion string `gorm:"type:varchar(50);not null;uniqueIndex:idx_ai_analyses_identity,priority:3" json:"prompt_version"`

	// Summary 是 AI 的一句话摘要，**不覆盖** video.Description（用户原文必须保留）。
	Summary string `gorm:"type:varchar(500);not null;default:''" json:"summary"`

	// TagsJSON 存模型给出的标签（含置信度），保留原始产出以便排查
	// "为什么这个标签没入库"（可能是被置信度阈值或归一化过滤掉了）。
	TagsJSON string `gorm:"type:text" json:"tags_json"`

	Status     string    `gorm:"type:varchar(20);not null;default:'';index" json:"status"`
	AnalyzedAt time.Time `gorm:"autoCreateTime" json:"analyzed_at"`
}

// TableName 显式指定表名。
//
// 不依赖 GORM 的复数推断：表名是对外契约的一部分（运维会直接查这张表），
// 推断规则一旦随版本变化，影响的就不只是代码。
func (VideoAIAnalysis) TableName() string { return "video_ai_analyses" }

// TagSuggestion 是模型给出的一个标签及其置信度。
type TagSuggestion struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

// AnalysisOutput 是一次分析的规范化结果。
type AnalysisOutput struct {
	Summary string          `json:"summary"`
	Tags    []TagSuggestion `json:"tags"`
}

// TagsJSON 把标签序列化成入库用的字符串。
//
// 序列化失败不返回错误：它只会发生在结构体不可序列化时（不可能），
// 而让一次已经成功的分析因为"记不下原始产出"而失败是本末倒置。
func (o AnalysisOutput) TagsJSON() string {
	b, err := json.Marshal(o.Tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}
