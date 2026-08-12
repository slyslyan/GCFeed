package infracache

import (
	domainfeed "GCFeed/internal/domain/feed"
	domaininteraction "GCFeed/internal/domain/interaction"
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(42))] = map[string]string{
		"like_count":     "1",
		"favorite_count": "1",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(43))] = map[string]string{
		"like_count": "-1",
	}
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(44))] = map[string]string{
		"like_count": "1",
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
	redisClient.hashes[interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(42))] = map[string]string{
		"like_count":     "1",
		"favorite_count": "-1",
	}

	stat, err := actionStat(ctx, redisClient, interactionStatCounterBaseKey(videoID), interactionStatCounterShardKeys(videoID), feedStatKey(videoID), videoID, initial)
	if err != nil {
		t.Fatalf("actionStat: %v", err)
	}
	if stat.LikeCount != 8 || stat.FavoriteCount != 0 || stat.CommentCount != 2 {
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
