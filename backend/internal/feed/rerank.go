package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/video"

	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
)

// 本文件实现 P2 §4.4「LLM 精排」。
//
// 一条不可协商的纪律（D1 的原文）：
//
//	**必须带严格超时**：超时立即退回融合分排序，绝不让用户请求等待模型。
//
// 实现上它体现为三件事，缺一条都会让"降级"变成"偶尔很慢"：
//
//  1. 精排的 ctx 是**独立的**、带硬超时的 context（不是请求的 ctx 派生出的
//     宽松 deadline）。请求被取消时精排也该停，但精排绝不能把请求拖长。
//  2. 任何异常（超时、模型报错、结果不合法、候选为空）都走同一个出口：
//     `degraded` 加一、记一条 Warn、返回融合分顺序。
//  3. 精排**只作用于列表重排**，不改变列表的集合：候选的 ID 集合在校验里
//     被强制一致，模型无法借此塞进一条不存在的视频，也无法丢掉一条。
//
// 默认关闭（ai.rerank_enabled=false）不是保守，而是**实测之前唯一安全的
// 选择**：本环境没有 API key，真实 provider 的 p99 没有被测过，
// 而把未知大小的延迟放进用户请求路径与上面那条纪律直接冲突。
// 开启的判定规则写在 AI-P2-设计决定.md 第 2 节。

// 精排相关的常量。
const (
	// RerankCacheTTL 是精排结果的缓存时长。
	//
	// 为什么需要缓存：同一个用户连续刷新（下拉、返回、切页）会给出几乎
	// 相同的候选集，而每次都调模型等于把成本与延迟都乘以刷新次数。
	// 30 秒的依据：它短于"用户会察觉内容没变"的阈值，又足以覆盖
	// 一轮连续的翻页操作。
	RerankCacheTTL = 30 * time.Second

	// RerankCacheSweep 是过期条目的清理间隔。
	RerankCacheSweep = 2 * time.Minute

	// defaultRerankCandidates 是送进模型精排的候选条数上限（D4 的默认 20）。
	//
	// 与 internal/config 的 defaultRerankCandidates 同值：那一层负责
	// "yaml 里没写"，这里负责"调用方直接构造时忘了写"。两处漂移会被
	// TestRerankDefaultsMatchConfig 直接抓住。
	defaultRerankCandidates = 20

	// rerankPreviewRunes 是拼进 prompt 时每条文本的截断长度。
	//
	// 60 个字符（≈ 40 token）足以让模型判断"讲的是什么"，
	// 又让 20 条候选的 prompt 控制在几百 token 量级（D4 的估算）。
	// 不截断时，描述较长的候选会单独把 prompt 撑大一倍。
	rerankPreviewRunes = 60
)

// RerankOutcome 是一次精排尝试的结果。
type RerankOutcome struct {
	// Candidates 是送进模型（或命中缓存）的候选条数。
	Candidates int
	// Ranked 是模型给出的顺序（按 ID）。降级时为空。
	Ranked []uint
	// Degraded 为 true 表示这次没有拿到可用的模型结果，调用方必须用融合分。
	Degraded bool
	// Reason 是降级原因的短语（用于日志与指标，不对外）。
	Reason string
	// Cached 表示结果来自缓存。
	Cached bool
}

// rerankScore 报告"精排这一层"的累计降级次数，用于 P2 的验收项
// "只打开开关而不调超时的人会立刻观察到 100% 降级率"。
//
// 用原子计数而不是引入一套 metrics 依赖：这个数字只需要能被读到
// （日志里打出来即可），而 P2 的验收要求的是"能观察到"，
// 不是"能画图"。真要做监控时它是换成一个 Counter 的一行改动。
func (f *FeedService) rerankDegradedCount() int64 { return f.rerankDegraded.Load() }

