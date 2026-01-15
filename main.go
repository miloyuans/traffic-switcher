package main

import (
	"context"
	"encoding/json" // json用于序列化k8s patch和CM数据
	"flag"
	"fmt"
	"html/template"
	"log" // 依然需要，因为zap的初始化需要
	"net/http"
	"os"
	"os/signal" // Used for graceful shutdown in main
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall" // Used for graceful shutdown in main
	"time"

	"github.com/fsnotify/fsnotify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2" // 引入yaml解析库
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
)

// TelegramTemplates 定义了Telegram通知消息的模板
type TelegramTemplates struct {
	StartupMessage            string `yaml:"startup_message" json:"startup_message"`
	FaultMessage              string `yaml:"fault_message" json:"fault_message"`
	ConfirmButtonText         string `yaml:"confirm_button_text" json:"confirm_button_text"` // 新增按钮文本
	ManualButtonText          string `yaml:"manual_button_text" json:"manual_button_text"`   // 新增按钮文本
	ConfirmReply              string `yaml:"confirm_reply" json:"confirm_reply"`
	ManualReply               string `yaml:"manual_reply" json:"manual_reply"`
	RecoveryMessage           string `yaml:"recovery_message" json:"recovery_message"`
	ForceMaintenanceOnMessage string `yaml:"force_maintenance_on_message" json:"force_maintenance_on_message"`
	ForceMaintenanceOffMessage string `yaml:"force_maintenance_off_message" json:"force_maintenance_off_message"`
}

// GlobalConfig 定义了应用程序的全局配置，包括HTTP监听和Telegram模板
type GlobalConfig struct {
	HTTPListenAddr    string            `yaml:"http_listen_addr" json:"http_listen_addr"`
	HTTPListenPort    string            `yaml:"http_listen_port" json:"http_listen_port"`
	TelegramTemplates TelegramTemplates `yaml:"telegram_templates" json:"telegram_templates"`
}

// Rule 定义了单个域名/服务的切换规则
type Rule struct {
	Domain            string      `yaml:"domain" json:"domain"`
	CheckURL          string      `yaml:"check_url" json:"check_url"`
	CheckCondition    string      `yaml:"check_condition" json:"check_condition"`
	FailThreshold     int         `yaml:"fail_threshold" json:"fail_threshold"`
	RecoveryThreshold int         `yaml:"recovery_threshold" json:"recovery_threshold"`
	CheckInterval     string      `yaml:"check_interval" json:"check_interval"`
	ForceSwitch       bool        `yaml:"force_switch" json:"force_switch"` // 新增：每域名独立强制切换开关
	MaintenanceLabel  string      `yaml:"maintenance_pod_label" json:"maintenance_pod_label"`
	Services          []ServiceNS `yaml:"services" json:"services"`
}

// ServiceNS 定义了服务及其所属的命名空间
type ServiceNS struct {
	Namespace string   `yaml:"namespace" json:"namespace"`
	SvcNames  []string `yaml:"svc_names" json:"svc_names"`
}

// Config 包含了所有规则和全局配置
type Config struct {
	Global GlobalConfig `yaml:"global_config" json:"global_config"`
	Rules  []Rule       `yaml:"rules" json:"rules"`
}

// State 存储了每个域名的当前状态
type State struct {
	Status    string `json:"status"` // "normal" or "failed"
	Notified  bool   `json:"notified"`
	Confirmed bool   `json:"confirmed"`
}

var (
	configPath        = "/config/rules.yaml"
	htmlPath          = "/config/maintenance.html"
	telegramToken     = os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatIDStr = os.Getenv("TELEGRAM_CHAT_ID")
	telegramChatID    int64
	rules             []Rule          // 存储当前加载的规则
	previousRulesMap  map[string]Rule // 存储上一次加载的规则，用于比较变化
	states            sync.Map        // domain -> *State
	podIPs            []string
	mu                sync.RWMutex // 用于保护 rules, htmlTemplate 和 globalAppConfig 的读写
	clientset         *kubernetes.Clientset
	htmlTemplate      *template.Template
	originalEndpoints sync.Map // key: ns-svc, value: []byte (json subsets)
	maintenancePort   = 80     // Default, but can be overridden by rule label or future config
	logger            *zap.Logger
	probeSuccess      = prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_success_rate", Help: "URL probe success rate"})
	probeFailure      = prometheus.NewCounter(prometheus.GaugeOpts{Name: "probe_failure_count", Help: "Number of probe failures"}) // corrected to GaugeOpts
	switchCount       = prometheus.NewCounter(prometheus.CounterOpts{Name: "switch_count", Help: "Number of traffic switches"})
	stateConfigMap    = "traffic-switch-states"    // Persistent state CM
	programNamespace  = os.Getenv("POD_NAMESPACE") // Set in Deployment
	programPodName    = os.Getenv("POD_NAME")
	leaderLeaseName   = "traffic-switcher-leader"

	appBotApi *tgbotapi.BotAPI // 全局Bot实例，指针类型
	globalAppConfig GlobalConfig // 全局应用配置
)

