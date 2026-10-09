package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"fmt"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

// RabbitMQ 管理一条**可自愈**的 Connection；Channel 由各组件按需创建。
//
// 为什么强调"可自愈"：此前这里只是一个裸的 *amqp.Connection 字段，连上之后
// 再没人管它。RabbitMQ 重启、网络抖动、broker 主动断开之后，这个对象永久处于
// 死状态，而所有 `conn.Channel()` 都会一直返回：
//
//	Exception (504) Reason: "channel/connection is not open"
//
// 后果不是崩溃，而是**静默降级**——进程活着、健康检查是绿的、接口照样 200：
//
//   - 消费者侧：worker 与 API 的重试循环每 5 秒重试一次，但重试的是"在同一条
//     死连接上开 Channel"，于是永远失败；outbox 消息不再被投递，视频永远进不了
//     时间线。
//   - 发布侧：上层（LikeService / CommentService）看到发布失败会兜底成同步写库，
//     于是流量长期走兜底路径，DB 压力静默抬高。
//
// 实测（把 RabbitMQ 容器停掉再启动，API 与 worker 都不重启）：
// 两个容器都报 healthy，而 /feed/global_timeline 再也不更新，直到手动重启进程。
// 现在改为：每次 NewChannel 都先确认连接可用，不可用就重新拨号。
type RabbitMQ struct {
	mu   sync.Mutex
	conn *amqp.Connection
	// url 留作重连用。只存 URL 而不是 config：拨号只需要它，且避免配置被改。
	url string
}

func rabbitMQURL(cfg *config.RabbitMQConfig) string {
	return "amqp://" + cfg.Username + ":" + cfg.Password + "@" + cfg.Host + ":" + strconv.Itoa(cfg.Port) + "/"
}

func NewRabbitMQ(cfg *config.RabbitMQConfig) (*RabbitMQ, error) {
	if cfg == nil {
		return nil, errors.New("rabbitmq config is nil")
	}
	url := rabbitMQURL(cfg)
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, err
	}
	r := &RabbitMQ{conn: conn, url: url}
	r.watchClose(conn)
	return r, nil
}

// watchClose 在连接断开时留下一条日志。
//
// 这不是可选项：没有它的话，"连接断了"这件事在日志里完全不可见，运维只能看到
// 一串 504，还会误以为是 broker 的问题。这是"静默降级"里最难查的一环。
func (r *RabbitMQ) watchClose(conn *amqp.Connection) {
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	go func() {
		logDisconnect(<-closed)
	}()
}

// logDisconnect 决定要不要为一次连接结束记日志。
//
// 拆成独立函数是为了能被直接测试：真实的"异常断开"要等库内部的读取 goroutine
// 发现 EOF 才产生，测试里没法可靠制造；而"主动 Close 不该报故障"这个判断恰恰
// 最容易写错，也最容易变成日志噪音。
//
// err == nil 表示是我们自己调的 Close（NotifyClose 的通道被正常关闭），
// 属于预期内的停机，不是故障。
func logDisconnect(err *amqp.Error) {
	if err == nil {
		return
	}
	logging.L().Warn("RabbitMQ 连接已断开，下次 NewChannel 时会自动重连", zap.Error(err))
}

// Connection 返回当前连接（可能为 nil）。供健康检查等只读场景使用。
func (r *RabbitMQ) Connection() *amqp.Connection {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn
}

// IsClosed 报告当前是否没有可用连接。
func (r *RabbitMQ) IsClosed() bool {
	conn := r.Connection()
	return conn == nil || conn.IsClosed()
}

// ensureConn 返回一条可用连接，必要时重新拨号。
func (r *RabbitMQ) ensureConn() (*amqp.Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != nil && !r.conn.IsClosed() {
		return r.conn, nil
	}

	conn, err := amqp.Dial(r.url)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq 重连失败: %w", err)
	}
	r.conn = conn
	r.watchClose(conn)
	// 这是运维需要知道的状态转变（服务从降级中恢复），故为 Info。
	logging.L().Info("RabbitMQ 已重连")
	return conn, nil
}

// invalidate 在确认某条连接不可用时把它清掉，迫使下次 ensureConn 重新拨号。
// 传 bad 而不是无条件清空，是为了避免把别的 goroutine 刚建好的连接误删。
func (r *RabbitMQ) invalidate(bad *amqp.Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == bad {
		r.conn = nil
	}
}

func (r *RabbitMQ) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

// NewChannel 返回一条可用的 Channel；连接已断开时先重连。
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error) {
	if r == nil {
		return nil, errors.New("rabbitmq connection is not initialized")
	}

	conn, err := r.ensureConn()
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err == nil {
		return ch, nil
	}

	// 连接在"确认可用"与"开 Channel"之间也可能刚好断掉。这里强制重连再试一次，
	// 否则调用方的 5 秒重试循环会一直拿着同一条死连接打转。
	r.invalidate(conn)
	conn, redialErr := r.ensureConn()
	if redialErr != nil {
		return nil, fmt.Errorf("%w（首次失败: %v）", redialErr, err)
	}
	ch, redialErr = conn.Channel()
	if redialErr != nil {
		return nil, fmt.Errorf("重连后仍无法创建 Channel: %w（首次失败: %v）", redialErr, err)
	}
	return ch, nil
}

