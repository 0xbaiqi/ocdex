package capital

import (
	"context"
	"fmt"
	"sync"

	"ocdex/config"

	"github.com/redis/go-redis/v9"
)

// RedisCapitalManager manages capital and concurrency using Redis
type RedisCapitalManager struct {
	client *redis.Client
	cfg    config.Config
	mu     sync.Mutex
}

func NewRedisCapitalManager(cfg config.Config) *RedisCapitalManager {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Host,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	return &RedisCapitalManager{
		client: rdb,
		cfg:    cfg,
	}
}

// Init initializes the capital state in Redis if not exists or resets checks
func (cm *RedisCapitalManager) Init(ctx context.Context) error {
	if _, err := cm.client.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("redis connection failed: %w", err)
	}

	cm.client.Set(ctx, "ocdex:capital:total", cm.cfg.Strategy.TotalCapitalUSD, 0)
	cm.client.Set(ctx, "ocdex:capital:per_trade", cm.cfg.Strategy.PerTradeUSD, 0)

	return nil
}

// Reserve attempts to reserve capital for a trade
func (cm *RedisCapitalManager) Reserve(ctx context.Context, tradeID string) error {
	script := `
		local total = tonumber(redis.call('GET', KEYS[1]) or 0)
		local inUse = tonumber(redis.call('GET', KEYS[2]) or 0)
		local perTrade = tonumber(ARGV[1])
		
		if total - inUse >= perTrade then
			redis.call('INCRBYFLOAT', KEYS[2], perTrade)
			redis.call('SADD', KEYS[3], ARGV[2])
			return 1
		end
		return 0
	`
	keys := []string{"ocdex:capital:total", "ocdex:capital:in_use", "ocdex:trades:active"}
	args := []interface{}{cm.cfg.Strategy.PerTradeUSD, tradeID}

	res, err := cm.client.Eval(ctx, script, keys, args...).Result()
	if err != nil {
		return err
	}

	if res.(int64) == 1 {
		return nil // Success
	}
	return fmt.Errorf("insufficient capital or max concurrent reached")
}

// Release releases the capital for a trade
func (cm *RedisCapitalManager) Release(ctx context.Context, tradeID string) {
	script := `
		local removed = redis.call('SREM', KEYS[2], ARGV[2])
		if removed == 1 then
			redis.call('INCRBYFLOAT', KEYS[1], -tonumber(ARGV[1]))
			return 1
		end
		return 0
	`
	keys := []string{"ocdex:capital:in_use", "ocdex:trades:active"}
	args := []interface{}{cm.cfg.Strategy.PerTradeUSD, tradeID}

	cm.client.Eval(ctx, script, keys, args...)
}
