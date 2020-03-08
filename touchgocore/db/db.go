package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	_ "github.com/go-sql-driver/mysql"
)

var connectLock sync.Mutex

type DBConfigModel struct {
	Host          string
	User          string
	Password      string
	DBName        string
	AutoCloseTime int
	MaxOpenConns  int
	MaxIdleConns  int
}

var MysqlDbMap map[string]*sql.DB

type Condition struct {
	whereSql string
	args     []interface{}
}

// 设置搜索条件
// key 字段名称
// ex  判断表达式 可以是 = , >, >=, <, <=, !=
// val 值, 如果是int或者string则表示等于; 如果是[]int 或者 []string 则表示in查询 其他类型不支持(如果ex不等于"=" 那么仅仅支持int 和 string)
func (this *Condition) SetFilterEx(key string, ex string, val interface{}) error {
	sql := ""
	if strings.Index(key, "`") != -1 || strings.Index(key, ".") != -1 {
		sql += key
	} else {
		sql += "`" + key + "`"
	}
	args := []interface{}{}
	switch val.(type) {
	default:
		sql += " " + ex + " ?"
		args = append(args, val)
	case string:
		sql += " " + ex + " '?'"
		args = append(args, val)
	case []interface{}:
		sql += " " + ex + " ("
		strAry := val.([]interface{})
		for i, v := range strAry {
			if i == 0 {
				sql += "?"
			} else {
				sql += ",?"
			}
			args = append(args, v)
		}
		sql += ")"
	case []string:
		sql += " in ("
		strAry := val.([]string)
		for i, v := range strAry {
			if i == 0 {
				sql += "'?'"
			} else {
				sql += ",'?'"
			}
			args = append(args, v)
		}
		sql += ")"
	case []int:
		sql += " in ("
		strAry := val.([]int)
		for i, v := range strAry {
			if i == 0 {
				sql += "?"
			} else {
				sql += ",?"
			}
			args = append(args, v)
		}
		sql += ")"
	case []int64:
		sql += " in ("
		strAry := val.([]int64)
		for i, v := range strAry {
			if i == 0 {
				sql += "?"
			} else {
				sql += ",?"
			}
			args = append(args, v)
		}
		sql += ")"
	case []float64:
		sql += " in ("
		strAry := val.([]float64)
		for i, v := range strAry {
			if i == 0 {
				sql += "?"
			} else {
				sql += ",?"
			}
			args = append(args, v)
		}
		sql += ")"
	}
	if len(this.whereSql) == 0 {
		this.whereSql = sql
	} else {
		this.whereSql += " and " + sql
	}

	if len(args) != 0 {
		this.args = append(this.args, args...)
	}
	return nil
}

// 设置搜索条件
// key 字段名称
// val 值, 如果是int或者string则表示等于; 如果是[]int 或者 []string 则表示in查询 其他类型不支持
func (this *Condition) SetFilter(key string, val interface{}) *Condition {
	this.SetFilterEx(key, "=", val)
	return this
}

func (this *Condition) SetFilterOr(conditions ...*Condition) {
	for _, condition := range conditions {
		sql, args := condition.getSql()
		if len(this.whereSql) == 0 {
			this.whereSql = "(" + sql + ")"
		} else {
			this.whereSql += " or (" + sql + ")"
		}
		this.args = append(this.args, args...)
	}
}

func (this *Condition) getSql() (string, []interface{}) {
	return this.whereSql, this.args
}

//返回结果
type Rows struct {
	row       *map[string]interface{}
	rows      *[]map[string]interface{}
	row_index int
}

func (this *Rows) Next() error {
	if this.rows == nil {
		return &DatabaseError{"返回多个数据才能使用"}
	}
	if len(*this.rows) == 0 {
		return &DatabaseError{"没有查询到数据"}
	}
	if this.row != nil {
		this.row_index++
		if this.row_index >= len(*this.rows) {
			return &DatabaseError{"已经是结果最后"}
		}
	}
	this.row = &(*this.rows)[this.row_index]
	return nil
}

func (this *Rows) GetInt(key string) int64 {
	if this.row != nil {
		if (*this.row)[key] != nil {
			val, _ := strconv.ParseInt((*this.row)[key].(string), 10, 64)
			return val
		}
	}
	return 0
}

func (this *Rows) GetFloat(key string) float64 {
	if this.row != nil {
		if (*this.row)[key] != nil {
			val, _ := strconv.ParseFloat((*this.row)[key].(string), 64)
			return val
		}
	}
	return 0
}

func (this *Rows) ForMap(fn func(k string, v interface{})) {
	for key, val := range *this.row {
		fn(key, val)
	}
}

