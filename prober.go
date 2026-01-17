package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "strconv"
    "strings"
    "time"

    "gopkg.in/yaml.v3"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/klog/v2"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// probeAndAct 改为处理多 endpoint + 连续确认
func (c *Controller) probeAndAct(rule *RuleRuntime) {
    if len(rule.Config.Endpoints) == 0 {
        klog.Warningf("rule 无 endpoints，跳过探测: %s", rule.Config.BaseDomain)
        return
    }

    klog.Infof("【探测开始】base_domain: %s, endpoints 数量: %d", rule.Config.BaseDomain, len(rule.Config.Endpoints))

	// 收集本次所有 endpoint 的探测细节，用于通知中展示
	var probeDetails []string

    // 所有 endpoint 都成功才算 rule 健康
     allOK := true
     for i, endpoint := range rule.Config.Endpoints {
         ok, details := c.probeEndpoint(rule.Config.BaseDomain, endpoint)
         klog.Infof("【endpoint %d 结果】path: %s, method: %s, 成功: %v, 详情: %s", i+1, endpoint.Path, endpoint.Method, ok, details)
        // 格式化细节，便于人类阅读（包含完整 URL）
        fullPath := rule.Config.BaseDomain
        if !strings.HasSuffix(fullPath, "/") && !strings.HasPrefix(endpoint.Path, "/") {
            fullPath += "/"
        }
        fullPath += endpoint.Path
        probeDetails = append(probeDetails, fmt.Sprintf("• %s (%s) → %v\n  %s", fullPath, strings.ToUpper(endpoint.Method), ok, details))
         if !ok {
             allOK = false
         }
     }

    // 连续确认逻辑
    confirmCount := rule.Config.ConfirmCount
    if confirmCount <= 0 {
        confirmCount = 1
    }

    if allOK {
        if rule.LastStreakOK {
            rule.CurrentStreak++
        } else {
            rule.CurrentStreak = 1
            rule.LastStreakOK = true
        }
    } else {
        if !rule.LastStreakOK {
            rule.CurrentStreak++
        } else {
            rule.CurrentStreak = 1
            rule.LastStreakOK = false
        }
    }

    klog.Infof("【rule 整体状态】健康: %v, 连续次数: %d / %d", allOK, rule.CurrentStreak, confirmCount)

    prevConfirmedOK := rule.LastProbeOK

    // 只有达到 confirm_count 才确认状态变化
    if rule.CurrentStreak >= confirmCount {
        rule.LastProbeOK = allOK
    }

	// 连续确认逻辑
    detailsText := strings.Join(probeDetails, "\n")

    if rule.LastProbeOK && !prevConfirmedOK && rule.IsSwitched {
        klog.Infof("【状态确认恢复】连续 %d 次健康，触发恢复流程", confirmCount)
        c.requestRecovery(rule, "health_check_recovered", "恢复探测细节：\n"+detailsText)
        go c.disableForceSwitchIfNeeded(rule)
    } else if !rule.LastProbeOK && prevConfirmedOK {
        klog.Warningf("【状态确认故障】连续 %d 次不健康，触发切换流程", confirmCount)
        c.requestFailover(rule, "health_check_failed", "故障探测细节：\n"+detailsText)
    } else if rule.Config.ForceSwitch {
        klog.Warningf("【强制切换】开关开启，触发切换")
        c.requestFailover(rule, "force_switch", "强制切换（无健康检查细节，由 force_switch 开关触发）")
    } else {
         klog.V(2).Infof("【状态稳定】无需操作，当前确认健康: %v", rule.LastProbeOK)
     }
 }

// 新：单个 endpoint 探测
func (c *Controller) probeEndpoint(baseDomain string, endpoint EndpointConfig) (ok bool, details string) {
    method := strings.ToUpper(endpoint.Method)
    if method == "" {
        method = "GET"
    }

    fullURL := baseDomain
    if !strings.HasSuffix(fullURL, "/") && !strings.HasPrefix(endpoint.Path, "/") {
        fullURL += "/"
    }
    fullURL += endpoint.Path

    var req *http.Request
    var err error

    if method == "POST" && len(endpoint.Params) > 0 {
        jsonBody, _ := json.Marshal(endpoint.Params)
        req, err = http.NewRequest(method, fullURL, bytes.NewBuffer(jsonBody))
        if err == nil {
            req.Header.Set("Content-Type", "application/json")
        }
        details = fmt.Sprintf("POST JSON body: %s", string(jsonBody))
    } else {
        // GET 或无 params
        if len(endpoint.Params) > 0 {
            q := url.Values{}
            for k, v := range endpoint.Params {
                q.Add(k, v)
            }
            fullURL += "?" + q.Encode()
            details = fmt.Sprintf("GET query: %s", q.Encode())
        }
        req, err = http.NewRequest(method, fullURL, nil)
    }

    if err != nil {
        return false, fmt.Sprintf("请求创建失败: %v", err)
    }

    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return false, fmt.Sprintf("请求失败: %v", err)
    }
    defer resp.Body.Close()

    bodyBytes, _ := io.ReadAll(resp.Body)
    bodyStr := string(bodyBytes)

    // 状态码检查
    expectedCodes := endpoint.ExpectedCodes
    if len(expectedCodes) == 0 {
        expectedCodes = c.config.Global.ExpectedCodes
    }
    codeOK := false
    for _, code := range expectedCodes {
        if resp.StatusCode == code {
            codeOK = true
            break
        }
    }

    // body 包含检查
    bodyOK := true
    if endpoint.ExpectedBodyContains != "" {
        bodyOK = strings.Contains(bodyStr, endpoint.ExpectedBodyContains)
    }

    ok = codeOK && bodyOK
    details += fmt.Sprintf(" | 状态码: %d (期望: %v) | body包含检查: %v (期望: %s)", resp.StatusCode, expectedCodes, bodyOK, endpoint.ExpectedBodyContains)

    return ok, details
}

