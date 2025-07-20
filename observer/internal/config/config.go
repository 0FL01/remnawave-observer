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
	Port                        string
	RedisURL                    string
	RabbitMQURL                 string
	MaxIPsPerUser               int
	AlertWebhookURL             string
	UserIPTTL                   time.Duration
	AlertCooldown               time.Duration
	ClearIPsDelay               time.Duration
	BlockDuration               string
	BlockingExchangeName        string
	MonitoringInterval          time.Duration
	DebugEmail                  string
	DebugIPLimit                int
	ExcludedUsers               map[string]bool
	ExcludedIPs                 map[string]bool
	WorkerPoolSize              int
	LogChannelBufferSize        int
	SideEffectWorkerPoolSize    int
	SideEffectChannelBufferSize int

	// ПАРАМЕТРЫ ДЛЯ РЕЖИМА ПОДСЕТЕЙ ---
	DetectBySubnet    bool          // Включить режим детекции по подсетям
	MaxSubnetsPerUser int           // Лимит подсетей на пользователя
	UserSubnetTTL     time.Duration // TTL для подсети пользователя
	SubnetMaskIPv4    int           // Маска для IPv4 подсетей (например, 24 для /24)
}

// New загружает конфигурацию из переменных окружения.
func New() *Config {
	cfg := &Config{
		Port:                        getEnv("PORT", "9000"),
		RedisURL:                    getEnv("REDIS_URL", "redis://localhost:6379/0"),
		RabbitMQURL:                 getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost/"),
		MaxIPsPerUser:               getEnvInt("MAX_IPS_PER_USER", 3),
		AlertWebhookURL:             getEnv("ALERT_WEBHOOK_URL", ""),
		UserIPTTL:                   time.Duration(getEnvInt("USER_IP_TTL_SECONDS", 24*60*60)) * time.Second,
		AlertCooldown:               time.Duration(getEnvInt("ALERT_COOLDOWN_SECONDS", 60*60)) * time.Second,
		ClearIPsDelay:               time.Duration(getEnvInt("CLEAR_IPS_DELAY_SECONDS", 30)) * time.Second,
		BlockDuration:               getEnv("BLOCK_DURATION", "5m"),
		BlockingExchangeName:        getEnv("BLOCKING_EXCHANGE_NAME", "blocking_exchange"),
		MonitoringInterval:          time.Duration(getEnvInt("MONITORING_INTERVAL", 300)) * time.Second,
		DebugEmail:                  getEnv("DEBUG_EMAIL", ""),
		DebugIPLimit:                getEnvInt("DEBUG_IP_LIMIT", 1),
		ExcludedUsers:               parseSet(getEnv("EXCLUDED_USERS", "")),
		ExcludedIPs:                 parseSet(getEnv("EXCLUDED_IPS", "")),
		WorkerPoolSize:              getEnvInt("WORKER_POOL_SIZE", 20),
		LogChannelBufferSize:        getEnvInt("LOG_CHANNEL_BUFFER_SIZE", 100),
		SideEffectWorkerPoolSize:    getEnvInt("SIDE_EFFECT_WORKER_POOL_SIZE", 10),
		SideEffectChannelBufferSize: getEnvInt("SIDE_EFFECT_CHANNEL_BUFFER_SIZE", 50),

		// --- Загрузка новых параметров ---
		DetectBySubnet:    getEnvBool("DETECT_BY_SUBNET", false),
		MaxSubnetsPerUser: getEnvInt("MAX_SUBNETS_PER_USER", 3),
		UserSubnetTTL:     time.Duration(getEnvInt("USER_SUBNET_TTL_SECONDS", 86400)) * time.Second,
		SubnetMaskIPv4:    getEnvInt("SUBNET_MASK_IPV4", 24),
	}

	log.Printf("Конфигурация загружена. Порт: %s", cfg.Port)
	if cfg.DetectBySubnet {
		log.Printf("!!! РЕЖИМ ОБНАРУЖЕНИЯ: по ПОДСЕТЯМ (/%d). Лимит: %d подсетей на пользователя.", cfg.SubnetMaskIPv4, cfg.MaxSubnetsPerUser)
	} else {
		log.Printf("!!! РЕЖИМ ОБНАРУЖЕНИЯ: по IP-адресам. Лимит: %d IP на пользователя.", cfg.MaxIPsPerUser)
	}
	log.Printf("Пул воркеров обработки логов: %d воркеров, размер буфера канала: %d", cfg.WorkerPoolSize, cfg.LogChannelBufferSize)
	log.Printf("Пул воркеров побочных задач (алерты, очистка): %d воркеров, размер буфера канала: %d", cfg.SideEffectWorkerPoolSize, cfg.SideEffectChannelBufferSize)
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

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
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