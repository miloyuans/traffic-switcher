package main

import (
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
    pendingRule sync.Map // 新增：string(uuid) -> *RuleRuntime，用于权限检查和@用户
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
    defer c.mu.Unlock()

    c.config = &cfg
    c.rules = make([]RuleRuntime, len(cfg.Rules))
    for i := range cfg.Rules {
        runtime := RuleRuntime{
            Config:       cfg.Rules[i],
            IsSwitched:   false,
            LastProbeOK:  true,
            CurrentStreak: 0,
            LastStreakOK: true,
        }

        // 兼容旧 domain 配置
        if runtime.Config.Domain != "" && len(runtime.Config.Endpoints) == 0 {
            // 解析 path 从 domain
            u, err := url.Parse(runtime.Config.Domain)
            if err == nil {
                runtime.Config.BaseDomain = u.Scheme + "://" + u.Host
                runtime.Config.Endpoints = []EndpointConfig{
                    {
                        Path:          u.Path,
                        Method:        "GET",
                        ExpectedCodes: cfg.Global.ExpectedCodes,
                    },
                }
            }
        }

        c.rules[i] = runtime
    }

    klog.Infof("配置文件加载成功！全局间隔: %s, Rules 数量: %d", cfg.Global.ProbeInterval, len(cfg.Rules))
    if len(cfg.Rules) == 0 {
        klog.Warning("警告：配置中没有定义任何 rule，探测器将不启动！")
    }

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
    intervalStr := c.config.Global.ProbeInterval
    if intervalStr == "" {
        intervalStr = "30s" // 默认值
    }
    interval, err := time.ParseDuration(intervalStr)
    if err != nil {
        klog.Errorf("ProbeInterval 解析失败 (%s)，使用默认 30s: %v", intervalStr, err)
        interval = 30 * time.Second
    }
    rulesCount := len(c.rules)
    c.mu.RUnlock()

    klog.Infof("启动 %d 个探测器", len(c.rules))

    if len(c.rules) == 0 {
        klog.Warning("无 rule 可探测")
        return
    }

    for i := range c.rules {
        rule := &c.rules[i]
        // rule 级间隔优先
        intervalStr := rule.Config.ProbeInterval
        if intervalStr == "" {
            intervalStr = c.config.Global.ProbeInterval
        }
        if intervalStr == "" {
            intervalStr = "30s"
        }
        interval, err := time.ParseDuration(intervalStr)
        if err != nil {
            klog.Errorf("rule %s interval 解析失败，使用默认 30s: %v", rule.Config.BaseDomain, err)
            interval = 30 * time.Second
        }

        confirmCount := rule.Config.ConfirmCount
        if confirmCount <= 0 {
            confirmCount = 1
        }

        go func(r *RuleRuntime) {
            ticker := time.NewTicker(interval)
            klog.Infof("探测器启动 → base_domain: %s, endpoints: %d, interval: %v, confirm_count: %d",
                r.Config.BaseDomain, len(r.Config.Endpoints), interval, confirmCount)

            for range ticker.C {
                klog.V(1).Infof("开始新一轮探测 → base_domain: %s", r.Config.BaseDomain)
                c.probeAndAct(r)
            }
        }(rule)
    }
}