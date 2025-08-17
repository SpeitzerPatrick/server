package cache

import (
	"fmt"
	"os"
	"time"

	"github.com/east-eden/server/utils"
	"github.com/go-redis/redis"
	json "github.com/json-iterator/go"
	log "github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
)

type MasterSlaveRedis struct {
	masterClient *redis.Client
	slaveClient  *redis.Client
	utils.WaitGroupWrapper
}

func NewMasterSlaveRedis(ctx *cli.Context) *MasterSlaveRedis {
	// 获取主从Redis地址
	masterAddr := os.Getenv("REDIS_MASTER_ADDR")
	slaveAddr := os.Getenv("REDIS_SLAVE_ADDR")

	if masterAddr == "" {
		masterAddr = ctx.String("redis_master_addr")
	}
	if slaveAddr == "" {
		slaveAddr = ctx.String("redis_slave_addr")
	}

	// 如果没有配置从库地址，使用主库地址
	if slaveAddr == "" {
		slaveAddr = masterAddr
	}

	password := os.Getenv("REDIS_PASSWORD")
	if password == "" {
		password = ctx.String("redis_password")
	}

	r := &MasterSlaveRedis{
		masterClient: redis.NewClient(&redis.Options{
			Addr:         masterAddr,
			Password:     password,
			DB:           0,
			PoolSize:     100,
			MinIdleConns: 10,
			IdleTimeout:  time.Minute * 5,
		}),
		slaveClient: redis.NewClient(&redis.Options{
			Addr:         slaveAddr,
			Password:     password,
			DB:           0,
			PoolSize:     100,
			MinIdleConns: 10,
			IdleTimeout:  time.Minute * 5,
		}),
	}

	// 测试连接
	if err := r.masterClient.Ping().Err(); err != nil {
		log.Fatal().Err(err).Str("addr", masterAddr).Msg("failed to connect to Redis master")
	}

	if err := r.slaveClient.Ping().Err(); err != nil {
		log.Warn().Err(err).Str("addr", slaveAddr).Msg("failed to connect to Redis slave, using master for reads")
		r.slaveClient = r.masterClient // 从库连接失败时使用主库
	}

	log.Info().
		Str("master", masterAddr).
		Str("slave", slaveAddr).
		Msg("Redis master-slave connection established")

	return r
}

// 写操作使用主库
func (r *MasterSlaveRedis) SaveObject(prefix string, k interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	data, err := json.Marshal(x)
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}

	_, err = r.masterClient.Set(key, data, ExpireTime).Result()
	if err != nil {
		return fmt.Errorf("Redis master SaveObject failed: %w", err)
	}

	return nil
}

func (r *MasterSlaveRedis) SaveHashObject(prefix string, k interface{}, field interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	data, err := json.Marshal(x)
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}

	f := fmt.Sprintf("%v", field)
	_, err = r.masterClient.HSet(key, f, data).Result()
	if err != nil {
		return fmt.Errorf("Redis master SaveHashObject failed: %w", err)
	}

	// 设置过期时间
	r.masterClient.Expire(key, ExpireTime)
	return nil
}

func (r *MasterSlaveRedis) SaveHashAll(prefix string, k interface{}, fields map[string]interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)

	_, err := r.masterClient.HMSet(key, fields).Result()
	if err != nil {
		return fmt.Errorf("Redis master SaveHashAll failed: %w", err)
	}

	// 设置过期时间
	r.masterClient.Expire(key, ExpireTime)
	return nil
}

// 读操作优先使用从库
func (r *MasterSlaveRedis) LoadObject(prefix string, k interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)

	// 先尝试从从库读取
	data, err := r.slaveClient.Get(key).Bytes()
	if err == redis.Nil {
		return ErrObjectNotFound
	}
	if err != nil {
		// 从库读取失败，尝试主库
		log.Warn().Err(err).Str("key", key).Msg("slave read failed, trying master")
		data, err = r.masterClient.Get(key).Bytes()
		if err == redis.Nil {
			return ErrObjectNotFound
		}
		if err != nil {
			return fmt.Errorf("Redis LoadObject failed: %w", err)
		}
	}

	err = json.Unmarshal(data, x)
	if err != nil {
		return fmt.Errorf("json unmarshal failed: %w", err)
	}

	// 更新过期时间（在主库上）
	r.masterClient.Expire(key, ExpireTime)
	return nil
}

func (r *MasterSlaveRedis) LoadHashAll(prefix string, keyValue interface{}) (interface{}, error) {
	key := fmt.Sprintf("%s:%v", prefix, keyValue)

	// 先尝试从从库读取
	result, err := r.slaveClient.HGetAll(key).Result()
	if err != nil {
		// 从库读取失败，尝试主库
		log.Warn().Err(err).Str("key", key).Msg("slave read failed, trying master")
		result, err = r.masterClient.HGetAll(key).Result()
		if err != nil {
			return nil, fmt.Errorf("Redis LoadHashAll failed: %w", err)
		}
	}

	if len(result) == 0 {
		return nil, ErrObjectNotFound
	}

	// 更新过期时间（在主库上）
	r.masterClient.Expire(key, ExpireTime)
	return result, nil
}

// 删除操作使用主库
func (r *MasterSlaveRedis) DeleteObject(prefix string, k interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	_, err := r.masterClient.Del(key).Result()
	if err != nil {
		return fmt.Errorf("Redis master DeleteObject failed: %w", err)
	}
	return nil
}

func (r *MasterSlaveRedis) DeleteHashObject(prefix string, k interface{}, field interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	f := fmt.Sprintf("%v", field)
	_, err := r.masterClient.HDel(key, f).Result()
	if err != nil {
		return fmt.Errorf("Redis master DeleteHashObject failed: %w", err)
	}
	return nil
}

func (r *MasterSlaveRedis) Exit() error {
	if r.masterClient != nil {
		r.masterClient.Close()
	}
	if r.slaveClient != nil && r.slaveClient != r.masterClient {
		r.slaveClient.Close()
	}
	log.Info().Msg("Redis master-slave connections closed")
	return nil
}

// 健康检查
func (r *MasterSlaveRedis) HealthCheck() error {
	// 检查主库
	if err := r.masterClient.Ping().Err(); err != nil {
		return fmt.Errorf("Redis master health check failed: %w", err)
	}

	// 检查从库（如果不是同一个实例）
	if r.slaveClient != r.masterClient {
		if err := r.slaveClient.Ping().Err(); err != nil {
			log.Warn().Err(err).Msg("Redis slave health check failed")
			// 从库失败不影响整体服务，只记录警告
		}
	}

	return nil
}

// 获取连接信息
func (r *MasterSlaveRedis) GetConnectionInfo() map[string]interface{} {
	info := make(map[string]interface{})

	if r.masterClient != nil {
		info["master"] = r.masterClient.Options().Addr
	}

	if r.slaveClient != nil && r.slaveClient != r.masterClient {
		info["slave"] = r.slaveClient.Options().Addr
	}

	return info
}
