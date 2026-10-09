package rabbitmq

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/logging"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// 本文件用**真实 RabbitMQ** 验证连接的运行期自愈。
//
// 为什么需要它：此前 RabbitMQ 对象只是一个裸的 *amqp.Connection，连上之后再没人
// 管它。broker 重启或网络抖动之后，这个对象永久处于死状态，而所有
// `conn.Channel()` 都会一直返回：
//
//	Exception (504) Reason: "channel/connection is not open"
//
// 后果不是崩溃，而是**静默降级**——进程活着、健康检查是绿的、接口照样 200：
// outbox 消息不再投递（视频永远进不了时间线），发布侧则一路走"MQ 失败就同步
// 写库"的兜底路径。实测把 RabbitMQ 容器停掉再启动、API 与 worker 都不重启时，
// 两个容器都报 healthy，而 /feed/global_timeline 再也不更新，直到手动重启进程。
//
// 这些用例专门覆盖"连接已经死掉之后还能不能自己爬起来"，而这正是 mock 无法
// 证明的部分——必须真的把一条已建立的 AMQP 连接弄死。
//
// 默认跳过；设置 TEST_RABBITMQ_URL 后运行：
//
//	TEST_RABBITMQ_URL='amqp://admin:password123@127.0.0.1:5672/' \
//	  go test -count=1 -v ./internal/middleware/rabbitmq/

func testRabbitMQConfig(t *testing.T) *config.RabbitMQConfig {
	t.Helper()
	raw := os.Getenv("TEST_RABBITMQ_URL")
	if raw == "" {
		t.Skip("TEST_RABBITMQ_URL 未设置，跳过依赖真实 RabbitMQ 的集成测试")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_RABBITMQ_URL 解析失败: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("TEST_RABBITMQ_URL 里没有合法端口: %v", err)
	}
	pass, _ := u.User.Password()
	return &config.RabbitMQConfig{
		Host:     u.Hostname(),
		Port:     port,
		Username: u.User.Username(),
		Password: pass,
	}
}

func dialTestRabbitMQ(t *testing.T) *RabbitMQ {
	t.Helper()
	rmq, err := NewRabbitMQ(testRabbitMQConfig(t))
	if err != nil {
		t.Fatalf("连接测试 RabbitMQ 失败（broker 起了吗？）: %v", err)
	}
	t.Cleanup(func() { _ = rmq.Close() })
	return rmq
}

// TestNewChannelReconnectsAfterConnectionDies 是 P2-a 的核心回归测试。
//
// 先把一条**已经建立**的连接弄死（等价于 broker 重启/网络断开之后库内部的
// 状态：IsClosed() 为真、任何 Channel() 都报 504），然后要求 NewChannel 仍然
// 能返回一条可用 Channel。
//
// 旧实现下这条必然失败：它只会对那条死连接调用 Channel()。
func TestNewChannelReconnectsAfterConnectionDies(t *testing.T) {
	rmq := dialTestRabbitMQ(t)

	ch, err := rmq.NewChannel()
	if err != nil {
		t.Fatalf("初始建 Channel 就失败了: %v", err)
	}
	_ = ch.Close()

	// 制造"运行中断连"：直接关掉底层连接。
	// 这里不能只关 Channel——关 Channel 不影响连接，测不出重连。
	if err := rmq.Connection().Close(); err != nil {
		t.Fatalf("关闭底层连接失败: %v", err)
	}
	if !rmq.IsClosed() {
		t.Fatal("连接刚被关掉，IsClosed() 应为真")
	}

	ch2, err := rmq.NewChannel()
	if err != nil {
		t.Fatalf("连接断开后 NewChannel 应当自动重连，实际报错: %v\n"+
			"这正是旧实现的表现：拿着死连接一直报 504，进程却始终 healthy", err)
	}
	defer ch2.Close()

	// 真正证明它是**可用**的 Channel，而不只是没报错。
	if err := ch2.ExchangeDeclare("itest.reconnect.probe", "topic", false, true, false, false, nil); err != nil {
		t.Fatalf("重连后拿到的 Channel 不可用: %v", err)
	}
	if rmq.IsClosed() {
		t.Error("重连之后 IsClosed() 应为假")
	}
}

// TestNewChannelReusesLiveConnection 反向守一遍：连接还活着时不该反复拨号。
// 每次 NewChannel 都重连会让消费者与发布者都变慢，也会给 broker 制造无谓压力。
func TestNewChannelReusesLiveConnection(t *testing.T) {
	rmq := dialTestRabbitMQ(t)
	first := rmq.Connection()

	for i := 0; i < 3; i++ {
		ch, err := rmq.NewChannel()
		if err != nil {
			t.Fatalf("第 %d 次建 Channel 失败: %v", i+1, err)
		}
		_ = ch.Close()
	}
	if rmq.Connection() != first {
		t.Error("连接还可用时不该重新拨号（Connection() 变了）")
	}
}

// TestNewChannelReportsErrorWhenBrokerUnreachable 覆盖反向：broker 真不可达时
// 必须干净地报错，而不是 panic、死等或返回一条不可用的 Channel。
func TestNewChannelReportsErrorWhenBrokerUnreachable(t *testing.T) {
	// 端口 1 上不会有人监听，TCP 会立刻拒绝。
	rmq := &RabbitMQ{url: "amqp://guest:guest@127.0.0.1:1/"}
	ch, err := rmq.NewChannel()
	if err == nil {
		_ = ch.Close()
		t.Fatal("broker 不可达时 NewChannel 必须报错")
	}
	if !strings.Contains(err.Error(), "重连失败") {
		t.Errorf("错误信息应指出是重连失败，实际: %v", err)
	}
}