// Publisher 持有一个可自愈的发布用 Channel。
//
// 各 MQ 类型（LikeMQ / CommentMQ / ...）原本在启动时建一条 Channel 用到底，
// 连接一断它就永久失效。上层看到发布失败会兜底成同步写库，于是故障被"优雅
// 降级"掩盖成常态：进程健康、接口 200、日志只有 Warn，但走的其实一直是兜底
// 路径，直到有人手动重启。这里让 Channel 失效后能被重建。
type Publisher struct {
	mu      sync.Mutex
	base    *RabbitMQ
	declare func(*amqp.Channel) error
	ch      *amqp.Channel
}

// NewPublisher 创建发布器。declare 会在每次（重）建 Channel 后执行，
// 用于重新声明交换机/队列/绑定——broker 若整体重启过，这些也需要重建。
func NewPublisher(base *RabbitMQ, declare func(*amqp.Channel) error) *Publisher {
	return &Publisher{base: base, declare: declare}
}

// Init 在启动期建立第一条 Channel，失败即返回错误（调用方据此决定是否致命）。
func (p *Publisher) Init() error {
	if p == nil || p.base == nil {
		return errors.New("rabbitmq base is nil")
	}
	_, err := p.acquire()
	return err
}

// Do 在一条可用 Channel 上执行 fn；Channel 已死时重建并重试一次。
func (p *Publisher) Do(fn func(*amqp.Channel) error) error {
	ch, err := p.acquire()
	if err != nil {
		return err
	}
	if err := fn(ch); err != nil {
		// 发布失败绝大多数是 Channel 已死。重建后再给一次机会，免得把一次本可
		// 自愈的抖动直接推给上层的兜底路径（那正是"永久降级"的起点）。
		p.discard(ch)
		ch, acquireErr := p.acquire()
		if acquireErr != nil {
			return err
		}
		return fn(ch)
	}
	return nil
}

func (p *Publisher) acquire() (*amqp.Channel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	ch, err := p.base.NewChannel()
	if err != nil {
		return nil, err
	}
	if p.declare != nil {
		if err := p.declare(ch); err != nil {
			_ = ch.Close()
			return nil, err
		}
	}
	p.ch = ch
	return ch, nil
}

func (p *Publisher) discard(bad *amqp.Channel) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == bad {
		_ = p.ch.Close()
		p.ch = nil
	}
}

// NewRabbitMQWithRetry 启动期指数退避重试拨号。
//
// 它只负责"启动时连上"；"运行中掉了自动恢复"由 RabbitMQ.NewChannel 负责
// （见 RabbitMQ 的注释），两者是互补的，不是一个 TODO 的两半。
func NewRabbitMQWithRetry(ctx context.Context, cfg *config.RabbitMQConfig,
	maxRetries int, onRetry func(attempt int, wait time.Duration, err error)) (*RabbitMQ, error) {

	if maxRetries <= 0 {
		maxRetries = 1
	}
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		rmq, err := NewRabbitMQ(cfg)
		if err == nil {
			return rmq, nil
		}
		lastErr = err

		wait := time.Duration(1<<i) * time.Second // 与 worker 侧退避策略一致
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		if onRetry != nil {
			onRetry(i+1, wait, err)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("rabbitmq 重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

func DeclareTopic(ch *amqp.Channel, exchange string, queue string, bindingKey string) error {
	if ch == nil {
		return errors.New("channel is not initialized")
	}
	if exchange == "" || queue == "" || bindingKey == "" {
		return errors.New("exchange/queue/bindingKey is required")
	}

	if err := ch.ExchangeDeclare(
		exchange,
		"topic",
		true,
		false,
		false,
		false,
		nil,
	); err != nil {
		return err
	}

	q, err := ch.QueueDeclare(
		queue,
		true,
		false,
		false,
		false,
		amqp.Table{"x-dead-letter-exchange": DLXExchange},
	)
	if err != nil {
		return err
	}

	if err := ch.QueueBind(
		q.Name,
		bindingKey,
		exchange,
		false,
		nil,
	); err != nil {
		return err
	}
	if err := DeclareDLX(ch, queue); err != nil {
		// 死信拓扑没建起来：被拒绝/过期的消息会被 broker 直接丢弃（无 DLX 可投），
		// 属于静默的数据丢失，故为 Error；调用方仍按原逻辑返回 nil。
		logging.L().Error("DLX declare failed", zap.String("queue", queue), zap.Error(err))
	}
	return nil
}

func PublishJSON(ctx context.Context, ch *amqp.Channel, exchange string, routingKey string, payload any) error {
	if ch == nil {
		return errors.New("channel is not initialized")
	}
	if exchange == "" || routingKey == "" {
		return errors.New("exchange and routingKey are required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return ch.PublishWithContext(ctx, exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Body:         b,
	})
}

func newEventID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
