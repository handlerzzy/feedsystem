package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/video"
)

// 本文件覆盖 P2 §4.4 精排的**每一条降级路径**。
//
// P2 的验收项写得很直接：
//
//	人为让精排超时，Feed 仍正常返回（顺序为融合分）。
//
// 因此下面每一条都是一个独立用例：超时、报错、结果不合法、候选不足、
// 开关关闭、客户端缺失。它们的共同出口是"保持原顺序"，
// 而任何一条走错的表现都是"用户请求被模型拖住"——那是最不能接受的失败。

func rerankVideos(ids ...uint) []*video.Video {
	out := make([]*video.Video, 0, len(ids))
	for _, id := range ids {
		out = append(out, semanticVideo(id))
	}
	return out
}

// scriptedReranker 按脚本返回顺序或错误，并记录被调用时的上下文状态。
type scriptedReranker struct {
	ranked  []uint
	err     error
	delay   time.Duration
	calls   int
	lastCtx context.Context
}

func (s *scriptedReranker) Rerank(ctx context.Context, _ uint, _ []RerankCandidate) ([]uint, error) {
	s.calls++
	s.lastCtx = ctx
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ranked, s.err
}

func newRerankService(t *testing.T, client RerankClient, enabled bool, timeout time.Duration, topN int) *FeedService {
	t.Helper()
	svc, _ := newSemanticService(t, &fakeVectorStore{}, 0)
	svc.WithRerank(RerankOptions{Client: client, Enabled: enabled, Timeout: timeout, TopN: topN})
	return svc
}

func idsOfFeedVideos(videos []*video.Video) []uint {
	out := make([]uint, 0, len(videos))
	for _, v := range videos {
		out = append(out, v.ID)
	}
	return out
}

func TestRerankDisabledKeepsOrderAndMakesNoCall(t *testing.T) {
	client := &scriptedReranker{ranked: []uint{3, 2, 1}}
	svc := newRerankService(t, client, false, 50*time.Millisecond, 3)
	items := rerankVideos(1, 2, 3)

	got := svc.applyRerank(context.Background(), 7, items)
	if !equalIDs(idsOfFeedVideos(got), []uint{1, 2, 3}) {
		t.Fatalf("关闭时顺序被改动了: %v", idsOfFeedVideos(got))
	}
	if client.calls != 0 {
		t.Fatalf("关闭时仍然调用了模型 %d 次", client.calls)
	}
}

func TestRerankAppliesModelOrder(t *testing.T) {
	client := &scriptedReranker{ranked: []uint{3, 1, 2}}
	svc := newRerankService(t, client, true, time.Second, 3)
	items := rerankVideos(1, 2, 3)

	got := svc.applyRerank(context.Background(), 7, items)
	if !equalIDs(idsOfFeedVideos(got), []uint{3, 1, 2}) {
		t.Fatalf("精排顺序没有被应用: %v", idsOfFeedVideos(got))
	}
	if len(got) != len(items) {
		t.Fatalf("精排改变了列表长度: %d -> %d", len(items), len(got))
	}
}

func TestRerankTimeoutFallsBackToFusedOrder(t *testing.T) {
	// 模型比超时慢：必须立刻放弃，绝不让用户请求等待模型。
	client := &scriptedReranker{ranked: []uint{3, 2, 1}, delay: 2 * time.Second}
	svc := newRerankService(t, client, true, 20*time.Millisecond, 3)
	items := rerankVideos(1, 2, 3)

	started := time.Now()
	got := svc.applyRerank(context.Background(), 7, items)
	elapsed := time.Since(started)

	if !equalIDs(idsOfFeedVideos(got), []uint{1, 2, 3}) {
		t.Fatalf("超时后没有退回融合分顺序: %v", idsOfFeedVideos(got))
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("超时后等了 %v 才返回：精排把用户请求拖住了", elapsed)
	}
	if svc.rerankDegradedCount() == 0 {
		t.Fatal("超时没有计入降级计数：验收项要求'只开开关不调超时'能立刻被观察到")
	}
}

