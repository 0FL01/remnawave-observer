package main

import (
	"blocker-worker/internal/config"
	"blocker-worker/internal/logger"
	"blocker-worker/internal/processor"
	"blocker-worker/internal/services/command"
	"blocker-worker/internal/worker"
)

func main() {
	// 1. Инициализация зависимостей
	l := logger.New()
	cfg := config.New()
	cmdExecutor := command.NewExecutor(l)
	msgProcessor := processor.NewMessageProcessor(l, cmdExecutor)

	// 2. Инициализация главного воркера
	appWorker := worker.New(l, cfg, msgProcessor)

	// 3. Запуск приложения
	appWorker.Run()
}