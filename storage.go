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
    client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(c.config.Global.MongoDB.URI))
    if err != nil {
        return err
    }
    c.mongoClient = client
    return nil
}

func (c *Controller) getCachedUsername(userID int64) (string, error) {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("user_cache")

    var doc UserCacheDoc
    err := coll.FindOne(context.TODO(), bson.M{"user_id": userID}).Decode(&doc)
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

    _, err := coll.UpdateOne(context.TODO(), filter, update, opts)
    return err
}

// 实时查询 Telegram username（修复为库 v5 正确结构）
func (c *Controller) fetchUsernameFromTelegram(userID int64, chatID int64) (string, error) {
    config := tgbotapi.ChatMemberConfig{
        ChatConfig: tgbotapi.ChatConfig{
            ChatID: chatID, // ChatID 是嵌套在 ChatConfig 中的 interface{}
        },
        UserID: int(userID), // UserID 是 int 类型
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
    for _, target := range rule.Config.TargetServices {
        svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(context.TODO(), target.Name, metav1.GetOptions{})
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
        _, err = coll.InsertOne(context.TODO(), doc)
        if err != nil {
            return err
        }
    }
    return nil
}

func (c *Controller) restoreSelectors(rule *RuleRuntime) error {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("backups")
    for _, target := range rule.Config.TargetServices {
        key := bson.M{
            "rule_domain": rule.Config.Domain,
            "namespace":   target.Namespace,
            "service":     target.Name,
        }
        opt := options.Find().
            SetSort(bson.M{"timestamp": -1}).
            SetLimit(1)

        cursor, err := coll.Find(context.TODO(), key, opt)
        if err != nil {
            return err
        }
        if !cursor.Next(context.TODO()) {
            klog.Warningf("No backup found for %s/%s in rule %s", target.Namespace, target.Name, rule.Config.Domain)
            continue
        }
        var backup BackupDoc
        if err := cursor.Decode(&backup); err != nil {
            return err
        }
        svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(context.TODO(), target.Name, metav1.GetOptions{})
        if err != nil {
            return err
        }
        svc.Spec.Selector = backup.Selector
        _, err = c.clientset.CoreV1().Services(target.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
        if err != nil {
            return err
        }
    }
    return nil
}

func (c *Controller) logEvent(ruleDomain, action, message string) {
    coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("events")
    doc := EventDoc{
        Timestamp:  time.Now(),
        RuleDomain: ruleDomain,
        Action:     action,
        Message:    message,
    }
    coll.InsertOne(context.TODO(), doc)
}

func (c *Controller) StartCleanupTask() {
    ticker := time.NewTicker(24 * time.Hour)
    for range ticker.C {
        cutoff := time.Now().AddDate(0, 0, -c.config.Global.MongoDB.RetentionDays)
        db := c.mongoClient.Database(c.config.Global.MongoDB.Database)
        db.Collection("events").DeleteMany(context.TODO(), bson.M{"timestamp": bson.M{"$lt": cutoff}})
        db.Collection("backups").DeleteMany(context.TODO(), bson.M{"timestamp": bson.M{"$lt": cutoff}})
    }
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