// TestPublisherDoRetriesOnFreshChannel 覆盖发布侧的自愈。
//
// 发布失败绝大多数是 Channel 已死。Publisher.Do 应当丢弃旧 Channel、新建一条
// 再试一次——否则一次可自愈的抖动就会被上层当成"发布失败"，直接落到"同步写库"
// 的兜底路径上，而那条路径一旦走上去就不会自己回来。
func TestPublisherDoRetriesOnFreshChannel(t *testing.T) {
	rmq := dialTestRabbitMQ(t)
	topo := TimelineTopology()
	pub := NewPublisher(rmq, topo.Declare)
	if err := pub.Init(); err != nil {
		t.Fatalf("Publisher 初始化失败: %v", err)
	}

	var seen []*amqp.Channel
	sentinel := errors.New("第一次调用故意失败")
	err := pub.Do(func(ch *amqp.Channel) error {
		seen = append(seen, ch)
		if len(seen) == 1 {
			return sentinel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do 应当在重建 Channel 后重试成功，实际: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("fn 被调用 %d 次，want 2（失败一次 + 重建后成功一次）", len(seen))
	}
	if seen[0] == seen[1] {
		t.Error("重试用的还是同一条 Channel——那样重试毫无意义")
	}
}

// TestPublisherReusesChannelWhenNothingFails 反向：一切正常时不该反复重建。
// 若每次发布都新建 Channel，AMQP 会迅速成为瓶颈。
func TestPublisherReusesChannelWhenNothingFails(t *testing.T) {
	rmq := dialTestRabbitMQ(t)
	topo := TimelineTopology()
	pub := NewPublisher(rmq, topo.Declare)
	if err := pub.Init(); err != nil {
		t.Fatalf("Publisher 初始化失败: %v", err)
	}

	var seen []*amqp.Channel
	for i := 0; i < 3; i++ {
		if err := pub.Do(func(ch *amqp.Channel) error {
			seen = append(seen, ch)
			return nil
		}); err != nil {
			t.Fatalf("第 %d 次 Do 失败: %v", i+1, err)
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[0] {
			t.Errorf("第 %d 次发布换了 Channel；正常路径应复用同一条", i+1)
		}
	}
}

// TestPublisherRecoversAfterChannelDies 端到端形态：Channel 被外部弄死之后，
// 发布仍应成功（真实发一条消息）。
func TestPublisherRecoversAfterChannelDies(t *testing.T) {
	rmq := dialTestRabbitMQ(t)
	topo := TimelineTopology()
	pub := NewPublisher(rmq, topo.Declare)
	if err := pub.Init(); err != nil {
		t.Fatalf("Publisher 初始化失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publish := func() error {
		return pub.Do(func(ch *amqp.Channel) error {
			return PublishJSON(ctx, ch, topo.Exchange, timelinePublishRK, map[string]any{
				"event_id": "itest", "video_id": 1,
			})
		})
	}

	if err := publish(); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}

	// 把 Publisher 内部那条 Channel 弄死，模拟连接抖动后的状态。
	pub.mu.Lock()
	dead := pub.ch
	pub.mu.Unlock()
	if dead == nil {
		t.Fatal("Publisher 内部没有 Channel，测试前提不成立")
	}
	_ = dead.Close()

	if err := publish(); err != nil {
		t.Fatalf("Channel 失效后发布应自动重建并成功，实际: %v", err)
	}
}

// TestLogDisconnectOnlyForAbnormalClose 覆盖"断连必须可见"的判断逻辑。
//
// 这里不连 broker，而是直接测那个决策函数：真实的异常断开要等库内部的读取
// goroutine 发现 EOF 才产生，测试里没法可靠制造（amqp 的 Close() 是优雅关闭，
// NotifyClose 只会把通道正常关掉、不带错误）。而"优雅关闭不该报故障"这条恰恰
// 最容易写错——写错就会在每次正常停机时刷一条假的断连告警。
//
// 异常断开的真实日志在端到端场景里验证过：把 RabbitMQ 容器停掉再启动，
// API 与 worker 都不重启，日志里会出现这条 Warn（见报告）。
func TestLogDisconnectOnlyForAbnormalClose(t *testing.T) {
	t.Run("异常断开：记一条 Warn 且带上原因", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		prev := logging.SetLogger(zap.New(core))
		t.Cleanup(func() { logging.SetLogger(prev) })

		logDisconnect(&amqp.Error{Code: 320, Reason: "CONNECTION_FORCED - broker forced connection closure"})

		entries := logs.FilterMessageSnippet("连接已断开").All()
		if len(entries) != 1 {
			t.Fatalf("异常断开应恰好记一条日志，实际 %d 条: %v", len(entries), logs.All())
		}
		if got := entries[0].Level; got != zap.WarnLevel {
			t.Errorf("级别 = %v, want Warn（可自愈的降级，不是 Error）", got)
		}
		// 必须带上 broker 给的原因，否则运维无从判断是网络还是 broker 主动踢的。
		if got := entries[0].ContextMap()["error"]; got == nil || !strings.Contains(got.(string), "CONNECTION_FORCED") {
			t.Errorf("日志应带上断开原因，实际字段 error=%v", got)
		}
	})

	t.Run("主动 Close：不记日志", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		prev := logging.SetLogger(zap.New(core))
		t.Cleanup(func() { logging.SetLogger(prev) })

		logDisconnect(nil) // NotifyClose 通道被正常关闭 => 我们自己调的 Close

		if n := logs.Len(); n != 0 {
			t.Errorf("优雅关闭不该产生告警，实际 %d 条: %v", n, logs.All())
		}
	})
}
