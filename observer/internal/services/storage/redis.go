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
	client    *redis.Client
	scriptSHA string
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

	script, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения Lua-скрипта '%s': %w", scriptPath, err)
	}

	scriptSHA, err := client.ScriptLoad(ctx, string(script)).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта в Redis: %w", err)
	}

	log.Println("Успешное подключение к Redis и загрузка Lua-скрипта.")
	return &RedisStore{client: client, scriptSHA: scriptSHA}, nil
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

	result, err := s.client.EvalSha(ctx, s.scriptSHA, []string{userIPsSetKey, alertSentKey}, args...).Result()
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

// ClearUserIPs удаляет все ключи, связанные с пользователем.
func (s *RedisStore) ClearUserIPs(ctx context.Context, email string) (int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", email)
	ips, err := s.client.SMembers(ctx, userIpsKey).Result()
	if err != nil {
		return 0, err
	}
	if len(ips) == 0 {
		return 0, nil
	}

	keysToDelete := make([]string, 0, len(ips)+1)
	keysToDelete = append(keysToDelete, userIpsKey)
	for _, ip := range ips {
		ipTtlKey := fmt.Sprintf("ip_ttl:%s:%s", email, ip)
		keysToDelete = append(keysToDelete, ipTtlKey)
	}

	deleted, err := s.client.Del(ctx, keysToDelete...).Result()
	if err != nil {
		return 0, err
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

// GetAllUserEmails сканирует ключи Redis для получения всех email пользователей.
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