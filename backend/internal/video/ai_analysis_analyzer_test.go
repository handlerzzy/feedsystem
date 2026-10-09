package video

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/ai"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 P1 的编排层：回查 -> 调模型 -> 校验 -> 幂等入库。
//
// 全部跑在假实现上：假模型客户端（可脚本化返回）与假存储
// （记录写入了什么）。CI 不需要 sidecar、不需要 API key、不需要 MySQL ——
// 这是总览第 8 节"模型调用有可替换的假实现"的实现方式。

// fakeStore 是 VideoAnalysisStore 的假实现。
type fakeStore struct {
	video *AnalysisInput
	// loadErr 非 nil 时模拟回查失败（含 ErrVideoNotFound）。
	loadErr error

	saved       []*VideoAIAnalysis
	savedTags   [][]TagSuggestion
	saveErr     error
	saveCalls   int
	upsertCalls int
	attachCalls int
}

func (f *fakeStore) LoadVideoForAnalysis(context.Context, uint) (*AnalysisInput, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.video, nil
}

func (f *fakeStore) UpsertAnalysis(context.Context, *VideoAIAnalysis) (uint, error) {
	f.upsertCalls++
	return 0, f.saveErr
}

func (f *fakeStore) AttachTags(context.Context, uint, []TagSuggestion) error {
	f.attachCalls++
	return f.saveErr
}

func (f *fakeStore) SaveAnalysisWithTags(_ context.Context, a *VideoAIAnalysis, tags []TagSuggestion) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, a)
	f.savedTags = append(f.savedTags, tags)
	return nil
}

// fakeClient 是 ai.Client 的假实现。
type fakeClient struct {
	text string
	err  error
	// seenReq 记录最后一次请求，用于断言提示词与超时。
	seenReq ai.Request
	calls   int
	delay   time.Duration
}

func (f *fakeClient) Complete(ctx context.Context, req ai.Request) (ai.Result, error) {
	f.calls++
	f.seenReq = req
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ai.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return ai.Result{}, f.err
	}
	return ai.Result{Text: f.text, Model: "test-model"}, nil
}

func (f *fakeClient) Health(context.Context) (ai.Health, error) { return ai.Health{Serving: true}, nil }
func (f *fakeClient) Close() error                              { return nil }

func validOutput() string {
	return `{"summary":"一只猫在打盹","tags":[{"name":"猫","confidence":0.9},{"name":"宠物","confidence":0.8}]}`
}

func newAnalyzer(store VideoAnalysisStore, client ai.Client) *VideoAIAnalyzer {
	return NewVideoAIAnalyzer(AnalyzerOptions{
		Store:  store,
		Client: client,
		Model:  "test-model",
		// PromptVersion 留空 → 应自动取 PromptVersion 常量（覆盖兜底逻辑）。
		Timeout: 50 * time.Millisecond,
	})
}

// TestAnalyzeWritesNormalizedResult 是 P1 主路径。
func TestAnalyzeWritesNormalizedResult(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫的一天", Description: "记录家里的猫"}}
	client := &fakeClient{text: validOutput()}
	a := newAnalyzer(store, client)

	if err := a.Analyze(context.Background(), 7); err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if store.saveCalls != 1 {
		t.Fatalf("应恰好写入一次，实际 %d", store.saveCalls)
	}

	saved := store.saved[0]
	if saved.VideoID != 7 || saved.Status != AnalysisStatusOK {
		t.Errorf("分析行不对: %+v", saved)
	}
	if saved.PromptVersion != PromptVersion {
		t.Errorf("prompt_version 应兜底为 %q，实际 %q", PromptVersion, saved.PromptVersion)
	}
	if saved.Model != "test-model" {
		t.Errorf("model 应取响应里的模型名，实际 %q", saved.Model)
	}
	if saved.Summary != "一只猫在打盹" {
		t.Errorf("summary = %q", saved.Summary)
	}
	if len(store.savedTags[0]) != 2 {
		t.Errorf("标签应被写入，实际 %+v", store.savedTags[0])
	}
	// JSON 标志必须打开：sidecar 据此追加"只输出 JSON"的约束。
	if !client.seenReq.JSON {
		t.Error("必须要求结构化输出（JSON=true）")
	}
	if client.seenReq.RequestID == "" {
		t.Error("必须带 request_id，否则 Go 日志与 sidecar 日志对不上")
	}
	// 请求里不该出现作者身份等无关信息（PII 面）。
	if strings.Contains(client.seenReq.Prompt, "author") {
		t.Error("提示词不该包含作者信息")
	}
}