// rerankCandidates 把列表里的前 N 条送进模型重排，返回**整个列表**的新顺序。
//
// 为什么返回整个列表的顺序而不是只返回前 N 条：调用方（融合阶段）需要
// 一个完整的顺序来做配额再分配，把"前 N 条的顺序"与"后面的原顺序"
// 拼起来是调用方最容易写错的地方（拼接点的重复/丢失）。
//
// 未启用、候选不足、超时、结果不合法 —— 全部返回 Degraded=true 且
// Ranked 为 nil，调用方据此保持原顺序。
func (f *FeedService) rerankCandidates(ctx context.Context, viewerAccountID uint, items []*video.Video) RerankOutcome {
	if !f.rerankEnabled || f.rerankClient == nil || len(items) == 0 {
		return RerankOutcome{Degraded: true, Reason: "disabled"}
	}
	topN := f.rerankTopN
	if topN <= 0 {
		topN = defaultRerankCandidates
	}
	if topN > len(items) {
		topN = len(items)
	}
	if topN < 2 {
		// 一条候选没有可排的空间：直接跳过，别浪费一次模型调用
		// （也避免"结果与输入一致"被记成一次成功精排）。
		return RerankOutcome{Degraded: true, Reason: "too_few_candidates"}
	}
	head := items[:topN]

	key := rerankCacheKey(viewerAccountID, head)
	if f.rerankCache != nil {
		if v, found := f.rerankCache.Get(key); found {
			if ranked, ok := v.([]uint); ok {
				return RerankOutcome{Candidates: topN, Ranked: ranked, Cached: true}
			}
		}
	}

	candidates := make([]RerankCandidate, 0, len(head))
	for _, v := range head {
		candidates = append(candidates, RerankCandidate{
			ID:          v.ID,
			Title:       v.Title,
			Description: v.Description,
			AuthorID:    v.AuthorID,
			LikesCount:  v.LikesCount,
			Popularity:  v.Popularity,
			CreateTime:  v.CreateTime,
		})
	}

	// 硬超时：rerankTimeout 是**整次调用**的上界，与 D1 一致。
	opCtx, cancel := context.WithTimeout(ctx, f.rerankTimeout)
	defer cancel()

	started := time.Now()
	ranked, err := f.rerankClient.Rerank(opCtx, viewerAccountID, candidates)
	latency := time.Since(started)

	if err != nil {
		f.noteRerankDegrade(ctx, "模型调用失败，本页退回融合分排序", latency, zap.Error(err))
		return RerankOutcome{Candidates: topN, Degraded: true, Reason: degradeReason(err)}
	}
	if err := validateRerankResult(ranked, candidates); err != nil {
		// 不合法的结果**整批丢弃**：只采纳"能对上的那部分"会让排序
		// 随模型的随机输出漂移，而现象是"顺序时好时坏"。
		f.noteRerankDegrade(ctx, "精排结果不合法，本页退回融合分排序", latency, zap.Error(err))
		return RerankOutcome{Candidates: topN, Degraded: true, Reason: "invalid_result"}
	}
	if f.rerankCache != nil {
		f.rerankCache.Set(key, ranked, RerankCacheTTL)
	}
	logging.Ctx(ctx).Debug("精排完成",
		zap.Int("candidates", topN), zap.Duration("latency", latency))
	return RerankOutcome{Candidates: topN, Ranked: ranked}
}

// validateRerankResult 校验模型返回的顺序是否可用。
//
// 三条硬校验，每一条都对应一种"看起来成功、实际有毒"的输出：
//
//  1. **条数一致**：少了说明模型丢了几条（那些视频会静默消失），
//     多了说明它编了 ID（会被滤镜掉，但顺序已经不可信）。
//  2. **集合一致**：必须是同一批 ID，不增不减。模型塞进一个列表里没有的
//     ID 时，若不校验就会出现"推荐了一条用户本轮根本看不到的视频"——
//     它不在候选池里，也就没有经过任何过滤。
//  3. **无重复**：重复 ID 会让同一张卡片出现两次（去重只能在后端做，
//     因为模型的输出顺序本身就是我们要用的东西）。
func validateRerankResult(ranked []uint, candidates []RerankCandidate) error {
	if len(ranked) != len(candidates) {
		return fmt.Errorf("feed: 精排返回 %d 条，与候选的 %d 条不一致", len(ranked), len(candidates))
	}
	want := make(map[uint]bool, len(candidates))
	for _, c := range candidates {
		want[c.ID] = true
	}
	seen := make(map[uint]bool, len(ranked))
	for _, id := range ranked {
		if !want[id] {
			return fmt.Errorf("feed: 精排返回了候选之外的视频 %d", id)
		}
		if seen[id] {
			return fmt.Errorf("feed: 精排返回了重复的视频 %d", id)
		}
		seen[id] = true
	}
	return nil
}

