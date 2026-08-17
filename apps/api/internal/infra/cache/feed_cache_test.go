package infracache

import (
	applicationinteraction "GCFeed/internal/application/interaction"
	domainfeed "GCFeed/internal/domain/feed"
	domaininteraction "GCFeed/internal/domain/interaction"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type actionStatFakeRedis struct {
	hashes map[string]map[string]string
	values map[string]string
}

func newActionStatFakeRedis() *actionStatFakeRedis {
	return &actionStatFakeRedis{
		hashes: map[string]map[string]string{},
		values: map[string]string{},
	}
}

func (r *actionStatFakeRedis) HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd {
	values := r.hashes[key]
	if values == nil {
		values = map[string]string{}
	}
	return redis.NewMapStringStringResult(values, nil)
}

func (r *actionStatFakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	value, ok := r.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (r *actionStatFakeRedis) Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	switch typed := value.(type) {
	case string:
		r.values[key] = typed
	case []byte:
		r.values[key] = string(typed)
	default:
		content, _ := json.Marshal(typed)
		r.values[key] = string(content)
	}
	return redis.NewStatusResult("OK", nil)
}

func (r *actionStatFakeRedis) MGet(ctx context.Context, keys ...string) *redis.SliceCmd {
	values := make([]any, 0, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values = append(values, value)
			continue
		}
		values = append(values, nil)
	}
	return redis.NewSliceResult(values, nil)
}

func TestActionStatAggregatesCounterShards(t *testing.T) {
	ctx := context.Background()
	videoID := int64(1001)
	redisClient := newActionStatFakeRedis()
	redisClient.hashes[interactionStatCounterBaseKey(videoID)] = map[string]string{
		"like_count":     "10",
		"comment_count":  "3",
		"favorite_count": "4",
		"epoch":          "epoch-1",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(42))] = map[string]string{
		"like_count":     "1",
		"favorite_count": "1",
		"epoch":          "epoch-1",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(43))] = map[string]string{
		"like_count": "-1",
		"epoch":      "epoch-1",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(44))] = map[string]string{
		"like_count": "1",
		"epoch":      "epoch-1",
	}
	// 旧版本残留 shard(base 重建前的增量)必须被跳过
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(45))] = map[string]string{
		"like_count":     "99",
		"favorite_count": "99",
		"epoch":          "stale-epoch",
	}

	stat, err := actionStat(ctx, redisClient, interactionStatCounterBaseKey(videoID), interactionStatCounterShardKeys(videoID), feedStatKey(videoID), videoID, nil)
	if err != nil {
		t.Fatalf("actionStat: %v", err)
	}
	if stat.LikeCount != 11 || stat.FavoriteCount != 5 || stat.CommentCount != 3 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func TestActionStatFallsBackToInitialStat(t *testing.T) {
	ctx := context.Background()
	videoID := int64(1002)
	redisClient := newActionStatFakeRedis()
	initial := &domaininteraction.VideoStat{
		VideoID:       videoID,
		LikeCount:     7,
		CommentCount:  2,
		FavoriteCount: 1,
	}
	// base 缺失时残留 shard 无法校验版本,只以 initialStat 打底,shard 增量不叠加。
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(42))] = map[string]string{
		"like_count":     "1",
		"favorite_count": "-1",
	}

	stat, err := actionStat(ctx, redisClient, interactionStatCounterBaseKey(videoID), interactionStatCounterShardKeys(videoID), feedStatKey(videoID), videoID, initial)
	if err != nil {
		t.Fatalf("actionStat: %v", err)
	}
	if stat.LikeCount != 7 || stat.FavoriteCount != 1 || stat.CommentCount != 2 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func TestGetStatsReadsShardedCountersOnJSONMiss(t *testing.T) {
	ctx := context.Background()
	videoID := int64(1003)
	redisClient := newActionStatFakeRedis()
	redisClient.hashes[interactionStatCounterBaseKey(videoID)] = map[string]string{
		"like_count":     "2",
		"comment_count":  "1",
		"favorite_count": "0",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(42))] = map[string]string{
		"like_count":     "1",
		"favorite_count": "1",
	}
	stats, err := getStats(ctx, redisClient, []int64{videoID})
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	stat := stats[videoID]
	if stat == nil || stat.LikeCount != 3 || stat.FavoriteCount != 1 || stat.CommentCount != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if _, ok := redisClient.values[feedStatKey(videoID)]; !ok {
		t.Fatalf("expected sharded stat to be written back to JSON cache")
	}
}

