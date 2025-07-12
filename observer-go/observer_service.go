package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

// Структуры данных
type LogEntry struct {
	UserEmail string `json:"user_email" binding:"required"`
	SourceIP  string `json:"source_ip" binding:"required"`
}

type AlertPayload struct {
	UserIdentifier    string   `json:"user_identifier"`
	DetectedIPsCount  int      `json:"detected_ips_count"`
	Limit             int      `json:"limit"`
	AllUserIPs        []string `json:"all_user_ips"`
	BlockDuration     string   `json:"block_duration"`
	ViolationType     string   `json:"violation_type"`
}

type UserIPStats struct {
	Email           string   `json:"email"`
	IPCount         int      `json:"ip_count"`
	Limit           int      `json:"limit"`
	IPs             []string `json:"ips"`
	IPsWithTTL      []string `json:"ips_with_ttl"`
	MinTTLHours     float64  `json:"min_ttl_hours"`
	MaxTTLHours     float64  `json:"max_ttl_hours"`
	Status          string   `json:"status"`
	HasAlertCooldown bool    `json:"has_alert_cooldown"`
	IsExcluded      bool     `json:"excluded"`
	IsDebug         bool     `json:"is_debug"`
}

type BlockMessage struct {
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// Глобальные переменные
var (
	redisClient     *redis.Client
	httpClient      *http.Client
	rabbitConn      *amqp091.Connection
	blockingChannel *amqp091.Channel
	excludedUsers   map[string]bool
	
	// Конфигурация из переменных окружения
	redisURL           string
	rabbitMQURL        string
	maxIPsPerUser      int
	alertWebhookURL    string
	userIPTTLSeconds   int
	alertCooldownSeconds int
	blockDuration      string
	blockingExchangeName string
	monitoringInterval int
	debugEmail         string
	debugIPLimit       int
	
	// Мьютекс для горутин
	mu sync.RWMutex
)

func init() {
	// Настройка логирования
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	
	// Загрузка конфигурации из переменных окружения
	redisURL = getEnv("REDIS_URL", "redis://localhost:6379/0")
	rabbitMQURL = getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost/")
	maxIPsPerUser = getEnvInt("MAX_IPS_PER_USER", 3)
	alertWebhookURL = getEnv("ALERT_WEBHOOK_URL", "")
	userIPTTLSeconds = getEnvInt("USER_IP_TTL_SECONDS", 24*60*60)
	alertCooldownSeconds = getEnvInt("ALERT_COOLDOWN_SECONDS", 60*60)
	blockDuration = getEnv("BLOCK_DURATION", "5m")
	blockingExchangeName = getEnv("BLOCKING_EXCHANGE_NAME", "blocking_exchange")
	monitoringInterval = getEnvInt("MONITORING_INTERVAL", 300)
	debugEmail = getEnv("DEBUG_EMAIL", "")
	debugIPLimit = getEnvInt("DEBUG_IP_LIMIT", 1)
	
	// Обработка списка исключений
	excludedUsersStr := getEnv("EXCLUDED_USERS", "")
	excludedUsers = make(map[string]bool)
	if excludedUsersStr != "" {
		emails := strings.Split(excludedUsersStr, ",")
		for _, email := range emails {
			email = strings.TrimSpace(email)
			if email != "" {
				excludedUsers[email] = true
			}
		}
	}
	
	if len(excludedUsers) > 0 {
		log.Printf("Загружен список исключений: %d пользователей", len(excludedUsers))
	}
	
	if debugEmail != "" {
		log.Printf("Режим дебага включен для email: %s с лимитом IP: %d", debugEmail, debugIPLimit)
	}
	
	// Создание HTTP клиента
	httpClient = &http.Client{
		Timeout: 15 * time.Second,
	}
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

func getUserIPLimit(userEmail string) int {
	if debugEmail != "" && userEmail == debugEmail {
		return debugIPLimit
	}
	return maxIPsPerUser
}

func connectRedis() error {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("ошибка парсинга Redis URL: %w", err)
	}
	
	redisClient = redis.NewClient(opt)
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ошибка подключения к Redis: %w", err)
	}
	
	log.Println("Успешное подключение к Redis")
	return nil
}

