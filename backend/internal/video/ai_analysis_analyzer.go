package video

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// VideoAIAnalyzer 是 P1 的编排层：回查视频 -> 调模型 -> 校验 -> 幂等入库。
//
// 为什么放在 internal/video 而不是 internal/worker：
// 它依赖的东西（AnalysisInput 回查、VideoAIAnalysis 实体、标签归一化）
// 全部属于 video 这个 domain。worker 只认识一个窄接口（Analyzer），
// 于是"模型网关怎么调、结果怎么写"这件事不需要 worker 知道，
// worker 依旧只负责"消费、重试、丢弃"这套与业务无关的骨架。
type VideoAIAnalyzer struct {
	store VideoAnalysisStore
	// client 为 nil 表示 AI 关闭（P0 的 Noop 是另一种关闭形态：
	// 非 nil 但恒定返回 ErrDisabled）。两种都要能正确退化为"什么都不做"。
	client ai.Client

	model         string
	promptVersion string
	minConfidence float64
	// timeout 是单次模型调用的兜底超时。
	//
	// 必须由调用方（worker）注入而不是写死：worker 的重试退避是秒级，
	// 而这里如果允许多等，一次卡住的调用会把 worker 的整个消费循环拖住。
	timeout time.Duration
}

// AnalyzerOptions 是构造分析器的参数。
type AnalyzerOptions struct {
	Store         VideoAnalysisStore
	Client        ai.Client
	Model         string
	PromptVersion string
	MinConfidence float64
	Timeout       time.Duration
}

// NewVideoAIAnalyzer 构造分析器。
//
// 它对参数做"宽松兜底"而不是报错：这个对象的生命周期在 worker 里，
// 而 worker 的启动不该因为一个可选功能的配置项写错而失败。
// 缺省值都在这里补齐，调用方（cmd/worker/main.go）可以只传必要的两个。
func NewVideoAIAnalyzer(opts AnalyzerOptions) *VideoAIAnalyzer {
	a := &VideoAIAnalyzer{
		store:         opts.Store,
		client:        opts.Client,
		model:         strings.TrimSpace(opts.Model),
		promptVersion: strings.TrimSpace(opts.PromptVersion),
		minConfidence: opts.MinConfidence,
		timeout:       opts.Timeout,
	}
	if a.promptVersion == "" {
		a.promptVersion = PromptVersion
	}
	if a.minConfidence <= 0 || a.minConfidence > 1 {
		a.minConfidence = DefaultMinConfidence
	}
	if a.timeout <= 0 {
		a.timeout = ai.DefaultTimeout
	}
	return a
}

// 编译期断言：分析器必须满足 worker 需要的那个窄接口。
//
// 接口在消费方（internal/worker）声明，这里断言的是"我确实满足它"。
var _ AnalyzerContract = (*VideoAIAnalyzer)(nil)

// AnalyzerContract 与 worker.Analyzer 同形。
//
// 在两边各声明一次是刻意的：internal/video 不该 import internal/worker
// （worker 已经 import video，反向引用会成环）。这里的断言保证
// "签名漂移时立刻编译失败"，而不是等到装配处才报错。
type AnalyzerContract interface {
	Analyze(ctx context.Context, videoID uint) error
}

// ErrNoAnalysis 表示这次分析**没有产生任何结果**，但也不是故障。
//
// 三种情况会返回它，worker 应当 Ack 丢弃而不是重试：
//  1. AI 已关闭（ai.enabled=false 或注入了 Noop）；
//  2. 视频已被删除；
//  3. 视频既没有标题也没有简介（没有可分析的文本）。
//
// 与"真的失败"分开的理由：失败要退避重试 3 次并最终记 Error，
// 而这三种情况重试一万次也不会有不同结果——把它们混进失败路径，
// 日志里就会出现大量"AI 分析失败"的假告警。
var ErrNoAnalysis = errors.New("video: 本次没有可写入的分析结果")