func TestSetVideoStatWritesJSONCache(t *testing.T) {
	ctx := context.Background()
	videoID := int64(1005)
	redisClient := newActionStatFakeRedis()

	err := setActionStatJSON(ctx, redisClient, feedStatKey(videoID), videoStatToFeedStat(&domaininteraction.VideoStat{
		VideoID:       videoID,
		LikeCount:     2,
		CommentCount:  3,
		FavoriteCount: 1,
	}))
	if err != nil {
		t.Fatalf("SetVideoStat: %v", err)
	}

	stats, err := getStats(ctx, redisClient, []int64{videoID})
	if err != nil {
		t.Fatalf("getStats: %v", err)
	}
	stat := stats[videoID]
	if stat == nil || stat.LikeCount != 2 || stat.CommentCount != 3 || stat.FavoriteCount != 1 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func TestActionStatBaseInitUsesInitialStat(t *testing.T) {
	videoID := int64(1004)
	initial := &domaininteraction.VideoStat{
		VideoID:       videoID,
		LikeCount:     1,
		CommentCount:  1,
		FavoriteCount: 1,
	}

	stat := actionStatBaseInit(videoID, initial)
	if stat != initial {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func followingTestMember(videoID, authorID int64, publishedAt time.Time) string {
	return fmt.Sprintf("%d:%d:%s", videoID, authorID, publishedAt.UTC().Format(time.RFC3339Nano))
}

func followingTestStreamItems(streams [][]string, authorIDs []int64, limit int) []*domainfeed.FeedPageItem {
	return mergeFollowingIndexes(streams, authorIDs, limit)
}

func followingTestVideoIDs(items []*domainfeed.FeedPageItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.VideoID)
	}
	return ids
}

func TestMergeFollowingIndexes(t *testing.T) {
	// 每个源必须已按 score 降序(Redis ZRevRangeByScore 的返回顺序)。
	t1 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	inbox := []string{
		followingTestMember(202, 2, t1),
		followingTestMember(101, 1, t1),
	}
	// 大 V 10 的 outbox: 101 与 inbox 同视频同时间(score 相同); 303 是最旧的视频。
	outboxA := []string{
		followingTestMember(101, 10, t1),
		followingTestMember(303, 10, t2),
	}
	// 作者 99 不在关注列表,应被过滤。
	outboxB := []string{
		followingTestMember(404, 99, t1),
	}
	streams := [][]string{inbox, outboxA, outboxB}
	authorIDs := []int64{10}

	t.Run("按score降序合并,去重保留inbox版,过滤非关注作者", func(t *testing.T) {
		items := followingTestStreamItems(streams, authorIDs, 5)
		want := []int64{202, 101, 303}
		if got := followingTestVideoIDs(items); !slices.Equal(got, want) {
			t.Fatalf("merge order = %v, want %v", got, want)
		}
		if len(items) != 3 {
			t.Fatalf("len = %d, want 3", len(items))
		}
		if items[1].AuthorID != 1 {
			t.Fatalf("video 101 kept outbox copy (author=%d), want inbox copy (author=1)", items[1].AuthorID)
		}
	})

	t.Run("limit截断并提前终止", func(t *testing.T) {
		items := followingTestStreamItems(streams, authorIDs, 2)
		if got := followingTestVideoIDs(items); !slices.Equal(got, []int64{202, 101}) {
			t.Fatalf("limit merge = %v, want [202 101]", got)
		}
	})

	t.Run("空流返回空", func(t *testing.T) {
		items := followingTestStreamItems([][]string{{}, {}}, nil, 5)
		if len(items) != 0 {
			t.Fatalf("empty streams got %v, want empty", items)
		}
	})

	t.Run("limit<=0返回空", func(t *testing.T) {
		items := followingTestStreamItems(streams, authorIDs, 0)
		if len(items) != 0 {
			t.Fatalf("limit 0 got %v, want empty", items)
		}
	})
}

// TestSetActionStateLua 用真实 Redis 验证 Lua 脚本状态机语义,与 memoryActionPipeline 行为一致:
// 首次点赞、幂等、状态翻转、初始计数初始化、并发原子性。无 TEST_REDIS_ADDR 时跳过。
// cleanupLuaTestKeys 删除指定视频的 base/shard/action 状态 key,保证 Lua 集成测试重跑可重复。
func cleanupLuaTestKeys(client *redis.Client, videoID int64, userIDs ...int64) {
	ctx := context.Background()
	keys := []string{interactionStatCounterBaseKey(videoID)}
	for shard := 0; shard < actionStatCounterShardCount; shard++ {
		keys = append(keys, interactionStatCounterShardKey(videoID, shard))
	}
	for _, userID := range userIDs {
		keys = append(keys,
			interactionActionKey(userID, videoID, domaininteraction.ActionTypeLike),
			interactionActionKey(userID, videoID, domaininteraction.ActionTypeFavorite),
		)
	}
	client.Del(ctx, keys...)
}

func TestSetActionStateLua(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set, skip Redis integration test")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	cache := NewFeedCache(client)

	// 清理上次运行的残留 key,保证测试重跑可重复
	cleanupLuaTestKeys(client, 7001, 70001)
	cleanupLuaTestKeys(client, 7002, 70002)
	cleanupLuaTestKeys(client, 7003, 70003)
	cleanupLuaTestKeys(client, 7004, 70041, 70042)

	t.Run("首次点赞与幂等与翻转", func(t *testing.T) {
		videoID := int64(7001)
		userID := int64(70001)
		initialStat := &domaininteraction.VideoStat{VideoID: videoID, LikeCount: 100, CommentCount: 20, FavoriteCount: 30}

		first, err := cache.SetActionState(ctx, userID, videoID, domaininteraction.ActionTypeLike, true, "idem-1", initialStat)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		if !first.Active || first.Delta != 1 || first.LikeCount != 101 {
			t.Fatalf("unexpected first like result: %+v", first)
		}
		shardKey := interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(userID))
		if got := client.HGet(ctx, shardKey, "like_count").Val(); got != "1" {
			t.Fatalf("shard like_count = %q, want 1", got)
		}
		if got := client.HGet(ctx, interactionStatCounterBaseKey(videoID), "like_count").Val(); got != "100" {
			t.Fatalf("base like_count = %q, want 100 (HSetNX 保留初始值)", got)
		}

		repeat, err := cache.SetActionState(ctx, userID, videoID, domaininteraction.ActionTypeLike, true, "idem-1", initialStat)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		if repeat.Delta != 0 || !repeat.Active {
			t.Fatalf("expected idempotent no-op, got %+v", repeat)
		}

		cancel, err := cache.SetActionState(ctx, userID, videoID, domaininteraction.ActionTypeLike, false, "idem-2", initialStat)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		if cancel.Delta != -1 || cancel.Active {
			t.Fatalf("expected cancel delta -1, got %+v", cancel)
		}
		if got := client.HGet(ctx, shardKey, "like_count").Val(); got != "0" {
			t.Fatalf("shard like_count = %q, want 0", got)
		}
	})

	t.Run("并发点赞恰好只计数一次", func(t *testing.T) {
		videoID := int64(7002)
		userID := int64(70002)
		const workers = 10

		results := make(chan *applicationinteraction.ActionStateResult, workers)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := cache.SetActionState(ctx, userID, videoID, domaininteraction.ActionTypeLike, true, "", nil)
				if err != nil {
					errs <- err
					return
				}
				results <- result
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("SetActionState: %v", err)
		}

		totalDelta := 0
		allActive := true
		for result := range results {
			totalDelta += result.Delta
			allActive = allActive && result.Active
		}
		// 脚本原子串行: 第一个把状态置为 active,其余 9 个看到状态已匹配 → delta=0。
		if totalDelta != 1 || !allActive {
			t.Fatalf("concurrent likes: totalDelta=%d allActive=%v, want 1 true", totalDelta, allActive)
		}
		shardKey := interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(userID))
		if got := client.HGet(ctx, shardKey, "like_count").Val(); got != "1" {
			t.Fatalf("shard like_count = %q, want 1", got)
		}
	})

	t.Run("首次取消不计数", func(t *testing.T) {
		videoID := int64(7003)
		userID := int64(70003)

		cancel, err := cache.SetActionState(ctx, userID, videoID, domaininteraction.ActionTypeFavorite, false, "", nil)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		if cancel.Delta != 0 || cancel.Active {
			t.Fatalf("expected first cancel delta 0, got %+v", cancel)
		}
	})

	t.Run("base 丢失重建不双计", func(t *testing.T) {
		videoID := int64(7004)
		userA, userB := int64(70041), int64(70042)
		initialStat := &domaininteraction.VideoStat{VideoID: videoID, LikeCount: 100, CommentCount: 20, FavoriteCount: 30}
		baseKey := interactionStatCounterBaseKey(videoID)
		staleShardKey := interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(userA))

		first, err := cache.SetActionState(ctx, userA, videoID, domaininteraction.ActionTypeLike, true, "idem-rebuild-1", initialStat)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		if first.Delta != 1 || first.LikeCount != 101 {
			t.Fatalf("unexpected first like result: %+v", first)
		}
		oldEpoch := client.HGet(ctx, baseKey, "epoch").Val()
		if oldEpoch == "" {
			t.Fatalf("base epoch 应为非空值")
		}

		// 模拟 base 被驱逐(LRU/删 key):删 base,残留 shard 仍带旧 epoch
		if err := client.Del(ctx, baseKey).Err(); err != nil {
			t.Fatalf("Del base: %v", err)
		}

		second, err := cache.SetActionState(ctx, userB, videoID, domaininteraction.ActionTypeLike, true, "idem-rebuild-2", initialStat)
		if err != nil {
			t.Fatalf("SetActionState: %v", err)
		}
		// 旧实现会得到 102:base 重建(100)+ 两个 shard 各 1;新实现按版本过滤,应为 101。
		if second.LikeCount != 101 {
			t.Fatalf("LikeCount = %d, want 101 (base 重建后不得叠加残留 shard)", second.LikeCount)
		}
		if got := client.HGet(ctx, baseKey, "like_count").Val(); got != "100" {
			t.Fatalf("base like_count = %q, want 100", got)
		}

		// 残留 shard 保留旧 epoch,与新 base epoch 不一致 → 读路径跳过
		newEpoch := client.HGet(ctx, baseKey, "epoch").Val()
		if staleEpoch := client.HGet(ctx, staleShardKey, "epoch").Val(); staleEpoch == newEpoch {
			t.Fatalf("残留 shard epoch %q 不应等于新 base epoch %q", staleEpoch, newEpoch)
		}

		stat, ok, err := actionStatFromCache(ctx, client, videoID)
		if err != nil {
			t.Fatalf("actionStatFromCache: %v", err)
		}
		if !ok || stat.LikeCount != 101 || stat.FavoriteCount != 30 || stat.CommentCount != 20 {
			t.Fatalf("unexpected read stat: %+v (ok=%v)", stat, ok)
		}
	})
}
