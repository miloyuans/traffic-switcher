package main

import (
    "time"
)

type Config struct {
    Global GlobalConfig `yaml:"global"`
    Rules  []RuleConfig `yaml:"rules"`
}

type GlobalConfig struct {
    SourceService struct {
        Namespace string `yaml:"namespace"`
        Name      string `yaml:"name"`
    } `yaml:"source_service"`
    ProbeInterval string `yaml:"probe_interval"`
    ExpectedCodes []int  `yaml:"expected_codes"`
    Telegram      struct {
        BotToken string `yaml:"bot_token"`
        ChatID   string `yaml:"chat_id"`  // string 支持负数群组
    } `yaml:"telegram"`
    MongoDB struct {
        URI           string `yaml:"uri"`
        Database      string `yaml:"database"`
        RetentionDays int    `yaml:"retention_days"`
    } `yaml:"mongodb"`
}

type RuleConfig struct {
    Domain                 string   `yaml:"domain"`                  // 探测域名（内部使用，不展示在通知）
    ExpectedCodes          []int    `yaml:"expected_codes"`
    ForceSwitch            bool     `yaml:"force_switch"`
    TargetServices         []ServiceRef `yaml:"target_services"`
    NotificationMessage    string   `yaml:"notification_message"`    // 旧字段，兼容（若新模板未定义，可fallback）
    FailoverMessageTemplate string  `yaml:"failover_message_template"` // 新：故障切换通知模板
    RecoveryMessageTemplate string  `yaml:"recovery_message_template"` // 新：恢复通知模板
    DisplayDomains         []string `yaml:"display_domains"`          // 新：受影响的主域名列表（通知中展示）
    AuthorizedUserIDs      []int64  `yaml:"authorized_user_ids"`      // 新：允许操作的用户ID列表（多用户）
}

type ServiceRef struct {
    Namespace string `yaml:"namespace"`
    Name      string `yaml:"name"`
}

type RuleRuntime struct {
    Config      RuleConfig
    IsSwitched  bool // 当前是否已切换到源 Selector
    LastProbeOK bool // 上一次探测是否正常
}

// 备份文档结构
type BackupDoc struct {
    RuleDomain string            `bson:"rule_domain"`
    Namespace  string            `bson:"namespace"`
    Service    string            `bson:"service"`
    Selector   map[string]string `bson:"selector"`
    Timestamp  time.Time         `bson:"timestamp"`
}

// 事件文档结构
type EventDoc struct {
    Timestamp  time.Time `bson:"timestamp"`
    RuleDomain string    `bson:"rule_domain"`
    Action     string    `bson:"action"`
    Message    string    `bson:"message"`
}