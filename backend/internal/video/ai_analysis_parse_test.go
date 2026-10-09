package video

import (
	"strings"
	"testing"
)

// 本文件穷举"模型输出 -> 可用结果"这段转换。
//
// 为什么值得这么细：这里的每一个失败模式都是"看起来成功了，其实写进去了脏数据"。
// 而脏数据一旦入库，只能靠全量重跑修复——所以宁可在解析层多拦一点。

func ptr[T any](v T) *T { return &v }

func rawTags(pairs ...any) *[]RawTag {
	tags := make([]RawTag, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		name := pairs[i].(string)
		conf := pairs[i+1].(float64)
		if name == "" {
			tags = append(tags, RawTag{Confidence: ptr(conf)})
			continue
		}
		tags = append(tags, RawTag{Name: ptr(name), Confidence: ptr(conf)})
	}
	return &tags
}

// ---------------------------------------------------------------------------
// 解析：合法与非法
// ---------------------------------------------------------------------------

func TestParseModelOutputAcceptsValidJSON(t *testing.T) {
	out, err := ParseModelOutput(`{"summary":"一只猫在打盹","tags":[{"name":"猫","confidence":0.9},{"name":"宠物","confidence":0.8}]}`, DefaultMinConfidence)
	if err != nil {
		t.Fatalf("合法输出不该报错: %v", err)
	}
	if out.Summary != "一只猫在打盹" {
		t.Errorf("summary = %q", out.Summary)
	}
	if len(out.Tags) != 2 {
		t.Fatalf("tags = %d, want 2", len(out.Tags))
	}
	// 高置信度的排在前面：前端与下游都依赖这个顺序。
	if out.Tags[0].Name != "猫" || out.Tags[1].Name != "宠物" {
		t.Errorf("标签顺序应按置信度降序: %+v", out.Tags)
	}
}

