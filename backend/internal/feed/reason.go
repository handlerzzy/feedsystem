package feed

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/video"

	"go.uber.org/zap"
)

// 本文件实现 P2 §4.6「推荐理由」。
//
// 一条不可协商的红线（P2 正文与 §7 的风险表都写了）：
//
//	理由必须基于**真实信号**生成。编造的理由比没有理由更糟，宁可返回空字符串。
//
// "真实信号"在这个系统里只有三种，本文件只允许这三种：
//
//	1. 这条视频与用户近期点赞过的内容**语义相似**（语义召回算出来的余弦分）；
//	2. 这条视频的标签属于用户点赞过的视频里**反复出现**的标签；
//	3. 这条视频是**新发布且互动很少**的内容（探索通道）。
//
// 刻意**不做**的事：不调用模型生成一句话。模型说"因为你喜欢技术内容"
// 而实际触发原因是"热度高"时，那条理由是假的，而用户无法分辨——
// 一旦被发现一次，之后所有理由都不再被相信。用模板拼一句基于事实的话
// 看起来朴素，但每一句都能被数据验证。

// 理由生成的常量。
const (
	// reasonTagMinHits 是一个标签要在用户的点赞历史里出现几次才算"兴趣标签"。
	//
	// 取 2 而不是 1：只出现一次就写"你常看 X"是**夸大事实**（用户只点过一次），
	// 而夸大是幻觉的温和版本——同样会侵蚀信任。
	// 也不要取 3：点赞本来稀疏，阈值太高会让绝大多数用户没有任何兴趣标签，
	// 理由功能整体变成"永远为空"。
	reasonTagMinHits = 2

	// reasonProfileLikes 是构建兴趣画像时最多回看多少条点赞。
	// 与语义路共用 UserProfileLikes 的取值理由（兴趣会变）。
	reasonProfileLikes = UserProfileLikes

	// reasonTagsPerVideo 是单条理由里最多提几个标签。
	reasonTagsPerVideo = 2

	// reasonTimeout 是理由相关查询的预算（与摘要查询同一个量级）。
	reasonTimeout = 50 * time.Millisecond
)

// reasonBuilder 是"每页一次"的理由计算上下文。
//
// 为什么把上下文攒起来而不是每条视频各查一次：一页 20 条视频逐条查标签
// 就是 20 次往返。这里固定为**两次**查询（候选标签 + 用户兴趣标签），
// 与摘要查询同一个思路。
type reasonBuilder struct {
	// tags 是候选视频的标签（videoID -> 标签名，按标签名升序，可复现）。
	tags map[uint][]string
	// interests 是用户的兴趣标签（标签名 -> 在点赞历史里出现的次数）。
	interests map[string]int
	// semantic 是被语义召回命中的视频 ID 集合（含余弦分，便于日志与调参）。
	semantic map[uint]float64
	// explore 是被冷启动探索通道选中的视频 ID 集合。
	explore map[uint]bool
	// reasons 是最终结果（videoID -> 理由文本，空串表示不展示）。
	reasons map[uint]string
}

// newReasonBuilder 组装本页的理由计算上下文。
//
// 任何一路失败都只是"这一部分信号用不上"，绝不返回错误：
// 理由是锦上添花里最花的那一朵，让 Feed 因为它 500 是把承重墙
// 拿去给可选功能陪葬（总览第 1 节的三条裁决）。
func (f *FeedService) newReasonBuilder(ctx context.Context, videos []*video.Video, viewerAccountID uint, semantic []SemanticHit, explore []*video.Video) *reasonBuilder {
	if len(videos) == 0 {
		return nil
	}
	b := &reasonBuilder{
		semantic:  make(map[uint]float64, len(semantic)),
		explore:   make(map[uint]bool, len(explore)),
		reasons:   make(map[uint]string, len(videos)),
		interests: map[string]int{},
	}
	for _, h := range semantic {
		if h.Video != nil {
			b.semantic[h.Video.ID] = h.Score
		}
	}
	for _, v := range explore {
		if v != nil {
			b.explore[v.ID] = true
		}
	}

	opCtx, cancel := context.WithTimeout(ctx, reasonTimeout)
	defer cancel()

	if f.reasonSignals != nil {
		ids := make([]uint, 0, len(videos))
		for _, v := range videos {
			ids = append(ids, v.ID)
		}
		tags, err := f.reasonSignals.VideoTagNames(opCtx, ids)
		if err != nil {
			logging.Ctx(ctx).Warn("查询视频标签失败，本次不生成基于标签的推荐理由（不影响 Feed 结果）",
				zap.Int("videos", len(ids)), zap.Error(err))
		} else {
			b.tags = tags
		}
	}

	b.interests = f.userInterestTags(opCtx, viewerAccountID)
	return b
}

