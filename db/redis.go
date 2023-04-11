package db

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/util"

	"github.com/go-redis/redis/v7"
)

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
	_DbMap.Store(str, client)
	return nil
}

// 使用有已有的连接资源
func (this *Redis) connectOnly(dataSourceName string) bool {
	if v, ok := _DbMap.Load(dataSourceName); ok {
		this.redisClient = v.(*redis.Client)
		return true
	}
	return false
}

// redis锁
func (this *Redis) Lock(lockkey string) {
	for {
		select {
		case <-time.After(time.Nanosecond * 10):
			//正常加锁
			if success, _ := this.redisClient.SetNX("lock-"+lockkey, util.GetNowtimeMD5(), 10*time.Second).Result(); success {
				return
			} else if this.redisClient.TTL("lock-"+lockkey).Val() == -1 { //-2:失效；-1：无过期；
				this.redisClient.Expire("lock-"+lockkey, 10*time.Second)
			}
		}
	}
}

// redis解锁
func (this *Redis) UnLock(lockkey string) {
	this.redisClient.Del("lock-" + lockkey)
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
