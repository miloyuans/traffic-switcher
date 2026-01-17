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
    Domain                       string   `yaml:"domain"`
    ExpectedCodes                []int    `yaml:"expected_codes"`
    ForceSwitch                  bool     `yaml:"force_switch"`
    TargetServices               []ServiceRef `yaml:"target_services"`
    NotificationMessage          string   `yaml:"notification_message"` // 旧交互通知 fallback
    FailoverMessageTemplate      string   `yaml:"failover_message_template"` // 交互故障通知模板
    RecoveryMessageTemplate      string   `yaml:"recovery_message_template"` // 交互恢复通知模板
    SuccessFailoverMessageTemplate string `yaml:"success_failover_message_template"` // 新：故障切换成功通知模板
    SuccessRecoveryMessageTemplate string `yaml:"success_recovery_message_template"` // 新：恢复成功通知模板
    DisplayDomains               []string `yaml:"display_domains"`
    AuthorizedUserIDs            []int64  `yaml:"authorized_user_ids"`
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

// Telegram 用户缓存结构（MongoDB）
type UserCacheDoc struct {
    UserID      int64     `bson:"user_id"`
    Username    string    `bson:"username"`
    LastUpdated time.Time `bson:"last_updated"`
}