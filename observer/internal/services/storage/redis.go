package storage

import (
	"context"
	"fmt"
	"log"
	"observer_service/internal/models"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Скрипт для атомарной очистки всех ключей пользователя.
// Он получает все IP из множества пользователя, формирует список всех связанных ключей
// (само множество и ключи TTL для каждого IP) и удаляет их одной командой DEL.
// Это гарантирует, что никакие новые IP не "просочатся" между чтением и удалением.
//
// KEYS[1]: ключ множества IP пользователя
// ARGV[1]: префикс для ключей TTL
const clearUserIPsScript = `
local ips = redis.call('SMEMBERS', KEYS[1])
local keysToDelete = { KEYS[1] }

for i, ip in ipairs(ips) do
    table.insert(keysToDelete, ARGV[1] .. ':' .. ip)
end

return redis.call('DEL', unpack(keysToDelete))
`

// IPStorage определяет интерфейс для работы с хранилищем IP-адресов.
type IPStorage interface {
	CheckAndAddIP(ctx context.Context, email, ip string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error)
	ClearUserIPs(ctx context.Context, email string) (int, error)
	GetUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error)
	GetAllUserEmails(ctx context.Context) ([]string, error)
	HasAlertCooldown(ctx context.Context, userEmail string) (bool, error)
	Ping(ctx context.Context) error
	Close() error
}

// RedisStore реализует IPStorage с использованием Redis.
type RedisStore struct {
	client            *redis.Client
	addCheckScriptSHA string
	clearScriptSHA    string
}

// NewRedisStore создает новый экземпляр RedisStore.
func NewRedisStore(ctx context.Context, redisURL, scriptPath string) (*RedisStore, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга Redis URL: %w", err)
	}
	client := redis.NewClient(opt)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ошибка подключения к Redis: %w", err)
	}

	// Загрузка скрипта проверки и добавления IP из файла
	addCheckScript, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения Lua-скрипта '%s': %w", scriptPath, err)
	}
	addCheckScriptSHA, err := client.ScriptLoad(ctx, string(addCheckScript)).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (add/check) в Redis: %w", err)
	}

	// Загрузка скрипта атомарной очистки из константы
	clearScriptSHA, err := client.ScriptLoad(ctx, clearUserIPsScript).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (clear) в Redis: %w", err)
	}

	log.Println("Успешное подключение к Redis и загрузка Lua-скриптов.")
	return &RedisStore{
		client:            client,
		addCheckScriptSHA: addCheckScriptSHA,
		clearScriptSHA:    clearScriptSHA,
	}, nil
}

// CheckAndAddIP выполняет Lua-скрипт для атомарной проверки и добавления IP.
func (s *RedisStore) CheckAndAddIP(ctx context.Context, email, ip string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error) {
	userIPsSetKey := fmt.Sprintf("user_ips:%s", email)
	alertSentKey := fmt.Sprintf("alert_sent:%s", email)

	args := []interface{}{
		ip,
		int(ttl.Seconds()),
		limit,
		int(cooldown.Seconds()),
	}

	result, err := s.client.EvalSha(ctx, s.addCheckScriptSHA, []string{userIPsSetKey, alertSentKey}, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка выполнения Lua-скрипта для %s: %w", email, err)
	}

	resSlice, ok := result.([]interface{})
	if !ok || len(resSlice) < 1 {
		return nil, fmt.Errorf("неожиданный результат от Lua-скрипта для %s", email)
	}

	statusCode, _ := resSlice[0].(int64)
	checkResult := &models.CheckResult{StatusCode: statusCode}

	switch statusCode {
	case 0: // OK
		checkResult.CurrentIPCount, _ = resSlice[1].(int64)
		isNew, _ := resSlice[2].(int64)
		checkResult.IsNewIP = isNew == 1
	case 1: // Limit exceeded, block
		ipInterfaces, _ := resSlice[1].([]interface{})
		for _, ipInt := range ipInterfaces {
			if ipStr, ok := ipInt.(string); ok {
				checkResult.AllUserIPs = append(checkResult.AllUserIPs, ipStr)
			}
		}
		checkResult.CurrentIPCount = int64(len(checkResult.AllUserIPs))
	case 2: // Limit exceeded, on cooldown
		checkResult.CurrentIPCount, _ = resSlice[1].(int64)
	}

	return checkResult, nil
}

// ClearUserIPs атомарно удаляет все ключи, связанные с пользователем, используя Lua-скрипт.
func (s *RedisStore) ClearUserIPs(ctx context.Context, email string) (int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", email)
	ipTtlPrefix := fmt.Sprintf("ip_ttl:%s", email)

	// Выполняем Lua-скрипт, который атомарно получает список IP и удаляет все связанные ключи.
	deleted, err := s.client.EvalSha(ctx, s.clearScriptSHA, []string{userIpsKey}, ipTtlPrefix).Int64()
	if err != nil {
		// Скрипт DEL не должен возвращать redis.Nil, но на всякий случай проверяем.
		if err == redis.Nil {
			return 0, nil
		}
		return 0, fmt.Errorf("ошибка выполнения Lua-скрипта (clear) для %s: %w", email, err)
	}

	return int(deleted), nil
}

// GetUserActiveIPs возвращает активные IP пользователя с их TTL.
func (s *RedisStore) GetUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", userEmail)
	ips, err := s.client.SMembers(ctx, userIpsKey).Result()
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return make(map[string]int), nil
	}

	activeIPs := make(map[string]int)
	pipe := s.client.Pipeline()
	ttlResults := make(map[string]*redis.DurationCmd)

	for _, ip := range ips {
		ipTtlKey := fmt.Sprintf("ip_ttl:%s:%s", userEmail, ip)
		ttlResults[ip] = pipe.TTL(ctx, ipTtlKey)
	}
	_, err = pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	for ip, cmd := range ttlResults {
		ttl, err := cmd.Result()
		if err != nil || ttl <= 0 {
			continue
		}
		activeIPs[ip] = int(ttl.Seconds())
	}
	return activeIPs, nil
}

// GetAllUserEmails сканирует ключи Redis для получения всех username (email) пользователей.
func (s *RedisStore) GetAllUserEmails(ctx context.Context) ([]string, error) {
	var cursor uint64
	var emails []string
	for {
		var keys []string
		var err error
		keys, cursor, err = s.client.Scan(ctx, cursor, "user_ips:*", 500).Result()
		if err != nil {
			return nil, fmt.Errorf("ошибка при сканировании ключей (SCAN): %w", err)
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 2)
			if len(parts) == 2 {
				emails = append(emails, parts[1])
			}
		}
		if cursor == 0 {
			break
		}
	}
	return emails, nil
}

// HasAlertCooldown проверяет наличие ключа кулдауна для пользователя.
func (s *RedisStore) HasAlertCooldown(ctx context.Context, userEmail string) (bool, error) {
	alertCooldownKey := fmt.Sprintf("alert_sent:%s", userEmail)
	res, err := s.client.Exists(ctx, alertCooldownKey).Result()
	if err != nil {
		return false, err
	}
	return res > 0, nil
}

// Ping проверяет соединение с Redis.
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Close закрывает соединение с Redis.
func (s *RedisStore) Close() error {
	return s.client.Close()
}