func (this *Rows) GetString(key string) string {
	if this.row != nil {
		if (*this.row)[key] != nil {
			return (*this.row)[key].(string)
		}
	}
	return ""
}

type DBResult struct {
	values []map[string]interface{}
}

func (this *DBResult) Count() int {
	return len(this.values)
}

func (this *DBResult) GetOne() *Rows {
	return &Rows{row: &this.values[0]}
}

func (this *DBResult) GetAll() *Rows {
	return &Rows{rows: &this.values}
}

type DbMysql struct {
	db        *sql.DB                   // 数据库连接对象
	config    *DBConfigModel            //
	values    *map[string](interface{}) //数据对象
	condition *Condition                //条件
	Result    *DBResult                 //返回结果
	tableName string                    //表名
	order     string                    //排序设置
	limit     string
	group     string
}

func NewDbMysql(config *DBConfig) (*DbMysql, error) {
	this := new(DbMysql)
	configModel := &DBConfigModel{config.Host, config.Username, config.Password, config.Name, 0, config.MaxIdleConns, config.MaxOpenConns}
	this.config = configModel
	this.values = nil
	this.condition = nil
	this.order = ""
	return this, this.connect()
}

//获取配置
func (this *DbMysql) GetConfig() *DBConfigModel {
	return this.config
}

// 数据库连接
func (this *DbMysql) connect() error {
	if MysqlDbMap == nil {
		MysqlDbMap = make(map[string]*sql.DB)
	}

	// 从配置文件中读取配置信息并初始化连接池(go中含有连接池处理机制)
	connStr := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&loc=Local&charset=utf8", this.config.User, this.config.Password, this.config.Host, this.config.DBName)
	if this.connectOnly(connStr) {
		// 如果同事还有其他协程创建连接成功了
		return nil
	}

	// 锁住,然后创建连接
	connectLock.Lock()
	defer connectLock.Unlock()
	if this.connectOnly(connStr) {
		// 如果同事还有其他协程创建连接成功了
		return nil
	}

	db, err := sql.Open("mysql", connStr)
	if err != nil {
		return err
	}

	if err := db.Ping(); err != nil {
		return err
	}

	if this.config.MaxIdleConns > 0 {
		db.SetMaxIdleConns(this.config.MaxIdleConns)
	}
	if this.config.MaxOpenConns > 0 {
		db.SetMaxOpenConns(this.config.MaxOpenConns)
	}
	if this.config.AutoCloseTime > 0 {
		db.SetConnMaxLifetime(time.Duration(this.config.AutoCloseTime) * time.Second)
	} else {
		db.SetConnMaxLifetime(time.Second * 2400) //保活10天
	}

	MysqlDbMap[connStr] = db
	this.db = db
	//Log.Println("连接数据库成功")
	return nil
}

// 使用有已有的连接资源
func (this *DbMysql) connectOnly(dataSourceName string) bool {
	if db, ok := MysqlDbMap[dataSourceName]; ok {
		this.db = db
		return true
	}
	return false
}

func (this *DbMysql) Ping() error {
	return this.db.Ping()
}

func (this *DbMysql) Close() {
	this.db.Close()
	this.db = nil
}

/**
 * 设置表名
 */
func (this *DbMysql) SetTableName(tableName string) *DbMysql {
	if strings.Index(tableName, "`") == -1 && strings.Index(tableName, ".") == -1 && strings.Index(tableName, " ") == -1 {
		tableName = "`" + tableName + "`"
	}
	this.tableName = tableName
	this.values = nil
	return this
}

/**
 * 设置数据对象(需要查询的键值或者更新插入的键值key/value,如果是查询，value不填)
 */
func (this *DbMysql) SetDataMap(data *map[string](interface{})) *DbMysql {
	this.values = data
	return this
}

/**
 * 设置数据对象(需要查询的键值或者更新插入的键值key/value,如果是查询，value不填)
 */
func (this *DbMysql) SetDataMapByOne(key string, value interface{}) *DbMysql {
	if this.values == nil {
		this.values = &map[string](interface{}){}
	}
	(*this.values)[key] = value
	return this
}

/**
 * 设置查询条件
 */
func (this *DbMysql) SetCondition(condition *Condition) *DbMysql {
	this.condition = condition
	return this
}

//排序顺序
func (this *DbMysql) Order(order string) *DbMysql {
	this.order = order
	return this
}

//数据分页
func (this *DbMysql) Limit(limit ...int) *DbMysql {
	tmp := make([]string, len(limit))
	for i, v := range limit {
		tmp[i] = strconv.Itoa(v)
	}
	this.limit = strings.Join(tmp, ",")
	return this
}

