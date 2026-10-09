package feed

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/video"
)

// 本文件覆盖 P2 §4.6 推荐理由。
//
// 一条红线贯穿全部用例：**理由必须基于真实信号**。
// 因此这里既验证"有信号时有正确的话"，也验证"没有信号时返回空"——
// 后者比前者更重要：编造的理由比没有理由更糟，而它不会让任何测试失败。

type fakeReasonSignals struct {
	tags    map[uint][]string
	err     error
	queried [][]uint
}

func (f *fakeReasonSignals) VideoTagNames(_ context.Context, ids []uint) (map[uint][]string, error) {
	f.queried = append(f.queried, append([]uint(nil), ids...))
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uint][]string, len(ids))
	for _, id := range ids {
		if names, ok := f.tags[id]; ok {
			out[id] = names
		}
	}
	return out, nil
}

// reasonService 构造一个"召回配额为 0 但理由链路可用"的服务。
//
// 为什么配额是 0 却仍然注入 store：理由与召回**共用**同一份用户兴趣数据
// （最近点赞），但它们的开关是独立的——运维可以只关掉语义召回
// （recall_quota=0）而保留基于标签的理由。这个组合是真实存在的，
// 因此测试必须覆盖它。
func reasonService(t *testing.T, signals ReasonSignalLookup, store VectorRecallStore) *FeedService {
	t.Helper()
	svc, _ := newSemanticService(t, store, 0)
	if store != nil {
		svc.WithSemanticRecall(SemanticRecallOptions{
			Store: store,
			Model: "test-model",
			Dim:   2,
			Quota: 0, // 只保留理由所需的兴趣数据，不做语义召回
		})
	}
	return svc.WithReasonSignals(signals)
}

func TestLatestReasonsNilWhenNoRecallHappened(t *testing.T) {
	// 信号源是一个"一旦被查询就记录"的假实现：一条召回都没开时，
	// 理由链路必须完全不查库（这保证了三关全闭时响应体逐字节一致）。
	signals := &fakeReasonSignals{tags: map[uint][]string{1: {"猫"}}}
	svc := reasonService(t, signals, &fakeVectorStore{})

	reasons := svc.latestReasons(context.Background(), []*video.Video{semanticVideo(1)}, 7, nil, nil)
	if reasons != nil {
		t.Fatalf("没有任何召回时不该生成理由: %v", reasons)
	}
	if len(signals.queried) != 0 {
		t.Fatalf("没有任何召回时不该查标签: %v", signals.queried)
	}
}

func TestLatestReasonsTagBased(t *testing.T) {
	// 用户点赞过两条"猫"标签的视频 -> "猫"成为兴趣标签（阈值 2）。
	signals := &fakeReasonSignals{tags: map[uint][]string{
		100: {"猫"},
		101: {"猫", "橘猫"},
		// 候选 1 有两个标签，其中"猫"是兴趣标签（出现 2 次）、
		// "宠物"只出现 1 次（候选自己那次不算——兴趣只看**点赞**历史）。
		1: {"猫", "宠物"},
		2: {"Go"},
	}}
	store := &fakeVectorStore{liked: []uint{100, 101}}
	svc := reasonService(t, signals, store)

	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1), semanticVideo(2)},
		7,
		[]SemanticHit{{Video: semanticVideo(1), Score: 0.9}},
		nil,
	)
	if reasons[1] == "" {
		t.Fatalf("命中兴趣标签的视频没有理由: %v", reasons)
	}
	if want := "你常看 猫"; reasons[1] != want {
		t.Errorf("理由 = %q, want %q（只按真实命中的兴趣标签生成，不多说）", reasons[1], want)
	}
	// 2 号视频只有 "Go" 标签，而用户没有 Go 兴趣 -> 它虽然不是语义命中的
	// 那条（命中排名在第 1 位之后）也不该被编出一个理由。
	if reasons[2] != "" {
		t.Errorf("没有真实信号的视频被编了理由: %q", reasons[2])
	}
}

func TestLatestReasonsSimilarityFallback(t *testing.T) {
	// 没有标签信号（AI 关闭的部署）时，语义命中本身仍是一条真实信号。
	signals := &fakeReasonSignals{}
	store := &fakeVectorStore{liked: []uint{100}}
	svc := reasonService(t, signals, store)

	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1)},
		7,
		[]SemanticHit{{Video: semanticVideo(1), Score: 0.5}},
		nil,
	)
	if reasons[1] != "与你近期点赞的内容相似" {
		t.Fatalf("理由 = %q, want 与你近期点赞的内容相似", reasons[1])
	}
}

func TestLatestReasonsExploreSignal(t *testing.T) {
	signals := &fakeReasonSignals{}
	svc := reasonService(t, signals, &fakeVectorStore{})

	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1)},
		7,
		nil,
		[]*video.Video{semanticVideo(1)},
	)
	if reasons[1] != "新发布，等待你的第一条反馈" {
		t.Fatalf("探索位没有说明理由: %q", reasons[1])
	}
}

