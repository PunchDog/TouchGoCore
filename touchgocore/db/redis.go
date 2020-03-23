package db

import (
	"fmt"
	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis"
)

var RedisDbMap *sync.Map = &sync.Map{}

type RedisConfigModel struct {
	Host     string
	Db       int
	Password string
}

type Redis struct {
	redisClient *redis.Client
	LockCnt     *sync.Map
	config      *RedisConfigModel
}

func NewRedis(config *config.RedisConfig) (*Redis, error) {
	this := new(Redis)
	configModel := &RedisConfigModel{config.Host, config.Db, config.Password}
	this.config = configModel
	return this, this.connect()
}

func (this *Redis) connect() error {
	this.LockCnt = &sync.Map{}
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
func (this *Redis) Lock(lockkey string) {
	for {
		select {
		case <-time.After(time.Nanosecond * 10):
			//正常加锁
			if success, _ := this.redisClient.SetNX("lock-"+lockkey, util.GetNowtimeMD5_TouchGoCore(), 10*time.Second).Result(); success {
				return
			} else if this.redisClient.TTL("lock-"+lockkey).Val() == -1 { //-2:失效；-1：无过期；
				this.redisClient.Expire("lock-"+lockkey, 10*time.Second)
			}
		}
	}
}

//redis解锁
func (this *Redis) UnLock(lockkey string) {
	this.redisClient.Del("lock-" + lockkey)
}

func (this *Redis) UnLockAndDo(lockkey string, fn func()) {
	for {
		val, ok := this.LockCnt.Load(lockkey)
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

func (this *Redis) Set(key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	this.Lock(key)
	defer this.UnLock(key)
	return this.redisClient.Set(key, value, expiration)
}

func (this *Redis) Get(key string) *redis.StringCmd {
	this.Lock(key)
	defer this.UnLock(key)
	return this.redisClient.Get(key)
}

func (this *Redis) HSet(key string, values ...interface{}) *redis.IntCmd {
	this.Lock(key)
	defer this.UnLock(key)
	return this.redisClient.HSet(key, values...)
}

func (this *Redis) HGet(key, field string) *redis.StringCmd {
	this.Lock(key)
	defer this.UnLock(key)
	return this.redisClient.HGet(key, field)
}

func (this *Redis) HDel(key string, fields ...string) *redis.IntCmd {
	this.Lock(key)
	defer this.UnLock(key)
	return this.redisClient.HDel(key, fields...)
}

func (this *Redis) Del(key ...string) *redis.IntCmd {
	this.Lock("default")
	defer this.UnLock("default")
	return this.redisClient.Del(key...)
}
