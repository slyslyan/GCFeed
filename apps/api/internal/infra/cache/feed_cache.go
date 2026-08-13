package infracache

import (
	applicationfeed "GCFeed/internal/application/feed"
	applicationinteraction "GCFeed/internal/application/interaction"
	domainfeed "GCFeed/internal/domain/feed"
	domaininteraction "GCFeed/internal/domain/interaction"
	inframetrics "GCFeed/internal/infra/metrics"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const hotWindowMinutes = 60
const hotMinuteBucketTTL = 2 * time.Hour
const hotWindowCacheTTL = 2 * time.Minute
const actionStateTTL = 30 * 24 * time.Hour
const actionStatTTL = 24 * time.Hour
const actionStatJSONTTL = 15 * time.Second
const actionStatCounterShardCount = 16
const followingIndexKeyTTL = 30 * 24 * time.Hour

// 删除标记 TTL 必须大于卡片缓存 TTL(15 分钟),否则标记先过期会重现残留窗口。
const deletedVideoMarkerTTL = 30 * time.Minute
const deletedVideoSetKey = "video:deleted:v1"

type redisCacheClient interface {
	redis.Cmdable
	Pipeline() redis.Pipeliner
}

// actionStateScript 把"读状态 → 算 delta → 写状态与计数"收进一个 Lua 脚本原子执行,
// 替代 WATCH+MULTI 乐观锁: 一次往返、无需重试、无竞态窗口。
// 注意: EVAL 要求全部 key 落在同一哈希槽,Redis Cluster 下必须重新设计 key 布局。
var actionStateScript = redis.NewScript(`
local stored = redis.call('HGETALL', KEYS[1])
local storedStatus = 0
local storedIDKey = ''
for i = 1, #stored, 2 do
	if stored[i] == 'status' then
		storedStatus = tonumber(stored[i + 1]) or 0
	elseif stored[i] == 'idempotency_key' then
		storedIDKey = stored[i + 1] or ''
	end
end

local active = tonumber(ARGV[1]) == 1
local targetStatus = tonumber(ARGV[2])
local idempotencyKey = ARGV[3]
local delta = 0
local effectiveStatus = targetStatus

if storedIDKey == idempotencyKey and idempotencyKey ~= '' then
	effectiveStatus = storedStatus
elseif storedStatus == 0 then
	if active then delta = 1 end
elseif storedStatus ~= targetStatus then
	if active then delta = 1 else delta = -1 end
end

redis.call('HSET', KEYS[1], 'status', effectiveStatus, 'idempotency_key', idempotencyKey, 'updated_at', ARGV[4])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[5]))

if redis.call('HEXISTS', KEYS[2], 'like_count') == 0 then
	redis.call('HSET', KEYS[2], 'like_count', ARGV[6])
end
if redis.call('HEXISTS', KEYS[2], 'comment_count') == 0 then
	redis.call('HSET', KEYS[2], 'comment_count', ARGV[7])
end
if redis.call('HEXISTS', KEYS[2], 'favorite_count') == 0 then
	redis.call('HSET', KEYS[2], 'favorite_count', ARGV[8])
end
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[9]))

if delta ~= 0 then
	redis.call('HINCRBY', KEYS[3], ARGV[10], delta)
end
redis.call('EXPIRE', KEYS[3], tonumber(ARGV[9]))

return {effectiveStatus, delta}
`)

type redisActionStatReader interface {
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	Get(ctx context.Context, key string) *redis.StringCmd
}

type redisActionStatWriter interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
}

type redisStatCacheClient interface {
	redisActionStatReader
	redisActionStatWriter
	MGet(ctx context.Context, keys ...string) *redis.SliceCmd
}

// FeedCache 使用 Redis 保存 Feed 查询结果。
type FeedCache struct {
	client redisCacheClient
}

// NewFeedCache 创建 Feed 结果缓存。
func NewFeedCache(client redisCacheClient) *FeedCache {
	return &FeedCache{client: client}
}

