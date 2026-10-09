package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/middleware/redis"
	"github.com/handlerzzy/feedsystem/internal/worker"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// 本文件覆盖 SSE 推送的跨副本扇出。
//
// 要解决的问题：notification 的三个消费者队列（notification.like /
// notification.comment / notification.social）在所有 API 副本之间是**竞争消费**，
// 同一条 MQ 消息只会落到其中一个副本上；而 SSE 连接是**每个副本各自持有**的。
// 于是用户连在哪个副本上，决定了他能不能收到这次推送——连在另一个副本上就什么
// 都收不到，只能等客户端轮询 /notification/list。
//
// 修法是经 Redis Pub/Sub 广播，每个副本各自投给自己持有的连接。
//
// 这里用两个 SSEHub 实例模拟两个副本，共用同一个 miniredis，因此是真正的
// "A 副本推送、B 副本投递"的端到端验证，而不是只测一个函数有没有被调用。

func newTestCache(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	client := redis.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), "test:")
	t.Cleanup(func() {
		_ = client.Close()
		mr.Close()
	})
	return client, mr
}

func recvWithin(t *testing.T, ch chan *worker.Notification, d time.Duration) *worker.Notification {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(d):
		return nil
	}
}

// assertSilent 断言在 d 之内不再收到推送。
func assertSilent(t *testing.T, ch chan *worker.Notification, d time.Duration) {
	t.Helper()
	select {
	case n := <-ch:
		t.Fatalf("收到了多余的推送（同一条通知被投递了两次？）: %+v", n)
	case <-time.After(d):
	}
}

func newNotification(id, recipient uint) *worker.Notification {
	return &worker.Notification{ID: id, RecipientID: recipient, Type: "like", Content: "x"}
}

// TestPushDeliversAcrossReplicas 是本次改动的核心断言。
//
// 副本 A 调用 Push，连接在副本 B 上的用户必须也收到——这正是改造前做不到的。
func TestPushDeliversAcrossReplicas(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const userID uint = 7

	replicaA := worker.NewSSEHub(nil).WithFanout(cache)
	replicaB := worker.NewSSEHub(nil).WithFanout(cache)
	go replicaA.StartFanout(ctx)
	go replicaB.StartFanout(ctx)

	chA := replicaA.Subscribe(userID)
	chB := replicaB.Subscribe(userID)
	defer replicaA.Unsubscribe(userID, chA)
	defer replicaB.Unsubscribe(userID, chB)

	// 等订阅建立：反复发哨兵直到 B 收到一条。哨兵 id 用 999 以便区分。
	ready := false
	for i := 0; i < 100 && !ready; i++ {
		payload := []byte(`{"origin":"probe","user_id":7,"notification":{"id":999,"recipient_id":7}}`)
		pubCtx, pubCancel := context.WithTimeout(context.Background(), time.Second)
		_ = cache.Publish(pubCtx, cache.Key("sse:notify"), payload)
		pubCancel()
		if n := recvWithin(t, chB, 100*time.Millisecond); n != nil {
			ready = true
		}
		// A 也会收到哨兵，清掉以免干扰后续断言。
		select {
		case <-chA:
		default:
		}
	}
	if !ready {
		t.Fatal("副本 B 的订阅始终没有建立，测试前提不成立")
	}

	notif := newNotification(42, userID)
	replicaA.Push(userID, notif)

	// 本副本：同步投递。
	if got := recvWithin(t, chA, 2*time.Second); got == nil || got.ID != 42 {
		t.Fatalf("副本 A 没收到自己推送的通知: %+v", got)
	}
	// 另一个副本：这才是改造前丢失的那条。
	if got := recvWithin(t, chB, 2*time.Second); got == nil || got.ID != 42 {
		t.Fatalf("副本 B 没收到 A 推送的通知——跨副本扇出没生效（改造前的行为）: %+v", got)
	}
}

// TestPushDoesNotDoubleDeliverOnSameReplica 守住 origin 去重。
//
// Redis Pub/Sub 会把消息投给**包括发布者在内**的所有订阅者。如果没有 origin
// 判断，本副本会先同步投一次、再从订阅回调里投一次，用户看到两条。
func TestPushDoesNotDoubleDeliverOnSameReplica(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const userID uint = 8
	replica := worker.NewSSEHub(nil).WithFanout(cache)
	go replica.StartFanout(ctx)

	ch := replica.Subscribe(userID)
	defer replica.Unsubscribe(userID, ch)

	// 等订阅建立（同上：用哨兵探测）。
	ready := false
	for i := 0; i < 100 && !ready; i++ {
		payload := []byte(`{"origin":"probe","user_id":8,"notification":{"id":999,"recipient_id":8}}`)
		pubCtx, pubCancel := context.WithTimeout(context.Background(), time.Second)
		_ = cache.Publish(pubCtx, cache.Key("sse:notify"), payload)
		pubCancel()
		if n := recvWithin(t, ch, 100*time.Millisecond); n != nil {
			ready = true
		}
	}
	if !ready {
		t.Fatal("订阅始终没有建立，测试前提不成立")
	}

	replica.Push(userID, newNotification(43, userID))

	if got := recvWithin(t, ch, 2*time.Second); got == nil || got.ID != 43 {
		t.Fatalf("应收到恰好一条推送: %+v", got)
	}
	// 给广播回来的那条留足时间窗口：它必须被 origin 判断挡掉。
	assertSilent(t, ch, 700*time.Millisecond)
}

