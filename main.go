package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
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
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type Rule struct {
	Domain            string   `json:"domain"`
	CheckURL          string   `json:"check_url"`
	CheckCondition    string   `json:"check_condition"`
	FailThreshold     int      `json:"fail_threshold"`
	RecoveryThreshold int      `json:"recovery_threshold"`
	CheckInterval     string   `json:"check_interval"`
	MaintenanceLabel  string   `json:"maintenance_pod_label"`
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
	configPath         = "/config/rules.yaml"
	htmlPath           = "/config/maintenance.html"
	telegramToken      = os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatID     int64 // Set from env or config
	rules              []Rule
	states             map[string]*State // key: domain
	podIPs             []string
	mu                 sync.RWMutex
	clientset          *kubernetes.Clientset
	htmlTemplate       *template.Template
	originalEndpoints  map[string][]byte // key: ns-svc, value: original subsets json
	maintenancePort    = 80 // Assume HTTP on port 80
)

func main() {
	// Load k8s config
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := flag.String("kubeconfig", "", "kubeconfig path")
		flag.Parse()
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err)
		}
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// Load initial config
	loadConfig()

	// Load HTML template
	loadHTML()

	// Start HTTP server for maintenance page
	go startHTTPServer()

	// Watch config file for changes
	go watchConfigFile()

	// Watch own Pods for IP changes
	go watchOwnPods()

	// Telegram bot setup
	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Panic(err)
	}

	// Start monitoring for each rule
	ctx, cancel := context.WithCancel(context.Background)
	defer cancel()

	states = make(map[string]*State)
	originalEndpoints = make(map[string][]byte)

	for i := range rules {
		rule := rules[i]
		states[rule.Domain] = &State{Status: "normal", Notified: false, Confirmed: false}
		go monitorRule(ctx, bot, rule)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	klog.Info("Shutting down...")
}

func loadConfig() {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}
	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		log.Fatalf("Error parsing config: %v", err)
	}
	mu.Lock()
	rules = config.Rules
	mu.Unlock()
	log.Println("Config loaded")
}

func loadHTML() {
	tmpl, err := template.ParseFiles(htmlPath)
	if err != nil {
		log.Fatalf("Error loading HTML template: %v", err)
	}
	mu.Lock()
	htmlTemplate = tmpl
	mu.Unlock()
	log.Println("HTML template loaded")
}

func watchConfigFile() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	err = watcher.Add(filepath.Dir(configPath))
	if err != nil {
		log.Fatal(err)
	}
	err = watcher.Add(filepath.Dir(htmlPath))
	if err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Write == fsnotify.Write {
				if strings.Contains(event.Name, "rules.yaml") {
					loadConfig()
				}
				if strings.Contains(event.Name, "maintenance.html") {
					loadHTML()
				}
			}
		case err := <-watcher.Errors:
			log.Println("Watcher error:", err)
		}
	}
}

func watchOwnPods() {
	listWatch := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		metav1.NamespaceDefault, // Assume program ns
		fields.Everything(),
	)

	_, controller := cache.NewInformer(
		listWatch,
		&v1.Pod{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				updatePodIPs()
			},
			UpdateFunc: func(old, new interface{}) {
				updatePodIPs()
			},
			DeleteFunc: func(obj interface{}) {
				updatePodIPs()
			},
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)
	if !cache.WaitForCacheSync(stop, controller.HasSynced) {
		log.Fatal("Failed to sync informer cache")
	}
}

func updatePodIPs() {
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceDefault).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=traffic-switcher", // Own label
	})
	if err != nil {
		log.Println("Error listing pods:", err)
		return
	}

	var ips []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}

	mu.Lock()
	podIPs = ips
	mu.Unlock()

	// Re-patch all switched svcs if IPs changed
	rePatchSwitchedSvcs()
}