// GetPage 读取缓存中的轻量 Feed 页。
func (c *FeedCache) GetPage(ctx context.Context, key string) (*applicationfeed.FeedPage, bool, error) {
	content, err := c.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		inframetrics.ObserveCacheRead("page", 1, 0, nil)
		return nil, false, nil
	}
	if err != nil {
		inframetrics.ObserveCacheRead("page", 1, 0, err)
		return nil, false, err
	}

	var page applicationfeed.FeedPage
	if err := json.Unmarshal(content, &page); err != nil {
		inframetrics.ObserveCacheRead("page", 1, 0, err)
		return nil, false, err
	}
	inframetrics.ObserveCacheRead("page", 1, 1, nil)
	return &page, true, nil
}

// SetPage 写入轻量 Feed 页，并设置过期时间。
func (c *FeedCache) SetPage(ctx context.Context, key string, page *applicationfeed.FeedPage, ttl time.Duration) error {
	content, err := json.Marshal(page)
	if err != nil {
		inframetrics.ObserveCacheWrite("page", 1, err)
		return err
	}
	err = c.client.Set(ctx, key, content, ttl).Err()
	inframetrics.ObserveCacheWrite("page", 1, err)
	return err
}

// GetCards 批量读取视频卡片缓存。
func (c *FeedCache) GetCards(ctx context.Context, videoIDs []int64) (map[int64]*domainfeed.FeedCard, error) {
	cards := map[int64]*domainfeed.FeedCard{}
	if len(videoIDs) == 0 {
		return cards, nil
	}

	values, err := c.client.MGet(ctx, cacheKeys(videoIDs, feedCardKey)...).Result()
	if err != nil {
		inframetrics.ObserveCacheRead("card", len(videoIDs), 0, err)
		return nil, err
	}
	for index, value := range values {
		content, ok := cacheValueBytes(value)
		if !ok {
			continue
		}
		var card domainfeed.FeedCard
		if err := json.Unmarshal(content, &card); err != nil {
			continue
		}
		if card.VideoID <= 0 {
			card.VideoID = videoIDs[index]
		}
		cards[card.VideoID] = &card
	}
	inframetrics.ObserveCacheRead("card", len(videoIDs), len(cards), nil)
	return cards, nil
}

// SetCards 批量写入视频卡片缓存。
func (c *FeedCache) SetCards(ctx context.Context, cards map[int64]*domainfeed.FeedCard, ttl time.Duration) error {
	pipe := c.client.Pipeline()
	queued := false

	for _, card := range cards {
		if card == nil || card.VideoID <= 0 {
			continue
		}
		content, err := json.Marshal(card)
		if err != nil {
			return err
		}
		pipe.Set(ctx, feedCardKey(card.VideoID), content, ttl)
		queued = true
	}
	if !queued {
		return nil
	}
	_, err := pipe.Exec(ctx)
	inframetrics.ObserveCacheWrite("card", len(cards), err)
	return err
}

// MarkVideoDeleted 把已删除视频写入 Redis 删除标记集合,Feed 组装时按标记过滤,
// 关闭软删后卡片缓存残留窗口。标记是尽力而为的缓存写,失败不影响删除本身。
// 注意: 若未来增加"恢复发布"流程,恢复时必须 SREM 移出标记,否则恢复后仍被过滤。
func (c *FeedCache) MarkVideoDeleted(ctx context.Context, videoID int64) error {
	if videoID <= 0 {
		return nil
	}
	pipe := c.client.Pipeline()
	pipe.SAdd(ctx, deletedVideoSetKey, videoID)
	pipe.Expire(ctx, deletedVideoSetKey, deletedVideoMarkerTTL)
	_, err := pipe.Exec(ctx)
	inframetrics.ObserveCacheWrite("deleted_video", 1, err)
	return err
}

// FilterDeletedVideos 批量检查 videoIDs 中哪些命中了删除标记集合。
func (c *FeedCache) FilterDeletedVideos(ctx context.Context, videoIDs []int64) (map[int64]bool, error) {
	deleted := map[int64]bool{}
	if len(videoIDs) == 0 {
		return deleted, nil
	}
	members := make([]interface{}, 0, len(videoIDs))
	for _, videoID := range videoIDs {
		members = append(members, videoID)
	}
	flags, err := c.client.SMIsMember(ctx, deletedVideoSetKey, members...).Result()
	if err != nil {
		inframetrics.ObserveCacheRead("deleted_video", len(videoIDs), 0, err)
		return nil, err
	}
	for index, flag := range flags {
		if flag {
			deleted[videoIDs[index]] = true
		}
	}
	inframetrics.ObserveCacheRead("deleted_video", len(videoIDs), len(deleted), nil)
	return deleted, nil
}

