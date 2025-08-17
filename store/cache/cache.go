package cache

import (
	"errors"
	"os"
	"time"

	"github.com/urfave/cli/v2"
)

// cache find no result
var (
	ErrNoResult       = errors.New("cache return no result")
	ErrObjectNotFound = errors.New("cache object not found")

	ExpireTime = 24 * time.Hour // 过期时间24小时
)

type Cache interface {
	SaveObject(prefix string, k interface{}, x interface{}) error
	SaveHashObject(prefix string, k interface{}, field interface{}, x interface{}) error
	SaveHashAll(prefix string, k interface{}, fields map[string]interface{}) error
	LoadObject(prefix string, k interface{}, x interface{}) error
	LoadHashAll(prefix string, keyValue interface{}) (interface{}, error)
	DeleteObject(prefix string, k interface{}) error
	DeleteHashObject(prefix string, k interface{}, field interface{}) error
	Exit() error
}

func NewCache(ctx *cli.Context) Cache {
	// 根据环境变量或配置选择Redis实现
	redisMode := os.Getenv("REDIS_MODE")
	switch redisMode {
	case "master-slave":
		return NewMasterSlaveRedis(ctx)
	case "cluster":
		return NewClusterRedis(ctx)
	case "single":
		return NewGoRedis(ctx)
	case "mini":
		return NewMiniRedis(ctx)
	default:
		return NewDummyRedis(ctx) // 默认保持兼容性
	}
}
