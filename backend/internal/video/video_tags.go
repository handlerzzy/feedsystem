package video

import (
	"context"
)

// VideoTagNames 返回每个视频的标签名。
//
// 用一条 JOIN 查询而不是"先查 video_tags 再查 tags"：后者对 50 条视频
// 会产生 2 次往返（还好），但对"用户点赞历史"那种成百上千条的场景
// 就变成了 N+1。
//
// 排序（video_id, name 升序）是刻意的：调用方会把标签名拼进用户可见的
// 文案，顺序随机会让同一份数据两次请求给出不同的理由。
func (vr *VideoRepository) VideoTagNames(ctx context.Context, videoIDs []uint) (map[uint][]string, error) {
	out := make(map[uint][]string, len(videoIDs))
	if len(videoIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		VideoID uint
		Name    string
	}
	err := vr.db.WithContext(ctx).
		Table("video_tags AS vt").
		Select("vt.video_id, t.name").
		Joins("JOIN tags AS t ON t.id = vt.tag_id").
		Where("vt.video_id IN ?", videoIDs).
		Order("vt.video_id ASC, t.name ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		out[r.VideoID] = append(out[r.VideoID], r.Name)
	}
	return out, nil
}
