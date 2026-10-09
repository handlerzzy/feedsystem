package evalset

import (
	"fmt"
	"sort"
	"strings"
)

// RenderText 生成终端输出。
//
// 排版目标：一眼能看出"哪个排序更好""这组数字能不能信"，以及
// **该看哪一张表**。最后一条是 B1 之后新增的：报告里现在有两套口径，
// 而"拿全局热度口径的数字去论证个性化推荐效果"正是前置条件文件存在的原因。
// 因此两张表分别加了标题，且全局那张明确标注"不能用于评估 P2"。
func RenderText(res *EvalResult) string {
	var b strings.Builder

	b.WriteString("\n=== 离线排序评测 ===\n")
	if IsSynthetic(res.Source) {
		// 合成数据的数字会看起来"很漂亮"（标签就是按规则造的），
		// 必须在最显眼的位置说清楚，避免被当成线上基线引用。
		b.WriteString("\n!!!!! 合成数据：本结果只用于验证评测流水线，不能作为线上基线或对外结论 !!!!!\n")
		b.WriteString("      真实基线请用 cmd/evalbaseline 从真实互动数据导出。\n")
	}

	b.WriteString("\n--- 按用户分组口径（P2 的结论只能用这一套）---\n")
	b.WriteString(renderPerUserText(res))

	b.WriteString("\n--- 全局热度口径（仅历史参考，不能用于评估 P2）---\n")
	b.WriteString("口径含义：切分点之后有哪些视频被**任何人**互动过。\n")
	b.WriteString("它没有用户维度，衡量的是全局热度预测，与「给某个用户推对了多少」无关。\n\n")
	fmt.Fprintf(&b, "时间切分点 : %s\n", res.SplitAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "候选集     : %d 条视频\n", res.Candidates)
	fmt.Fprintf(&b, "相关样本   : %d 条（Recall 的分母）\n", res.Relevant)
	fmt.Fprintf(&b, "观测到的互动: %d 条\n", res.HeldOutInteractions)

	if len(res.Warnings) > 0 {
		b.WriteString("\n⚠ 这组数字不可作为结论：\n")
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
		b.WriteString("  （流程是通的，指标算法也已经过单测；等数据量上来后重跑即可得到有意义的基线）\n")
	}

	b.WriteString("\n")
	b.WriteString(renderRankerTableText(res.Rankers))
	b.WriteString("\nR@K = Recall@K；N@K = NDCG@K（指数增益、log2 折扣，与 sklearn 口径一致）\n")
	b.WriteString(renderRankerParamsText(res))
	return b.String()
}

// renderRankerParamsText 打印各策略自报的参数。
//
// 为什么终端输出里也要有它：一份"数字变了"的报告，第一件要确认的事是
// "参数变了没有"。把参数放在指标下面，读的人不需要再去翻 JSON。
func renderRankerParamsText(res *EvalResult) string {
	if len(res.RankerParams) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n--- 排序策略参数（P2 §4.7 要求结果含参数）---\n")
	names := make([]string, 0, len(res.RankerParams))
	for name := range res.RankerParams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "\n[%s]\n", name)
		params := res.RankerParams[name]
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %-24s %v\n", k, params[k])
		}
	}
	return b.String()
}

