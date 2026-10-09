package video

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件是 P1 的"模型输出 -> 可用结果"这一段：提示词、解析、归一化、校验。
//
// 为什么全部做成**纯函数**（不碰 ctx、不碰 DB、不碰网络）：
// 这段逻辑的失败模式全是"看起来成功了，其实写进去了脏数据"——
// 只有能被穷举输入的单测覆盖，才谈得上可靠。CI 不需要 API key 也正是靠它。

// PromptVersion 标识当前提示词与解析规则的版本。
//
// **改了这里的提示词或下面的解析/归一化规则，必须同时改这个常量。**
// 它写进 video_ai_analyses.prompt_version 并参与幂等键：
//   - 不改版本号 → 改完 prompt 后重跑，结果会被幂等键合并进旧行，
//     永远看不出"改 prompt 之后效果变了吗"；
//   - 改了版本号 → 同一视频会留下两行，可以直接对比。
const PromptVersion = "v1"

// TagSuggestionMax 是一次分析最多采纳的标签数。
//
// 上限的意义不是省存储：标签过多会让 listByTag 变成"几乎所有视频都命中"，
// 聚合页失去筛选能力。8 个足够描述一条短视频的内容。
const TagSuggestionMax = 8

// TagNameMaxLen 是标签名长度上限（按 rune 计）。
//
// 与 tags.name 的 varchar(100) 对齐但更严：中文标签 20 个字已经很长，
// 放宽到 100 只会让模型的长句片段混进标签体系。
const TagNameMaxLen = 20

// SummaryMaxLen 是一句话摘要的长度上限（按 rune 计）。
//
// 与 video_ai_analyses.summary 的 varchar(500) 对齐但留有余量：
// 存不下时宁可截断也不要让整次分析因为一次写入失败而重试。
const SummaryMaxLen = 200

// DefaultMinConfidence 是标签入库的默认置信度阈值。
//
// 为什么默认 0.6：P1 的目标是"让 listByTag 有内容"，宁可少而准。
// 阈值可从配置覆盖（ai.tag_min_confidence），关掉 AI 时该值不影响任何行为。
const DefaultMinConfidence = 0.6

// tagAliases 是标签同义合并表：键为归一化后的写法，值为规范写法。
//
// 为什么需要它：模型对同一概念会给出不同写法（"人工智能"/"AI"/"ai 技术"），
// 不合并就会在 tags 表里堆出多个语义重复的行，listByTag 也被割裂成多份。
//
// 只放**确定等价**的映射。近义但不等价的（如"机器学习"与"深度学习"）
// 一律不合并：错并会让用户搜不到本该出现的内容，比多几行标签更糟。
var tagAliases = map[string]string{
	"ai":              "人工智能",
	"aigc":            "人工智能",
	"人工智能技术":          "人工智能",
	"ai技术":            "人工智能",
	"人工智能ai":          "人工智能",
	"machinelearning": "机器学习",
	"ml":              "机器学习",
	"deeplearning":    "深度学习",
	"dl":              "深度学习",
	"大模型":             "大语言模型",
	"llm":             "大语言模型",
	"短视频":             "短视频",
	"vlog":            "生活记录",
	"美食制作":            "美食",
	"做菜":              "美食",
	"旅行vlog":          "旅行",
	"旅游":              "旅行",
	"数码产品":            "数码",
	"手机摄影":            "摄影",
	"健身训练":            "健身",
	"运动健身":            "健身",
	"编程教学":            "编程",
	"代码":              "编程",
}

// RawTag 是模型给出的一个原始标签。
//
// Name/Confidence 用指针是为了能把"字段缺失"与"零值"区分开：
// 置信度缺失（常见）应当按阈值保守通过，而不是当成 0 被过滤掉——
// 那会让"模型漏写 confidence"表现为"模型一个标签都没给"。
type RawTag struct {
	Name       *string  `json:"name"`
	Confidence *float64 `json:"confidence"`
}

// rawModelOutput 是模型返回的 JSON 契约。
//
// Tags 用指针是为了区分"字段缺失"（模型没按契约输出）与"显式空数组"
// （模型确实认为没有标签）——两者的处置不同：前者是解析失败，后者是结果为空。
type rawModelOutput struct {
	Summary *string   `json:"summary"`
	Tags    *[]RawTag `json:"tags"`
}

