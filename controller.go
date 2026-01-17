package main

import (
	"context" // context is often used in controller methods, good to include
	"net/url"
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

// Controller 结构体定义，包含了所有核心组件和状态
type Controller struct {
	mu          sync.RWMutex
	config      *Config                 // 全局配置，通常从 YAML 文件加载 (定义在 types.go)
	rules       []RuleRuntime           // 运行时规则列表 (定义在 types.go)
	clientset   *kubernetes.Clientset   // Kubernetes API 客户端
	mongoClient *mongo.Client           // MongoDB 客户端
	tgBot       *tgbotapi.BotAPI        // Telegram Bot API 客户端
	pending     sync.Map                // 用于异步操作，如等待告警确认
	pendingRule sync.Map                // 用于存储处于 pending 状态的 rule
	configPath  string                  // 配置文件的路径
}

// LoadConfig 从指定路径加载并解析配置文件。
// 它会初始化 Controller 的 config 和 rules 字段，并处理旧版域名的兼容性。
func (c *Controller) LoadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		klog.Errorf("Failed to read config file %s: %v", path, err)
		return err
	}
	var cfg Config // Config 类型定义在 types.go
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		klog.Errorf("Failed to unmarshal config file %s: %v", path, err)
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.config = &cfg
	c.rules = make([]RuleRuntime, len(cfg.Rules)) // RuleRuntime 类型定义在 types.go

	for i := range cfg.Rules {
		runtime := RuleRuntime{ // RuleRuntime 类型定义在 types.go
			Config:        cfg.Rules[i],
			IsSwitched:    false,
			LastProbeOK:   true,
			CurrentStreak: 0,
			LastStreakOK:  true,
		}

		// 兼容旧的 domain 配置：如果提供了 domain 但没有提供 endpoints，
		// 则尝试解析 domain 并创建一个默认的 endpoint。
		if runtime.Config.Domain != "" && len(runtime.Config.Endpoints) == 0 {
			u, err := url.Parse(runtime.Config.Domain)
			if err == nil && u.Host != "" {
				runtime.Config.BaseDomain = u.Scheme + "://" + u.Host
				runtime.Config.Endpoints = []EndpointConfig{ // EndpointConfig 类型定义在 types.go
					{
						Path:          u.Path,
						Method:        "GET",
						ExpectedCodes: cfg.Global.ExpectedCodes, // 使用全局默认状态码
					},
				}
				klog.V(2).Infof("Rule %s: Migrated old 'domain' to 'base_domain' and default endpoint.", runtime.Config.Domain)
			} else if err != nil {
				klog.Warningf("Rule %s: Failed to parse old domain, please check format or convert to new 'base_domain' + 'endpoints' format: %v", runtime.Config.Domain, err)
			}
		}

		c.rules[i] = runtime
	}

	klog.Infof("Configuration loaded successfully from %s! Number of rules: %d", path, len(cfg.Rules))
	if len(cfg.Rules) == 0 {
		klog.Warning("Warning: No rules defined in the configuration. Probes will not start!")
	}

	return nil
}

// WatchConfig 监控配置文件路径的更改，并在文件写入时重新加载配置。
func (c *Controller) WatchConfig(path string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Fatalf("Failed to create file watcher: %v", err)
	}
	defer watcher.Close()

	err = watcher.Add(path)
	if err != nil {
		klog.Fatalf("Failed to add file %s to watcher: %v", path, err)
	}

	klog.Infof("Watching config file for changes: %s", path)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				klog.Info("Config file watcher events channel closed.")
				return
			}
			// 仅在文件被写入时重新加载配置，避免不必要的重载（如 stat/chmod）
			if event.Op&fsnotify.Write == fsnotify.Write {
				klog.Infof("Config file %s modified, reloading configuration...", path)
				if err := c.LoadConfig(path); err != nil {
					klog.Errorf("Error reloading configuration from %s: %v", path, err)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				klog.Info("Config file watcher errors channel closed.")
				return
			}
			klog.Errorf("Config file watcher error: %v", err)
		}
	}
}

