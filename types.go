package main

import (
	"time"
)

// UserCacheDoc definition (missing from original code, added here as it's a core data structure)
type UserCacheDoc struct {
	UserID      int64     `bson:"user_id"`
	Username    string    `bson:"username"`
	LastUpdated time.Time `bson:"last_updated"`
}

type Config struct {
	Global GlobalConfig `yaml:"global"`
	Rules  []RuleConfig `yaml:"rules"`
}

type GlobalConfig struct {
	SourceService struct {
		Namespace string `yaml:"namespace"`
		Name      string `yaml:"name"`
	} `yaml:"source_service"`
	ProbeInterval string `yaml:"probe_interval"` // 全局默认间隔
	ExpectedCodes []int  `yaml:"expected_codes"` // 全局默认状态码
	Telegram      struct {
		BotToken string `yaml:"bot_token"`
		ChatID   string `yaml:"chat_id"`
	} `yaml:"telegram"`
	MongoDB struct {
		URI           string `yaml:"uri"`
		Database      string `yaml:"database"`
		RetentionDays int    `yaml:"retention_days"`
	} `yaml:"mongodb"`
}

type EndpointConfig struct {
	Path                 string            `yaml:"path"`                   // 路径，如 "/casino"
	Method               string            `yaml:"method"`                 // "GET" 或 "POST"，默认 "GET"
	Params               map[string]string `yaml:"params"`                 // 参数（GET query, POST JSON body）
	ExpectedCodes        []int             `yaml:"expected_codes"`         // 期望状态码，默认继承 rule/global
	ExpectedBodyContains string            `yaml:"expected_body_contains"` // body 包含此字符串算成功，可空
}

type RuleConfig struct {
	Domain                         string            `yaml:"domain"`                          // 兼容旧单域名配置（自动转为 endpoint）
	BaseDomain                     string            `yaml:"base_domain"`                     // 新：主域名，如 "https://hkv2.vpbet.com"
	Endpoints                      []EndpointConfig  `yaml:"endpoints"`                       // 新：多个探测端点
	ProbeInterval                  string            `yaml:"probe_interval"`                  // rule 级探测间隔，覆盖全局
	ConfirmCount                   int               `yaml:"confirm_count"`                   // 连续多少次才确认状态，默认 1
	ForceSwitch                    bool              `yaml:"force_switch"`
	TargetServices                 []ServiceRef      `yaml:"target_services"`	
	NotificationMessage            string            `yaml:"notification_message"`
	FailoverMessageTemplate        string            `yaml:"failover_message_template"`
	RecoveryMessageTemplate        string            `yaml:"recovery_message_template"`
	SuccessFailoverMessageTemplate string            `yaml:"success_failover_message_template"`
	SuccessRecoveryMessageTemplate string            `yaml:"success_recovery_message_template"`
	DisplayDomains                 []string          `yaml:"display_domains"`
	AuthorizedUserIDs              []int64           `yaml:"authorized_user_ids"`
}

type ServiceRef struct {
	Namespace        string            `yaml:"namespace"`
	Name             string            `yaml:"name"`
	OriginalSelector map[string]string `yaml:"original_selector,omitempty"` // 可选：该 Service 的原始正确 Selector

type RuleRuntime struct {
	Config        RuleConfig
	IsSwitched    bool
	LastProbeOK   bool
	CurrentStreak int  // 当前连续状态次数
	LastStreakOK  bool // 上次连续状态是否健康
}

type BackupDoc struct {
	RuleDomain string            `bson:"rule_domain"`
	Namespace  string            `bson:"namespace"`
	Service    string            `bson:"service"`
	Selector   map[string]string `bson:"selector"`
	Timestamp  time.Time         `bson:"timestamp"`
}

type EventDoc struct {
	Timestamp  time.Time `bson:"timestamp"`
	RuleDomain string    `bson:"rule_domain"`
	Action     string    `bson:"action"`
	Message    string    `bson:"message"`
}