// TestAnalyzeRejectsInvalidJSONWithoutWriting 是总览第 4 节的验收项：
// 模型返回非法 JSON 时**不写脏数据**，只返回错误让 worker 去重试/丢弃。
func TestAnalyzeRejectsInvalidJSONWithoutWriting(t *testing.T) {
	cases := map[string]string{
		"纯文本":      "这只猫很可爱",
		"缺 tags":   `{"summary":"只有摘要"}`,
		"空标签数组":    `{"summary":"x","tags":[]}`,
		"低置信度全部过滤": `{"summary":"x","tags":[{"name":"猫","confidence":0.1}]}`,
		"截断":       `{"summary":"x","tags":[{"name":"猫"`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫"}}
			a := newAnalyzer(store, &fakeClient{text: text})

			err := a.Analyze(context.Background(), 7)
			if err == nil {
				t.Fatal("非法输出必须报错")
			}
			// 关键断言：一个字节都不能写进去。
			if store.saveCalls != 0 {
				t.Errorf("非法输出时不该调用写入，实际 %d 次", store.saveCalls)
			}
		})
	}
}

// TestAnalyzeNoTextIsNotFailure 覆盖"视频没有文本"。
//
// 必须归为 ErrNoAnalysis（worker 会 Ack 丢弃），而不是普通错误（会退避重试 7 秒）。
func TestAnalyzeNoTextIsNotFailure(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "   ", Description: ""}}
	client := &fakeClient{text: validOutput()}
	a := newAnalyzer(store, client)

	err := a.Analyze(context.Background(), 7)
	if !errors.Is(err, ErrNoAnalysis) {
		t.Fatalf("应返回 ErrNoAnalysis，实际: %v", err)
	}
	if client.calls != 0 {
		t.Error("没有文本时不该调用模型（白花钱）")
	}
	if store.saveCalls != 0 {
		t.Error("不该写入任何数据")
	}
}

// TestAnalyzeMissingVideoIsNotFailure 覆盖"视频已被作者删除"这个正常竞态。
func TestAnalyzeMissingVideoIsNotFailure(t *testing.T) {
	store := &fakeStore{loadErr: ErrVideoNotFound}
	client := &fakeClient{text: validOutput()}
	a := newAnalyzer(store, client)

	err := a.Analyze(context.Background(), 7)
	if !errors.Is(err, ErrNoAnalysis) {
		t.Fatalf("视频不存在应归为 ErrNoAnalysis，实际: %v", err)
	}
	if client.calls != 0 {
		t.Error("视频不存在时不该调用模型")
	}
}

// TestAnalyzeDisabledClientIsNotFailure 守住 4.6：ai.enabled=false 时不调模型、不写库，
// 而且这件事**不是故障**（否则关闭 AI 的部署会持续刷失败日志）。
func TestAnalyzeDisabledClientIsNotFailure(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫"}}
	// P0 的 Noop 就是"AI 关闭"的形态：恒定返回 ErrDisabled。
	a := newAnalyzer(store, ai.Noop{})

	err := a.Analyze(context.Background(), 7)
	if !errors.Is(err, ErrNoAnalysis) {
		t.Fatalf("关闭状态应归为 ErrNoAnalysis，实际: %v", err)
	}
	if store.saveCalls != 0 {
		t.Error("关闭状态不该写任何数据")
	}
}

// TestAnalyzeNilClientIsNotFailure 覆盖装配方直接传 nil 客户端（另一种关闭形态）。
func TestAnalyzeNilClientIsNotFailure(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫"}}
	a := newAnalyzer(store, nil)

	if err := a.Analyze(context.Background(), 7); !errors.Is(err, ErrNoAnalysis) {
		t.Fatalf("应返回 ErrNoAnalysis，实际: %v", err)
	}
}

