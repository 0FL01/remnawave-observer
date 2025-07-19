package config

import (
	"os"
	"time"
)

const (
	defaultRabbitMQURL = "amqp://guest:guest@localhost/"
	reconnectDelay     = 5 * time.Second
)

// Config хранит конфигурацию приложения.
type Config struct {
	RabbitMQURL    string
	ReconnectDelay time.Duration
}

// New создает новый экземпляр Config из переменных окружения.
func New() *Config {
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = defaultRabbitMQURL
	}

	return &Config{
		RabbitMQURL:    rabbitmqURL,
		ReconnectDelay: reconnectDelay,
	}
}