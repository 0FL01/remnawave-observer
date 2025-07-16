package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	defaultRabbitMQURL      = "amqp://guest:guest@localhost/"
	blockingExchangeName    = "blocking_exchange"
	reconnectDelay          = 5 * time.Second
	logPrefix               = "[BlockerWorker]"
)

// BlockingPayload представляет структуру входящих сообщений
type BlockingPayload struct {
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// Logger обертка для логирования с префиксом
type Logger struct {
	*log.Logger
}

func NewLogger() *Logger {
	return &Logger{
		Logger: log.New(os.Stdout, "", log.LstdFlags),
	}
}

func (l *Logger) Info(msg string) {
	l.Printf("INFO - %s - %s", logPrefix, msg)
}

func (l *Logger) Error(msg string) {
	l.Printf("ERROR - %s - %s", logPrefix, msg)
}

func (l *Logger) Warning(msg string) {
	l.Printf("WARNING - %s - %s", logPrefix, msg)
}

// BlockerWorker основная структура воркера
type BlockerWorker struct {
	logger     *Logger
	rabbitmqURL string
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewBlockerWorker() *BlockerWorker {
	ctx, cancel := context.WithCancel(context.Background())
	
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = defaultRabbitMQURL
	}

	return &BlockerWorker{
		logger:      NewLogger(),
		rabbitmqURL: rabbitmqURL,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// runCommand выполняет одну команду nftables
func (bw *BlockerWorker) runCommand(args ...string) error {
	// Первый аргумент - это сама команда, остальные - её параметры
	cmd := exec.CommandContext(bw.ctx, args[0], args[1:]...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Собираем команду в строку только для логирования
		fullCommand := strings.Join(args, " ")
		bw.logger.Error(fmt.Sprintf("Ошибка выполнения команды '%s': %s", fullCommand, string(output)))
		return err
	}

	fullCommand := strings.Join(args, " ")
	bw.logger.Info(fmt.Sprintf("Команда '%s' выполнена успешно.", fullCommand))
	return nil
}

// processMessage обрабатывает одно сообщение из очереди
func (bw *BlockerWorker) processMessage(body []byte) error {
	var payload BlockingPayload
	
	if err := json.Unmarshal(body, &payload); err != nil {
		bw.logger.Error(fmt.Sprintf("Не удалось декодировать JSON из сообщения: %s", string(body)))
		return err
	}

	if len(payload.IPs) == 0 {
		return nil
	}

	// Устанавливаем значение по умолчанию для duration
	duration := payload.Duration
	if duration == "" {
		duration = "5m"
	}

	var wg sync.WaitGroup
	for _, ip := range payload.IPs {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			// Передаем команду и аргументы раздельно, чтобы избежать инъекции
			err := bw.runCommand("nft", "add", "element", "inet", "firewall", "user_blacklist", "{", ip, "timeout", duration, "}")
			if err != nil {
				bw.logger.Error(fmt.Sprintf("Ошибка при обработке IP %s: %v", ip, err))
			}
		}(ip)
	}

	wg.Wait()
	return nil
}

// handleConnection обрабатывает одно соединение с RabbitMQ
func (bw *BlockerWorker) handleConnection() error {
	conn, err := amqp.Dial(bw.rabbitmqURL)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к RabbitMQ: %w", err)
	}
	defer conn.Close()

	bw.logger.Info("Соединение с RabbitMQ успешно установлено.")

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("не удалось открыть канал: %w", err)
	}
	defer ch.Close()

	// Устанавливаем QoS
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("не удалось установить QoS: %w", err)
	}

	// Объявляем exchange
	err = ch.ExchangeDeclare(
		blockingExchangeName, // name
		"fanout",             // type
		true,                 // durable
		false,                // auto-deleted
		false,                // internal
		false,                // no-wait
		nil,                  // arguments
	)
	if err != nil {
		return fmt.Errorf("не удалось объявить exchange: %w", err)
	}

	// Объявляем очередь
	q, err := ch.QueueDeclare(
		"",    // name (пустое имя означает генерацию случайного имени)
		false, // durable
		false, // delete when unused
		true,  // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return fmt.Errorf("не удалось объявить очередь: %w", err)
	}

	// Привязываем очередь к exchange
	err = ch.QueueBind(
		q.Name,               // queue name
		"",                   // routing key
		blockingExchangeName, // exchange
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("не удалось привязать очередь: %w", err)
	}

	// Начинаем потребление сообщений
	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	if err != nil {
		return fmt.Errorf("не удалось зарегистрировать потребителя: %w", err)
	}

	bw.logger.Info(" [*] Воркер готов. Ожидание сообщений о блокировке...")

	// Слушаем сообщения до тех пор, пока соединение не будет закрыто
	for {
		select {
		case <-bw.ctx.Done():
			bw.logger.Info("Получен сигнал остановки, завершаем работу...")
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("канал сообщений закрыт")
			}
			
			if err := bw.processMessage(msg.Body); err != nil {
				bw.logger.Error(fmt.Sprintf("Произошла непредвиденная ошибка при обработке сообщения: %v", err))
			}
		}
	}
}

// Run запускает основной цикл воркера
func (bw *BlockerWorker) Run() {
	bw.logger.Info("Запуск Blocker Worker...")

	// Обработка сигналов для graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		bw.logger.Info("Получен сигнал завершения...")
		bw.cancel()
	}()

	// Основной цикл с переподключением
	for {
		select {
		case <-bw.ctx.Done():
			bw.logger.Info("Воркер остановлен.")
			return
		default:
			if err := bw.handleConnection(); err != nil {
				bw.logger.Warning(fmt.Sprintf("Не удалось подключиться к RabbitMQ или соединение было потеряно: %v. Повторная попытка через 5 секунд...", err))
				
				// Ждем перед повторной попыткой, но также слушаем контекст
				select {
				case <-bw.ctx.Done():
					return
				case <-time.After(reconnectDelay):
					continue
				}
			}
		}
	}
}

func main() {
	worker := NewBlockerWorker()
	worker.Run()
}