// ParseModelOutput 把模型返回的文本解析成规范化结果。
//
// 返回的 error 一律表示"这次结果不可用"（按总览第 4 节：丢弃 + 记 Warn，
// 不写脏数据入库）。调用方不需要区分错误种类就能做出正确处置。
func ParseModelOutput(text string, minConfidence float64) (AnalysisOutput, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return AnalysisOutput{}, errors.New("模型返回了空内容")
	}

	// 容忍模型把 JSON 包在 ```json 代码块里。
	//
	// sidecar 已经要求"只输出 JSON"，但这是概率性约束，实测会偶发违反。
	// 这里剥一层代码块的成本极低，而收益是省掉一次重试（重试要花 7 秒退避）。
	trimmed = stripCodeFence(trimmed)

	var raw rawModelOutput
	dec := json.NewDecoder(strings.NewReader(trimmed))
	// 不忽略未知字段：模型多给的字段无害，但接受它们会让"契约"逐渐漂移，
	// 将来真正需要的字段缺失时反而发现不了。这里保持严格，只接受约定字段。
	if err := dec.Decode(&raw); err != nil {
		return AnalysisOutput{}, fmt.Errorf("模型输出不是合法 JSON: %w", err)
	}
	if raw.Tags == nil {
		return AnalysisOutput{}, errors.New("模型输出缺少 tags 字段")
	}

	out := AnalysisOutput{Summary: normalizeSummary(raw.Summary), Tags: normalizeTags(*raw.Tags, minConfidence)}
	if len(out.Tags) == 0 {
		// 一个有效标签都没有：本次结果对 P1 的目标（让 listByTag 有内容）毫无价值。
		// 当作失败处理会触发重试——这可能白花模型钱，但比写入一行"空分析"
		// 更好：空行会让"这个视频分析过了"为真，从此再也不会重跑。
		return AnalysisOutput{}, errors.New("模型没有给出任何有效标签")
	}
	return out, nil
}

// stripCodeFence 去掉包裹 JSON 的 Markdown 代码块标记。
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 去掉起始行（可能是 ``` 或 ```json）。
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	} else {
		return s
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// normalizeSummary 归一化摘要。
//
// 只做三件事：去空白、折叠内部换行、按 rune 截断。
// 刻意**不**做"空摘要时用标题兜底"：那会把用户原标题伪装成 AI 产出，
// 之后无法分辨哪些摘要是模型真正生成的。
func normalizeSummary(s *string) string {
	if s == nil {
		return ""
	}
	// 换行会把一句话摘要变成多行，破坏前端一行的排版，因此折叠成空格。
	out := strings.Join(strings.Fields(*s), " ")
	return truncateRunes(out, SummaryMaxLen)
}

// normalizeTags 归一化标签列表：trim -> 小写 -> 同义合并 -> 长度校验
// -> 置信度阈值 -> 去重，并限制总数。
//
// 顺序很重要，改动前先想清楚每一步为什么在这个位置：
//  1. 先 trim + 小写，后面所有比较才有一致的基准；
//  2. 同义合并要在去重**之前**，否则 "AI" 与 "人工智能" 会各占一个名额，
//     去重后再合并就只剩一个、白白浪费一个位置；
//  3. 阈值过滤放在合并之后：同义合并可能把两个各 0.4 的标签合成一个，
//     而合并后的置信度取最大值——先过滤会让它俩都被扔掉。
func normalizeTags(raw []RawTag, minConfidence float64) []TagSuggestion {
	best := make(map[string]float64)

	for _, t := range raw {
		if t.Name == nil {
			continue
		}
		name := normalizeTagName(*t.Name)
		if name == "" {
			continue
		}

		conf := 0.0
		if t.Confidence != nil {
			conf = clamp01(*t.Confidence)
		}
		// 置信度缺失视为"未评分"而不是 0：模型漏给这个字段是常见现象，
		// 直接判 0 会让整批标签被阈值过滤干净（表现为"模型一个标签都没给"）。
		// 取阈值本身作为保守估计：刚好能通过、也不会盖过明确给出高分的结果。
		if t.Confidence == nil {
			conf = minConfidence
		}
		if conf < minConfidence {
			continue
		}
		if prev, ok := best[name]; !ok || conf > prev {
			best[name] = conf
		}
	}

	if len(best) == 0 {
		return nil
	}

	// 稳定输出顺序：按置信度降序、同分按名字升序。
	//
	// 不排序的话 map 遍历顺序随机，同一份模型输出会写出不同顺序的 tags_json，
	// 快照/对比/diff 全都失去可比性。
	out := make([]TagSuggestion, 0, len(best))
	for name, conf := range best {
		out = append(out, TagSuggestion{Name: name, Confidence: conf})
	}
	sortSuggestions(out)

	if len(out) > TagSuggestionMax {
		out = out[:TagSuggestionMax]
	}
	return out
}

// normalizeTagName 归一化单个标签名：trim、折叠空白、小写、去 #、同义合并、限长。
func normalizeTagName(raw string) string {
	name := strings.TrimSpace(raw)
	// 模型经常把标签写成 "#猫"：保留 # 会让 tags 表里同时出现 "猫" 与 "#猫"
	// 两个语义相同的行（用户手写的是不带 # 的，因为 ExtractTags 已经把 # 剥掉了）。
	name = strings.TrimPrefix(name, "#")
	name = strings.TrimSpace(name)
	// 折叠内部空白：数据库里 "机器 学习" 与 "机器学习" 是两行。
	name = strings.Join(strings.Fields(name), "")
	name = strings.ToLower(name)
	if name == "" {
		return ""
	}
	if canonical, ok := tagAliases[name]; ok {
		name = canonical
	}
	return truncateRunes(name, TagNameMaxLen)
}

// sortSuggestions 按置信度降序、名字升序排序（原地）。
//
// 用标准库排序而不是手写：标签数虽然不多，但"排序写错"会表现为
// tags_json 顺序不稳定，而那是最难察觉的一类不确定性。
func sortSuggestions(list []TagSuggestion) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.Name < b.Name
	})
}