// renderPerUserText 渲染按用户分组的段落。
//
// 单独抽出来是因为它有"数据里没有用户维度"这一条**必须显式说出来**的分支：
// P2 的一切结论都依赖这套口径，静默少打一张表会让人以为"没有就是没问题"。
func renderPerUserText(res *EvalResult) string {
	var b strings.Builder

	if res.PerUser == nil {
		b.WriteString("⚠ 这份快照里没有任何带 account_id 的互动，**无法做按用户分组的评测**。\n")
		b.WriteString("  也就是说：这份数据不能用来评估语义召回/精排是否让推荐变好。\n")
		b.WriteString("  请用新版 cmd/evalbaseline 重新导出（它会带上互动者账号），\n")
		b.WriteString("  或使用 cmd/evalseed 生成的带话题结构评测集。\n")
		return b.String()
	}

	pu := res.PerUser
	fmt.Fprintf(&b, "用户数     : %d（其中 %d 个参与评测", pu.Users, pu.EvaluatedUsers)
	if pu.SkippedNoRelevant > 0 {
		fmt.Fprintf(&b, "，%d 个因观测窗口内无相关候选样本被剔除", pu.SkippedNoRelevant)
	}
	b.WriteString("）\n")
	fmt.Fprintf(&b, "每用户候选集: min=%d p50=%d p90=%d max=%d 平均=%.1f\n",
		pu.CandidatesPerUser.Min, pu.CandidatesPerUser.P50, pu.CandidatesPerUser.P90,
		pu.CandidatesPerUser.Max, pu.CandidatesPerUser.Mean)
	fmt.Fprintf(&b, "每用户相关样本: min=%d p50=%d p90=%d max=%d 平均=%.1f（低于阈值 %d 的用户 %d 个）\n",
		pu.RelevantPerUser.Min, pu.RelevantPerUser.P50, pu.RelevantPerUser.P90,
		pu.RelevantPerUser.Max, pu.RelevantPerUser.Mean, minRelevantPerUser, pu.UsersBelowMinRelevant)

	if len(pu.Warnings) > 0 {
		b.WriteString("\n⚠ 按用户口径还不足以支撑结论：\n")
		for _, w := range pu.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	for _, n := range pu.Notes {
		fmt.Fprintf(&b, "ℹ %s\n", n)
	}

	b.WriteString("\n")
	b.WriteString(renderRankerTableText(pu.Rankers))
	b.WriteString("（以上为**每用户算指标再对用户取平均**的结果）\n")
	return b.String()
}

// renderRankerTableText 渲染指标表；没有策略时给一行说明而不是 panic。
//
// 不做越界索引：这个函数会被命令行直接调用，一次 panic 会把
// "数据为空"的问题伪装成程序 bug，排查方向直接被带偏。
func renderRankerTableText(rankers []RankerResult) string {
	if len(rankers) == 0 {
		return "（没有可评测的排序策略）\n"
	}
	var b strings.Builder
	header := "排序策略"
	for _, kr := range rankers[0].Results {
		header += fmt.Sprintf("  R@%-2d  N@%-2d", kr.K, kr.K)
	}
	b.WriteString(header + "\n")
	b.WriteString(strings.Repeat("-", len(header)+8) + "\n")
	for _, r := range rankers {
		line := fmt.Sprintf("%-10s", r.Name)
		for _, kr := range r.Results {
			line += fmt.Sprintf("  %.3f %.3f", kr.Recall, kr.NDCG)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// RenderMarkdown 生成可提交、可贴进文档的摘要。
//
// 它把"数据集规模"与"可靠性"写在最前面：报告一旦脱离上下文流传，
// 最容易被误读的就是"Recall@10 = 0.000"这种在小样本上必然出现的数字。
func RenderMarkdown(res *EvalResult, snap *Snapshot) string {
	var b strings.Builder

	b.WriteString("# 离线排序评测报告\n\n")
	producer := res.Producer
	if producer == "" {
		producer = "backend/cmd/evalbaseline"
	}
	fmt.Fprintf(&b, "> 由 `%s` 生成。**请勿手工编辑**：改动请重跑命令。\n\n", producer)
	if IsSynthetic(res.Source) {
		// 合成数据的标签是我们自己按规则造的，三个策略的分数会异常漂亮。
		// 报告一旦脱离上下文流传，最容易被误引用的就是这一张表，所以在标题下方说清楚。
		b.WriteString("> [!WARNING]\n")
		b.WriteString("> **这是合成数据，不是线上基线。** 标签由 `cmd/evalseed` 按规则生成，\n")
		b.WriteString("> 它用于验证「语义召回能不能跑赢热度排序」这件事可以被度量，\n")
		b.WriteString("> **不可**作为简历数字或线上性能结论。\n")
		b.WriteString("> 真实基线请对真实互动数据跑 `cmd/evalbaseline`（见 `testdata/eval/README.md`）。\n\n")
	}

	b.WriteString("## 评测口径\n\n")
	b.WriteString("| 项 | 值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 数据来源 | `%s` |\n", snap.Source)
	fmt.Fprintf(&b, "| 快照生成时间 | %s |\n", snap.GeneratedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "| 时间切分点 | %s（按发布时间 %.0f%% 分位） |\n",
		res.SplitAt.Format("2006-01-02 15:04:05"), snap.SplitRatio*100)
	if snap.FeatureNote != "" {
		fmt.Fprintf(&b, "| 排序特征口径 | %s |\n", snap.FeatureNote)
	}
	fmt.Fprintf(&b, "| 候选集 | %d 条视频（切分点之前发布） |\n", res.Candidates)
	fmt.Fprintf(&b, "| 相关样本 | %d 条（切分点之后被点赞或评论过的候选视频） |\n", res.Relevant)
	fmt.Fprintf(&b, "| 观测互动 | %d 条 |\n", res.HeldOutInteractions)
	b.WriteString("\n")
	b.WriteString("**不使用未来信息**：候选集只包含切分点之前发布的视频，\n")
	b.WriteString("相关性标签只取切分点**之后**发生的点赞/评论；\n")
	b.WriteString("排序特征（likes_count / popularity）同样只由切分点**之前**的互动推算——\n")
	b.WriteString("`videos` 表里的这两列是持续累加的，直接取导出值就等于用答案排序。\n\n")
	b.WriteString("**相关性等级**：点赞 = 2（成本更高、信号更强），评论 = 1，无互动 = 0。\n")
	b.WriteString("同一视频同时被点赞与评论时取点赞，不相加——相加会让单条热门视频主导理想排序。\n\n")
	b.WriteString("**指标**：Recall@K 与 NDCG@K。NDCG 使用指数增益 `2^rel - 1` 与折扣 `log2(i+2)`，\n")
	b.WriteString("与 `sklearn.metrics.ndcg_score` 的默认口径一致，便于外部复核。\n\n")

	b.WriteString("## 按用户分组口径（**P2 的结论只能用这一套**）\n\n")
	b.WriteString(renderPerUserMarkdown(res))

	if len(res.RankerParams) > 0 {
		b.WriteString("### 排序策略参数\n\n")
		b.WriteString("结论必须与参数一起引用：换了配额、候选窗口或 embedding 模型，\n")
		b.WriteString("同一份数据上的数字就会变。\n\n")
		names := make([]string, 0, len(res.RankerParams))
		for name := range res.RankerParams {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "#### `%s`\n\n", name)
			b.WriteString("| 参数 | 值 |\n|---|---|\n")
			params := res.RankerParams[name]
			keys := make([]string, 0, len(params))
			for k := range params {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "| `%s` | %v |\n", k, params[k])
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("## 全局热度口径（仅历史参考，**不能**用于评估 P2）\n\n")
	b.WriteString("这张表衡量的是「切分点之后哪些视频被**任何人**互动过」，即全局热度预测。\n")
	b.WriteString("它没有用户维度，因此**无法**回答「语义召回有没有让某个用户看到更相关的内容」。\n")
	b.WriteString("保留它只是为了与 P0 时期的历史数字对得上。\n\n")
	if len(res.Warnings) > 0 {
		b.WriteString("> [!NOTE]\n")
		b.WriteString("> 数据规模不足以支撑结论：\n")
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "> - %s\n", w)
		}
		b.WriteString("\n")
	}
	b.WriteString(renderRankerTableMarkdown(res.Rankers))
	b.WriteString("\n")

	b.WriteString("## 每个全局排序策略对应的线上实现\n\n")
	b.WriteString("| 策略 | 线上 SQL（internal/feed/repo.go） |\n|---|---|\n")
	b.WriteString("| `timeline` | `ListLatest`: `ORDER BY create_time DESC` |\n")
	b.WriteString("| `popularity` | `ListByPopularity`: `ORDER BY popularity DESC, create_time DESC, id DESC` |\n")
	b.WriteString("| `likes_count` | `ListLikesCountWithCursor`: `ORDER BY likes_count DESC, id DESC` |\n\n")
	b.WriteString("排序规则必须与线上逐字一致——基线数字的意义就是「与线上可比」，\n")
	b.WriteString("口径不同的话，P2 的精排再准也无法与它比较。\n\n")

	b.WriteString("## 如何重跑\n\n")
	b.WriteString("```bash\n")
	b.WriteString("cd backend\n")
	b.WriteString("# 用冻结的快照复现（不需要数据库，建议用于核对历史数字）\n")
	b.WriteString("go run ./cmd/evalbaseline -snapshot testdata/eval/snapshot.json\n\n")
	b.WriteString("# 重新从 MySQL 导出（会覆盖快照，数字随之变化）\n")
	b.WriteString("go run ./cmd/evalbaseline \\\n")
	b.WriteString("  -dsn 'root:<password>@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \\\n")
	b.WriteString("  -snapshot-out testdata/eval/snapshot.json \\\n")
	b.WriteString("  -json testdata/eval/baseline.json \\\n")
	b.WriteString("  -markdown testdata/eval/baseline.md\n")
	b.WriteString("```\n\n")
	b.WriteString("指标算法与切分规则的单测在 `internal/evalset/evalset_test.go`，\n")
	b.WriteString("它用手算过的期望值钉住了 Recall@K / NDCG@K 的语义——\n")
	b.WriteString("指标算错会让后续所有比较失去意义，所以这部分必须有独立验证。\n")
	b.WriteString("按用户分组口径的单测在 `internal/evalset/peruser_test.go`。\n")

	return b.String()
}

func renderPerUserMarkdown(res *EvalResult) string {
	var b strings.Builder
	if res.PerUser == nil {
		b.WriteString("> [!WARNING]\n")
		b.WriteString("> **这份快照没有用户维度，无法评估 P2。**\n")
		b.WriteString("> 快照里没有任何带 `account_id` 的互动，因此只能算全局热度口径。\n")
		b.WriteString("> 请用新版 `cmd/evalbaseline` 重新导出，或使用 `cmd/evalseed` 生成的评测集。\n\n")
		return b.String()
	}

	pu := res.PerUser
	b.WriteString("| 项 | 值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 用户数 | %d |\n", pu.Users)
	fmt.Fprintf(&b, "| 参与评测的用户数 | %d |\n", pu.EvaluatedUsers)
	fmt.Fprintf(&b, "| 无相关候选样本而剔除的用户数 | %d |\n", pu.SkippedNoRelevant)
	fmt.Fprintf(&b, "| 每用户候选集（min/p50/p90/max） | %d / %d / %d / %d |\n",
		pu.CandidatesPerUser.Min, pu.CandidatesPerUser.P50, pu.CandidatesPerUser.P90, pu.CandidatesPerUser.Max)
	fmt.Fprintf(&b, "| 每用户相关样本（min/p50/p90/max） | %d / %d / %d / %d |\n",
		pu.RelevantPerUser.Min, pu.RelevantPerUser.P50, pu.RelevantPerUser.P90, pu.RelevantPerUser.Max)
	fmt.Fprintf(&b, "| 相关样本少于 %d 条的用户数 | %d |\n", minRelevantPerUser, pu.UsersBelowMinRelevant)
	fmt.Fprintf(&b, "| 是否可支撑结论 | %s |\n", yesNo(pu.Reliable))
	b.WriteString("\n")
	b.WriteString("**每个用户的候选集是他自己可见的、切分点之前发布的视频**（排除自己发布的），\n")
	b.WriteString("**相关性只来自该用户自己**在切分点之后的点赞/评论；\n")
	b.WriteString("指标先在每个用户内部算一遍，再对用户取平均。\n\n")
	if len(pu.Warnings) > 0 {
		b.WriteString("> [!WARNING]\n")
		b.WriteString("> 按用户口径还不足以支撑结论：\n")
		for _, w := range pu.Warnings {
			fmt.Fprintf(&b, "> - %s\n", w)
		}
		b.WriteString("\n")
	}
	for _, n := range pu.Notes {
		fmt.Fprintf(&b, "> ℹ %s\n\n", n)
	}
	b.WriteString(renderRankerTableMarkdown(pu.Rankers))
	b.WriteString("\n")
	return b.String()
}

func renderRankerTableMarkdown(rankers []RankerResult) string {
	if len(rankers) == 0 {
		// 没有排序器时不 panic：这个函数会被命令直接调用，
		// 越界崩溃会把一条"数据为空"的问题伪装成程序 bug。
		return "（没有可评测的排序策略）\n"
	}
	var b strings.Builder
	b.WriteString("| 排序策略 |")
	for _, kr := range rankers[0].Results {
		fmt.Fprintf(&b, " Recall@%d | NDCG@%d |", kr.K, kr.K)
	}
	b.WriteString("\n|---|")
	for range rankers[0].Results {
		b.WriteString("---|---|")
	}
	b.WriteString("\n")
	for _, r := range rankers {
		fmt.Fprintf(&b, "| `%s` |", r.Name)
		for _, kr := range r.Results {
			fmt.Fprintf(&b, " %.4f | %.4f |", kr.Recall, kr.NDCG)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}