func (c *Controller) requestFailover(rule *RuleRuntime, reason string, probeDetails string) {
	if rule.IsSwitched {
		klog.Infof("【故障切换已执行】当前已处于切换状态，跳过重复操作, 域名: %s", rule.Config.Domain)
		return
	}

	klog.Warningf("【准备故障切换】发送人工确认通知, 域名: %s, 原因: %s", rule.Config.Domain, reason)

	approved, err := c.sendConfirmation(rule, "🚨 故障检测到异常，准备切换流量 🚨", reason, probeDetails)
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

		oldSelector := cloneMap(svc.Spec.Selector)

		// 明确先清空 Selector（确保无残留），再全量覆盖源 Selector
		svc.Spec.Selector = nil
		svc.Spec.Selector = sourceSelector

		_, err = c.clientset.CoreV1().Services(target.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
		if err != nil {
			updateErrors = append(updateErrors, fmt.Sprintf("update %s/%s: %v", target.Namespace, target.Name, err))
		} else {
			klog.Infof("【切换成功】目标 Service %s/%s Selector 从 %v → %v", target.Namespace, target.Name, oldSelector, sourceSelector)
		}
	}

	if len(updateErrors) > 0 {
		klog.Errorf("【部分切换失败】%v", updateErrors)
	}

	rule.IsSwitched = true
	c.logEvent(rule.Config.Domain, "failover_executed", reason)

	// 自定义成功切换通知模板
	template := "✅ 已执行流量故障切换\n原因: " + reason // 默认 fallback
	if rule.Config.SuccessFailoverMessageTemplate != "" {
		template = rule.Config.SuccessFailoverMessageTemplate
	}

	display := buildDisplayDomains(rule.Config.DisplayDomains)
	msgText := strings.ReplaceAll(template, "{{display_domains}}", display)

	chatID, err := c.getChatID()
	if err != nil {
		klog.Errorf("发送最终切换成功通知失败 (chat_id 无效): %v", err)
		return
	}
	c.tgBot.Send(tgbotapi.NewMessage(chatID, msgText))
}

func (c *Controller) requestRecovery(rule *RuleRuntime, reason string, probeDetails string) {
	klog.Infof("【准备流量恢复】发送人工确认通知, 域名: %s", rule.Config.Domain)

	approved, err := c.sendConfirmation(rule, "✅ 探测恢复正常，准备恢复原流量 ✅", reason, probeDetails)
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

    // 自定义成功恢复通知模板
    template := "✅ 已执行流量恢复" // 默认 fallback
    if rule.Config.SuccessRecoveryMessageTemplate != "" {
        template = rule.Config.SuccessRecoveryMessageTemplate
    }

    display := buildDisplayDomains(rule.Config.DisplayDomains)
    msgText := strings.ReplaceAll(template, "{{display_domains}}", display)

    chatID, err := c.getChatID()
    if err != nil {
        klog.Errorf("发送最终恢复成功通知失败 (chat_id 无效): %v", err)
        return
    }
    c.tgBot.Send(tgbotapi.NewMessage(chatID, msgText))
}

// 恢复后自动关闭强制开关（仅内存 + 尝试写回配置文件，ConfigMap 读只挂载会失败，但不影响核心功能）
func (c *Controller) disableForceSwitchIfNeeded(rule *RuleRuntime) {
    if !rule.Config.ForceSwitch {
        return
    }

    klog.Infof("【自动关闭强制开关】探测恢复正常，关闭 force_switch, 域名: %s", rule.Config.Domain)

    rule.Config.ForceSwitch = false

    // 尝试写回配置文件
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

    // 发送通知（使用自定义模板或默认）
    template := "🔧 强制切换开关已自动关闭"
    if rule.Config.SuccessRecoveryMessageTemplate != "" { // 复用恢复模板或新增专用模板
        template = rule.Config.SuccessRecoveryMessageTemplate
    }

    display := buildDisplayDomains(rule.Config.DisplayDomains)
    msgText := strings.ReplaceAll(template, "{{display_domains}}", display)

    chatIDStr := c.config.Global.Telegram.ChatID
    if chatIDStr == "" {
        klog.Errorf("发送强制开关关闭通知失败: chat_id 配置为空")
        return
    }
    chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
    if err != nil {
        klog.Errorf("发送强制开关关闭通知失败: chat_id 解析错误 (%s): %v", chatIDStr, err)
        return
    }

    c.tgBot.Send(tgbotapi.NewMessage(chatID, msgText))
}