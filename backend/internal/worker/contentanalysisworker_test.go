package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"
	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// 本文件覆盖内容分析 worker 的**载荷处理与失败语义**，不覆盖 MQ 交互
// （那部分需要真实 broker，见 rabbitmq/content_analysis_integration_test.go）。
//
// 为什么值得单独测 process()：
// P0 的 worker 只记日志，看起来没什么可测的；但真正会被 P1 继承下来的是
// "什么算失败、什么算无事可做"这套判断。它一旦写错，表现是两种极端：
// 要么坏消息在队列里无限重投（刷爆日志与 CPU），
// 要么真实故障被静默 Ack 丢弃（数据静默丢失）。
// 这两种错误都不会让进程崩溃，所以只能靠测试发现。

// newTestWorker 造一个不连 MQ 的 worker：只测 process()。
func newTestWorker(handle HandlerFunc) *ContentAnalysisWorker {
	return &ContentAnalysisWorker{queue: "test", handle: handle}
}

func observeWorkerLogs(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(level)
	prev := logging.SetLogger(zap.New(core))
	t.Cleanup(func() { logging.SetLogger(prev) })
	return logs
}

// TestProcessDispatchesCompletePayload 守住"载荷被完整地交给 handler"。
//
// 只断言"handler 被调用了"是不够的：字段在中间被丢掉（比如漏了
// analysis_type）会让 P1 的打标逻辑静默走错分支。
func TestProcessDispatchesCompletePayload(t *testing.T) {
	var got rabbitmq.ContentAnalysisEvent
	called := 0
	w := newTestWorker(func(_ context.Context, evt rabbitmq.ContentAnalysisEvent) error {
		called++
		got = evt
		return nil
	})

	body, err := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID:      "evt-1",
		VideoID:      42,
		AuthorID:     7,
		Title:        "标题",
		Description:  "简介",
		AnalysisType: rabbitmq.AnalysisTypeText,
		OccurredAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}

	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("process 不该失败: %v", err)
	}
	if called != 1 {
		t.Fatalf("handler 调用次数 = %d, want 1", called)
	}
	if got.VideoID != 42 || got.AuthorID != 7 || got.EventID != "evt-1" {
		t.Errorf("标识字段丢失: %+v", got)
	}
	if got.Title != "标题" || got.Description != "简介" {
		t.Errorf("文本字段丢失: %+v", got)
	}
	if got.AnalysisType != rabbitmq.AnalysisTypeText {
		t.Errorf("analysis_type 丢失: %q", got.AnalysisType)
	}
}

// TestProcessWithoutHandlerLogsAndSucceeds 是 P0 默认路径的行为断言。
//
// 断言两件事：返回 nil（消息会被 Ack，而不是卡在队列里），
// 日志里带上了 video_id（"发布一条测试消息能被 worker 消费到"这条验收
// 就是靠肉眼核对这条日志完成的）。
func TestProcessWithoutHandlerLogsAndSucceeds(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	w := newTestWorker(nil)

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID:      "evt-2",
		VideoID:      99,
		Title:        "标题",
		AnalysisType: rabbitmq.AnalysisTypeText,
	})

	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("P0 默认路径必须成功（返回 error 会让消息被无限重投）: %v", err)
	}

	var found bool
	for _, e := range logs.All() {
		if e.ContextMap()["video_id"] == uint64(99) {
			found = true
			if e.Level == zap.WarnLevel || e.Level == zap.ErrorLevel {
				t.Errorf("P0 收到任务不该记 Warn/Error（那会让运维以为出了问题），实际 %v", e.Level)
			}
		}
	}
	if !found {
		t.Errorf("日志里应带上 video_id 以便核对链路，实际日志: %v", logs.All())
	}
}

// TestProcessBadJSONIsDroppedNotRetried 守住"坏消息不重试"。
//
// 重试一段坏字节永远不会成功，只会白耗 7 秒退避并刷日志。
// 返回 nil（= Ack 丢弃）才是正确行为，且必须留下一条 **Warn**——
// 静默丢弃一条上游发错的消息，会让"消息去哪了"变成无解问题。
func TestProcessBadJSONIsDroppedNotRetried(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.WarnLevel)
	w := newTestWorker(nil)

	if err := w.process(context.Background(), []byte("{不是 JSON")); err != nil {
		t.Fatalf("坏 JSON 不该返回 error（会触发无意义重试）: %v", err)
	}

	all := logs.All()
	if len(all) != 1 {
		t.Fatalf("期望恰好一条日志，实际 %d 条: %v", len(all), all)
	}
	if all[0].Level != zap.WarnLevel {
		t.Errorf("级别 = %v, want Warn（上游发坏消息是运维需要知道的）", all[0].Level)
	}
}

