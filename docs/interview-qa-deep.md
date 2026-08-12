# GCFeed 面试深度讲解

本文档围绕 GCFeed（短视频 Feed 系统）的五大核心能力展开，每节含基础问题、追问及详细答案，用于面试深度复盘。

---

## 1. 多类型 Feed 流分发优化

### 核心叙事

> 实现时间线、关注流、推荐流、热榜流四大流，采用游标分页、缓存、批量查询机制大幅提升 Feed 读取性能；使用推拉结合分发策略：普通创作者发布推送至粉丝收件箱，大 V 发布采用拉取合并模式，规避海量 fanout 造成的服务压力。

### 基础问题

**Q1: 四种 Feed 流分别怎么实现？场景如何切换？**

答：通过 `scene` 参数切换四种 Feed 策略，同一入口 `GET /api/feed-items?scene=xxx`：

| 流 | scene | 排序 | 数据结构 | 鉴权 |
|----|-------|------|---------|------|
| 时间线 | `timeline` | `published_at DESC, id DESC` | MySQL 全量视频表 | 无需登录 |
| 热榜 | `hot` | 互动热度分 DESC | Redis ZSET 60 分钟滑动窗口 | 无需登录 |
| 推荐 | `recommend` | RankScore DESC | 基于曝光去重 + 候选排序 | 需登录（POST /api/feed-queries） |
| 关注 | `following` | `published_at DESC` | Redis ZSET inbox + 大 V outbox 合并 | 需登录 |

Feed Service 根据 scene 分发到不同策略函数，返回统一结构 `items + next_cursor + has_more`。

**Q2: 推拉结合是怎么做的？大 V 阈值为什么是 10000？**

答：

- **推（Push/Fanout-on-write）**：普通创作者（粉丝 < 10000）发布视频时，同步写入每个粉丝的 Redis inbox（`feed:following:inbox:v1:{followerID}`）。因为粉丝量小，fanout 耗时可控。
- **拉（Pull-on-read）**：大 V（粉丝 ≥ 10000）发布时不推送到粉丝 inbox，而是写入自己的 outbox（`feed:following:author:v1:{authorID}`）。粉丝刷关注流时，从关注的大 V 列表中读取各家的 outbox，与自己的 inbox 合并、去重、排序后返回。

合并实现（`ListFollowingIndexPage` → `mergeFollowingIndexes`）：每源 `ZRevRangeByScore` 已按 score 降序返回，**K 路归并（最大堆）取前 limit 条，复杂度 O(limit·logN)、取满提前终止**；同一视频（VideoID）去重保留 inbox 版（源下标小的优先），outbox 条目校验作者仍在关注列表（防取关残留）。排序键 score = `publishedAt.Unix()*1000000 + videoID%1000000`，游标用开区间传回每源，翻页不重不漏。

10000 这个阈值不是拍脑袋定的，而是从四个维度综合推导出来的：

**1. 写入延迟预算**（最直接的约束）

粉丝 inbox 是 Redis ZSET，`ZADD` 逐个 key 执行。设单次 ZADD 耗时 ~0.05ms（同机房 Redis），P99 发布延迟预算 500ms：
- 10000 粉丝 × 0.05ms = 500ms，刚好卡在预算上限
- 50000 粉丝 = 2.5s，接口直接超时
- 所以阈值必须控制在"ZADD 总耗时 < 发布接口超时"的范围内

实际代码里 batch size 是 500（`defaultFollowerBatchSize = 500`），即 10000 粉丝 = 20 次 pipeline 批量写入，不是 10000 次单 key 操作，所以真实耗时在 **100-200ms** 级别，仍有安全余量。

**2. Redis 内存约束**（容量维度）

每个用户 inbox 上限 1000 条 ZSET 条目，每条 ~64 bytes。100 万 DAU × 1000 条 × 64 bytes ≈ 64GB。这是 Redis 集群的基线内存。如果全部走推模式，大 V 一条视频要写入百万级粉丝的 inbox，每条 ZADD 还会触发 `ZREMRANGEBYRANK` 清理超限条目——相当于一个大 V 发一条视频就触发百万次 Redis 写操作，不仅延迟不可接受，还会瞬间打满 Redis CPU。

**3. 服务器资源约束**（你提到的点）

