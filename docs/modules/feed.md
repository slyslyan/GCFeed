# Feed 模块设计

## 1. 模块职责
负责刷视频主链路，输出游标分页结果并记录观看行为。

## 2. 接口设计

| 方法 | 接口路径 | 作用 | 鉴权 | 幂等键 |
| --- | --- | --- | --- | --- |
| GET | `/api/feed-items` | 获取按 scene 排序的视频流 | 最新/热门可匿名；推荐/关注需登录 | - |
| POST | `/api/feed-queries` | 使用请求体传入 scene、cursor、limit、context | 同上 | - |
| POST | `/api/video-view-events` | 上报曝光和观看事件 | 登录 | 请求标识参与事件去重 |

### 2.1 Feed Items API

用于返回已上线视频，支持最新、热门、关注和推荐四个 Feed 场景。推荐的训练与服务契约见[推荐模块](recommendation.md)。

#### GET `/api/feed-items`

请求参数：

| 参数 | 位置 | 类型 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `cursor` | query | string | 否 | - | 上一页返回的游标 |
| `limit` | query | int | 否 | 10 | 返回数量，最大 100 |
| `scene` | query | string | 否 | `timeline` | Feed 场景：`timeline`、`hot`、`following`、`recommend` |

响应：

```json
{
  "scene": "timeline",
  "items": [
    {
      "video_id": 1001,
      "author_id": 12,
      "author_nickname": "tester",
      "author_avatar_url": "https://example.com/avatar.png",
      "title": "first video",
      "description": "hello timeline",
      "media_url": "https://example.com/video.mp4",
      "cover_url": "https://example.com/cover.jpg",
      "like_count": 0,
      "comment_count": 0,
      "favorite_count": 0,
      "published_at": "2026-05-03T12:00:00Z"
    }
  ],
  "next_cursor": "eyJwdWJsaXNoZWRfYXQiOiIyMDI2LTA1LTAzVDEyOjAwOjAwWiIsInZpZGVvX2lkIjoxMDAxfQ",
  "has_more": true
}
```

排序规则：

`scene=timeline`：

| 排序字段 | 方向 | 说明 |
| --- | --- | --- |
| `published_at` | DESC | 发布时间越新越靠前 |
| `id` | DESC | 同一发布时间下按视频ID倒序 |

`scene=hot`：

| 排序字段 | 方向 | 说明 |
| --- | --- | --- |
| `hot_score` | DESC | 最近 60 个分钟桶内的互动热度分 |
| `video_id` | DESC | 同分时按视频 ID 倒序 |

游标内容：

`scene=timeline`：

| 字段 | 说明 |
| --- | --- |
| `published_at` | 当前页最后一条视频的发布时间 |
| `video_id` | 当前页最后一条视频ID |

`scene=hot`：

| 字段 | 说明 |
| --- | --- |
| `window_end` | 当前热榜窗口结束分钟 |
| `offset` | 下一页起始排名位置 |

## 3. 数据表设计

Feed 依赖已有 `video` 和 `video_stat` 表读取已上线视频与互动计数。曝光上报写入 `video_view_events` 行为流水，并在 `event_type=exposed` 时维护 `exposures` 聚合索引。

Web 端累计可见且实际前进的播放达到 3 秒时上报 `play`，实际播放覆盖视频时长的 95% 时上报 `complete`。短视频先达到完播条件时也会上报 `play`。暂停、卡顿、后台播放和 seek 跳过的区间不计作有效观看；loop 不依赖 `ended` 回调。`watch_ms` 是该事件触发时的累计有效时长，`play` 不是离开视频时的最终观看时长。

带 `request_id` 的新事件按用户、视频、场景、请求和事件类型去重，重试返回原记录，重复曝光不额外增加计数。历史记录不回填去重键；没有请求标识的事件保持独立流水。反馈失败不修改页面登录态，切视频及切账号不会把旧观看时长转给新对象。这些行为用于后续离线训练，不会实时改变已加载的推荐快照。

`video_view_events`：

| 字段 | 说明 |
| --- | --- |
| `id` | 主键 |
| `user_id` | 上报用户 |
| `video_id` | 视频 ID |
| `scene` | Feed 场景 |
| `request_id` | 一次 Feed 请求标识 |
| `dedup_key` | 带请求标识的新事件使用唯一键去重。历史记录和没有请求标识的事件允许为空 |
| `event_type` | `exposed`、`play`、`complete`、`skip` |
| `watch_ms` | 观看时长 |
| `completed` | 是否完播 |
| `created_at` | 事件时间 |

`exposures`：

| 字段 | 说明 |
| --- | --- |
| `user_id` + `video_id` | 唯一曝光事实 |
| `first_exposed_at` | 首次曝光时间 |
| `last_exposed_at` | 最近曝光时间 |
| `exposure_count` | 重复曝光次数 |
| `last_scene` | 最近曝光场景 |

推荐索引：

```sql
CREATE INDEX idx_video_timeline ON video (status, published_at DESC, id DESC);
```

## 4. Timeline 访问优化

`scene=timeline` 启用 Redis 读缓存：

| 缓存项 | TTL |
| --- | --- |
| 首页 | 5 秒 + 抖动 |
| 后续页 | 45 秒 + 抖动 |
| 视频卡片 | 15 分钟 |
| 视频计数 | 15 秒 |

缓存 key：

```text
feed:page:v1:{scene}:limit:{limit}:first
feed:page:v1:{scene}:limit:{limit}:cursor:{cursorHash}
video:card:v1:{video_id}
video:stat:v1:{video_id}
```

页缓存只保存 `video_id` 和排序字段。Feed Service 读取页后使用 Redis MGET 批量读取 `video:card` 和 `video:stat`，缓存缺失时批量回源 MySQL。页缓存未命中时使用 singleflight 合并同 key 回源请求。

## 5. Hot 访问优化

`scene=hot` 使用 Redis ZSET 维护一小时滑动热榜，粒度为 1 分钟。互动写入时按权重写入当前分钟桶：点赞 3 分，收藏 4 分，评论 5 分；取消点赞、取消收藏、删除评论写入对应负分。

缓存 key：

```text
feed:hot:minute:v1:{yyyyMMddHHmm}
feed:hot:window:v1:{windowEndUnix}
```

读取热榜时，Feed Service 合并窗口结束分钟前 60 个分钟桶，移除汇总分小于等于 0 的条目，再按分数倒序读取当前页。分钟桶 TTL 为 2 小时，窗口临时 key TTL 为 2 分钟。

> 拓展规划：时间衰减、观看权重、冷启动探索和异步热度写入尚未实施，需另建 OpenSpec change。热门榜单与当前双塔推荐是独立频道。

## 6. Recommend 读取与分页

`scene=recommend` 在登录后调用双塔推荐服务，保留相似度排序、同作者打散和近期曝光过滤，再批量组装视频卡片。`POST /api/feed-queries` 可在 `context.request_id` 传入本次浏览请求标识；Web 观看事件复用该标识。

推荐游标绑定快照内容版本；模型升级后旧模型游标返回 400，客户端应刷新首屏。模型文件不存在时仅推荐首屏回退到最新流，续页使用 `latest.` 前缀绑定最新流顺序，期间加载模型也不会改换排序。模型损坏或 Milvus 不可用返回 503；有效模型没有可用候选时返回空列表。响应场景始终为 `recommend`，因此仅看到 200 和该场景名称不能证明模型已经加载。
