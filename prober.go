package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (c *Controller) probeAndAct(rule *RuleRuntime) {
	c.mu.RLock()
	globalCodes := c.config.Global.ExpectedCodes
	c.mu.RUnlock()

	expected := globalCodes
	if len(rule.Config.ExpectedCodes) > 0 {
		expected = rule.Config.ExpectedCodes
	}

	klog.Infof("【探测开始】域名: %s, 期望状态码: %v", rule.Config.Domain, expected)

	ok, statusCode, err := c.probeURL(rule.Config.Domain)
	if err != nil {
		klog.Errorf("【探测失败】域名: %s, 错误: %v", rule.Config.Domain, err)
	} else {
		klog.Infof("【探测结果】域名: %s, 返回状态码: %d, 是否符合期望: %v", rule.Config.Domain, statusCode, ok)
	}

	prevOK := rule.LastProbeOK
	rule.LastProbeOK = ok

	// 强制切换开关优先处理（即使探测正常，也会触发切换）
	if rule.Config.ForceSwitch {
		klog.Warningf("【强制切换开关开启】触发故障切换流程, 域名: %s", rule.Config.Domain)
		c.requestFailover(rule, "force_switch")
		// 不 return，继续后续判断，以便在探测恢复正常时自动关闭开关并恢复
	}

	// 新故障：从正常 → 异常
	if !ok && prevOK {
		klog.Warningf("【健康检查失败】新故障检测到，触发故障切换通知, 域名: %s", rule.Config.Domain)
		c.requestFailover(rule, "health_check_failed")
	} else if ok && !prevOK && rule.IsSwitched {
		// 恢复：从异常 → 正常，且当前已切换状态
		klog.Infof("【健康检查恢复正常】触发流量恢复流程, 域名: %s", rule.Config.Domain)
		c.requestRecovery(rule)
		// 如果是从强制开关触发的，恢复后自动关闭开关
		go c.disableForceSwitchIfNeeded(rule)
	} else {
		klog.V(2).Infof("【状态无变化】无需操作, 域名: %s, 当前探测正常: %v, 已切换状态: %v",
			rule.Config.Domain, ok, rule.IsSwitched)
	}
}

// probeURL 只负责 HTTP 请求，返回是否成功、状态码和错误（不处理 expected 比较）
func (c *Controller) probeURL(urlStr string) (ok bool, statusCode int, err error) {
	// 添加超时防止挂死
	client := &http.Client{
		Timeout: 10 * time.Second,
		// 可选：禁止跳转，避免 301/302 干扰
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(urlStr)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()

	// 读取 body（可选，避免大响应卡住，但这里仅探测状态码，通常不需要读 body）
	// io.Copy(ioutil.Discard, resp.Body)

	// 从 rule 获取 expected（这里无法直接访问 rule，所以只返回状态码，比较在外层）
	// 注意：ok 判断在外层 probeAndAct 中完成
	c.mu.RLock()
	globalCodes := c.config.Global.ExpectedCodes
	c.mu.RUnlock()

	expected := globalCodes
	// 注意：rule 特定 expected 在 probeAndAct 中覆盖，这里仅为备用
	for _, code := range expected {
		if resp.StatusCode == code {
			return true, resp.StatusCode, nil
		}
	}
	return false, resp.StatusCode, nil
}

func (c *Controller) requestFailover(rule *RuleRuntime, reason string) {
	if rule.IsSwitched {
		klog.Infof("【故障切换已执行】当前已处于切换状态，跳过重复操作, 域名: %s", rule.Config.Domain)
		return
	}

	klog.Warningf("【准备故障切换】发送人工确认通知, 域名: %s, 原因: %s", rule.Config.Domain, reason)

	approved, err := c.sendConfirmation(rule, "🚨 故障检测到异常，准备切换流量 🚨", reason)
	if err != nil || !approved {
		klog.Warningf("【故障切换取消】人工拒绝或超时, 域名: %s, 错误: %v", rule.Config.Domain, err)
		c.logEvent(rule.Config.Domain, "failover_denied", reason+" (denied or timeout)")
		return
	}

	klog.Infof("【人工确认通过】开始执行 Selector 备份与覆盖切换")

	// 备份原 Selector
	if err := c.backupSelectors(rule); err != nil {
		klog.Errorf("【备份失败】无法备份原 Selector: %v", err)
		return
	}

	// 获取源 Service Selector
	sourceSvc, err := c.clientset.CoreV1().Services(c.config.Global.SourceService.Namespace).
		Get(context.TODO(), c.config.Global.SourceService.Name, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("【获取源 Service 失败】%s/%s: %v", c.config.Global.SourceService.Namespace, c.config.Global.SourceService.Name, err)
		return
	}
	sourceSelector := cloneMap(sourceSvc.Spec.Selector)

	// 应用到所有目标 Service
	var updateErrors []string
	for _, target := range rule.Config.TargetServices {
		svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(context.TODO(), target.Name, metav1.GetOptions{})
		if err != nil {
			updateErrors = append(updateErrors, fmt.Sprintf("get %s/%s: %v", target.Namespace, target.Name, err))
			continue
		}
		svc.Spec.Selector = sourceSelector
		_, err = c.clientset.CoreV1().Services(target.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
		if err != nil {
			updateErrors = append(updateErrors, fmt.Sprintf("update %s/%s: %v", target.Namespace, target.Name, err))
		} else {
			klog.Infof("【切换成功】目标 Service %s/%s 已更新 Selector", target.Namespace, target.Name)
		}
	}

	if len(updateErrors) > 0 {
		klog.Errorf("【部分切换失败】%v", updateErrors)
	}

	rule.IsSwitched = true
	c.logEvent(rule.Config.Domain, "failover_executed", reason)
	c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID,
		fmt.Sprintf("✅ 已执行流量故障切换: %s\n原因: %s", rule.Config.Domain, reason)))
}