- **Redis 连接池**：fanout worker 的并发连接数有限（通常 50-100）。推 10K 粉丝用 20 个 pipeline 批次，连接池够用。推 100 万粉丝用 2000 次 pipeline，连接池耗尽，其他请求排队等连接。
- **API 进程 CPU**：fanout 过程中需要序列化消息、构建 ZADD 参数，粉丝数越大 CPU 占用越高。推 10K 粉丝 CPU spike 可接受，推百万粉丝可能把 API 进程打满影响其他接口。
- **网络带宽**：10K 次 Redis 写操作的数据量在 MB 级别，同机房内网不构成瓶颈。

**4. 粉丝分布数据**（业务维度）

实际业务中粉丝分布是典型的长尾/幂律分布：
- 95% 的创作者粉丝 < 1000
- 只有 < 1% 的创作者粉丝 > 10000

意味着 **99% 的发布走推模式，1% 走拉模式**。推模式体验好（inbox 直接有数据），拉模式成本低（只写一个 outbox）。阈值设在 10000 保证了绝大多数发布走推、极少数走拉，这是最优平衡点。

**面试回答的组织顺序**：

先说结论——10000 是从四个维度推导出来的，不是一个 magic number。然后展开：先讲写入延迟预算（最硬的技术约束，500ms 上限倒推），再讲服务器资源（连接池、CPU）、内存约束，最后用粉丝长尾分布收尾（证明 99% 场景推模式适用）。这样面试官听到的是一个完整的设计决策过程，而不是一个死记硬背的数字。

**追问"如果要调整呢"**：改 `BigCreatorFollowerThreshold` 常量即可。但调整需要同步考虑——阈值下调（如改 5000）会让更多创作者走拉模式，粉丝 feed 加载时需要 merge 更多 outbox，读延迟升高；阈值上调（如改 50000）会让更多创作者走推模式，发布延迟和 Redis 压力增大。需要结合线上 P99 发布延迟、Redis CPU、粉丝分布数据做灰度验证。

**Q3: 游标分页怎么解决翻页重复/漏数问题？**

答：传统的 offset 分页在数据频繁插入时会出现"第 2 页看到第 1 页的重复视频"或"漏掉新发布的视频"。游标分页的核心思想是：**用上一页最后一条记录的排序字段值作为下一页的起点**。

Timeline 的游标编码为 `base64(published_at, video_id)`。下一页查询时加上条件 `WHERE (published_at, id) < (cursor.published_at, cursor.video_id)`，保证新插入的数据不会影响已翻过的页。

复合排序字段的设计要求：排序字段 + 稳定次级排序字段（通常是主键 ID），确保每一行的排序位置唯一确定。

**Q4: Feed 缓存架构是怎么设计的？**

答：三层缓存，逐级组装：

```
页缓存 (Page Cache)    → 只存 [video_id, author_id, published_at]
  TTL: 首页 5s+jitter, 后续页 45s+jitter
         ↓ MGET
卡片缓存 (Card Cache)  → { video_id: { title, author, cover_url } }
  TTL: 15min
         ↓ MGET  
计数缓存 (Stat Cache)  → { video_id: { like_count, comment_count, favorite_count } }
  TTL: 15s
```

关键设计：
- **页缓存只存 ID**：最轻量化，一页 10 条只存 10 个 ID + 排序字段，而非完整的 Feed 响应。避免 CDN 缓存内容漂移（标题/计数变了但缓存没更新）。
- **MGET 批量读取**：拿到 ID 列表后 Redis `MGET` 批量读卡片和计数，而不是逐条 `GET`，避免 N+1 问题。
- **回源兜底**：Redis miss 的 ID 收集后批量 `SELECT ... WHERE id IN (...)` 回源 MySQL，写回 Redis。最多一次卡片查询 + 一次计数查询。
- **singleflight 合并**：同一 cache miss key 的并发请求合并为一次回源，防止缓存击穿瞬间打爆 MySQL。

**Q5: Hot Feed 的 60 分钟滑动窗口是怎么实现的？**

答：使用 Redis ZSET 的分钟桶（minute bucket）方案：

