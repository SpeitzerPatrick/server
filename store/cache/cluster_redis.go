package cache

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/east-eden/server/utils"
	"github.com/go-redis/redis"
	json "github.com/json-iterator/go"
	log "github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
)

type ClusterRedis struct {
	client *redis.ClusterClient
	utils.WaitGroupWrapper
}

func NewClusterRedis(ctx *cli.Context) *ClusterRedis {
	// 获取集群节点地址
	clusterAddrs := os.Getenv("REDIS_CLUSTER_ADDRS")
	if clusterAddrs == "" {
		clusterAddrs = ctx.String("redis_cluster_addrs")
	}

	if clusterAddrs == "" {
		log.Fatal().Msg("Redis cluster addresses not configured")
	}

	addrs := strings.Split(clusterAddrs, ",")
	for i, addr := range addrs {
		addrs[i] = strings.TrimSpace(addr)
	}

	password := os.Getenv("REDIS_PASSWORD")
	if password == "" {
		password = ctx.String("redis_password")
	}

	r := &ClusterRedis{
		client: redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        addrs,
			Password:     password,
			PoolSize:     100,
			MinIdleConns: 10,
			IdleTimeout:  time.Minute * 5,
		}),
	}

	// 测试连接
	if err := r.client.Ping().Err(); err != nil {
		log.Fatal().Err(err).Strs("addrs", addrs).Msg("failed to connect to Redis cluster")
	}

	log.Info().Strs("cluster_nodes", addrs).Msg("Redis cluster connection established")
	return r
}

func (r *ClusterRedis) SaveObject(prefix string, k interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	data, err := json.Marshal(x)
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}

	_, err = r.client.Set(key, data, ExpireTime).Result()
	if err != nil {
		return fmt.Errorf("Redis cluster SaveObject failed: %w", err)
	}

	return nil
}

func (r *ClusterRedis) SaveHashObject(prefix string, k interface{}, field interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	data, err := json.Marshal(x)
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}

	f := fmt.Sprintf("%v", field)
	_, err = r.client.HSet(key, f, data).Result()
	if err != nil {
		return fmt.Errorf("Redis cluster SaveHashObject failed: %w", err)
	}

	r.client.Expire(key, ExpireTime)
	return nil
}

func (r *ClusterRedis) SaveHashAll(prefix string, k interface{}, fields map[string]interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)

	_, err := r.client.HMSet(key, fields).Result()
	if err != nil {
		return fmt.Errorf("Redis cluster SaveHashAll failed: %w", err)
	}

	r.client.Expire(key, ExpireTime)
	return nil
}

func (r *ClusterRedis) LoadObject(prefix string, k interface{}, x interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)

	data, err := r.client.Get(key).Bytes()
	if err == redis.Nil {
		return ErrObjectNotFound
	}
	if err != nil {
		return fmt.Errorf("Redis cluster LoadObject failed: %w", err)
	}

	err = json.Unmarshal(data, x)
	if err != nil {
		return fmt.Errorf("json unmarshal failed: %w", err)
	}

	r.client.Expire(key, ExpireTime)
	return nil
}

func (r *ClusterRedis) LoadHashAll(prefix string, keyValue interface{}) (interface{}, error) {
	key := fmt.Sprintf("%s:%v", prefix, keyValue)

	result, err := r.client.HGetAll(key).Result()
	if err != nil {
		return nil, fmt.Errorf("Redis cluster LoadHashAll failed: %w", err)
	}

	if len(result) == 0 {
		return nil, ErrObjectNotFound
	}

	r.client.Expire(key, ExpireTime)
	return result, nil
}

func (r *ClusterRedis) DeleteObject(prefix string, k interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	_, err := r.client.Del(key).Result()
	if err != nil {
		return fmt.Errorf("Redis cluster DeleteObject failed: %w", err)
	}
	return nil
}

func (r *ClusterRedis) DeleteHashObject(prefix string, k interface{}, field interface{}) error {
	key := fmt.Sprintf("%s:%v", prefix, k)
	f := fmt.Sprintf("%v", field)
	_, err := r.client.HDel(key, f).Result()
	if err != nil {
		return fmt.Errorf("Redis cluster DeleteHashObject failed: %w", err)
	}
	return nil
}

func (r *ClusterRedis) Exit() error {
	if r.client != nil {
		r.client.Close()
	}
	log.Info().Msg("Redis cluster connection closed")
	return nil
}

func (r *ClusterRedis) HealthCheck() error {
	if err := r.client.Ping().Err(); err != nil {
		return fmt.Errorf("Redis cluster health check failed: %w", err)
	}
	return nil
}

func (r *ClusterRedis) GetConnectionInfo() map[string]interface{} {
	info := make(map[string]interface{})
	if r.client != nil {
		info["cluster_nodes"] = r.client.Options().Addrs
	}
	return info
}