// TestPushWithoutFanoutStillDeliversLocally 覆盖单副本 / Redis 不可用时的行为。
//
// 推送不能依赖 Redis：通知已经落库，但"实时推一下"是纯加速手段，Redis 挂了不该
// 连本进程内的客户端也收不到。
func TestPushWithoutFanoutStillDeliversLocally(t *testing.T) {
	hub := worker.NewSSEHub(nil) // 不调用 WithFanout
	const userID uint = 9
	ch := hub.Subscribe(userID)
	defer hub.Unsubscribe(userID, ch)

	hub.Push(userID, newNotification(44, userID))
	if got := recvWithin(t, ch, time.Second); got == nil || got.ID != 44 {
		t.Fatalf("未启用扇出时本进程推送必须照常工作: %+v", got)
	}
}

// TestPushSurvivesRedisFailure 覆盖 Redis 报错时的降级。
//
// 广播失败只影响其它副本；本进程的客户端不该因此收不到推送，请求也不该被拖慢。
func TestPushSurvivesRedisFailure(t *testing.T) {
	cache, mr := newTestCache(t)
	const userID uint = 10
	hub := worker.NewSSEHub(nil).WithFanout(cache)
	ch := hub.Subscribe(userID)
	defer hub.Unsubscribe(userID, ch)

	mr.Close() // Redis 挂了

	hub.Push(userID, newNotification(45, userID))
	if got := recvWithin(t, ch, time.Second); got == nil || got.ID != 45 {
		t.Fatalf("Redis 故障时本进程推送仍应送达: %+v", got)
	}
}

// TestFanoutDropsMalformedMessageAndKeepsGoing 覆盖坏消息不影响后续。
//
// 频道是共享的：版本不一致或别的程序往同一个 key 写东西都可能产生无法解析的
// 负载。一条坏消息不该让整个副本从此收不到任何推送。
func TestFanoutDropsMalformedMessageAndKeepsGoing(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const userID uint = 11
	hub := worker.NewSSEHub(nil).WithFanout(cache)
	go hub.StartFanout(ctx)
	ch := hub.Subscribe(userID)
	defer hub.Unsubscribe(userID, ch)

	// 先等订阅建立。
	ready := false
	for i := 0; i < 100 && !ready; i++ {
		pubCtx, pubCancel := context.WithTimeout(context.Background(), time.Second)
		_ = cache.Publish(pubCtx, cache.Key("sse:notify"), []byte(`not json at all`))
		pubCancel()
		// 坏消息不会产生投递，所以再用一条好哨兵探测。
		pubCtx2, pubCancel2 := context.WithTimeout(context.Background(), time.Second)
		_ = cache.Publish(pubCtx2, cache.Key("sse:notify"),
			[]byte(`{"origin":"probe","user_id":11,"notification":{"id":999,"recipient_id":11}}`))
		pubCancel2()
		if n := recvWithin(t, ch, 100*time.Millisecond); n != nil {
			ready = true
		}
	}
	if !ready {
		t.Fatal("订阅始终没有建立，测试前提不成立")
	}

	// 坏消息之后再发一条正常广播，必须仍然送达。
	pubCtx, pubCancel := context.WithTimeout(context.Background(), time.Second)
	_ = cache.Publish(pubCtx, cache.Key("sse:notify"),
		[]byte(`{"origin":"other","user_id":11,"notification":{"id":46,"recipient_id":11}}`))
	pubCancel()

	if got := recvWithin(t, ch, 2*time.Second); got == nil || got.ID != 46 {
		t.Fatalf("坏消息之后的正常广播应照常送达: %+v", got)
	}
}

// TestStartFanoutWithoutCacheReturnsImmediately 覆盖未启用时的调用方便利性：
// 调用方（tasks.go）无需分支判断，直接 go StartFanout 即可。
func TestStartFanoutWithoutCacheReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		worker.NewSSEHub(nil).StartFanout(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未启用扇出时 StartFanout 应立即返回")
	}
}

// TestPushIsNilSafe 防御 nil 入参：Push 由 MQ 消费回调驱动，
// 一条构造异常的消息不该让消费循环 panic。
func TestPushIsNilSafe(t *testing.T) {
	hub := worker.NewSSEHub(nil)
	hub.Push(1, nil) // 不应 panic
	(*worker.SSEHub)(nil).Push(1, newNotification(1, 1))
}