1. **写入**：每次互动（点赞/收藏/评论）时 `ZINCRBY feed:hot:minute:v1:{yyyyMMddHHmm} videoID scoreDelta`。分数增量：点赞 +3，收藏 +4，评论 +5（取消时减去对应分数）。
2. **读取**：`ZUNIONSTORE feed:hot:window:v1:{windowEndUnix}` 合并最近 60 个分钟桶，移除分数 ≤ 0 的条目。
3. **分页**：在合并后的窗口 ZSET 上按 `hot_score DESC, video_id DESC` 做 offset 分页（因为窗口每次重建，游标不适用）。

TTL 策略：分钟桶 2 小时（防止窗口滑动时历史桶丢失），合并窗口 2 分钟（短期缓存避免重复 ZUNIONSTORE 开销）。

### 追问

**Q6: 推荐流的去重是怎么做的？**

答：通过曝光记录表 `exposures(user_id, video_id)` 做 7 天内去重。每次推荐请求时查询该用户最近 7 天的曝光记录，从推荐候选集中过滤掉已曝光的视频。曝光事件通过独立的 `exposure` exchange → `gcfeed.exposure.view_event_recorded` 队列异步写入，不阻塞 Feed 响应。

**Q7: 如果页缓存失效（Redis 挂了），Feed 接口还能用吗？**

答：能。每一层缓存都有回源路径——页缓存 miss → 直接查 MySQL 获取 ID 列表 → 卡片 miss → 批量查 MySQL → 计数 miss → 批量查 MySQL。没有 Redis 只是延迟升高（从 5ms 升到几十 ms），不会 500。这是"缓存优先，MySQL 兜底"的设计原则。

**Q8: inbox 为什么限制 1000 条？超出怎么办？**

答：Redis ZSET 的 ZADD 每次操作会检查集合大小，超过 1000 时 `ZREMRANGEBYRANK` 移除最老的条目。限制 1000 条是因为：① 移动端用户通常只看前几十条，1000 条覆盖了数小时的使用；② Redis 内存可控，百万用户各 1000 条 = 10 亿条目，每个条目约 64 bytes → 约 64GB，仍在可控范围。

**Q9: 缓存雪崩、缓存击穿、缓存穿透三者的区别？分别怎么应对？**

答：这是必考八股。三个问题都导致请求打到 DB，但成因不同：

| 问题 | 成因 | 现象 | 本项目方案 |
|------|------|------|-----------|
| 缓存穿透 | 查不存在的数据（恶意伪造 ID），缓存永远 miss | 每次请求穿透到 DB 查不到结果 | 布隆过滤器判断"一定不存在"→ 直接返回，不到 DB |
| 缓存击穿 | 热点数据缓存过期瞬间，大量并发同时回源 | 瞬间 100+ 个请求同时查同一条 SQL | singleflight 合并回源（100→1 个 DB 查询） |
| 缓存雪崩 | 大量缓存在同一时间过期，或 Redis 整体挂掉 | 所有请求都打到 DB，DB 可能被打死 | ① TTL + jitter（5s ± random）错开过期时间；② Redis 挂了仍有 MySQL 兜底（降级不崩溃）|

核心区别：穿透是"查不到的东西反复查"（靠布隆过滤器拦），击穿是"一个热点 key 过期瞬间"（靠 singleflight 合并），雪崩是"一大批 key 同时过期"（靠 TTL jitter 错峰）。面试时要能清晰区分三者。

**Q10: Redis ZSET 底层是什么数据结构？为什么适合热榜？**

答：ZSET 底层在元素少时用 **ziplist**（压缩列表，连续内存、O(n) 查找但省内存），元素多或单个元素大时转 **skiplist + dict**。

跳表（skiplist）是一种随机化的多层链表结构：每一层是一个有序链表，上层是下层的"快速通道"（约 1/2 概率升级）。查找时从最高层开始，遇到比目标大的值就降一层继续，直到第 0 层。平均时间复杂度 O(log n)，和平衡树一样，但实现简单、不需要旋转再平衡。

热榜用 ZSET 的原因：
- `ZADD/ZINCRBY` O(log n) 写入分数（写入快）
- `ZREVRANGE 0 9 WITHSCORES` O(log n + 10) 读取 Top 10（排名查询快）
- `ZUNIONSTORE` 合并 60 个分钟桶（聚合操作原生支持）