func TestLatestReasonsSemanticSignalOnlyForTopRanked(t *testing.T) {
	// 一页里每条都写"与你点赞过的内容相似"时，这句话就不再传递信息。
	signals := &fakeReasonSignals{}
	svc := reasonService(t, signals, &fakeVectorStore{})

	videos := make([]*video.Video, 0, 6)
	hits := make([]SemanticHit, 0, 6)
	for i := uint(1); i <= 6; i++ {
		videos = append(videos, semanticVideo(i))
		hits = append(hits, SemanticHit{Video: semanticVideo(i), Score: 1 - float64(i)/10})
	}
	reasons := svc.latestReasons(context.Background(), videos, 7, hits, nil)
	withReason := 0
	for _, r := range reasons {
		if r != "" {
			withReason++
		}
	}
	if withReason != reasonSimilarityTopN {
		t.Fatalf("有理由的条数 = %d, want %d（只有召回最前面的几条才值得说这句话）",
			withReason, reasonSimilarityTopN)
	}
}

func TestLatestReasonsIsDeterministic(t *testing.T) {
	signals := &fakeReasonSignals{tags: map[uint][]string{
		100: {"猫"}, 101: {"猫"}, 102: {"狗"}, 103: {"狗"},
		1: {"猫", "狗", "宠物"},
	}}
	svc := reasonService(t, signals, &fakeVectorStore{liked: []uint{100, 101, 102, 103}})

	first := ""
	for i := 0; i < 20; i++ {
		reasons := svc.latestReasons(context.Background(),
			[]*video.Video{semanticVideo(1)}, 7,
			[]SemanticHit{{Video: semanticVideo(1), Score: 1}}, nil)
		if i == 0 {
			first = reasons[1]
			continue
		}
		if reasons[1] != first {
			t.Fatalf("同一份数据两次生成的理由不同: %q vs %q（map 顺序泄漏进了用户可见文案）",
				first, reasons[1])
		}
	}
}

func TestLatestReasonsSurvivesSignalFailure(t *testing.T) {
	signals := &fakeReasonSignals{err: errors.New("tags table missing")}
	store := &fakeVectorStore{liked: []uint{100}}
	svc := reasonService(t, signals, store)

	// 标签查询失败：仍然要能用语义信号给出理由，且不能报错。
	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1)}, 7,
		[]SemanticHit{{Video: semanticVideo(1), Score: 1}}, nil)
	if reasons[1] == "" {
		t.Fatalf("标签查询失败后连语义信号都丢了: %v", reasons)
	}
}

func TestLatestReasonsWithoutSignalsOrVectorStore(t *testing.T) {
	// AI 完全关闭的部署：没有标签查询能力、没有向量能力。
	// 语义召回（如果配额被人手工开过）仍然是一条真实信号。
	svc, _ := newSemanticService(t, &fakeVectorStore{}, 0)
	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1)}, 7,
		[]SemanticHit{{Video: semanticVideo(1), Score: 1}}, nil)
	if reasons[1] != "与你近期点赞的内容相似" {
		t.Fatalf("理由 = %q，want 与你近期点赞的内容相似", reasons[1])
	}
}

func TestLatestReasonsSingleLikeIsNotAnInterest(t *testing.T) {
	// 只点赞过一次的标签不能写"你常看 X"——那是夸大事实，
	// 而夸大是幻觉的温和版本，同样会侵蚀信任。
	signals := &fakeReasonSignals{tags: map[uint][]string{
		100: {"猫"},
		1:   {"猫"},
	}}
	svc := reasonService(t, signals, &fakeVectorStore{liked: []uint{100}})

	reasons := svc.latestReasons(context.Background(),
		[]*video.Video{semanticVideo(1)}, 7,
		[]SemanticHit{{Video: semanticVideo(1), Score: 0.1}}, nil)
	if reasons[1] == "你常看 猫" {
		t.Fatalf("只点赞过一次的标签被当成了兴趣: %q", reasons[1])
	}
}

func TestFeedVideoItemReasonIsOmitempty(t *testing.T) {
	// 理由字段缺失时前端不报错、不显示空行 —— 这条性质由 omitempty 保证。
	// 这里直接断言 JSON 序列化结果里不出现空 reason。
	item := FeedVideoItem{ID: 1}
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"id":1,"author":{"id":0,"username":""},"title":"","play_url":"","cover_url":"","create_time":0,"likes_count":0,"is_liked":false}` {
		t.Fatalf("空 reason 出现在响应体里: %s", b)
	}
	item.Reason = "你常看 猫"
	b, err = json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"reason":"你常看 猫"`) {
		t.Fatalf("reason 没有被序列化: %s", b)
	}
}
