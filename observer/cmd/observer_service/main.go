package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

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

	cfg := config.New()

	// Контекст для сигнализации о завершении работы фоновых процессов
	ctx, cancel := context.WithCancel(context.Background())
	// WaitGroup для ожидания завершения всех фоновых горутин
	var wg sync.WaitGroup

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

	logProcessor := processor.NewLogProcessor(redisStore, rabbitPublisher, webhookAlerter, cfg)
	poolMonitor := monitor.NewPoolMonitor(redisStore, cfg)
	apiServer := api.NewServer(cfg.Port, logProcessor, redisStore)

	wg.Add(2) // Сообщаем WaitGroup, что будем ждать две горутины
	go poolMonitor.Run(ctx, &wg)
	go logProcessor.StartWorkerPool(ctx, &wg)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: apiServer.GetRouter(), // Получаем роутер из нашего api.Server
	}

	go func() {
		log.Printf("Сервер Observer Service запущен на порту %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Получен сигнал завершения, начинаю остановку сервиса...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
	} else {
		log.Println("HTTP-сервер успешно остановлен.")
	}

	cancel()

	log.Println("Ожидание завершения фоновых процессов...")
	wg.Wait()

	log.Println("Все фоновые процессы остановлены. Сервис успешно остановлен.")
}