如果自己实现——按分数排序的榜单 + 定时过期——需要：排序树 + 定时器扫描 + 合并逻辑。ZSET 三个操作全部内置，代码量减少 90%。

---

## 2. 高并发互动架构

### 核心叙事

> 点赞、收藏等高频互动操作先写入 Redis 缓存，通过 RabbitMQ 异步持久化落库，缩短接口响应耗时，削峰填谷。

### 基础问题

**Q1: 互动写入的完整链路是什么？**

答：

```
HTTP Handler（~2ms）
  → Interaction Service.SetAction
  → Redis WATCH + TxPipelined: 更新 action state + shard counter（~1ms）
  → RabbitMQ Publish ActionChangedEvent
  → 返回 200（总耗时 < 5ms）
  
  ... 异步 ...

  → Worker consume ActionChangedEvent
  → MySQL INSERT/UPDATE interaction_action
  → MySQL UPDATE video_stat
  → Worker ACK
```

同步路径只到 Redis + RabbitMQ，HTTP 响应在毫秒级返回。MySQL 落库完全异步，与用户请求解耦。

**Q2: Redis 怎么保证并发点赞/取消的原子性？**

答：使用 Redis WATCH + TxPipelined（乐观锁）实现 CAS（Compare-And-Swap）：

1. `WATCH interaction:action:v1:{userID}:{videoID}:{type}` 监视当前状态
2. `GET` 读取当前状态（active/canceled）
3. 如果状态未变化 → `MULTI` + `SET` 新状态 + `ZINCRBY` 热榜分数 + `EXEC`
4. 如果 WATCH 期间 key 被其他请求修改 → `EXEC` 失败，重试

这是乐观锁模式——假设冲突概率低（同一用户对同一视频的并发操作极少），不做悲观锁。冲突时重试一次即可，性能远优于分布式锁。

**Q3: 计数为什么用 16 个 shard？**

答：Redis 的 `INCR` 是原子的单 key 操作，但热点视频的计数会被大量并发请求命中同一个 key——即 Redis 热 key 问题。16 shard 方案：`video:stat:counter:v1:{videoID}:shard:{userID % 16}`，将写入分散到 16 个 key 上。

读取时聚合所有 shard：`sum(shard_{00..15}) + base_count`。base_count 是 MySQL 落库后的持久化计数（TTL 24 小时），shard 是增量计数。两个值叠加得到"最终一致"的计数——可能短暂偏差，但 TTL 过期后自动修正。

**Q4: 幂等是怎么保证的？用户重复点赞不会计两次？**

答：多层幂等保护：

- **Redis 层**：WATCH + 状态检查——如果当前已是 LIKE 状态，再次 LIKE 直接返回成功（幂等）。
- **MySQL 层**：`interaction_action` 表有唯一约束 `uk_user_video_type (user_id, video_id, action_type)`，同一操作重复 INSERT 会触发唯一键冲突。
- **业务层**：所有写接口支持 `Idempotency-Key` 头（最长 128 字符），Worker 消费时检查 `user_id + idempotency_key` 是否已存在，重复事件直接 ACK 丢弃。


### 追问

**Q6: RabbitMQ 投递失败怎么办？数据会丢吗？**

答：发布消息时如果 RabbitMQ 不可达，当前设计返回错误给客户端（HTTP 500）。这是一种权衡——宁可让客户端知道操作未完成，也不在后台悄悄丢失数据。Worker 侧：消费使用手动 ACK，只有 MySQL 写入成功后才 ACK。如果 Worker 处理中途崩溃，消息重新入队，另一 Worker 继续处理。MySQL 的唯一约束保证重试不会重复写入。

**Q7: Redis 计数和 MySQL 最终不一致会有多大偏差？**

答：Redis shard counter 的 TTL 是短期缓存（热榜窗口 2 小时），MySQL 是持久化事实。偏差窗口最大是 Worker 消费延迟——通常 < 1 秒。极端情况（Worker 积压几分钟）下，Redis 计数会比 MySQL 高（增量已写入 Redis 但未写入 MySQL）。这是"最终一致性"的代价——换取的是 < 5ms 的互动响应延迟。

**Q8: 为什么不用 Kafka 而用 RabbitMQ？**

