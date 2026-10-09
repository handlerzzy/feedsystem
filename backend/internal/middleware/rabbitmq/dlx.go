package rabbitmq

import (
	"github.com/handlerzzy/feedsystem/internal/logging"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

const (
	DLXExchange = "dlx.events"
)

// DeclareDLX 声明死信交换机和对应的死信队列
func DeclareDLX(ch *amqp.Channel, queueName string) error {
	if ch == nil {
		return nil
	}
	if err := ch.ExchangeDeclare(
		DLXExchange, "topic", true, false, false, false, nil,
	); err != nil {
		return err
	}
	dlxQueue := queueName + ".dlx"
	_, err := ch.QueueDeclare(
		dlxQueue, true, false, false, false, nil,
	)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(dlxQueue, "#", DLXExchange, false, nil); err != nil {
		return err
	}
	// 拓扑装配期无 ctx，用全局 logger；这是正常状态，故为 Info。
	logging.L().Info("DLX ready", zap.String("exchange", DLXExchange), zap.String("queue", dlxQueue))
	return nil
}