func init() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatalf("Failed to init zap: %v", err)
	}
	// `probeFailure` must be CounterOpts for NewCounter
	// Corrected here, assuming it was a copy-paste error.
	probeFailure = prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_failure_count", Help: "Number of probe failures"})
	prometheus.MustRegister(probeSuccess, switchCount, probeFailure)

	if telegramChatIDStr != "" {
		telegramChatID, err = strconv.ParseInt(telegramChatIDStr, 10, 64)
		if err != nil {
			logger.Fatal("Invalid TELEGRAM_CHAT_ID format, please provide a valid integer chat ID.", zap.Error(err))
		}
		logger.Info("Telegram chat ID configured", zap.Int64("chat_id", telegramChatID))
	} else {
		logger.Warn("TELEGRAM_CHAT_ID environment variable is empty. Telegram notifications will be disabled.")
	}

	if programNamespace == "" {
		programNamespace = "default"
		logger.Info("POD_NAMESPACE not set, defaulting to 'default' namespace.")
	}

	previousRulesMap = make(map[string]Rule) // 初始化
}

func main() {
	defer logger.Sync()

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := flag.String("kubeconfig", "", "kubeconfig path")
		flag.Parse()
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			logger.Fatal("Failed to build Kubernetes config. Ensure you are running in a cluster or have a valid kubeconfig.", zap.Error(err))
		}
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal("Failed to create Kubernetes clientset.", zap.Error(err))
	}

	loadConfig() // 首次加载配置
	loadHTML()
	loadStatesFromCM()

	httpListenAddr := fmt.Sprintf("%s:%s", globalAppConfig.HTTPListenAddr, globalAppConfig.HTTPListenPort)
	http.HandleFunc("/", maintenanceHandler)
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", healthHandler)
	go func() {
		logger.Info("Starting HTTP server", zap.String("listen_address", httpListenAddr))
		if err := http.ListenAndServe(httpListenAddr, nil); err != nil {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	go watchConfigFile()

	rl, err := resourcelock.New(resourcelock.LeasesResourceLock,
		programNamespace,
		leaderLeaseName,
		clientset.CoreV1(),
		clientset.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: programPodName,
		})
	if err != nil {
		logger.Fatal("Failed to create leader election lock.", zap.Error(err))
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				logger.Info("Lost leadership, shutting down.")
				os.Exit(0)
			},
		},
		Name: leaderLeaseName,
	})
	if err != nil {
		logger.Fatal("Failed to create leader elector.", zap.Error(err))
	}

	// 启动Leader选举，并在此处等待系统信号
	// 这样os/signal和syscall就不会被标记为未使用
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 监听系统中断信号，优雅关闭
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-sig:
			logger.Info("Received termination signal, shutting down Leader Elector and application.")
			cancel() // 取消Leader选举的Context，从而停止run函数
		case <-ctx.Done():
			// 如果上下文被取消，也退出信号监听
			return
		}
	}()

	le.Run(ctx) // le.Run 会阻塞直到ctx被取消
	logger.Info("Leader Elector stopped. Application shutting down.")
	updateStatesToCM() // 最后保存状态
}

// run 在成为Leader后执行，负责核心业务逻辑
func run(ctx context.Context) {
	logger.Info("Successfully acquired leadership, starting core operations.")

	go watchOwnPods()

	var err error
	appBotApi, err = tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		logger.Fatal("Failed to initialize Telegram Bot API. Please ensure TELEGRAM_BOT_TOKEN is valid.", zap.Error(err))
	}

	botUser, err := appBotApi.GetMe()
	if err != nil {
		logger.Fatal("Telegram Token is invalid or API is unreachable. Cannot get bot information.", zap.Error(err))
	}
	logger.Info("Telegram Bot connected successfully", zap.String("bot_username", botUser.UserName))

	// 清除潜在的旧 Webhook (重要)
	// 确保在尝试 Long Polling 之前，Bot 没有配置 Webhook
	deleteWebhookConfig := tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true}
	_, err = appBotApi.Request(deleteWebhookConfig)
	if err != nil {
		logger.Warn("Failed to delete old Telegram webhook (if any, this is often fine if no webhook was set): %v", zap.Error(err))
	} else {
		logger.Info("Successfully deleted old Telegram webhook configuration.")
	}

	// 启动 Long Polling 来接收 Telegram 消息
	// 使用 Leader Elector 提供的 Context 来控制 Long Polling goroutine 的生命周期
	go processTelegramUpdates(ctx, appBotApi)

	if telegramChatID != 0 && globalAppConfig.TelegramTemplates.StartupMessage != "" {
		startupMsgText := fmt.Sprintf(globalAppConfig.TelegramTemplates.StartupMessage, programPodName, programNamespace)
		sendTelegramMessage(telegramChatID, startupMsgText, "Markdown", nil)
	} else {
		logger.Warn("Skipping Telegram startup message: TELEGRAM_CHAT_ID not set or template is empty.")
	}

	// 为 monitorRule 创建独立的上下文，因为它的生命周期与 Leader election 可能不同步（如Leader Elector可能在monitorRule的中间阶段取消ctx）
	// 但是这里如果直接用le.Run传入的ctx，那么当失去Leader时所有监控goroutine都会停止。
	// 为了使monitorRule能响应配置文件改变，它需要重新读取规则，但它的生命周期受传入的ctx控制。
	// 这里仍然使用le.Run传入的ctx，意味着失去Leader即停止监控，这符合单一Leader的原则。
	// 所以无需独立的cancelCtx，直接用传入的ctx即可
	monitorCtx := ctx

	mu.RLock()
	currentRules := rules
	mu.RUnlock()

	if len(currentRules) == 0 {
		logger.Warn("No rules loaded from config. Monitoring will not start.")
	}

	// 为每个规则启动监控goroutine
	for _, rule := range currentRules {
		domain := rule.Domain
		if _, loaded := states.LoadOrStore(domain, &State{Status: "normal"}); !loaded {
			logger.Info("Initialized state for new domain", zap.String("domain", domain), zap.String("status", "normal"))
			updateStatesToCM()
		}
		// 传递 rule 的一个副本，避免在 goroutine 外部 rule 变量变化影响
		go monitorRule(monitorCtx, rule)
	}

	// 阻塞直到 Leader Context 被取消
	<-ctx.Done()
	logger.Info("Leader context cancelled, run function is shutting down.")
	// `updateStatesToCM()` 在 main 函数的 defer 中调用，确保最终保存
}