// GetStats 批量读取视频计数缓存。
func (c *FeedCache) GetStats(ctx context.Context, videoIDs []int64) (map[int64]*domainfeed.FeedStat, error) {
	return getStats(ctx, c.client, videoIDs)
}

func getStats(ctx context.Context, client redisStatCacheClient, videoIDs []int64) (map[int64]*domainfeed.FeedStat, error) {
	stats := map[int64]*domainfeed.FeedStat{}
	if len(videoIDs) == 0 {
		return stats, nil
	}

	values, err := client.MGet(ctx, cacheKeys(videoIDs, feedStatKey)...).Result()
	if err != nil {
		inframetrics.ObserveCacheRead("stat", len(videoIDs), 0, err)
		return nil, err
	}
	for index, value := range values {
		content, ok := cacheValueBytes(value)
		if !ok {
			continue
		}
		var stat domainfeed.FeedStat
		if err := json.Unmarshal(content, &stat); err != nil {
			continue
		}
		if stat.VideoID <= 0 {
			stat.VideoID = videoIDs[index]
		}
		stats[stat.VideoID] = &stat
	}
	for _, videoID := range videoIDs {
		if stats[videoID] != nil {
			continue
		}
		stat, ok, err := actionStatFromCache(ctx, client, videoID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		stats[videoID] = stat
		_ = setActionStatJSON(ctx, client, feedStatKey(videoID), stat)
	}
	inframetrics.ObserveCacheRead("stat", len(videoIDs), len(stats), nil)
	return stats, nil
}

// SetStats 批量写入视频计数缓存。
func (c *FeedCache) SetStats(ctx context.Context, stats map[int64]*domainfeed.FeedStat, ttl time.Duration) error {
	pipe := c.client.Pipeline()
	queued := false

	for _, stat := range stats {
		if stat == nil || stat.VideoID <= 0 {
			continue
		}
		content, err := json.Marshal(stat)
		if err != nil {
			return err
		}
		pipe.Set(ctx, feedStatKey(stat.VideoID), content, ttl)
		queued = true
	}
	if !queued {
		return nil
	}
	_, err := pipe.Exec(ctx)
	inframetrics.ObserveCacheWrite("stat", len(stats), err)
	return err
}

// SetVideoStat 写入单个视频的计数缓存，用于评论写入后刷新 Feed 展示。
func (c *FeedCache) SetVideoStat(ctx context.Context, stat *domaininteraction.VideoStat) error {
	if stat == nil || stat.VideoID <= 0 {
		return nil
	}
	err := setActionStatJSON(ctx, c.client, feedStatKey(stat.VideoID), videoStatToFeedStat(stat))
	inframetrics.ObserveCacheWrite("stat", 1, err)
	return err
}

func videoStatToFeedStat(stat *domaininteraction.VideoStat) *domainfeed.FeedStat {
	if stat == nil {
		return nil
	}
	return &domainfeed.FeedStat{
		VideoID:       stat.VideoID,
		LikeCount:     stat.LikeCount,
		CommentCount:  stat.CommentCount,
		FavoriteCount: stat.FavoriteCount,
	}
}

func (c *FeedCache) AddInboxItems(ctx context.Context, authorID int64, userIDs []int64, item *domainfeed.FeedPageItem, maxLen int64) error {
	if authorID <= 0 || item == nil || item.VideoID <= 0 || item.PublishedAt.IsZero() || len(userIDs) == 0 {
		return nil
	}
	if maxLen <= 0 {
		maxLen = 1000
	}
	pipe := c.client.Pipeline()
	score := followingIndexScore(item.PublishedAt, item.VideoID)
	member := followingIndexMember(item.VideoID, authorID, item.PublishedAt)
	for _, userID := range userIDs {
		if userID <= 0 {
			continue
		}
		key := followingInboxKey(userID)
		pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: member})
		pipe.ZRemRangeByRank(ctx, key, 0, -maxLen-1)
		pipe.Expire(ctx, key, followingIndexKeyTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (c *FeedCache) AddAuthorOutboxItem(ctx context.Context, authorID int64, item *domainfeed.FeedPageItem, maxLen int64) error {
	if authorID <= 0 || item == nil || item.VideoID <= 0 || item.PublishedAt.IsZero() {
		return nil
	}
	if maxLen <= 0 {
		maxLen = 500
	}
	key := followingAuthorOutboxKey(authorID)
	score := followingIndexScore(item.PublishedAt, item.VideoID)
	member := followingIndexMember(item.VideoID, authorID, item.PublishedAt)
	pipe := c.client.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: member})
	pipe.ZRemRangeByRank(ctx, key, 0, -maxLen-1)
	pipe.Expire(ctx, key, followingIndexKeyTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *FeedCache) ListFollowingIndexPage(ctx context.Context, viewerID int64, authorIDs []int64, cursor *domainfeed.TimelineCursor, limit int) ([]*domainfeed.FeedPageItem, bool, error) {
	if viewerID <= 0 || limit <= 0 {
		return []*domainfeed.FeedPageItem{}, false, nil
	}
	keys := []string{followingInboxKey(viewerID)}
	for _, authorID := range authorIDs {
		if authorID > 0 {
			keys = append(keys, followingAuthorOutboxKey(authorID))
		}
	}

	pipe := c.client.Pipeline()
	cardinalityCommands := make([]*redis.IntCmd, 0, len(keys))
	rangeCommands := make([]*redis.StringSliceCmd, 0, len(keys))
	minScore := "-inf"
	maxScore := "+inf"
	if cursor != nil {
		maxScore = fmt.Sprintf("(%f", followingIndexScore(cursor.PublishedAt, cursor.VideoID))
	}
	for _, key := range keys {
		cardinalityCommands = append(cardinalityCommands, pipe.ZCard(ctx, key))
		rangeCommands = append(rangeCommands, pipe.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{
			Min:   minScore,
			Max:   maxScore,
			Count: int64(limit),
		}))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, false, err
	}

	hasIndex := false
	for _, cmd := range cardinalityCommands {
		count, err := cmd.Result()
		if err != nil && err != redis.Nil {
			return nil, false, err
		}
		if count > 0 {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return nil, false, nil
	}

	// 每源已按 score 降序返回,交给 K 路归并合并取前 limit 条。
	streams := make([][]string, len(rangeCommands))
	for i, cmd := range rangeCommands {
		members, err := cmd.Result()
		if err != nil && err != redis.Nil {
			return nil, false, err
		}
		streams[i] = members
	}
	items := mergeFollowingIndexes(streams, authorIDs, limit)
	return items, true, nil
}

// AddHotScore 把一次互动热度写入 1 分钟粒度的热榜桶。
func (c *FeedCache) AddHotScore(ctx context.Context, videoID int64, scoreDelta int, at time.Time) error {
	if videoID <= 0 || scoreDelta == 0 {
		return nil
	}

	key := hotMinuteKey(at)
	if err := c.client.ZIncrBy(ctx, key, float64(scoreDelta), hotRankMember(videoID)).Err(); err != nil {
		return err
	}
	return c.client.Expire(ctx, key, hotMinuteBucketTTL).Err()
}

// ListHotWindowPage 合并最近 60 个分钟桶，返回一小时滑动窗口内的热榜页。
func (c *FeedCache) ListHotWindowPage(ctx context.Context, windowEnd time.Time, offset int, limit int) ([]*domainfeed.FeedPageItem, error) {
	items := []*domainfeed.FeedPageItem{}
	if limit <= 0 {
		return items, nil
	}
	if offset < 0 {
		offset = 0
	}

	windowEnd = windowEnd.UTC().Truncate(time.Minute)
	windowKey := hotWindowKey(windowEnd)
	exists, err := c.client.Exists(ctx, windowKey).Result()
	if err != nil {
		return nil, err
	}
	if exists == 0 {
		if err := c.rebuildHotWindow(ctx, windowKey, windowEnd); err != nil {
			return nil, err
		}
	}

	return c.listHotWindowPage(ctx, windowKey, offset, limit)
}

func (c *FeedCache) rebuildHotWindow(ctx context.Context, windowKey string, windowEnd time.Time) error {
	if _, err := c.client.ZUnionStore(ctx, windowKey, &redis.ZStore{
		Keys:      hotWindowMinuteKeys(windowEnd),
		Aggregate: "SUM",
	}).Result(); err != nil {
		return err
	}

	pipe := c.client.Pipeline()
	pipe.ZRemRangeByScore(ctx, windowKey, "-inf", "0")
	pipe.Expire(ctx, windowKey, hotWindowCacheTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *FeedCache) listHotWindowPage(ctx context.Context, windowKey string, offset int, limit int) ([]*domainfeed.FeedPageItem, error) {
	items := []*domainfeed.FeedPageItem{}
	values, err := c.client.ZRevRangeWithScores(ctx, windowKey, int64(offset), int64(offset+limit-1)).Result()
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		member, ok := value.Member.(string)
		if !ok {
			continue
		}
		videoID, ok := hotRankVideoID(member)
		if !ok {
			continue
		}
		items = append(items, &domainfeed.FeedPageItem{
			VideoID:  videoID,
			HotScore: int(value.Score),
		})
	}
	return items, nil
}

// SetActionState 写入 Redis 行为状态和实时计数，供点赞收藏接口快速返回。
// 读-算-写由 Lua 脚本原子完成,不再依赖 WATCH+MULTI 的乐观锁与重试。
func (c *FeedCache) SetActionState(ctx context.Context, userID int64, videoID int64, actionType string, active bool, idempotencyKey string, initialStat *domaininteraction.VideoStat) (*applicationinteraction.ActionStateResult, error) {
	actionType, err := domaininteraction.NormalizeActionType(actionType)
	if err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)

	actionKey := interactionActionKey(userID, videoID, actionType)
	counterBaseKey := interactionStatCounterBaseKey(videoID)
	counterShardKey := interactionStatCounterShardKey(videoID, interactionStatCounterShardIndex(userID))
	jsonKey := feedStatKey(videoID)
	targetStatus := domaininteraction.ActionStatusCanceled
	if active {
		targetStatus = domaininteraction.ActionStatusActive
	}

	baseStat := actionStatBaseInit(videoID, initialStat)

	activeFlag := 0
	if active {
		activeFlag = 1
	}

	values, err := actionStateScript.Run(ctx, c.client, []string{actionKey, counterBaseKey, counterShardKey},
		activeFlag,
		targetStatus,
		idempotencyKey,
		time.Now().UTC().Format(time.RFC3339Nano),
		int64(actionStateTTL/time.Second),
		baseStat.LikeCount,
		baseStat.CommentCount,
		baseStat.FavoriteCount,
		int64(actionStatTTL/time.Second),
		interactionStatField(actionType),
	).Result()
	if err != nil {
		return nil, err
	}

	parts, ok := values.([]interface{})
	if !ok || len(parts) < 2 {
		return nil, fmt.Errorf("unexpected action state lua result: %v", values)
	}
	effectiveStatus, ok1 := parts[0].(int64)
	delta, ok2 := parts[1].(int64)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("unexpected action state lua result: %v", values)
	}

	result := &applicationinteraction.ActionStateResult{
		VideoID:        videoID,
		ActionType:     actionType,
		Active:         effectiveStatus == domaininteraction.ActionStatusActive,
		Delta:          int(delta),
		IdempotencyKey: idempotencyKey,
	}

	stat, err := actionStat(ctx, c.client, counterBaseKey, interactionStatCounterShardKeys(videoID), jsonKey, videoID, initialStat)
	if err != nil {
		return nil, err
	}
	result.LikeCount = stat.LikeCount
	result.FavoriteCount = stat.FavoriteCount
	_ = setActionStatJSON(ctx, c.client, jsonKey, stat)
	return result, nil
}

