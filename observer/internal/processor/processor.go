package processor

import (
	"context"
	"log"
	"observer_service/internal/config"
	"observer_service/internal/models"
	"observer_service/internal/services/alerter"
	"observer_service/internal/services/publisher"
	"observer_service/internal/services/storage"
	"time"
)

// LogProcessor обрабатывает входящие логи.
type LogProcessor struct {
	storage   storage.IPStorage
	publisher publisher.EventPublisher
	alerter   alerter.Notifier
	cfg       *config.Config
}

// NewLogProcessor создает новый экземпляр LogProcessor.
func NewLogProcessor(s storage.IPStorage, p publisher.EventPublisher, a alerter.Notifier, cfg *config.Config) *LogProcessor {
	return &LogProcessor{
		storage:   s,
		publisher: p,
		alerter:   a,
		cfg:       cfg,
	}
}

// ProcessEntries обрабатывает пачку записей логов.
func (p *LogProcessor) ProcessEntries(ctx context.Context, entries []models.LogEntry) {
	for _, entry := range entries {
		p.processSingleEntry(ctx, entry)
	}
}

func (p *LogProcessor) processSingleEntry(ctx context.Context, entry models.LogEntry) {
	if p.cfg.ExcludedUsers[entry.UserEmail] {
		return // Пользователь в списке исключений
	}

	userIPLimit := p.getUserIPLimit(entry.UserEmail)
	debugMarker := p.getDebugMarker(entry.UserEmail)

	res, err := p.storage.CheckAndAddIP(ctx, entry.UserEmail, entry.SourceIP, userIPLimit, p.cfg.UserIPTTL, p.cfg.AlertCooldown)
	if err != nil {
		log.Printf("Ошибка обработки записи для %s: %v", entry.UserEmail, err)
		return
	}

	if res.StatusCode == 0 && res.IsNewIP {
		log.Printf("Новый IP для пользователя %s%s: %s. Всего IP: %d/%d",
			entry.UserEmail, debugMarker, entry.SourceIP, res.CurrentIPCount, userIPLimit)
	}

	if res.StatusCode == 1 { // Лимит превышен, нужна блокировка
		log.Printf("ПРЕВЫШЕНИЕ ЛИМИТА%s: Пользователь %s, IP-адресов: %d/%d",
			debugMarker, entry.UserEmail, res.CurrentIPCount, userIPLimit)

		ipsToBlock := p.filterExcludedIPs(res.AllUserIPs, entry.UserEmail)

		if len(ipsToBlock) > 0 {
			if err := p.publisher.PublishBlockMessage(ipsToBlock, p.cfg.BlockDuration); err != nil {
				log.Printf("Ошибка отправки сообщения о блокировке: %v", err)
			} else {
				log.Printf("Сообщение о блокировке %d IP-адресов для %s%s отправлено", len(ipsToBlock), entry.UserEmail, debugMarker)
				go p.scheduleIPsClear(entry.UserEmail)
			}
		}

		alertPayload := models.AlertPayload{
			UserIdentifier:   entry.UserEmail,
			DetectedIPsCount: int(res.CurrentIPCount),
			Limit:            userIPLimit,
			AllUserIPs:       res.AllUserIPs,
			BlockDuration:    p.cfg.BlockDuration,
			ViolationType:    "ip_limit_exceeded",
		}
		go func() {
			if err := p.alerter.SendAlert(alertPayload); err != nil {
				log.Printf("Ошибка отправки вебхук-уведомления: %v", err)
			}
		}()
	}
}

func (p *LogProcessor) getUserIPLimit(userEmail string) int {
	if p.cfg.DebugEmail != "" && userEmail == p.cfg.DebugEmail {
		return p.cfg.DebugIPLimit
	}
	return p.cfg.MaxIPsPerUser
}

func (p *LogProcessor) getDebugMarker(userEmail string) string {
	if p.cfg.DebugEmail != "" && userEmail == p.cfg.DebugEmail {
		return " [DEBUG]"
	}
	return ""
}

func (p *LogProcessor) filterExcludedIPs(ips []string, email string) []string {
	var filtered []string
	for _, ip := range ips {
		if p.cfg.ExcludedIPs[ip] {
			log.Printf("IP-адрес %s для пользователя %s пропущен, так как находится в списке исключений.", ip, email)
			continue
		}
		filtered = append(filtered, ip)
	}
	return filtered
}

func (p *LogProcessor) scheduleIPsClear(userEmail string) {
	time.Sleep(p.cfg.ClearIPsDelay)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cleared, err := p.storage.ClearUserIPs(ctx, userEmail)
	if err != nil {
		log.Printf("Ошибка при отложенной очистке IP для %s: %v", userEmail, err)
		return
	}
	log.Printf("Отложенная очистка IP для %s%s выполнена. Очищено ключей: %d",
		userEmail, p.getDebugMarker(userEmail), cleared)
}