func rePatchSwitchedSvcs() {
	// Logic to find all currently switched svcs (from states or records)
	for domain, state := range states {
		if state.Status == "failed" && state.Confirmed {
			// Find rule
			for _, rule := range rules {
				if rule.Domain == domain {
					switchToMaintenance(rule)
					break
				}
			}
		}
	}
}

func monitorRule(ctx context.Context, bot *tgbotapi.BotAPI, rule Rule) {
	failCount := 0
	recoveryCount := 0
	interval, _ := time.ParseDuration(rule.CheckInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			healthy := checkURL(rule.CheckURL, rule.CheckCondition)
			mu.RLock()
			state := states[rule.Domain]
			mu.RUnlock()

			if !healthy {
				failCount++
				recoveryCount = 0
				if failCount >= rule.FailThreshold && state.Status == "normal" {
					// Send Telegram notification
					sendTelegramNotification(bot, rule.Domain)
					state.Notified = true
					// Wait for confirmation (handled in webhook or separate goroutine)
					// For simplicity, assume confirmation sets state.Confirmed = true
				}
				if state.Confirmed {
					switchToMaintenance(rule)
					state.Status = "failed"
				}
			} else {
				recoveryCount++
				failCount = 0
				if recoveryCount >= rule.RecoveryThreshold && state.Status == "failed" {
					switchBack(rule)
					state.Status = "normal"
					state.Notified = false
					state.Confirmed = false
				}
			}
		}
	}
}

func checkURL(url string, condition string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Simple check, can extend to body content
	return strings.Contains(condition, fmt.Sprintf("%d", resp.StatusCode))
}

func sendTelegramNotification(bot *tgbotapi.BotAPI, domain string) {
	msg := tgbotapi.NewMessage(telegramChatID, fmt.Sprintf("Domain %s fault detected. Confirm switch to maintenance?", domain))
	// Add buttons for confirm/manual
	// ...
	bot.Send(msg)
	// Handle callback in separate HTTP endpoint
}

func switchToMaintenance(rule Rule) {
	mu.RLock()
	ips := podIPs
	mu.RUnlock()
	if len(ips) == 0 {
		log.Println("No maintenance IPs available")
		return
	}

	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			// Backup original
			ep, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Get(context.TODO(), svc, metav1.GetOptions{})
			if err != nil {
				log.Println(err)
				continue
			}
			original, _ := json.Marshal(ep.Subsets)
			originalEndpoints[key] = original

			// Patch to maintenance IPs
			var addresses []v1.EndpointAddress
			for _, ip := range ips {
				addresses = append(addresses, v1.EndpointAddress{IP: ip})
			}
			patchData, _ := json.Marshal(map[string]interface{}{
				"subsets": []interface{}{
					map[string]interface{}{
						"addresses": addresses,
						"ports":     ep.Subsets[0].Ports, // Assume first subset ports
					},
				},
			})
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.TODO(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return err
			})
			if err != nil {
				log.Println(err)
			}
		}
	}
}

func switchBack(rule Rule) {
	for _, svcNS := range rule.Services {
		for _, svc := range svcNS.SvcNames {
			key := fmt.Sprintf("%s-%s", svcNS.Namespace, svc)
			original, ok := originalEndpoints[key]
			if !ok {
				log.Println("No original for", key)
				continue
			}
			// Patch back original
			patchData, _ := json.Marshal(map[string]interface{}{
				"subsets": json.RawMessage(original),
			})
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, err := clientset.CoreV1().Endpoints(svcNS.Namespace).Patch(context.TODO(), svc, types.MergePatchType, patchData, metav1.PatchOptions{})
				return err
			})
			if err != nil {
				log.Println(err)
			}
			delete(originalEndpoints, key)
		}
	}
}

func startHTTPServer() {
	http.HandleFunc("/", maintenanceHandler)
	log.Fatal(http.ListenAndServe(":80", nil))
}

func maintenanceHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	tmpl := htmlTemplate
	mu.RUnlock()
	tmpl.Execute(w, map[string]string{"Domain": r.Host})
}