// TestParseModelOutputRejectsInvalidJSON 是总览第 4 节"非法 JSON 丢弃、不写脏数据"的实现。
func TestParseModelOutputRejectsInvalidJSON(t *testing.T) {
	cases := map[string]string{
		"空字符串":      "",
		"纯空白":       "   \n\t ",
		"纯文本":       "这只猫很可爱",
		"截断的 JSON":  `{"summary":"x","tags":[{"name":"猫"`,
		"数组而不是对象":   `["猫"]`,
		"缺 tags 字段": `{"summary":"只有摘要"}`,
		"tags 是字符串": `{"summary":"x","tags":"猫"}`,
		"tags 为空数组": `{"summary":"x","tags":[]}`,
		"标签名为空":     `{"summary":"x","tags":[{"name":"","confidence":0.9}]}`,
		"标签名只有空白":   `{"summary":"x","tags":[{"name":"   ","confidence":0.9}]}`,
		"标签名只有井号":   `{"summary":"x","tags":[{"name":"#","confidence":0.9}]}`,
		"全部低于置信度阈值": `{"summary":"x","tags":[{"name":"猫","confidence":0.1}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := ParseModelOutput(body, DefaultMinConfidence)
			if err == nil {
				t.Fatalf("必须被拒绝，实际得到 %+v", out)
			}
			if len(out.Tags) != 0 || out.Summary != "" {
				t.Errorf("被拒绝时不该返回任何可用结果: %+v", out)
			}
		})
	}
}

// TestParseModelOutputStripsCodeFence 覆盖"模型把 JSON 包在代码块里"。
//
// sidecar 已经用 system 提示要求"只输出 JSON"，但那是概率性约束。
// 这里剥一层代码块的收益是省掉一次 7 秒退避的重试。
func TestParseModelOutputStripsCodeFence(t *testing.T) {
	for _, body := range []string{
		"```json\n{\"summary\":\"猫\",\"tags\":[{\"name\":\"猫\",\"confidence\":0.9}]}\n```",
		"```\n{\"summary\":\"猫\",\"tags\":[{\"name\":\"猫\",\"confidence\":0.9}]}\n```",
	} {
		out, err := ParseModelOutput(body, DefaultMinConfidence)
		if err != nil {
			t.Fatalf("代码块包裹应当被容忍，实际报错: %v", err)
		}
		if len(out.Tags) != 1 {
			t.Errorf("tags = %+v", out.Tags)
		}
	}
}

// ---------------------------------------------------------------------------
// 标签归一化
// ---------------------------------------------------------------------------

// TestNormalizeTagName 覆盖 trim / 去 # / 折叠空白 / 小写 / 同义合并 / 限长。
func TestNormalizeTagName(t *testing.T) {
	cases := map[string]string{
		"  猫  ": "猫",
		"#猫":    "猫",
		"# 猫":   "猫",
		"机器 学习": "机器学习",
		"AI":    "人工智能",
		"ai":    "人工智能",
		"AIGC":  "人工智能",
		"AI 技术": "人工智能",
		"ML":    "机器学习",
		"LLM":   "大语言模型",
		"旅游":    "旅行",
		"vlog":  "生活记录",
		"":      "",
		"   ":   "",
		"#":     "",
	}
	for raw, want := range cases {
		if got := normalizeTagName(raw); got != want {
			t.Errorf("normalizeTagName(%q) = %q, want %q", raw, got, want)
		}
	}

	// 超长标签按 rune 截断，且不能切出乱码。
	long := strings.Repeat("很长的标签", 10)
	got := normalizeTagName(long)
	if len([]rune(got)) > TagNameMaxLen {
		t.Errorf("截断后长度 = %d, want <= %d", len([]rune(got)), TagNameMaxLen)
	}
	if !utf8Valid(got) {
		t.Errorf("截断产生了非法 UTF-8: %q", got)
	}
}

// TestNormalizeTagsMergesDuplicates 覆盖"同义标签只留一个，且取最高置信度"。
//
// 顺序很关键：同义合并必须在去重**之前**，否则 "AI" 与 "人工智能"
// 会各占一个标签名额。
func TestNormalizeTagsMergesDuplicates(t *testing.T) {
	tags := normalizeTags(*rawTags(
		"AI", 0.9,
		"人工智能", 0.7,
		"猫", 0.8,
		"猫", 0.95,
		"#猫", 0.5,
	), DefaultMinConfidence)

	if len(tags) != 2 {
		t.Fatalf("合并后应只剩 2 个标签，实际 %+v", tags)
	}
	byName := map[string]float64{}
	for _, t := range tags {
		byName[t.Name] = t.Confidence
	}
	if byName["人工智能"] != 0.9 {
		t.Errorf("同义合并应取最高置信度 0.9，实际 %v", byName["人工智能"])
	}
	if byName["猫"] != 0.95 {
		t.Errorf("#猫 与 猫 应合并并取 0.95，实际 %v", byName["猫"])
	}
}

// TestNormalizeTagsMissingConfidencePasses 覆盖"模型漏写 confidence"。
//
// 这是常见现象。判 0 会让整批标签被阈值过滤干净，
// 表现为"模型一个标签都没给"——而真正的原因只是少了个字段。
func TestNormalizeTagsMissingConfidencePasses(t *testing.T) {
	raw := []RawTag{{Name: ptr("猫")}} // 没有 Confidence
	tags := normalizeTags(raw, DefaultMinConfidence)
	if len(tags) != 1 {
		t.Fatalf("置信度缺失不应导致过滤，实际 %+v", tags)
	}
	if tags[0].Confidence != DefaultMinConfidence {
		t.Errorf("缺失时应取阈值本身作为保守估计，实际 %v", tags[0].Confidence)
	}
}

// TestNormalizeTagsClampsConfidence 覆盖模型给出越界置信度。
func TestNormalizeTagsClampsConfidence(t *testing.T) {
	tags := normalizeTags(*rawTags("猫", 1.5, "狗", -0.3), 0.0)
	byName := map[string]float64{}
	for _, tg := range tags {
		byName[tg.Name] = tg.Confidence
	}
	if byName["猫"] != 1.0 {
		t.Errorf("超过 1 应被夹到 1，实际 %v", byName["猫"])
	}
	if byName["狗"] != 0 {
		t.Errorf("负数应被夹到 0，实际 %v", byName["狗"])
	}
}

// TestNormalizeTagsCapsCount 守住标签数量上限。
func TestNormalizeTagsCapsCount(t *testing.T) {
	var pairs []any
	for i := 0; i < 20; i++ {
		pairs = append(pairs, string(rune('a'+i)), 0.9)
	}
	tags := normalizeTags(*rawTags(pairs...), DefaultMinConfidence)
	if len(tags) != TagSuggestionMax {
		t.Errorf("标签数 = %d, want %d", len(tags), TagSuggestionMax)
	}
}

// TestNormalizeTagsIsDeterministic 守住输出顺序稳定。
//
// map 遍历顺序随机：不排序的话同一份模型输出会写出不同顺序的 tags_json，
// 版本对比与 diff 全部失去可比性。
func TestNormalizeTagsIsDeterministic(t *testing.T) {
	raw := *rawTags("a", 0.8, "b", 0.8, "c", 0.8, "d", 0.8)
	first := normalizeTags(raw, DefaultMinConfidence)
	for i := 0; i < 20; i++ {
		got := normalizeTags(raw, DefaultMinConfidence)
		for j := range first {
			if got[j].Name != first[j].Name {
				t.Fatalf("第 %d 次顺序不同: %+v vs %+v", i, got, first)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 摘要
// ---------------------------------------------------------------------------

func TestNormalizeSummary(t *testing.T) {
	if got := normalizeSummary(nil); got != "" {
		t.Errorf("缺失摘要应为空串，实际 %q", got)
	}
	if got := normalizeSummary(ptr("  一句话\n摘要  ")); got != "一句话 摘要" {
		t.Errorf("换行应折叠成空格，实际 %q", got)
	}
	long := strings.Repeat("摘要", 300)
	got := normalizeSummary(&long)
	if len([]rune(got)) > SummaryMaxLen {
		t.Errorf("摘要长度 = %d, want <= %d", len([]rune(got)), SummaryMaxLen)
	}
}

// ---------------------------------------------------------------------------
// 提示词
// ---------------------------------------------------------------------------

// TestBuildAnalysisPromptIncludesContent 守住提示词里确实带上了内容与输出契约。
//
// 只断言"包含标题/简介"是不够的：JSON 契约与长度约束也必须出现，
// 否则模型会自由发挥，而解析层会把这些输出全部拒掉（表现为"分析总是失败"）。
func TestBuildAnalysisPromptIncludesContent(t *testing.T) {
	p := BuildAnalysisPrompt(&AnalysisInput{VideoID: 1, Title: "猫的一天", Description: "记录家里的猫"})
	for _, want := range []string{"猫的一天", "记录家里的猫", "JSON", "summary", "tags", "confidence"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词缺少 %q:\n%s", want, p)
		}
	}

	// 简介为空时要显式说明，而不是留一个空行让模型猜。
	p2 := BuildAnalysisPrompt(&AnalysisInput{VideoID: 2, Title: "只有标题"})
	if !strings.Contains(p2, "没有填写简介") {
		t.Errorf("简介为空时应显式说明:\n%s", p2)
	}
}

// utf8Valid 报告字符串是否是合法 UTF-8（避免为了一个断言引入 unicode/utf8 的别名）。
func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
