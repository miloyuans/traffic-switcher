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
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

type Config struct {
	Telegram struct {
		Token  string `yaml:"token"`
		ChatID int64  `yaml:"chat_id"`
	} `yaml:"telegram"`

	HTTP struct {
		ListenAddr string `yaml:"listen_addr"` // 默认 "0.0.0.0"
		Port       string `yaml:"port"`        // 默认 "80"
	} `yaml:"http"`

	Maintenance struct {
		HTMLPath string `yaml:"html_path"` // 默认 "/config/maintenance.html"
	} `yaml:"maintenance"`

	Switch struct {
		ForceSwitch bool `yaml:"force_switch"` // 核心开关，默认 false

		MaintenanceNamespace string `yaml:"maintenance_namespace"` // 维护页服务所在的命名空间
		MaintenanceService   string `yaml:"maintenance_service"`   // 维护页服务名称

		Targets []struct {
			Namespace string   `yaml:"namespace"`
			Services  []string `yaml:"services"` // 支持多个 svc per namespace
		} `yaml:"targets"`
	} `yaml:"switch"`
}

var (
	configPath   = "/config/config.yaml"
	config       Config
	clientset    *kubernetes.Clientset
	bot          *tgbotapi.BotAPI
	mu           sync.RWMutex
	htmlTemplate *template.Template
	logger       = log.New(os.Stdout, "[traffic-switcher] ", log.LstdFlags)

	// 用于存储每个目标服务的原始 subsets
	originalSubsets sync.Map // key: "ns/svc" value: []corev1.EndpointSubset

	// 监控停止通道
	stopCh sync.Map // key: "ns/svc" → chan struct{}
)

func main() {
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.Parse()

	// 初始化 k8s client
	var err error
	var cfg *rest.Config
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		logger.Fatalf("Failed to get kubernetes config: %v", err)
	}

	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Failed to create kubernetes client: %v", err)
	}

	// 首次加载配置
	loadConfig()

	// 启动 http server
	go startHTTPServer()

	// 监听配置文件变化
	go watchConfig()

	// 等待系统信号退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	logger.Println("Shutting down...")
	// 停止所有监控
	stopCh.Range(func(k, v interface{}) bool {
		close(v.(chan struct{}))
		return true
	})
}

func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Printf("Failed to read config file: %v", err)
		return
	}
	var newConfig Config
	if err = yaml.Unmarshal(data, &newConfig); err != nil {
		logger.Printf("Failed to parse config file: %v", err)
		return
	}

	mu.Lock()
	oldForce := config.Switch.ForceSwitch
	config = newConfig
	mu.Unlock()

	// 默认值
	if config.HTTP.ListenAddr == "" {
		config.HTTP.ListenAddr = "0.0.0.0"
	}
	if config.HTTP.Port == "" {
		config.HTTP.Port = "80"
	}
	if config.Maintenance.HTMLPath == "" {
		config.Maintenance.HTMLPath = "/config/maintenance.html"
	}
	if config.Switch.MaintenanceNamespace == "" {
		config.Switch.MaintenanceNamespace = "default"
	}
	if config.Switch.MaintenanceService == "" {
		config.Switch.MaintenanceService = "traffic-switcher"
	}

	loadHTMLTemplate()

	// 初始化 Telegram Bot
	if config.Telegram.Token != "" && config.Telegram.ChatID != 0 {
		bot, err = tgbotapi.NewBotAPI(config.Telegram.Token)
		if err != nil {
			logger.Printf("Failed to initialize Telegram Bot: %v", err)
			bot = nil
		} else {
			logger.Printf("Telegram Bot initialized: @%s", bot.Self.UserName)
		}
	}

	shouldSwitch := config.Switch.ForceSwitch

	if shouldSwitch && !oldForce {
		logger.Println("Force switch enabled, starting switch to maintenance...")
		switchToMaintenance()
		sendTelegram("🚧 **Maintenance mode ON**")
	} else if !shouldSwitch && oldForce {
		logger.Println("Force switch disabled, recovering original...")
		recoverOriginal()
		sendTelegram("✅ **Maintenance mode OFF**")
	}
}

func loadHTMLTemplate() {
	mu.Lock()
	defer mu.Unlock()

	tmpl, err := template.ParseFiles(config.Maintenance.HTMLPath)
	if err != nil {
		logger.Printf("Failed to load HTML template: %v", err)
		htmlTemplate = nil
		return
	}
	htmlTemplate = tmpl
	logger.Println("HTML template loaded successfully")
}

func startHTTPServer() {
	http.HandleFunc("/", maintenanceHandler)
	addr := fmt.Sprintf("%s:%s", config.HTTP.ListenAddr, config.HTTP.Port)
	logger.Printf("HTTP server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		logger.Fatalf("HTTP server failed: %v", err)
	}
}

func maintenanceHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	tmpl := htmlTemplate
	mu.RUnlock()

	if tmpl == nil {
		http.Error(w, "Maintenance page not loaded", http.StatusInternalServerError)
		return
	}

	if err := tmpl.Execute(w, nil); err != nil {
		logger.Printf("Failed to execute template: %v", err)
		http.Error(w, "Template execution failed", http.StatusInternalServerError)
		return
	}
}

