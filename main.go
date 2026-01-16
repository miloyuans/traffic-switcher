package main

import (
    "flag"
    "os"
    "os/signal"
	"sync"

    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/klog/v2"
)

var (
    configPath = flag.String("config", "/config/config.yaml", "Path to config file")
)

func main() {
    flag.Parse()

    // K8s client
    config, err := rest.InClusterConfig()
    if err != nil {
        klog.Fatal(err)
    }
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        klog.Fatal(err)
    }

    ctrl := &Controller{
		clientset:  clientset,
		pending:    sync.Map{},
		configPath: *configPath,  // 新增这一行
	}

    // 加载初始配置
    if err := ctrl.LoadConfig(*configPath); err != nil {
        klog.Fatal(err)
    }

    // 初始化 Mongo 和 Telegram
    if err := ctrl.InitMongo(); err != nil {
        klog.Fatal(err)
    }
    if err := ctrl.InitTelegram(); err != nil {
        klog.Fatal(err)
    }

    // 启动配置热加载
    go ctrl.WatchConfig(*configPath)

    // 启动 Telegram 回调处理
    go ctrl.HandleTelegramCallbacks()

    // 启动探测器
    ctrl.StartProbers()

    // 启动清理任务
    go ctrl.StartCleanupTask()

    // 优雅退出
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt)
    <-sig
    klog.Info("Shutting down...")
}