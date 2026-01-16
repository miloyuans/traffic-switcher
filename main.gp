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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

type Config struct {
	Telegram struct {
		Token   string `yaml:"token"`
		ChatID  int64  `yaml:"chat_id"`
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

		Services []struct {
			Namespace string   `yaml:"namespace"`
			Services  []string `yaml:"services"` // 要切换的服务名列表
		} `yaml:"services"`
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

	// 用于记录每个 service 的原始 endpoints
	originalEndpoints = sync.Map() // key: "ns/svc"  value: []corev1.EndpointSubset
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

	// 加载配置（首次）
	loadConfig()

	// 启动 http server
	go startHTTPServer()

	// 监听配置文件变化
	go watchConfig()

	// 监听系统信号优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	logger.Println("Shutting down...")
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

	// 设置默认值
	if config.HTTP.ListenAddr == "" {
		config.HTTP.ListenAddr = "0.0.0.0"
	}
	if config.HTTP.Port == "" {
		config.HTTP.Port = "80"
	}
	if config.Maintenance.HTMLPath == "" {
		config.Maintenance.HTMLPath = "/config/maintenance.html"
	}

	// 加载维护页面模板
	loadHTMLTemplate()

	// 初始化 Telegram Bot（如果配置了）
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

	// 根据开关决定是否切换
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
		if err := tmpl.Execute(w, nil); err != nil {
			logger.Printf("Template execute error: %v", err)
			http.Error(w, "Template error", http.StatusInternalServerError)
		}
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
			if (event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create) &&
				(strings.HasSuffix(event.Name, "config.yaml") || strings.HasSuffix(event.Name, "maintenance.html")) {
				logger.Printf("Detected change in %s, reloading...", event.Name)
				loadConfig()
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
	myPodIPs := getMyPodIPs()
	if len(myPodIPs) == 0 {
		logger.Println("WARNING: No pod IPs found, cannot switch to maintenance")
		return
	}

	for _, group := range config.Switch.Services {
		for _, svcName := range group.Services {
			switchService(group.Namespace, svcName, myPodIPs)
		}
	}
}

func recoverOriginal() {
	for _, group := range config.Switch.Services {
		for _, svcName := range group.Services {
			key := fmt.Sprintf("%s/%s", group.Namespace, svcName)
			if raw, ok := originalEndpoints.LoadAndDelete(key); ok {
				subsets := raw.([]corev1.EndpointSubset)
				patchEndpoints(group.Namespace, svcName, subsets)
			} else {
				logger.Printf("No original endpoints found for %s/%s, skip recover", group.Namespace, svcName)
			}
		}
	}
}

func switchService(namespace, svcName string, targetIPs []string) {
	key := fmt.Sprintf("%s/%s", namespace, svcName)

	ep, err := clientset.CoreV1().Endpoints(namespace).Get(context.Background(), svcName, metav1.GetOptions{})
	if err != nil {
		logger.Printf("Failed to get endpoints %s/%s: %v", namespace, svcName, err)
		return
	}

	// 保存原始状态（如果还没保存）
	if _, loaded := originalEndpoints.Load(key); !loaded {
		originalEndpoints.Store(key, ep.Subsets)
		logger.Printf("Saved original endpoints for %s/%s", namespace, svcName)
	}

	// 构造新的 subsets，只保留端口信息，地址全部换成我们自己的 pod IPs
	var newSubsets []corev1.EndpointSubset
	if len(ep.Subsets) > 0 {
		newSubsets = []corev1.EndpointSubset{{
			Addresses: toEndpointAddresses(targetIPs),
			Ports:     ep.Subsets[0].Ports,
		}}
	} else {
		logger.Printf("Warning: %s/%s has no subsets, cannot determine ports", namespace, svcName)
		return
	}

	patchEndpoints(namespace, svcName, newSubsets)
}

func patchEndpoints(namespace, svcName string, subsets []corev1.EndpointSubset) {
	patch := map[string]interface{}{
		"subsets": subsets,
	}
	patchBytes, _ := json.Marshal(patch)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := clientset.CoreV1().Endpoints(namespace).Patch(
			context.Background(),
			svcName,
			types.MergePatchType,
			patchBytes,
			metav1.PatchOptions{},
		)
		return err
	})

	if err != nil {
		logger.Printf("Failed to patch endpoints %s/%s: %v", namespace, svcName, err)
	} else {
		logger.Printf("Endpoints patched successfully: %s/%s", namespace, svcName)
	}
}

func toEndpointAddresses(ips []string) []corev1.EndpointAddress {
	var addrs []corev1.EndpointAddress
	for _, ip := range ips {
		addrs = append(addrs, corev1.EndpointAddress{IP: ip})
	}
	return addrs
}

func getMyPodIPs() []string {
	// 简单实现：假设 Deployment 里设置了环境变量 POD_IP
	// 生产环境建议使用更可靠的方式（如 downward API 或 informer）
	ip := os.Getenv("POD_IP")
	if ip == "" {
		return nil
	}
	return []string{ip}
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