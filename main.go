package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log" // 依然需要，因为zap的初始化需要
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields" // 修复：导入 fields 包
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
	StartupMessage  string `json:"startup_message"`  // 服务启动时的消息
	FaultMessage    string `json:"fault_message"`    // 探测到故障时的通知
	ConfirmReply    string `json:"confirm_reply"`    // 确认切换后的回复
	ManualReply     string `json:"manual_reply"`     // 手动模式切换后的回复
	RecoveryMessage string `json:"recovery_message"` // 故障恢复后的通知
}

// GlobalConfig 定义了应用程序的全局配置，包括HTTP监听和Telegram模板
type GlobalConfig struct {
	HTTPListenAddr    string            `json:"http_listen_addr"`
	HTTPListenPort    string            `json:"http_listen_port"`
	TelegramTemplates TelegramTemplates `json:"telegram_templates"`
}

// Rule 定义了单个域名/服务的切换规则
type Rule struct {
	Domain            string      `json:"domain"`
	CheckURL          string      `json:"check_url"`
	CheckCondition    string      `json:"check_condition"`
	FailThreshold     int         `json:"fail_threshold"`
	RecoveryThreshold int         `json:"recovery_threshold"`
	CheckInterval     string      `json:"check_interval"`
	MaintenanceLabel  string      `json:"maintenance_pod_label"`
	Services          []ServiceNS `json:"services"`
}

// ServiceNS 定义了服务及其所属的命名空间
type ServiceNS struct {
	Namespace string   `json:"namespace"`
	SvcNames  []string `json:"svc_names"`
}

// Config 包含了所有规则和全局配置
type Config struct {
	Global GlobalConfig `json:"global_config"`
	Rules  []Rule       `json:"rules"`
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
	rules             []Rule
	states            sync.Map // domain -> *State
	podIPs            []string
	mu                sync.RWMutex
	clientset         *kubernetes.Clientset
	htmlTemplate      *template.Template
	originalEndpoints sync.Map // key: ns-svc, value: []byte (json subsets)
	maintenancePort   = 80 // Default, but can be overridden by rule label or future config
	logger            *zap.Logger
	probeSuccess      = prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_success_rate", Help: "URL probe success rate"})
	probeFailure      = prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_failure_count", Help: "Number of probe failures"})
	switchCount       = prometheus.NewCounter(prometheus.CounterOpts{Name: "switch_count", Help: "Number of traffic switches"})
	stateConfigMap    = "traffic-switch-states"    // Persistent state CM
	programNamespace  = os.Getenv("POD_NAMESPACE") // Set in Deployment
	programPodName    = os.Getenv("POD_NAME")
	leaderLeaseName   = "traffic-switcher-leader"

	appConfig tgbotapi.BotAPI // 全局Bot实例
	// 用于存储加载的全局配置，便于在各个函数中使用
	globalAppConfig struct {
		HTTPListenAddr    string
		HTTPListenPort    string
		TelegramTemplates TelegramTemplates
	}
)

func init() {
	var err error
	// 使用Zap Logger，生产环境配置
	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatalf("Failed to init zap: %v", err)
	}
	// 注册Prometheus指标
	prometheus.MustRegister(probeSuccess, switchCount, probeFailure)

	// 解析Telegram Chat ID
	if telegramChatIDStr != "" {
		telegramChatID, err = strconv.ParseInt(telegramChatIDStr, 10, 64)
		if err != nil {
			logger.Fatal("Invalid TELEGRAM_CHAT_ID format, please provide a valid integer chat ID.", zap.Error(err))
		}
		logger.Info("Telegram chat ID configured", zap.Int64("chat_id", telegramChatID))
	} else {
		logger.Warn("TELEGRAM_CHAT_ID environment variable is empty. Telegram notifications will be disabled.")
	}

	// 设置程序运行的命名空间，如果环境变量未设置则默认为"default"
	if programNamespace == "" {
		programNamespace = "default"
		logger.Info("POD_NAMESPACE not set, defaulting to 'default' namespace.")
	}
}

