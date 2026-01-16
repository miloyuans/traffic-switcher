package main

import (
    "context"
    "os"
    "sync"

    "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/klog/v2"
    "gopkg.in/yaml.v3"
)

func (c *Controller) requestFailover(rule *RuleRuntime, reason string) {
    // 检查是否已切换
    if rule.IsSwitched {
        c.logEvent(rule.Config.Domain, "failover_skipped", "already switched")
        return
    }

    approved, err := c.sendConfirmation(rule, "故障检测到异常，准备切换流量", reason)
    if err != nil || !approved {
        c.logEvent(rule.Config.Domain, "failover_denied", reason+" (denied or timeout)")
        return
    }

    // 备份
    if err := c.backupSelectors(rule); err != nil {
        klog.Error(err)
        return
    }

    // 获取源 Selector
    sourceSvc, err := c.clientset.CoreV1().Services(c.config.Global.SourceService.Namespace).
        Get(context.TODO(), c.config.Global.SourceService.Name, metav1.GetOptions{})
    if err != nil {
        klog.Error(err)
        return
    }
    sourceSelector := cloneMap(sourceSvc.Spec.Selector)

    // 应用到目标
    for _, target := range rule.Config.TargetServices {
        svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(context.TODO(), target.Name, metav1.GetOptions{})
        if err != nil {
            klog.Error(err)
            continue
        }
        svc.Spec.Selector = sourceSelector
        _, err = c.clientset.CoreV1().Services(target.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
        if err != nil {
            klog.Error(err)
        }
    }

    rule.IsSwitched = true
    c.logEvent(rule.Config.Domain, "failover_executed", reason)
    c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID, fmt.Sprintf("已执行流量切换: %s", rule.Config.Domain)))
}

func (c *Controller) requestRecovery(rule *RuleRuntime) {
    approved, err := c.sendConfirmation(rule, "探测恢复正常，准备恢复流量", "health_check_recovered")
    if err != nil || !approved {
        c.logEvent(rule.Config.Domain, "recovery_denied", "denied or timeout")
        return
    }

    if err := c.restoreSelectors(rule); err != nil {
        klog.Error(err)
        return
    }

    rule.IsSwitched = false
    c.logEvent(rule.Config.Domain, "recovery_executed", "recovered")
    c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID, fmt.Sprintf("已执行流量恢复: %s", rule.Config.Domain)))
}

func (c *Controller) disableForceSwitchIfNeeded(rule *RuleRuntime) {
    if !rule.Config.ForceSwitch {
        return
    }
    // 修改内存配置
    rule.Config.ForceSwitch = false

    // 写回文件
    c.mu.Lock()
    // 找到对应 rule 索引并更新
    for i := range c.config.Rules {
        if c.config.Rules[i].Domain == rule.Config.Domain {
            c.config.Rules[i].ForceSwitch = false
            break
        }
    }
    data, _ := yaml.Marshal(c.config)
    os.WriteFile(*configPath, data, 0644)
    c.mu.Unlock()

    c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID, fmt.Sprintf("强制切换开关已自动关闭: %s", rule.Config.Domain)))
}