// processTelegramUpdates 持续处理从Telegram Bot API接收到的更新
func processTelegramUpdates(ctx context.Context, bot *tgbotapi.BotAPI) {
	logger.Info("Starting Telegram long polling for updates...")
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60 // Long polling timeout

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping Telegram update processing due to context cancellation.")
			return
		default:
			// GetUpdatesChan returns a channel that will yield updates.
			// It will manage the offset automatically.
			// If the underlying connection breaks, the channel will close,
			// and we'll re-call GetUpdatesChan to get a new one.
			updatesChan, err := bot.GetUpdatesChan(updateConfig) // 修正：GetUpdatesChan只返回一个值
			if err != nil {
				logger.Error("Failed to get Telegram updates channel. Retrying in 5 seconds...", zap.Error(err))
				time.Sleep(5 * time.Second)
				continue
			}

			// Process updates from the channel
			for update := range updatesChan { // Loop will break if updatesChan closes
				select {
				case <-ctx.Done():
					logger.Info("Telegram updates processing interrupted by context cancellation.")
					return // Exit this inner loop and the outer loop's select will catch ctx.Done()
				default:
					if update.CallbackQuery != nil {
						// 这是 Inline Keyboard 按钮点击事件
						handleTelegramCallbackQuery(update.CallbackQuery)
					} else if update.Message != nil {
						// 这是常规消息 (文本命令，或其他)
						handleTelegramMessage(update.Message)
					} else {
						logger.Debug("Received unknown Telegram update type, ignoring.", zap.Any("update", update))
					}
				}
			}
			logger.Warn("Telegram updates channel closed unexpectedly. Re-initializing GetUpdatesChan.")
			time.Sleep(2 * time.Second) // Small delay before re-calling to prevent tight loop on error
		}
	}
}

// handleTelegramMessage 处理单个Telegram文本消息 (例如直接输入的命令)
func handleTelegramMessage(message *tgbotapi.Message) {
	text := message.Text
	chatID := message.Chat.ID
	logger.Info("Received Telegram message", zap.String("from", message.From.UserName), zap.String("text", text), zap.Int64("chat_id", chatID))

	var replyText string
	// 这里可以放置处理非按钮点击的文本命令逻辑
	// 比如用户直接输入 "/status" 获取状态等。
	// 对于 /confirm_ 和 /manual_，现在主要通过按钮处理。
	// 作为通用回复，暂时提供一个帮助信息
	replyText = "Hello! I am Traffic Switcher bot. Please use the buttons provided in fault notifications to interact with me, or try `/help` for more info."

	if replyText != "" {
		sendTelegramMessage(chatID, replyText, "Markdown", nil)
	}
}

// handleTelegramCallbackQuery 处理Inline Keyboard按钮点击事件
func handleTelegramCallbackQuery(callback *tgbotapi.CallbackQuery) {
	callbackData := callback.Data
	chatID := callback.Message.Chat.ID
	messageID := callback.Message.MessageID // 可以用于编辑原消息
	userName := callback.From.UserName

	logger.Info("Received Telegram CallbackQuery",
		zap.String("from", userName),
		zap.String("data", callbackData),
		zap.Int64("chat_id", chatID),
		zap.Int("message_id", messageID))

	var replyText string

	// 回复 CallbackQuery，通常用于消除按钮上的加载动画
	callbackAnswer := tgbotapi.NewCallback(callback.ID, "") // 空文本会立即消除动画
	if _, err := appBotApi.Request(callbackAnswer); err != nil {
		logger.Error("Failed to answer Telegram CallbackQuery", zap.Error(err))
	}

	if strings.HasPrefix(callbackData, "confirm_") {
		domain := strings.TrimPrefix(callbackData, "confirm_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			if state.Status == "normal" {
				replyText = fmt.Sprintf("ℹ️ Domain `%s` is currently healthy. No action needed.", domain)
			} else {
				if state.Confirmed { // 已经确认过
					replyText = fmt.Sprintf("✅ Domain `%s` is already in confirmed maintenance mode. No change.", domain)
				} else {
					state.Confirmed = true
					updateStatesToCM()
					logger.Info("Traffic switch confirmed by Telegram user", zap.String("domain", domain), zap.String("user", userName))
					replyText = fmt.Sprintf(globalAppConfig.TelegramTemplates.ConfirmReply, domain)

					// 找到对应的rule并执行切换
					mu.RLock()
					currentRules := rules
					mu.RUnlock()
					for _, rule := range currentRules {
						if rule.Domain == domain {
							switchToMaintenance(rule) // 立即执行切换
							break
						}
					}
				}
			}
		} else {
			replyText = fmt.Sprintf("⚠️ No active rule found for domain: `%s`.", domain)
		}
	} else if strings.HasPrefix(callbackData, "manual_") {
		domain := strings.TrimPrefix(callbackData, "manual_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			if state.Status == "normal" { // 如果是正常状态，则不需要回切
				replyText = fmt.Sprintf("ℹ️ Domain `%s` is currently healthy. No switch to revert.", domain)
			} else {
				// 如果是故障状态，则取消确认，并尝试切回
				state.Confirmed = false
				state.Notified = false // 允许重新通知
				updateStatesToCM()
				logger.Info("Manual mode enabled by Telegram user, reverting switch if active", zap.String("domain", domain), zap.String("user", userName))
				replyText = fmt.Sprintf(globalAppConfig.TelegramTemplates.ManualReply, domain)
				
				// 找到对应的rule并执行切回
				mu.RLock()
				currentRules := rules
				mu.RUnlock()
				for _, rule := range currentRules {
					if rule.Domain == domain {
						switchBack(rule) // 立即执行切回
						break
					}
				}
			}
		} else {
			replyText = fmt.Sprintf("⚠️ No active rule found for domain: `%s`.", domain)
		}
	} else {
		replyText = "Unknown command or callback data."
	}

	if replyText != "" {
		// 可以选择编辑原消息或发送新消息。这里为了演示方便，发送新回复消息。
		sendTelegramMessage(chatID, replyText, "Markdown", nil)
	}
}


