package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config хранит всю конфигурацию приложения.
type Config struct {
	Port                 string
	RedisURL             string
	RabbitMQURL          string
	MaxIPsPerUser        int
	AlertWebhookURL      string
	UserIPTTL            time.Duration
	AlertCooldown        time.Duration
	ClearIPsDelay        time.Duration
	BlockDuration        string
	BlockingExchangeName string
	MonitoringInterval   time.Duration
	DebugEmail           string
	DebugIPLimit         int
	ExcludedUsers        map[string]bool
	ExcludedIPs          map[string]bool
}

// New загружает конфигурацию из переменных окружения.
func New() *Config {
	cfg := &Config{
		Port:                 getEnv("PORT", "9000"),
		RedisURL:             getEnv("REDIS_URL", "redis://localhost:6379/0"),
		RabbitMQURL:          getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost/"),
		MaxIPsPerUser:        getEnvInt("MAX_IPS_PER_USER", 3),
		AlertWebhookURL:      getEnv("ALERT_WEBHOOK_URL", ""),
		UserIPTTL:            time.Duration(getEnvInt("USER_IP_TTL_SECONDS", 24*60*60)) * time.Second,
		AlertCooldown:        time.Duration(getEnvInt("ALERT_COOLDOWN_SECONDS", 60*60)) * time.Second,
		ClearIPsDelay:        time.Duration(getEnvInt("CLEAR_IPS_DELAY_SECONDS", 30)) * time.Second,
		BlockDuration:        getEnv("BLOCK_DURATION", "5m"),
		BlockingExchangeName: getEnv("BLOCKING_EXCHANGE_NAME", "blocking_exchange"),
		MonitoringInterval:   time.Duration(getEnvInt("MONITORING_INTERVAL", 300)) * time.Second,
		DebugEmail:           getEnv("DEBUG_EMAIL", ""),
		DebugIPLimit:         getEnvInt("DEBUG_IP_LIMIT", 1),
		ExcludedUsers:        parseSet(getEnv("EXCLUDED_USERS", "")),
		ExcludedIPs:          parseSet(getEnv("EXCLUDED_IPS", "")),
	}

	log.Printf("Конфигурация загружена. Порт: %s", cfg.Port)
	if len(cfg.ExcludedUsers) > 0 {
		log.Printf("Загружен список исключений: %d пользователей", len(cfg.ExcludedUsers))
	}
	if len(cfg.ExcludedIPs) > 0 {
		log.Printf("Загружен список исключений IP-адресов: %d", len(cfg.ExcludedIPs))
	}
	if cfg.DebugEmail != "" {
		log.Printf("Режим дебага включен для email: %s с лимитом IP: %d", cfg.DebugEmail, cfg.DebugIPLimit)
	}

	return cfg
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func parseSet(value string) map[string]bool {
	set := make(map[string]bool)
	if value == "" {
		return set
	}
	items := strings.Split(value, ",")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			set[item] = true
		}
	}
	return set
}