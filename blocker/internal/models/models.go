package models

// BlockingPayload представляет структуру входящих сообщений из RabbitMQ.
type BlockingPayload struct {
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}