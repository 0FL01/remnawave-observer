package monitor

import (
	"context"
	"fmt"
	"log"
	"math"
	"observer_service/internal/config"
	"observer_service/internal/models"
	"observer_service/internal/services/storage"
	"sort"
	"strings"
	"sync"
	"time"
)

// PoolMonitor выполняет периодический мониторинг пулов IP.
type PoolMonitor struct {
	storage storage.IPStorage
	cfg     *config.Config
}

// NewPoolMonitor создает новый экземпляр PoolMonitor.
func NewPoolMonitor(s storage.IPStorage, cfg *config.Config) *PoolMonitor {
	return &PoolMonitor{
		storage: s,
		cfg:     cfg,
	}
}

// Run запускает бесконечный цикл мониторинга.
func (m *PoolMonitor) Run(ctx context.Context, wg *sync.WaitGroup) {
	// Гарантируем вызов Done() при выходе из функции
	defer wg.Done()

	log.Printf("Мониторинг IP-пулов запущен с интервалом %v", m.cfg.MonitoringInterval)
	ticker := time.NewTicker(m.cfg.MonitoringInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.performMonitoring(context.Background())
		case <-ctx.Done():
			log.Println("Остановка мониторинга IP-пулов.")
			return
		}
	}
}

func (m *PoolMonitor) performMonitoring(ctx context.Context) {
	userEmails, err := m.storage.GetAllUserEmails(ctx)
	if err != nil {
		log.Printf("Ошибка мониторинга (GetAllUserEmails): %v", err)
		return
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	if len(userEmails) == 0 {
		fmt.Printf("[%s] === IP POOLS MONITORING === НЕТ АКТИВНЫХ ПОЛЬЗОВАТЕЛЕЙ\n", now)
		return
	}

	fmt.Printf("\n[%s] === IP POOLS MONITORING START ===\n", now)
	defer fmt.Printf("[%s] === IP POOLS MONITORING END ===\n\n", time.Now().Format("2006-01-02 15:04:05"))

	var allStats []models.UserIPStats
	for _, email := range userEmails {
		stats, err := m.buildUserStats(ctx, email)
		if err != nil {
			log.Printf("Ошибка при сборе статистики для %s: %v", email, err)
			continue
		}
		if stats != nil {
			allStats = append(allStats, *stats)
		}
	}

	sort.Slice(allStats, func(i, j int) bool {
		return allStats[i].IPCount > allStats[j].IPCount
	})

	m.printSummary(allStats)
	m.printTopUsers(allStats)
	m.printOverLimitUsers(allStats)
}

func (m *PoolMonitor) buildUserStats(ctx context.Context, email string) (*models.UserIPStats, error) {
	activeIPs, err := m.storage.GetUserActiveIPs(ctx, email)
	if err != nil {
		return nil, err
	}
	if len(activeIPs) == 0 {
		return nil, nil
	}

	userLimit := m.getUserIPLimit(email)
	ipCount := len(activeIPs)
	status := "NORMAL"
	if float64(ipCount) >= float64(userLimit)*0.8 {
		status = "NEAR_LIMIT"
	}
	if ipCount > userLimit {
		status = "OVER_LIMIT"
	}

	hasCooldown, _ := m.storage.HasAlertCooldown(ctx, email)

	var ips, ipsWithTTL []string
	var ttlValues []int
	for ip, ttl := range activeIPs {
		ips = append(ips, ip)
		ipsWithTTL = append(ipsWithTTL, fmt.Sprintf("%s(%.1fh)", ip, float64(ttl)/3600.0))
		ttlValues = append(ttlValues, ttl)
	}
	sort.Strings(ips)
	sort.Strings(ipsWithTTL)

	minTTL, maxTTL := 0.0, 0.0
	if len(ttlValues) > 0 {
		sort.Ints(ttlValues)
		minTTL = float64(ttlValues[0]) / 3600.0
		maxTTL = float64(ttlValues[len(ttlValues)-1]) / 3600.0
	}

	return &models.UserIPStats{
		Email:            email,
		IPCount:          ipCount,
		Limit:            userLimit,
		IPs:              ips,
		IPsWithTTL:       ipsWithTTL,
		MinTTLHours:      math.Round(minTTL*10) / 10,
		MaxTTLHours:      math.Round(maxTTL*10) / 10,
		Status:           status,
		HasAlertCooldown: hasCooldown,
		IsExcluded:       m.cfg.ExcludedUsers[email],
		IsDebug:          m.cfg.DebugEmail != "" && email == m.cfg.DebugEmail,
	}, nil
}

func (m *PoolMonitor) printSummary(stats []models.UserIPStats) {
	var total, nearLimit, overLimit, excluded, debug int
	total = len(stats)
	for _, s := range stats {
		if s.Status == "NEAR_LIMIT" {
			nearLimit++
		}
		if s.Status == "OVER_LIMIT" {
			overLimit++
		}
		if s.IsExcluded {
			excluded++
		}
		if s.IsDebug {
			debug++
		}
	}
	fmt.Println("📊 ОБЩАЯ СТАТИСТИКА:")
	fmt.Printf("   👥 Всего активных пользователей: %d\n", total)
	fmt.Printf("   ⚠️  Близко к лимиту: %d\n", nearLimit)
	fmt.Printf("   🚨 Превышение лимита: %d\n", overLimit)
	fmt.Printf("   🛡️  Исключенных пользователей: %d\n", excluded)
	if m.cfg.DebugEmail != "" {
		fmt.Printf("   🐛 Debug пользователей: %d\n", debug)
	}
}

func (m *PoolMonitor) printTopUsers(stats []models.UserIPStats) {
	fmt.Println("\n📈 ТОП ПОЛЬЗОВАТЕЛИ ПО КОЛИЧЕСТВУ IP:")
	limit := 10
	if len(stats) < limit {
		limit = len(stats)
	}
	for i := 0; i < limit; i++ {
		user := stats[i]
		fmt.Printf("   %2d. %s %s%s\n", i+1, getStatusEmoji(user.Status), user.Email, getMarkers(user))
		fmt.Printf("       IP: %d/%d | TTL: %.1f-%.1fh\n", user.IPCount, user.Limit, user.MinTTLHours, user.MaxTTLHours)
		fmt.Printf("       IPs: %s\n", strings.Join(user.IPsWithTTL, ", "))
	}
}

func (m *PoolMonitor) printOverLimitUsers(stats []models.UserIPStats) {
	var overLimitUsers []models.UserIPStats
	for _, user := range stats {
		if user.Status == "OVER_LIMIT" {
			overLimitUsers = append(overLimitUsers, user)
		}
	}
	if len(overLimitUsers) > 0 {
		fmt.Println("\n🚨 ПОЛЬЗОВАТЕЛИ С ПРЕВЫШЕНИЕМ ЛИМИТА:")
		for _, user := range overLimitUsers {
			fmt.Printf("   • %s%s\n", user.Email, getMarkers(user))
			fmt.Printf("     IP: %d/%d | TTL: %.1f-%.1fh\n", user.IPCount, user.Limit, user.MinTTLHours, user.MaxTTLHours)
			fmt.Printf("     IPs: %s\n", strings.Join(user.IPsWithTTL, ", "))
		}
	}
}

func (m *PoolMonitor) getUserIPLimit(userEmail string) int {
	if m.cfg.DebugEmail != "" && userEmail == m.cfg.DebugEmail {
		return m.cfg.DebugIPLimit
	}
	return m.cfg.MaxIPsPerUser
}

func getStatusEmoji(status string) string {
	switch status {
	case "NORMAL":
		return "✅"
	case "NEAR_LIMIT":
		return "⚠️"
	case "OVER_LIMIT":
		return "🚨"
	default:
		return "❓"
	}
}

func getMarkers(user models.UserIPStats) string {
	var markers []string
	if user.IsExcluded {
		markers = append(markers, "[EXCLUDED]")
	}
	if user.HasAlertCooldown {
		markers = append(markers, "[ALERT_COOLDOWN]")
	}
	if user.IsDebug {
		markers = append(markers, "[DEBUG]")
	}
	if len(markers) > 0 {
		return " " + strings.Join(markers, " ")
	}
	return ""
}