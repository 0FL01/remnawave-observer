package processor

import (
	"blocker-worker/internal/logger"
	"blocker-worker/internal/models"
	"blocker-worker/internal/services/command"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
)

var validDurationPattern = regexp.MustCompile(`^\d+[smhd]$`)

// MessageProcessor инкапсулирует логику обработки одного сообщения RabbitMQ.
type MessageProcessor struct {
	logger   *logger.Logger
	executor *command.Executor
}

// NewMessageProcessor создает новый обработчик сообщений.
func NewMessageProcessor(l *logger.Logger, exec *command.Executor) *MessageProcessor {
	return &MessageProcessor{
		logger:   l,
		executor: exec,
	}
}

// isValidIPOrCIDR проверяет, является ли строка валидным IP-адресом или CIDR.
func isValidIPOrCIDR(s string) bool {
	// Проверяем CIDR (например, 192.168.1.0/24)
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	// Проверяем обычный IP-адрес
	return net.ParseIP(s) != nil
}

// Process принимает тело сообщения и выполняет действие по блокировке.
func (p *MessageProcessor) Process(ctx context.Context, body []byte) error {
	var payload models.BlockingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		p.logger.Error(fmt.Sprintf("Не удалось декодировать JSON из сообщения: %s", string(body)))
		return err
	}

	if len(payload.IPs) == 0 {
		p.logger.Info("Сообщение не содержит IP-адресов для блокировки, пропускаем.")
		return nil
	}

	duration := payload.Duration
	if duration == "" {
		duration = "5m"
	}

	if !validDurationPattern.MatchString(duration) {
		err := fmt.Errorf("недопустимый формат duration, сообщение отклонено: '%s'", duration)
		p.logger.Error(err.Error())
		return err
	}

	var wg sync.WaitGroup
	for _, ip := range payload.IPs {
		wg.Add(1)
		go func(ipAddress string) {
			defer wg.Done()

			// Баг #3: Валидация IP/CIDR перед выполнением nft команды
			if !isValidIPOrCIDR(ipAddress) {
				p.logger.Error(fmt.Sprintf("Невалидный IP/CIDR пропущен: %s", ipAddress))
				return
			}

			// Баг #1: Объединяем set expression в один аргумент для корректной работы nft
			setExpr := fmt.Sprintf("{ %s timeout %s }", ipAddress, duration)
			err := p.executor.RunNftCommand(ctx, "add", "element", "inet", "firewall", "user_blacklist", setExpr)
			if err != nil {
				p.logger.Error(fmt.Sprintf("Ошибка при обработке IP %s: %v", ipAddress, err))
			}
		}(ip)
	}
	wg.Wait()
	return nil
}