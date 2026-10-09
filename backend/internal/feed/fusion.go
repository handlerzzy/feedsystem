package feed

import (
	"context"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/rank"
	"github.com/handlerzzy/feedsystem/internal/video"

	"go.uber.org/zap"
)

// 本文件实现 P2 §4.3「多路融合」与 §4.5「冷启动」。
//
// 融合方式是**按配额分配槽位**，不是按分数加权 —— 这是 P2 前置 D3 的决定，
// 理由是"加权时配额是软的"：热度分数一旦量级更大，语义路就被淹没了，
// 而"配额"这个词也就没有意义了。槽位分配让下面这条性质**可验证**：
//
//	recall_quota = 0 且 explore_quota = 0 时，输出与基准线逐字节一致。
//
// 它成立的机制是：配额为 0 时 QuotaPlan 是零值，融合函数不碰任何一路、
// 不做任何替换，直接返回基础页的前 limit 条 —— 也就是基准线本身。
// 这条性质不依赖"两边的排序恰好一致"这种脆弱假设。

// 融合相关的常量。
const (
	// FusionPoolSize 是三路融合后的候选池大小（P2 前置 D4：200 条）。
	//
	// 它的作用域只有一个：把返回列表里"配额位"的**替换来源**限制在一个
	// 有界的集合里。没有它，一次请求可能要为一个语义位去翻几百条候选，
	// 而 D1 的预算是 50ms。
	//
	// 取 200 的依据（D4）：200 × 1536 维线性扫描 ≈ 0.15 MFLOP，
	// 在现代 CPU 上是亚毫秒级，因此"配额位需要更大候选池"时不至于
	// 先把预算吃光。
	FusionPoolSize = 200

	// ExploreMaxCandidates 是一次探索召回最多取多少条候选。
	// 只需覆盖 explore_quota × limit 个位置（默认 10 × 0.1 = 1），
	// 留 4 倍余量应对"候选被去重掉"的情况。
	ExploreMaxCandidates = 40

	// ExploreFreshness 是"新内容"的时间窗（P2 前置 D2：24 小时）。
	//
	// 为什么是 24 小时而不是"最近 N 条"：探索要解决的是"内容发出来
	// 没人看得到"，这是一个**时间**问题（创作者当天看不到反馈就流失了）。
	// 按条数取会让一个发布量大的站把窗口压到几分钟，
	// 反而把最需要曝光的内容挤出窗口。
	ExploreFreshness = 24 * time.Hour

	// ExploreMaxPopularity 是"互动很少"的判定阈值（D2：互动数 < 5）。
	//
	// 取 5 而不是 0：完全零互动的判定会把"刚获得第一个赞"的内容也排掉，
	// 而那条内容同样需要曝光。阈值太高则探索位会被普通内容占满，
	// 挤掉相关性（用户会觉得"推荐变差了"）。
	ExploreMaxPopularity = 5
)

// QuotaPlan 与槽位几何定义在 internal/rank：P2 的离线评测必须与线上
// 用同一份配额规则，否则"离线说更好"只证明了评测里那个近似实现更好。
// 这里做一层别名，让本包内部的代码读起来短一些。
type QuotaPlan = rank.QuotaPlan

// buildQuotaPlan 是 rank.Plan 的薄包装（保留本包内的调用点名字）。
func buildQuotaPlan(limit int, recallQuota, exploreQuota float64) QuotaPlan {
	return rank.Plan(limit, recallQuota, exploreQuota)
}

// fuseInput 是一次融合的全部输入。
type fuseInput struct {
	// Base 是基础页（时序路），已经按时间倒序。
	Base []*video.Video
	// Extras 是基础页之外**更旧**的候选（融合池的剩余部分），
	// 只在基础页不够填满列表时使用。它们不参与配额位。
	Extras []*video.Video
	// Semantic / Explore 是两路召回的候选，都已按各自的优先级排好。
	Semantic []SemanticHit
	Explore  []*video.Video
	// Plan 是槽位计划。
	Plan QuotaPlan
}