// applyRerankOrder 把整个列表按"前 N 条的新顺序 + 其余的原顺序"重排。
//
// 刻意**不做**的事：把 N 之后的条目也按融合分重排。它们在融合阶段已经
// 按融合分排好（见 fuseChannels 的槽位顺序），这里再排一次等于把同一件事
// 做两遍，而两遍的结果一旦不同（同分时的次序），顺序就会随实现细节漂移。
func applyRerankOrder(items []*video.Video, ranked []uint) []*video.Video {
	if len(ranked) == 0 || len(items) == 0 {
		return items
	}
	byID := make(map[uint]*video.Video, len(items))
	for _, v := range items {
		byID[v.ID] = v
	}
	out := make([]*video.Video, 0, len(items))
	used := make(map[uint]bool, len(ranked))
	for _, id := range ranked {
		if v := byID[id]; v != nil && !used[id] {
			out = append(out, v)
			used[id] = true
		}
	}
	for _, v := range items {
		if !used[v.ID] {
			out = append(out, v)
		}
	}
	if len(out) != len(items) {
		// 理论不可达（校验已保证集合一致）。真发生时按原顺序返回：
		// 丢一条视频比顺序不够好严重得多。
		logging.L().Warn("精排重排后条数不一致，放弃本次精排顺序",
			zap.Int("before", len(items)), zap.Int("after", len(out)))
		return items
	}
	return out
}

// rerankCacheKey 由"用户 + 候选 ID 序列"拼出缓存键。
//
// 用 FNV 而不是把 ID 拼成字符串：候选最多 64 条，拼出来的键会有几百字节，
// 而它要参与 map 查找与日志。哈希碰撞的后果只是"命中了一份别人的顺序"——
// 在 30 秒的窗口内、同一个用户、同一批候选下，这不构成正确性问题。
func rerankCacheKey(accountID uint, items []*video.Video) string {
	h := fnv.New64a()
	var buf [8]byte
	for _, v := range items {
		buf[0] = byte(v.ID)
		buf[1] = byte(v.ID >> 8)
		buf[2] = byte(v.ID >> 16)
		buf[3] = byte(v.ID >> 24)
		_, _ = h.Write(buf[:4])
	}
	return "rerank:" + strconv.FormatUint(uint64(accountID), 10) + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// noteRerankDegrade 记录一次降级。
//
// 级别是 **Warn** 而不是 Info：与语义路的降级不同，精排"本该有结果"，
// 降级说明配置或 provider 有问题（最典型的是"只开了开关没调超时"，
// 那时这里会是 100% 的降级率）。P2 的验收要求这个现象**立刻可观察**，
// 而一条 Info 日志达不到这个效果。
func (f *FeedService) noteRerankDegrade(ctx context.Context, msg string, latency time.Duration, fields ...zap.Field) {
	total := f.rerankDegraded.Add(1)
	logging.Ctx(ctx).Warn(msg, append(fields,
		zap.Duration("latency", latency),
		zap.Duration("timeout", f.rerankTimeout),
		zap.Int("candidates", f.rerankTopN),
		zap.Int64("degraded_total", total),
	)...)
}

// degradeReason 把一次调用错误翻译成一句可用于日志/指标的短语。
func degradeReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errorsIsTimeout(err):
		return "timeout"
	case errorsIsDisabled(err):
		return "ai_disabled"
	default:
		return "call_failed"
	}
}

