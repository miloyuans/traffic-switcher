package main

import (
    "fmt"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/google/uuid"
    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    "k8s.io/klog/v2"
)

func (c *Controller) InitTelegram() error {
    token := c.config.Global.Telegram.BotToken
    if token == "" {
        return fmt.Errorf("telegram bot_token is empty")
    }
    bot, err := tgbotapi.NewBotAPI(token)
    if err != nil {
        return err
    }
    c.tgBot = bot
    klog.Infof("Telegram Bot initialized: @%s", bot.Self.UserName)
    return nil
}

func (c *Controller) getChatID() (int64, error) {
    chatIDStr := c.config.Global.Telegram.ChatID
    if chatIDStr == "" {
        return 0, fmt.Errorf("chat_id 配置为空")
    }
    chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
    if err != nil {
        return 0, fmt.Errorf("chat_id 解析失败 (%s): %v", chatIDStr, err)
    }
    return chatID, nil
}

// 新增：获取授权用户@列表（实时查询 Telegram + Mongo 缓存）
func (c *Controller) getAuthorizedMentions(rule *RuleRuntime) string {
    if len(rule.Config.AuthorizedUserIDs) == 0 {
        return "" // 无授权限制
    }

    chatID, err := c.getChatID()
    if err != nil {
        klog.Errorf("获取 chat_id 失败，无法查询用户名: %v", err)
        return ""
    }

    var mentions []string
    for _, userID := range rule.Config.AuthorizedUserIDs {
        // 先查缓存（调用 storage.go 中的函数）
        username, cacheErr := c.getCachedUsername(userID)
        if cacheErr != nil {
            // 缓存 miss 或过期，实时查询
            fetchedUsername, fetchErr := c.fetchUsernameFromTelegram(userID, chatID)
            if fetchErr != nil {
                klog.Warningf("查询用户 %d username 失败: %v，使用 UserID 替代", userID, fetchErr)
                mentions = append(mentions, fmt.Sprintf("UserID:%d", userID))
            } else {
                // 更新缓存
                c.updateUserCache(userID, fetchedUsername)
                if fetchedUsername != "" {
                    mentions = append(mentions, "@"+fetchedUsername)
                } else {
                    mentions = append(mentions, fmt.Sprintf("UserID:%d", userID))
                }
            }
        } else {
            // 缓存命中
            if username != "" {
                mentions = append(mentions, "@"+username)
            } else {
                mentions = append(mentions, fmt.Sprintf("UserID:%d", userID))
            }
        }
    }

    if len(mentions) > 0 {
        return "\n\n@授权用户: " + strings.Join(mentions, " ")
    }
    return ""
}

func (c *Controller) HandleTelegramCallbacks() {
    for {
        u := tgbotapi.NewUpdate(0)
        u.Timeout = 60

        updates := c.tgBot.GetUpdatesChan(u)

        klog.Info("=== Telegram 长轮询监听已启动，等待按钮回调(update) ===")

        for update := range updates {
            klog.Infof("收到 Telegram Update ID: %d", update.UpdateID)

            if update.CallbackQuery == nil {
                klog.V(2).Infof("非按钮更新，已忽略: %+v", update)
                continue
            }

            callback := update.CallbackQuery
            userID := callback.From.ID
            klog.Infof("收到按钮点击！用户: %s (%d), 数据: %s, 消息ID: %d",
                callback.From.UserName, userID, callback.Data, callback.Message.MessageID)

            data := callback.Data
            var approved bool
            var prefix string
            if strings.HasPrefix(data, "approve:") {
                approved = true
                prefix = "approve:"
            } else if strings.HasPrefix(data, "reject:") {
                approved = false
                prefix = "reject:"
            } else {
                klog.Warningf("未知回调数据: %s", data)
                c.answerCallback(callback.ID, "无效操作")
                continue
            }

            uid := data[len(prefix):]

            // 获取关联的 rule 用于权限检查
            var rule *RuleRuntime
            if rAny, ok := c.pendingRule.Load(uid); ok {
                rule = rAny.(*RuleRuntime)
            } else {
                klog.Warningf("未找到对应 UID 的 rule: %s", uid)
                c.answerCallback(callback.ID, "操作已过期")
                continue
            }

            // 严格权限检查
            if len(rule.Config.AuthorizedUserIDs) > 0 {
                authorized := false
                for _, id := range rule.Config.AuthorizedUserIDs {
                    if int64(userID) == id {
                        authorized = true
                        break
                    }
                }
                if !authorized {
                    c.answerCallback(callback.ID, "❌ 无权限操作")
                    klog.Warningf("用户 %d 无权限操作 rule %s", userID, rule.Config.Domain)
                    continue
                }
            }

            // 权限通过，发送结果到通道
            if chAny, ok := c.pending.Load(uid); ok {
                ch := chAny.(chan bool)
                ch <- approved
                close(ch)
                c.pending.Delete(uid)
                c.pendingRule.Delete(uid)
                klog.Infof("授权用户 %d 操作生效: approved=%v", userID, approved)
            } else {
                klog.Warningf("未找到对应 pending channel: %s", uid)
            }

            text := "✅ 已确认"
            if !approved {
                text = "❌ 已拒绝"
            }
            c.answerCallback(callback.ID, text)
        }

        klog.Errorf("Telegram 长轮询通道意外关闭，正在重连...")
        time.Sleep(5 * time.Second)
    }
}

