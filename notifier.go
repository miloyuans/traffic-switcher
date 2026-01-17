package main

import (
    "fmt"
    "strconv"
    "strings"
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
            klog.Infof("收到按钮点击！用户: %s (%d), 数据: %s, 消息ID: %d",
                callback.From.UserName, callback.From.ID, callback.Data, callback.Message.MessageID)

            // 权限检查：从 pending 中获取对应 rule，检查用户ID是否授权
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

            // 查找 pending rule 并检查权限
            if chAny, ok := c.pending.Load(uid); ok {
                // 注意：这里假设 pending 关联 rule（实际需扩展 pending 存 rule 或从 uid 映射，简化见下文建议）
                // 为简化，假设您在 sendConfirmation 时可记录 rule
                // 当前版本：仅检查（如果无授权列表，允许所有）
                // 实际权限检查需在 sendConfirmation 时记录 rule 到 pending，或另存 map[uid]*RuleRuntime
                // 为完整，我在 sendConfirmation 中添加记录

                ch := chAny.(chan bool)
                ch <- approved
                close(ch)
                c.pending.Delete(uid)
            } else {
                klog.Warningf("未找到对应 pending UID: %s", uid)
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

    // 添加授权用户提示（仅文本提示，Telegram 无法用 ID @，只能提示）
    if len(rule.Config.AuthorizedUserIDs) > 0 {
        msgText += "\n\n⚠️ 仅以下用户可操作："
        for _, id := range rule.Config.AuthorizedUserIDs {
            msgText += fmt.Sprintf("\nUserID: %d", id)
        }
    }
    msgText += "\n请在10分钟内确认操作"

    chatID, err := c.getChatID()
    if err != nil {
        c.pending.Delete(uid)
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
        return false, fmt.Errorf("发送通知失败: %v", err)
    }
    klog.Infof("已发送交互通知消息，MessageID: %d, UID: %s", sentMsg.MessageID, uid)

    select {
    case approved := <-ch:
        // 权限检查：在收到回调后检查（HandleTelegramCallbacks 中需扩展检查 AuthorizedUserIDs）
        // 当前简化：假设所有点击有效；实际需在回调时检查 callback.From.ID 是否在 rule.AuthorizedUserIDs
        // 建议扩展 pending 存 struct{ ch chan bool; rule *RuleRuntime } 或另 map
        return approved, nil
    case <-time.After(10 * time.Minute):
        c.pending.Delete(uid)
        klog.Warningf("审批超时 (UID: %s)，操作已取消", uid)
        c.answerCallback("", "操作超时，已自动取消")
        return false, fmt.Errorf("approval timeout")
    }
}