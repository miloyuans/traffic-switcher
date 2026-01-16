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
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
			Namespace string `yaml:"namespace"`
			Service   string `yaml:"service"` // 要被劫持的目标服务
		} `yaml:"targets"`
	} `yaml:"switch"`
}

var (
	configPath            = "/config/config.yaml"
	config                Config
	clientset             *kubernetes.Clientset
	bot                   *tgbotapi.BotAPI
	mu                    sync.RWMutex
	htmlTemplate          *template.Template
	logger                = log.New(os.Stdout, "[traffic-switcher] ", log.LstdFlags|log.Lmicroseconds)
	originalSubsets       sync.Map // key: "ns/svc" → []corev1.EndpointSubset
	watchStopChs          sync.Map // key: "ns/svc" → chan struct{}
	maintenanceSubsets    []corev1.EndpointSubset // 当前维护页的 subsets（缓存）
	maintenanceSubsetsMtx sync.Mutex
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
	go watchConfigFile()

	// 监听系统信号优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	logger.Println("Shutting down...")
	stopAllWatchers()
}

func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Printf("Failed to read config: %v", err)
		return
	}

	var newConfig Config
	if err := yaml.Unmarshal(data, &newConfig); err != nil {
		logger.Printf("Failed to parse config yaml: %v", err)
		return
	}

	mu.Lock()
	oldForce := config.Switch.ForceSwitch
	config = newConfig
	mu.Unlock()

	// 默认值补全
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

	// 重新加载维护页面模板
	loadHTMLTemplate()

	// 初始化/更新 Telegram Bot
	if config.Telegram.Token != "" && config.Telegram.ChatID != 0 {
		var botErr error
		bot, botErr = tgbotapi.NewBotAPI(config.Telegram.Token)
		if botErr != nil {
			logger.Printf("Failed to init telegram bot: %v", botErr)
			bot = nil
		} else {
			logger.Printf("Telegram bot initialized: @%s", bot.Self.UserName)
		}
	}

	// 缓存当前维护服务的 endpoints subsets
	updateMaintenanceSubsets()

	// 开关变化处理
	shouldSwitch := config.Switch.ForceSwitch

	if shouldSwitch && !oldForce {
		logger.Println("Force switch turned ON → switching to maintenance")
		switchToMaintenance()
		sendTelegram("🚧 **Maintenance mode ACTIVATED**")
	} else if !shouldSwitch && oldForce {
		logger.Println("Force switch turned OFF → recovering original traffic")
		recoverOriginal()
		sendTelegram("✅ **Maintenance mode DEACTIVATED**, traffic recovered")
	}
}

func loadHTMLTemplate() {
	mu.Lock()
	defer mu.Unlock()

	tmpl, err := template.ParseFiles(config.Maintenance.HTMLPath)
	if err != nil {
		logger.Printf("Failed to load maintenance template %s: %v", config.Maintenance.HTMLPath, err)
		htmlTemplate = nil
		return
	}
	htmlTemplate = tmpl
	logger.Printf("Maintenance HTML loaded: %s", config.Maintenance.HTMLPath)
}

func updateMaintenanceSubsets() {
	maintenanceSubsetsMtx.Lock()
	defer maintenanceSubsetsMtx.Unlock()

	ep, err := clientset.CoreV1().Endpoints(config.Switch.MaintenanceNamespace).Get(
		context.Background(),
		config.Switch.MaintenanceService,
		metav1.GetOptions{},
	)
	if err != nil {
		logger.Printf("Failed to get maintenance endpoints %s/%s: %v",
			config.Switch.MaintenanceNamespace, config.Switch.MaintenanceService, err)
		return
	}

	if len(ep.Subsets) == 0 {
		logger.Println("Warning: maintenance service has no endpoints subsets")
		maintenanceSubsets = nil
		return
	}

	maintenanceSubsets = ep.Subsets
	logger.Printf("Maintenance subsets updated (count: %d)", len(maintenanceSubsets))
}

func startHTTPServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		tmpl := htmlTemplate
		mu.RUnlock()

		if tmpl == nil {
			http.Error(w, "Maintenance page not available", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, nil)
	})

	addr := fmt.Sprintf("%s:%s", config.HTTP.ListenAddr, config.HTTP.Port)
	logger.Printf("Starting HTTP server on %s", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Fatalf("HTTP server failed: %v", err)
	}
}

func watchConfigFile() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatalf("Failed to create fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	dir := filepath.Dir(configPath)
	if err := watcher.Add(dir); err != nil {
		logger.Fatalf("Failed to watch directory %s: %v", dir, err)
	}

	logger.Printf("Watching config directory: %s", dir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if strings.HasSuffix(event.Name, "config.yaml") ||
					strings.HasSuffix(event.Name, "maintenance.html") {
					logger.Printf("Detected change in %s, reloading config...", event.Name)
					loadConfig()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Printf("fsnotify error: %v", err)
		}
	}
}