// TestProcessMissingVideoIDIsDropped 覆盖"载荷结构合法但不可用"。
//
// 没有 video_id 就无法定位到任何视频，重试多少次都一样。
func TestProcessMissingVideoIDIsDropped(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.WarnLevel)
	w := newTestWorker(func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		t.Error("缺少 video_id 时不该调用 handler")
		return nil
	})

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{Title: "有标题但没 id"})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if logs.Len() == 0 {
		t.Error("丢弃消息必须留下日志")
	}
}

// TestProcessSkipsWhenNoTextOnlyWithoutAnalyzer 覆盖"既没标题也没简介"。
//
// 这条规则**只在完全没有分析能力时**成立。P1 之后载荷里本来就不带文本
// （4.2：worker 自己回查，避免把长文本塞进 MQ），所以"文本为空"是正常形态，
// 不能成为跳过的理由——把判据写成"文本为空就跳过"会让 AI 静默失效。
func TestProcessSkipsWhenNoTextOnlyWithoutAnalyzer(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	// 刻意用 nil handler + nil analyzer：这才是"没有任何分析能力"的形态。
	// newTestWorker 会注入一个 handler（那是给别的用例用的），这里不能复用它。
	w := &ContentAnalysisWorker{queue: "test"}

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-3", VideoID: 1, AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("没有文本不是失败: %v", err)
	}
	for _, e := range logs.All() {
		if e.Level >= zap.WarnLevel {
			t.Errorf("跳过无文本消息不该记 %v 级别日志: %v", e.Level, e.Message)
		}
	}
}

// TestProcessHandlerErrorIsRetried 守住反方向：handler 报错必须传出去。
//
// 把真实失败（模型超时、DB 暂时写不进去）吞掉会让消息被静默 Ack，
// 表现为"AI 结果时有时无且无任何日志"。
func TestProcessHandlerErrorIsRetried(t *testing.T) {
	wantErr := errors.New("模型超时")
	w := newTestWorker(func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		return wantErr
	})

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-4", VideoID: 5, Title: "标题", AnalysisType: rabbitmq.AnalysisTypeText,
	})
	err := w.process(context.Background(), body)
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler 的错误必须原样传出，实际: %v", err)
	}
}

// TestRunRejectsUninitialized 覆盖装配错误（忘了传 channel / queue）。
//
// 这类错误在 compose 里表现为"worker 起来了但什么都没消费"，
// 是最难查的一类；让它在 Run 的第一行就响亮地失败。
func TestRunRejectsUninitialized(t *testing.T) {
	cases := []struct {
		name string
		w    *ContentAnalysisWorker
	}{
		{"nil channel", &ContentAnalysisWorker{queue: "q"}},
		{"empty queue", &ContentAnalysisWorker{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.w.Run(context.Background()); err == nil {
				t.Error("装配不完整时 Run 必须立刻报错")
			}
		})
	}

	var nilWorker *ContentAnalysisWorker
	if err := nilWorker.Run(context.Background()); err == nil {
		t.Error("nil worker 的 Run 必须报错而不是 panic")
	}
}

// ---------------------------------------------------------------------------
// 重试与丢弃：用假的 amqp.Acknowledger 覆盖，不需要真实 broker
// ---------------------------------------------------------------------------

// fakeAcker 记录 Ack/Nack 调用。
//
// 为什么值得写：worker 与项目里其它 worker 共享同一套契约
// （3 次重试 + 指数退避 + 耗尽后 Ack 丢弃）。这套逻辑写反了（例如耗尽后
// Nack requeue=true）不会让任何测试变红，却会让坏消息在队列里无限重投，
// 把队列变成雪球。review 指出这条路径此前完全没有测试覆盖。
type fakeAcker struct {
	acks    int
	nacks   int
	requeue []bool
}

func (f *fakeAcker) Ack(uint64, bool) error { f.acks++; return nil }
func (f *fakeAcker) Nack(_ uint64, _ bool, requeue bool) error {
	f.nacks++
	f.requeue = append(f.requeue, requeue)
	return nil
}
func (f *fakeAcker) Reject(uint64, bool) error { return nil }

