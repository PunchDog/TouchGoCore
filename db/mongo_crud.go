package db

import (
	"errors"

	"touchgocore/vars"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ==================== MongoDB CRUD ====================

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
		opts = options.Find().SetSort(bson.D{{Key: sortCond, Value: -1}}).SetLimit(1)
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