//数据分组
func (this *DbMysql) Group(field string) *DbMysql {
	this.group = field
	return this
}

type eQueryType int

const (
	eQueryType_Normarl eQueryType = iota
	eQueryType_Count
	eQueryType_Sum
	eQueryType_Max
)

//获取查询语句
func (this *DbMysql) getQueryStatement(etype eQueryType, rowname string) string {
	strSql := "select * from " + this.tableName
	if etype == eQueryType_Normarl {
		if this.values != nil {
			keys := []string{}
			for key, _ := range *this.values {
				keys = append(keys, key)
			}
			strSql = "select " + strings.Join(keys, ",") + " from " + this.tableName
		}
	} else if etype == eQueryType_Count {
		strSql = "select count(*) from " + this.tableName
	} else if etype == eQueryType_Sum {
		strSql = "select sum(" + rowname + ") from " + this.tableName
	} else if etype == eQueryType_Max {
		strSql = "select max(" + rowname + ") from " + this.tableName
	}

	var args []interface{}
	if this.condition != nil {
		var wheresql string
		wheresql, args = this.condition.getSql()
		strSql += " where " + wheresql
	}

	if this.group != "" {
		strSql += " group by " + this.group
	}

	if this.order != "" {
		strSql += " order by " + this.order
	}
	if this.limit != "" {
		strSql += " limit " + this.limit
	}

	return util.Sprintf(strSql, args...)
}

//查询
func (this *DbMysql) Query() string {
	return this.getQueryStatement(eQueryType_Normarl, "")
}

func (this *DbMysql) QueryCount() string {
	return this.getQueryStatement(eQueryType_Count, "")
}

func (this *DbMysql) QuerySum(rowname string) string {
	return this.getQueryStatement(eQueryType_Sum, rowname)
}

func (this *DbMysql) QueryMax(rowname string) string {
	return this.getQueryStatement(eQueryType_Max, rowname)
}

func (this *DbMysql) QueryExec(strSql string) (*DBResult, error) {
	rows, err := this.db.Query(strSql)

	if err != nil {
		vars.Error(err.Error())
		return nil, &DatabaseError{"查询语句出错"}
	}
	defer rows.Close()
	cloumns, err := rows.Columns()
	if err != nil {
		vars.Error(err.Error())
		return nil, &DatabaseError{"获取关键字出错"}
	}

	values := make([]sql.RawBytes, len(cloumns))
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	this.Result = &DBResult{}
	for rows.Next() {
		err = rows.Scan(scanArgs...)
		if err != nil {
			vars.Error(err.Error())
			continue
		}
		var datas map[string]interface{} = make(map[string]interface{})
		for i, col := range values {
			datas[cloumns[i]] = string(col)
		}
		this.Result.values = append(this.Result.values, datas)
	}
	if err = rows.Err(); err != nil {
		vars.Error(err.Error())
	}
	return this.Result, nil
}

//添加
func (this *DbMysql) Insert() string {
	if this.values == nil {
		return "没有要插入的数据"
	}
	fields := []string{}
	values := []string{}
	exeArgs := []interface{}{}
	for key, val := range *this.values {
		fields = append(fields, key)
		switch val.(type) {
		case string:
			values = append(values, "'?'")
		default:
			values = append(values, "?")
		}
		exeArgs = append(exeArgs, val)
	}
	sql := "insert into " + this.tableName + " (`" + strings.Join(fields, "`,`") + "`) values (" + strings.Join(values, ",") + ")"
	return util.Sprintf(sql, exeArgs...)
}

//更新
func (this *DbMysql) Update() string {
	if this.values == nil {
		return "没有要修改的数据"
	}
	strsql := "update " + this.tableName + " set "
	idx := 0
	var args []interface{}
	for key, val := range *this.values {
		if idx != 0 {
			strsql += ","
		}
		switch val.(type) {
		case string:
			strsql += "`" + key + "`='?'"
		default:
			strsql += "`" + key + "`=?"
		}
		args = append(args, val)
		idx++
	}

	if this.condition != nil {
		wheresql, args1 := this.condition.getSql()
		strsql += " where " + wheresql
		args = append(args, args1...)
	}

	return util.Sprintf(strsql, args...)
}

//删除
func (this *DbMysql) Del() string {
	if this.condition == nil {
		return "没有删除条件"
	}
	wheresql, args := this.condition.getSql()
	sql := "delete from " + this.tableName + " where " + wheresql
	return util.Sprintf(sql, args...)
}

//执行
func (this *DbMysql) Exec(strsql string, args ...interface{}) error {
	_, err := this.db.Exec(strsql, args...)
	if err != nil {
		return err
	}
	return err
}
