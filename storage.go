package main

import (
    "context"
    "fmt"
    "time"

    "go.mongodb.org/mongo-driver/bson"
    "go.mongodb.org/mongo-driver/mongo"
    "go.mongodb.org/mongo-driver/mongo/options"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/klog/v2"
)

func (c *Controller) InitMongo() error {
    // Set a timeout for the initial connection
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    client, err := mongo.Connect(ctx, options.Client().ApplyURI(c.config.Global.MongoDB.URI))
    if err != nil {
        return err
    }

    // Verify connection
    if err := client.Ping(ctx, nil); err != nil {
        return err
    }

    c.mongoClient = client
    return nil
}

func (c *Controller) getCachedUsername(userID int64) (string, error) {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("user_cache")

    var doc UserCacheDoc
    err := coll.FindOne(context.Background(), bson.M{"user_id": userID}).Decode(&doc)
    if err != nil {
        if err == mongo.ErrNoDocuments {
            return "", fmt.Errorf("no cache")
        }
        return "", err
    }

    // 缓存过期（1小时）
    if time.Since(doc.LastUpdated) > 1*time.Hour {
        return "", fmt.Errorf("cache expired")
    }

    if doc.Username == "" {
        return fmt.Sprintf("%d", userID), nil // fallback UserID
    }
    return doc.Username, nil
}

func (c *Controller) updateUserCache(userID int64, username string) error {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("user_cache")

    filter := bson.M{"user_id": userID}
    update := bson.M{
        "$set": bson.M{
            "username":     username,
            "last_updated": time.Now(),
        },
    }
    opts := options.Update().SetUpsert(true)

    _, err := coll.UpdateOne(context.Background(), filter, update, opts)
    return err
}

// 实时查询 Telegram username（使用 v5 库正确结构：ChatConfigWithUser）
func (c *Controller) fetchUsernameFromTelegram(userID int64, chatID int64) (string, error) {
    config := tgbotapi.ChatConfigWithUser{
        ChatID: chatID,      // interface{}，int64 兼容
        UserID: int(userID), // int 类型
    }

    member, err := c.tgBot.GetChatMember(config)
    if err != nil {
        return "", err
    }

    username := member.User.UserName
    return username, nil
}

func (c *Controller) backupSelectors(rule *RuleRuntime) error {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("backups")

    ctx := context.Background()

    for _, target := range rule.Config.TargetServices {
        svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
        if err != nil {
            return err
        }
        doc := BackupDoc{
            RuleDomain: rule.Config.Domain,
            Namespace:  target.Namespace,
            Service:    target.Name,
            Selector:   cloneMap(svc.Spec.Selector),
            Timestamp:  time.Now(),
        }
        _, err = coll.InsertOne(ctx, doc)
        if err != nil {
            return err
        }
    }
    return nil
}

func (c *Controller) restoreSelectors(rule *RuleRuntime) error {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("backups")

    ctx := context.Background()

    for _, target := range rule.Config.TargetServices {
        key := bson.M{
            "rule_domain": rule.Config.Domain,
            "namespace":   target.Namespace,
            "service":     target.Name,
        }

        var backup BackupDoc
        err := coll.FindOne(ctx, key, options.FindOne().SetSort(bson.M{"timestamp": -1})).Decode(&backup)
        if err != nil {
            if err == mongo.ErrNoDocuments {
                klog.Warningf("No backup found for %s/%s in rule %s", target.Namespace, target.Name, rule.Config.Domain)
                continue
            }
            return err
        }

        svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
        if err != nil {
            return err
        }
        svc.Spec.Selector = backup.Selector
        _, err = c.clientset.CoreV1().Services(target.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
        if err != nil {
            return err
        }
    }
    return nil
}

func (c *Controller) logEvent(ruleDomain, action, message string) {
    go func() {
        coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("events")
        doc := EventDoc{
            Timestamp:  time.Now(),
            RuleDomain: ruleDomain,
            Action:     action,
            Message:    message,
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _, err := coll.InsertOne(ctx, doc)
        if err != nil {
            klog.Errorf("Failed to log event to mongo: %v", err)
        }
    }()
}

func (c *Controller) StartCleanupTask() {
    go func() {
        ticker := time.NewTicker(24 * time.Hour)
        defer ticker.Stop()

        for range ticker.C {
            cutoff := time.Now().AddDate(0, 0, -c.config.Global.MongoDB.RetentionDays)
            db := c.mongoClient.Database(c.config.Global.MongoDB.Database)

            ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
            defer cancel()

            _, err1 := db.Collection("events").DeleteMany(ctx, bson.M{"timestamp": bson.M{"$lt": cutoff}})
            if err1 != nil {
                klog.Errorf("Cleanup events failed: %v", err1)
            }

            _, err2 := db.Collection("backups").DeleteMany(ctx, bson.M{"timestamp": bson.M{"$lt": cutoff}})
            if err2 != nil {
                klog.Errorf("Cleanup backups failed: %v", err2)
            }
        }
    }()
}

func cloneMap(m map[string]string) map[string]string {
    if m == nil {
        return nil
    }
    c := make(map[string]string, len(m))
    for k, v := range m {
        c[k] = v
    }
    return c
}