package command

import (
	"blocker-worker/internal/logger"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Executor отвечает за выполнение внешних команд.
type Executor struct {
	logger *logger.Logger
}

// NewExecutor создает новый исполнитель команд.
func NewExecutor(l *logger.Logger) *Executor {
	return &Executor{logger: l}
}

// RunNftCommand безопасно выполняет команду nftables.
func (e *Executor) RunNftCommand(ctx context.Context, args ...string) error {
	// Добавляем 'nft' в начало списка аргументов
	fullArgs := append([]string{"nft"}, args...)

	cmd := exec.CommandContext(ctx, fullArgs[0], fullArgs[1:]...)
	output, err := cmd.CombinedOutput()

	fullCommandStr := strings.Join(fullArgs, " ")
	if err != nil {
		e.logger.Error(fmt.Sprintf("Ошибка выполнения команды '%s': %s", fullCommandStr, string(output)))
		return err
	}

	e.logger.Info(fmt.Sprintf("Команда '%s' выполнена успешно.", fullCommandStr))
	return nil
}