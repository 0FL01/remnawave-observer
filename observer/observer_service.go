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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const addAndCheckIPScript = `
-- KEYS[1]: ключ множества IP пользователя (например, user_ips:email@example.com)
-- KEYS[2]: ключ кулдауна алертов для пользователя (например, alert_sent:email@example.com)
-- ARGV[1]: IP-адрес для добавления
-- ARGV[2]: TTL для IP-адреса в секундах
-- ARGV[3]: Лимит IP-адресов для пользователя
-- ARGV[4]: TTL для кулдауна алертов в секундах

-- Добавляем IP в множество. Если он уже там, команда ничего не сделает, но вернет 0.
local isNewIp = redis.call('SADD', KEYS[1], ARGV[1])

-- Устанавливаем/обновляем TTL для конкретного IP-адреса.
-- Ключ для TTL формируется из ключа множества и самого IP.
local ipTtlKey = 'ip_ttl:' .. string.sub(KEYS[1], 10) .. ':' .. ARGV[1]
redis.call('SETEX', ipTtlKey, ARGV[2], '1')

-- Получаем текущее количество IP в множестве. Это быстрая операция O(1).
local currentIpCount = redis.call('SCARD', KEYS[1])
local ipLimit = tonumber(ARGV[3])

-- Проверяем, превышен ли лимит
if currentIpCount > ipLimit then
    -- Лимит превышен. Проверяем, был ли уже отправлен алерт (существует ли ключ кулдауна).
    local alertSent = redis.call('EXISTS', KEYS[2])
    if alertSent == 0 then
        -- Кулдауна нет. Устанавливаем его и возвращаем сигнал на блокировку.
        redis.call('SETEX', KEYS[2], ARGV[4], '1')
        -- Получаем все IP пользователя для отправки в сообщении о блокировке.
        local allIps = redis.call('SMEMBERS', KEYS[1])
        -- Возвращаем статус 1 (блокировать) и список IP
        return {1, allIps}
    else
        -- Кулдаун уже есть. Ничего не делаем.
        -- Возвращаем статус 2 (лимит превышен, но алерт на кулдауне)
        return {2, currentIpCount}
    end
end

-- Лимит не превышен. Возвращаем статус 0 (все в порядке) и текущее количество IP.
return {0, currentIpCount, isNewIp}
`

// Структуры данных
type LogEntry struct {
	UserEmail string `json:"user_email" binding:"required"`
	SourceIP  string `json:"source_ip" binding:"required"`
}

type AlertPayload struct {
	UserIdentifier   string   `json:"user_identifier"`
	DetectedIPsCount int      `json:"detected_ips_count"`
	Limit            int      `json:"limit"`
	AllUserIPs       []string `json:"all_user_ips"`
	BlockDuration    string   `json:"block_duration"`
	ViolationType    string   `json:"violation_type"`
}

type UserIPStats struct {
	Email            string   `json:"email"`
	IPCount          int      `json:"ip_count"`
	Limit            int      `json:"limit"`
	IPs              []string `json:"ips"`
	IPsWithTTL       []string `json:"ips_with_ttl"`
	MinTTLHours      float64  `json:"min_ttl_hours"`
	MaxTTLHours      float64  `json:"max_ttl_hours"`
	Status           string   `json:"status"`
	HasAlertCooldown bool     `json:"has_alert_cooldown"`
	IsExcluded       bool     `json:"excluded"`
	IsDebug          bool     `json:"is_debug"`
}

type BlockMessage struct {
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// Глобальные переменные
var (
	redisClient          *redis.Client
	httpClient           *http.Client
	rabbitConn           *amqp091.Connection
	blockingChannel      *amqp091.Channel
	excludedUsers        map[string]bool
	excludedIPs          map[string]bool
	// Конфигурация из переменных окружения
	redisURL             string
	rabbitMQURL          string
	maxIPsPerUser        int
	alertWebhookURL      string
	userIPTTLSeconds     int
	clearIPsDelaySeconds int
	alertCooldownSeconds int
	blockDuration        string
	blockingExchangeName string
	monitoringInterval   int
	debugEmail           string
	debugIPLimit         int
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
	clearIPsDelaySeconds = getEnvInt("CLEAR_IPS_DELAY_SECONDS", 30)
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
	// Обработка списка исключенных IP-адресов
	excludedIPsStr := getEnv("EXCLUDED_IPS", "")
	excludedIPs = make(map[string]bool)
	if excludedIPsStr != "" {
		ips := strings.Split(excludedIPsStr, ",")
		for _, ip := range ips {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				excludedIPs[ip] = true
			}
		}
	}
	if len(excludedUsers) > 0 {
		log.Printf("Загружен список исключений: %d пользователей", len(excludedUsers))
	}
	if len(excludedIPs) > 0 {
		log.Printf("Загружен список исключений IP-адресов: %d", len(excludedIPs))
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
		"fanout",             // type
		true,                 // durable
		false,                // auto-deleted
		false,                // internal
		false,                // no-wait
		nil,                  // arguments
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

// connectRabbitMQWithRetry пытается подключиться к RabbitMQ с указанным количеством
// повторных попыток и задержкой между ними.
func connectRabbitMQWithRetry(maxRetries int, delay time.Duration) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = connectRabbitMQ()
		if err == nil {
			return nil
		}
		log.Printf("Не удалось подключиться к RabbitMQ (попытка %d/%d): %v. Повтор через %v...", i+1, maxRetries, err, delay)
		time.Sleep(delay)
	}
	return fmt.Errorf("не удалось подключиться к RabbitMQ после %d попыток: %w", maxRetries, err)
}

