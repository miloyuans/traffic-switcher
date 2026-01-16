package main

import (
    "net/http"
    "os"
    "sync"
    "time"

    "github.com/fsnotify/fsnotify"
    "gopkg.in/yaml.v3"

    "k8s.io/client-go/kubernetes"
    "go.mongodb.org/mongo-driver/mongo"
    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    "k8s.io/klog/v2"
)

type Controller struct {
    mu          sync.RWMutex
    config      *Config
    rules       []RuleRuntime
    clientset   *kubernetes.Clientset
    mongoClient *mongo.Client
    tgBot       *tgbotapi.BotAPI
    pending     sync.Map // string(uuid) -> chan bool
    configPath  string   // 新增：配置文件路径，用于写回 force_switch
}

func (c *Controller) LoadConfig(path string) error {
    data, err := os.ReadFile(path)
    if err != nil {
        return err
    }
    var cfg Config
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return err
    }

    c.mu.Lock()
    c.config = &cfg
    // 重建 runtime rules
    c.rules = make([]RuleRuntime, len(cfg.Rules))
    for i := range cfg.Rules {
        c.rules[i] = RuleRuntime{
            Config:      cfg.Rules[i],
            LastProbeOK: true, // 初始假设正常
        }
    }
    c.mu.Unlock()
    klog.Info("Config loaded")
    return nil
}

func (c *Controller) WatchConfig(path string) {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        klog.Fatal(err)
    }
    defer watcher.Close()

    err = watcher.Add(path)
    if err != nil {
        klog.Fatal(err)
    }

    for {
        select {
        case event, ok := <-watcher.Events:
            if !ok {
                return
            }
            if event.Op&fsnotify.Write == fsnotify.Write {
                if err := c.LoadConfig(path); err != nil {
                    klog.Error(err)
                }
            }
        case err, ok := <-watcher.Errors:
            if !ok {
                return
            }
            klog.Error(err)
        }
    }
}

func (c *Controller) StartProbers() {
    c.mu.RLock()
    interval, _ := time.ParseDuration(c.config.Global.ProbeInterval)
    c.mu.RUnlock()

    for i := range c.rules {
        rule := &c.rules[i]
        go func(r *RuleRuntime) {
            ticker := time.NewTicker(interval)
            for range ticker.C {
                c.probeAndAct(r)
            }
        }(rule)
    }
}

func (c *Controller) probeAndAct(rule *RuleRuntime) {
    c.mu.RLock()
    globalCodes := c.config.Global.ExpectedCodes
    c.mu.RUnlock()

    expected := globalCodes
    if len(rule.Config.ExpectedCodes) > 0 {
        expected = rule.Config.ExpectedCodes
    }

    ok := c.probeURL(rule.Config.Domain, expected)

    prevOK := rule.LastProbeOK
    rule.LastProbeOK = ok

    // 强制开关优先（但不 return，让其继续判断恢复）
    if rule.Config.ForceSwitch {
        c.requestFailover(rule, "force_switch")
    }

    if !ok {
        c.requestFailover(rule, "health_check_failed")
    } else if ok && !prevOK && rule.IsSwitched {
        // 从异常恢复到正常，且已切换 → 触发恢复
        c.requestRecovery(rule)
        go c.disableForceSwitchIfNeeded(rule)
    }
}

func (c *Controller) probeURL(url string, expected []int) bool {
    resp, err := http.Get(url)
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    for _, code := range expected {
        if resp.StatusCode == code {
            return true
        }
    }
    return false
}