// TestAnalyzeProviderFailureIsRetryable 覆盖真失败：必须**不是** ErrNoAnalysis，
// 让 worker 走退避重试。
func TestAnalyzeProviderFailureIsRetryable(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫"}}
	a := newAnalyzer(store, &fakeClient{err: ai.ErrTimeout})

	err := a.Analyze(context.Background(), 7)
	if err == nil {
		t.Fatal("模型失败必须报错")
	}
	if errors.Is(err, ErrNoAnalysis) {
		t.Error("模型超时是可重试的失败，不该被归为'没有结果也不是故障'")
	}
	if !errors.Is(err, ai.ErrTimeout) {
		t.Errorf("原始错误必须能被 errors.Is 判定（worker 靠 ai.StatusFor 分级），实际: %v", err)
	}
	if store.saveCalls != 0 {
		t.Error("失败时不该写入")
	}
}

// TestAnalyzeStoreFailureIsRetryable 覆盖数据库暂时写不进去。
func TestAnalyzeStoreFailureIsRetryable(t *testing.T) {
	store := &fakeStore{
		video:   &AnalysisInput{VideoID: 7, Title: "猫"},
		saveErr: errors.New("mysql: connection refused"),
	}
	a := newAnalyzer(store, &fakeClient{text: validOutput()})

	err := a.Analyze(context.Background(), 7)
	if err == nil {
		t.Fatal("写入失败必须报错")
	}
	if errors.Is(err, ErrNoAnalysis) {
		t.Error("写入失败要重试，不该被归为'没有结果'")
	}
}

// TestAnalyzeHonoursTimeout 守住"调用模型必须带 deadline"。
//
// 一个慢到超过预算的模型调用必须返回错误，而不是把 worker 的消费循环拖住。
func TestAnalyzeHonoursTimeout(t *testing.T) {
	store := &fakeStore{video: &AnalysisInput{VideoID: 7, Title: "猫"}}
	// 客户端慢于分析器的 50ms 预算。
	a := newAnalyzer(store, &fakeClient{text: validOutput(), delay: 2 * time.Second})

	start := time.Now()
	err := a.Analyze(context.Background(), 7)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时必须报错")
	}
	if elapsed > time.Second {
		t.Errorf("耗时 %v，超时没有生效（模型调用没有带 deadline）", elapsed)
	}
	if store.saveCalls != 0 {
		t.Error("超时时不该写入")
	}
}

// TestAnalyzeZeroVideoID 覆盖载荷异常（worker 侧已过滤，这里是第二道防线）。
func TestAnalyzeZeroVideoID(t *testing.T) {
	a := newAnalyzer(&fakeStore{}, &fakeClient{text: validOutput()})
	if err := a.Analyze(context.Background(), 0); !errors.Is(err, ErrNoAnalysis) {
		t.Fatalf("video_id=0 应归为 ErrNoAnalysis，实际: %v", err)
	}
}

// TestNewVideoAIAnalyzerFillsDefaults 覆盖构造期的宽松兜底。
//
// 这些兜底很重要：worker 的启动不该因为一个可选功能的配置项写错而失败。
func TestNewVideoAIAnalyzerFillsDefaults(t *testing.T) {
	a := NewVideoAIAnalyzer(AnalyzerOptions{Store: &fakeStore{}})
	if a.promptVersion != PromptVersion {
		t.Errorf("promptVersion = %q, want %q", a.promptVersion, PromptVersion)
	}
	if a.minConfidence != DefaultMinConfidence {
		t.Errorf("minConfidence = %v, want %v", a.minConfidence, DefaultMinConfidence)
	}
	if a.timeout != ai.DefaultTimeout {
		t.Errorf("timeout = %v, want %v", a.timeout, ai.DefaultTimeout)
	}

	// 越界阈值同样兜底：0.9 是合法的，1.5 不是。
	a2 := NewVideoAIAnalyzer(AnalyzerOptions{Store: &fakeStore{}, MinConfidence: 0.9})
	if a2.minConfidence != 0.9 {
		t.Errorf("合法阈值不该被覆盖，实际 %v", a2.minConfidence)
	}
	a3 := NewVideoAIAnalyzer(AnalyzerOptions{Store: &fakeStore{}, MinConfidence: 1.5})
	if a3.minConfidence != DefaultMinConfidence {
		t.Errorf("越界阈值应兜底，实际 %v", a3.minConfidence)
	}
}
