package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"touchgocore/config"
	"touchgocore/vars"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// 默认数据库操作超时时间
const defaultDBTimeout = 5 * time.Second

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
	if s, ok := _DbMap.Load(dataSourceName); ok {
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
					Keys:    bson.D{{str, 1}},
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

	_DbMap.Store(url, dbo.session)
	vars.Info("DbOperate Connect %s mongodb...OK", dbo.url)
	return nil
}

func (dbo *DbOperate) DBClose() {
	if dbo.session != nil {
		ctx, cancel := newTimeoutContext()
		defer cancel()
		_ = dbo.session.Disconnect(ctx)
		if dbo.url != "" {
			_DbMap.Delete(dbo.url)
		}
		dbo.session = nil
		vars.Info("Disconnect mongodb host/db closed")
	}
}

/* name 表名, doc 内容 */
func (dbo *DbOperate) Insert(name string, doc interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	c := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := c.InsertOne(ctx, doc)
	return err
}

/* name 表名,  cond 条件, change 内容 */
func (dbo *DbOperate) Update(name string, cond interface{}, change interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session..")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.UpdateOne(ctx, cond, bson.M{"$set": change})
	return err
}

// update && insert
/* name 表名,  cond 条件, doc 内容 */
func (dbo *DbOperate) UpdateInsert(name string, cond interface{}, doc interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.UpdateOne(ctx, cond, bson.M{"$set": doc}, options.Update().SetUpsert(true))
	if nil != err {
		vars.Error("UpdateInsert failed name is:%s. cond is:%v", name, cond)
	}

	return err
}

/* name 表名,  cond_name 字段名, cond_value 字段值 */
func (dbo *DbOperate) RemoveOne(name string, cond_name string, cond_value int64) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.DeleteOne(ctx, bson.M{cond_name: cond_value})

	return err
}

/* name 表名,  cond 条件 */
func (dbo *DbOperate) RemoveOneByCond(name string, cond interface{}) error {

	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteOne(ctx, cond, nil)

	return err

}

/* name 表名,  cond 条件 */
func (dbo *DbOperate) RemoveAll(name string, cond interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteMany(ctx, cond)
	if nil != err && mongo.ErrNilDocument != err {
		vars.Debug("DbOperate.RemoveAll failed : %s, %v", name, cond)
		return err
	}
	//vars.Debug("DbOperate.RemoveAll: %v", change)
	return nil
}

//TODO
/* name 表名,  query 条件, resHandler 回调*/
func (dbo *DbOperate) DBFindOne(name string, query interface{}, resHandler func(bson.M) error) error {
	if dbo.session == nil {
		return errors.New("DBFindOne Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	var m bson.M
	err := collection.FindOne(ctx, query).Decode(&m)

	if err != nil {
		return err
	}

	if nil != resHandler {
		return resHandler(m)
	}

	return nil

}

/* name 表名,  query 条件, resHandler 回调*/
func (dbo *DbOperate) DBFindAll(name string, query interface{}, resHandler func(bson.M) error) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	vars.Debug("[DbOperate.DBFindAll] dbo.dbName = %v, dbo.url= %v", dbo.dbName, dbo.url)
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	qCursor, err := collection.Find(ctx, query)
	if err != nil {
		return err
	}
	defer qCursor.Close(ctx)

	vars.Debug("[DBFindAll] name:%s,query:%v, q:%v", name, query, qCursor)

	for qCursor.TryNext(ctx) {
		if nil != resHandler {
			var doc bson.M
			if err = qCursor.Decode(&doc); err != nil {
				vars.Error("[DBFindAll] Decode error: %v", err)
				return err
			}
			err = resHandler(doc)
			if nil != err {
				vars.Error("[DBFindAll] resHandler error :%v!!!", err)
				return err
			}
		}
	}

	return nil
}

/* name 表名,  query 条件, resHandler 回调 , sortCond 排序, projection 筛选*/
func (dbo *DbOperate) DBFindAllEx(name string, query interface{}, resHandler func(*mongo.Cursor) error, sortCond string, projection interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	//sortCond 查询结果进行排序
	opts := options.Find()
	if sortCond != "" {
		opts = options.Find().SetSort(bson.D{{sortCond, -1}}).SetLimit(1)
	}
	if projection != nil {
		opts.SetProjection(projection)
	}
	qCursor, err := collection.Find(ctx, query, opts)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	if qCursor != nil {
		defer qCursor.Close(ctx)
	}

	err = qCursor.Err()
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}

	if nil != resHandler {
		return resHandler(qCursor)
	}
	return nil
}