func newDelivery(acker amqp.Acknowledger, body []byte) amqp.Delivery {
	return amqp.Delivery{
		Acknowledger: acker,
		Body:         body,
		DeliveryTag:  1,
	}
}

// ackBody 造一条合法的 text 分析事件。
func ackBody(t *testing.T, videoID uint) []byte {
	t.Helper()
	b, err := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-retry", VideoID: videoID, Title: "标题",
		AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}
	return b
}

// TestHandleDeliveryAcksOnSuccess 守住"成功即 Ack"。
func TestHandleDeliveryAcksOnSuccess(t *testing.T) {
	acker := &fakeAcker{}
	w := NewContentAnalysisWorker(nil, "q", func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		return nil
	})
	w.handleDelivery(context.Background(), newDelivery(acker, ackBody(t, 1)))

	if acker.acks != 1 || acker.nacks != 0 {
		t.Errorf("成功路径应当恰好 Ack 一次，实际 acks=%d nacks=%d", acker.acks, acker.nacks)
	}
}

// TestHandleDeliveryRetriesThenAcksAndDrops 守住最容易写反的那条：
// 重试 3 次之后 **Ack 丢弃**（而不是 Nack 回去无限循环）。
//
// 同时断言"总共尝试了 maxRetries+1 次"——少了是提前放弃，
// 多了是退避策略失效（会把一次模型抖动放大成十几次调用）。
func TestHandleDeliveryRetriesThenAcksAndDrops(t *testing.T) {
	attempts := 0
	w := NewContentAnalysisWorker(nil, "q", func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		attempts++
		return errors.New("模型超时")
	})
	// 把退避改成不等待：真实退避是 1s+2s+4s，跑满一次要 7 秒。
	w.sleep = func(time.Duration) {}

	acker := &fakeAcker{}
	w.handleDelivery(context.Background(), newDelivery(acker, ackBody(t, 2)))

	if attempts != 4 { // 首次 + 3 次重试
		t.Errorf("尝试次数 = %d, want 4（首次 + maxRetries 次重试）", attempts)
	}
	if acker.acks != 1 {
		t.Errorf("重试耗尽后必须 Ack 丢弃，实际 acks=%d", acker.acks)
	}
	if acker.nacks != 0 {
		t.Errorf("重试耗尽不该 Nack（那会让消息被无限重投），实际 nacks=%d", acker.nacks)
	}
}

// TestHandleDeliveryNacksWithRequeueOnShutdown 覆盖进程退出时的处理。
//
// 与"重试耗尽"必须区分开：进程退出时消息还没被处理过，
// Nack + requeue 让下一个实例捡起来；丢掉它才是真的丢数据。
func TestHandleDeliveryNacksWithRequeueOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	acked := &fakeAcker{}
	w := NewContentAnalysisWorker(nil, "q", func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		t.Error("ctx 已取消时不该再进入 handler")
		return nil
	})
	w.handleDelivery(ctx, newDelivery(acked, ackBody(t, 3)))

	if acked.nacks != 1 {
		t.Fatalf("ctx 取消时必须 Nack，实际 nacks=%d", acked.nacks)
	}
	if len(acked.requeue) != 1 || !acked.requeue[0] {
		t.Errorf("Nack 必须 requeue=true（消息要回到队列），实际 %v", acked.requeue)
	}
	if acked.acks != 0 {
		t.Errorf("ctx 取消时不该 Ack，实际 acks=%d", acked.acks)
	}
}

// TestProcessSkipsUnknownAnalysisType 守住 P0/P1 的兼容约定：
// 不认识的分析类型记日志后 **Ack**（返回 nil），而不是当失败重试。
//
// 滚动升级期间新旧版本必然共存：按"失败重试"处理会让每条新类型消息
// 都白跑 3 次退避并刷 4 条误导性日志。
func TestProcessSkipsUnknownAnalysisType(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	w := newTestWorker(func(context.Context, rabbitmq.ContentAnalysisEvent) error {
		t.Error("不认识的分析类型不该交给 handler")
		return nil
	})

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-future", VideoID: 9, Title: "标题", AnalysisType: "moderation",
	})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("未知分析类型不该触发重试，实际: %v", err)
	}
	var found bool
	for _, e := range logs.All() {
		if e.ContextMap()["analysis_type"] == "moderation" {
			found = true
			// Info 而不是 Warn：滚动升级期间这条日志会出现很多次，
			// 用 Warn 会把真正的告警淹没。
			if e.Level >= zap.WarnLevel {
				t.Errorf("未知类型应记 Info，实际 %v", e.Level)
			}
		}
	}
	if !found {
		t.Errorf("应留下一条带 analysis_type 的日志，实际: %v", logs.All())
	}
}