func connectRabbitMQ() error {
	conn, err := amqp091.Dial(rabbitMQURL)
	if err != nil {
		return fmt.Errorf("ошибка подключения к RabbitMQ: %w", err)
	}
	
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("ошибка создания канала RabbitMQ: %w", err)
	}
	
	err = ch.ExchangeDeclare(
		blockingExchangeName, // name
		"fanout",            // type
		true,                // durable
		false,               // auto-deleted
		false,               // internal
		false,               // no-wait
		nil,                 // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("ошибка создания exchange: %w", err)
	}
	
	rabbitConn = conn
	blockingChannel = ch
	
	log.Println("Успешное подключение к RabbitMQ")
	return nil
}

func getUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error) {
	pattern := fmt.Sprintf("user_ip:%s:*", userEmail)
	keys, err := redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}
	
	if len(keys) == 0 {
		return make(map[string]int), nil
	}
	
	activeIPs := make(map[string]int)
	
	for _, key := range keys {
		ttl, err := redisClient.TTL(ctx, key).Result()
		if err != nil {
			continue
		}
		
		if ttl > 0 {
			// Извлекаем IP из ключа: user_ip:email:192.168.1.1 -> 192.168.1.1
			parts := strings.Split(key, ":")
			if len(parts) >= 3 {
				ip := parts[2]
				activeIPs[ip] = int(ttl.Seconds())
			}
		}
	}
	
	return activeIPs, nil
}

func delayedClearUserIPs(userEmail string, delaySeconds int) {
	time.Sleep(time.Duration(delaySeconds) * time.Second)
	
	cleared, err := clearUserIPsAfterBlock(userEmail)
	if err != nil {
		log.Printf("Ошибка при отложенной очистке IP для %s: %v", userEmail, err)
		return
	}
	
	debugMarker := ""
	if debugEmail != "" && userEmail == debugEmail {
		debugMarker = " [DEBUG]"
	}
	
	log.Printf("Отложенная очистка IP для %s%s выполнена через %d секунд. Очищено: %d", 
		userEmail, debugMarker, delaySeconds, cleared)
}

func clearUserIPsAfterBlock(userEmail string) (int, error) {
	ctx := context.Background()
	pattern := fmt.Sprintf("user_ip:%s:*", userEmail)
	
	keys, err := redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return 0, err
	}
	
	if len(keys) == 0 {
		return 0, nil
	}
	
	deleted, err := redisClient.Del(ctx, keys...).Result()
	if err != nil {
		return 0, err
	}
	
	debugMarker := ""
	if debugEmail != "" && userEmail == debugEmail {
		debugMarker = " [DEBUG]"
	}
	
	log.Printf("Очищено %d IP-адресов для заблокированного пользователя %s%s", deleted, userEmail, debugMarker)
	return int(deleted), nil
}

