package main

import (
    "fmt"
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

func (c *Controller) HandleTelegramCallbacks() {
    // 永久循环 + 自动重连
    for {
        u := tgbotapi.NewUpdate(0)
        u.Timeout = 60 // 长轮询超时 60s，更稳定

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

            data := callback.Data
            var approved bool
            var text string
            var prefixLen int

            if strings.HasPrefix(data, "approve:") {
                approved = true
                text = "✅ 已确认"
                prefixLen = len("approve:")
            } else if strings.HasPrefix(data, "reject:") {
                approved = false
                text = "❌ 已拒绝"
                prefixLen = len("reject:")
            } else {
                klog.Warningf("未知回调数据: %s", data)
                // 回复未知错误
                c.answerCallback(callback.ID, "无效操作")
                continue
            }

            uid := data[prefixLen:]
            klog.Infof("解析动作: %s, UID: %s", text, uid)

            if chAny, ok := c.pending.Load(uid); ok {
                ch := chAny.(chan bool)
                ch <- approved
                close(ch) // 防止泄漏
                c.pending.Delete(uid)
                klog.Infof("已发送审批结果到通道: approved=%v", approved)
            } else {
                klog.Warningf("未找到对应 pending UID: %s (可能已超时或重复点击)", uid)
            }

            // 必须回复 Telegram 确认回调（否则按钮可能不更新）
            c.answerCallback(callback.ID, text)
        }

        // 如果通道关闭（网络错误等），日志并重连
        klog.Errorf("Telegram 长轮询通道意外关闭，正在重连...")
        time.Sleep(5 * time.Second) // 避免急速重连
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

func (c *Controller) sendConfirmation(rule *RuleRuntime, action, reason string) (bool, error) {
    uid := uuid.New().String()
    ch := make(chan bool, 1)
    c.pending.Store(uid, ch)

    msgText := fmt.Sprintf("%s\n规则域名: %s\n原因: %s\n请在10分钟内确认操作", action, rule.Config.Domain, reason)
    if rule.Config.NotificationMessage != "" {
        msgText = rule.Config.NotificationMessage + "\n\n" + msgText
    }

    msg := tgbotapi.NewMessage(c.config.Global.Telegram.ChatID, msgText)
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
        return approved, nil
    case <-time.After(10 * time.Minute):
        c.pending.Delete(uid)
        klog.Warningf("审批超时 (UID: %s)，操作已取消", uid)
        c.answerCallback("", "操作超时，已自动取消") // 可选：尝试编辑原消息
        return false, fmt.Errorf("approval timeout")
    }
}