package db

import (
	"context"
	"fmt"
	"time"

	"touchgocore/config"
	"touchgocore/db/dbmap"
	"touchgocore/vars"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// 默认数据库操作超时时间
const defaultDBTimeout = 5 * time.Second

// DbOperate 封装 MongoDB 操作
type DbOperate struct {
	session *mongo.Client
	dbName  string
	url     string
}

// newTimeoutContext 创建带超时的 context，替代 context.Background()
func newTimeoutContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), defaultDBTimeout)
}

func (db *DbOperate) GetDbSession() *mongo.Client {
	return db.session
}

// 使用有已有的连接资源
func (this *DbOperate) connectOnly(dataSourceName string) bool {
	if s, ok := dbmap.Global.Load(dataSourceName); ok {
		this.session = s.(*mongo.Client)
		return true
	}
	return false
}

func NewMongoDB(cfg *config.MongoDBConfig) (*DbOperate, error) {
	dbo := new(DbOperate)
	if err := dbo.newMongoDB(cfg); err != nil {
		return nil, err
	}
	return dbo, nil
}

func (dbo *DbOperate) newMongoDB(cfg *config.MongoDBConfig) error {
	var url string = ""
	if cfg.Username == "" && cfg.Password == "" {
		url = fmt.Sprintf(cfg.MongoUpUrl, cfg.Host, cfg.DBName)
	} else {
		url = fmt.Sprintf(cfg.MongoUpUrl, cfg.Username, cfg.Password, cfg.Host, cfg.DBName)
	}
	if cfg.ReplicaSetName != "" {
		url += fmt.Sprintf("?replicaSet=%s", cfg.ReplicaSetName)
	}

	vars.Info("DbOperate mongodb connecting host:%s db:%s", cfg.Host, cfg.DBName)

	dbo.dbName = cfg.DBName
	dbo.url = url

	//有连接直接用
	if dbo.connectOnly(url) {
		return nil
	}

	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbo.session, err = mongo.Connect(ctx, options.Client().ApplyURI(url))
	if err != nil {
		vars.Error("DbOperate Connect err:%v", err)
		return err
	}

	// 判断服务是不是可用
	pingCtx, pingCancel := newTimeoutContext()
	defer pingCancel()
	err = dbo.session.Ping(pingCtx, readpref.Primary())
	if err != nil {
		return err
	}

	//添加查询索引
	if len(cfg.InitDBTableIndex) > 0 {
		indexCtx, indexCancel := newTimeoutContext()
		defer indexCancel()
		opts := options.CreateIndexes().SetMaxTime(10 * time.Second)
		for _, table := range cfg.InitDBTableIndex {
			models := make([]mongo.IndexModel, 0)
			for _, str := range table.Index {
				models = append(models, mongo.IndexModel{
					Keys:    bson.D{{Key: str, Value: 1}},
					Options: options.Index().SetName(str),
				})
			}
			if _, err := dbo.session.Database(dbo.dbName).Collection(table.TableName).Indexes().CreateMany(indexCtx, models, opts); err != nil {
				vars.Error("创建MongoDB索引失败: table=%s, err=%v", table.TableName, err)
				return fmt.Errorf("创建MongoDB索引失败: %w", err)
			}
			vars.Info("创建MongoDB索引成功: table=%s, indexes=%v", table.TableName, table.Index)
		}
	}

	dbmap.Global.Store(url, dbo.session)
	vars.Info("DbOperate Connect %s mongodb...OK", dbo.url)
	return nil
}

func (dbo *DbOperate) DBClose() {
	if dbo.session != nil {
		ctx, cancel := newTimeoutContext()
		defer cancel()
		_ = dbo.session.Disconnect(ctx)
		if dbo.url != "" {
			dbmap.Global.Delete(dbo.url)
		}
		dbo.session = nil
		vars.Info("Disconnect mongodb host/db closed")
	}
}
