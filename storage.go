package main

import (
    "context"
    "time"

    "go.mongodb.org/mongo-driver/bson"
    "go.mongodb.org/mongo-driver/mongo"
    "go.mongodb.org/mongo-driver/mongo/options"

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
        // 修复：使用 bson.M 进行降序排序（-1）
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