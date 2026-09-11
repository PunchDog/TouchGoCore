package db

import (
	"bytes"
	"errors"

	"touchgocore/vars"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ==================== MongoDB GridFS / 批量操作 ====================

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