func TestRerankErrorFallsBackToFusedOrder(t *testing.T) {
	client := &scriptedReranker{err: errors.New("provider 500")}
	svc := newRerankService(t, client, true, time.Second, 3)

	got := svc.applyRerank(context.Background(), 7, rerankVideos(1, 2, 3))
	if !equalIDs(idsOfFeedVideos(got), []uint{1, 2, 3}) {
		t.Fatalf("模型报错后没有退回融合分顺序: %v", idsOfFeedVideos(got))
	}
	if svc.rerankDegradedCount() != 1 {
		t.Fatalf("降级计数 = %d, want 1", svc.rerankDegradedCount())
	}
}

func TestRerankRejectsInvalidResults(t *testing.T) {
	cases := []struct {
		name   string
		ranked []uint
	}{
		{"条数少了", []uint{3, 1}},
		{"条数多了", []uint{3, 1, 2, 9}},
		{"包含候选之外的 id", []uint{3, 1, 99}},
		{"有重复 id", []uint{3, 3, 1}},
		{"返回空", []uint{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedReranker{ranked: tc.ranked}
			svc := newRerankService(t, client, true, time.Second, 3)

			got := svc.applyRerank(context.Background(), 7, rerankVideos(1, 2, 3))
			if !equalIDs(idsOfFeedVideos(got), []uint{1, 2, 3}) {
				t.Fatalf("不合法的精排结果被采纳了: %v", idsOfFeedVideos(got))
			}
			if len(got) != 3 {
				t.Fatalf("列表被改坏: %v（模型不该能增删候选）", idsOfFeedVideos(got))
			}
		})
	}
}

func TestRerankSkipsWhenTooFewCandidates(t *testing.T) {
	client := &scriptedReranker{ranked: []uint{1}}
	svc := newRerankService(t, client, true, time.Second, 3)

	got := svc.applyRerank(context.Background(), 7, rerankVideos(1))
	if len(got) != 1 {
		t.Fatalf("got %v", idsOfFeedVideos(got))
	}
	if client.calls != 0 {
		t.Fatal("只有一条候选时不该调用模型：一次注定无意义的调用只会白花成本与延迟")
	}
}

func TestRerankHonoursTopNAndKeepsRestInOrder(t *testing.T) {
	// 只重排前 2 条，第 3 条之后保持融合分顺序。
	client := &scriptedReranker{ranked: []uint{2, 1}}
	svc := newRerankService(t, client, true, time.Second, 2)
	items := rerankVideos(1, 2, 3, 4)

	got := svc.applyRerank(context.Background(), 7, items)
	if !equalIDs(idsOfFeedVideos(got), []uint{2, 1, 3, 4}) {
		t.Fatalf("got %v, want [2 1 3 4]", idsOfFeedVideos(got))
	}
}

func TestRerankCachesResultByUserAndCandidates(t *testing.T) {
	client := &scriptedReranker{ranked: []uint{3, 2, 1}}
	svc := newRerankService(t, client, true, time.Second, 3)
	items := rerankVideos(1, 2, 3)

	svc.applyRerank(context.Background(), 7, items)
	svc.applyRerank(context.Background(), 7, items)
	if client.calls != 1 {
		t.Fatalf("连续两次相同请求调用了模型 %d 次，want 1（刷新会把成本乘以刷新次数）", client.calls)
	}
	// 不同用户不该复用同一份顺序。
	svc.applyRerank(context.Background(), 8, items)
	if client.calls != 2 {
		t.Fatalf("不同用户复用了缓存：调用次数 = %d, want 2", client.calls)
	}
}

func TestValidateRerankResult(t *testing.T) {
	candidates := []RerankCandidate{{ID: 1}, {ID: 2}}
	if err := validateRerankResult([]uint{2, 1}, candidates); err != nil {
		t.Fatalf("合法结果被判为非法: %v", err)
	}
	for _, bad := range [][]uint{{}, {1}, {1, 1}, {1, 2, 3}, {1, 9}} {
		if err := validateRerankResult(bad, candidates); err == nil {
			t.Errorf("非法结果 %v 没有被拒绝", bad)
		}
	}
}