func actionStat(ctx context.Context, client redisActionStatReader, counterBaseKey string, counterShardKeys []string, jsonKey string, videoID int64, initialStat *domaininteraction.VideoStat) (*domainfeed.FeedStat, error) {
	stat, _, err := actionStatWithPresence(ctx, client, counterBaseKey, counterShardKeys, jsonKey, videoID, initialStat)
	if err != nil {
		return nil, err
	}
	if stat == nil {
		return &domainfeed.FeedStat{VideoID: videoID}, nil
	}
	return stat, nil
}

func actionStatFromCache(ctx context.Context, client redisActionStatReader, videoID int64) (*domainfeed.FeedStat, bool, error) {
	return actionStatWithPresence(ctx, client, interactionStatCounterBaseKey(videoID), interactionStatCounterShardKeys(videoID), feedStatKey(videoID), videoID, nil)
}

func actionStatWithPresence(ctx context.Context, client redisActionStatReader, counterBaseKey string, counterShardKeys []string, jsonKey string, videoID int64, initialStat *domaininteraction.VideoStat) (*domainfeed.FeedStat, bool, error) {
	stat := &domainfeed.FeedStat{VideoID: videoID}
	found := false
	values, err := client.HGetAll(ctx, counterBaseKey).Result()
	if err != nil {
		return nil, false, err
	}
	if len(values) > 0 {
		applyActionStatFields(stat, values)
		found = true
	} else {
		fallbackStat, ok, err := actionStatFallback(ctx, client, jsonKey, videoID, initialStat)
		if err != nil {
			return nil, false, err
		}
		if ok {
			stat = fallbackStat
			found = true
		}
	}
	shardFound, err := applyActionStatShardDeltas(ctx, client, stat, counterShardKeys)
	if err != nil {
		return nil, false, err
	}
	found = found || shardFound
	if !found {
		return nil, false, nil
	}
	return stat, true, nil
}

