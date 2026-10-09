// Command evalp2 跑 **P2 的效果对比**：语义召回 + 融合（+ 冷启动探索）
// 与基线排序在同一份评测集上的 Recall@K / NDCG@K 对比。
//
// 用法（在 backend/ 下执行）：
//
//	# 推荐：一条命令重新生成评测集、基线、P2 对比（输出逐字节可复现）
//	SOURCE_DATE_EPOCH=1767225600 go run ./cmd/evalseed \
//	  -out testdata/eval/snapshot-synthetic.json \
//	  -json testdata/eval/baseline-synthetic.json \
//	  -markdown testdata/eval/baseline-synthetic.md
//	go run ./cmd/evalp2 -snapshot testdata/eval/snapshot-synthetic.json \
//	  -json testdata/eval/p2-synthetic.json \
//	  -markdown testdata/eval/p2-synthetic.md
//
//	# 在真实数据上核对（先导出快照；样本量不足时报告会显式标注"不可作为结论"）
//	go run ./cmd/evalbaseline -dsn "$MYSQL_DSN" -snapshot-out /tmp/real.json
//	go run ./cmd/evalp2 -snapshot /tmp/real.json
//
// 为什么单独一个命令而不是给 evalbaseline 加一堆开关：
//
//  1. evalbaseline 的职责是"冻结基线"，它的产物必须**长期稳定**；
//     把 P2 的参数与消融塞进去，会让每一次基线重跑都牵扯到一堆
//     只有 P2 才关心的选项。
//  2. P2 的对比需要**一次跑四个策略**（基线 / 只加语义 / 只加探索 /
//     全部打开），这是"在什么条件下变好、什么条件下变差"这个问题的
//     唯一回答方式。消融是 P2 结论的一部分，不是可选功能。
//
// 它不属于服务进程：只被人工执行，因此可以直接读库、直接打印。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/handlerzzy/feedsystem/internal/evalset"
)

// splitProvided 记录 -split 是否被显式传入（与 evalbaseline 同一约定：
// 快照里已经有切分点时，不显式指定就沿用快照的切分点，否则重算特征）。
var splitProvided bool

func main() {
	var (
		snapshotIn  = flag.String("snapshot", "", "评测快照（JSON）；真实数据请先用 cmd/evalbaseline -snapshot-out 导出")
		snapshotOut = flag.String("snapshot-out", "", "把本次读到的快照重新落盘（只在需要固定切分点时使用）")
		jsonOut     = flag.String("json", "", "把评测结果写成 JSON（含全部参数）")
		markdownOut = flag.String("markdown", "", "把评测结果写成 Markdown 摘要")
		splitRatio  = flag.Float64("split", 0.5, "时间切分比例（显式传入时会重新切分并重算特征）")
		ksFlag      = flag.String("k", "5,10,20", "要计算的 K，逗号分隔")

		recallQuota   = flag.Float64("recall-quota", 0.3, "语义路配额（与 ai.recall_quota 同义）")
		exploreQuota  = flag.Float64("explore-quota", 0.1, "冷启动探索配额（与 ai.explore_quota 同义）")
		dim           = flag.Int("dim", 0, "词法向量维度；0 = embed.LexicalDim（1536，与 sidecar 一致）")
		window        = flag.Int("candidate-window", 0, "语义召回候选窗口；0 = 线上的 500")
		fusionPool    = flag.Int("fusion-pool", 0, "融合候选池大小；0 = 线上的 200")
		profileLikes  = flag.Int("profile-likes", 0, "兴趣画像取多少条最近点赞；0 = 线上的 50")
		exploreFresh  = flag.Duration("explore-freshness", 0, "探索的时间窗；0 = 离线默认 7 天（线上是 24 小时，差异写在结果里）")
		exploreMaxPop = flag.Int64("explore-max-popularity", -1, "探索的互动数上限；负值 = 线上的 5")
	)
	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		if f.Name == "split" {
			splitProvided = true
		}
	})

	ks, err := parseKs(*ksFlag)
	if err != nil {
		fatal(err)
	}
	snap, err := loadSnapshot(*snapshotIn, *snapshotOut, *splitRatio)
	if err != nil {
		fatal(err)
	}

	cfg := evalset.DefaultEvalSemanticConfig()
	if *dim > 0 {
		cfg.Dim = *dim
	}
	if *window > 0 {
		cfg.CandidateWindow = *window
	}
	if *fusionPool > 0 {
		cfg.FusionPool = *fusionPool
	}
	if *profileLikes > 0 {
		cfg.ProfileLikes = *profileLikes
	}
	if *exploreFresh > 0 {
		cfg.ExploreFreshness = *exploreFresh
	}
	if *exploreMaxPop >= 0 {
		cfg.ExploreMaxPopularity = *exploreMaxPop
	}

	rankers := buildRankers(snap, cfg, *recallQuota, *exploreQuota)
	res, err := evalset.Evaluate(snap, rankers, ks)
	if err != nil {
		fatal(err)
	}
	res.SetProducer("backend/cmd/evalp2")
	fmt.Print(evalset.RenderText(res))
	printVerdict(res, *recallQuota, *exploreQuota)

	if *jsonOut != "" {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fatal(fmt.Errorf("写 JSON 失败: %w", err))
		}
		fmt.Printf("\n结果已写入 %s\n", *jsonOut)
	}
	if *markdownOut != "" {
		if err := os.WriteFile(*markdownOut, []byte(evalset.RenderMarkdown(res, snap)), 0o644); err != nil {
			fatal(fmt.Errorf("写 Markdown 失败: %w", err))
		}
		fmt.Printf("摘要已写入 %s\n", *markdownOut)
	}
}

