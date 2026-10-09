package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ContentAnalysisMQ 发布"待分析的视频内容"事件，供 contentanalysisworker 消费。
//
// 为什么不走 outbox（本文件最重要的一段话，改动前务必读完）：
// outbox 的 claim() 只按 status = pending 过滤，**没有 event_type 过滤**，
// deliver() 随后无条件把消息当 timeline 事件投进 video.timeline.events 并
// 删除该行。所以往 outbox_msgs 里写一条"AI 分析任务"，它会被 timeline poller
// 抢走、错误投递、然后删掉——AI 任务永远不会被消费，且全程没有任何报错日志。
//
// 代价是这些 AI 事件在 MQ 不可用时会丢失。这个取舍是正确的：
// AI 打标丢了可以事后批量补跑，视频本身完全正常；而 timeline 事件丢了，
// 视频就永远不出现在任何人的时间线里。**不要为了给锦上添花的功能买保险，
// 去动承重墙的代码。**（详见 docs/AI-00-总览与全局约束.md 第 6.1 节）
type ContentAnalysisMQ struct {
	// pub 而不是裸的 *amqp.Channel：连接断开后 Channel 会永久失效，
	// 由 Publisher 负责重建（同 PopularityMQ 的理由）。
	pub *Publisher
}

const (
	// 与已有 5 个拓扑平行的新增拓扑，常量名与队列名都带 content.analysis 前缀，
	// 与 like./comment./social./video.popularity./video.timeline. 不冲突。
	//
	// 交换机的类型与声明参数完全照抄 DeclareTopic 的行为（topic + 持久化 +
	// x-dead-letter-exchange），因此 worker 先于 API 启动时两者声明的是
	// 同一个拓扑，不会出现参数不一致导致的 PRECONDITION_FAILED。
	contentAnalysisExchange   = "ai.content.analysis.events"
	contentAnalysisQueue      = "ai.content.analysis.events"
	contentAnalysisBindingKey = "ai.content.analysis.*"

	// routing key 与 binding key 的匹配关系：ai.content.analysis.request → 上面的 *。
	contentAnalysisRequestRK = "ai.content.analysis.request"
)

// ContentAnalysisEvent 是分析任务的载荷。
//
// 关于"事件类型"的取舍（值得写下来，因为这是长期契约里最容易做错的一步）：
// 用**一个字段 + 取值区分**（AnalysisType），而不是每种分析一个 event_type
// 常量再各配一个队列。理由是 P1 打标、P3 审核需要的是同一份输入
// （视频的文本内容），差别只在提示词与输出结构；一个队列 + 一个判别字段
// 可以共用同一套重试、幂等与死信处理，而 N 个队列会把这套逻辑复制 N 份。
//
// 新增分析类型时**只新增 AnalysisType 的取值**，不改结构体、不改队列名——
// 这样已部署的旧 worker 收到不认识的取值时只需记一条日志后 Ack（见 worker 侧），
// 不会把消息卡在队列里反复重投。
type ContentAnalysisEvent struct {
	EventID  string `json:"event_id"`
	VideoID  uint   `json:"video_id"`
	AuthorID uint   `json:"author_id"`
	// Title / Description 是当前可用的文本内容。留空是合法的：
	// 视频可能既没标题也没简介，此时 worker 侧应当直接跳过，而不是报错重试。
	Title       string `json:"title"`
	Description string `json:"description"`
	// AnalysisType 见上：取值为 "text"（P1 起会有 "summary"、"moderation" 等）。
	// 这里不定义成 Go 枚举类型，是为了让"旧版本收到新取值"退化成一条日志，
	// 而不是 JSON 反序列化失败。worker 侧已按这个约定处理未知取值
	// （见 internal/worker/contentanalysisworker.go 的 process）。
	AnalysisType string    `json:"analysis_type"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// AnalysisTypeText 是 P0 定义、P1 使用的第一个分析类型。
const AnalysisTypeText = "text"

func NewContentAnalysisMQ(base *RabbitMQ) (*ContentAnalysisMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	// declare 与 NewPopularityMQ 完全同形：每次（重）建 Channel 都重新声明，
	// broker 整体重启后拓扑也能自愈。
	pub := NewPublisher(base, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, contentAnalysisExchange, contentAnalysisQueue, contentAnalysisBindingKey)
	})
	// 启动期仍然要求连得上第一条 Channel：与既有 MQ 的行为一致。
	// 注意这**不**影响 AI 的降级：这个发布器尚未接入任何业务路径（P1 才接线，
	// 见下），接入后也必须是"构造失败 → 跳过注入 + 记 Warn"，
	// 绝不能让 MQ 不可用把 API 的启动拖垮。
	if err := pub.Init(); err != nil {
		return nil, err
	}
	return &ContentAnalysisMQ{pub: pub}, nil
}

// Request 投递一次分析任务。
//
// **best-effort 语义**：调用方拿到 error 时应记 Warn 后放弃，
// 不要降级成同步调用模型，也不要重试到阻塞请求——
// 这是与 like_service.go 里"M 失败则直接写库"不同的取舍，
// 因为 AI 结果不是业务正确性的必要条件。
func (c *ContentAnalysisMQ) Request(ctx context.Context, evt ContentAnalysisEvent) error {
	if c == nil || c.pub == nil {
		return errors.New("content analysis mq is not initialized")
	}
	if evt.VideoID == 0 {
		return errors.New("video_id is required")
	}
	if evt.AnalysisType == "" {
		return errors.New("analysis_type is required")
	}
	// event_id 由发布方生成：worker 侧靠它做幂等与日志串联。
	// 调用方传了就用调用方的（便于业务侧关联），没传则这里补一个。
	if evt.EventID == "" {
		id, err := newEventID(16)
		if err != nil {
			return err
		}
		evt.EventID = id
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	return c.pub.Do(func(ch *amqp.Channel) error {
		return PublishJSON(ctx, ch, contentAnalysisExchange, contentAnalysisRequestRK, evt)
	})
}