func actionStatFallback(ctx context.Context, client redisActionStatReader, jsonKey string, videoID int64, initialStat *domaininteraction.VideoStat) (*domainfeed.FeedStat, bool, error) {
	stat := &domainfeed.FeedStat{VideoID: videoID}
	content, err := client.Get(ctx, jsonKey).Bytes()
	if err == redis.Nil {
		if initialStat != nil {
			stat.LikeCount = initialStat.LikeCount
			stat.CommentCount = initialStat.CommentCount
			stat.FavoriteCount = initialStat.FavoriteCount
			return stat, true, nil
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(content, stat); err != nil {
		return nil, false, nil
	}
	if stat.VideoID <= 0 {
		stat.VideoID = videoID
	}
	return stat, true, nil
}

func actionStatBaseInit(videoID int64, initialStat *domaininteraction.VideoStat) *domaininteraction.VideoStat {
	if initialStat != nil {
		return initialStat
	}
	return &domaininteraction.VideoStat{VideoID: videoID}
}

func setActionStatJSON(ctx context.Context, client redisActionStatWriter, jsonKey string, stat *domainfeed.FeedStat) error {
	content, err := json.Marshal(stat)
	if err != nil {
		return err
	}
	return client.Set(ctx, jsonKey, content, actionStatJSONTTL).Err()
}

func applyActionStatShardDeltas(ctx context.Context, client redisActionStatReader, stat *domainfeed.FeedStat, shardKeys []string) (bool, error) {
	if stat == nil || len(shardKeys) == 0 {
		return false, nil
	}

	shardValues, err := loadActionStatShardValues(ctx, client, shardKeys)
	if err != nil {
		return false, err
	}
	found := false
	likeDelta := 0
	favoriteDelta := 0
	for _, values := range shardValues {
		if len(values) > 0 {
			found = true
		}
		likeDelta += actionStatFieldInt(values, "like_count")
		favoriteDelta += actionStatFieldInt(values, "favorite_count")
	}
	stat.LikeCount = clampRedisCount(stat.LikeCount + likeDelta)
	stat.FavoriteCount = clampRedisCount(stat.FavoriteCount + favoriteDelta)
	return found, nil
}

func loadActionStatShardValues(ctx context.Context, client redisActionStatReader, shardKeys []string) ([]map[string]string, error) {
	type pipelineProvider interface {
		Pipeline() redis.Pipeliner
	}

	if provider, ok := client.(pipelineProvider); ok {
		pipe := provider.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, 0, len(shardKeys))
		for _, key := range shardKeys {
			cmds = append(cmds, pipe.HGetAll(ctx, key))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return nil, err
		}
		values := make([]map[string]string, 0, len(cmds))
		for _, cmd := range cmds {
			value, err := cmd.Result()
			if err != nil && err != redis.Nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}

	values := make([]map[string]string, 0, len(shardKeys))
	for _, key := range shardKeys {
		value, err := client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func applyActionStatFields(stat *domainfeed.FeedStat, values map[string]string) {
	if stat == nil {
		return
	}
	stat.LikeCount = actionStatFieldInt(values, "like_count")
	stat.CommentCount = actionStatFieldInt(values, "comment_count")
	stat.FavoriteCount = actionStatFieldInt(values, "favorite_count")
}

func actionStatFieldInt(values map[string]string, field string) int {
	value, _ := strconv.Atoi(values[field])
	return value
}

func cacheKeys(videoIDs []int64, build func(int64) string) []string {
	keys := make([]string, 0, len(videoIDs))
	for _, videoID := range videoIDs {
		keys = append(keys, build(videoID))
	}
	return keys
}

func feedCardKey(videoID int64) string {
	return fmt.Sprintf("video:card:v1:%d", videoID)
}

func feedStatKey(videoID int64) string {
	return fmt.Sprintf("video:stat:v1:%d", videoID)
}

func followingInboxKey(userID int64) string {
	return fmt.Sprintf("feed:following:inbox:v1:%d", userID)
}

func followingAuthorOutboxKey(authorID int64) string {
	return fmt.Sprintf("feed:following:author:v1:%d", authorID)
}

func followingIndexScore(publishedAt time.Time, videoID int64) float64 {
	return float64(publishedAt.UTC().Unix()*1000000 + videoID%1000000)
}

func followingIndexMember(videoID int64, authorID int64, publishedAt time.Time) string {
	return fmt.Sprintf("%d:%d:%s", videoID, authorID, publishedAt.UTC().Format(time.RFC3339Nano))
}

func feedPageItemFromFollowingMember(member string) (*domainfeed.FeedPageItem, bool) {
	parts := strings.SplitN(member, ":", 3)
	if len(parts) != 2 && len(parts) != 3 {
		return nil, false
	}
	videoID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || videoID <= 0 {
		return nil, false
	}
	authorID := int64(0)
	publishedAtIndex := 1
	if len(parts) == 3 {
		authorID, _ = strconv.ParseInt(parts[1], 10, 64)
		publishedAtIndex = 2
	}
	publishedAt, err := time.Parse(time.RFC3339Nano, parts[publishedAtIndex])
	if err != nil || publishedAt.IsZero() {
		return nil, false
	}
	return &domainfeed.FeedPageItem{
		VideoID:     videoID,
		AuthorID:    authorID,
		PublishedAt: publishedAt,
	}, true
}

func int64Set(values []int64) map[int64]struct{} {
	set := map[int64]struct{}{}
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// followingMergeEntry 是 K 路归并的堆元素:一个数据源当前暴露的头部条目。
type followingMergeEntry struct {
	score  float64
	srcIdx int
	item   *domainfeed.FeedPageItem
}

// followingMergeHeap 是最大堆:score 大的优先弹出;同 score 时 inbox(下标 0)优先。
type followingMergeHeap []*followingMergeEntry

func (h followingMergeHeap) Len() int { return len(h) }
func (h followingMergeHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score > h[j].score
	}
	return h[i].srcIdx < h[j].srcIdx
}
func (h followingMergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *followingMergeHeap) Push(x any)   { *h = append(*h, x.(*followingMergeEntry)) }
func (h *followingMergeHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	*h = old[:n-1]
	return entry
}

// mergeFollowingIndexes 对多个已按 score 降序排列的成员流做 K 路归并,
// 返回按 score 降序的前 limit 条:同一视频(VideoID)只保留第一条(源下标小的优先),
// outbox(下标 > 0)条目校验作者仍在关注列表,取满 limit 即提前终止。
func mergeFollowingIndexes(streams [][]string, authorIDs []int64, limit int) []*domainfeed.FeedPageItem {
	if limit <= 0 {
		return nil
	}
	allowed := int64Set(authorIDs)
	pos := make([]int, len(streams))
	h := &followingMergeHeap{}
	heap.Init(h)

	// next 从 srcIdx 源取下一条有效条目入堆:跳过解析失败、作者不在关注列表(outbox)的。
	next := func(srcIdx int) {
		members := streams[srcIdx]
		for pos[srcIdx] < len(members) {
			member := members[pos[srcIdx]]
			pos[srcIdx]++
			item, ok := feedPageItemFromFollowingMember(member)
			if !ok {
				continue
			}
			if srcIdx > 0 && item.AuthorID > 0 {
				if _, followed := allowed[item.AuthorID]; !followed {
					continue
				}
			}
			heap.Push(h, &followingMergeEntry{
				score:  followingIndexScore(item.PublishedAt, item.VideoID),
				srcIdx: srcIdx,
				item:   item,
			})
			return
		}
	}

	for i := range streams {
		next(i)
	}

	seen := map[int64]struct{}{}
	items := make([]*domainfeed.FeedPageItem, 0, limit)
	for len(items) < limit && h.Len() > 0 {
		entry := heap.Pop(h).(*followingMergeEntry)
		next(entry.srcIdx)
		if _, exists := seen[entry.item.VideoID]; exists {
			continue
		}
		seen[entry.item.VideoID] = struct{}{}
		items = append(items, entry.item)
	}
	return items
}

func interactionActionKey(userID int64, videoID int64, actionType string) string {
	return fmt.Sprintf("interaction:action:v1:%d:%d:%s", userID, videoID, strings.ToLower(actionType))
}

func interactionStatCounterKey(videoID int64) string {
	return fmt.Sprintf("video:stat:counter:v1:%d", videoID)
}

func interactionStatCounterBaseKey(videoID int64) string {
	return fmt.Sprintf("%s:base", interactionStatCounterKey(videoID))
}

func interactionStatCounterShardKey(videoID int64, shard int) string {
	return fmt.Sprintf("%s:shard:%02d", interactionStatCounterKey(videoID), shard)
}

func interactionStatCounterShardKeys(videoID int64) []string {
	keys := make([]string, 0, actionStatCounterShardCount)
	for shard := 0; shard < actionStatCounterShardCount; shard++ {
		keys = append(keys, interactionStatCounterShardKey(videoID, shard))
	}
	return keys
}

func interactionStatCounterShardIndex(userID int64) int {
	if userID <= 0 {
		return 0
	}
	return int(userID % actionStatCounterShardCount)
}

func interactionStatField(actionType string) string {
	if actionType == domaininteraction.ActionTypeLike {
		return "like_count"
	}
	return "favorite_count"
}

func clampRedisCount(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func hotMinuteKey(at time.Time) string {
	return fmt.Sprintf("feed:hot:minute:v1:%s", at.UTC().Truncate(time.Minute).Format("200601021504"))
}

func hotWindowKey(windowEnd time.Time) string {
	return fmt.Sprintf("feed:hot:window:v1:%d", windowEnd.UTC().Truncate(time.Minute).Unix())
}

func hotWindowMinuteKeys(windowEnd time.Time) []string {
	keys := make([]string, 0, hotWindowMinutes)
	for index := hotWindowMinutes - 1; index >= 0; index-- {
		keys = append(keys, hotMinuteKey(windowEnd.Add(-time.Duration(index)*time.Minute)))
	}
	return keys
}

func hotRankMember(videoID int64) string {
	return fmt.Sprintf("%020d", videoID)
}

func hotRankVideoID(member string) (int64, bool) {
	value := strings.TrimLeft(member, "0")
	if value == "" {
		return 0, false
	}
	videoID, err := strconv.ParseInt(value, 10, 64)
	return videoID, err == nil && videoID > 0
}

func cacheValueBytes(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}
