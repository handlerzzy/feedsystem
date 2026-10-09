package feed

import "time"

type FeedAuthor struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
}

type FeedVideoItem struct {
	ID          uint       `json:"id"`
	Author      FeedAuthor `json:"author"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	PlayURL     string     `json:"play_url"`
	CoverURL    string     `json:"cover_url"`
	CreateTime  int64      `json:"create_time"`
	LikesCount  int64      `json:"likes_count"`
	IsLiked     bool       `json:"is_liked"`
	// Summary 是 P1 新增的 AI 一句话摘要，**可缺省**：
	// omitempty 保证 AI 关闭（或该视频还没被分析）时响应体与改造前逐字节一致，
	// 旧前端不解析它也不会受影响（总览第 2 节红线 6：只允许新增可缺省字段）。
	//
	// 它**不是** Description 的替代品：用户原文永远保留在 description 里。
	Summary string `json:"summary,omitempty"`
	// Reason 是 P2 新增的"为什么推给你"，**可缺省**（同上：AI 关闭、
	// 没有可用信号、或这一路没开时它根本不出现，响应体与改造前逐字节一致）。
	//
	// 红线（P2 §4.6）：它必须来自真实信号——命中的兴趣标签、
	// 与点赞历史语义相似、或是新内容探索。**编造的理由比没有理由更糟**，
	// 所以没有信号时这里是空字符串，而不是一句"猜你喜欢"。
	Reason string `json:"reason,omitempty"`
}

type ListLatestRequest struct {
	Limit      int   `json:"limit"`
	LatestTime int64 `json:"latest_time"`
}

type ListLatestResponse struct {
	VideoList []FeedVideoItem `json:"video_list"`
	NextTime  int64           `json:"next_time"`
	HasMore   bool            `json:"has_more"`
}

type ListLikesCountRequest struct {
	Limit            int    `json:"limit"`
	LikesCountBefore *int64 `json:"likes_count_before,omitempty"`
	IDBefore         *uint  `json:"id_before,omitempty"`
}

type LikesCountCursor struct {
	LikesCount int64
	ID         uint
}

type ListLikesCountResponse struct {
	VideoList            []FeedVideoItem `json:"video_list"`
	NextLikesCountBefore *int64          `json:"next_likes_count_before,omitempty"`
	NextIDBefore         *uint           `json:"next_id_before,omitempty"`
	HasMore              bool            `json:"has_more"`
}

type ListByFollowingRequest struct {
	Limit      int   `json:"limit"`
	LatestTime int64 `json:"latest_time"`
}

type ListByFollowingResponse struct {
	VideoList []FeedVideoItem `json:"video_list"`
	NextTime  int64           `json:"next_time"`
	HasMore   bool            `json:"has_more"`
}

type ListByPopularityRequest struct {
	Limit          int   `json:"limit"`
	AsOf           int64 `json:"as_of"`  // 服务器返回的分钟时间戳；第一页传0
	Offset         int   `json:"offset"` // 下一页从这里开始；第一页传0
	LatestIDBefore *uint `json:"latest_id_before,omitempty"`

	// DB fallback 用（可选）
	LatestPopularity int64     `json:"latest_popularity"`
	LatestBefore     time.Time `json:"latest_before"`
}

type ListByPopularityResponse struct {
	VideoList  []FeedVideoItem `json:"video_list"`
	AsOf       int64           `json:"as_of"`
	NextOffset int             `json:"next_offset"`
	HasMore    bool            `json:"has_more"`

	NextLatestPopularity *int64     `json:"next_latest_popularity,omitempty"`
	NextLatestBefore     *time.Time `json:"next_latest_before,omitempty"`
	NextLatestIDBefore   *uint      `json:"next_latest_id_before,omitempty"`
}