func watchConfig() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatalf("Failed to create watcher: %v", err)
	}
	defer watcher.Close()

	dir := filepath.Dir(configPath)
	if err := watcher.Add(dir); err != nil {
		logger.Fatalf("Failed to watch dir: %v", err)
	}
	logger.Printf("Watching dir: %s", dir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				logger.Printf("Detected event: %s on %s", event.Op, event.Name)
				if strings.HasSuffix(event.Name, "config.yaml") || strings.HasSuffix(event.Name, "maintenance.html") {
					loadConfig()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Printf("Watcher error: %v", err)
		}
	}
}

func switchToMaintenance() {
	// 获取维护页的完整 subsets
	maintenanceEp, err := clientset.CoreV1().Endpoints(config.Switch.MaintenanceNamespace).Get(context.Background(), config.Switch.MaintenanceService, metav1.GetOptions{})
	if err != nil {
		logger.Printf("Failed to get maintenance endpoints: %v", err)
		return
	}
	maintenanceSubsets := maintenanceEp.Subsets
	if len(maintenanceSubsets) == 0 {
		logger.Println("Maintenance endpoints have no subsets, cannot switch")
		return
	}
	logger.Printf("Maintenance subsets fetched (count: %d)", len(maintenanceSubsets))

	for _, targetGroup := range config.Switch.Targets {
		for _, svc := range targetGroup.Services {
			key := fmt.Sprintf("%s/%s", targetGroup.Namespace, svc)

			// 保存原始 subsets
			targetEp, err := clientset.CoreV1().Endpoints(targetGroup.Namespace).Get(context.Background(), svc, metav1.GetOptions{})
			if err != nil {
				logger.Printf("Failed to get target %s: %v", key, err)
				continue
			}
			if _, loaded := originalSubsets.Load(key); !loaded {
				originalSubsets.Store(key, targetEp.Subsets)
				logger.Printf("Original subsets saved for %s", key)
			}

			// 覆盖 subsets
			patchSubsets(targetGroup.Namespace, svc, maintenanceSubsets)

			// 启动监控
			ch := make(chan struct{})
			stopCh.Store(key, ch)
			go monitorEndpoints(targetGroup.Namespace, svc, maintenanceSubsets, ch)
		}
	}
}

func recoverOriginal() {
	for _, targetGroup := range config.Switch.Targets {
		for _, svc := range targetGroup.Services {
			key := fmt.Sprintf("%s/%s", targetGroup.Namespace, svc)

			// 停止监控
			if chI, loaded := stopCh.LoadAndDelete(key); loaded {
				close(chI.(chan struct{}))
				logger.Printf("Monitor stopped for %s", key)
			}

			// 恢复原始
			if raw, loaded := originalSubsets.LoadAndDelete(key); loaded {
				subsets := raw.([]corev1.EndpointSubset)
				patchSubsets(targetGroup.Namespace, svc, subsets)
			} else {
				logger.Printf("No original subsets for %s, skip recovery", key)
			}
		}
	}
}

func patchSubsets(namespace, svc string, subsets []corev1.EndpointSubset) {
	patchData, err := json.Marshal(map[string]interface{}{"subsets": subsets})
	if err != nil {
		logger.Printf("Marshal failed for %s/%s: %v", namespace, svc, err)
		return
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := clientset.CoreV1().Endpoints(namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		logger.Printf("Patch failed for %s/%s: %v", namespace, svc, err)
	} else {
		logger.Printf("Patch success for %s/%s", namespace, svc)
	}
}

func monitorEndpoints(namespace, svc string, desired []corev1.EndpointSubset, stop chan struct{}) {
	logger.Printf("Monitor started for %s/%s", namespace, svc)

	for {
		select {
		case <-stop:
			logger.Printf("Monitor stopped for %s/%s", namespace, svc)
			return
		default:
			watcher, err := clientset.CoreV1().Endpoints(namespace).Watch(context.Background(), metav1.ListOptions{
				FieldSelector: fmt.Sprintf("metadata.name=%s", svc),
			})
			if err != nil {
				logger.Printf("Watch init failed for %s/%s: %v, retry in 5s", namespace, svc, err)
				time.Sleep(5 * time.Second)
				continue
			}

			for event := range watcher.ResultChan() {
				if event.Type == watch.Modified {
					ep := event.Object.(*corev1.Endpoints)
					if !reflect.DeepEqual(ep.Subsets, desired) {
						logger.Printf("Change detected in %s/%s, re-patching", namespace, svc)
						patchSubsets(namespace, svc, desired)
					}
				}
			}
			watcher.Stop()
			logger.Printf("Watch channel closed for %s/%s, restarting", namespace, svc)
			time.Sleep(5 * time.Second)
		}
	}
}

func sendTelegram(msg string) {
	if bot == nil {
		return
	}
	sendMsg := tgbotapi.NewMessage(config.Telegram.ChatID, msg)
	sendMsg.ParseMode = "Markdown"
	if _, err := bot.Send(sendMsg); err != nil {
		logger.Printf("Telegram send failed: %v", err)
	} else {
		logger.Printf("Telegram sent: %s", msg)
	}
}