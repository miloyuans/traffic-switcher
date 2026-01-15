package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
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

type ServiceNS struct {
	Namespace string   `json:"namespace"`
	SvcNames  []string `json:"svc_names"`
}

type Config struct {
	Rules []Rule `json:"rules"`
}

type State struct {
	Status    string `json:"status"` // "normal" or "failed"
	Notified  bool   `json:"notified"`
	Confirmed bool   `json:"confirmed"`
}

var (
	configPath        = "/config/rules.yaml"
	htmlPath          = "/config/maintenance.html"
	telegramToken     = os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatIDStr = os.Getenv("TELEGRAM_CHAT_ID") // 修复：添加了缺失的括号
	telegramChatID    int64
	rules             []Rule
	states            sync.Map // domain -> *State
	podIPs            []string
	mu                sync.RWMutex
	clientset         *kubernetes.Clientset
	htmlTemplate      *template.Template
	originalEndpoints sync.Map // key: ns-svc, value: []byte (json subsets)
	maintenancePort   = 80
	logger            *zap.Logger
	probeSuccess      = prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_success_rate", Help: "URL probe success rate"})
	probeFailure      = prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_failure_count", Help: "Number of probe failures"})
	switchCount       = prometheus.NewCounter(prometheus.CounterOpts{Name: "switch_count", Help: "Number of traffic switches"})
	stateConfigMap    = "traffic-switch-states"    // Persistent state CM
	programNamespace  = os.Getenv("POD_NAMESPACE") // Set in Deployment
	programPodName    = os.Getenv("POD_NAME")
	leaderLeaseName   = "traffic-switcher-leader"
)

func init() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatalf("Failed to init zap: %v", err)
	}
	prometheus.MustRegister(probeSuccess, switchCount, probeFailure)
	if telegramChatIDStr != "" {
		telegramChatID, err = strconv.ParseInt(telegramChatIDStr, 10, 64)
		if err != nil {
			logger.Fatal("Invalid TELEGRAM_CHAT_ID format", zap.Error(err))
		}
	} else {
		logger.Warn("TELEGRAM_CHAT_ID is empty")
	}
	if programNamespace == "" {
		programNamespace = "default"
	}
}

func main() {
	defer logger.Sync()

	// Load k8s config
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := flag.String("kubeconfig", "", "kubeconfig path")
		flag.Parse()
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			logger.Fatal("Failed to build config", zap.Error(err))
		}
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal("Failed to create clientset", zap.Error(err))
	}

	// Load initial config and states
	loadConfig()
	loadHTML()
	loadStatesFromCM()

	// Start HTTP server for maintenance and webhook
	http.HandleFunc("/", maintenanceHandler)
	http.HandleFunc("/callback", telegramCallbackHandler)
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", healthHandler)
	go func() {
		if err := http.ListenAndServe(":8080", nil); err != nil {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	// Watch files
	go watchConfigFile()

	// Leader election
	rl, err := resourcelock.New(resourcelock.LeasesResourceLock,
		programNamespace,
		leaderLeaseName,
		clientset.CoreV1(),
		clientset.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: programPodName,
		})
	if err != nil {
		logger.Fatal("Failed to create lock", zap.Error(err))
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				logger.Info("Lost leader")
				os.Exit(0)
			},
		},
		Name: leaderLeaseName,
	})
	if err != nil {
		logger.Fatal("Failed leader election", zap.Error(err))
	}

	le.Run(context.Background())
}

func run(ctx context.Context) {
	// Watch own Pods
	go watchOwnPods()

	// ---------------------------------------------------------
	// 优化：Telegram 初始化与验证逻辑
	// ---------------------------------------------------------
	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		logger.Fatal("Failed to initialize Telegram Bot API. Check TELEGRAM_BOT_TOKEN.", zap.Error(err))
	}

	// 1. 验证 Token 有效性 (GetMe)
	botUser, err := bot.GetMe()
	if err != nil {
		logger.Fatal("Telegram Token is invalid or API is unreachable", zap.Error(err))
	}
	logger.Info("Telegram Bot connected successfully", zap.String("bot_username", botUser.UserName))

	// 2. 发送启动消息验证 ChatID 是否正确
	if telegramChatID != 0 {
		startupMsg := tgbotapi.NewMessage(telegramChatID, fmt.Sprintf("🚀 **Traffic Switcher Started**\n\nPod: `%s`\nNamespace: `%s`\nStatus: Leader Acquired", programPodName, programNamespace))
		startupMsg.ParseMode = "Markdown"
		_, err = bot.Send(startupMsg)
		if err != nil {
			logger.Error("Failed to send startup message. Check TELEGRAM_CHAT_ID or Bot permissions.",
				zap.Error(err),
				zap.Int64("chat_id", telegramChatID))
		} else {
			logger.Info("Telegram startup message sent successfully")
		}
	} else {
		logger.Warn("Skipping Telegram startup message: TELEGRAM_CHAT_ID not set")
	}
	// ---------------------------------------------------------

	// Start monitoring
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, rule := range rules {
		domain := rule.Domain
		if _, loaded := states.LoadOrStore(domain, &State{Status: "normal"}); !loaded {
			updateStatesToCM()
		}
		go monitorRule(cancelCtx, bot, rule)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("Shutting down")
	updateStatesToCM()
}