func errorsIsTimeout(err error) bool {
	return errors.Is(err, ai.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

func errorsIsDisabled(err error) bool {
	return errors.Is(err, ai.ErrDisabled) || errors.Is(err, ai.ErrGatewayNotConfigured)
}

// AIModelReranker 是 RerankClient 的真实实现：用模型网关做一次补全。
//
// 它只做两件与协议相关的事：拼 prompt、解析 JSON。**排序结果怎么用、
// 什么时候该放弃**全部留在 feed 包（消费方），于是这个类型可以在
// 完全不碰 Feed 的情况下被单测覆盖。
type AIModelReranker struct {
	client ai.Client
	model  string
}

// 编译期断言：接口漂移时立刻编译失败。
var _ RerankClient = (*AIModelReranker)(nil)

// NewAIModelReranker 构造一个基于模型网关的精排器。
//
// client 为 nil 时退化为 Noop：与既有的 NewEmbeddingBackfiller 同一个理由
// ——"AI 关闭"的部署不该在装配层多写一个 if。
func NewAIModelReranker(client ai.Client, model string) *AIModelReranker {
	if client == nil {
		client = ai.NewNoop()
	}
	return &AIModelReranker{client: client, model: model}
}

// Rerank 让模型给出候选的相关度顺序。
func (r *AIModelReranker) Rerank(ctx context.Context, accountID uint, candidates []RerankCandidate) ([]uint, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	req := ai.Request{
		System: rerankSystemPrompt,
		Prompt: buildRerankPrompt(accountID, candidates),
		// 精排是一个**判定**任务而不是创作任务：temperature 必须为 0
		// （不然同一批候选两次调用会给出不同顺序，用户会看到列表抖动），
		// 并要求 JSON 输出以便解析。
		Temperature: 0,
		JSON:        true,
		Model:       r.model,
		RequestID:   fmt.Sprintf("rerank-%d-%d", accountID, time.Now().UnixNano()),
	}
	res, err := r.client.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return parseRerankResponse(res.Text, candidates)
}

// rerankSystemPrompt 要求模型只做一件事：给候选排序。
//
// 刻意强调"只返回 JSON 数组、使用原始 id、不新增不遗漏"：这三句话
// 对应的三种失败模式都会让整次精排被丢弃（见 validateRerankResult），
// 而让模型照着格式回答比在代码里补救便宜得多。
const rerankSystemPrompt = `你是一个视频推荐排序器。你会收到若干候选视频，请按"这个用户最可能感兴趣"的顺序返回它们的 id。
只输出一个 JSON 对象，形如 {"ids":[3,1,2]}，ids 必须恰好包含全部候选 id、每个 id 只出现一次。
不要输出任何解释、不要新增或遗漏 id。`

// buildRerankPrompt 拼出候选列表。
//
// 字段选择的原则：**只给模型它真的能用上的信号**，且每个信号都必须是
// 数据里真实存在的（P2 §4.6 对理由的红线同样适用于这里——模型不能
// 凭空知道用户喜欢什么）。
func buildRerankPrompt(accountID uint, candidates []RerankCandidate) string {
	var b strings.Builder
	b.WriteString("用户 id：")
	b.WriteString(strconv.FormatUint(uint64(accountID), 10))
	b.WriteString("\n候选视频（id | 标题 | 描述 | 点赞数 | 发布时间）：\n")
	for _, c := range candidates {
		b.WriteString(strconv.FormatUint(uint64(c.ID), 10))
		b.WriteString(" | ")
		b.WriteString(truncateRunes(c.Title, rerankPreviewRunes))
		b.WriteString(" | ")
		b.WriteString(truncateRunes(c.Description, rerankPreviewRunes))
		b.WriteString(" | ")
		b.WriteString(strconv.FormatInt(c.LikesCount, 10))
		b.WriteString(" | ")
		b.WriteString(c.CreateTime.Format("2006-01-02"))
		b.WriteString("\n")
	}
	return b.String()
}

// parseRerankResponse 从模型输出里取出 id 顺序。
//
// 容错策略：只接受 `{"ids":[...]}`，但**允许**模型在 JSON 前后多写几个字
// （很多 provider 会带上 ```json 围栏或一句"好的"）。容错的上限是
// "截取第一段合法 JSON 对象"，而不是"用正则从文本里捞数字"——
// 后者会把解释文字里的数字（比如"共 3 条"）当成 id，
// 而那种错误的后果是整批校验失败、精排静默失效。
func parseRerankResponse(text string, candidates []RerankCandidate) ([]uint, error) {
	body := extractJSONObject(text)
	if body == "" {
		return nil, fmt.Errorf("feed: 精排响应里没有 JSON 对象")
	}
	var payload struct {
		IDs []json.RawMessage `json:"ids"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return nil, fmt.Errorf("feed: 解析精排响应失败: %w", err)
	}
	if len(payload.IDs) == 0 {
		return nil, fmt.Errorf("feed: 精排响应里 ids 为空")
	}
	out := make([]uint, 0, len(payload.IDs))
	for _, raw := range payload.IDs {
		id, err := parseSnowflakeishID(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := validateRerankResult(out, candidates); err != nil {
		return nil, err
	}
	return out, nil
}

// parseSnowflakeishID 接受数字或数字字符串两种写法。
//
// 为什么两种都收：模型经常把 id 写成字符串（"3" 而不是 3），
// 而这只影响解析，不影响语义。**不接受**浮点以外的形式（如 "id3"）：
// 那种输出说明模型没有理解任务，硬解释出来的顺序不可信。
func parseSnowflakeishID(raw json.RawMessage) (uint, error) {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	if s == "" {
		return 0, fmt.Errorf("feed: 精排返回了空 id")
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("feed: 精排返回的 id 不是整数: %q", s)
	}
	return uint(v), nil
}

// extractJSONObject 从一段文本里取出第一个完整的 JSON 对象。
//
// 用括号配对而不是正则：候选的描述里可能有花括号与引号，正则会在那里
// 提前结束或吃过头。配对扫描只看结构、不看内容。
func extractJSONObject(text string) string {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// 字符串内部的括号不参与配对。
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}

// truncateRunes 按 rune 截断，避免把多字节字符切成半个。
func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// sortReranked 是按精排顺序稳定排序的辅助函数（供测试与后续扩展使用）。
//
// 保留它的理由：当前实现走 applyRerankOrder（重排整个列表），
// 但如果将来只重排前 N 条、后面保持融合分，就需要"按给定 ID 顺序"的
// 稳定排序。把它写下来并测到，比那个时候临时加一段没有测试的排序安全。
func sortReranked(items []*video.Video, order []uint) {
	rank := make(map[uint]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, okI := rank[items[i].ID]
		rj, okJ := rank[items[j].ID]
		switch {
		case okI && okJ:
			return ri < rj
		case okI:
			return true
		case okJ:
			return false
		default:
			return false
		}
	})
}

// newRerankCache 构造精排结果缓存。
func newRerankCache() *cache.Cache {
	return cache.New(RerankCacheTTL, RerankCacheSweep)
}

// applyRerank 是 listLatestFused 里的精排入口。
//
// 它的实现刻意只有三行：所有判断、超时、校验、日志都在 rerankCandidates
// 与 noteRerankDegrade 里。这一段代码在用户请求路径上，越短越好读——
// 读它的人需要一眼看出"任何异常都只是保持原顺序"。
func (f *FeedService) applyRerank(ctx context.Context, viewerAccountID uint, items []*video.Video) []*video.Video {
	if !f.rerankEnabled || len(items) < 2 {
		return items
	}
	outcome := f.rerankCandidates(ctx, viewerAccountID, items)
	if outcome.Degraded {
		// 降级不是错误：D1 明确要求"超时立即退回融合分排序"。
		// 这里不额外记日志（rerankCandidates 已经记过带原因的 Warn 了），
		// 只把顺序原样返回。
		return items
	}
	return applyRerankOrder(items, outcome.Ranked)
}
