package rabbitmq

import (
	"blocker-worker/internal/logger"
	"context"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	blockingExchangeName = "blocking_exchange"
)

// Consumer обрабатывает подключение и получение сообщений из RabbitMQ.
type Consumer struct {
	logger      *logger.Logger
	conn        *amqp.Connection
	channel     *amqp.Channel
	rabbitMQURL string
}

// NewConsumer создает нового потребителя RabbitMQ.
func NewConsumer(l *logger.Logger, url string) *Consumer {
	return &Consumer{
		logger:      l,
		rabbitMQURL: url,
	}
}

// Connect устанавливает соединение с RabbitMQ и настраивает канал.
func (c *Consumer) Connect() error {
	var err error
	c.conn, err = amqp.Dial(c.rabbitMQURL)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к RabbitMQ: %w", err)
	}

	c.channel, err = c.conn.Channel()
	if err != nil {
		c.conn.Close() // Очищаем соединение, если канал не удалось создать
		return fmt.Errorf("не удалось открыть канал: %w", err)
	}

	c.logger.Info("Соединение с RabbitMQ успешно установлено.")
	return nil
}

// SetupAndConsume настраивает обменник, очередь и начинает потребление сообщений.
// Принимает функцию-обработчик для обработки сообщений.
func (c *Consumer) SetupAndConsume(ctx context.Context, handler func(context.Context, amqp.Delivery) error) error {
	if err := c.channel.Qos(1, 0, false); err != nil {
		return fmt.Errorf("не удалось установить QoS: %w", err)
	}

	err := c.channel.ExchangeDeclare(
		blockingExchangeName, "fanout", true, false, false, false, nil,
	)
	if err != nil {
		return fmt.Errorf("не удалось объявить exchange: %w", err)
	}

	q, err := c.channel.QueueDeclare(
		"", false, false, true, false, nil,
	)
	if err != nil {
		return fmt.Errorf("не удалось объявить очередь: %w", err)
	}

	err = c.channel.QueueBind(
		q.Name, "", blockingExchangeName, false, nil,
	)
	if err != nil {
		return fmt.Errorf("не удалось привязать очередь: %w", err)
	}

	msgs, err := c.channel.Consume(
		q.Name, "", false, false, false, false, nil,
	)
	if err != nil {
		return fmt.Errorf("не удалось зарегистрировать потребителя: %w", err)
	}

	c.logger.Info(" [*] Воркер готов. Ожидание сообщений о блокировке...")

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("Контекст отменен, прекращаем потребление сообщений.")
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("канал сообщений RabbitMQ был закрыт")
			}
			if err := handler(ctx, msg); err != nil {
				c.logger.Error(fmt.Sprintf("Ошибка при вызове обработчика сообщения: %v", err))
			}
		}
	}
}

// Close корректно закрывает соединение и канал.
func (c *Consumer) Close() {
	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil {
		c.conn.Close()
	}
	c.logger.Info("Соединение с RabbitMQ закрыто.")
}