答：MVP 阶段 RabbitMQ 足够——支持消息确认、持久化、管理界面友好、Docker Compose 一键部署。Kafka 的优势（高吞吐、日志压缩、分区有序）在当前的互动量级下用不上。演进到百万 DAU 时可以切换到 Kafka——但短期引入 Kafka 只会增加运维复杂度。

**Q9: RabbitMQ 的 exchange 有哪几种类型？项目中用的哪种？**

答：RabbitMQ 的核心路由模型：Producer → Exchange → Queue → Consumer。Exchange 有 4 种类型：

| 类型 | 路由方式 | 适用场景 |
|------|---------|---------|
| direct | routing key 精确匹配 queue binding key | 点对点单播 |
| fanout | 忽略 routing key，广播到所有绑定的 queue | 一条消息多个消费者独立处理 |
| topic | routing key 模式匹配（`*` 匹配一个词，`#` 匹配零或多个词） | 按主题分发，最灵活 |
| headers | 忽略 routing key，按 header 键值对匹配 | 复杂条件路由（很少用） |

项目中互动事件用 **topic** exchange：`interaction.action_changed.{like/comment/favorite}` 的 routing key 让 Worker 可以按互动类型选择性消费。如果未来需要多个独立消费者（一个更新热榜、一个发通知、一个写推荐特征），增加 queue + binding 即可，不需要改 Producer 代码。

---

## 3. 视频播放与上传优化

### 核心叙事

> 支持 HTTP Range 分片请求、播放预加载、播放 QoS 埋点上报、上传文件合法性校验，优化用户短视频播放流畅度。

### 基础问题

**Q1: 视频上传的安全校验做了哪些？**

答：上传接口 `POST /api/uploads` 的合法性校验：

- **文件类型**：校验 MIME type，只允许 video/mp4、image/jpeg、image/png、image/webp
- **文件大小**：限制视频 ≤ 100MB、封面 ≤ 5MB、头像 ≤ 2MB
- **文件扩展名**：双重校验——MIME type + 扩展名白名单
- **文件名**：随机生成 UUID 文件名，不信任用户上传的原始文件名（防路径遍历攻击）
- **存储路径**：`uploads/{kind}/{uuid}.{ext}`，kind 隔离为 video/cover/avatar

上传完成后返回 `/uploads/{kind}/{filename}` 的相对路径，发布视频时引用这些路径。

**Q2: HTTP Range 分片请求是怎么支持的？**

答：视频文件通过 Nginx 静态文件服务（或 Go `http.ServeFile`）返回，HTTP Server 原生支持 `Range` 头。客户端播放器发送 `Range: bytes=0-1048575`，服务端返回 `206 Partial Content` + `Content-Range` 头。效果：用户拖动进度条时只需下载对应分片，不用等整个视频下载完。

Go 侧 `http.ServeContent` 会自动处理 Range 请求、If-Modified-Since、ETag 等标准 HTTP 缓存头。不需要业务代码额外处理。

**Q3: 播放预加载怎么做？**

答：`playback` 模块维护播放配置表，按平台（Web）+ 网络类型（WiFi/4G/5G）配置预加载策略：

- `preload_count = 3`（默认预加载后续 3 个视频）
- `buffer_ms = 1200`（预加载 1.2 秒的缓冲区）

前端播放器根据当前网络类型调用 `/api/playback/config` 获取配置，在播放第一个视频时后台预加载后续视频的首个分片。WiFi 下可以预加载完整的 3 个视频，4G 下只预加载首字节（最小化流量消耗）。

**Q4: 播放 QoS 埋点怎么设计？**

答：通过曝光/播放事件上报，记录完整播放质量数据：

- `event_type`: exposed / play / complete / skip
- `watch_ms`: 实际观看时长
- `completed`: 是否完整播放
- `scene`: 来源场景（timeline/hot/recommend/following）
- `request_id`: 关联推荐请求

数据通过 RabbitMQ 异步写入 `video_view_events` 表，不阻塞 API 响应。分析维度：完播率 = complete_count / play_count，跳过率 = skip_count / exposed_count，按 scene 分维度对比推荐效果。


---

## 4. 高可用与稳定性保障

### 核心叙事

> 使用 Singleflight 合并热点回源、布隆过滤器防止缓存穿透、提交请求去重、多维度限流，保障高并发场景系统稳定。

### 基础问题