// loadConfig 从配置文件加载规则和全局配置
func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Error("Failed to read config file", zap.String("path", configPath), zap.Error(err))
		return
	}
	var loadedConfig Config
	if err = yaml.Unmarshal(data, &loadedConfig); err != nil { // 使用 yaml.Unmarshal
		logger.Error("Failed to parse config file (YAML format expected)", zap.String("path", configPath), zap.Error(err))
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// 复制当前的 rules 到 previousRulesMap
	previousRulesMap = make(map[string]Rule, len(rules))
	for _, r := range rules {
		previousRulesMap[r.Domain] = r
	}

	// 更新全局配置和规则
	globalAppConfig = loadedConfig.Global
	rules = loadedConfig.Rules

	// 设置HTTP监听地址和端口的默认值
	if globalAppConfig.HTTPListenAddr == "" {
		globalAppConfig.HTTPListenAddr = "0.0.0.0"
	}
	if globalAppConfig.HTTPListenPort == "" {
		globalAppConfig.HTTPListenPort = "8080"
	}

	// 设置Telegram模板的默认值
	if globalAppConfig.TelegramTemplates.StartupMessage == "" {
		globalAppConfig.TelegramTemplates.StartupMessage = "🚀 Traffic Switcher Pod: `%s` in `%s` acquired leadership."
	}
	if globalAppConfig.TelegramTemplates.FaultMessage == "" {
		globalAppConfig.TelegramTemplates.FaultMessage = "🚨 **Domain Fault Detected!**\n\nDomain: `%s` is failing.\n\n_Auto-switch will happen if confirmed._\n\nConfirm switch to maintenance?"
	}
	if globalAppConfig.TelegramTemplates.ConfirmButtonText == "" {
		globalAppConfig.TelegramTemplates.ConfirmButtonText = "✅ 确认切换到维护页"
	}
	if globalAppConfig.TelegramTemplates.ManualButtonText == "" {
		globalAppConfig.TelegramTemplates.ManualButtonText = "🔧 保持人工模式 (忽略此故障)"
	}
	if globalAppConfig.TelegramTemplates.ConfirmReply == "" {
		globalAppConfig.TelegramTemplates.ConfirmReply = "✅ **Switch Confirmed** for `%s`.\nTraffic will be directed to maintenance page."
	}
	if globalAppConfig.TelegramTemplates.ManualReply == "" {
		globalAppConfig.TelegramTemplates.ManualReply = "🔧 **Manual Mode Enabled** for `%s`.\nNotification will re-trigger on sustained failure."
	}
	if globalAppConfig.TelegramTemplates.RecoveryMessage == "" {
		globalAppConfig.TelegramTemplates.RecoveryMessage = "🟢 **Domain Recovered!**\n\nDomain: `%s` is healthy again.\nTraffic switched back to original endpoints."
	}
	if globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage == "" {
		globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage = "🚧 **Force Maintenance ON!**\n\nDomain: `%s` is manually forced into maintenance mode. Health checks are suspended."
	}
	if globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage == "" {
		globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage = "✅ **Force Maintenance OFF!**\n\nDomain: `%s` is restored from manual maintenance mode. Health checks resumed."
	}

	logger.Info("Config loaded successfully",
		zap.Int("rules_count", len(rules)),
		zap.String("http_listen_addr", globalAppConfig.HTTPListenAddr),
		zap.String("http_listen_port", globalAppConfig.HTTPListenPort))

	// 处理规则中 ForceSwitch 的变化
	handleRuleForceSwitchChanges()
}

