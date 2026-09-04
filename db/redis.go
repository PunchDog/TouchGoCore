package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/redis/go-redis/v9"
)

type RedisConfigModel struct {
	Host     string
	Db       int
	Password string
}

type Redis struct {
	redisClient redis.Cmdable // 使用接口，兼容单机和集群模式
	config      *RedisConfigModel
	isCluster   bool
}

// NewRedis 创建Redis连接（支持单机和集群模式）
func NewRedis(config *config.RedisConfig) (*Redis, error) {
	this := new(Redis)
	configModel := &RedisConfigModel{config.Host, config.Db, config.Password}
	this.config = configModel
	return this, this.connect()
}

func (this *Redis) poolKey() string {
	sum := sha256.Sum256([]byte(this.config.Password))
	return this.config.Host + "-" + strconv.Itoa(this.config.Db) + "-" + hex.EncodeToString(sum[:8])
}

func (this *Redis) connect() error {
	str := this.poolKey()
	if this.connectOnly(str) {
		return nil
	}

	// 判断是否为集群模式：多个地址用逗号分隔
	if isClusterMode(this.config.Host) {
		return this.connectCluster(str)
	}
	return this.connectStandalone(str)
}

// isClusterMode 判断是否为集群模式（多地址用逗号分隔）
func isClusterMode(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] == ',' {
			return true
		}
	}
	return false
}

// connectStandalone 单机模式连接
func (this *Redis) connectStandalone(connKey string) error {
	client := redis.NewClient(&redis.Options{
		Addr:         this.config.Host,
		Password:     this.config.Password,
		DB:           this.config.Db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("redis单机连接失败: %w", err)
	}

	this.redisClient = client
	this.isCluster = false
	_DbMap.Store(connKey, client)
	return nil
}

// connectCluster 集群模式连接
func (this *Redis) connectCluster(connKey string) error {
	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        splitAddrs(this.config.Host),
		Password:     this.config.Password,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("redis集群连接失败: %w", err)
	}

	this.redisClient = client
	this.isCluster = true
	_DbMap.Store(connKey, client)
	return nil
}

// splitAddrs 将逗号分隔的地址字符串拆分为地址切片
func splitAddrs(host string) []string {
	var addrs []string
	start := 0
	for i := 0; i <= len(host); i++ {
		if i == len(host) || host[i] == ',' {
			addr := host[start:i]
			if addr != "" {
				addrs = append(addrs, addr)
			}
			start = i + 1
		}
	}
	return addrs
}

// 使用已有的连接资源
func (this *Redis) connectOnly(dataSourceName string) bool {
	if v, ok := _DbMap.Load(dataSourceName); ok {
		this.redisClient = v.(redis.Cmdable)
		switch v.(type) {
		case *redis.ClusterClient:
			this.isCluster = true
		case *redis.Client:
			this.isCluster = false
		default:
			// 未知类型，通过类型断言推断
			if _, ok := v.(*redis.ClusterClient); ok {
				this.isCluster = true
			} else {
				this.isCluster = false
			}
		}
		return true
	}
	return false
}

func (this *Redis) FlushAll() {
	if !util.DEBUG && os.Getenv("TOUCHGO_ALLOW_FLUSHALL") != "1" {
		vars.Error("FlushAll 已拒绝：仅 debug 或 TOUCHGO_ALLOW_FLUSHALL=1 可用")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	this.redisClient.FlushAll(ctx)
}

func (this *Redis) Close() {
	if this.isCluster {
		if cc, ok := this.redisClient.(*redis.ClusterClient); ok {
			cc.Close()
		}
	} else {
		if sc, ok := this.redisClient.(*redis.Client); ok {
			sc.Close()
		}
	}
	this.redisClient = nil
	if this.config != nil {
		_DbMap.Delete(this.poolKey())
	}
}

// Get 获取原始Redis客户端（返回redis.Cmdable接口，兼容单机和集群）
func (this *Redis) Get() redis.Cmdable {
	return this.redisClient
}

// GetStandaloneClient 获取单机客户端（仅单机模式可用）
func (this *Redis) GetStandaloneClient() *redis.Client {
	if this.isCluster {
		return nil
	}
	if sc, ok := this.redisClient.(*redis.Client); ok {
		return sc
	}
	return nil
}

// GetClusterClient 获取集群客户端（仅集群模式可用）
func (this *Redis) GetClusterClient() *redis.ClusterClient {
	if !this.isCluster {
		return nil
	}
	if cc, ok := this.redisClient.(*redis.ClusterClient); ok {
		return cc
	}
	return nil
}

// IsCluster 返回是否为集群模式
func (this *Redis) IsCluster() bool {
	return this.isCluster
}
