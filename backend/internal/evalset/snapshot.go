package evalset

import (
	"fmt"
	"time"
)

// 本文件负责"把原始行变成可用来评测的快照"这一段，核心是**不使用未来信息**。
//
// 为什么单独抽出来：这条原则有两个落地点，而且在两个不同的地方容易各错一半——
//   - 标签只取切分点之后的互动（RelevanceOf）；
//   - 排序特征只取切分点之前的状态（本文件的 DeriveFeatures）。
//
// 后者尤其隐蔽：videos.likes_count / videos.popularity 是会被**持续累加**的列，
// 导出时的值里已经包含了观测窗口内的互动。如果直接拿它当排序特征，
// 就等于"用答案去排序"，基线数字会虚高，而报告里完全看不出来。
// 这是 review 抓到的真实缺陷，别再改回去。

// InteractionKinds 是评测认识的全部互动类型。
const (
	KindLike    = "like"
	KindComment = "comment"
)

// AsOf 把一份"导出时刻"的快照转换成"切分点时刻"的快照：
// 所有排序特征都用切分点**之前**的互动重算。
//
// 参数 likes / comments 是导出时刻的全部互动行（二者共用 Interaction 结构，
// 用 Kind 区分）；返回的是把特征回填到切分点状态的视频列表。
//
// 注意本函数不做任何"猜测"：
//   - 切分点之前的点赞数 → 直接数出来，这是真实可观测的；
//   - 热度（popularity）→ 原始值是 Redis 里的滑动累计量，历史值无法从 MySQL
//     精确还原，因此这里用一个**显式近似的代理**并在 Snapshot.FeatureNote 里
//     写明。宁可写清"这是代理"，也不要让读者以为它是线上那个 popularity。
func AsOf(videos []Video, likes, comments []Interaction, splitAt time.Time) ([]Video, string) {
	likeCount := map[uint]int64{}
	commentCount := map[uint]int64{}
	for _, it := range likes {
		if it.At.After(splitAt) {
			continue
		}
		likeCount[it.VideoID]++
	}
	for _, it := range comments {
		if it.At.After(splitAt) {
			continue
		}
		commentCount[it.VideoID]++
	}

	out := make([]Video, 0, len(videos))
	for _, v := range videos {
		likes := likeCount[v.ID]
		comments := commentCount[v.ID]
		v.LikesCount = likes
		// 代理热度：与线上 UpdatePopularityCache 的加权方向一致（评论 > 点赞），
		// 但绝对量级不同。它只用于**比较不同排序策略**，不能与线上 popularity 数值对照。
		v.Popularity = likes + 2*comments
		out = append(out, v)
	}

	note := "likes_count 与 popularity 均为时间切分点之前的互动推算值；" +
		"popularity 是代理值（likes + 2*comments），与线上 Redis 累计量的绝对量级不同，" +
		"仅用于策略之间的相对比较"
	return out, note
}

// SplitAtFor 返回快照的切分时间点；为零时按 SplitRatio 现算一个。
func (s *Snapshot) SplitAtFor() (time.Time, error) {
	if !s.SplitAt.IsZero() {
		return s.SplitAt, nil
	}
	ratio := s.SplitRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultSplitRatio
	}
	return ComputeSplit(s.Videos, ratio)
}

// DefaultSplitRatio 是切分比例的缺省值。
//
// 单独定义成常量而不是在多处写 0.5：这个值会出现在报告文案里，
// 一旦与计算用的值不一致，报告就会"说一套、算一套"（review 抓到过）。
const DefaultSplitRatio = 0.5

// Rebuild 以给定的切分比例**重新**计算切分点与特征。
//
// 为什么需要它：命令行允许用 -split 覆盖快照里的切分比例。若只改 SplitRatio
// 字段而不重算 SplitAt 与特征，报告会显示"按 80% 分位切分"而实际仍按文件里
// 那个切分点算——一个只会误导人的元数据。
//
// 它只做纯计算，不访问数据库，因此可以被直接单测。
func (s *Snapshot) Rebuild(ratio float64, rawVideos []Video, likes, comments []Interaction) error {
	if ratio <= 0 || ratio >= 1 {
		return fmt.Errorf("切分比例必须在 (0,1) 之间，实际 %v", ratio)
	}
	at, err := ComputeSplit(rawVideos, ratio)
	if err != nil {
		return err
	}
	features, note := AsOf(rawVideos, likes, comments, at)
	s.Videos = features
	s.SplitAt = at
	s.SplitRatio = ratio
	s.FeatureNote = note
	return nil
}
