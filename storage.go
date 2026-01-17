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

	var doc UserCacheDoc // UserCacheDoc is now defined in types.go
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
	// FIX: In tgbotapi v5, GetChatMember expects a GetChatMemberConfig struct.
	// The UserID field within ChatConfigWithUser (which is embedded in GetChatMemberConfig)
	// must be of type int64.
	config := tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: userID, // Corrected: UserID must be int64 for tgbotapi v5
		},
	}

	member, err := c.tgBot.GetChatMember(config) // Corrected: Pass GetChatMemberConfig
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

		// 优化：备份前记录当前 Selector，并根据是否有 original_selector 输出不同日志
		currentSelector := cloneMap(svc.Spec.Selector)

		ruleIdentifier := rule.Config.BaseDomain
		if ruleIdentifier == "" {
			ruleIdentifier = rule.Config.Domain // 兼容旧配置
		}

		if len(target.OriginalSelector) > 0 {
			klog.Infof("【备份】rule %s → Service %s/%s 当前 Selector: %v （恢复时将优先使用配置的 original_selector: %v）",
				ruleIdentifier, target.Namespace, target.Name, currentSelector, target.OriginalSelector)
		} else {
			klog.Infof("【备份】rule %s → Service %s/%s 当前 Selector: %v （恢复时将使用此备份）",
				ruleIdentifier, target.Namespace, target.Name, currentSelector)
		}

		doc := BackupDoc{
			RuleDomain: ruleIdentifier, // 使用 BaseDomain 或 Domain 作为标识
			Namespace:  target.Namespace,
			Service:    target.Name,
			Selector:   currentSelector,
			Timestamp:  time.Now(),
		}
		_, err = coll.InsertOne(ctx, doc)
		if err != nil {
			return err
		}

		klog.Infof("【备份成功】Service %s/%s 当前 Selector 已备份: %v", target.Namespace, target.Name, currentSelector)
	}
	return nil
}

func (c *Controller) restoreSelectors(rule *RuleRuntime) error {
	coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("backups")

	ctx := context.Background()

	for _, target := range rule.Config.TargetServices {
		ruleIdentifier := rule.Config.BaseDomain
		if ruleIdentifier == "" {
			ruleIdentifier = rule.Config.Domain
		}

		var selectorToRestore map[string]string

		// 优先级1：使用该 Service 配置的 original_selector（硬编码正确值）
		if len(target.OriginalSelector) > 0 {
			selectorToRestore = cloneMap(target.OriginalSelector)
			klog.Infof("【恢复优先使用配置】rule %s → Service %s/%s 使用配置文件中硬编码的 original_selector: %v",
				ruleIdentifier, target.Namespace, target.Name, selectorToRestore)
		} else {
			// 优先级2：使用 Mongo 中的最新备份
			key := bson.M{
				"rule_domain": ruleIdentifier,
				"namespace":   target.Namespace,
				"service":     target.Name,
			}

			var backup BackupDoc
			err := coll.FindOne(ctx, key, options.FindOne().SetSort(bson.M{"timestamp": -1})).Decode(&backup)
			if err != nil {
				if err == mongo.ErrNoDocuments {
					klog.Warningf("【恢复警告】rule %s → Service %s/%s 无备份且无 original_selector 配置，跳过恢复（保持当前 Selector 不变）",
						ruleIdentifier, target.Namespace, target.Name)
					continue
				}
				return err
			}

			selectorToRestore = cloneMap(backup.Selector)
			klog.Infof("【恢复使用备份】rule %s → Service %s/%s 使用 Mongo 备份 Selector: %v",
				ruleIdentifier, target.Namespace, target.Name, selectorToRestore)
		}

		svc, err := c.clientset.CoreV1().Services(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		oldSelector := cloneMap(svc.Spec.Selector)

		// 明确先清空 Selector（确保无残留旧 key），再全量覆盖
		svc.Spec.Selector = nil
		svc.Spec.Selector = selectorToRestore

		_, err = c.clientset.CoreV1().Services(target.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		klog.Infof("【恢复成功】Service %s/%s Selector 从 %v → %v", target.Namespace, target.Name, oldSelector, selectorToRestore)
	}
	return nil
}

func (c *Controller) logEvent(ruleDomain, action, message string) {
	// Logging events can often be fire-and-forget, making it non-blocking with a goroutine.
	// Context with timeout prevents indefinite hanging on a failing DB connection.
	go func() {
		coll := c.mongoClient.Database(c.config.Global.MongoDB.Database).Collection("events")
		doc := EventDoc{ // EventDoc is now defined in types.go
			Timestamp:  time.Now(),
			RuleDomain: ruleDomain,
			Action:     action,
			Message:    message,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel() // Ensure the context is cancelled when the goroutine exits

		_, err := coll.InsertOne(ctx, doc)
		if err != nil {
			klog.Errorf("Failed to log event to mongo: %v", err)
		}
	}()
}

func (c *Controller) StartCleanupTask() {
	// Run cleanup in a separate goroutine to avoid blocking the main application flow
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop() // Ensure the ticker is stopped when the goroutine exits

		for range ticker.C {
			cutoff := time.Now().AddDate(0, 0, -c.config.Global.MongoDB.RetentionDays)
			db := c.mongoClient.Database(c.config.Global.MongoDB.Database)

			// Create a new context for each cleanup iteration with its own timeout
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel() // Defer cancel for this specific context within the loop

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
