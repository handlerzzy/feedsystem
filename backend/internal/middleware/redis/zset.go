package redis

import (
	"context"
	"errors"
	"time"

	redis "github.com/redis/go-redis/v9"
)

func (c *Client) ZincrBy(ctx context.Context, key string, member string, score float64) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.ZIncrBy(ctx, key, score, member).Err()
}

func (c *Client) ZAdd(ctx context.Context, key string, members ...redis.Z) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.ZAdd(ctx, key, members...).Err()
}

func (c *Client) ZRemRangeByRank(ctx context.Context, key string, start int64, stop int64) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.ZRemRangeByRank(ctx, key, start, stop).Err()
}

func (c *Client) ZRangeWithScores(ctx context.Context, key string, start int64, stop int64) ([]redis.Z, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("redis client not initialized")
	}
	return c.rdb.ZRangeWithScores(ctx, key, start, stop).Result()
}

// ZMScore 一次返回多个成员在 ZSET 里的分数。
//
// 为什么需要"批量"版本：调用方（时间线落后探测）要判断一批视频
// **每一条**在不在集合里。逐个 ZSCORE 是 N 次往返，而这是读路径上的
// 例行检查，N 次往返会直接把"例行检查"变成"例行开销"。
//
// 不存在的成员在返回数组里是 redis.Nil（用 IsMiss 判定），
// 而不是 0："分数是 0"与"这个成员压根不在集合里"是两件完全不同的事，
// 把后者当成前者会让"时间线里有没有这条视频"这个判断永远为真。
//
// 返回的切片与 members 一一对应、长度相同；key 不存在时全是 redis.Nil。
func (c *Client) ZMScore(ctx context.Context, key string, members ...string) ([]float64, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("redis client not initialized")
	}
	return c.rdb.ZMScore(ctx, key, members...).Result()
}

func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Expire(ctx, key, ttl).Err()
}

func (c *Client) ZUnionStore(ctx context.Context, dst string, keys []string, aggregate string) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.ZUnionStore(ctx, dst, &redis.ZStore{
		Keys:      keys,
		Aggregate: aggregate,
	}).Err()
}

func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, nil
	}
	n, err := c.rdb.Exists(ctx, key).Result()
	return n > 0, err
}

func (c *Client) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	return c.rdb.ZRevRange(ctx, key, start, stop).Result()
}

func (c *Client) ZRevRangeByScore(ctx context.Context, key string, max, min string, offset, count int64) ([]string, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	return c.rdb.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{
		Max:    max,
		Min:    min,
		Offset: offset,
		Count:  count,
	}).Result()
}