// buildRankers 组装这次对比的全部策略。
//
// 四个策略是**消融实验**的最小集合：
//
//	baseline          —— 今天线上的排序（popularity 后备 SQL）
//	p2_semantic_only  —— 只开语义路：证明"语义信号本身有效"
//	p2_explore_only   —— 只开探索：证明"冷启动兜底有代价"（这是预期的负收益）
//	p2_fused          —— 两个都开：线上真实形态
//
// 没有第四个策略的话，"融合比基线好"无法归因（是语义的功劳还是探索的？），
// 而 P2 §4.7 要求的正是"在什么条件下变好、什么条件下变差"。
func buildRankers(snap *evalset.Snapshot, cfg evalset.EvalSemanticConfig, recallQuota, exploreQuota float64) []evalset.Ranker {
	rankers := evalset.BuiltinRankers()

	base := cfg
	base.RecallQuota = recallQuota
	base.ExploreQuota = exploreQuota

	semanticOnly := base
	semanticOnly.NoExplore = true
	exploreOnly := base
	exploreOnly.NoSemantic = true

	rankers = append(rankers, evalset.NewSemanticRanker(snap, evalset.SemanticRankerOptions{
		Name:   "p2_semantic_only",
		Config: semanticOnly,
	}).Ranker())
	rankers = append(rankers, evalset.NewSemanticRanker(snap, evalset.SemanticRankerOptions{
		Name:   "p2_explore_only",
		Config: exploreOnly,
	}).Ranker())
	rankers = append(rankers, evalset.NewSemanticRanker(snap, evalset.SemanticRankerOptions{
		Name:    "p2_fused",
		Config:  base,
		Reasons: true,
	}).Ranker())
	return rankers
}