func (c *Controller) requestRecovery(rule *RuleRuntime) {
	klog.Infof("【准备流量恢复】发送人工确认通知, 域名: %s", rule.Config.Domain)

	approved, err := c.sendConfirmation(rule, "✅ 探测恢复正常，准备恢复原流量 ✅", "health_check_recovered")
	if err != nil || !approved {
		klog.Warningf("【恢复取消】人工拒绝或超时, 域名: %s", rule.Config.Domain)
		c.logEvent(rule.Config.Domain, "recovery_denied", "denied or timeout")
		return
	}

	klog.Infof("【人工确认通过】开始恢复原 Selector")

	if err := c.restoreSelectors(rule); err != nil {
		klog.Errorf("【恢复失败】恢复 Selector 出错: %v", err)
		return
	}

	rule.IsSwitched = false
	c.logEvent(rule.Config.Domain, "recovery_executed", "recovered")
	c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID,
		fmt.Sprintf("✅ 已执行流量恢复: %s", rule.Config.Domain)))
}

// 恢复后自动关闭强制开关（仅内存 + 尝试写回配置文件，ConfigMap 读只挂载会失败，但不影响核心功能）
func (c *Controller) disableForceSwitchIfNeeded(rule *RuleRuntime) {
	if !rule.Config.ForceSwitch {
		return
	}

	klog.Infof("【自动关闭强制开关】探测恢复正常，关闭 force_switch, 域名: %s", rule.Config.Domain)

	rule.Config.ForceSwitch = false

	// 尝试写回配置文件（如果挂载为 readOnly，会失败，仅日志记录）
	c.mu.Lock()
	for i := range c.config.Rules {
		if c.config.Rules[i].Domain == rule.Config.Domain {
			c.config.Rules[i].ForceSwitch = false
			break
		}
	}
	data, err := yaml.Marshal(c.config)
	if err != nil {
		klog.Errorf("序列化配置失败: %v", err)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	if err := os.WriteFile(c.configPath, data, 0644); err != nil {
		klog.Warningf("【写回配置文件失败】通常因 ConfigMap readOnly 挂载引起，无需担心，开关已内存关闭: %v", err)
	}

	c.tgBot.Send(tgbotapi.NewMessage(c.config.Global.Telegram.ChatID,
		fmt.Sprintf("🔧 强制切换开关已自动关闭: %s", rule.Config.Domain)))
}