func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Error("Read config failed", zap.Error(err))
		return
	}
	var config Config
	if err = json.Unmarshal(data, &config); err != nil {
		logger.Error("Parse config failed", zap.Error(err))
		return
	}
	mu.Lock()
	rules = config.Rules
	mu.Unlock()
	logger.Info("Config loaded", zap.Int("rules", len(rules)))
}

func loadHTML() {
	tmpl, err := template.ParseFiles(htmlPath)
	if err != nil {
		logger.Error("Load HTML failed", zap.Error(err))
		return
	}
	mu.Lock()
	htmlTemplate = tmpl
	mu.Unlock()
	logger.Info("HTML loaded")
}

func loadStatesFromCM() {
	cm, err := clientset.CoreV1().ConfigMaps(programNamespace).Get(context.Background(), stateConfigMap, metav1.GetOptions{})
	if err != nil {
		logger.Info("No state CM, creating")
		// Create if not exist
		_, err = clientset.CoreV1().ConfigMaps(programNamespace).Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: stateConfigMap},
			Data:       make(map[string]string),
		}, metav1.CreateOptions{})
		if err != nil {
			logger.Error("Create state CM failed", zap.Error(err))
		}
		return
	}
	for k, v := range cm.Data {
		if strings.HasPrefix(k, "state-") {
			domain := strings.TrimPrefix(k, "state-")
			var state State
			json.Unmarshal([]byte(v), &state)
			states.Store(domain, &state)
		} else if strings.HasPrefix(k, "original-") {
			key := strings.TrimPrefix(k, "original-")
			originalEndpoints.Store(key, []byte(v))
		}
	}
	logger.Info("States loaded from CM", zap.Int("count", len(cm.Data)))
}

func updateStatesToCM() {
	cmData := make(map[string]string)
	states.Range(func(k, v interface{}) bool {
		domain := k.(string)
		state := v.(*State)
		data, _ := json.Marshal(state)
		cmData["state-"+domain] = string(data)
		return true
	})
	originalEndpoints.Range(func(k, v interface{}) bool {
		key := k.(string)
		cmData["original-"+key] = string(v.([]byte))
		return true
	})

	patch, _ := json.Marshal(map[string]interface{}{"data": cmData})
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := clientset.CoreV1().ConfigMaps(programNamespace).Patch(context.Background(), stateConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		logger.Error("Update state CM failed", zap.Error(err))
	}
}

func watchConfigFile() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatal("Watcher failed", zap.Error(err))
	}
	defer watcher.Close()

	err = watcher.Add(filepath.Dir(configPath))
	if err != nil {
		logger.Fatal("Add watcher failed", zap.Error(err))
	}
	err = watcher.Add(filepath.Dir(htmlPath))
	if err != nil {
		logger.Fatal("Add watcher failed", zap.Error(err))
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				if strings.Contains(event.Name, "rules.yaml") {
					loadConfig()
				}
				if strings.Contains(event.Name, "maintenance.html") {
					loadHTML()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Error("Watcher error", zap.Error(err))
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
			AddFunc:    func(obj interface{}) { updatePodIPs() },
			UpdateFunc: func(_, new interface{}) { updatePodIPs() },
			DeleteFunc: func(obj interface{}) { updatePodIPs() },
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)
	select {}
}

func updatePodIPs() {
	pods, err := clientset.CoreV1().Pods(programNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{"app": "traffic-switcher"}).String(),
	})
	if err != nil {
		logger.Error("List pods failed", zap.Error(err))
		return
	}

	var ips []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}

	mu.Lock()
	podIPs = ips
	mu.Unlock()

	logger.Info("Pod IPs updated", zap.Int("count", len(ips)))

	// Re-patch if needed
	rePatchSwitchedSvcs()
}

func rePatchSwitchedSvcs() {
	for _, rule := range rules {
		stateI, ok := states.Load(rule.Domain)
		if !ok {
			continue
		}
		state := stateI.(*State)
		if state.Status == "failed" && state.Confirmed {
			switchToMaintenance(rule)
		}
	}
}

func monitorRule(ctx context.Context, bot *tgbotapi.BotAPI, rule Rule) {
	failCount := 0
	recoveryCount := 0
	interval, err := time.ParseDuration(rule.CheckInterval)
	if err != nil {
		logger.Error("Parse interval failed", zap.Error(err))
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			healthy := false
			// 修复：使用简单的重试循环代替错误的 BackoffUntil 调用
			for i := 0; i < 3; i++ {
				if checkURL(rule.CheckURL, rule.CheckCondition) {
					healthy = true
					break
				}
				time.Sleep(1 * time.Second)
			}

			if healthy {
				probeSuccess.Set(1)
			} else {
				probeFailure.Inc()
				probeSuccess.Set(0)
			}

			stateI, ok := states.Load(rule.Domain)
			if !ok {
				continue
			}
			state := stateI.(*State)

			if !healthy {
				failCount++
				recoveryCount = 0
				if failCount >= rule.FailThreshold && state.Status == "normal" && !state.Notified {
					sendTelegramNotification(bot, rule.Domain)
					state.Notified = true
					updateStatesToCM()
				}
				if state.Confirmed {
					switchToMaintenance(rule)
					state.Status = "failed"
					switchCount.Inc()
					updateStatesToCM()
				}
			} else {
				recoveryCount++
				failCount = 0
				if recoveryCount >= rule.RecoveryThreshold && state.Status == "failed" {
					switchBack(rule)
					state.Status = "normal"
					state.Notified = false
					state.Confirmed = false
					updateStatesToCM()
				}
			}
		}
	}
}