// fuseChannels 按槽位计划把各路候选合成最终列表。
//
// 返回的 second 是最终列表里**基础页**的最旧一条时间，调用方用它当游标
// （理由见下面的长注释）。基础页没有进入最终列表时返回零值。
//
// 三条不变量（每一条都对应一个可验证的性质）：
//
//  1. **配额为 0 时输出就是 Base 的前 limit 条**。没有分支会碰它：
//     计划是零值时 picks 为空，填充循环走的全是 base 通道。
//     这正是"逐字节一致"那条验收项的实现方式。
//  2. **去重优先于填满配额位**。同一路自己的候选可能重复
//     （不同来源的投票、或 JOIN 的重复行），跨路重复更常见
//     （语义召回很容易命中基础页里的内容）。重复的视频在列表里出现两次
//     是**用户可见**的错误，而少一个语义位只是效果差一点。
//  3. **配额位不改变游标**。游标取范围里最旧的一条基础页，而不是最旧的
//     一条结果——语义召回**按定义**会捞到比基础页更旧的内容
//     （那正是它存在的理由），若游标跟着它走，翻页会跳过基础页里
//     介于两者之间的视频。这是"在既有分页契约上加一路召回"的固有代价，
//     取舍与后果写在 README 的已知限制里。
func fuseChannels(in fuseInput) (final []*video.Video, second time.Time) {
	limit := len(in.Base)
	if limit == 0 {
		return nil, time.Time{}
	}

	seen := make(map[uint]bool, len(in.Base)+len(in.Semantic)+len(in.Explore))
	// 先把基础页标记为已用：配额位必须避开它们，否则预览一个已经在
	// 列表里的视频，用户会看到两条一模一样的卡片。
	for _, v := range in.Base {
		seen[v.ID] = true
	}

	// picks 是槽位表：picks[i] 表示最终列表第 i 位由哪一路提供。
	// 几何定义在 internal/rank（线上与离线共用同一个函数）。
	picks := rank.Slots(limit, in.Plan)

	semanticIdx, exploreIdx, baseIdx, extraIdx := 0, 0, 0, 0
	final = make([]*video.Video, 0, limit)
	// oldestBase 在**装配过程中**记录，而不是装配完再回查：
	// 回查需要判断"这条是否属于基础页"，而基础页里可能有一条比所有
	// 配额位内容都更旧、却没被放进最终列表（列表被截断了）。
	// 那样回查出来的游标会比实际展示的最旧内容更旧，翻页会漏掉视频。
	var oldestBase time.Time
	for i := 0; i < limit; i++ {
		switch picks[i] {
		case rank.Semantic:
			if v := nextUnseenHit(in.Semantic, &semanticIdx, seen); v != nil {
				final = append(final, v)
				continue
			}
			// 语义候选不够/全被去重：这一位退回基础页，
			// 而不是留空或强行塞一条重复内容。
		case rank.Explore:
			if v := nextUnseenVideo(in.Explore, &exploreIdx, seen); v != nil {
				final = append(final, v)
				continue
			}
		}
		// 基础页通道。
		//
		// 这里**不查 seen**：基础页的 ID 在开始时被标记为已用，
		// 是为了让配额位避开它们；它们本身永远是"未被用过"的那一批。
		// 首批循环结束后也不会再有别的路把基础页视频标记进来
		// （配额位只标记从 Semantic/Explore 取走的 ID）。
		if baseIdx < len(in.Base) {
			v := in.Base[baseIdx]
			baseIdx++
			final = append(final, v)
			if oldestBase.IsZero() || v.CreateTime.Before(oldestBase) {
				oldestBase = v.CreateTime
			}
			continue
		}
		if extraIdx < len(in.Extras) {
			final = append(final, in.Extras[extraIdx])
			extraIdx++
		}
	}

	return final, oldestBase
}

// nextUnseenHit 从语义候选里取下一条未被去重的结果。
func nextUnseenHit(hits []SemanticHit, idx *int, seen map[uint]bool) *video.Video {
	for *idx < len(hits) {
		h := hits[*idx]
		*idx++
		if h.Video == nil || seen[h.Video.ID] {
			continue
		}
		seen[h.Video.ID] = true
		return h.Video
	}
	return nil
}

// nextUnseenVideo 从探索候选里取下一条未被去重的结果。
func nextUnseenVideo(videos []*video.Video, idx *int, seen map[uint]bool) *video.Video {
	for *idx < len(videos) {
		v := videos[*idx]
		*idx++
		if v == nil || seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		return v
	}
	return nil
}

// exploreRecall 返回冷启动探索的候选（新的且互动很少的内容）。
//
// 与语义路一样，任何失败都只记日志并返回 nil：
// **"新内容永不曝光"是事故，"这次没有探索内容"只是少一个位**。
// 返回错误会让一次可选查询的失败变成 Feed 的失败——那正是把
// 锦上添花变成承重墙。
//
// excludeAuthorID 传当前用户：推荐流不会把自己发的内容推给自己。
// 这不是隐私问题（自己的视频本来就公开），而是产品问题——
// 首屏出现自己的内容会让"推荐"立刻露馅。
func (f *FeedService) exploreRecall(ctx context.Context, viewerAccountID uint, quota QuotaPlan) []*video.Video {
	if quota.Explore <= 0 {
		return nil
	}
	fresh, err := f.repo.ListFreshLowEngagement(ctx, ExploreMaxCandidates, ExploreFreshness, ExploreMaxPopularity, viewerAccountID)
	if err != nil {
		logging.Ctx(ctx).Info("探索通道查询失败，本次不做冷启动兜底（不影响 Feed 结果）",
			zap.Duration("freshness", ExploreFreshness), zap.Error(err))
		return nil
	}
	return fresh
}