// truncateRunes 按 rune 截断，避免把一个多字节字符切成两半。
//
// 用 []rune 而不是 s[:n]：后者会在中文标签上切出乱码，
// 而乱码一旦入库就再也无法修复（只能全量重跑）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	// 截断后可能把结尾留成空格，再去一次。
	return strings.TrimSpace(string(r[:max]))
}

// BuildAnalysisPrompt 生成喂给模型的用户提示词。
//
// 单独导出以便单测断言"提示词里确实包含了提示词版本相关的约束"，
// 也便于将来做 A/B（不同 PromptVersion 走不同措辞）。
func BuildAnalysisPrompt(in *AnalysisInput) string {
	var b strings.Builder
	b.WriteString("请分析下面这条短视频的标题与简介，提取内容标签并写一句话摘要。\n\n")
	b.WriteString("标题：")
	b.WriteString(strings.TrimSpace(in.Title))
	b.WriteString("\n简介：")
	if desc := strings.TrimSpace(in.Description); desc != "" {
		b.WriteString(desc)
	} else {
		b.WriteString("（作者没有填写简介）")
	}
	b.WriteString("\n\n输出要求：\n")
	b.WriteString("1. 只输出一个 JSON 对象，不要输出解释文字或 Markdown 代码块；\n")
	b.WriteString("2. 结构为 {\"summary\": \"一句话摘要\", \"tags\": [{\"name\": \"标签\", \"confidence\": 0.0-1.0}]}；\n")
	b.WriteString("3. 标签用常见的短名词，2-6 个字，不要带 # 号，最多 8 个；\n")
	b.WriteString("4. 不确定的标签就给低置信度，不要为了凑数编造标签；\n")
	b.WriteString("5. 摘要不超过 40 个字，只描述内容本身，不要复述标题。\n")
	return b.String()
}

// SystemPrompt 是给模型的系统提示词。
//
// 约束"不要输出与内容无关的推断"是有意的：模型很爱从"用户可能喜欢什么"
// 延伸出人群画像类标签（"年轻人""女性向"），那类标签既不可靠，
// 也会让标签体系变成猜谜。标签只描述内容本身。
const SystemPrompt = "你是一个视频内容标注助手。你只根据给定的标题与简介做标注，" +
	"不推测作者身份、不做人群画像、不输出与内容无关的判断。"
