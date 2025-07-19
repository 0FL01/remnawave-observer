package logger

import (
	"log"
	"os"
)

const logPrefix = "[BlockerWorker]"

// Logger — это обертка для стандартного log.Logger для добавления префикса.
type Logger struct {
	*log.Logger
}

// New создает новый экземпляр Logger.
func New() *Logger {
	return &Logger{
		Logger: log.New(os.Stdout, "", log.LstdFlags),
	}
}

// Info логирует информационное сообщение.
func (l *Logger) Info(msg string) {
	l.Printf("INFO - %s - %s", logPrefix, msg)
}

// Error логирует сообщение об ошибке.
func (l *Logger) Error(msg string) {
	l.Printf("ERROR - %s - %s", logPrefix, msg)
}

// Warning логирует предупреждение.
func (l *Logger) Warning(msg string) {
	l.Printf("WARNING - %s - %s", logPrefix, msg)
}