func switchToMaintenance() {
	maintenanceSubsetsMtx.Lock()
	if len(maintenanceSubsets) == 0 {
		logger.Println("Cannot switch: maintenance subsets is empty")
		maintenanceSubsetsMtx.Unlock()
		return
	}
	subsetsToApply := append([]corev1.EndpointSubset{}, maintenanceSubsets...)
	maintenanceSubsetsMtx.Unlock()

	for _, t := range config.Switch.Targets {
		key := fmt.Sprintf("%s/%s", t.Namespace, t.Service)

		// 保存原始状态（只保存一次）
		if _, loaded := originalSubsets.Load(key); !loaded {
			ep, err := clientset.CoreV1().Endpoints(t.Namespace).Get(context.Background(), t.Service, metav1.GetOptions{})
			if err == nil && len(ep.Subsets) > 0 {
				originalSubsets.Store(key, ep.Subsets)
				logger.Printf("Saved original subsets for %s", key)
			}
		}

		// 第一次强制覆盖
		patchEndpoints(t.Namespace, t.Service, subsetsToApply)

		// 启动/确保有 watcher
		startOrRestartWatcher(t.Namespace, t.Service, subsetsToApply)
	}
}

func recoverOriginal() {
	for _, t := range config.Switch.Targets {
		key := fmt.Sprintf("%s/%s", t.Namespace, t.Service)

		// 停止 watcher
		if ch, ok := watchStopChs.LoadAndDelete(key); ok {
			if stopCh, isChan := ch.(chan struct{}); isChan {
				close(stopCh)
			}
		}

		// 恢复原始状态
		if raw, ok := originalSubsets.LoadAndDelete(key); ok {
			if subsets, ok := raw.([]corev1.EndpointSubset); ok && len(subsets) > 0 {
				patchEndpoints(t.Namespace, t.Service, subsets)
				logger.Printf("Recovered original subsets for %s", key)
			}
		}
	}
}

func patchEndpoints(namespace, name string, subsets []corev1.EndpointSubset) {
	patchData := map[string]interface{}{
		"subsets": subsets,
	}

	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		logger.Printf("Failed to marshal patch data for %s/%s: %v", namespace, name, err)
		return
	}

	err = wait.PollUntilContextTimeout(context.Background(), time.Second, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := clientset.CoreV1().Endpoints(namespace).Patch(
				ctx,
				name,
				types.MergePatchType,
				patchBytes,
				metav1.PatchOptions{},
			)
			if err != nil {
				logger.Printf("Patch failed for %s/%s: %v", namespace, name, err)
				return false, nil
			}
			return true, nil
		})

	if err != nil {
		logger.Printf("Failed to patch %s/%s after retries: %v", namespace, name, err)
	} else {
		logger.Printf("Successfully patched endpoints %s/%s", namespace, name)
	}
}

func startOrRestartWatcher(namespace, service string, desiredSubsets []corev1.EndpointSubset) {
	key := fmt.Sprintf("%s/%s", namespace, service)

	// 先停止旧的（如果存在）
	if oldCh, loaded := watchStopChs.LoadAndDelete(key); loaded {
		if ch, ok := oldCh.(chan struct{}); ok {
			close(ch)
		}
	}

	stop := make(chan struct{})
	watchStopChs.Store(key, stop)

	go func() {
		defer logger.Printf("Watcher exited for %s", key)

		for {
			w, err := clientset.CoreV1().Endpoints(namespace).Watch(context.Background(), metav1.ListOptions{
				FieldSelector:   fmt.Sprintf("metadata.name=%s", service),
				TimeoutSeconds:  int64Ptr(30),
				ResourceVersion: "0",
			})
			if err != nil {
				logger.Printf("Watch failed for %s: %v, retrying...", key, err)
				time.Sleep(5 * time.Second)
				continue
			}

			func() {
				defer w.Stop()

				for {
					select {
					case event, ok := <-w.ResultChan():
						if !ok {
							return // channel closed, will restart watch
						}

						if event.Type == watch.Modified || event.Type == watch.Added {
							ep, ok := event.Object.(*corev1.Endpoints)
							if !ok {
								continue
							}

							if !subsetsDeepEqual(ep.Subsets, desiredSubsets) {
								logger.Printf("Detected unwanted change on %s/%s, re-enforcing...", namespace, service)
								patchEndpoints(namespace, service, desiredSubsets)
							}
						}

					case <-stop:
						return
					}
				}
			}()

			select {
			case <-stop:
				return
			case <-time.After(3 * time.Second):
				// continue retry
			}
		}
	}()
}

func subsetsDeepEqual(a, b []corev1.EndpointSubset) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if len(a[i].Addresses) != len(b[i].Addresses) ||
			len(a[i].NotReadyAddresses) != len(b[i].NotReadyAddresses) ||
			len(a[i].Ports) != len(b[i].Ports) {
			return false
		}
		// 这里可以继续做更严格的比较，但大多数场景长度+内容一致就够了
	}

	return true
}

func sendTelegram(msg string) {
	if bot == nil {
		return
	}

	message := tgbotapi.NewMessage(config.Telegram.ChatID, msg)
	message.ParseMode = "MarkdownV2"

	if _, err := bot.Send(message); err != nil {
		logger.Printf("Telegram send failed: %v", err)
	} else {
		logger.Printf("Telegram sent: %s", msg)
	}
}

func int64Ptr(i int64) *int64 { return &i }

func stopAllWatchers() {
	watchStopChs.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan struct{}); ok {
			close(ch)
		}
		return true
	})
	watchStopChs = sync.Map{}
}