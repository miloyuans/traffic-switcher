package main

import (
    "net/url" // 新增：用于 url.Parse
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
    pendingRule sync.Map // string(uuid) -> *RuleRuntime
    configPath  string
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
            Config:        cfg.Rules[i],
            IsSwitched:    false,
            LastProbeOK:   true,
            CurrentStreak: 0,
            LastStreakOK:  true,
        }

        // 兼容旧 domain 配置
        if runtime.Config.Domain != "" && len(runtime.Config.Endpoints) == 0 {
            u, err := url.Parse(runtime.Config.Domain)
            if err == nil && u.Host != "" {
                runtime.Config.BaseDomain = u.Scheme + "://" + u.Host
                runtime.Config.Endpoints = []EndpointConfig{
                    {
                        Path:          u.Path,
                        Method:        "GET",
                        ExpectedCodes: cfg.Global.ExpectedCodes,
                    },
                }
            } else if err != nil {
                klog.Warningf("旧 domain 解析失败: %v", err)
            }
        }

        c.rules[i] = runtime
    }

    klog.Infof("配置文件加载成功！Rules 数量: %d", len(cfg.Rules))
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
            klog.Errorf("rule interval 解析失败，使用默认 30s: %v", err)
            interval = 30 * time.Second
        }

        confirmCount := rule.Config.ConfirmCount
        if confirmCount <= 0 {
            confirmCount = 1
        }

        go func(r *RuleRuntime) {
            ticker := time.NewTicker(interval)
            defer ticker.Stop()
            klog.Infof("探测器启动 → base_domain: %s, endpoints: %d, interval: %v, confirm_count: %d",
                r.Config.BaseDomain, len(r.Config.Endpoints), interval, confirmCount)

            for range ticker.C {
                klog.V(1).Infof("开始新一轮探测 → base_domain: %s", r.Config.BaseDomain)
                c.probeAndAct(r)
            }
        }(rule)
    }
}