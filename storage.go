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
	"k8s.io/client-go/kubernetes" // Required for Controller.clientset
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
		doc := BackupDoc{ // BackupDoc is now defined in types.go
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

		var backup BackupDoc // BackupDoc is now defined in types.go
		// Using FindOne directly for simplicity when retrieving a single document
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
