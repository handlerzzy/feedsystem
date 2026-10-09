package video_test

import (
	"strconv"
	"strings"
	"testing"

	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// 三个 service 单测共用的固定 ID，避免各文件各写一套魔法数字。
const (
	fixtureVideoID   uint = 42
	fixtureAccountID uint = 7
)

// newCache 返回一个基于 miniredis 的真实 Redis 客户端（前缀 test:）。
// 与 internal/account 的单测保持一致：缓存相关行为用真实 Redis 语义验证，
// 而不是 mock 掉 rediscache.Client。
func newCache(t *testing.T) (*rediscache.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	// MaxRetries=-1（而不是 0）关闭 go-redis 的重试退避：本套测试里有若干
	// "Redis 立刻不可用" 的 best-effort 用例，默认 3 次重试会让每个用例
	// 白等 ~1s/命令。关闭重试不改变被验证的语义——命令依然失败，
	// service 依然必须忽略这个失败。
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rediscache.NewClient(rdb, "test:"), mr
}

// hotScore 返回热榜窗口 ZSET 中该视频的分数。
//
// 窗口 key 里带"当前分钟"时间戳（hot:video:1m:200601021504），直接构造 key
// 会在分钟边界上 flaky，所以按前缀扫描 mr.Keys()。
func hotScore(t *testing.T, mr *miniredis.Miniredis, videoID uint) (float64, bool) {
	t.Helper()
	member := strconv.FormatUint(uint64(videoID), 10)
	for _, k := range mr.Keys() {
		if !strings.HasPrefix(k, "test:hot:video:1m:") {
			continue
		}
		if score, err := mr.ZScore(k, member); err == nil {
			return score, true
		}
	}
	return 0, false
}

// hotKeys 返回当前热榜窗口 key（没有则为空串），用于断言"要么写、要么没写"。
func hotKeys(mr *miniredis.Miniredis) []string {
	var keys []string
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, "test:hot:video:1m:") {
			keys = append(keys, k)
		}
	}
	return keys
}