// TestProcessSkipsMissingAnalysisType 覆盖载荷缺类型字段（坏载荷）。
func TestProcessSkipsMissingAnalysisType(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.WarnLevel)
	w := newTestWorker(nil)

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{VideoID: 7, Title: "标题"})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("缺类型不该触发重试: %v", err)
	}
	if logs.Len() == 0 {
		t.Error("缺 analysis_type 必须留下日志，否则消息静默消失")
	}
}

// ---------------------------------------------------------------------------
// P1：分析器接入后的行为
// ---------------------------------------------------------------------------

// fakeAnalyzer 记录被要求分析的视频，并可脚本化返回错误。
type fakeAnalyzer struct {
	calls []uint
	err   error
}

func (f *fakeAnalyzer) Analyze(_ context.Context, videoID uint) error {
	f.calls = append(f.calls, videoID)
	return f.err
}

func analyzableBody(t *testing.T, videoID uint) []byte {
	t.Helper()
	b, err := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-analyze", VideoID: videoID, AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}
	return b
}

// TestProcessCallsAnalyzerWhenWired 是 P1 的主路径：
// worker 不再只记日志，而是真的去分析。
func TestProcessCallsAnalyzerWhenWired(t *testing.T) {
	analyzer := &fakeAnalyzer{}
	w := newTestWorker(nil).WithAnalyzer(analyzer, nil)

	if err := w.process(context.Background(), analyzableBody(t, 42)); err != nil {
		t.Fatalf("分析成功时不该报错: %v", err)
	}
	if len(analyzer.calls) != 1 || analyzer.calls[0] != 42 {
		t.Errorf("分析器调用 = %v, want [42]", analyzer.calls)
	}
}

// TestProcessAcksWhenAnalyzerReportsNoResult 守住 4.6 的降级语义：
// "没有结果但也不是故障"（AI 关闭、视频已删、无文本）必须 **Ack 丢弃**，
// 而不是走 3 次退避重试。
//
// 判错方向的代价很具体：AI 关闭的部署里，每发布一个视频就会白跑
// 1+2+4=7 秒退避并刷 4 条日志——一个纯属自伤的噪音源。
func TestProcessAcksWhenAnalyzerReportsNoResult(t *testing.T) {
	analyzer := &fakeAnalyzer{err: video.ErrNoAnalysis}
	w := newTestWorker(nil).WithAnalyzer(analyzer, video.IsNoAnalysis)

	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	if err := w.process(context.Background(), analyzableBody(t, 42)); err != nil {
		t.Fatalf("'没有结果'不该触发重试，实际返回: %v", err)
	}
	// 必须留一条痕迹（否则消息静默消失），但级别不能是 Warn/Error。
	for _, e := range logs.All() {
		if e.Level >= zap.WarnLevel {
			t.Errorf("AI 关闭属于预期状态，不该记 %v 级别日志: %v", e.Level, e.Message)
		}
	}
}

// TestProcessRetriesWhenAnalyzerFails 反方向：真失败必须传出错误触发重试。
func TestProcessRetriesWhenAnalyzerFails(t *testing.T) {
	wantErr := errors.New("模型超时")
	analyzer := &fakeAnalyzer{err: wantErr}
	w := newTestWorker(nil).WithAnalyzer(analyzer, video.IsNoAnalysis)

	err := w.process(context.Background(), analyzableBody(t, 42))
	if !errors.Is(err, wantErr) {
		t.Fatalf("真失败必须传给调用方（触发重试），实际: %v", err)
	}
}