**Q1: singleflight 解决什么问题？怎么实现的？**

答：singleflight 解决**缓存击穿**——当热点数据的缓存过期瞬间，大量并发请求同时回源 MySQL，造成数据库瞬时压力。

Go 标准库 `golang.org/x/sync/singleflight` 的实现：对同一个 key 的并发调用，只执行一次实际函数，其他调用等待并共享结果。在 Feed Service 中，页缓存 miss 的回源查询被 singleflight 包装：

```go
// 100 个并发请求同一页，只有 1 个真正查 MySQL
result, err, _ := sg.Do(cacheKey, func() (interface{}, error) {
    return repo.ListTimelinePage(ctx, limit, cursor)
})
```

效果：缓存过期瞬间的 MySQL 查询量从 100 → 1，压力降低 99%。

**Q2: 布隆过滤器怎么防止缓存穿透？**

答：缓存穿透是指查询一个数据库中不存在的数据（如不存在的 videoID），缓存永远 miss，每次请求都穿透到 MySQL。布隆过滤器在 Redis 层之前快速判断"这个 key 一定不存在"。

在 Feed 缓存中，布隆过滤器存储所有有效的 videoID。查询时先过布隆过滤器——如果返回"不存在"，直接返回空，不再查询 Redis 和 MySQL。布隆过滤器的特点：不存在的一定准确（无假阴性），存在的可能误判（有假阳性，概率可控）。假阳性只会导致多一次 MySQL 查询（最终发现不存在），但大幅减少了非法的缓存穿透请求。

**Q3: 去重是怎么做的？**

答：两层去重：

- **幂等键去重**：所有写接口支持 `Idempotency-Key` 头。同一幂等键的重复请求返回同一业务结果，不会产生副作用。实现：首次请求正常处理并缓存 `(userID, idempotency_key) → result`，后续同 key 请求直接返回缓存结果。
- **Worker 消息去重**：RabbitMQ 可能因网络问题投递重复消息。Worker 消费时检查 `user_id + video_id + action_type + idempotency_key` 组合是否已处理。已处理的直接 ACK 丢弃。

**Q4: 多维度限流怎么做？**

答：限流保护核心接口不被突发流量打垮：

- **IP 限流**：基于 Gin middleware，对单 IP 做令牌桶限流（如每秒 100 次）。超过阈值的请求返回 429 Too Many Requests。
- **用户级限流**：对登录用户做更精细的限流（如每分钟 60 次 Feed 刷新），防止单用户恶意刷接口。
- **接口级限流**：对不同接口分级——Feed 读接口限流宽松（500 QPS），写接口严格（50 QPS），上传接口最严格（10 QPS，大文件上传消耗大）。

**Q5: MySQL 连接池怎么配置？**

答：`MaxOpenConns=50, MaxIdleConns=10`。50 个连接足够支撑数百 QPS（每个查询几毫秒），10 个空闲连接避免频繁 TCP 握手。配置过大会浪费 MySQL 内存（每个连接 ~2MB），过小会在突发流量时排队等待连接。此外 DSN 启用 `parseTime=true`（time.Time 扫描）、`charset=utf8mb4`（emoji 支持）、`loc=Local`。

### 追问

**Q6: Redis 不可用时怎么降级？**

答：当前降级策略：读路径中的缓存 miss → 直接回源 MySQL（singleflight 保护）；写路径中的 Redis WATCH 操作 → 返回错误，由客户端重试。更完善的降级方案（后续演进）：写路径也支持 MySQL 直写模式（跳过 Redis，接口延迟升高但功能不受影响），通过配置开关或自动探测 Redis 存活状态切换。


**Q8: DDD 四层架构对稳定性有什么帮助？**

答：DDD 分层在稳定性方面三个关键价值：

1. **Domain 层零外部依赖**：所有业务规则和不变量的单元测试不需要 Mock MySQL/Redis/RabbitMQ，可以极快地跑完（毫秒级）。
2. **依赖反转**：Infrastructure 实现 Domain 定义的接口。换缓存实现（Redis → Memcached）或换消息队列（RabbitMQ → Kafka）时，Domain/Application 层代码零改动。
3. **错误隔离**：每层有明确的错误边界——Domain 返回业务错误（`ErrVideoNotFound`），Application 包装上下文，Infrastructure 处理技术错误（连接超时、主键冲突），Interfaces 映射 HTTP 状态码。错误不会跨层泄漏（不会把 GORM 的 `ErrRecordNotFound` 直接返回给客户端）。

