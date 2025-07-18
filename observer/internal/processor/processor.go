package processor

import (
	"context"
	"errors"
	"log"
	"observer_service/internal/config"
	"observer_service/internal/models"
	"observer_service/internal/services/alerter"
	"observer_service/internal/services/publisher"
	"observer_service/internal/services/storage"
	"sync"
	"time"
)

// LogProcessor обрабатывает входящие логи.
type LogProcessor struct {
	storage           storage.IPStorage
	publisher         publisher.EventPublisher
	alerter           alerter.Notifier
	cfg               *config.Config
	logChannel        chan []models.LogEntry // Канал для получения пачек логов
	sideEffectChannel chan func()            // Канал для побочных задач (алерты, очистка)
}

// NewLogProcessor создает новый экземпляр LogProcessor.
func NewLogProcessor(s storage.IPStorage, p publisher.EventPublisher, a alerter.Notifier, cfg *config.Config) *LogProcessor {
	return &LogProcessor{
		storage:           s,
		publisher:         p,
		alerter:           a,
		cfg:               cfg,
		logChannel:        make(chan []models.LogEntry, cfg.LogChannelBufferSize),
		sideEffectChannel: make(chan func(), cfg.SideEffectChannelBufferSize),
	}
}

// StartWorkerPool запускает пул горутин-воркеров для обработки логов.
func (p *LogProcessor) StartWorkerPool(ctx context.Context, mainWg *sync.WaitGroup) {
	defer mainWg.Done()

	var workerWg sync.WaitGroup
	log.Printf("Запуск пула воркеров обработки логов в количестве %d...", p.cfg.WorkerPoolSize)

	for i := 0; i < p.cfg.WorkerPoolSize; i++ {
		workerWg.Add(1)
		go func(workerID int) {
			defer workerWg.Done()
			log.Printf("Воркер обработки логов %d запущен", workerID)
			for entries := range p.logChannel {
				p.ProcessEntries(ctx, entries)
			}
			log.Printf("Воркер обработки логов %d останавливается.", workerID)
		}(i + 1)
	}

	<-ctx.Done()
	log.Println("Получен сигнал остановки для воркеров обработки логов. Закрываю канал...")
	close(p.logChannel)
	workerWg.Wait()
	log.Println("Все воркеры обработки логов успешно остановлены.")
}

// StartSideEffectWorkerPool запускает пул воркеров для выполнения побочных задач.
func (p *LogProcessor) StartSideEffectWorkerPool(ctx context.Context, mainWg *sync.WaitGroup) {
	defer mainWg.Done()

	var workerWg sync.WaitGroup
	log.Printf("Запуск пула воркеров побочных задач в количестве %d...", p.cfg.SideEffectWorkerPoolSize)

	for i := 0; i < p.cfg.SideEffectWorkerPoolSize; i++ {
		workerWg.Add(1)
		go func(workerID int) {
			defer workerWg.Done()
			log.Printf("Воркер побочных задач %d запущен", workerID)
			for task := range p.sideEffectChannel {
				// Проверяем, не был ли контекст отменен перед выполнением задачи
				select {
				case <-ctx.Done():
					log.Printf("Воркер побочных задач %d пропустил задачу из-за отмены контекста.", workerID)
				default:
					task()
				}
			}
			log.Printf("Воркер побочных задач %d останавливается.", workerID)
		}(i + 1)
	}

	<-ctx.Done()
	log.Println("Получен сигнал остановки для воркеров побочных задач. Закрываю канал...")
	close(p.sideEffectChannel)
	workerWg.Wait()
	log.Println("Все воркеры побочных задач успешно остановлены.")
}

// EnqueueEntries добавляет пачку логов в очередь на обработку.
func (p *LogProcessor) EnqueueEntries(entries []models.LogEntry) error {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Попытка записи в закрытый канал логов. Сервис находится в процессе остановки.")
		}
	}()

	select {
	case p.logChannel <- entries:
		return nil
	default:
		return errors.New("log channel is full, rejecting new entries")
	}
}

// enqueueSideEffectTask добавляет побочную задачу в очередь на выполнение.
func (p *LogProcessor) enqueueSideEffectTask(task func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Попытка записи в закрытый канал побочных задач. Сервис находится в процессе остановки.")
		}
	}()

	select {
	case p.sideEffectChannel <- task:
		// Задача успешно добавлена в очередь
	default:
		log.Println("Warning: очередь побочных задач заполнена. Задача отброшена.")
	}
}

// ProcessEntries обрабатывает пачку записей логов.
func (p *LogProcessor) ProcessEntries(ctx context.Context, entries []models.LogEntry) {
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			log.Printf("Обработка пачки прервана из-за отмены контекста: %v", ctx.Err())
			return
		default:
			p.processSingleEntry(ctx, entry)
		}
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
		// Проверяем, не была ли ошибка вызвана отменой контекста
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Операция CheckAndAddIP отменена для %s: %v", entry.UserEmail, err)
		} else {
			log.Printf("Ошибка обработки записи для %s: %v", entry.UserEmail, err)
		}
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
				taskCtx := ctx
				p.enqueueSideEffectTask(func() {
					p.scheduleIPsClear(taskCtx, entry.UserEmail)
				})
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
		p.enqueueSideEffectTask(func() {
			if err := p.alerter.SendAlert(alertPayload); err != nil {
				log.Printf("Ошибка отправки вебхук-уведомления: %v", err)
			}
		})
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

func (p *LogProcessor) scheduleIPsClear(ctx context.Context, userEmail string) {
	timer := time.NewTimer(p.cfg.ClearIPsDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		log.Printf("Отложенная очистка IP для %s отменена (до таймера) из-за остановки сервиса.", userEmail)
		return
	case <-timer.C:
	}

	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cleared, err := p.storage.ClearUserIPs(opCtx, userEmail)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("Отложенная очистка IP для %s отменена из-за остановки сервиса во время выполнения.", userEmail)
		} else if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Таймаут при отложенной очистке IP для %s.", userEmail)
		} else {
			log.Printf("Ошибка при отложенной очистке IP для %s: %v", userEmail, err)
		}
		return
	}
	log.Printf("Отложенная очистка IP для %s%s выполнена. Очищено ключей: %d",
		userEmail, p.getDebugMarker(userEmail), cleared)
}