// Command evalseed 生成**带话题结构**的合成评测集（P2 前置项 B2）。
//
// 用法（在 backend/ 下执行）：
//
//	# 一次命令重新生成评测集与基线（推荐；输出逐字节可复现）
//	SOURCE_DATE_EPOCH=1767225600 go run ./cmd/evalseed \
//	  -out testdata/eval/snapshot-synthetic.json \
//	  -json testdata/eval/baseline-synthetic.json \
//	  -markdown testdata/eval/baseline-synthetic.md \
//	  -lexical-probe
//
// 它同时把基线跑出来，而不是"生成数据"与"跑基线"分成两条命令：
// 分开之后最常见的结果是快照被重新生成、而基线文件还是旧的，
// 于是报告里的数字与快照对不上，却没有任何地方会报错。
//
// 为什么用 SOURCE_DATE_EPOCH 而不是自带一个 -now 参数：
// 这是可复现构建领域的通用约定（工具链、发行版打包都在用）。
// 快照里必须记录生成时间（前置条件要求"结果文件含时间"），
// 而"逐字节一致"又要求它固定——这两件事只能靠固定时间源来同时满足。
// 不设置时用当前时间，也就是说：默认行为对人不意外，复现靠显式固定。
//
// **它生成的数字不能作为线上基线或简历数字**：标签是按用户偏好采样造的。
// 报告里会显式标注这一点（见 evalset.IsSynthetic）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/evalset"
	"github.com/handlerzzy/feedsystem/internal/evalset/synth"
)

func main() {
	def := synth.DefaultOptions()
	var (
		out          = flag.String("out", "testdata/eval/snapshot-synthetic.json", "评测快照输出路径")
		jsonOut      = flag.String("json", "", "基线结果 JSON 输出路径（留空则只生成快照）")
		markdownOut  = flag.String("markdown", "", "基线结果 Markdown 输出路径")
		lexicalProbe = flag.Bool("lexical-probe", false, "在基线里额外评测纯词法探针（证明数据里存在可利用的语义结构）")
		ksFlag       = flag.String("k", "5,10,20", "要计算的 K，逗号分隔")

		seed    = flag.Int64("seed", def.Seed, "随机种子：固定种子才能复现同一份数据")
		videos  = flag.Int("videos", def.Videos, "视频条数（前置条件要求 ≥ 500）")
		users   = flag.Int("users", def.Users, "用户数（前置条件要求 ≥ 50）")
		topics  = flag.Int("topics", def.Topics, "话题数")
		days    = flag.Int("days", def.Days, "发布时间跨度（天）")
		authors = flag.Int("authors", def.Authors, "作者池大小")
		train   = flag.Int("train-per-user", def.TrainPerUser, "每个用户在观测窗口之前的互动条数")
		labels  = flag.Int("labels-per-user", def.LabelPerUser, "每个用户在观测窗口之内的互动条数（= 每用户相关样本数，前置条件要求 ≥ 20）")
		split   = flag.Float64("split", def.SplitRatio, "时间切分比例（按发布时间分位）")
	)
	flag.Parse()

	genAt, fixed, err := generatedAt()
	if err != nil {
		fatal(err)
	}

	opts := def
	opts.Seed = *seed
	opts.Videos = *videos
	opts.Users = *users
	opts.Topics = *topics
	opts.Days = *days
	opts.Authors = *authors
	opts.TrainPerUser = *train
	opts.LabelPerUser = *labels
	opts.SplitRatio = *split
	opts.GeneratedAt = genAt

	snap, stats, err := synth.Generate(opts)
	if err != nil {
		fatal(err)
	}
	if err := snap.Save(*out); err != nil {
		fatal(fmt.Errorf("写快照失败: %w", err))
	}

	fmt.Printf("合成评测集已写入 %s\n", *out)
	fmt.Printf("  videos=%d（候选集 %d） users=%d topics=%d interactions=%d\n",
		stats.Videos, stats.Candidates, stats.Users, stats.Topics, stats.Interactions)
	fmt.Printf("  每用户相关样本: min=%d max=%d\n",
		stats.LabelsPerUser[0], stats.LabelsPerUser[len(stats.LabelsPerUser)-1])
	fmt.Printf("  split_at=%s  generated_at=%s\n",
		snap.SplitAt.Format(time.RFC3339), snap.GeneratedAt.Format(time.RFC3339))
	if fixed {
		fmt.Println("  生成时间由 SOURCE_DATE_EPOCH 固定：同一 seed 下本文件逐字节可复现")
	} else {
		fmt.Println("  ⚠ 生成时间取自当前时刻：本次输出**不能**与上次逐字节比对；复现请设置 SOURCE_DATE_EPOCH")
	}
	fmt.Println("注意：这是合成数据（标签按用户话题偏好采样生成），只能用于验证评测流水线与比较排序策略，")
	fmt.Println("      不能作为线上基线或对外结论。真实基线请用 cmd/evalbaseline 从真实互动数据导出。")

	if *jsonOut == "" && *markdownOut == "" {
		return
	}

	ks, err := parseKs(*ksFlag)
	if err != nil {
		fatal(err)
	}
	rankers := evalset.BuiltinRankers()
	if *lexicalProbe {
		rankers = append(rankers, evalset.LexicalProbeRanker(snap))
	}
	res, err := evalset.Evaluate(snap, rankers, ks)
	if err != nil {
		fatal(err)
	}
	fmt.Print(evalset.RenderText(res))

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

// generatedAt 解析 SOURCE_DATE_EPOCH（Unix 秒）。
//
// 返回值第二个分量表示"是否被显式固定"——命令行据此打印不同的提示，
// 因为"这次输出能不能与上次逐字节比对"是使用者最需要知道的一件事。
func generatedAt() (time.Time, bool, error) {
	raw := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH"))
	if raw == "" {
		return time.Now().UTC(), false, nil
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false, fmt.Errorf(
			"SOURCE_DATE_EPOCH 必须是 Unix 秒（整数），实际 %q", raw)
	}
	return time.Unix(sec, 0).UTC(), true, nil
}

func parseKs(raw string) ([]int, error) {
	var ks []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 用 strconv 而不是 Sscanf：Sscanf("%d") 会把 "5abc" 解析成 5，
		// 于是一个拼错的参数被静默接受。
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
	fmt.Fprintf(os.Stderr, "evalseed: %v\n", err)
	os.Exit(1)
}