func TestApplyRerankOrderKeepsEveryItem(t *testing.T) {
	items := rerankVideos(1, 2, 3, 4)
	// 模型给出一个"不完整"的顺序（正常路径下校验会先拒绝它）：
	// 兜底逻辑必须仍然保留所有条目，一条都不能丢。
	got := applyRerankOrder(items, []uint{4, 3})
	if len(got) != 4 {
		t.Fatalf("重排后条数 = %d, want 4（任何情况下都不能丢视频）", len(got))
	}
	seen := map[uint]bool{}
	for _, v := range got {
		if seen[v.ID] {
			t.Fatalf("重排后出现重复条目: %v", idsOfFeedVideos(got))
		}
		seen[v.ID] = true
	}
}

func TestRerankDefaultsMatchConfigValues(t *testing.T) {
	// 这两个默认值与 internal/config 的取值必须一致，否则
	// "yaml 里没写"与"直接构造 Options"会得到两种行为，
	// 而它们都会在同一个部署里生效（配置层兜底 + feed 层兜底）。
	if defaultRerankCandidates != 20 {
		t.Errorf("defaultRerankCandidates = %d, want 20（D4 的设计取值）", defaultRerankCandidates)
	}
	if defaultRerankTimeout != 120*time.Millisecond {
		t.Errorf("defaultRerankTimeout = %v, want 120ms（D1 的设计取值）", defaultRerankTimeout)
	}
	// 客户端为 nil 时即使 Enabled=true 也必须是关闭的：
	// 否则会走到一个 nil 接口的方法调用上。
	svc, _ := newSemanticService(t, &fakeVectorStore{}, 0)
	svc.WithRerank(RerankOptions{Enabled: true, Timeout: time.Second, TopN: 5})
	if svc.rerankEnabled {
		t.Fatal("没有客户端时精排必须是关闭的")
	}
}

func TestParseRerankResponse(t *testing.T) {
	candidates := []RerankCandidate{{ID: 1}, {ID: 2}, {ID: 3}}
	cases := []struct {
		name string
		text string
		want []uint
		bad  bool
	}{
		{name: "标准 JSON", text: `{"ids":[3,1,2]}`, want: []uint{3, 1, 2}},
		{name: "带围栏", text: "```json\n{\"ids\":[2,3,1]}\n```", want: []uint{2, 3, 1}},
		{name: "id 是字符串", text: `{"ids":["2","3","1"]}`, want: []uint{2, 3, 1}},
		{name: "前后有解释", text: "好的，这是排序结果：{\"ids\":[1,3,2]} 以上。", want: []uint{1, 3, 2}},
		{name: "没有 JSON", text: "我觉得 3 最好", bad: true},
		{name: "ids 非整数", text: `{"ids":["id3",1,2]}`, bad: true},
		{name: "缺 id", text: `{"ids":[1,2]}`, bad: true},
		{name: "空 ids", text: `{"ids":[]}`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRerankResponse(tc.text, candidates)
			if tc.bad {
				if err == nil {
					t.Fatalf("应当报错，实际得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalIDs(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRerankPromptIncludesAllCandidates(t *testing.T) {
	candidates := []RerankCandidate{
		{ID: 1, Title: "Go 并发", Description: "goroutine 与 channel", LikesCount: 3, CreateTime: time.Unix(0, 0)},
		{ID: 2, Title: "猫", Description: "橘猫的一天"},
	}
	prompt := buildRerankPrompt(42, candidates)
	for _, want := range []string{"42", "Go 并发", "goroutine", "橘猫"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt 里缺少 %q:\n%s", want, prompt)
		}
	}
	// 太长会撑大 prompt（D4 的成本估算基于 500~700 token）。
	long := RerankCandidate{ID: 3, Title: string(make([]rune, 500))}
	_ = buildRerankPrompt(42, []RerankCandidate{long})
	if got := truncateRunes("一二三四五", 3); got != "一二三…" {
		t.Errorf("truncateRunes = %q, want 一二三…", got)
	}
}

func TestDegradeReasonClassifiesErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ai.ErrTimeout, "timeout"},
		{context.DeadlineExceeded, "timeout"},
		{ai.ErrDisabled, "ai_disabled"},
		{ai.ErrGatewayNotConfigured, "ai_disabled"},
		{errors.New("boom"), "call_failed"},
		{ai.ErrUnavailable, "call_failed"},
	}
	for _, tc := range cases {
		if got := degradeReason(tc.err); got != tc.want {
			t.Errorf("degradeReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