func main() {
	defer logger.Sync() // 确保所有缓冲的日志都被刷新

	// 加载Kubernetes配置
	config, err := rest.InClusterConfig() // 尝试在集群内加载配置
	if err != nil {
		// 如果在集群内加载失败，尝试从kubeconfig文件加载
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

	// 加载初始配置和状态
	loadConfig()
	loadHTML()
	loadStatesFromCM()

	// 启动HTTP服务器用于维护页和webhook
	httpListenAddr := fmt.Sprintf("%s:%s", globalAppConfig.HTTPListenAddr, globalAppConfig.HTTPListenPort)
	http.HandleFunc("/", maintenanceHandler)
	http.HandleFunc("/callback", telegramCallbackHandler)
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", healthHandler)
	go func() {
		logger.Info("Starting HTTP server", zap.String("listen_address", httpListenAddr))
		if err := http.ListenAndServe(httpListenAddr, nil); err != nil {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	// 启动配置文件监听
	go watchConfigFile()

	// Leader选举
	rl, err := resourcelock.New(resourcelock.LeasesResourceLock, // 修复：使用正确的常量名
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
			OnStartedLeading: run, // 成为Leader后执行run函数
			OnStoppedLeading: func() {
				logger.Info("Lost leadership, shutting down.")
				os.Exit(0) // 失去Leader权限则退出
			},
		},
		Name: leaderLeaseName,
	})
	if err != nil {
		logger.Fatal("Failed to create leader elector.", zap.Error(err))
	}

	le.Run(context.Background()) // 开始Leader选举
}

// run 在成为Leader后执行，负责核心业务逻辑
func run(ctx context.Context) {
	logger.Info("Successfully acquired leadership, starting core operations.")

	// 监听自身Pod的变化，更新维护IP列表
	go watchOwnPods()

	// 初始化Telegram Bot API
	var err error
	appConfig, err = tgbotapi.NewBotAPI(telegramToken) // 使用全局bot实例
	if err != nil {
		logger.Fatal("Failed to initialize Telegram Bot API. Please ensure TELEGRAM_BOT_TOKEN is valid.", zap.Error(err))
	}

	// 验证Telegram Token有效性
	botUser, err := appConfig.GetMe()
	if err != nil {
		logger.Fatal("Telegram Token is invalid or API is unreachable. Cannot get bot information.", zap.Error(err))
	}
	logger.Info("Telegram Bot connected successfully", zap.String("bot_username", botUser.UserName))

	// 如果配置了Telegram Chat ID，发送启动消息
	if telegramChatID != 0 && globalAppConfig.TelegramTemplates.StartupMessage != "" {
		startupMsgText := fmt.Sprintf(globalAppConfig.TelegramTemplates.StartupMessage, programPodName, programNamespace)
		startupMsg := tgbotapi.NewMessage(telegramChatID, startupMsgText)
		startupMsg.ParseMode = "Markdown"
		_, err = appConfig.Send(startupMsg)
		if err != nil {
			logger.Error("Failed to send Telegram startup message. Check TELEGRAM_CHAT_ID and Bot permissions.",
				zap.Error(err),
				zap.Int64("chat_id", telegramChatID),
				zap.String("message", startupMsgText))
		} else {
			logger.Info("Telegram startup message sent successfully", zap.String("message", startupMsgText))
		}
	} else {
		logger.Warn("Skipping Telegram startup message: TELEGRAM_CHAT_ID not set or template is empty.")
	}

	// 开始监控所有规则
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if len(rules) == 0 {
		logger.Warn("No rules loaded from config. Monitoring will not start.")
	}

	for _, rule := range rules {
		domain := rule.Domain
		// 初始化域名状态，如果不存在则设为正常
		if _, loaded := states.LoadOrStore(domain, &State{Status: "normal"}); !loaded {
			logger.Info("Initialized state for new domain", zap.String("domain", domain), zap.String("status", "normal"))
			updateStatesToCM() // 新增状态时更新CM
		}
		go monitorRule(cancelCtx, rule) // 为每个规则启动独立的监控goroutine
	}

	// 监听系统中断信号，优雅关闭
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("Shutting down application due to signal.")
	updateStatesToCM() // 关闭前保存所有状态
}

// loadConfig 从配置文件加载规则和全局配置
func loadConfig() {
	data, err := os.ReadFile(configPath) // 修复：使用os.ReadFile
	if err != nil {
		logger.Error("Failed to read config file", zap.String("path", configPath), zap.Error(err))
		return
	}
	var config Config
	if err = json.Unmarshal(data, &config); err != nil {
		logger.Error("Failed to parse config file (JSON format expected)", zap.String("path", configPath), zap.Error(err))
		return
	}

	// 更新全局配置
	globalAppConfig.HTTPListenAddr = config.Global.HTTPListenAddr
	globalAppConfig.HTTPListenPort = config.Global.HTTPListenPort
	globalAppConfig.TelegramTemplates = config.Global.TelegramTemplates

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
		globalAppConfig.TelegramTemplates.FaultMessage = "🚨 **Domain Fault Detected!**\n\nDomain: `%s` is failing.\n\n_Auto-switch will happen if confirmed._\n\nConfirm switch to maintenance? /confirm_%s or /manual_%s"
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

	mu.Lock()
	rules = config.Rules // 更新规则
	mu.Unlock()
	logger.Info("Config loaded successfully",
		zap.Int("rules_count", len(rules)),
		zap.String("http_listen_addr", globalAppConfig.HTTPListenAddr),
		zap.String("http_listen_port", globalAppConfig.HTTPListenPort),
		zap.Bool("telegram_templates_loaded", globalAppConfig.TelegramTemplates.FaultMessage != ""))
}

// loadHTML 加载维护页面的HTML模板
func loadHTML() {
	tmpl, err := template.ParseFiles(htmlPath)
	if err != nil {
		logger.Error("Failed to load maintenance HTML template", zap.String("path", htmlPath), zap.Error(err))
		return
	}
	mu.Lock()
	htmlTemplate = tmpl
	mu.Unlock()
	logger.Info("Maintenance HTML template loaded successfully", zap.String("path", htmlPath))
}

// loadStatesFromCM 从ConfigMap加载持久化的状态
func loadStatesFromCM() {
	cm, err := clientset.CoreV1().ConfigMaps(programNamespace).Get(context.Background(), stateConfigMap, metav1.GetOptions{})
	if err != nil {
		logger.Warn("State ConfigMap not found, attempting to create it.", zap.String("cm_name", stateConfigMap), zap.Error(err))
		// 如果ConfigMap不存在，则创建
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

// updateStatesToCM 将当前内存中的状态持久化到ConfigMap
func updateStatesToCM() {
	cmData := make(map[string]string)
	states.Range(func(k, v interface{}) bool {
		domain := k.(string)
		state := v.(*State)
		data, marshalErr := json.Marshal(state)
		if marshalErr != nil {
			logger.Error("Failed to marshal state for domain", zap.String("domain", domain), zap.Error(marshalErr))
			return true // 继续处理下一个
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

// watchConfigFile 监听配置文件和HTML模板文件的变化，并重新加载
func watchConfigFile() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatal("Failed to create file system watcher", zap.Error(err))
	}
	defer watcher.Close()

	// 监听配置文件目录
	configDir := filepath.Dir(configPath)
	if err = watcher.Add(configDir); err != nil {
		logger.Fatal("Failed to add config directory to watcher", zap.String("dir", configDir), zap.Error(err))
	}
	// 监听HTML模板文件目录
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
			// 只处理写入事件
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

// watchOwnPods 监听自身Pod的变化，以更新维护模式下的IP列表
func watchOwnPods() {
	// 创建ListWatch，过滤条件为当前命名空间下label为"app=traffic-switcher"的Pod
	// 这里使用 fields.Everything() 作为 PodListOptions 的 FieldSelector 是不准确的，
	// 应该使用 label selector。但在 NewListWatchFromClient 中，FieldSelector 是用于选择资源的字段，
	// 而 LabelSelector 通常在 ListOptions 中使用。这里结合 informer 的设计，通常不需要直接在 ListWatch 中设置过细的 LabelSelector，
	// 而是在后续的 ListOptions 中应用。
	listWatch := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		programNamespace,
		fields.Everything(), // 这里的 fields.Everything() 是可以的，因为 LabelSelector 会在 ListOptions 中应用
	)

	// 创建Informer，监听Pod的Add/Update/Delete事件
	_, controller := cache.NewInformer(
		listWatch,
		&corev1.Pod{},
		0, // 重新同步周期，0表示不定期
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { logger.Debug("Pod added event detected"); updatePodIPs() },
			UpdateFunc: func(oldObj, newObj interface{}) { logger.Debug("Pod updated event detected"); updatePodIPs() },
			DeleteFunc: func(obj interface{}) { logger.Debug("Pod deleted event detected"); updatePodIPs() },
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	logger.Info("Kubernetes Pod watcher started for traffic-switcher pods.")
	go controller.Run(stop) // 运行控制器
	select {}               // 阻塞当前goroutine，使其永不退出
}

// updatePodIPs 获取所有打有"app=traffic-switcher"标签的Pod的IP
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
		// 只选择处于Running状态且有IP的Pod
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}

	mu.Lock()
	currentPodIPs := podIPs
	podIPs = ips
	mu.Unlock()

	// 只有IP列表实际发生变化才记录Info日志，避免日志刷屏
	if !stringSliceEqual(currentPodIPs, ips) {
		logger.Info("Pod IPs updated", zap.Int("count", len(ips)), zap.Strings("new_ips", ips))
		// 如果IP列表发生变化，重新patch已切换到维护模式的服务
		logger.Debug("Pod IPs changed, re-patching switched services if any.")
		rePatchSwitchedSvcs()
	} else {
		logger.Debug("Pod IPs checked, no change detected.", zap.Int("count", len(ips)))
	}
}

// 辅助函数，比较两个字符串切片是否相等（不考虑顺序）
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

// rePatchSwitchedSvcs 重新patch所有已处于"failed"且"confirmed"状态的服务，
// 用于Pod IP变化后更新维护页面的目标IP
func rePatchSwitchedSvcs() {
	logger.Info("Initiating re-patch for currently switched services.")
	mu.RLock() // 读锁，因为只读rules
	currentRules := rules
	mu.RUnlock()

	for _, rule := range currentRules {
		stateI, ok := states.Load(rule.Domain)
		if !ok {
			continue // 该域名没有状态，跳过
		}
		state := stateI.(*State)
		if state.Status == "failed" && state.Confirmed {
			logger.Info("Re-patching service for confirmed failed domain", zap.String("domain", rule.Domain))
			switchToMaintenance(rule) // 重新执行切换逻辑，更新endpoints的IPs
		}
	}
	logger.Info("Re-patch process completed.")
}

// monitorRule 针对单个规则进行周期性健康检查和状态管理
func monitorRule(ctx context.Context, rule Rule) {
	failCount := 0    // 连续失败计数
	recoveryCount := 0 // 连续恢复计数
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
			// 执行URL探测，最多重试3次，每次间隔1秒
			healthy := false
			for i := 0; i < 3; i++ {
				if checkURL(rule.CheckURL, rule.CheckCondition) {
					healthy = true
					break
				}
				time.Sleep(1 * time.Second) // 重试间隔
			}

			// 更新Prometheus指标
			if healthy {
				probeSuccess.Set(1)
				logger.Debug("Probe successful", zap.String("domain", rule.Domain), zap.String("url", rule.CheckURL))
			} else {
				probeFailure.Inc()
				probeSuccess.Set(0)
				logger.Warn("Probe failed", zap.String("domain", rule.Domain), zap.String("url", rule.CheckURL))
			}

			stateI, ok := states.Load(rule.Domain)
			if !ok {
				logger.Error("State for domain not found in sync.Map, possibly a race condition or uninitialized. Skipping.", zap.String("domain", rule.Domain))
				continue
			}
			state := stateI.(*State)

			// 处理探测结果
			if !healthy {
				failCount++
				recoveryCount = 0 // 失败则重置恢复计数
				logger.Debug("Domain failing", zap.String("domain", rule.Domain), zap.Int("fail_count", failCount), zap.Int("fail_threshold", rule.FailThreshold))

				// 达到失败阈值且状态正常，未通知过
				if failCount >= rule.FailThreshold && state.Status == "normal" && !state.Notified {
					logger.Warn("Domain reached failure threshold, sending notification.",
						zap.String("domain", rule.Domain),
						zap.Int("fail_count", failCount),
						zap.Int("threshold", rule.FailThreshold))
					sendTelegramNotification(rule.Domain)
					state.Notified = true // 标记为已通知
					updateStatesToCM()
				}
				// 已经确认切换，则执行或保持切换到维护模式
				if state.Confirmed {
					logger.Info("Domain confirmed for maintenance, switching to or ensuring maintenance mode.", zap.String("domain", rule.Domain))
					switchToMaintenance(rule)
					if state.Status != "failed" { // 只有在状态改变时才更新并计数
						state.Status = "failed"
						switchCount.Inc()
						logger.Info("Traffic successfully switched to maintenance page.", zap.String("domain", rule.Domain))
						updateStatesToCM()
					} else {
						logger.Debug("Domain already in failed state, maintenance mode ensured.", zap.String("domain", rule.Domain))
					}
				}
			} else {
				recoveryCount++
				failCount = 0 // 恢复则重置失败计数
				logger.Debug("Domain healthy", zap.String("domain", rule.Domain), zap.Int("recovery_count", recoveryCount), zap.Int("recovery_threshold", rule.RecoveryThreshold))

				// 达到恢复阈值且状态为故障
				if recoveryCount >= rule.RecoveryThreshold && state.Status == "failed" {
					logger.Info("Domain reached recovery threshold, switching back to original service.",
						zap.String("domain", rule.Domain),
						zap.Int("recovery_count", recoveryCount),
						zap.Int("threshold", rule.RecoveryThreshold))
					switchBack(rule)
					state.Status = "normal"
					state.Notified = false  // 恢复后重置通知状态
					state.Confirmed = false // 恢复后重置确认状态
					logger.Info("Traffic successfully switched back to original service.", zap.String("domain", rule.Domain))
					updateStatesToCM()
					// 发送恢复通知
					if telegramChatID != 0 && globalAppConfig.TelegramTemplates.RecoveryMessage != "" {
						recoveryMsgText := fmt.Sprintf(globalAppConfig.TelegramTemplates.RecoveryMessage, rule.Domain)
						recoveryMsg := tgbotapi.NewMessage(telegramChatID, recoveryMsgText)
						recoveryMsg.ParseMode = "Markdown"
						_, sendErr := appConfig.Send(recoveryMsg)
						if sendErr != nil {
							logger.Error("Failed to send Telegram recovery message.", zap.Error(sendErr), zap.String("domain", rule.Domain))
						} else {
							logger.Info("Telegram recovery message sent.", zap.String("domain", rule.Domain), zap.String("message", recoveryMsgText))
						}
					}
				}
			}
		}
	}
}

// checkURL 执行HTTP健康检查
func checkURL(url string, condition string) bool {
	client := &http.Client{Timeout: 5 * time.Second} // 5秒超时
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

// sendTelegramNotification 发送Telegram故障通知
func sendTelegramNotification(domain string) {
	if telegramChatID == 0 || globalAppConfig.TelegramTemplates.FaultMessage == "" {
		logger.Warn("Skipping Telegram notification: Chat ID not set or fault message template is empty.", zap.String("domain", domain))
		return
	}
	notificationText := fmt.Sprintf(globalAppConfig.TelegramTemplates.FaultMessage, domain, domain, domain)
	msg := tgbotapi.NewMessage(telegramChatID, notificationText)
	msg.ParseMode = "Markdown" // 启用Markdown格式

	_, err := appConfig.Send(msg) // 使用全局bot实例
	if err != nil {
		logger.Error("Failed to send Telegram notification after multiple retries", zap.Error(err), zap.String("domain", domain), zap.String("message", notificationText))
	} else {
		logger.Info("Telegram notification sent", zap.String("domain", domain), zap.String("message", notificationText))
	}
}

// telegramCallbackHandler 处理Telegram的webhook回调
func telegramCallbackHandler(w http.ResponseWriter, r *http.Request) {
	var update tgbotapi.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		logger.Error("Failed to decode Telegram webhook update", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	// 确保是消息更新并且有文本内容
	if update.Message == nil || update.Message.Text == "" {
		logger.Debug("Received non-message or empty message Telegram update, ignoring.")
		return
	}

	text := update.Message.Text
	chatID := update.Message.Chat.ID
	logger.Info("Received Telegram message", zap.String("from", update.Message.From.UserName), zap.String("text", text), zap.Int64("chat_id", chatID))

	var replyText string
	if strings.HasPrefix(text, "/confirm_") {
		domain := strings.TrimPrefix(text, "/confirm_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			if state.Status == "normal" {
				replyText = fmt.Sprintf("ℹ️ Domain `%s` is currently healthy. No action needed.", domain)
			} else {
				state.Confirmed = true
				updateStatesToCM()
				logger.Info("Traffic switch confirmed by Telegram user", zap.String("domain", domain), zap.String("user", update.Message.From.UserName))
				replyText = fmt.Sprintf(globalAppConfig.TelegramTemplates.ConfirmReply, domain)
				// 立即触发一次切换，而不是等待下一个监控周期
				if state.Status == "failed" { // 只有在已检测到故障时才立即切换
					mu.RLock()
					currentRules := rules
					mu.RUnlock()
					for _, rule := range currentRules {
						if rule.Domain == domain {
							switchToMaintenance(rule)
							break
						}
					}
				}
			}
		} else {
			replyText = fmt.Sprintf("⚠️ No active rule found for domain: `%s`.", domain)
		}
	} else if strings.HasPrefix(text, "/manual_") {
		domain := strings.TrimPrefix(text, "/manual_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			state.Confirmed = false
			state.Notified = false // 允许重新通知
			updateStatesToCM()
			logger.Info("Manual mode enabled by Telegram user", zap.String("domain", domain), zap.String("user", update.Message.From.UserName))
			replyText = fmt.Sprintf(globalAppConfig.TelegramTemplates.ManualReply, domain)
			// 如果服务已在维护模式，则回切
			if state.Status == "failed" {
				mu.RLock()
				currentRules := rules
				mu.RUnlock()
				for _, rule := range currentRules {
					if rule.Domain == domain {
						switchBack(rule)
						break
					}
				}
			}
		} else {
			replyText = fmt.Sprintf("⚠️ No active rule found for domain: `%s`.", domain)
		}
	} else {
		replyText = "Hello! I am Traffic Switcher bot. You can interact with me to manage traffic for your domains. Try `/confirm_yourdomain` or `/manual_yourdomain`."
	}

	// 回复Telegram用户
	if replyText != "" {
		replyMsg := tgbotapi.NewMessage(chatID, replyText)
		replyMsg.ParseMode = "Markdown"
		_, err := appConfig.Send(replyMsg) // 使用全局bot实例
		if err != nil {
			logger.Error("Failed to send Telegram reply message", zap.Error(err), zap.Int64("chat_id", chatID), zap.String("reply_text", replyText))
		} else {
			logger.Info("Telegram reply sent", zap.Int64("chat_id", chatID), zap.String("reply_text", replyText))
		}
	}
	w.WriteHeader(http.StatusOK) // 总是返回200 OK给Telegram
}

// switchToMaintenance 将服务的Endpoints指向维护Pod的IP
func switchToMaintenance(rule Rule) {
	mu.RLock()
	ips := podIPs // 获取当前所有traffic-switcher Pod的IPs
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

			// 如果原始Endpoints尚未保存，则保存一份
			if _, loaded := originalEndpoints.Load(key); !loaded {
				original, marshalErr := json.Marshal(ep.Subsets)
				if marshalErr != nil {
					logger.Error("Failed to marshal original Endpoints subsets for service", zap.String("service", fullSvcName), zap.Error(marshalErr))
					// 继续执行，但不保存原始Endpoints可能会导致回切失败
				} else {
					originalEndpoints.Store(key, original)
					logger.Debug("Original Endpoints saved for service", zap.String("service", fullSvcName))
				}
			}

			// 构建指向维护Pod的Endpoints
			var addresses []corev1.EndpointAddress
			for _, ip := range ips {
				addresses = append(addresses, corev1.EndpointAddress{IP: ip})
			}
			// 假设所有服务都有至少一个EndpointSubset，并且端口配置是通用的
			// 注意：这里简单复制第一个Subset的端口，实际可能需要更复杂的端口映射逻辑
			var newSubsets []corev1.EndpointSubset
			if len(ep.Subsets) > 0 {
				newSubsets = []corev1.EndpointSubset{{
					Addresses: addresses,
					Ports:     ep.Subsets[0].Ports, // 复制第一个子集的端口
				}}
			} else {
				// 如果没有现有端口，无法确定维护页面的端口，这是一个潜在问题
				logger.Error("Service has no existing EndpointSubset, cannot determine ports for maintenance page.", zap.String("service", fullSvcName))
				continue
			}

			patchData, marshalErr := json.Marshal(map[string]interface{}{"subsets": newSubsets})
			if marshalErr != nil {
				logger.Error("Failed to marshal patch data for maintenance switch", zap.String("service", fullSvcName), zap.Error(marshalErr))
				continue
			}

			// 使用重试机制更新Endpoints
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
	updateStatesToCM() // 更新状态到ConfigMap，包括保存的原始Endpoints
}

// switchBack 将服务的Endpoints恢复到原始状态
func switchBack(rule Rule) {
	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			fullSvcName := fmt.Sprintf("%s/%s", svcNS.Namespace, svc)
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			logger.Info("Attempting to switch service back to original endpoints",
				zap.String("domain", rule.Domain),
				zap.String("service", fullSvcName))

			originalI, ok := originalEndpoints.LoadAndDelete(key) // 获取并移除原始Endpoints
			if !ok {
				logger.Warn("No original endpoints found in cache for service, cannot switch back.", zap.String("service", fullSvcName))
				continue
			}
			original := originalI.([]byte) // 原始Endpoints的JSON字节数据

			// 构建patch数据，恢复原始subsets
			patchData, marshalErr := json.Marshal(map[string]interface{}{"subsets": json.RawMessage(original)})
			if marshalErr != nil {
				logger.Error("Failed to marshal patch data for reverting service", zap.String("service", fullSvcName), zap.Error(marshalErr))
				continue
			}

			// 使用重试机制更新Endpoints
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
	updateStatesToCM() // 更新状态到ConfigMap
}

// maintenanceHandler 处理HTTP请求，返回维护页面
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
		"Domain": r.Host, // 动态显示访问的域名
		// 可以添加更多动态数据
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

// healthHandler 提供健康检查端点
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	logger.Debug("Health check endpoint hit", zap.String("path", r.URL.Path))
}