// StartProbers 为每个配置的规则启动一个独立的探测器 goroutine。
func (c *Controller) StartProbers() {
	c.mu.RLock() // Read lock to access c.rules and c.config
	defer c.mu.RUnlock()

	if len(c.rules) == 0 {
		klog.Warning("No rules to probe. Skipping prober startup.")
		return
	}

	klog.Infof("Starting %d probers for configured rules...", len(c.rules))

	for i := range c.rules {
		// IMPORTANT: Create a local copy of the rule pointer for the goroutine.
		// Otherwise, all goroutines might end up using the last rule in the slice.
		rule := &c.rules[i]

		// 解析探测间隔：规则级间隔优先，然后是全局间隔，最后是默认值 30s。
		intervalStr := rule.Config.ProbeInterval
		if intervalStr == "" {
			intervalStr = c.config.Global.ProbeInterval
		}
		if intervalStr == "" {
			intervalStr = "30s" // Default probe interval
		}
		interval, err := time.ParseDuration(intervalStr)
		if err != nil {
			klog.Errorf("Rule '%s' probe interval parsing failed ('%s'), using default 30s: %v", rule.Config.Domain, intervalStr, err)
			interval = 30 * time.Second
		}

		// 获取确认计数，默认为 1。
		confirmCount := rule.Config.ConfirmCount
		if confirmCount <= 0 {
			confirmCount = 1
		}

		go func(r *RuleRuntime) {
			ticker := time.NewTicker(interval)
			defer ticker.Stop() // 确保 goroutine 退出时 ticker 被停止

			klog.Infof("Prober started for rule → base_domain: %s, endpoints: %d, interval: %v, confirm_count: %d",
				r.Config.BaseDomain, len(r.Config.Endpoints), interval, confirmCount)

			// Initial probe immediately
			klog.V(1).Infof("Performing initial probe for → base_domain: %s", r.Config.BaseDomain)
			c.probeAndAct(r)

			for range ticker.C {
				klog.V(1).Infof("Starting new probe cycle for → base_domain: %s", r.Config.BaseDomain)
				// c.probeAndAct 方法应该在另一个文件（例如 storage.go 或 prober.go）中实现，
				// 它会执行实际的探测逻辑并根据结果采取行动。
				c.probeAndAct(r)
			}
		}(rule)
	}
}

// probeAndAct 是一个占位符，假定在其他地方（例如在 storage.go 或者一个新的 prober.go 中）实现了该方法。
// 它的职责是执行实际的探测并根据结果进行切换和通知。
// 由于它需要访问 Controller 的字段 (如 clientset, tgBot, mongoClient)，
// 因此它必须是 Controller 的一个方法。
func (c *Controller) probeAndAct(rule *RuleRuntime) {
    // 实际的探测逻辑、状态更新、切换 K8s Service Selector、发送 Telegram 通知等将在这里实现。
    // 这部分逻辑是您业务的核心，这里仅作为占位符。
	klog.V(2).Infof("Executing probe and act for rule: %s", rule.Config.BaseDomain)

    // Example placeholder:
    // isHealthy := c.performHealthChecks(rule)
    // if isHealthy {
    //    c.handleHealthyState(rule)
    // } else {
    //    c.handleUnhealthyState(rule)
    // }

    // This method would likely call other Controller methods like:
    // - c.backupSelectors(...)
    // - c.restoreSelectors(...)
    // - c.updateUserCache(...)
    // - c.logEvent(...)
    // - c.tgBot.Send(...)
    // It would also update the rule's runtime state: rule.IsSwitched, rule.LastProbeOK, rule.CurrentStreak, etc.
}

// 示例：这是一个假设的方法，用于在 Controller 初始化后设置 Kubernetes 客户端。
// 实际应用中，这通常在 main 函数或 Controller 的初始化逻辑中完成。
func (c *Controller) SetKubernetesClient(clientset *kubernetes.Clientset) {
	c.clientset = clientset
}

// 示例：这是假设的方法，用于在 Controller 初始化后设置 Telegram 机器人客户端。
func (c *Controller) SetTelegramBot(bot *tgbotapi.BotAPI) {
	c.tgBot = bot
}

// 示例：这是假设的方法，用于在 Controller 初始化后设置 MongoDB 客户端。
// 注意：InitMongo 方法已经在 storage.go 中定义，它会设置 c.mongoClient。
// 所以这里只是一个示意，如果需要直接设置，可以有类似方法。
func (c *Controller) SetMongoClient(client *mongo.Client) {
    c.mongoClient = client
}
