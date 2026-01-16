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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
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
	stopCh.Range(func(key, value interface{}) bool {
		close(value.(chan struct{}))
		return true
	})
}

func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Printf("Failed to read config: %v", err)
		return
	}

	var newConfig Config
	if err := yaml.Unmarshal(data, &newConfig); err != nil {
		logger.Printf("Failed to parse yaml: %v", err)
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

	// 加载维护页面
	loadHTMLTemplate()

	// 初始化 Telegram Bot
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

	// 开关变化检测
	shouldSwitch := config.Switch.ForceSwitch

	if shouldSwitch && !oldForce {
		logger.Println("Force switch turned ON -> switching to maintenance")
		switchToMaintenance()
		sendTelegram("🚧 **Maintenance mode ACTIVATED**")
	} else if !shouldSwitch && oldForce {
		logger.Println("Force switch turned OFF -> recovering original traffic")
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

func watchConfig() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatalf("Failed to create fs watcher: %v", err)
	}
	defer watcher.Close()

	dir := filepath.Dir(configPath)
	if err := watcher.Add(dir); err != nil {
		logger.Fatalf("Failed to watch dir %s: %v", dir, err)
	}
	logger.Printf("Watching config directory: %s", dir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if strings.HasSuffix(event.Name, "config.yaml") || strings.HasSuffix(event.Name, "maintenance.html") {
					logger.Printf("Detected change in %s, reloading...", event.Name)
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
	// 获取维护页服务的当前 subsets
	maintenanceEp, err := clientset.CoreV1().Endpoints(config.Switch.MaintenanceNamespace).Get(context.Background(), config.Switch.MaintenanceService, metav1.GetOptions{})
	if err != nil {
		logger.Printf("Failed to get maintenance endpoints %s/%s: %v", config.Switch.MaintenanceNamespace, config.Switch.MaintenanceService, err)
		return
	}
	maintenanceSubsets := maintenanceEp.Subsets

	if len(maintenanceSubsets) == 0 {
		logger.Println("Maintenance service has no subsets, cannot switch")
		return
	}

	for _, group := range config.Switch.Targets {
		for _, svc := range group.Services {
			key := fmt.Sprintf("%s/%s", group.Namespace, svc)

			// 保存原始 subsets
			targetEp, err := clientset.CoreV1().Endpoints(group.Namespace).Get(context.Background(), svc, metav1.GetOptions{})
			if err != nil {
				logger.Printf("Failed to get target %s: %v", key, err)
				continue
			}
			if _, loaded := originalSubsets.Load(key); !loaded {
				originalSubsets.Store(key, targetEp.Subsets)
				logger.Printf("Saved original subsets for %s", key)
			}

			// 覆盖 subsets
			patchSubsets(group.Namespace, svc, maintenanceSubsets)

			// 启动监控（拦截模式）
			ch := make(chan struct{})
			stopCh.Store(key, ch)
			go monitorEndpoints(group.Namespace, svc, maintenanceSubsets, ch)
		}
	}
}

func recoverOriginal() {
	for _, group := range config.Switch.Targets {
		for _, svc := range group.Services {
			key := fmt.Sprintf("%s/%s", group.Namespace, svc)

			// 先停止监控（关闭拦截模式）
			if chI, loaded := stopCh.LoadAndDelete(key); loaded {
				close(chI.(chan struct{}))
				logger.Printf("Stopped monitor (interception disabled) for %s", key)
			}

			// 恢复原始 subsets
			if raw, loaded := originalSubsets.LoadAndDelete(key); loaded {
				subsets := raw.([]corev1.EndpointSubset)
				patchSubsets(group.Namespace, svc, subsets)
				logger.Printf("Recovered original subsets for %s", key)
			}

			// 重启关联 Deployment
			restartAssociatedDeployment(group.Namespace, svc)
		}
	}
}

func patchSubsets(namespace, svc string, subsets []corev1.EndpointSubset) {
	patchData, err := json.Marshal(map[string]interface{}{"subsets": subsets})
	if err != nil {
		logger.Printf("Failed to marshal patch for %s/%s: %v", namespace, svc, err)
		return
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := clientset.CoreV1().Endpoints(namespace).Patch(context.Background(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		logger.Printf("Failed to patch %s/%s: %v", namespace, svc, err)
	} else {
		logger.Printf("Patched %s/%s successfully", namespace, svc)
	}
}

func monitorEndpoints(namespace, svc string, desiredSubsets []corev1.EndpointSubset, stop chan struct{}) {
	logger.Printf("Starting interception monitor for %s/%s", namespace, svc)

	for {
		select {
		case <-stop:
			logger.Printf("Interception monitor stopped for %s/%s", namespace, svc)
			return
		default:
			watcher, err := clientset.CoreV1().Endpoints(namespace).Watch(context.Background(), metav1.ListOptions{
				FieldSelector: fmt.Sprintf("metadata.name=%s", svc),
			})
			if err != nil {
				logger.Printf("Failed to start watch for %s/%s: %v, retrying in 5s...", namespace, svc, err)
				time.Sleep(5 * time.Second)
				continue
			}

			for event := range watcher.ResultChan() {
				if event.Type == watch.Modified {
					ep := event.Object.(*corev1.Endpoints)
					if !reflect.DeepEqual(ep.Subsets, desiredSubsets) {
						logger.Printf("Detected unauthorized change on %s/%s, intercepting and re-patching...", namespace, svc)
						patchSubsets(namespace, svc, desiredSubsets)
					}
				}
			}

			watcher.Stop()
			logger.Printf("Watch channel closed for %s/%s, restarting watch...", namespace, svc)
			time.Sleep(5 * time.Second)
		}
	}
}

func restartAssociatedDeployment(namespace, svc string) {
	// 获取 service selector
	service, err := clientset.CoreV1().Services(namespace).Get(context.Background(), svc, metav1.GetOptions{})
	if err != nil {
		logger.Printf("Failed to get service %s/%s for restart: %v", namespace, svc, err)
		return
	}

	selector := labels.SelectorFromSet(service.Spec.Selector)
	if selector.Empty() {
		logger.Printf("Service %s/%s has no selector, cannot restart deployment", namespace, svc)
		return
	}

	// 列出匹配的 Deployment
	deployments, err := clientset.AppsV1().Deployments(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		logger.Printf("Failed to list deployments for %s/%s: %v", namespace, svc, err)
		return
	}

	if len(deployments.Items) == 0 {
		logger.Printf("No deployments found for service %s/%s", namespace, svc)
		return
	}

	for _, dep := range deployments.Items {
		// 使用 annotation 触发 rolling restart
		patch := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`, time.Now().Format(time.RFC3339)))
		_, err := clientset.AppsV1().Deployments(namespace).Patch(context.Background(), dep.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			logger.Printf("Failed to restart deployment %s: %v", dep.Name, err)
		} else {
			logger.Printf("Triggered rolling restart for deployment %s", dep.Name)
		}
	}
}

func sendTelegram(msg string) {
	if bot == nil {
		return
	}

	message := tgbotapi.NewMessage(config.Telegram.ChatID, msg)
	message.ParseMode = "Markdown"

	if _, err := bot.Send(message); err != nil {
		logger.Printf("Failed to send telegram: %v", err)
	} else {
		logger.Printf("Telegram sent: %s", msg)
	}
}