func monitorUserIPPools() {
	ticker := time.NewTicker(time.Duration(monitoringInterval) * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			ctx := context.Background()
			
			// Получаем все ключи пользователей
			keys, err := redisClient.Keys(ctx, "user_ip:*").Result()
			if err != nil {
				log.Printf("Ошибка получения ключей: %v", err)
				continue
			}
			
			if len(keys) == 0 {
				fmt.Printf("[%s] === IP POOLS MONITORING === НЕТ АКТИВНЫХ ПОЛЬЗОВАТЕЛЕЙ\n", 
					time.Now().Format("2006-01-02 15:04:05"))
				continue
			}
			
			fmt.Printf("\n[%s] === IP POOLS MONITORING START ===\n", 
				time.Now().Format("2006-01-02 15:04:05"))
			
			// Группируем ключи по пользователям
			userEmails := make(map[string]bool)
			for _, key := range keys {
				parts := strings.Split(key, ":")
				if len(parts) >= 2 {
					userEmails[parts[1]] = true
				}
			}
			
			var userStats []UserIPStats
			totalUsers := 0
			usersNearLimit := 0
			usersOverLimit := 0
			
			for userEmail := range userEmails {
				activeIPs, err := getUserActiveIPs(ctx, userEmail)
				if err != nil {
					log.Printf("Ошибка при обработке пользователя %s: %v", userEmail, err)
					continue
				}
				
				if len(activeIPs) == 0 {
					continue
				}
				
				totalUsers++
				ipCount := len(activeIPs)
				userLimit := getUserIPLimit(userEmail)
				
				// Определяем статус пользователя
				status := "NORMAL"
				if float64(ipCount) >= float64(userLimit)*0.8 {
					status = "NEAR_LIMIT"
					usersNearLimit++
				}
				if ipCount > userLimit {
					status = "OVER_LIMIT"
					usersOverLimit++
				}
				
				// Проверяем кулдаун на алерты
				alertCooldownKey := fmt.Sprintf("alert_sent:%s", userEmail)
				hasAlertCooldown, _ := redisClient.Exists(ctx, alertCooldownKey).Result()
				
				// Подготавливаем данные об IP с TTL
				var ips []string
				var ipsWithTTL []string
				var ttlValues []int
				
				for ip, ttl := range activeIPs {
					ips = append(ips, ip)
					ttlHours := float64(ttl) / 3600
					ipsWithTTL = append(ipsWithTTL, fmt.Sprintf("%s(%.1fh)", ip, ttlHours))
					ttlValues = append(ttlValues, ttl)
				}
				
				sort.Strings(ips)
				sort.Strings(ipsWithTTL)
				
				var minTTL, maxTTL float64
				if len(ttlValues) > 0 {
					minTTL = float64(ttlValues[0]) / 3600
					maxTTL = float64(ttlValues[0]) / 3600
					for _, ttl := range ttlValues {
						ttlHours := float64(ttl) / 3600
						if ttlHours < minTTL {
							minTTL = ttlHours
						}
						if ttlHours > maxTTL {
							maxTTL = ttlHours
						}
					}
				}
				
				userStats = append(userStats, UserIPStats{
					Email:           userEmail,
					IPCount:         ipCount,
					Limit:           userLimit,
					IPs:             ips,
					IPsWithTTL:      ipsWithTTL,
					MinTTLHours:     minTTL,
					MaxTTLHours:     maxTTL,
					Status:          status,
					HasAlertCooldown: hasAlertCooldown > 0,
					IsExcluded:      excludedUsers[userEmail],
					IsDebug:         debugEmail != "" && userEmail == debugEmail,
				})
			}
			
			// Сортируем по количеству IP (по убыванию)
			sort.Slice(userStats, func(i, j int) bool {
				return userStats[i].IPCount > userStats[j].IPCount
			})
			
			// Выводим общую статистику
			fmt.Println("📊 ОБЩАЯ СТАТИСТИКА:")
			fmt.Printf("   👥 Всего активных пользователей: %d\n", totalUsers)
			fmt.Printf("   ⚠️  Близко к лимиту: %d\n", usersNearLimit)
			fmt.Printf("   🚨 Превышение лимита: %d\n", usersOverLimit)
			
			excludedCount := 0
			debugCount := 0
			for _, user := range userStats {
				if user.IsExcluded {
					excludedCount++
				}
				if user.IsDebug {
					debugCount++
				}
			}
			fmt.Printf("   🛡️  Исключенных пользователей: %d\n", excludedCount)
			if debugEmail != "" {
				fmt.Printf("   🐛 Debug пользователей: %d\n", debugCount)
			}
			
			// Выводим топ-10 пользователей
			fmt.Println("\n📈 ТОП ПОЛЬЗОВАТЕЛИ ПО КОЛИЧЕСТВУ IP:")
			for i, user := range userStats {
				if i >= 10 {
					break
				}
				
				statusEmoji := "❓"
				switch user.Status {
				case "NORMAL":
					statusEmoji = "✅"
				case "NEAR_LIMIT":
					statusEmoji = "⚠️"
				case "OVER_LIMIT":
					statusEmoji = "🚨"
				}
				
				excludedMarker := ""
				if user.IsExcluded {
					excludedMarker = " [EXCLUDED]"
				}
				
				cooldownMarker := ""
				if user.HasAlertCooldown {
					cooldownMarker = " [ALERT_COOLDOWN]"
				}
				
				debugMarker := ""
				if user.IsDebug {
					debugMarker = " [DEBUG]"
				}
				
				fmt.Printf("   %2d. %s %s%s%s%s\n", i+1, statusEmoji, user.Email, 
					excludedMarker, cooldownMarker, debugMarker)
				fmt.Printf("       IP: %d/%d | TTL: %.1f-%.1fh\n", 
					user.IPCount, user.Limit, user.MinTTLHours, user.MaxTTLHours)
				fmt.Printf("       IPs: %s\n", strings.Join(user.IPsWithTTL, ", "))
			}
			
			// Отдельно выводим всех пользователей с превышением лимита
			var overLimitUsers []UserIPStats
			for _, user := range userStats {
				if user.Status == "OVER_LIMIT" {
					overLimitUsers = append(overLimitUsers, user)
				}
			}
			
			if len(overLimitUsers) > 0 {
				fmt.Println("\n🚨 ПОЛЬЗОВАТЕЛИ С ПРЕВЫШЕНИЕМ ЛИМИТА:")
				for _, user := range overLimitUsers {
					excludedMarker := ""
					if user.IsExcluded {
						excludedMarker = " [EXCLUDED - НЕ БЛОКИРУЕТСЯ]"
					}
					
					cooldownMarker := ""
					if user.HasAlertCooldown {
						cooldownMarker = " [ALERT_COOLDOWN]"
					}
					
					debugMarker := ""
					if user.IsDebug {
						debugMarker = " [DEBUG]"
					}
					
					fmt.Printf("   • %s%s%s%s\n", user.Email, excludedMarker, cooldownMarker, debugMarker)
					fmt.Printf("     IP: %d/%d | TTL: %.1f-%.1fh\n", 
						user.IPCount, user.Limit, user.MinTTLHours, user.MaxTTLHours)
					fmt.Printf("     IPs: %s\n", strings.Join(user.IPsWithTTL, ", "))
				}
			}
			
			fmt.Printf("[%s] === IP POOLS MONITORING END ===\n\n", 
				time.Now().Format("2006-01-02 15:04:05"))
		}
	}
}

