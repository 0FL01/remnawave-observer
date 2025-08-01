package models

// LogEntry представляет запись лога, поступающую в сервис.
type LogEntry struct {
	UserEmail string `json:"user_email" binding:"required"`
	SourceIP  string `json:"source_ip" binding:"required"`
}

// AlertPayload представляет данные для отправки в вебхук.
type AlertPayload struct {
	UserIdentifier   string   `json:"user_identifier"`
	DetectedIPsCount int      `json:"detected_ips_count"` // Может содержать кол-во IP или подсетей
	Limit            int      `json:"limit"`
	AllUserIPs       []string `json:"all_user_ips"` // Может содержать IP или подсети
	BlockDuration    string   `json:"block_duration"`
	ViolationType    string `json:"violation_type"`
}

// UserIPStats содержит статистику по IP-адресам пользователя для мониторинга.
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

// BlockMessage представляет сообщение для отправки в очередь на блокировку.
type BlockMessage struct {
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// CheckResult представляет результат выполнения Lua-скрипта.
type CheckResult struct {
	StatusCode   int64
	CurrentCount int64
	IsNew        bool
	AllUserItems []string
}