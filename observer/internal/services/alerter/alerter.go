package alerter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"observer_service/internal/models"
	"time"
)

// Notifier определяет интерфейс для отправки уведомлений.
type Notifier interface {
	SendAlert(payload models.AlertPayload) error
}

// WebhookAlerter реализует Notifier для отправки вебхуков.
type WebhookAlerter struct {
	client   *http.Client
	url      string
}

// NewWebhookAlerter создает новый экземпляр WebhookAlerter.
func NewWebhookAlerter(url string) *WebhookAlerter {
	return &WebhookAlerter{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		url: url,
	}
}

// SendAlert отправляет уведомление на заданный URL.
func (a *WebhookAlerter) SendAlert(payload models.AlertPayload) error {
	if a.url == "" {
		log.Println("ALERT_WEBHOOK_URL не задан, вебхук не отправляется")
		return nil
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка сериализации payload: %w", err)
	}

	log.Printf("Попытка отправить вебхук на URL: %s", a.url)
	resp, err := a.client.Post(a.url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("сетевая ошибка при отправке вебхука: %w", err)
	}
	defer resp.Body.Close()

	var respBody bytes.Buffer
	_, _ = respBody.ReadFrom(resp.Body)

	log.Printf("Вебхук-уведомление для %s отправлен. Статус ответа: %d. Тело ответа: %s",
		payload.UserIdentifier, resp.StatusCode, respBody.String())

	if resp.StatusCode >= 400 {
		return fmt.Errorf("сервер вебхука ответил ошибкой: %s", resp.Status)
	}

	return nil
}