/* name 表名,  query 条件, change 内容, upsert 插入(没有时), val 返回值*/
func (dbo *DbOperate) FindAndModify(name string, query interface{}, change interface{}, upsert bool, val interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After).SetUpsert(upsert)
	err := collection.FindOneAndUpdate(ctx, query, change, opts).Decode(val)
	return err
}

/* name 表名,  query 条件, 不可传数组，需要再外面Decode */
func (dbo *DbOperate) FindAll(name string, query interface{}, resHandler func(*mongo.Cursor) error) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	qCursor, err := collection.Find(ctx, query)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	if qCursor != nil {
		defer qCursor.Close(ctx)
	}

	err = qCursor.Err()
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}

	if nil != resHandler {
		return resHandler(qCursor)
	}
	return nil
}

/* name 表名,  query 条件, ret 返回内容 */
func (dbo *DbOperate) FindOne(name string, query interface{}, ret interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	return collection.FindOne(ctx, query).Decode(ret)
}

/* name 表名,  query 条件 */
func (dbo *DbOperate) Delete(name string, query interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteOne(ctx, query)

	return err
}

// gridfs //, dsc string
/* filename 文件名字, data 文件流 */
func (dbo *DbOperate) CreateGridFile(filename string, data []byte) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	vars.Debug("[DbOperate.CreateGridFile] dbo.dbName:%v filename:%v", dbo.dbName, filename)
	bucket, err := gridfs.NewBucket(dbo.session.Database(dbo.dbName))

	if err != nil {
		vars.Debug("[CreateGridFile] gridfs.NewBucket  err%v", err)
		return err
	}

	//新做一个桶
	fileId, err := bucket.UploadFromStream(filename, bytes.NewBuffer(data))

	//查找老桶 并删除
	filter := bson.M{"filename": filename, "_id": bson.M{"$ne": fileId}}
	qCursor, err := bucket.Find(filter)
	if err != nil {
		vars.Error("[CreateGridFile] bucket.Find(%s) err = %+v ", filename, err)
		return err
	}
	defer qCursor.Close(ctx)
	for qCursor.TryNext(ctx) {
		var doc bson.M
		qCursor.Decode(&doc)
		primid := doc["_id"].(primitive.ObjectID)
		bucket.Delete(primid)
	}
	return err
}

// gridfs //, dsc string
/* filename 文件名字 返回 文件流*/
func (dbo *DbOperate) OpenGridFile(filename string) ([]byte, error) {

	if dbo.session == nil {
		vars.Debug("[DbOperate.OpenGridFile] name:%s,dbo.session == nil", filename)
		return nil, errors.New("DbOperate Invalid session.")
	}
	bucket, err := gridfs.NewBucket(dbo.session.Database(dbo.dbName))

	if err != nil {
		vars.Debug("[DbOperate.OpenGridFile] NewBucket  name:%s,err%v", filename, err)
		return nil, err
	}

	var buf bytes.Buffer
	_, err = bucket.DownloadToStreamByName(filename, &buf)

	if err != nil {
		vars.Debug("[OpenGridFile] DownloadToStreamByName  name:%s,err%v", filename, err)
		return nil, err
	}

	return buf.Bytes(), nil
}

/* name 表名, documents 批量内容*/
func (dbo *DbOperate) BulkInsert(name string, documents []interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.InsertMany(ctx, documents)
	return err
}

/* name 表名, models 批量更新内容*/
func (dbo *DbOperate) BulkUpdate(name string, models []mongo.WriteModel) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	ctx, cancel := newTimeoutContext()
	defer cancel()
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	opts := options.BulkWrite().SetOrdered(false).SetBypassDocumentValidation(true)
	_, err := collection.BulkWrite(ctx, models, opts)
	return err
}
