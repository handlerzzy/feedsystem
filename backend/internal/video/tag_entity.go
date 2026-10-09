package video

import "regexp"

type Tag struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"uniqueIndex;type:varchar(100);not null" json:"name"`
}

type VideoTag struct {
	ID      uint `gorm:"primaryKey"`
	TagID   uint `gorm:"index:idx_video_tag_tag_video,priority:1;index;not null"`
	VideoID uint `gorm:"index:idx_video_tag_tag_video,priority:2;index;not null"`

	// 以下两列是 P1 新增的，都**带默认值**（总览第 2 节红线 3：
	// 只允许新增带默认值或可空的列）。
	//
	// Source —— user / ai。用户手写的 #话题 与模型产出的标签共用同一张
	//           tags 表（同名标签只有一行），靠这一列区分来源。
	//           默认 user 是关键：既有代码插入时完全不写这一列，
	//           得到的语义与改造前完全一致。
	// Confidence —— AI 标签的置信度（0~1）。用户标签恒为 1：
	//           用户亲手写的标签就是确定的，不该因为"没有置信度"
	//           而在任何按置信度过滤的逻辑里被误伤。
	Source     string  `gorm:"type:varchar(10);not null;default:'user'" json:"source"`
	Confidence float64 `gorm:"not null;default:1" json:"confidence"`
}

var tagRegex = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

func ExtractTags(text string) []string {
	matches := tagRegex.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var tags []string
	for _, m := range matches {
		tag := m[1]
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}
