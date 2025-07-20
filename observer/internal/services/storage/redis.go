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

// Скрипт для атомарной очистки всех ключей IP пользователя.
const clearUserIPsScript = `
local ips = redis.call('SMEMBERS', KEYS[1])
if #ips == 0 then
    return redis.call('DEL', KEYS[1])
end
local keysToDelete = { KEYS[1] }
local prefix = ARGV[1]
for i, ip in ipairs(ips) do
    table.insert(keysToDelete, prefix .. ':' .. ip)
end
return redis.call('DEL', unpack(keysToDelete))
`

// Скрипт для атомарной очистки всех ключей ПОДСЕТЕЙ пользователя.
const clearUserSubnetsScript = `
local subnets = redis.call('SMEMBERS', KEYS[1])
if #subnets == 0 then
    return redis.call('DEL', KEYS[1])
end
local keysToDelete = { KEYS[1] }
local prefix = ARGV[1]
for i, subnet in ipairs(subnets) do
    table.insert(keysToDelete, prefix .. ':' .. subnet)
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
	CheckAndAddSubnet(ctx context.Context, email, subnet string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error)
	ClearUserSubnets(ctx context.Context, email string) (int, error)
}

// RedisStore реализует IPStorage с использованием Redis.
type RedisStore struct {
	client                  *redis.Client
	addCheckIPScriptSHA     string
	clearIPsScriptSHA       string
	addCheckSubnetScriptSHA string
	clearSubnetsScriptSHA   string
}

// NewRedisStore создает новый экземпляр RedisStore.
func NewRedisStore(ctx context.Context, redisURL string, scriptPaths ...string) (*RedisStore, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга Redis URL: %w", err)
	}
	client := redis.NewClient(opt)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ошибка подключения к Redis: %w", err)
	}

	// Загрузка скрипта проверки и добавления IP из файла
	addCheckIPScript, err := os.ReadFile("internal/scripts/add_and_check_ip.lua")
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения Lua-скрипта 'add_and_check_ip.lua': %w", err)
	}
	addCheckIPScriptSHA, err := client.ScriptLoad(ctx, string(addCheckIPScript)).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (add/check ip) в Redis: %w", err)
	}

	// Загрузка скрипта проверки и добавления ПОДСЕТИ из файла
	addCheckSubnetScript, err := os.ReadFile("internal/scripts/add_and_check_subnet.lua")
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения Lua-скрипта 'add_and_check_subnet.lua': %w", err)
	}
	addCheckSubnetScriptSHA, err := client.ScriptLoad(ctx, string(addCheckSubnetScript)).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (add/check subnet) в Redis: %w", err)
	}

	// Загрузка скрипта атомарной очистки IP из константы
	clearIPsScriptSHA, err := client.ScriptLoad(ctx, clearUserIPsScript).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (clear ip) в Redis: %w", err)
	}

	// Загрузка скрипта атомарной очистки ПОДСЕТЕЙ из константы
	clearSubnetsScriptSHA, err := client.ScriptLoad(ctx, clearUserSubnetsScript).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (clear subnet) в Redis: %w", err)
	}

	log.Println("Успешное подключение к Redis и загрузка Lua-скриптов.")
	return &RedisStore{
		client:                  client,
		addCheckIPScriptSHA:     addCheckIPScriptSHA,
		clearIPsScriptSHA:       clearIPsScriptSHA,
		addCheckSubnetScriptSHA: addCheckSubnetScriptSHA,
		clearSubnetsScriptSHA:   clearSubnetsScriptSHA,
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

	result, err := s.client.EvalSha(ctx, s.addCheckIPScriptSHA, []string{userIPsSetKey, alertSentKey}, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка выполнения Lua-скрипта для %s: %w", email, err)
	}

	return parseCheckResult(result, email)
}

// CheckAndAddSubnet выполняет Lua-скрипт для атомарной проверки и добавления подсети.
func (s *RedisStore) CheckAndAddSubnet(ctx context.Context, email, subnet string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error) {
	userSubnetsSetKey := fmt.Sprintf("user_subnets:%s", email)
	alertSentKey := fmt.Sprintf("alert_sent:%s", email)

	args := []interface{}{
		subnet,
		int(ttl.Seconds()),
		limit,
		int(cooldown.Seconds()),
	}

	result, err := s.client.EvalSha(ctx, s.addCheckSubnetScriptSHA, []string{userSubnetsSetKey, alertSentKey}, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка выполнения Lua-скрипта (subnet) для %s: %w", email, err)
	}

	return parseCheckResult(result, email)
}

func parseCheckResult(result interface{}, identifier string) (*models.CheckResult, error) {
	resSlice, ok := result.([]interface{})
	if !ok || len(resSlice) < 1 {
		return nil, fmt.Errorf("неожиданный результат от Lua-скрипта для %s", identifier)
	}

	statusCode, _ := resSlice[0].(int64)
	checkResult := &models.CheckResult{StatusCode: statusCode}

	switch statusCode {
	case 0: // OK
		checkResult.CurrentCount, _ = resSlice[1].(int64)
		isNew, _ := resSlice[2].(int64)
		checkResult.IsNew = isNew == 1
	case 1: // Limit exceeded, block
		itemsInterfaces, _ := resSlice[1].([]interface{})
		for _, itemInt := range itemsInterfaces {
			if itemStr, ok := itemInt.(string); ok {
				checkResult.AllUserItems = append(checkResult.AllUserItems, itemStr)
			}
		}
		checkResult.CurrentCount = int64(len(checkResult.AllUserItems))
	case 2: // Limit exceeded, on cooldown
		checkResult.CurrentCount, _ = resSlice[1].(int64)
	}

	return checkResult, nil
}

// ClearUserIPs атомарно удаляет все ключи, связанные с IP пользователя, используя Lua-скрипт.
func (s *RedisStore) ClearUserIPs(ctx context.Context, email string) (int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", email)
	ipTtlPrefix := fmt.Sprintf("ip_ttl:%s", email)

	deleted, err := s.client.EvalSha(ctx, s.clearIPsScriptSHA, []string{userIpsKey}, ipTtlPrefix).Int64()
	if err != nil {
		if err == redis.Nil {
			return 0, nil
		}
		return 0, fmt.Errorf("ошибка выполнения Lua-скрипта (clear ip) для %s: %w", email, err)
	}

	return int(deleted), nil
}

// ClearUserSubnets атомарно удаляет все ключи, связанные с подсетями пользователя.
func (s *RedisStore) ClearUserSubnets(ctx context.Context, email string) (int, error) {
	userSubnetsKey := fmt.Sprintf("user_subnets:%s", email)
	subnetTtlPrefix := fmt.Sprintf("subnet_ttl:%s", email)

	deleted, err := s.client.EvalSha(ctx, s.clearSubnetsScriptSHA, []string{userSubnetsKey}, subnetTtlPrefix).Int64()
	if err != nil {
		if err == redis.Nil {
			return 0, nil
		}
		return 0, fmt.Errorf("ошибка выполнения Lua-скрипта (clear subnet) для %s: %w", email, err)
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
	emailSet := make(map[string]struct{})

	for {
		var keys []string
		var err error
		// Сканируем по общему паттерну, чтобы захватить и IP, и подсети
		keys, cursor, err = s.client.Scan(ctx, cursor, "user_*s:*", 50).Result()
		if err != nil {
			return nil, fmt.Errorf("ошибка при сканировании ключей (SCAN): %w", err)
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 2)
			if len(parts) == 2 {
				emailSet[parts[1]] = struct{}{}
			}
		}
		if cursor == 0 {
			break
		}
	}

	emails := make([]string, 0, len(emailSet))
	for email := range emailSet {
		emails = append(emails, email)
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