func publishBlockMessage(ips []string) error {
	if blockingChannel == nil {
		return fmt.Errorf("RabbitMQ канал не инициализирован")
	}
	
	blockMsg := BlockMessage{
		IPs:      ips,
		Duration: blockDuration,
	}
	
	body, err := json.Marshal(blockMsg)
	if err != nil {
		return fmt.Errorf("ошибка сериализации сообщения: %w", err)
	}
	
	err = blockingChannel.Publish(
		blockingExchangeName, // exchange
		"",                   // routing key
		false,                // mandatory
		false,                // immediate
		amqp091.Publishing{
			ContentType:  "application/json",
			Body:         body,
			DeliveryMode: amqp091.Persistent,
		},
	)
	
	return err
}

func sendAlert(payload AlertPayload) error {
	if alertWebhookURL == "" {
		log.Println("ALERT_WEBHOOK_URL не задан, вебхук не отправляется")
		return nil
	}
	
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка сериализации payload: %w", err)
	}
	
	log.Printf("Попытка отправить вебхук на URL: %s", alertWebhookURL)
	
	resp, err := httpClient.Post(alertWebhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("сетевая ошибка при отправке вебхука: %w", err)
	}
	defer resp.Body.Close()
	
	// Читаем тело ответа
	var respBody bytes.Buffer
	respBody.ReadFrom(resp.Body)
	
	log.Printf("Вебхук-уведомление для %s отправлен. Статус ответа: %d. Тело ответа: %s", 
		payload.UserIdentifier, resp.StatusCode, respBody.String())
	
	return nil
}