**Q10: 分布式 CAP 理论在你这个项目中怎么体现？**

答：CAP = Consistency（一致性）+ Availability（可用性）+ Partition Tolerance（分区容错性）。分布式系统中 P 是不可避免的（网络会断），所以主要是在 C 和 A 之间做权衡。

GCFeed 中的体现：

- **Redis 互动计数（AP → 最终一致性）**：16 shard counter 先写 Redis → 异步落库 MySQL。Redis 和 MySQL 之间可能短暂不一致（Worker 来不及消费），但最终会一致。牺牲了 C（实时一致），换取了 A（< 5ms 响应，Redis 不可用时有 MySQL 回退）。

- **粉丝 inbox 推送（CP → 强一致）**：发布视频时必须所有粉丝 inbox 写入成功（或批次写入成功），才返回发布成功。如果有粉丝 inbox 写入失败，依赖事务回滚或补偿。这里不牺牲一致性——用户发布了视频，粉丝必须能看到。

- **互动操作的唯一约束（CP）**：MySQL 层 `UNIQUE KEY (user_id, video_id, action_type)` 保证不会重复计数。即使 Redis 被穿透、MQ 重复投递，DB 的唯一约束是最后的防线。这是 DB 层的强一致保证。

总结：**关键数据走 CP（发布成功必须写入成功），展示数据走 AP（计数允许短暂不一致）**。

---

## 5. 工程化、监控与压测

### 核心叙事

> 部署方案：支持 Docker Compose 单机部署、Kubernetes 集群部署；配套完整核心接口自动化测试。监控体系：基于 Prometheus + Grafana 搭建监控面板，覆盖 QPS、错误率、接口延迟、缓存命中率、Worker 消费成功率五大核心指标。压测结果：使用 k6 压测，HTTP 失败率 0%，业务处理成功率 100%。

### 基础问题

**Q1: 项目怎么部署？Docker Compose 和 K8s 分别什么场景用？**

答：

**Docker Compose**：`apps/docker-compose.yml` 定义 6 个服务（MySQL、Redis、RabbitMQ、API、Worker、Web）外加 Prometheus + Grafana。一个 `docker compose up -d --build` 启动全栈。用于本地开发、演示、CI 测试。

**Kubernetes**：`apps/deploy.yaml` 包含 Namespace（gcfeed）、Secrets（DB 密码 + API 配置）、PVC（MySQL 10Gi / Redis 1Gi / RabbitMQ 2Gi / uploads 10Gi）、Deployment + Service。用于生产或模拟生产环境。

Readiness/Liveness 探针：API 容器暴露 `/health` 端点做探活，MySQL/Redis/RabbitMQ 用各自官方镜像自带探针。

**Q2: 监控面板覆盖了哪些指标？**

答：Grafana 预置 dashboard `gcfeed-overview`，五大核心指标：

| 面板 | 指标 | PromQL |
|------|------|--------|
| API QPS | `gcfeed_http_requests_total` | `sum(rate(...[5m])) by (route)` |
| 5xx 错误率 | `gcfeed_http_requests_total{code=~"5.."}` | rate 过滤 5xx |
| API P95 延迟 | `gcfeed_http_request_duration_seconds` | `histogram_quantile(0.95, ...)` |
| Feed P95 延迟 | `gcfeed_feed_request_duration_seconds` | 按 scene 分维度 |
| Feed 缓存命中率 | `gcfeed_feed_cache_requests_total` | `sum(rate(...{result="hit"}[5m])) / sum(rate(...[5m]))` |
| Worker 成功率 | Worker 自定义指标 | 成功消费数 / 总消费数 |

Prometheus 每 15s scrape `:8080/metrics` (API) 和 `:9091/metrics` (Worker)。

**Q3: 压测结果具体是多少？怎么测的？**

答：使用 k6 进行压力测试，核心数据：

