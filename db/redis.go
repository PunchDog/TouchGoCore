package db

import (
	"fmt"
	"strconv"
	"sync"

	"touchgocore/config"

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
