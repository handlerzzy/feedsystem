package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/video"
)

// 本文件覆盖 P2 向量回填的**后台循环**。
//
// 为什么它值得单独测：回填是一条没有外部触发者的链路（视频发布不直接
// 产生向量），它的正确性完全由这个循环保证。而循环里最容易错的两件事
// 都不会报错、只会静默失效：
//
//   - 出错就退出 -> 向量池停止更新，语义召回慢慢变空；
//   - "没有变化"时也疯狂打日志 -> 日志被刷满，真正的故障看不见。

// fakeBackfiller 记录调用次数并按脚本返回结果或错误。
type fakeBackfiller struct {
	calls  int
	stats  []video.BackfillStats
	errs   []error
	onCall func()
}

func (b *fakeBackfiller) BackfillOnce(context.Context) (video.BackfillStats, error) {
	b.calls++
	if b.onCall != nil {
		b.onCall()
	}
	i := b.calls - 1
	var stats video.BackfillStats
	if i < len(b.stats) {
		stats = b.stats[i]
	} else if len(b.stats) > 0 {
		stats = b.stats[len(b.stats)-1]
	}
	var err error
	if i < len(b.errs) {
		err = b.errs[i]
	} else if len(b.errs) > 0 {
		err = b.errs[len(b.errs)-1]
	}
	return stats, err
}

func TestEmbeddingBackfillLoopRunsImmediatelyAndStopsOnCancel(t *testing.T) {
	fb := &fakeBackfiller{}
	once := make(chan struct{}, 8)
	fb.onCall = func() { once <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		NewEmbeddingBackfillLoop(fb, time.Hour).Run(ctx)
		close(done)
	}()

	select {
	case <-once:
	case <-time.After(2 * time.Second):
		t.Fatal("循环没有立即执行一次回填：新视频在第一个间隔内不会被向量化")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后循环没有退出：进程无法优雅停止")
	}
}

func TestEmbeddingBackfillLoopKeepsRunningAfterError(t *testing.T) {
	fb := &fakeBackfiller{
		errs:  []error{errors.New("db down"), errors.New("db down"), nil},
		stats: []video.BackfillStats{{}, {}, {Embedded: 1}},
	}
	var calls int
	got3 := make(chan struct{})
	fb.onCall = func() {
		calls++
		if calls == 3 {
			close(got3)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewEmbeddingBackfillLoop(fb, 5*time.Millisecond).Run(ctx)

	select {
	case <-got3:
	case <-time.After(5 * time.Second):
		t.Fatalf("循环在第 %d 次调用后停止了：一次数据库故障会让向量池永久停止更新", calls)
	}
}

func TestEmbeddingBackfillLoopRejectsMisconfiguration(t *testing.T) {
	// 间隔为 0 时用默认值而不是"忙等"：否则循环会变成一次 100% CPU 的死循环，
	// 而现象只是"CPU 莫名其妙满了"。
	loop := NewEmbeddingBackfillLoop(&fakeBackfiller{}, 0)
	if loop.interval <= 0 {
		t.Fatal("间隔为 0 时必须回退到默认值，否则会忙等")
	}
	if loop.interval < time.Second {
		t.Fatalf("默认间隔 %v 过短：回填会变成高频轮询", loop.interval)
	}
	// backfiller 为 nil 时 Run 必须安全返回（装配被跳过时的防御）。
	done := make(chan struct{})
	go func() {
		NewEmbeddingBackfillLoop(nil, time.Millisecond).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backfiller 为 nil 时 Run 没有立即返回")
	}
}