// Analyze 对一个视频执行一次完整分析。
func (a *VideoAIAnalyzer) Analyze(ctx context.Context, videoID uint) error {
	if a == nil || a.store == nil {
		return errors.New("video: 分析器未正确初始化")
	}
	if videoID == 0 {
		return fmt.Errorf("%w: video_id 为 0", ErrNoAnalysis)
	}
	if a.client == nil {
		// 配置层没给客户端（例如 ai.enabled=false 时装配方直接传 nil）。
		// 这是预期状态，故不记日志：它每条消息都会发生，记了就是刷屏。
		return fmt.Errorf("%w: AI 未启用", ErrNoAnalysis)
	}

	input, err := a.store.LoadVideoForAnalysis(ctx, videoID)
	if err != nil {
		if errors.Is(err, ErrVideoNotFound) {
			// 视频在入队之后被作者删除：属于正常竞态，不是故障。
			// 由 worker 侧统一决定日志级别（见 worker 的注释）。
			return fmt.Errorf("%w: 视频 %d 已不存在", ErrNoAnalysis, videoID)
		}
		return fmt.Errorf("回查视频 %d: %w", videoID, err)
	}
	if strings.TrimSpace(input.Title) == "" && strings.TrimSpace(input.Description) == "" {
		return fmt.Errorf("%w: 视频 %d 没有任何文本内容", ErrNoAnalysis, videoID)
	}

	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// RequestID 用 video_id 派生：它让 Go 日志与 sidecar 日志能对起来，
	// 同时不含任何用户内容（标题/简介都不进 request_id）。
	requestID := fmt.Sprintf("video-analysis:%d:%s", videoID, a.promptVersion)

	start := time.Now()
	res, err := a.client.Complete(callCtx, ai.Request{
		System:    SystemPrompt,
		Prompt:    BuildAnalysisPrompt(input),
		JSON:      true,
		Model:     a.model,
		RequestID: requestID,
	})
	if err != nil {
		// 透传原始错误：调用方（worker）按 ai.StatusFor 分级，
		// 这里再包一层只会让 StatusFor 判不出超时/不可达。
		//
		// 已关闭（ErrDisabled）也算"没有结果"，不是故障——否则
		// ai.enabled=false 时每条消息都会走进重试路径。
		if errors.Is(err, ai.ErrDisabled) || errors.Is(err, ai.ErrGatewayNotConfigured) {
			return fmt.Errorf("%w: %v", ErrNoAnalysis, err)
		}
		return fmt.Errorf("调用模型失败（video_id=%d）: %w", videoID, err)
	}

	out, err := ParseModelOutput(res.Text, a.minConfidence)
	if err != nil {
		// 非法输出**不入库**（总览第 4 节）。返回 error 让 worker 走重试：
		// 模型偶尔返回坏 JSON 是概率事件，重试一次往往就好了；
		// 重试仍失败则 Ack 丢弃并记 Error（与 popularityworker 一致）。
		return fmt.Errorf("模型输出不可用（video_id=%d）: %w", videoID, err)
	}

	model := res.Model
	if model == "" {
		model = a.model
	}
	if model == "" {
		// 对端没回填模型名且本地也没配：用一个显式占位而不是空串。
		// 幂等键包含 model，空串会让"换模型后重跑"变成覆盖旧行而不是新增一行。
		model = "unknown"
	}

	analysis := &VideoAIAnalysis{
		VideoID:       videoID,
		Model:         model,
		PromptVersion: a.promptVersion,
		Summary:       out.Summary,
		TagsJSON:      out.TagsJSON(),
		Status:        AnalysisStatusOK,
		AnalyzedAt:    time.Now(),
	}
	if err := a.store.SaveAnalysisWithTags(ctx, analysis, out.Tags); err != nil {
		return fmt.Errorf("写入分析结果失败（video_id=%d）: %w", videoID, err)
	}

	// 成功路径记 Info：它是"AI 真的产出了东西"的唯一证据，
	// 排查"为什么某个视频没有标签"时全靠它。
	logging.Ctx(ctx).Info("AI 分析完成",
		zap.Uint("video_id", videoID),
		zap.String("model", model),
		zap.String("prompt_version", a.promptVersion),
		zap.Int("tags", len(out.Tags)),
		zap.Int("summary_len", len([]rune(out.Summary))),
		zap.Duration("latency", time.Since(start)),
	)
	return nil
}

// IsNoAnalysis 报告一个错误是否表示"没有结果但也不是故障"。
//
// 单独导出给 worker 用：worker 不该 import 这里的 errors.Is 细节，
// 但必须能区分"重试"与"Ack 丢弃"。
func IsNoAnalysis(err error) bool { return errors.Is(err, ErrNoAnalysis) }