// handleRuleForceSwitchChanges 处理规则中 ForceSwitch 状态的变化
func handleRuleForceSwitchChanges() {
	// 创建新的规则映射方便查找
	newRulesMap := make(map[string]Rule, len(rules))
	for _, r := range rules {
		newRulesMap[r.Domain] = r
	}

	// 遍历新规则，检测 ForceSwitch 变化
	for domain, newRule := range newRulesMap {
		oldRule, exists := previousRulesMap[domain]

		if exists { // 规则存在且未被删除
			if newRule.ForceSwitch && !oldRule.ForceSwitch {
				// ForceSwitch 从 false 变为 true
				logger.Info("Force switch enabled for domain via config.", zap.String("domain", domain))
				forceDomainToMaintenance(newRule)
				if telegramChatID != 0 && globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage != "" {
					sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage, domain), "Markdown", nil)
				}
			} else if !newRule.ForceSwitch && oldRule.ForceSwitch {
				// ForceSwitch 从 true 变为 false
				logger.Info("Force switch disabled for domain via config.", zap.String("domain", domain))
				forceDomainToNormal(newRule)
				if telegramChatID != 0 && globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage != "" {
					sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage, domain), "Markdown", nil)
				}
			}
		} else {
			// 新增的规则，如果 ForceSwitch 为 true，则强制维护
			if newRule.ForceSwitch {
				logger.Info("New rule with force switch enabled via config.", zap.String("domain", domain))
				forceDomainToMaintenance(newRule)
				if telegramChatID != 0 && globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage != "" {
					sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.ForceMaintenanceOnMessage, domain), "Markdown", nil)
				}
			}
		}
	}

	// 处理被删除的规则，如果之前是强制维护状态，则需要切回
	for domain, oldRule := range previousRulesMap {
		if _, exists := newRulesMap[domain]; !exists { // 规则在新配置中不存在
			if oldRule.ForceSwitch {
				logger.Info("Rule removed, disabling force switch for domain.", zap.String("domain", domain))
				// 此时 oldRule 是唯一的规则信息，直接使用它来回切
				forceDomainToNormal(oldRule)
				if telegramChatID != 0 && globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage != "" {
					sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.ForceMaintenanceOffMessage, domain), "Markdown", nil)
				}
			}
		}
	}
}

// forceDomainToMaintenance 强制某个域名进入维护模式
func forceDomainToMaintenance(rule Rule) {
	stateI, _ := states.LoadOrStore(rule.Domain, &State{}) // 确保状态存在
	state := stateI.(*State)
	// 只有当状态与期望不符时才执行操作，避免重复调用
	if state.Status != "failed" || !state.Confirmed || !state.Notified {
		state.Status = "failed"
		state.Notified = true  // 标记为已通知，防止自动再次通知
		state.Confirmed = true // 标记为已确认，以便执行切换
		updateStatesToCM()
		logger.Info("Manually forcing domain to maintenance state", zap.String("domain", rule.Domain), zap.Bool("force_switch", rule.ForceSwitch))
		switchToMaintenance(rule)
	} else {
		logger.Debug("Domain already in expected forced maintenance state, no action needed.", zap.String("domain", rule.Domain))
	}
}

// forceDomainToNormal 强制某个域名恢复正常模式
func forceDomainToNormal(rule Rule) {
	stateI, _ := states.LoadOrStore(rule.Domain, &State{}) // 确保状态存在
	state := stateI.(*State)
	// 只有当状态与期望不符时才执行操作
	if state.Status != "normal" || state.Confirmed || state.Notified {
		state.Status = "normal"
		state.Notified = false
		state.Confirmed = false
		updateStatesToCM()
		logger.Info("Manually forcing domain to normal state", zap.String("domain", rule.Domain), zap.Bool("force_switch", rule.ForceSwitch))
		switchBack(rule)
	} else {
		logger.Debug("Domain already in expected normal state, no action needed.", zap.String("domain", rule.Domain))
	}
}

func loadHTML() {
	tmpl, err := template.ParseFiles(htmlPath)
	if err != nil {
		logger.Error("Failed to load maintenance HTML template", zap.String("path", htmlPath), zap.Error(err))
		return
	}
	mu.Lock()
	defer mu.Unlock()
	htmlTemplate = tmpl
	logger.Info("Maintenance HTML template loaded successfully", zap.String("path", htmlPath))
}

func loadStatesFromCM() {
	cm, err := clientset.CoreV1().ConfigMaps(programNamespace).Get(context.Background(), stateConfigMap, metav1.GetOptions{})
	if err != nil {
		logger.Warn("State ConfigMap not found, attempting to create it.", zap.String("cm_name", stateConfigMap), zap.Error(err))
		_, createErr := clientset.CoreV1().ConfigMaps(programNamespace).Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: stateConfigMap},
			Data:       make(map[string]string),
		}, metav1.CreateOptions{})
		if createErr != nil {
			logger.Error("Failed to create state ConfigMap", zap.String("cm_name", stateConfigMap), zap.Error(createErr))
		} else {
			logger.Info("State ConfigMap created successfully", zap.String("cm_name", stateConfigMap))
		}
		return
	}

	loadedStateCount := 0
	loadedEndpointCount := 0
	for k, v := range cm.Data {
		if strings.HasPrefix(k, "state-") {
			domain := strings.TrimPrefix(k, "state-")
			var state State
			if jsonErr := json.Unmarshal([]byte(v), &state); jsonErr != nil {
				logger.Error("Failed to unmarshal state from ConfigMap", zap.String("key", k), zap.Error(jsonErr))
				continue
			}
			states.Store(domain, &state)
			loadedStateCount++
		} else if strings.HasPrefix(k, "original-") {
			key := strings.TrimPrefix(k, "original-")
			originalEndpoints.Store(key, []byte(v))
			loadedEndpointCount++
		}
	}
	logger.Info("States loaded from ConfigMap",
		zap.Int("loaded_state_count", loadedStateCount),
		zap.Int("loaded_original_endpoint_count", loadedEndpointCount),
		zap.String("cm_name", stateConfigMap))
}

