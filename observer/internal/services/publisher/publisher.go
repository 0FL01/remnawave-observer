package publisher

import (
	"encoding/json"
	"fmt"
	"log"
	"observer_service/internal/models"
	"sync"
	"time"

	"github.com/rabbitmq/amqp091-go"
)

// EventPublisher определяет интерфейс для публикации событий.
type EventPublisher interface {
	PublishBlockMessage(ips []string, duration string) error
	Close() error
}

// RabbitMQPublisher реализует EventPublisher для RabbitMQ.
type RabbitMQPublisher struct {
	conn          *amqp091.Connection
	channel       *amqp091.Channel
	exchangeName  string
	url           string
	mux           sync.Mutex
	maxRetries    int
	retryDelay    time.Duration
}

// NewRabbitMQPublisher создает и настраивает нового издателя RabbitMQ.
func NewRabbitMQPublisher(url, exchangeName string) (*RabbitMQPublisher, error) {
	p := &RabbitMQPublisher{
		url:          url,
		exchangeName: exchangeName,
		maxRetries:   5,
		retryDelay:   2 * time.Second,
	}

	if err := p.connectWithRetry(); err != nil {
		return nil, fmt.Errorf("не удалось подключиться к RabbitMQ при инициализации: %w", err)
	}

	return p, nil
}

func (p *RabbitMQPublisher) connect() error {
	conn, err := amqp091.Dial(p.url)
	if err != nil {
		return fmt.Errorf("ошибка подключения к RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("ошибка создания канала RabbitMQ: %w", err)
	}

	err = ch.ExchangeDeclare(
		p.exchangeName, // name
		"fanout",       // type
		true,           // durable
		false,          // auto-deleted
		false,          // internal
		false,          // no-wait
		nil,            // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("ошибка создания exchange: %w", err)
	}

	p.conn = conn
	p.channel = ch
	log.Println("Успешное (пере)подключение к RabbitMQ и настройка канала.")
	return nil
}

func (p *RabbitMQPublisher) connectWithRetry() error {
	var err error
	for i := 0; i < p.maxRetries; i++ {
		err = p.connect()
		if err == nil {
			return nil
		}
		log.Printf("Не удалось подключиться к RabbitMQ (попытка %d/%d): %v. Повтор через %v...", i+1, p.maxRetries, err, p.retryDelay)
		time.Sleep(p.retryDelay)
	}
	return fmt.Errorf("не удалось подключиться к RabbitMQ после %d попыток: %w", p.maxRetries, err)
}

// PublishBlockMessage публикует сообщение о блокировке с логикой переподключения.
func (p *RabbitMQPublisher) PublishBlockMessage(ips []string, duration string) error {
	p.mux.Lock()
	defer p.mux.Unlock()

	blockMsg := models.BlockMessage{
		IPs:      ips,
		Duration: duration,
	}
	body, err := json.Marshal(blockMsg)
	if err != nil {
		return fmt.Errorf("ошибка сериализации сообщения о блокировке: %w", err)
	}

	for i := 0; i < p.maxRetries; i++ {
		if p.conn == nil || p.conn.IsClosed() {
			log.Println("Соединение с RabbitMQ потеряно. Попытка переподключения...")
			if err := p.connectWithRetry(); err != nil {
				log.Printf("Не удалось восстановить соединение с RabbitMQ: %v", err)
				time.Sleep(p.retryDelay)
				continue
			}
		}

		err = p.channel.Publish(
			p.exchangeName,
			"",
			false,
			false,
			amqp091.Publishing{
				ContentType:  "application/json",
				Body:         body,
				DeliveryMode: amqp091.Persistent,
			},
		)

		if err == nil {
			return nil // Успех
		}

		log.Printf("Ошибка публикации сообщения в RabbitMQ (попытка %d/%d): %v. Повтор...", i+1, p.maxRetries, err)
		if p.conn != nil {
			p.conn.Close() // Принудительно закрываем, чтобы пересоздать на следующей итерации
		}
		time.Sleep(p.retryDelay)
	}

	return fmt.Errorf("критическая ошибка: не удалось опубликовать сообщение в RabbitMQ после %d попыток", p.maxRetries)
}

// Close закрывает соединение с RabbitMQ.
func (p *RabbitMQPublisher) Close() error {
	if p.conn != nil && !p.conn.IsClosed() {
		return p.conn.Close()
	}
	return nil
}