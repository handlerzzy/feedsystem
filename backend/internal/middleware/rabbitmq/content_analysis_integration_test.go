package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件用**真实 RabbitMQ** 验证 P0 新增的 AI 分析拓扑能端到端跑通：
// 发布一条消息 -> worker 侧（这里用同一份消费逻辑）能收到。
//
// 为什么必须用真实 broker 而不能只靠单测：
// 拓扑声明（exchange/queue/binding key 三者是否真的对得上）是 broker 侧的
// 行为，单测里 mock 不掉。binding key 写成 "ai.content.analysis.*" 而
// routing key 发成 "ai.content.analysis.request" 只差一个字符，
// 在 mock 里永远测不出来，在真实环境里表现是"消息发出去了但没人收到"。
//
// 默认跳过；设置 TEST_RABBITMQ_URL 后运行：
//
//	TEST_RABBITMQ_URL='amqp://admin:password123@127.0.0.1:5672/' \
//	  go test -count=1 -v -run TestContentAnalysis ./internal/middleware/rabbitmq/

// TestContentAnalysisTopologyRoutesMessage 验证"发布的消息真的会进队列"。
//
// 做法：先声明拓扑并清空队列，再发布一条，然后从队列里同步取一条。
// 直接消费队列（而不是依赖 worker）让用例与 worker 实现解耦——
// 这里要证明的是拓扑正确，不是 worker 正确。
func TestContentAnalysisTopologyRoutesMessage(t *testing.T) {
	rmq := dialTestRabbitMQ(t)

	ch, err := rmq.NewChannel()
	if err != nil {
		t.Fatalf("建 Channel 失败: %v", err)
	}
	defer ch.Close()

	topo := ContentAnalysisTopology()
	if err := topo.Declare(ch); err != nil {
		t.Fatalf("声明拓扑失败: %v", err)
	}

	// 清空队列：用一个独立 Channel 反复 Get，把上一次跑剩下的消息排干净，
	// 否则"取出第一条"可能是历史消息，测试会变得不可重复。
	drainCh, err := rmq.NewChannel()
	if err != nil {
		t.Fatalf("建 drain Channel 失败: %v", err)
	}
	defer drainCh.Close()
	for i := 0; i < 100; i++ {
		d, ok, err := drainCh.Get(topo.Queue, false)
		if err != nil {
			t.Fatalf("清空队列失败: %v", err)
		}
		if !ok {
			break
		}
		_ = d.Ack(false)
	}

	// 直接往交换机发：这条路径与 ContentAnalysisMQ.Request 内部完全一致
	// （同一个 exchange、同一个 routing key），因此能真实反映线上行为。
	evt := ContentAnalysisEvent{
		EventID:      "itest-evt-1",
		VideoID:      4242,
		AuthorID:     7,
		Title:        "集成测试标题",
		Description:  "集成测试简介",
		AnalysisType: AnalysisTypeText,
		OccurredAt:   time.Now().UTC(),
	}
	if err := PublishJSON(context.Background(), ch, contentAnalysisExchange, contentAnalysisRequestRK, evt); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// 投递是异步的，给 broker 一点时间落队；Get 本身也会等（非阻塞返回 not ok）。
	var got ContentAnalysisEvent
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, ok, err := ch.Get(topo.Queue, false)
		if err != nil {
			t.Fatalf("从队列取消息失败: %v", err)
		}
		if ok {
			if err := unmarshalEvent(t, d.Body, &got); err != nil {
				_ = d.Nack(false, false)
				t.Fatalf("载荷解析失败: %v", err)
			}
			_ = d.Ack(false)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("5 秒内没有从 ai.content.analysis.events 取到消息：" +
				"binding key 与 routing key 可能对不上")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got.EventID != evt.EventID || got.VideoID != evt.VideoID {
		t.Errorf("消息内容不符: got=%+v want=%+v", got, evt)
	}
	if got.AnalysisType != AnalysisTypeText {
		t.Errorf("analysis_type 丢失: %q", got.AnalysisType)
	}
}

// TestContentAnalysisMQRequestRoundTrip 覆盖发布器的公开入口。
//
// 与上一个用例的区别：那个直接调 PublishJSON，这个走 ContentAnalysisMQ.Request，
// 因此它还覆盖了 "event_id 自动补全""视频 ID 校验""Publisher 复用"这几段。
func TestContentAnalysisMQRequestRoundTrip(t *testing.T) {
	rmq := dialTestRabbitMQ(t)

	mq, err := NewContentAnalysisMQ(rmq)
	if err != nil {
		t.Fatalf("构造发布器失败: %v", err)
	}

	// 校验：没有 video_id 必须被拒绝（否则 worker 侧只能丢弃，白跑一趟队列）。
	if err := mq.Request(context.Background(), ContentAnalysisEvent{AnalysisType: AnalysisTypeText}); err == nil {
		t.Error("缺少 video_id 必须被拒绝")
	}
	// 校验：没有 analysis_type 必须被拒绝（worker 无法判断该做什么）。
	if err := mq.Request(context.Background(), ContentAnalysisEvent{VideoID: 1}); err == nil {
		t.Error("缺少 analysis_type 必须被拒绝")
	}

	// 正常投递：event_id 留空，应由发布器补上。
	const videoID = 4243
	if err := mq.Request(context.Background(), ContentAnalysisEvent{
		VideoID:      videoID,
		Title:        "标题",
		AnalysisType: AnalysisTypeText,
	}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	ch, err := rmq.NewChannel()
	if err != nil {
		t.Fatalf("建 Channel 失败: %v", err)
	}
	defer ch.Close()
	topo := ContentAnalysisTopology()
	if err := topo.Declare(ch); err != nil {
		t.Fatalf("声明拓扑失败: %v", err)
	}

	var found *ContentAnalysisEvent
	deadline := time.Now().Add(5 * time.Second)
	for found == nil && time.Now().Before(deadline) {
		d, ok, err := ch.Get(topo.Queue, false)
		if err != nil {
			t.Fatalf("取消息失败: %v", err)
		}
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var evt ContentAnalysisEvent
		if err := unmarshalEvent(t, d.Body, &evt); err != nil {
			_ = d.Nack(false, false)
			t.Fatalf("解析失败: %v", err)
		}
		_ = d.Ack(false)
		if evt.VideoID == videoID {
			found = &evt
		}
	}
	if found == nil {
		t.Fatal("没收到自己发出的那条消息")
	}
	if found.EventID == "" {
		t.Error("发布器必须补全 event_id（worker 靠它做幂等与日志串联）")
	}
	if found.OccurredAt.IsZero() {
		t.Error("发布器必须补全 occurred_at")
	}
}

// TestContentAnalysisTopologyIsStable 是一个"契约快照"测试。
//
// 名字、exchange、queue、routing key 一旦被改动，已经发布出去的消息就会
// 进不了新队列（旧 worker 仍在消费旧队列）——这在滚动升级期间是静默的数据丢失。
// 这里把四个字符串钉死，改动必须是有意识的（并且同步改常量、刻意更新本用例）。
func TestContentAnalysisTopologyIsStable(t *testing.T) {
	topo := ContentAnalysisTopology()
	if topo.Exchange != "ai.content.analysis.events" {
		t.Errorf("exchange = %q，拓扑常量属于长期契约，改名会让在途消息失配", topo.Exchange)
	}
	if topo.Queue != "ai.content.analysis.events" {
		t.Errorf("queue = %q", topo.Queue)
	}
	if topo.BindingKey != "ai.content.analysis.*" {
		t.Errorf("binding key = %q", topo.BindingKey)
	}
	if contentAnalysisRequestRK != "ai.content.analysis.request" {
		t.Errorf("routing key = %q", contentAnalysisRequestRK)
	}
	// binding key 必须能匹配上 routing key，否则消息发出去没人收。
	//
	// 这里用真正的 topic 匹配规则（逐段比较，* 匹配恰好一段），
	// 而不是前缀包含：前缀检查会把 "ai.content.*" 这种**不匹配**的
	// binding key 判成通过（review 指出的不健全断言）。
	if !topicMatches(topo.BindingKey, contentAnalysisRequestRK) {
		t.Errorf("binding key %q 匹配不上 routing key %q：消息会发出去但没人收",
			topo.BindingKey, contentAnalysisRequestRK)
	}
	// 反向：确认这个匹配器本身是有效的（否则上面的断言永远为真）。
	if topicMatches("ai.content.*", contentAnalysisRequestRK) {
		t.Error("匹配器有误：ai.content.* 不该匹配四段的 routing key")
	}
}

// TestContentAnalysisTopologyDoesNotTouchExistingOnes 守住总览第 2 节红线 2：
// 只能新增拓扑，不得改动已有的 5 个。
//
// 这条断言很笨，但正是它要的效果：有人"顺手统一一下命名"时，这里会红。
func TestContentAnalysisTopologyDoesNotTouchExistingOnes(t *testing.T) {
	existing := map[string]Topology{
		"like":       LikeTopology(),
		"comment":    CommentTopology(),
		"social":     SocialTopology(),
		"popularity": PopularityTopology(),
		"timeline":   TimelineTopology(),
	}
	// 期望值刻意写成字面量（而不是引用常量）：这样"改常量"这个动作本身
	// 会让用例变红，逼着改动者确认自己是在新增还是真的在动红线。
	want := map[string][3]string{
		"like":       {"like.events", "like.events", "like.*"},
		"comment":    {"comment.events", "comment.events", "comment.*"},
		"social":     {"social.events", "social.events", "social.*"},
		"popularity": {"video.popularity.events", "video.popularity.events", "video.popularity.*"},
		// timeline 的 queue 名与 exchange 名不同，这是历史遗留，同样不能动：
		// 改名意味着在途消息全部进不了新队列。
		"timeline": {"video.timeline.events", "video.timeline.update.queue", "video.timeline.*"},
	}
	for name, topo := range existing {
		w, ok := want[name]
		if !ok {
			t.Fatalf("用例缺少 %s 的期望值", name)
		}
		if topo.Exchange != w[0] || topo.Queue != w[1] || topo.BindingKey != w[2] {
			t.Errorf("%s 拓扑被改动了: got=%+v want=%v\n"+
				"已有的 5 个拓扑常量是硬性红线，只能新增不能修改", name, topo, w)
		}
	}

	// 新拓扑必须与这 5 个都不同名，否则会互相抢消息。
	newTopo := ContentAnalysisTopology()
	for name, topo := range existing {
		if topo.Exchange == newTopo.Exchange || topo.Queue == newTopo.Queue {
			t.Errorf("新拓扑与 %s 拓扑重名: %+v vs %+v", name, topo, newTopo)
		}
	}
}

// unmarshalEvent 解析一条 delivery 的载荷。
//
// 单独抽出来是为了在解析失败时给出"原始字节"这一关键线索：
// 载荷格式写错（比如字段名大小写）时，只说"json 解析失败"是查不到原因的。
func unmarshalEvent(t *testing.T, body []byte, dst *ContentAnalysisEvent) error {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", string(body), err)
	}
	return nil
}

// topicMatches 实现 AMQP topic 交换机的匹配规则：用 "." 分段，
// "*" 匹配恰好一段，"#" 匹配零段或多段。
//
// 为什么自己实现而不是用字符串前缀比较：前缀比较会把 "ai.content.*"
// 这种**不匹配**的 binding key 判成通过，于是"路由断言"变成一句永远为真的
// 废话（review 指出此处断言不健全）。两者差别正是"消息能不能被投递"。
func topicMatches(pattern, routingKey string) bool {
	return matchSegments(strings.Split(pattern, "."), strings.Split(routingKey, "."))
}

func matchSegments(pattern, key []string) bool {
	if len(pattern) == 0 {
		return len(key) == 0
	}
	switch pattern[0] {
	case "#":
		// '#' 可以吃掉任意多段（含 0 段）。
		for i := 0; i <= len(key); i++ {
			if matchSegments(pattern[1:], key[i:]) {
				return true
			}
		}
		return false
	case "*":
		if len(key) == 0 {
			return false
		}
		return matchSegments(pattern[1:], key[1:])
	default:
		if len(key) == 0 || pattern[0] != key[0] {
			return false
		}
		return matchSegments(pattern[1:], key[1:])
	}
}

// TestTopicMatchesIsSound 单独守一遍匹配器本身：断言依赖它，
// 它错了的话所有路由断言都会变成"永远通过"。
func TestTopicMatchesIsSound(t *testing.T) {
	cases := []struct {
		pattern, key string
		want         bool
	}{
		{"ai.content.analysis.*", "ai.content.analysis.request", true},
		{"ai.content.analysis.*", "ai.content.analysis.a.b", false}, // * 只匹配一段
		{"ai.content.*", "ai.content.analysis.request", false},      // 段数不足
		{"ai.#", "ai.content.analysis.request", true},               // # 吃多段
		{"#", "any.thing.at.all", true},
		{"ai.content.analysis.request", "ai.content.analysis.request", true},
		{"ai.content.analysis.request", "ai.content.analysis.other", false},
	}
	for _, tc := range cases {
		if got := topicMatches(tc.pattern, tc.key); got != tc.want {
			t.Errorf("topicMatches(%q, %q) = %v, want %v", tc.pattern, tc.key, got, tc.want)
		}
	}
}