func checkURL(url string, condition string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		logger.Warn("Probe failed", zap.Error(err))
		return false
	}
	defer resp.Body.Close()
	return strings.Contains(condition, fmt.Sprintf("%d", resp.StatusCode))
}

func sendTelegramNotification(bot *tgbotapi.BotAPI, domain string) {
	msg := tgbotapi.NewMessage(telegramChatID, fmt.Sprintf("🚨 **Domain Fault**: %s\n\nConfirm switch? /confirm_%s or /manual_%s", domain, domain, domain))
	msg.ParseMode = "Markdown"
	_, err := bot.Send(msg)
	if err != nil {
		logger.Error("Send notification failed", zap.Error(err))
		// Retry logic: 使用 Poll 确保消息发送
		wait.Poll(5*time.Second, 1*time.Minute, func() (bool, error) {
			_, err = bot.Send(msg)
			return err == nil, nil
		})
	}
	logger.Info("Notification sent", zap.String("domain", domain))
}

func telegramCallbackHandler(w http.ResponseWriter, r *http.Request) {
	var update tgbotapi.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		logger.Error("Webhook decode failed", zap.Error(err))
		return
	}
	if update.Message == nil {
		return
	}
	text := update.Message.Text
	if strings.HasPrefix(text, "/confirm_") {
		domain := strings.TrimPrefix(text, "/confirm_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			state.Confirmed = true
			updateStatesToCM()
			logger.Info("Confirmed switch", zap.String("domain", domain))
			
			// 反馈用户
			reply := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("✅ Switching traffic for %s to maintenance page.", domain))
			bot, _ := tgbotapi.NewBotAPI(telegramToken) // 这里为了简单重新创建bot实例，实际上最好传递进来
			if bot != nil { bot.Send(reply) }
		}
	} else if strings.HasPrefix(text, "/manual_") {
		domain := strings.TrimPrefix(text, "/manual_")
		stateI, ok := states.Load(domain)
		if ok {
			state := stateI.(*State)
			state.Confirmed = false
			state.Notified = false // Allow re-notify if needed
			updateStatesToCM()
			logger.Info("Manual mode", zap.String("domain", domain))
			
			// 反馈用户
			reply := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("🔧 Manual mode enabled for %s. Will re-notify on failure.", domain))
			bot, _ := tgbotapi.NewBotAPI(telegramToken)
			if bot != nil { bot.Send(reply) }
		}
	}
}

func switchToMaintenance(rule Rule) {
	mu.RLock()
	ips := podIPs
	mu.RUnlock()
	if len(ips) == 0 {
		logger.Warn("No IPs")
		return
	}

	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			ep, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Get(context.Background(), svc, metav1.GetOptions{})
			if err != nil {
				logger.Error("Get ep failed", zap.Error(err))
				continue
			}
			original, _ := json.Marshal(ep.Subsets)
			originalEndpoints.Store(key, original)

			var addresses []corev1.EndpointAddress
			for _, ip := range ips {
				addresses = append(addresses, corev1.EndpointAddress{IP: ip})
			}
			subsets := []corev1.EndpointSubset{{
				Addresses: addresses,
				Ports:     ep.Subsets[0].Ports, // Copy ports
			}}
			patchData, _ := json.Marshal(map[string]interface{}{"subsets": subsets})
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return err
			})
			if err != nil {
				logger.Error("Patch failed", zap.Error(err))
			} else {
				logger.Info("Switched", zap.String("svc", key))
			}
		}
	}
	updateStatesToCM()
}

func switchBack(rule Rule) {
	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			originalI, ok := originalEndpoints.LoadAndDelete(key)
			if !ok {
				logger.Warn("No original", zap.String("key", key))
				continue
			}
			original := originalI.([]byte)
			patchData, _ := json.Marshal(map[string]interface{}{"subsets": json.RawMessage(original)})
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return err
			})
			if err != nil {
				logger.Error("Revert failed", zap.Error(err))
			} else {
				logger.Info("Reverted", zap.String("svc", key))
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
		http.Error(w, "Maintenance page template not loaded", http.StatusInternalServerError)
		return
	}

	data := map[string]string{
		"Domain": r.Host,
	}

	if err := tmpl.Execute(w, data); err != nil {
		logger.Error("Failed to render maintenance page",
			zap.String("host", r.Host),
			zap.Error(err))
		http.Error(w, "Failed to render maintenance page", http.StatusInternalServerError)
		return
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
