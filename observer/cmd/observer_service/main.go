package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"observer_service/internal/api"
	"observer_service/internal/config"
	"observer_service/internal/monitor"
	"observer_service/internal/processor"
	"observer_service/internal/services/alerter"
	"observer_service/internal/services/publisher"
	"observer_service/internal/services/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 1. Загрузка конфигурации
	cfg := config.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Инициализация зависимостей (внешних сервисов)
	redisStore, err := storage.NewRedisStore(ctx, cfg.RedisURL, "internal/scripts/add_and_check_ip.lua")
	if err != nil {
		log.Fatalf("Критическая ошибка: не удалось подключиться к Redis: %v", err)
	}
	defer redisStore.Close()

	rabbitPublisher, err := publisher.NewRabbitMQPublisher(cfg.RabbitMQURL, cfg.BlockingExchangeName)
	if err != nil {
		log.Fatalf("Критическая ошибка: не удалось подключиться к RabbitMQ: %v", err)
	}
	defer rabbitPublisher.Close()

	webhookAlerter := alerter.NewWebhookAlerter(cfg.AlertWebhookURL)

	// 3. Инициализация основных компонентов приложения
	logProcessor := processor.NewLogProcessor(redisStore, rabbitPublisher, webhookAlerter, cfg)
	poolMonitor := monitor.NewPoolMonitor(redisStore, cfg)
	server := api.NewServer(cfg.Port, logProcessor, redisStore)

	// 4. Запуск фоновых процессов
	go poolMonitor.Run(ctx)

	// 5. Запуск API сервера
	go func() {
		log.Printf("Сервер Observer Service запущен на порту %s", cfg.Port)
		if err := server.Run(); err != nil {
			log.Fatalf("Ошибка запуска сервера: %v", err)
		}
	}()

	// 6. Ожидание сигнала завершения для graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Получен сигнал завершения, начинаю остановку сервиса...")
	cancel() // Сигнализируем всем компонентам, использующим context, о завершении
	// Здесь можно добавить логику ожидания завершения всех горутин, если требуется
	log.Println("Сервис успешно остановлен.")
}