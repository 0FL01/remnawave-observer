package worker

import (
	"blocker-worker/internal/config"
	"blocker-worker/internal/logger"
	"blocker-worker/internal/processor"
	"blocker-worker/internal/services/rabbitmq"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Worker — это основная структура приложения.
type Worker struct {
	logger    *logger.Logger
	cfg       *config.Config
	processor *processor.MessageProcessor
	ctx       context.Context
	cancel    context.CancelFunc
}

// New создает нового Worker'а.
func New(l *logger.Logger, cfg *config.Config, proc *processor.MessageProcessor) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		logger:    l,
		cfg:       cfg,
		processor: proc,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// handleMessage является функцией обратного вызова для потребителя RabbitMQ.
func (w *Worker) handleMessage(ctx context.Context, msg amqp.Delivery) error {
	err := w.processor.Process(ctx, msg.Body)
	if err != nil {
		w.logger.Error(fmt.Sprintf("Произошла ошибка при обработке сообщения: %v. Сообщение не будет подтверждено.", err))
		if errNack := msg.Nack(false, false); errNack != nil {
			w.logger.Error(fmt.Sprintf("Ошибка при Nack сообщения: %v", errNack))
		}
		return nil
	}

	if errAck := msg.Ack(false); errAck != nil {
		w.logger.Error(fmt.Sprintf("Ошибка при Ack сообщения: %v", errAck))
		return errAck
	}
	return nil
}

// Run запускает воркер, обрабатывает корректное завершение и переподключения к RabbitMQ.
func (w *Worker) Run() {
	w.logger.Info("Запуск Blocker Worker...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		w.logger.Info("Получен сигнал завершения, начинаем остановку...")
		w.cancel()
	}()

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("Воркер остановлен.")
			return
		default:
			consumer := rabbitmq.NewConsumer(w.logger, w.cfg.RabbitMQURL)
			if err := consumer.Connect(); err != nil {
				w.logger.Warning(fmt.Sprintf("Не удалось подключиться к RabbitMQ: %v. Повторная попытка через %v...", err, w.cfg.ReconnectDelay))
				w.waitOrExit()
				continue
			}

			err := consumer.SetupAndConsume(w.ctx, w.handleMessage)
			consumer.Close()

			if err != nil {
				w.logger.Warning(fmt.Sprintf("Соединение с RabbitMQ потеряно: %v. Повторная попытка через %v...", err, w.cfg.ReconnectDelay))
			}

			w.waitOrExit()
		}
	}
}

// waitOrExit приостанавливает выполнение на время задержки переподключения, но немедленно выходит, если контекст отменен.
func (w *Worker) waitOrExit() {
	select {
	case <-w.ctx.Done():
		return
	case <-time.After(w.cfg.ReconnectDelay):
	}
}