func processLogEntries(c *gin.Context) {
	var entries []LogEntry
	if err := c.ShouldBindJSON(&entries); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	
	ctx := context.Background()
	
	for _, entry := range entries {
		// Проверка, находится ли пользователь в списке исключений
		if excludedUsers[entry.UserEmail] {
			continue
		}
		
		// Получаем лимит IP для конкретного пользователя
		userIPLimit := getUserIPLimit(entry.UserEmail)
		
		// Новая схема: отдельный ключ для каждого IP
		userIPKey := fmt.Sprintf("user_ip:%s:%s", entry.UserEmail, entry.SourceIP)
		alertSentKey := fmt.Sprintf("alert_sent:%s", entry.UserEmail)
		
		// Проверяем, существует ли уже такой IP у пользователя
		ipExists, _ := redisClient.Exists(ctx, userIPKey).Result()
		
		// Устанавливаем/обновляем TTL для конкретного IP
		err := redisClient.SetEx(ctx, userIPKey, time.Now().Format(time.RFC3339), 
			time.Duration(userIPTTLSeconds)*time.Second).Err()
		if err != nil {
			log.Printf("Ошибка установки TTL для IP %s пользователя %s: %v", 
				entry.SourceIP, entry.UserEmail, err)
			continue
		}
		
		// Получаем все активные IP пользователя
		activeIPs, err := getUserActiveIPs(ctx, entry.UserEmail)
		if err != nil {
			log.Printf("Ошибка получения активных IP для %s: %v", entry.UserEmail, err)
			continue
		}
		
		currentIPCount := len(activeIPs)
		
		// Проверяем кулдаун на алерты
		alertWasSent, _ := redisClient.Exists(ctx, alertSentKey).Result()
		
		// Логируем с учетом дебага
		if ipExists == 0 {
			debugMarker := ""
			if debugEmail != "" && entry.UserEmail == debugEmail {
				debugMarker = " [DEBUG]"
			}
			log.Printf("Новый IP для пользователя %s%s: %s. Всего IP: %d/%d", 
				entry.UserEmail, debugMarker, entry.SourceIP, currentIPCount, userIPLimit)
		}
		
		// Проверка на превышение лимита и отсутствие кулдауна
		if currentIPCount > userIPLimit && alertWasSent == 0 {
			debugMarker := ""
			if debugEmail != "" && entry.UserEmail == debugEmail {
				debugMarker = " [DEBUG]"
			}
			log.Printf("ПРЕВЫШЕНИЕ ЛИМИТА%s: Пользователь %s, IP-адресов: %d/%d", 
				debugMarker, entry.UserEmail, currentIPCount, userIPLimit)
			
			// Получаем список всех активных IP
			var allUserIPs []string
			for ip := range activeIPs {
				allUserIPs = append(allUserIPs, ip)
			}
			
			// Отправка команды на блокировку через RabbitMQ
			if blockingChannel != nil && len(allUserIPs) > 0 {
				err := publishBlockMessage(allUserIPs)
				if err != nil {
					log.Printf("Ошибка отправки сообщения о блокировке: %v", err)
				} else {
					log.Printf("Сообщение о блокировке для %s%s отправлено", entry.UserEmail, debugMarker)
					
					// ОТЛОЖЕННАЯ ОЧИСТКА IP-АДРЕСОВ ПОЛЬЗОВАТЕЛЯ
					go delayedClearUserIPs(entry.UserEmail, 30)
				}
			}
			
			// Установка кулдауна на алерты
			err = redisClient.SetEx(ctx, alertSentKey, "1", 
				time.Duration(alertCooldownSeconds)*time.Second).Err()
			if err != nil {
				log.Printf("Ошибка установки кулдауна алертов: %v", err)
			}
			
			// Отправка уведомления
			alertPayload := AlertPayload{
				UserIdentifier:   entry.UserEmail,
				DetectedIPsCount: currentIPCount,
				Limit:            userIPLimit,
				AllUserIPs:       allUserIPs,
				BlockDuration:    blockDuration,
				ViolationType:    "ip_limit_exceeded",
			}
			
			go func() {
				if err := sendAlert(alertPayload); err != nil {
					log.Printf("Ошибка отправки вебхук-уведомления: %v", err)
				}
			}()
		}
	}
	
	c.JSON(http.StatusOK, gin.H{
		"status":            "ok",
		"processed_entries": len(entries),
	})
}

func healthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	err := redisClient.Ping(ctx).Err()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":           "error",
			"redis_connection": "failed",
			"error":            err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"redis_connection": "ok",
	})
}

func main() {
	// Подключение к Redis
	if err := connectRedis(); err != nil {
		log.Fatalf("Ошибка подключения к Redis: %v", err)
	}
	defer redisClient.Close()
	
	// Подключение к RabbitMQ
	if err := connectRabbitMQ(); err != nil {
		log.Printf("Предупреждение: Не удалось подключиться к RabbitMQ: %v", err)
	} else {
		defer rabbitConn.Close()
		defer blockingChannel.Close()
	}
	
	// Запуск мониторинга IP-пулов
	go monitorUserIPPools()
	log.Printf("Мониторинг IP-пулов запущен с интервалом %d секунд", monitoringInterval)
	
	// Настройка Gin
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()
	
	// Middleware для логирования
	router.Use(gin.Logger())
	router.Use(gin.Recovery())
	
	// Эндпоинты
	router.POST("/log-entry", processLogEntries)
	router.GET("/health", healthCheck)
	
	// Запуск сервера
	port := getEnv("PORT", "9000")
	log.Printf("Сервер Observer Service запущен на порту %s", port)
	
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("Ошибка запуска сервера: %v", err)
	}
}