func updateStatesToCM() {
	cmData := make(map[string]string)
	states.Range(func(k, v interface{}) bool {
		domain := k.(string)
		state := v.(*State)
		data, marshalErr := json.Marshal(state)
		if marshalErr != nil {
			logger.Error("Failed to marshal state for domain", zap.String("domain", domain), zap.Error(marshalErr))
			return true
		}
		cmData["state-"+domain] = string(data)
		return true
	})
	originalEndpoints.Range(func(k, v interface{}) bool {
		key := k.(string)
		cmData["original-"+key] = string(v.([]byte))
		return true
	})

	patch, marshalErr := json.Marshal(map[string]interface{}{"data": cmData})
	if marshalErr != nil {
		logger.Error("Failed to marshal ConfigMap patch data", zap.Error(marshalErr))
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, patchErr := clientset.CoreV1().ConfigMaps(programNamespace).Patch(context.Background(), stateConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
		return patchErr
	})
	if err != nil {
		logger.Error("Failed to update state ConfigMap after retries", zap.String("cm_name", stateConfigMap), zap.Error(err))
	} else {
		logger.Info("States successfully updated in ConfigMap",
			zap.Int("state_entries", len(cmData)),
			zap.String("cm_name", stateConfigMap))
	}
}

func watchConfigFile() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatal("Failed to create file system watcher", zap.Error(err))
	}
	defer watcher.Close()

	configDir := filepath.Dir(configPath)
	if err = watcher.Add(configDir); err != nil {
		logger.Fatal("Failed to add config directory to watcher", zap.String("dir", configDir), zap.Error(err))
	}
	htmlDir := filepath.Dir(htmlPath)
	if err = watcher.Add(htmlDir); err != nil {
		logger.Fatal("Failed to add HTML directory to watcher", zap.String("dir", htmlDir), zap.Error(err))
	}
	logger.Info("File watcher started for config and HTML directories", zap.String("config_dir", configDir), zap.String("html_dir", htmlDir))

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				logger.Info("File system event detected", zap.String("event", event.Op.String()), zap.String("file", event.Name))
				if strings.Contains(event.Name, "rules.yaml") {
					logger.Info("rules.yaml modified, reloading config...")
					loadConfig()
				}
				if strings.Contains(event.Name, "maintenance.html") {
					logger.Info("maintenance.html modified, reloading HTML template...")
					loadHTML()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Error("File system watcher error", zap.Error(err))
		}
	}
}

func watchOwnPods() {
	listWatch := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		programNamespace,
		fields.Everything(),
	)

	_, controller := cache.NewInformer(
		listWatch,
		&corev1.Pod{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { logger.Debug("Pod added event detected"); updatePodIPs() },
			UpdateFunc: func(oldObj, newObj interface{}) { logger.Debug("Pod updated event detected"); updatePodIPs() },
			DeleteFunc: func(obj interface{}) { logger.Debug("Pod deleted event detected"); updatePodIPs() },
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	logger.Info("Kubernetes Pod watcher started for traffic-switcher pods.")
	go controller.Run(stop)
	select {}
}

func updatePodIPs() {
	selector := labels.SelectorFromSet(labels.Set{"app": "traffic-switcher"}).String()
	pods, err := clientset.CoreV1().Pods(programNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		logger.Error("Failed to list traffic-switcher pods", zap.String("label_selector", selector), zap.Error(err))
		return
	}

	var ips []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}

	mu.Lock()
	currentPodIPs := podIPs
	podIPs = ips
	mu.Unlock()

	if !stringSliceEqual(currentPodIPs, ips) {
		logger.Info("Pod IPs updated", zap.Int("count", len(ips)), zap.Strings("new_ips", ips))
		logger.Debug("Pod IPs changed, re-patching switched services if any.")
		rePatchSwitchedSvcs()
	} else {
		logger.Debug("Pod IPs checked, no change detected.", zap.Int("count", len(ips)))
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int)
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}

func rePatchSwitchedSvcs() {
	logger.Info("Initiating re-patch for currently switched services.")
	mu.RLock()
	currentRules := rules
	mu.RUnlock()

	for _, rule := range currentRules {
		stateI, ok := states.Load(rule.Domain)
		if !ok {
			continue
		}
		state := stateI.(*State)
		if state.Status == "failed" && state.Confirmed {
			logger.Info("Re-patching service for confirmed failed domain", zap.String("domain", rule.Domain))
			switchToMaintenance(rule)
		}
	}
	logger.Info("Re-patch process completed.")
}

