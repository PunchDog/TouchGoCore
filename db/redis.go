package db

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/go-redis/redis"
)

var RedisDbMap *sync.Map = &sync.Map{}

type RedisConfigModel struct {
	Host     string
	Db       int
	Password string
}

type Redis struct {
	redisClient  *redis.Client
	redisLockCnt *sync.Map
	config       *RedisConfigModel
}

func NewRedis(config *util.RedisConfig) (*Redis, error) {
	this := new(Redis)
	configModel := &RedisConfigModel{config.Host, config.Db, config.Password}
	this.config = configModel
	return this, this.connect()
}

func (this *Redis) connect() error {
	this.redisLockCnt = &sync.Map{}
	str := this.config.Host + "-" + strconv.Itoa(this.config.Db) + "-" + this.config.Password
	if this.connectOnly(str) {
		// 如果同事还有其他协程创建连接成功了
		return nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:     this.config.Host,
		Password: this.config.Password,
		DB:       this.config.Db,
	})

	// 通过 cient.Ping() 来检查是否成功连接到了 redis 服务器
	_, err := client.Ping().Result()
	if err != nil {
		fmt.Println(err)
		return err
	}

	this.redisClient = client
	RedisDbMap.Store(str, client)
	return nil
}

// 使用有已有的连接资源
func (this *Redis) connectOnly(dataSourceName string) bool {
	if v, ok := RedisDbMap.Load(dataSourceName); ok {
		this.redisClient = v.(*redis.Client)
		return true
	}
	return false
}

//redis锁
func (this *Redis) RedisLock(lockkey string) {
	if val, ok := this.redisLockCnt.LoadOrStore(lockkey, int32(1)); ok {
		this.redisLockCnt.Store(lockkey, val.(int32)+1)
	}

	for {
		fields, err := this.redisClient.HGet(lockkey, "lock").Result()
		if err != nil {
			break
		}
		if fields == "unlock" {
			break
		}
		//time.Sleep(time.Nanosecond * 10)
	}
	this.redisClient.HSet(lockkey, "lock", "lock")
}

//redis解锁
func (this *Redis) RedisUnLock(lockkey string) {
	this.redisClient.HSet(lockkey, "lock", "unlock")
	if val, ok := this.redisLockCnt.LoadOrStore(lockkey, int32(0)); ok && val.(int32) > 0 {
		cnt := val.(int32) - 1
		this.redisLockCnt.Store(lockkey, cnt)
	}
}

func (this *Redis) RedisUnLockAndDo(lockkey string, fn func()) {
	for {
		val, ok := this.redisLockCnt.Load(lockkey)
		if (ok && val.(int32) <= 0) || !ok {
			fn()
			return
		}
	}
}

func (this *Redis) FlushAll() {
	this.redisClient.FlushAll()
}

func (this *Redis) Close() {
	this.redisClient.Close()
	this.redisClient = nil
}

func (this *Redis) Get() *redis.Client {
	return this.redisClient
}