// userInterestTags 统计用户点赞历史里的标签频次。
//
// 数据来源是**点赞**而不是"任何互动"：点赞是成本最低、量最大、也最接近
// "我想要更多这样的内容"的信号。评论更多表达观点（可能是反对），
// 用它当兴趣会得到"你常看 X"而用户其实在批评 X。
func (f *FeedService) userInterestTags(ctx context.Context, accountID uint) map[string]int {
	if f.reasonSignals == nil || f.vectorStore == nil || accountID == 0 {
		// 没有向量能力就没有"最近点赞"这条查询能力（它是 VectorRecallStore
		// 的一部分）：这时不生成基于兴趣的理由，而不是退回"查全部点赞"
		// ——后者可能成千上万条。
		return nil
	}
	likedIDs, err := f.vectorStore.RecentLikedVideoIDs(ctx, accountID, reasonProfileLikes)
	if err != nil {
		logging.Ctx(ctx).Warn("查询用户点赞历史失败，本次不生成兴趣理由（不影响 Feed 结果）",
			zap.Uint("account_id", accountID), zap.Error(err))
		return nil
	}
	if len(likedIDs) == 0 {
		return nil
	}
	tags, err := f.reasonSignals.VideoTagNames(ctx, likedIDs)
	if err != nil {
		logging.Ctx(ctx).Warn("查询点赞视频标签失败，本次不生成兴趣理由（不影响 Feed 结果）",
			zap.Int("videos", len(likedIDs)), zap.Error(err))
		return nil
	}
	out := make(map[string]int)
	for _, names := range tags {
		for _, name := range names {
			out[name]++
		}
	}
	return out
}

// build 为每条视频生成理由。顺序与入参一致，便于调用方按下标取值。
func (b *reasonBuilder) build(videos []*video.Video, semanticOrder []SemanticHit) {
	if b == nil {
		return
	}
	// 语义召回的名次：排在最前面的那条最值得说"与你点赞过的内容相似"。
	// 名次而不是分数：分数是模型相关的数（换模型就不可比），
	// 而"是不是召回的最前面几条"是一条与模型无关的事实。
	rank := make(map[uint]int, len(semanticOrder))
	for i, h := range semanticOrder {
		if h.Video != nil {
			if _, exists := rank[h.Video.ID]; !exists {
				rank[h.Video.ID] = i
			}
		}
	}
	for _, v := range videos {
		b.reasons[v.ID] = b.reasonFor(v, rank[v.ID], rank)
	}
}

// reasonFor 按优先级生成一条理由。返回空串表示"这条没有可说的真实信号"。
//
// 优先级（从"最具体"到"最泛"）：
//
//	① 命中的兴趣标签  —— 最可验证，用户能立刻对上号（"你常看 猫"）
//	② 与点赞内容相似  —— 语义信号，真实但抽象
//	③ 新内容探索      —— 说明为什么它出现在这里（否则用户会疑惑为什么推一条没人看的视频）
//
// 注意 ② 只在"没有命中兴趣标签"时使用：有了具体标签还说一句笼统的
// "相似"，等于把更有信息量的那句话丢掉。
func (b *reasonBuilder) reasonFor(v *video.Video, semanticRank int, rank map[uint]int) string {
	if v == nil {
		return ""
	}
	if reason := b.tagReason(v.ID); reason != "" {
		return reason
	}
	if _, ok := rank[v.ID]; ok {
		// 只对语义召回里排在最前面的若干条说这句话：一个页面里如果
		// 每条都写"与你点赞过的内容相似"，这句话就不再传递任何信息。
		if semanticRank < reasonSimilarityTopN {
			return "与你近期点赞的内容相似"
		}
	}
	if b.explore[v.ID] {
		return "新发布，等待你的第一条反馈"
	}
	return ""
}

// tagReason 生成基于兴趣标签的理由。
func (b *reasonBuilder) tagReason(videoID uint) string {
	names := b.tags[videoID]
	if len(names) == 0 || len(b.interests) == 0 {
		return ""
	}
	hit := make([]string, 0, len(names))
	for _, name := range names {
		if b.interests[name] >= reasonTagMinHits {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return ""
	}
	// 稳定排序：标签频次降序、同频次按名字升序。
	// 不排序的话同一份数据两次请求可能给出不同的理由（Go 的 map 顺序随机），
	// 而"同一个视频两次刷新理由不一样"是最容易被察觉的不可信来源。
	sort.Slice(hit, func(i, j int) bool {
		if b.interests[hit[i]] != b.interests[hit[j]] {
			return b.interests[hit[i]] > b.interests[hit[j]]
		}
		return hit[i] < hit[j]
	})
	if len(hit) > reasonTagsPerVideo {
		hit = hit[:reasonTagsPerVideo]
	}
	return "你常看 " + strings.Join(hit, "、")
}

// reasonSimilarityTopN 是"值得说一句与点赞相似"的语义召名次上限。
const reasonSimilarityTopN = 3

// latestReasons 批量生成推荐理由。失败一律降级为空（前端不显示）。
//
// 触发条件刻意写得很紧：**没有开任何召回路时不查任何东西**。
// 这一条保证了"三关全闭时与基准线逐字节一致"——理由字段是
// omitempty 的，空 map 让响应体与改造前完全相同。
func (f *FeedService) latestReasons(ctx context.Context, videos []*video.Video, viewerAccountID uint, semantic []SemanticHit, explore []*video.Video) map[uint]string {
	if len(videos) == 0 {
		return nil
	}
	// 一条召回都没开：不生成任何理由。
	// 判据用"两路候选都为空"而不是重新读配额：这样即使有人手工构造
	// 了一次带配额的调用，只要没真的召回到东西就不会产生理由。
	if len(semantic) == 0 && len(explore) == 0 {
		return nil
	}
	b := f.newReasonBuilder(ctx, videos, viewerAccountID, semantic, explore)
	if b == nil {
		return nil
	}
	b.build(videos, semantic)

	out := make(map[uint]string, len(b.reasons))
	for id, reason := range b.reasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		out[id] = reason
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// 编译期断言：GORM 实现必须满足 ReasonSignalLookup（表的所有者是 video 包）。
var _ ReasonSignalLookup = (*video.VideoRepository)(nil)