| 指标 | 值 |
|------|-----|
| 并发用户 (VUs) | 20 |
| 持续时间 | 60s |
| 总请求数 | ~1200 |
| QPS | 19.85/s |
| 平均延迟 | 5.35ms |
| P95 延迟 | 17.96ms |
| HTTP 失败率 | 0.00% |
| 业务成功率 | 100.00% |

k6 脚本通过 `SCENE` 环境变量切换 timeline/hot/recommend/following 四种场景。极限压测逐步提升 VUS（50→100→200），`THINK_TIME=0` 下测试服务极限吞吐。

阈值设定：`http_req_failed < 1%`、`http_req_duration p95 < 500ms`、`feed_success_rate > 99%`。

**Q4: 测试覆盖了哪些场景？**

答：`apps/api/test/` 下每个模块有独立测试文件，每个接口覆盖：

- 成功路径（正常参数 → 200/201）
- 参数错误（非法 ID、超长字符串、负数 limit → 400）
- 鉴权错误（无 Token / 过期 Token → 401）
- 幂等重复请求（同一 Idempotency-Key 两次 → 返回相同结果）
- 游标分页稳定性（翻多页 → 无重复、顺序一致）
- 关键状态变化和计数变化（点赞后计数 +1，取消后 -1）

全部测试通过 `go test ./...` 运行，是 CI 的准入门槛。

**Q5: 项目工程化方面有哪些亮点？**

答：

1. **DDD 四层架构**：Domain → Application → Infrastructure → Interfaces，每层依赖方向明确，Domain 零外部依赖，可独立测试。
2. **模块标准化**：每个业务模块遵循统一文件组（entity.go / errors.go / repository.go / service.go / model.go / gorm.go / dto.go / handler.go / test），新增模块有明确模板。
3. **依赖注入**：Service 构造函数接收 Repository 接口，Router 层手动装配全链路依赖。没有全局变量和隐式依赖。
4. **配置管理**：`configs/config.yaml` 集中管理所有配置（数据库、Redis、RabbitMQ、JWT、上传限制），支持环境变量覆盖。
5. **优雅启动**：`cmd/feed/main.go` 按顺序启动——config → DB → Redis → RabbitMQ → Gin → health check → metrics endpoint。
6. **OpenSpec 规格驱动**：`openspec/` 目录维护变更提案（proposal / design / tasks），每个功能先设计后编码。

### 追问

**Q6: 为什么选 DDD 四层而不是 MVC 三层？**

答：MVC（Controller → Service → DAO）在业务复杂后容易变成"胖 Service"——所有逻辑堆在 Service 层，切换存储或拆分服务时牵一发动全身。DDD 的核心区别是**依赖反转**——Service 不直接依赖 GORM/Redis 实现，而是依赖 Domain 定义的接口。换数据库只需要改 Infrastructure 层实现，Service 代码不动。DDD 的代码行数确实比 MVC 多（多一层 Domain + Interface 定义），但在面试中更能体现工程素养——你知道什么时候该多写代码，以及为什么。

**Q7: Docker Compose 8 个服务在本地跑得动吗？**

答：可以。MySQL 和 RabbitMQ 是相对轻量的官方镜像，Go API 二进制编译后 ~15MB，Worker 共用同一代码库。本地 8GB 内存的机器足够跑全栈——Docker Compose 有健康检查依赖编排（`depends_on` + `healthcheck`），启动顺序自动管理。

**Q8: 如果 DAU 从 1000 涨到 100 万，哪些地方会先出问题？**

答：按风险排序：

1. **MySQL**：单机扛不住。最先出问题的是 `video` 表（全量查询）+ `video_view_events`（写入量大）。解法：读写分离（主库写、从库读 timeline）、事件表分库（按日期分表）、冷热分离（热榜视频 Redis、冷数据归档）。
2. **Feed 页缓存**：首页 5s TTL 在百万 DAU 下回源频率太高。解法：预热（发布时主动写缓存）、适当延长 TTL、CDN 边缘缓存公开 Feed。
3. **RabbitMQ Worker**：互动量大了以后 Worker 消费速度跟不上。解法：增加 Worker 实例（水平扩展）、互动计数批量写入 MySQL（减少单条 INSERT 事务开销）。
4. **微服务拆分**：`docs/evolution.md` 定义了四阶段拆分路线——先拆 Recommend Service → 再拆 Feed/Interaction Service → gRPC 通信 → 独立数据库。