func (c *Controller) answerCallback(callbackID, text string) {
    callbackResp := tgbotapi.NewCallback(callbackID, text)
    if _, err := c.tgBot.Request(callbackResp); err != nil {
        klog.Errorf("回复回调确认失败: %v", err)
    } else {
        klog.Infof("已回复回调确认: %s", text)
    }
}

// 新：构建展示域名字符串
func buildDisplayDomains(domains []string) string {
    if len(domains) == 0 {
        return "无"
    }
    var lines []string
    for _, d := range domains {
        if !strings.HasPrefix(strings.ToLower(d), "http") {
            d = "https://" + d
        }
        lines = append(lines, "- "+d)
    }
    return strings.Join(lines, "\n")
}

func (c *Controller) sendConfirmation(rule *RuleRuntime, action, reason string) (bool, error) {
    uid := uuid.New().String()
    ch := make(chan bool, 1)
    c.pending.Store(uid, ch)
    c.pendingRule.Store(uid, rule) // 保存 rule 用于权限检查和@用户

    // 自定义模板优先
    template := rule.Config.NotificationMessage // fallback 旧字段
    if reason == "health_check_failed" && rule.Config.FailoverMessageTemplate != "" {
        template = rule.Config.FailoverMessageTemplate
    } else if reason == "health_check_recovered" && rule.Config.RecoveryMessageTemplate != "" {
        template = rule.Config.RecoveryMessageTemplate
    }

    // 替换占位符 {{display_domains}}
    display := buildDisplayDomains(rule.Config.DisplayDomains)
    msgText := strings.ReplaceAll(template, "{{display_domains}}", display)

    // 添加@授权用户（实时查询 + 缓存）
    mentions := c.getAuthorizedMentions(rule)
    msgText += mentions

    msgText += "\n请在10分钟内确认操作"

    chatID, err := c.getChatID()
    if err != nil {
        c.pending.Delete(uid)
        c.pendingRule.Delete(uid)
        klog.Errorf("发送确认通知失败: %v", err)
        return false, err
    }

    msg := tgbotapi.NewMessage(chatID, msgText)
    keyboard := tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("✅ 确认执行", "approve:"+uid),
            tgbotapi.NewInlineKeyboardButtonData("❌ 拒绝执行", "reject:"+uid),
        ),
    )
    msg.ReplyMarkup = keyboard

    sentMsg, err := c.tgBot.Send(msg)
    if err != nil {
        c.pending.Delete(uid)
        c.pendingRule.Delete(uid)
        return false, fmt.Errorf("发送通知失败: %v", err)
    }
    klog.Infof("已发送交互通知消息，MessageID: %d, UID: %s", sentMsg.MessageID, uid)

    select {
    case approved := <-ch:
        return approved, nil
    case <-time.After(10 * time.Minute):
        c.pending.Delete(uid)
        c.pendingRule.Delete(uid)
        klog.Warningf("审批超时 (UID: %s)，操作已取消", uid)
        c.answerCallback("", "操作超时，已自动取消")
        return false, fmt.Errorf("approval timeout")
    }
}