// TestProcessRetriesThenDropsOnAnalyzerFailure 端到端守一遍重试+丢弃：
// 分析器持续失败时，消息最终被 Ack 丢弃（不会无限重投）。
func TestProcessRetriesThenDropsOnAnalyzerFailure(t *testing.T) {
	analyzer := &fakeAnalyzer{err: errors.New("模型持续超时")}
	w := newTestWorker(nil).WithAnalyzer(analyzer, video.IsNoAnalysis)
	w.sleep = func(time.Duration) {}

	acker := &fakeAcker{}
	w.handleDelivery(context.Background(), newDelivery(acker, analyzableBody(t, 42)))

	if len(analyzer.calls) != 4 { // 首次 + 3 次重试
		t.Errorf("分析尝试次数 = %d, want 4", len(analyzer.calls))
	}
	if acker.acks != 1 || acker.nacks != 0 {
		t.Errorf("重试耗尽应 Ack 丢弃，实际 acks=%d nacks=%d", acker.acks, acker.nacks)
	}
}

// TestProcessWithoutAnalyzerStillLogs 守住 P0 行为的向后兼容：
// 未注入分析器时只记日志（AI 关闭的部署）。
func TestProcessWithoutAnalyzerStillLogs(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	w := newTestWorker(nil) // 没有 analyzer

	if err := w.process(context.Background(), analyzableBody(t, 42)); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if logs.Len() == 0 {
		t.Error("未启用时也应留下一条日志（证明链路是通的）")
	}
}

// TestProcessDoesNotSkipEmptyTextWhenAnalyzerPresent 是真实联调踩到的缺陷的回归测试。
//
// 缺陷（P0 遗留，P1 已删）：process 里有一条
//
//	if evt.Title == "" && evt.Description == "" { 跳过 }
//
// 的短路。P0 时载荷里带着文本，这条判断看起来合理；但 P1 改成
// "worker 自己回查、载荷不带长文本"之后，它对**每一条**真实消息都成立，
// 于是分析器一次都不会被调用：AI 静默失效，日志上只留下一句
// "无文本内容，跳过"，看不出任何异常。真库联调才发现。
//
// 这条用例直接钉住修好之后的行为：载荷不带文本时，分析器**必须**被调用。
func TestProcessDoesNotSkipEmptyTextWhenAnalyzerPresent(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	analyzer := &fakeAnalyzer{}
	// 生产装配形态：analyzer 非 nil、handle 为 nil、载荷不带文本。
	w := NewContentAnalysisWorker(nil, "q", nil).WithAnalyzer(analyzer, nil)

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-empty-text", VideoID: 88, AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "无文本内容") {
			t.Fatalf("这条日志不该再存在：它的判据（载荷里有没有文本）与"+
				"真实载荷形态不符，会让 AI 静默失效：%v", e.Message)
		}
	}
	if len(analyzer.calls) != 1 {
		t.Fatalf("分析器必须被调用，实际 %v", analyzer.calls)
	}
}

// TestProcessWithoutAnalyzerSaysAIUnavailable 守住"关闭 AI 时的日志说的是真话"。
//
// 删掉文本短路之前，这条路径会打出"无文本内容，跳过"——一个把
// "AI 没开"说成"没有内容可分析"的错误答案。运维照着这句话去查视频内容，
// 方向完全错了；而正确答案（ai.enabled=false）就在配置里。
func TestProcessWithoutAnalyzerSaysAIUnavailable(t *testing.T) {
	logs := observeWorkerLogs(t, zapcore.InfoLevel)
	// 生产上 AI 关闭时的装配形态：analyzer 为 nil，handle 也是 nil。
	w := NewContentAnalysisWorker(nil, "q", nil)

	body, _ := json.Marshal(rabbitmq.ContentAnalysisEvent{
		EventID: "evt-ai-off", VideoID: 89, AnalysisType: rabbitmq.AnalysisTypeText,
	})
	if err := w.process(context.Background(), body); err != nil {
		t.Fatalf("AI 关闭不是故障，不该触发重试: %v", err)
	}
	var said bool
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "AI 未启用") {
			said = true
			// Info 而不是 Warn：这是配置决定的正常状态，
			// 用 Warn 会让所有关掉 AI 的部署持续刷告警。
			if e.Level >= zap.WarnLevel {
				t.Errorf("AI 关闭是正常状态，应为 Info，实际 %v", e.Level)
			}
		}
		if strings.Contains(e.Message, "无文本内容") {
			t.Errorf("日志把'AI 没开'说成了'没有内容可分析'：%v", e.Message)
		}
	}
	if !said {
		t.Errorf("必须留下一条说明 AI 未启用的日志，实际: %v", logs.All())
	}
}