func monitorRule(ctx context.Context, rule Rule) { // rule 现在是副本
	failCount := 0
	recoveryCount := 0
	interval, err := time.ParseDuration(rule.CheckInterval)
	if err != nil {
		logger.Error("Failed to parse check interval for rule", zap.String("domain", rule.Domain), zap.String("interval_str", rule.CheckInterval), zap.Error(err))
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("Starting monitor for rule",
		zap.String("domain", rule.Domain),
		zap.String("check_url", rule.CheckURL),
		zap.String("check_condition", rule.CheckCondition),
		zap.Duration("interval", interval))

	for {
		select {
		case <-ctx.Done():
			logger.Info("Monitor for rule stopped due to context cancellation", zap.String("domain", rule.Domain))
			return
		case <-ticker.C:
			// 在每次循环开始时，从全局规则中获取当前域名的最新规则配置
			mu.RLock()
			var currentRule Rule
			found := false
			for _, r := range rules {
				if r.Domain == rule.Domain {
					currentRule = r
					found = true
					break
				}
			}
			mu.RUnlock()

			if !found {
				logger.Warn("Rule for domain no longer exists in config, stopping monitor.", zap.String("domain", rule.Domain))
				return // 规则被删除，退出监控goroutine
			}

			// 如果当前规则被标记为强制维护，则跳过健康检查
			if currentRule.ForceSwitch {
				logger.Debug("Domain under forced maintenance, skipping health check.", zap.String("domain", currentRule.Domain))
				continue
			}

			// 以下是正常模式下的健康检查和状态管理逻辑
			healthy := false
			for i := 0; i < 3; i++ {
				if checkURL(currentRule.CheckURL, currentRule.CheckCondition) {
					healthy = true
					break
				}
				logger.Debug("URL probe retry", zap.String("domain", currentRule.Domain), zap.String("url", currentRule.CheckURL), zap.Int("attempt", i+1))
				time.Sleep(1 * time.Second)
			}

			if healthy {
				probeSuccess.Set(1)
				logger.Debug("Probe successful", zap.String("domain", currentRule.Domain), zap.String("url", currentRule.CheckURL))
			} else {
				probeFailure.Inc()
				probeSuccess.Set(0)
				logger.Warn("Probe failed", zap.String("domain", currentRule.Domain), zap.String("url", currentRule.CheckURL))
			}

			stateI, ok := states.Load(currentRule.Domain)
			if !ok {
				logger.Error("State for domain not found in sync.Map, possibly a race condition or uninitialized. Skipping.", zap.String("domain", currentRule.Domain))
				continue
			}
			state := stateI.(*State)

			if !healthy {
				failCount++
				recoveryCount = 0
				logger.Debug("Domain failing", zap.String("domain", currentRule.Domain), zap.Int("fail_count", failCount), zap.Int("fail_threshold", currentRule.FailThreshold))

				if failCount >= currentRule.FailThreshold && state.Status == "normal" && !state.Notified {
					logger.Warn("Domain reached failure threshold, sending notification.",
						zap.String("domain", currentRule.Domain),
						zap.Int("fail_count", failCount),
						zap.Int("threshold", currentRule.FailThreshold))
					
					// 构建Inline Keyboard
					confirmBtn := tgbotapi.NewInlineKeyboardButtonData(globalAppConfig.TelegramTemplates.ConfirmButtonText, "confirm_"+currentRule.Domain)
					manualBtn := tgbotapi.NewInlineKeyboardButtonData(globalAppConfig.TelegramTemplates.ManualButtonText, "manual_"+currentRule.Domain)
					keyboard := tgbotapi.NewInlineKeyboardMarkup(
						tgbotapi.NewInlineKeyboardRow(confirmBtn, manualBtn),
					)
					sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.FaultMessage, currentRule.Domain), "Markdown", &keyboard)
					state.Notified = true
					updateStatesToCM()
				}
				if state.Confirmed {
					logger.Info("Domain confirmed for maintenance, switching to or ensuring maintenance mode.", zap.String("domain", currentRule.Domain))
					switchToMaintenance(currentRule)
					if state.Status != "failed" {
						state.Status = "failed"
						switchCount.Inc()
						logger.Info("Traffic successfully switched to maintenance page.", zap.String("domain", currentRule.Domain))
						updateStatesToCM()
					} else {
						logger.Debug("Domain already in failed state, maintenance mode ensured.", zap.String("domain", currentRule.Domain))
					}
				}
			} else {
				recoveryCount++
				failCount = 0
				logger.Debug("Domain healthy", zap.String("domain", currentRule.Domain), zap.Int("recovery_count", recoveryCount), zap.Int("recovery_threshold", currentRule.RecoveryThreshold))

				if recoveryCount >= currentRule.RecoveryThreshold && state.Status == "failed" {
					logger.Info("Domain reached recovery threshold, switching back to original service.",
						zap.String("domain", currentRule.Domain),
						zap.Int("recovery_count", recoveryCount),
						zap.Int("threshold", currentRule.RecoveryThreshold))
					switchBack(currentRule)
					state.Status = "normal"
					state.Notified = false
					state.Confirmed = false
					logger.Info("Traffic successfully switched back to original service.", zap.String("domain", currentRule.Domain))
					updateStatesToCM()
					if telegramChatID != 0 && globalAppConfig.TelegramTemplates.RecoveryMessage != "" {
						sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.RecoveryMessage, currentRule.Domain), "Markdown", nil)
					}
				}
			}
		}
	}
}

func checkURL(url string, condition string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		logger.Debug("URL probe failed with network error", zap.String("url", url), zap.Error(err))
		return false
	}
	defer resp.Body.Close()

	isHealthy := strings.Contains(condition, fmt.Sprintf("%d", resp.StatusCode))
	logger.Debug("URL probe completed",
		zap.String("url", url),
		zap.Int("status_code", resp.StatusCode),
		zap.String("expected_condition", condition),
		zap.Bool("is_healthy", isHealthy))

	return isHealthy
}

// sendTelegramMessage 通用函数，用于发送Telegram消息，包含重试逻辑
func sendTelegramMessage(chatID int64, text string, parseMode string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	if chatID == 0 {
		logger.Warn("Skipping Telegram message: Chat ID is not set.", zap.String("message_text", text))
		return
	}
	if appBotApi == nil {
		logger.Error("Skipping Telegram message: Bot API not initialized.", zap.String("message_text", text))
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = parseMode
	if keyboard != nil {
		msg.ReplyMarkup = keyboard
	}

	if _, err := appBotApi.Send(msg); err != nil {
		logger.Error("Initial Telegram message send failed, retrying...", zap.Error(err), zap.Int64("chat_id", chatID), zap.String("message", text))
		retryErr := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			if _, sendErr := appBotApi.Send(msg); sendErr != nil {
				logger.Warn("Telegram message retry failed", zap.Error(sendErr))
				return false, nil
			}
			return true, nil
		})
		if retryErr != nil && retryErr != context.DeadlineExceeded {
			logger.Error("Failed to send Telegram message after retries", zap.Error(retryErr), zap.Int64("chat_id", chatID))
		} else if retryErr == context.DeadlineExceeded {
			logger.Warn("Telegram message send timed out after retries", zap.Int64("chat_id", chatID))
		} else {
			logger.Info("Telegram message sent successfully after retries", zap.String("message", text))
		}
	} else {
		logger.Info("Telegram message sent immediately", zap.String("message", text))
	}
}

