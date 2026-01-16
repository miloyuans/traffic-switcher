package main

import (
    "fmt"
    "time"

    "github.com/google/uuid"                                      // 解决 undefined: uuid
    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"  // tgbotapi
    "k8s.io/klog/v2"
)

func (c *Controller) InitTelegram() error {
    bot, err := tgbotapi.NewBotAPI(c.config.Global.Telegram.BotToken)
    if err != nil {
        return err
    }
    c.tgBot = bot
    return nil
}

func (c *Controller) HandleTelegramCallbacks() {
    u := tgbotapi.NewUpdate(0)
    u.Timeout = 30
    updates := c.tgBot.GetUpdatesChan(u)

    for update := range updates {
        if update.CallbackQuery == nil {
            continue
        }
        callback := update.CallbackQuery
        data := callback.Data
        approved := false
        text := "已拒绝"
        if dataHasPrefix(data, "approve:") {
            approved = true
            text = "已确认"
        } else if dataHasPrefix(data, "reject:") {
            approved = false
        } else {
            continue
        }

        uid := data[8:] // remove prefix
        if chAny, ok := c.pending.Load(uid); ok {
            ch := chAny.(chan bool)
            ch <- approved
            c.pending.Delete(uid)
        }

        c.tgBot.Send(tgbotapi.NewCallback(callback.ID, text))
    }
}

func (c *Controller) sendConfirmation(rule *RuleRuntime, action, reason string) (bool, error) {
    uid := uuid.New().String()
    ch := make(chan bool, 1)
    c.pending.Store(uid, ch)

    msgText := fmt.Sprintf("%s\n规则: %s\n原因: %s\n请确认？", action, rule.Config.Domain, reason)
    if rule.Config.NotificationMessage != "" {
        msgText = rule.Config.NotificationMessage + "\n" + msgText
    }

    msg := tgbotapi.NewMessage(c.config.Global.Telegram.ChatID, msgText)
    keyboard := tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("✅ 确认", "approve:"+uid),
            tgbotapi.NewInlineKeyboardButtonData("❌ 拒绝", "reject:"+uid),
        ),
    )
    msg.ReplyMarkup = keyboard
    _, err := c.tgBot.Send(msg)
    if err != nil {
        return false, err
    }

    select {
    case approved := <-ch:
        return approved, nil
    case <-time.After(10 * time.Minute):
        c.pending.Delete(uid)
        return false, fmt.Errorf("timeout")
    }
}

func dataHasPrefix(data, prefix string) bool {
    return len(data) > len(prefix) && data[:len(prefix)] == prefix
}