// Новая версия getUserActiveIPs, работающая с SET
func getUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error) {
	// Комментарий: Эта функция теперь используется только для мониторинга.
	userIpsKey := fmt.Sprintf("user_ips:%s", userEmail)
	// Получаем все IP из множества. Это быстрая операция.
	ips, err := redisClient.SMembers(ctx, userIpsKey).Result()
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return make(map[string]int), nil
	}
	activeIPs := make(map[string]int)
	// Для каждого IP получаем его индивидуальный TTL.
	// Можно оптимизировать, используя пайплайн (pipeline) для уменьшения сетевых задержек.
	for _, ip := range ips {
		ipTtlKey := fmt.Sprintf("ip_ttl:%s:%s", userEmail, ip)
		ttl, err := redisClient.TTL(ctx, ipTtlKey).Result()
		if err != nil {
			// IP мог только что истечь, это нормально
			continue
		}
		if ttl > 0 {
			activeIPs[ip] = int(ttl.Seconds())
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

// Новая версия clearUserIPsAfterBlock
func clearUserIPsAfterBlock(userEmail string) (int, error) {
	ctx := context.Background()
	userIpsKey := fmt.Sprintf("user_ips:%s", userEmail)
	// Получаем все IP, которые нужно будет удалить
	ips, err := redisClient.SMembers(ctx, userIpsKey).Result()
	if err != nil {
		return 0, err
	}
	if len(ips) == 0 {
		return 0, nil
	}
	// Собираем все ключи для удаления: ключ самого множества и ключи TTL для каждого IP
	keysToDelete := make([]string, 0, len(ips)+1)
	keysToDelete = append(keysToDelete, userIpsKey)
	for _, ip := range ips {
		ipTtlKey := fmt.Sprintf("ip_ttl:%s:%s", userEmail, ip)
		keysToDelete = append(keysToDelete, ipTtlKey)
	}
	// Удаляем все ключи одной командой
	deleted, err := redisClient.Del(ctx, keysToDelete...).Result()
	if err != nil {
		return 0, err
	}
	debugMarker := ""
	if debugEmail != "" && userEmail == debugEmail {
		debugMarker = " [DEBUG]"
	}
	log.Printf("Очищено %d ключей (1 set + %d ip_ttl) для заблокированного пользователя %s%s", deleted, len(ips), userEmail, debugMarker)
	return int(deleted), nil
}

// Новая версия monitorUserIPPools с использованием SCAN
func monitorUserIPPools() {
	ticker := time.NewTicker(time.Duration(monitoringInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx := context.Background()
			// Используем SCAN для безопасной итерации по ключам без блокировки Redis
			var cursor uint64
			userEmails := make(map[string]bool)
			for {
				var keys []string
				var err error
				keys, cursor, err = redisClient.Scan(ctx, cursor, "user_ips:*", 500).Result() // Сканируем по 500 ключей за раз
				if err != nil {
					log.Printf("Ошибка при сканировании ключей (SCAN): %v", err)
					break
				}
				for _, key := range keys {
					// Извлекаем email из ключа 'user_ips:email@example.com'
					parts := strings.SplitN(key, ":", 2)
					if len(parts) == 2 {
						userEmails[parts[1]] = true
					}
				}
				if cursor == 0 { // Конец итерации
					break
				}
			}
			if len(userEmails) == 0 {
				fmt.Printf("[%s] === IP POOLS MONITORING === НЕТ АКТИВНЫХ ПОЛЬЗОВАТЕЛЕЙ\n",
					time.Now().Format("2006-01-02 15:04:05"))
				continue
			}
			// --- Дальнейшая логика мониторинга остается почти без изменений ---
			fmt.Printf("\n[%s] === IP POOLS MONITORING START ===\n",
				time.Now().Format("2006-01-02 15:04:05"))
			var userStats []UserIPStats
			totalUsers := 0
			usersNearLimit := 0
			usersOverLimit := 0
			for userEmail := range userEmails {
				// Используем уже обновленную функцию getUserActiveIPs
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
				// ... остальная часть функции monitorUserIPPools остается такой же, как была ...
				// ... она уже использует getUserActiveIPs, которая теперь работает правильно ...
				// (Код для краткости опущен, он идентичен старому)
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
					Email:            userEmail,
					IPCount:          ipCount,
					Limit:            userLimit,
					IPs:              ips,
					IPsWithTTL:       ipsWithTTL,
					MinTTLHours:      minTTL,
					MaxTTLHours:      maxTTL,
					Status:           status,
					HasAlertCooldown: hasAlertCooldown > 0,
					IsExcluded:       excludedUsers[userEmail],
					IsDebug:          debugEmail != "" && userEmail == debugEmail,
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
		// Определяем ключи для Redis
		userIPsSetKey := fmt.Sprintf("user_ips:%s", entry.UserEmail)
		alertSentKey := fmt.Sprintf("alert_sent:%s", entry.UserEmail)
		// Выполняем Lua-скрипт. Redis гарантирует его атомарное выполнение.
		result, err := redisClient.Eval(ctx, addAndCheckIPScript,
			[]string{userIPsSetKey, alertSentKey},
			entry.SourceIP,
			userIPTTLSeconds,
			userIPLimit,
			alertCooldownSeconds,
		).Result()
		if err != nil {
			log.Printf("Ошибка выполнения Lua-скрипта для %s: %v", entry.UserEmail, err)
			continue
		}
		// Разбираем результат работы скрипта
		resSlice, ok := result.([]interface{})
		if !ok || len(resSlice) < 1 {
			log.Printf("Неожиданный результат от Lua-скрипта для %s", entry.UserEmail)
			continue
		}
		statusCode, _ := resSlice[0].(int64)
		// Логируем добавление нового IP
		if statusCode == 0 {
			isNewIp, _ := resSlice[2].(int64)
			if isNewIp == 1 {
				currentIpCount, _ := resSlice[1].(int64)
				debugMarker := ""
				if debugEmail != "" && entry.UserEmail == debugEmail {
					debugMarker = " [DEBUG]"
				}
				log.Printf("Новый IP для пользователя %s%s: %s. Всего IP: %d/%d",
					entry.UserEmail, debugMarker, entry.SourceIP, currentIpCount, userIPLimit)
			}
		}
		// Статус 1 означает, что лимит превышен и нужно блокировать
		if statusCode == 1 {
			debugMarker := ""
			if debugEmail != "" && entry.UserEmail == debugEmail {
				debugMarker = " [DEBUG]"
			}
			// Получаем список IP из ответа скрипта
			ipInterfaces, _ := resSlice[1].([]interface{})
			var allUserIPs []string
			for _, ipInt := range ipInterfaces {
				if ipStr, ok := ipInt.(string); ok {
					allUserIPs = append(allUserIPs, ipStr)
				}
			}
			currentIPCount := len(allUserIPs)
			log.Printf("ПРЕВЫШЕНИЕ ЛИМИТА%s: Пользователь %s, IP-адресов: %d/%d",
				debugMarker, entry.UserEmail, currentIPCount, userIPLimit)
			// Фильтруем IP-адреса, исключая те, что находятся в списке исключений
			var ipsToBlock []string
			for _, ip := range allUserIPs {
				if excludedIPs[ip] {
					log.Printf("IP-адрес %s для пользователя %s пропущен, так как находится в списке исключений.", ip, entry.UserEmail)
					continue
				}
				ipsToBlock = append(ipsToBlock, ip)
			}
			// Отправка команды на блокировку через RabbitMQ
			if blockingChannel != nil && len(ipsToBlock) > 0 {
				if err := publishBlockMessage(ipsToBlock); err != nil {
					log.Printf("Ошибка отправки сообщения о блокировке: %v", err)
				} else {
					log.Printf("Сообщение о блокировке %d IP-адресов для %s%s отправлено", len(ipsToBlock), entry.UserEmail, debugMarker)
					go delayedClearUserIPs(entry.UserEmail, clearIPsDelaySeconds)
				}
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
	// Подключение к RabbitMQ с логикой ретраев
	const maxRabbitRetries = 10
	const rabbitRetryDelay = 5 * time.Second
	if err := connectRabbitMQWithRetry(maxRabbitRetries, rabbitRetryDelay); err != nil {
		log.Fatalf("Критическая ошибка: не удалось подключиться к RabbitMQ: %v", err)
	}
	defer rabbitConn.Close()
	defer blockingChannel.Close()
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