// sendTelegramNotification 是一个过时函数，调用通用的sendTelegramMessage
// 保持它只是为了兼容之前monitorRule里的调用，可以考虑直接替换掉
// 此函数会被monitorRule调用，如果不想看到弃用警告，可以直接在monitorRule中替换调用
func sendTelegramNotification(domain string) {
	logger.Debug("Deprecated sendTelegramNotification called. Use sendTelegramMessage directly.", zap.String("domain", domain))
	// 注意：这里无法传递键盘，因为老函数签名不支持。所以强烈建议直接替换monitorRule中的调用
	sendTelegramMessage(telegramChatID, fmt.Sprintf(globalAppConfig.TelegramTemplates.FaultMessage, domain), "Markdown", nil)
}


func switchToMaintenance(rule Rule) {
	mu.RLock()
	ips := podIPs
	mu.RUnlock()

	if len(ips) == 0 {
		logger.Warn("Cannot switch to maintenance: no traffic-switcher Pod IPs found. Ensure pods with label 'app=traffic-switcher' are running.", zap.String("domain", rule.Domain))
		return
	}

	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			fullSvcName := fmt.Sprintf("%s/%s", svcNS.Namespace, svc)
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			logger.Info("Attempting to switch service to maintenance mode",
				zap.String("domain", rule.Domain),
				zap.String("service", fullSvcName),
				zap.Strings("maintenance_ips", ips))

			ep, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Get(context.Background(), svc, metav1.GetOptions{})
			if err != nil {
				logger.Error("Failed to get Endpoints for service, skipping switch to maintenance.", zap.String("service", fullSvcName), zap.Error(err))
				continue
			}

			if _, loaded := originalEndpoints.Load(key); !loaded {
				original, marshalErr := json.Marshal(ep.Subsets)
				if marshalErr != nil {
					logger.Error("Failed to marshal original Endpoints subsets for service", zap.String("service", fullSvcName), zap.Error(marshalErr))
				} else {
					originalEndpoints.Store(key, original)
					logger.Debug("Original Endpoints saved for service", zap.String("service", fullSvcName))
				}
			}

			var addresses []corev1.EndpointAddress
			for _, ip := range ips {
				addresses = append(addresses, corev1.EndpointAddress{IP: ip})
			}
			var newSubsets []corev1.EndpointSubset
			if len(ep.Subsets) > 0 {
				newSubsets = []corev1.EndpointSubset{{
					Addresses: addresses,
					Ports:     ep.Subsets[0].Ports,
				}}
			} else {
				logger.Error("Service has no existing EndpointSubset, cannot determine ports for maintenance page. Please ensure service has endpoints before switching.", zap.String("service", fullSvcName))
				continue
			}

			patchData, marshalErr := json.Marshal(map[string]interface{}{"subsets": newSubsets})
			if marshalErr != nil {
				logger.Error("Failed to marshal patch data for maintenance switch", zap.String("service", fullSvcName), zap.Error(marshalErr))
				continue
			}

			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, patchErr := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return patchErr
			})
			if err != nil {
				logger.Error("Failed to patch Endpoints for service to maintenance mode after retries", zap.String("service", fullSvcName), zap.Error(err))
			} else {
				logger.Info("Service successfully switched to maintenance mode", zap.String("service", fullSvcName), zap.Strings("target_ips", ips))
			}
		}
	}
	updateStatesToCM()
}

func switchBack(rule Rule) {
	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			fullSvcName := fmt.Sprintf("%s/%s", svcNS.Namespace, svc)
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			logger.Info("Attempting to switch service back to original endpoints",
				zap.String("domain", rule.Domain),
				zap.String("service", fullSvcName))

			originalI, ok := originalEndpoints.LoadAndDelete(key)
			if !ok {
				logger.Warn("No original endpoints found in cache for service, cannot switch back. This might indicate an issue or that the service was never switched.", zap.String("service", fullSvcName))
				continue
			}
			original := originalI.([]byte)

			patchData, marshalErr := json.Marshal(map[string]interface{}{"subsets": json.RawMessage(original)})
			if marshalErr != nil {
				logger.Error("Failed to marshal patch data for reverting service", zap.String("service", fullSvcName), zap.Error(marshalErr))
				continue
			}

			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, patchErr := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return patchErr
			})
			if err != nil {
				logger.Error("Failed to revert Endpoints for service to original state after retries", zap.String("service", fullSvcName), zap.Error(err))
			} else {
				logger.Info("Service successfully reverted to original endpoints", zap.String("service", fullSvcName))
			}
		}
	}
	updateStatesToCM()
}

func maintenanceHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	tmpl := htmlTemplate
	mu.RUnlock()

	if tmpl == nil {
		logger.Error("Maintenance page template not loaded, serving generic error.", zap.String("host", r.Host))
		http.Error(w, "Maintenance page template not loaded", http.StatusInternalServerError)
		return
	}

	data := map[string]string{
		"Domain": r.Host,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		logger.Error("Failed to render maintenance page",
			zap.String("host", r.Host),
			zap.Error(err))
		http.Error(w, "Failed to render maintenance page", http.StatusInternalServerError)
		return
	}
	logger.Debug("Maintenance page served", zap.String("host", r.Host), zap.String("path", r.URL.Path))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	logger.Debug("Health check endpoint hit", zap.String("path", r.URL.Path))
}