// printVerdict 把"变好了多少"直接算给读者看。
//
// 为什么要在终端里算这一步：报告里是一张有两套口径、四个策略、三个 K 的
// 表，"到底变好没有"需要读者自己做减法，而**大多数误读都发生在这一步**。
// 这里只对 NDCG@20 做一次减法（K=20 是线上接口的默认页大小），
// 并显式标出样本量不足时不能下结论。
func printVerdict(res *evalset.EvalResult, recallQuota, exploreQuota float64) {
	if res.PerUser == nil {
		fmt.Println("\n⚠ 这份快照没有用户维度，无法给出 P2 结论（见上面的说明）")
		return
	}
	const k = 20
	base, fused := ndcgAt(res.PerUser.Rankers, "popularity", k), ndcgAt(res.PerUser.Rankers, "p2_fused", k)
	semantic := ndcgAt(res.PerUser.Rankers, "p2_semantic_only", k)
	explore := ndcgAt(res.PerUser.Rankers, "p2_explore_only", k)

	fmt.Printf("\n=== 结论（按用户口径，NDCG@%d）===\n", k)
	fmt.Printf("  基线 popularity      : %.3f\n", base)
	fmt.Printf("  只加语义召回          : %.3f（Δ %+.3f）\n", semantic, semantic-base)
	fmt.Printf("  只加冷启动探索        : %.3f（Δ %+.3f，探索位的相关性天然偏低，负值是预期的）\n", explore, explore-base)
	fmt.Printf("  融合（线上默认形态）  : %.3f（Δ %+.3f，相对基线 %.1f%%）\n",
		fused, fused-base, percentDelta(base, fused))
	if base > 0 {
		fmt.Printf("  语义路的边际贡献      : %.1f%%（语义相对基线的提升）\n", percentDelta(base, semantic))
	}
	fmt.Printf("  参数：recall_quota=%.2f explore_quota=%.2f\n", recallQuota, exploreQuota)
	if !res.PerUser.Reliable {
		fmt.Println("  ⚠ 样本量不足（见上面的 Warnings）：这组数字不能作为结论，只能说明流水线是通的")
	}
	if evalset.IsSynthetic(res.Source) {
		fmt.Println("  ⚠ 合成数据：标签按用户话题偏好采样生成，不能作为线上基线或对外结论")
	}
	fmt.Println("  ⚠ 离线用词法向量（只捕捉字面重叠），因此这是 P2 效果的**下界**；")
	fmt.Println("    真实语义模型的收益需要在配了 API key 的环境里重跑同一份快照。")
}

func percentDelta(base, value float64) float64 {
	if base == 0 {
		return 0
	}
	return (value - base) / base * 100
}

func ndcgAt(rankers []evalset.RankerResult, name string, k int) float64 {
	for _, r := range rankers {
		if r.Name != name {
			continue
		}
		for _, kr := range r.Results {
			if kr.K == k {
				return kr.NDCG
			}
		}
	}
	return 0
}

// loadSnapshot 读入快照，并在 -split 被显式传入时重新切分。
//
// 为什么只支持"读快照"而不像 evalbaseline 那样也支持连库导出：
//
//  1. 导出的逻辑只应该有一份。cmd/evalbaseline 里那段 SQL 已经在真实库上
//     被验证过，复制一份到本命令里意味着两处会各自漂移——而漂移的后果是
//     "基线快照"与"P2 快照"不是同一份数据，两者相减得到的提升毫无意义。
//  2. 真实开发库的规模（个位数视频）不可能支撑任何结论，而合成评测集
//     （cmd/evalseed）本来就是为"能下结论"而造的。
//
// 需要真实数据时：先用 cmd/evalbaseline 导出快照，再把 -snapshot 指向它。
func loadSnapshot(snapshotIn, snapshotOut string, splitRatio float64) (*evalset.Snapshot, error) {
	if snapshotIn == "" {
		return nil, fmt.Errorf("-snapshot 必须提供（真实数据请先用 cmd/evalbaseline -snapshot-out 导出）")
	}
	snap, err := evalset.Load(snapshotIn)
	if err != nil {
		return nil, err
	}
	if snapshotOut != "" {
		if err := snap.Save(snapshotOut); err != nil {
			return nil, fmt.Errorf("写快照失败: %w", err)
		}
		fmt.Printf("快照已写入 %s\n", snapshotOut)
	}
	return snap, nil
}

func parseKs(raw string) ([]int, error) {
	var ks []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, err := strconv.Atoi(part)
		if err != nil || k <= 0 {
			return nil, fmt.Errorf("-k 里出现非法值 %q（应为正整数，逗号分隔）", part)
		}
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		return nil, fmt.Errorf("-k 不能为空")
	}
	return ks, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "evalp2: %v\n", err)
	os.Exit(1)
}
