package video_test

import (
	"context"
	"errors"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/middleware/rabbitmq"
	"github.com/handlerzzy/feedsystem/internal/video"
	"github.com/handlerzzy/feedsystem/internal/video/mock"

	"go.uber.org/mock/gomock"
)

// 本文件守 P1 里最不能出错的一条：**AI 投递失败绝不影响视频发布**。
//
// 发布是承重墙，打标是锦上添花（总览第 1 节）。这条性质如果坏了，
// 表现是"MQ 抖动时用户发不出视频"——而那是不可接受的故障放大。

// TestPublishSucceedsWhenAnalysisPublishFails 是 4.2 验收项
// "MQ 关闭时，发布接口仍返回成功，只有一条 Warn 日志"的实现。
func TestPublishSucceedsWhenAnalysisPublishFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockVideoStore(ctrl)
	// 真实 repo 会在事务里回填自增主键，假实现必须照做：
	// 否则视频 ID 为 0，投递会被"ID 异常"的判断直接跳过，测不到想测的东西。
	store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, v *video.Video) error {
			v.ID = 7
			return nil
		})

	publisher := mock.NewMockContentAnalysisPublisher(ctrl)
	publisher.EXPECT().Request(gomock.Any(), gomock.Any()).
		Return(errors.New("amqp: channel closed"))

	svc := video.NewVideoService(store, nil, nil).WithContentAnalysis(publisher)

	v := &video.Video{AuthorID: 1, Title: "标题", PlayURL: "http://x/p.mp4", CoverURL: "http://x/c.jpg"}
	if err := svc.Publish(context.Background(), v); err != nil {
		t.Fatalf("投递失败不该让发布失败，实际: %v", err)
	}
}

// TestPublishDoesNotCallAnalysisWhenNotWired 覆盖 AI 关闭的默认状态。
//
// 未注入投递器时不能有任何调用（包括不能因为"接口非 nil 但实现是空壳"
// 而走进发布路径）。
func TestPublishDoesNotCallAnalysisWhenNotWired(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockVideoStore(ctrl)
	store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, v *video.Video) error {
			v.ID = 7
			return nil
		})

	publisher := mock.NewMockContentAnalysisPublisher(ctrl)
	// 不设 EXPECT：任何调用都会让 gomock 报"unexpected call"。
	_ = publisher

	svc := video.NewVideoService(store, nil, nil) // 不调用 WithContentAnalysis
	v := &video.Video{AuthorID: 1, Title: "标题", PlayURL: "http://x/p.mp4", CoverURL: "http://x/c.jpg"}
	if err := svc.Publish(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
}

// TestPublishDoesNotCallAnalysisWhenDBWriteFails 守住投递时机：
// **DB 写入成功之后**才投递。
//
// 先投递再写库会让"分析任务进队列但视频没入库"，worker 回查时发现视频不存在，
// 于是白跑一次（P1 里它会因此 Ack 丢弃，日志上看起来像正常竞态，
// 真正的失败原因却永远不会出现）。
func TestPublishDoesNotCallAnalysisWhenDBWriteFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockVideoStore(ctrl)
	store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	publisher := mock.NewMockContentAnalysisPublisher(ctrl)
	// 不设 EXPECT：投递器一次都不该被调用。
	svc := video.NewVideoService(store, nil, nil).WithContentAnalysis(publisher)

	v := &video.Video{AuthorID: 1, Title: "标题", PlayURL: "http://x/p.mp4", CoverURL: "http://x/c.jpg"}
	if err := svc.Publish(context.Background(), v); err == nil {
		t.Fatal("写库失败必须报错")
	}
}

// TestPublishRequestsAnalysisWithVideoIDOnly 守住载荷契约：
// 只带 video_id 等标识，**不把长文本塞进 MQ**（4.2 的要求）。
//
// 长文本进队列有两个问题：消息体膨胀、以及内容一旦进了队列就无法随视频更新。
// worker 自己回查数据库，拿到的永远是最新内容。
func TestPublishRequestsAnalysisWithVideoIDOnly(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockVideoStore(ctrl)
	store.EXPECT().PublishWithOutbox(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, v *video.Video) error {
			v.ID = 42 // 模拟 GORM 回填主键
			return nil
		})

	var got rabbitmq.ContentAnalysisEvent
	publisher := mock.NewMockContentAnalysisPublisher(ctrl)
	publisher.EXPECT().Request(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, evt rabbitmq.ContentAnalysisEvent) error {
			got = evt
			return nil
		})

	svc := video.NewVideoService(store, nil, nil).WithContentAnalysis(publisher)
	v := &video.Video{
		AuthorID: 9, Title: "不该进队列的标题", Description: "不该进队列的简介",
		PlayURL: "http://x/p.mp4", CoverURL: "http://x/c.jpg",
	}
	if err := svc.Publish(context.Background(), v); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	if got.VideoID != 42 {
		t.Errorf("video_id = %d, want 42（必须是回填后的真实主键）", got.VideoID)
	}
	if got.AnalysisType != rabbitmq.AnalysisTypeText {
		t.Errorf("analysis_type = %q, want %q", got.AnalysisType, rabbitmq.AnalysisTypeText)
	}
	if got.Title != "" || got.Description != "" {
		t.Errorf("长文本不该进 MQ（worker 自己回查），实际 title=%q description=%q",
			got.Title, got.Description)
	}
}
