package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type DbOperate struct {
	session *mongo.Client
	dbName  string
	url     string
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

func NewMongoDB(cfg *config.MongoDBConfig) (dbo *DbOperate, err error) {
	dbo = new(DbOperate)
	dbo.newMongoDB(cfg)
	return dbo, err
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

	vars.Info("DbOperate mongodb connect url:%v.", url)

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
	err = dbo.session.Ping(context.Background(), readpref.Primary())
	if err != nil {
		return err
	}

	//添加查询索引
	if len(cfg.InitDBTableIndex) > 0 {
		opts := options.CreateIndexes().SetMaxTime(10 * time.Second)
		for _, table := range cfg.InitDBTableIndex {
			models := make([]mongo.IndexModel, 0)
			for _, str := range table.Index {
				models = append(models, mongo.IndexModel{
					Keys:    bson.D{{str, 1}},
					Options: options.Index().SetName(str),
				})
			}
			if _, err := dbo.session.Database(dbo.dbName).Collection(table.TableName).Indexes().CreateMany(context.Background(), models, opts); err != nil {
				panic(err)
			}
		}
	}

	_DbMap.Store(url, dbo.session)
	vars.Info("DbOperate Connect %s mongodb...OK", dbo.url)
	return nil
}

func (dbo *DbOperate) DBClose() {
	if dbo.session != nil {
		dbo.session.Disconnect(context.Background())
		dbo.session = nil
		vars.Info("Disconnect %s mongodb...", dbo.url)
	}
}

/* name 表名, doc 内容 */
func (dbo *DbOperate) Insert(name string, doc interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	c := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := c.InsertOne(context.Background(), doc)
	return err
}

/* name 表名,  cond 条件, change 内容 */
func (dbo *DbOperate) Update(name string, cond interface{}, change interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session..")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.UpdateOne(context.Background(), cond, bson.M{"$set": change})
	return err
}

// update && insert
/* name 表名,  cond 条件, doc 内容 */
func (dbo *DbOperate) UpdateInsert(name string, cond interface{}, doc interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.UpdateOne(context.Background(), cond, bson.M{"$set": doc}, options.Update().SetUpsert(true))
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

	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.DeleteOne(context.Background(), bson.M{cond_name: cond_value})

	return err
}

/* name 表名,  cond 条件 */
func (dbo *DbOperate) RemoveOneByCond(name string, cond interface{}) error {

	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteOne(context.Background(), cond, nil)

	return err

}

/* name 表名,  cond 条件 */
func (dbo *DbOperate) RemoveAll(name string, cond interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteMany(context.Background(), cond)
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

	collection := dbo.session.Database(dbo.dbName).Collection(name)

	var m bson.M
	err := collection.FindOne(context.Background(), query).Decode(&m)

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
	vars.Debug("[DbOperate.DBFindAll] dbo.dbName = %v, dbo.url= %v", dbo.dbName, dbo.url)
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	qCursor, err := collection.Find(context.Background(), query)

	vars.Debug("[DBFindAll] name:%s,query:%v, q:%b", name, query, qCursor)

	if err != nil {
		return err
	}

	for qCursor.TryNext(context.Background()) {
		if nil != resHandler {
			var doc bson.M
			qCursor.Decode(&doc)
			err = resHandler(doc)
			if nil != err {
				vars.Error("[DBFindAll] resHandler error :%v!!!", err)
				return err
			}
		}
	}

	return nil
}

//TODO
/* name 表名,  query 条件, resHandler 回调 , sortCond 排序, projection 筛选*/
func (dbo *DbOperate) DBFindAllEx(name string, query interface{}, resHandler func(*mongo.Cursor) error, sortCond string, projection interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)

	//sortCond 查询结果进行排序
	opts := options.Find()
	if sortCond != "" {
		opts = options.Find().SetSort(bson.D{{sortCond, -1}}).SetLimit(1)
	}
	if projection != nil {
		opts.SetProjection(projection)
	}
	qCursor, err := collection.Find(context.Background(), query, opts)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
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
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After).SetUpsert(upsert)
	err := collection.FindOneAndUpdate(context.Background(), query, change, opts).Decode(val)
	return err
}

/* name 表名,  query 条件, 不可传数组，需要再外面Decode */
func (dbo *DbOperate) FindAll(name string, query interface{}, resHandler func(*mongo.Cursor) error) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)
	qCursor, err := collection.Find(context.Background(), query)

	if err != nil && err != mongo.ErrNoDocuments {
		return err
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
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	return collection.FindOne(context.Background(), query).Decode(&ret)
}

/* name 表名,  query 条件 */
func (dbo *DbOperate) Delete(name string, query interface{}) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}
	collection := dbo.session.Database(dbo.dbName).Collection(name)
	_, err := collection.DeleteOne(context.Background(), query)

	return err
}

// gridfs //, dsc string
/* filename 文件名字, data 文件流 */
func (dbo *DbOperate) CreateGridFile(filename string, data []byte) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

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
	for qCursor.TryNext(context.Background()) {
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
	collection := dbo.session.Database(dbo.dbName).Collection(name)

	_, err := collection.InsertMany(context.Background(), documents)
	return err
}

/* name 表名, models 批量更新内容*/
func (dbo *DbOperate) BulkUpdate(name string, models []mongo.WriteModel) error {
	if dbo.session == nil {
		return errors.New("DbOperate Invalid session.")
	}

	collection := dbo.session.Database(dbo.dbName).Collection(name)
	opts := options.BulkWrite().SetOrdered(false).SetBypassDocumentValidation(true)
	_, err := collection.BulkWrite(context.Background(), models, opts)
	return err
}
