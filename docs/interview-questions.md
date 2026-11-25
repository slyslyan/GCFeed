# GCFeed 面试问题与答案清单

这份清单用于围绕 GCFeed 做项目面试复盘。每个问题下面给出一版可直接作答的参考答案，建议回答时按“项目现状、设计原因、扩展方向”三段展开。

推荐相关实现可对照[推荐模块设计](modules/recommendation.md)、[训练与导出说明](../tools/twotower/README.md)及代码核查；演示数据、离线指标与线上效果分别说明。

## 1. 项目总览

### 基础问题

1. GCFeed 解决的核心业务问题是什么？

   答：GCFeed 是一个短视频 Feed 系统，覆盖内容发布、Feed 分发、刷流消费、互动计数、曝光反馈、推荐排序、消息通知和播放优化。它的核心目标是让用户能稳定、低延迟地刷到视频内容，同时让发布、互动和推荐这些高频链路具备扩展能力。项目重点展示后端系统设计能力，包括缓存、异步解耦、游标分页、推拉结合关注流和监控压测。

2. 这个项目的后端、前端、数据库、缓存和消息队列分别用了什么技术？

   答：后端使用 Go、Gin 和 GORM，前端使用 React 和 Vite，数据库使用 MySQL，缓存使用 Redis，消息队列使用 RabbitMQ。鉴权使用 JWT，监控使用 Prometheus 和 Grafana，本地编排使用 Docker Compose。代码按 `Domain / Application / Infrastructure / Interfaces` 四层组织，适合讲清业务边界和技术实现边界。

3. 从用户发布视频到另一个用户刷到视频，完整链路有哪些步骤？

   答：用户先上传媒体文件，再调用 `POST /api/videos`，视频与初始计数在同一 MySQL 事务中保存。发布事件供 fanout Worker 预热卡片、计数并分发关注流。timeline 可以按发布时间读取新视频；推荐频道只检索当前模型快照中的视频，新视频进入推荐需要下一轮离线训练、导出并重启 API 加载，发布 Worker 不生成推荐向量。所有 Feed 再复用批量卡片和计数组装。

4. 项目里有哪些 Feed 场景？`timeline`、`hot`、`following`、`recommend` 的差异是什么？

   答：`timeline` 按发布时间读取公开视频，`hot` 读取互动热榜，`following` 读取关注内容，三者是独立频道。`recommend` 使用离线训练的双塔用户/视频向量，做单路相似度召回、状态和近期曝光过滤，再按页打散作者。缺少模型文件时，推荐首屏展示最新流；文件损坏或 Milvus 不可用返回 503。场景通过 `Strategy` 注册表分发，GET 和 POST 均可进入推荐策略。

5. 这个项目最能体现后端能力的 2-3 个设计点是什么？

   答：第一是 Feed 的 ID-Detail 分离、游标分页、短 TTL 缓存和批量回源；第二是点赞收藏异步落库与视频发布 fanout，API 和 Worker 通过 RabbitMQ 解耦；第三是可复现的 CPU 双塔训练与服务接入，用户/视频向量同批次导出，Go 只做单路相似度检索和结果处理。观看行为先同步保存到 MySQL，离线训练读取这份流水，不能把消息投递或演示集得分说成推荐质量提升。

### 追问

1. 如果让你用 2 分钟介绍这个项目，你会怎么组织答案？

   答：先说明它覆盖短视频发布、Feed 分发、互动与行为反馈。再讲发布写 MySQL、Worker 做关注流 fanout，Feed 按场景批量组装卡片。最后选两个细节展开：缓存与稳定分页，以及从观看流水构造时间隔离样本、训练双塔、导出版本快照并在 API 检索的闭环。效果部分分别报告接口压测和离线召回指标，线上收益需要受控实验。

2. 当前系统的性能瓶颈可能出现在 API、MySQL、Redis、RabbitMQ 哪一层？你怎么判断？

   答：判断顺序看指标：API 看 HTTP P95 和 5xx，MySQL 看慢查询和连接池，Redis 看缓存命中率和命令耗时，RabbitMQ 看队列积压和 Worker 成功率。Feed 读慢通常先看页缓存、卡片缓存和计数缓存命中率，再看批量回源 SQL。互动链路慢则看 Redis Lua 脚本更新耗时、RabbitMQ 投递和 Worker 消费耗时。

3. 如果访问量扩大 10 倍，你会先优化哪条链路？

   答：优先优化 Feed 首页读取，因为它是最高频入口。具体动作包括提升页缓存命中率、缓存预热、增加本地和 Redis 层保护、减少详情回源、对热点 key 做副本或分片。随后优化互动写入链路，把点赞收藏的数据库写入交给 Worker，控制 MQ 消费能力和重试策略。

4. 这个项目适合拆成微服务吗？拆分边界怎么选？

   答：现阶段 Go API 单体更适合快速迭代，目录分层已经提供清晰模块边界。访问量和团队规模上来后，可以按业务稳定边界拆分：账号鉴权、视频发布、Feed、推荐、互动、消息、播放。拆分时优先选择吞吐差异大、依赖清晰、独立扩容价值高的模块，例如 Feed 读取和推荐服务。

## 2. 后端分层架构

### 基础问题

1. `Domain / Application / Infrastructure / Interfaces` 四层分别承担什么职责？

   答：Domain 放实体、领域错误、业务规则和仓储接口；Application 放用例编排、分页游标、幂等、跨模块流程和小接口依赖；Infrastructure 放 GORM、Redis、RabbitMQ、JWT、配置和迁移；Interfaces 放 HTTP Handler、DTO、路由和中间件。依赖方向从外层指向内层，业务规则集中在 Domain 和 Application。

2. `domain/feed`、`application/feed`、`infra/cache`、`interfaces/http/feed` 在 Feed 链路中分别做什么？

   答：`domain/feed` 定义 FeedItem、FeedCard、FeedStat、Scene、Cursor 和 Repository 接口。`application/feed` 实现 timeline、hot、following、recommend 策略，负责分页、缓存读取和卡片组装。`infra/cache` 实现 Redis 页缓存、卡片缓存、计数缓存、热榜 ZSET 和关注流索引。`interfaces/http/feed` 解析 HTTP 参数，调用 Feed Service，并把结果转换成响应 DTO。

3. 为什么 Application 层依赖 Domain 的 Repository 接口？

   答：Application 关心业务能力，例如列出 timeline 页、批量读取卡片、读取关注流。Repository 接口放在 Domain 层后，Application 只依赖抽象能力，GORM、内存实现或测试 mock 都可以替换。这个设计让用例逻辑容易测试，也让基础设施变更对业务层影响更小。

4. GORM Repository 为什么放在 Infrastructure 层？

   答：GORM 是具体持久化技术，属于外部资源实现。Infrastructure 层负责把数据库模型转换成 Domain 实体，并实现 Domain Repository 接口。这样 Application 看到的是业务对象，数据库表结构、GORM 标签和事务细节集中在 infra/persistence 目录。

5. Handler 里应该负责哪些事情？

   答：Handler 负责解析 path、query、body、header，读取鉴权上下文，调用 Application Service，并把业务结果转换成 HTTP 响应。它还负责把领域错误映射成合适的状态码，例如 400、401、403、404、409。业务规则放在 Domain 和 Application，Handler 保持薄入口。

### 追问

1. 如果把业务规则写进 Handler，会带来哪些维护问题？

   答：业务规则分散在 HTTP 入口后，同一能力被 Worker、内部接口或测试复用时会产生重复逻辑。Handler 变厚也会让错误映射、参数解析和业务判断混在一起，降低可读性。GCFeed 通过 Service 和 Domain 构造函数承载规则，让 HTTP 入口只做协议适配。

2. 这个项目里 `Service` 构造函数通过 option 注入 Redis、MQ、JWT，这种设计的好处是什么？

   答：option 注入让基础能力按配置启用，例如 Redis 存在时启用 FeedCache，RabbitMQ 存在时启用发布事件和异步互动。测试时可以传 mock 实现，单体运行时也可以裁剪能力。构造函数保持稳定，新增能力时通过 `WithFeedCache`、`WithRecommender`、`WithAsyncActionPipeline` 这类 option 扩展。

3. 新增审核模块时，你会按什么文件顺序落地？

   答：先建 `domain/review` 的实体、错误和 Repository 接口，再写 `application/review/service.go` 完成审核提交、审核通过、驳回等用例。然后实现 `infra/persistence/review` 的 GORM model 和 repository，补 `interfaces/http/review` 的 DTO 和 Handler，最后在 `router.Register` 装配路由，并增加 API 流程测试和 `docs/modules/review.md`。

4. 为什么 Domain 层尽量只依赖标准库？

   答：Domain 表达核心业务概念，应保持稳定和轻量。它依赖标准库后，业务规则可以脱离 Gin、GORM、Redis 运行，测试成本更低。基础设施升级时，Domain 的实体和不变量仍然保持稳定。

5. 这个分层方式和传统 MVC 的主要差异是什么？

   答：传统 MVC 常把 Controller、Model 和 View 按表现形态拆分，业务逻辑容易落到 Controller 或 Model 方法里。GCFeed 的四层结构按业务规则、用例编排、技术实现和协议入口拆分，依赖方向更明确。这个结构更适合后端项目讲清模块边界、测试替换和基础设施演进。

## 3. Feed 读取链路

### 基础问题

1. `/api/feed-items?scene=timeline&limit=10` 的请求从 Router 到 Service 再到 Repository 的调用链路是什么？

   答：请求先进入 Gin Router，`optionalAuthMiddleware` 解析可选 JWT，然后 `feedHandler.ListFeedItems` 解析 scene、cursor 和 limit。Handler 调用 `feedService.GetFeed`，Service 根据 scene 选择 `TimelineStrategy`，策略先通过 `loadFeedPage` 读取缓存或回源 `repo.ListTimelinePage`。拿到轻量页后，`assembleFeedItems` 批量读取卡片和计数，最后返回 items、next_cursor 和 has_more。

2. `FeedService` 为什么使用 scene 策略注册表？

   答：不同 Feed 场景读取逻辑差异很大，timeline 走时间排序，hot 走热榜窗口，following 走关注索引，recommend 走推荐服务。策略注册表把 scene 和具体策略绑定，`GetFeed` 只负责分发和统一指标记录。新增场景时实现 `Strategy` 接口并注册即可。

3. `TimelineStrategy`、`HotStrategy`、`FollowingStrategy`、`RecommendStrategy` 各自读取数据的方式是什么？

   答：timeline 读取发布时间排序页并缓存，hot 优先读 Redis 分钟桶窗口，following 合并 inbox 与大作者 outbox、索引缺失时回源。recommend 从双塔快照选取用户向量或默认用户向量，以 Milvus ANN 检索；未配置 Milvus 时对同一快照做本地精确相似度检索。随后查询 MySQL 过滤不可用视频、异常作者和近期曝光，复用 Feed 卡片缓存。推荐缺文件首屏及带 `latest.` 前缀的续页走时间线。

4. Feed 返回项为什么需要先查页，再组装详情？

   答：页查询关注排序和分页，只需要视频 ID、作者 ID、发布时间、热度分等轻量字段。详情组装再批量读取视频标题、封面、作者信息和互动计数。这个 ID-Detail 分离设计降低页缓存体积，也让详情缓存和计数缓存可以按不同 TTL 管理。

5. `limit+1` 分页技巧解决什么问题？

   答：查询时多取一条，用来判断后面是否还有下一页。返回给客户端时裁掉多余的那条，并用当前页最后一条生成 next_cursor。这样无需额外执行 count 查询，分页响应也能给出 `has_more`。

### 追问

1. `assembleFeedItems` 为什么要按原始 page item 顺序恢复结果？

   答：Feed 排序由页查询或推荐服务决定，批量读取卡片和计数后 map 的遍历顺序独立于业务顺序。`assembleFeedItems` 按 pageItems 原顺序逐个组装，保证 timeline、hot、recommend 的排序结果稳定返回。这样缓存命中和回源混合时，用户看到的顺序也保持一致。

2. Feed 卡片组装阶段如何避免 N+1 查询？

   答：先从 pageItems 提取 videoIDs，然后 Redis 使用 MGET 批量读 `video:card` 和 `video:stat`。缺失的卡片和计数分别通过 `BatchGetFeedCards`、`BatchGetFeedStats` 一次性回源 MySQL。最后把回源结果写回 Redis，后续请求可以直接命中缓存。

3. 如果视频详情缓存命中，但计数缓存缺失，系统应该怎么处理？

   答：系统会复用命中的卡片缓存，只对缺失的计数执行批量回源。计数缓存 TTL 较短，缺失是正常情况，回源后写回 `video:stat`，并保持卡片缓存的 15 分钟 TTL。这个设计让稳定字段和高频变化字段独立演进。

4. 如果 Redis 故障，timeline Feed 怎么降级？

   答：`loadFeedPage` 读取页缓存失败时会走 Repository 回源，`assembleFeedItems` 读取卡片或计数缓存失败时也会批量回源 MySQL。用户仍然能拿到 Feed，只是数据库压力和响应时间会上升。监控上会看到 cache error、Feed P95 上升和 MySQL 查询量增加。

5. `ErrLoadFeedFailed` 这类应用层错误为什么要封装底层错误？

   答：应用层错误给 Handler 稳定的响应语义，避免暴露数据库等内部细节。但需要单独处理的错误不能一律吞掉：推荐保留可用 `errors.Is` 判断的 `ErrModelUnavailable`、`ErrSnapshotMissing` 和 `ErrInvalidCursor`，分别支持 503、仅缺快照首屏的最新流回退，以及非法或跨模型版本游标的 400。

## 4. 游标分页

### 基础问题

1. timeline 游标里为什么保存 `published_at` 和 `video_id`？

   答：timeline 的排序字段是发布时间倒序，`video_id` 作为同发布时间下的次级排序字段。游标保存这两个字段后，下一页可以从当前页最后一条之后继续扫描。这样新视频插入时，分页位置依然由排序边界决定。

2. timeline 排序为什么使用 `published_at DESC, id DESC`？

   答：用户期望最新发布的视频排在前面，所以主排序是 `published_at DESC`。同一发布时间可能出现多条视频，使用 `id DESC` 作为稳定兜底排序。这个排序方式也能匹配 `status, published_at, id` 组合索引。

3. hot 游标和 timeline 游标有什么区别？

   答：timeline 游标记录时间排序边界，包含 `published_at` 和 `video_id`。hot 在 Redis 热榜窗口中使用 `window_end + offset`，因为窗口结果来自一小时 ZSET 合并后的排名页。基础仓储路径下 hot 游标也可使用 `hot_score + published_at + video_id` 维持稳定顺序。

4. 推荐流游标为什么包含模型版本、相似度排序分、发布时间和视频 ID？

   答：当前 `rank_score` 就是模型向量相似度，范围为 [-1,1]；按分数、发布时间、视频 ID 倒序建立排序。游标还保存模型文件摘要 `model_version`，换快照后拒绝旧版本游标，客户端需重拉首屏。先按排序边界截页、生成游标，最后才做页内作者打散，因此展示顺序变化不会改变续页边界。分页只覆盖经过过滤后的最多 500 条候选窗口。

5. cursor 为什么用 URL-safe base64 编码？

   答：cursor 要放在 query 参数里，URL-safe base64 可以减少特殊字符带来的转义问题。内容本质是 JSON payload，编码后对客户端透明，服务端可以按版本和字段解析。这样前端只需要原样传回 next_cursor。

### 追问

1. offset 分页在视频持续发布时会出现什么问题？

   答：offset 基于位置，列表前面插入新视频后，同一个 offset 指向的内容会变化，用户翻页容易看到重复或跳过内容。游标分页基于排序字段边界，下一页从最后一条之后继续读，稳定性更好。Feed 这种持续写入列表更适合游标分页。

2. 两个视频发布时间相同，怎么保证翻页稳定？

   答：使用视频 ID 作为次级排序字段，排序规则为 `published_at DESC, id DESC`。游标也携带 video_id，查询下一页时同时比较发布时间和 ID。这样相同发布时间的视频仍然有全序关系。

3. cursor 被客户端篡改时，接口应该返回什么错误？

   答：服务端会 base64 解码并解析 JSON，再校验时间、视频 ID、分数等字段。解析失败或字段非法时返回领域错误 `ErrInvalidCursor`，HTTP 层映射为 400。客户端可以清空 cursor 后重新请求第一页。

4. 推荐流浮点排序为什么与游标使用完全相同的比较规则？

   答：当前排序和游标都使用分数的严格大小与相等比较，只有完全相等才比较发布时间和 ID。JSON 编解码保留可回读的浮点值。若排序区分两个很接近的分数、游标却用 epsilon 把它们当同分，就可能跨页重复或漏数，因此已移除 `sameScore`。NaN、Inf 和超出 [-1,1] 的分数不进入候选。

5. 热榜 Redis 窗口分页为什么使用 `window_end + offset`？

   答：热榜窗口是按分钟截断的一小时统计结果，`window_end` 固定了这次分页使用的榜单快照。offset 表示在这个窗口中的排名位置。客户端翻页时沿用同一个 window_end，可以减少榜单刷新造成的分页漂移。

6. 读完第一页拿到 cursor 后，此时有新视频发布，翻第二页会看到这个新视频吗？

   答：不会。cursor 锁定了第一页最后一条的排序边界 `(published_at, video_id)`，第二页 SQL 条件是 `WHERE (published_at < cursor.PublishedAt OR (published_at = cursor.PublishedAt AND id < cursor.VideoID))`。新视频的 `published_at` 晚于 cursor 的时间锚点，落在游标"上方"，不在扫描范围内。这是游标分页的预期行为——翻页过程保持连续快照，不被头部插入干扰。用户回到顶部刷新（不带 cursor 重拉第一页）即可看到新视频。抖音、Twitter、微博的下拉翻页 + 顶部刷新都是这个模式。

## 5. Feed 缓存设计

### 基础问题

1. Feed 页缓存里保存哪些字段？

   答：页缓存保存 `FeedPage`，包括 scene、轻量 pageItems、next_cursor 和 has_more。pageItems 主要包含 video_id、author_id、published_at、hot_score 这类排序和组装需要的字段。视频详情和计数放在独立缓存里。

2. 为什么页缓存只保存轻量 ID 和排序字段？

   答：Feed 页的核心价值是排序结果，轻量缓存能降低 Redis 内存占用和序列化成本。视频详情变化频率低，计数变化频率高，两者适合独立缓存和独立 TTL。页缓存只保存 ID 后，详情更新和计数更新都可以单独控制。

3. `video:card` 缓存和 `video:stat` 缓存分别保存什么？

   答：`video:card` 保存视频标题、描述、媒体地址、封面、作者昵称、作者头像等相对稳定字段。`video:stat` 保存 like_count、comment_count、favorite_count 等互动计数。代码中分别通过 `GetCards/SetCards` 和 `GetStats/SetStats` 批量读写。

4. timeline 首页 TTL 和后续页 TTL 分别是多少？

   答：首页缓存 TTL 是 5 秒，后续页缓存 TTL 是 45 秒，并通过 key hash 增加抖动。首页对新内容敏感，TTL 更短；后续页访问稳定性更强，TTL 可以更长。代码常量分别是 `timelineFirstPageCacheTTL` 和 `timelinePageCacheTTL`。

5. 卡片缓存和计数缓存 TTL 为什么差异很大？

   答：卡片字段变化低频，缓存 TTL 是 15 分钟，可以显著降低详情回源。计数变化高频，展示允许短暂最终一致，TTL 是 15 秒。这个差异让读性能和数据新鲜度取得平衡。

### 追问

1. 首页缓存 5 秒为什么比后续页 45 秒短？

   答：首页承载最新内容发现，新视频发布后用户期望较快看到，因此 TTL 设置短。后续页内容天然更旧，访问频率相对低，45 秒 TTL 可以降低数据库压力。TTL 抖动进一步降低同一时间大面积失效的概率。

2. 新视频发布后，timeline 首页缓存怎么保持可接受的一致性？

   答：当前设计依靠短 TTL 和发布后的 Feed 预热来保证可接受的新鲜度。新视频写入 MySQL 后，首页缓存最多在短时间内过期并回源刷新。更高实时性场景可以在发布成功后主动删除首页页缓存或写入增量索引。

3. 为什么使用 singleflight 合并同 key 回源？

   答：缓存 miss 时，同一页可能被大量请求同时访问。singleflight 能让同 key 的并发请求共享一次回源结果，降低 MySQL 瞬时压力。GCFeed 在 `loadFeedPage` 里用缓存 key 作为 singleflight key。

4. TTL 抖动解决什么问题？

   答：大量缓存如果在同一时刻过期，会导致数据库瞬时回源压力升高。TTL 抖动让过期时间分散到不同时间点。GCFeed 通过 key hash 给页缓存 TTL 增加轻微差异。

5. Feed 缓存异常时，哪些错误可以忽略并回源 MySQL？

   答：页缓存读取错误、卡片缓存读取错误、计数缓存读取错误都可以走回源路径。写缓存失败也可以记录指标后继续返回业务结果。只要 MySQL 回源成功，Feed 主链路就能继续服务。

6. 如果热点视频计数更新很频繁，直接更新单个 Redis key 会有什么问题？

   答：所有写请求集中到同一个 key，会形成热点写压力。GCFeed 的互动计数使用基础计数加分片增量 key，按用户维度映射到 16 个分片。读取时聚合基础计数和分片增量，降低单 key 写入冲突。

7. 视频删除后，Feed 缓存里还能看到已删视频吗？怎么解决？

   答：能，但窗口被关闭了。删除是软删，只更新数据库状态，不清收件箱条目、不清卡片缓存——Feed 组装时卡片缓存命中就直接展示，所以删除后最长 15 分钟（卡片缓存 TTL）内 Feed 里还能看到已删视频，而详情页是实时 404。解决是 Redis 删除标记：删除落库成功后把视频 ID 写入标记集合（TTL 30 分钟，必须大于卡片缓存 TTL，否则标记先过期会重现窗口），Feed 组装时对所有条目批量校验，命中标记的直接剔除。标记是读侧统一过滤，时间线/热榜/关注/推荐四种 Feed 都经过同一个组装点，一处覆盖全部读取路径。两个降级：读标记失败就跳过过滤，退回旧行为，数据库状态过滤仍是兜底；写标记失败只记日志，删除照常成功。这套机制和未来的内容审核黑名单是同一个东西，做审核可以直接复用。

## 6. Hot Feed 热榜

### 基础问题

1. 点赞、收藏、评论对热度分分别贡献多少？

   答：点赞权重是 3，收藏权重是 4，评论权重是 5。取消点赞和取消收藏会写入对应负分，删除评论会扣除评论权重。权重定义在 `application/interaction/service.go` 的常量里。

2. 热榜为什么使用 Redis ZSET？

   答：ZSET 天然支持成员分数累加和按分数倒序读取。热榜需要频繁 `ZINCRBY` 更新视频热度，并通过 `ZREVRANGE` 获取排名。Redis 在这种短窗口排行榜场景下延迟低、实现简单。

3. 分钟桶 key 怎么设计？

   答：每分钟一个 ZSET key，形如 `feed:hot:minute:v1:{yyyyMMddHHmm}`。互动发生时把 video_id 作为 member，把权重增量写入当前分钟桶。分钟桶 TTL 为 2 小时，覆盖一小时窗口和延迟消费空间。

4. 一小时滑动热榜怎么生成？

   答：读取时以当前分钟作为 window_end，取最近 60 个分钟桶，通过 `ZUNIONSTORE` 聚合到临时 window key。聚合后移除分数小于等于 0 的成员，再按分数倒序分页读取。window key TTL 为 2 分钟，用于复用短时间内的相同热榜窗口。

   > **追问：60 个桶合并后排序具体怎么发生？** 写入阶段每个分钟桶内部已经是 ZSET（member=video_id, score=权重累加），`ZINCRBY` 只负责往当前分钟桶加分，不做排序。读取阶段 `ZUNIONSTORE ... AGGREGATE SUM` 把 60 个桶里相同 video_id 的 score 纵向求和，生成一个合并后的窗口 ZSET。最后 `ZREVRANGE` 按合并后的 score 从高到低取，天然就是热度排名。排序不是发生在桶之间，而是 `ZUNIONSTORE` 合并后的窗口 ZSET 上。

5. 为什么窗口 key 只保留 2 分钟？

   答：热榜强调实时性，窗口 key 缓存时间过长会让榜单更新滞后。2 分钟 TTL 可以减少频繁合并 60 个桶的成本，同时保持热榜新鲜。这个值适合 MVP，本地压测可以根据 QPS 和 Redis CPU 调整。

### 追问

1. 取消点赞和取消收藏如何影响热度分？

   答：取消点赞写入 -3，取消收藏写入 -4。这样热榜分数能反映用户当前有效互动状态，体现状态型行为变化。配合互动幂等，重复取消的 delta 为 0。

2. 为什么要移除分数小于等于 0 的热榜项？

   答：分数小于等于 0 的视频代表窗口内有效热度较低或被取消行为抵消。移除这些成员可以让热榜只展示正向热度内容，也减少后续分页和卡片组装的低价值项。代码在 rebuild window 后执行 `ZRemRangeByScore("-inf", "0")`。

3. `ZUNIONSTORE` 合并 60 个分钟桶的成本怎么评估？

   答：成本主要取决于 60 个桶的成员总数、Redis CPU 和网络延迟。可以通过 Redis 命令耗时、热榜接口 P95、window key 命中率来评估。热点较高时可以增加窗口缓存、分层榜单或后台定时构建窗口。

4. 热榜窗口 key 成为热点时怎么优化？

   答：可以增加本地短缓存、延长窗口 key TTL、按场景或地域拆 key、预计算窗口结果，也可以对热门页做 CDN/API 层缓存。读压力极高时，第一页和后续页可以分开缓存。写入侧继续使用分钟桶分散时间维度压力。

5. 如果业务希望”新视频有冷启动扶持”，热榜分数公式怎么调整？

   答：可以在热度分中加入新鲜度加权，例如按发布时间计算衰减因子，或者给新发布视频在前几小时增加启动分。也可以把完播率、播放时长、曝光点击率纳入分数。最终公式应通过监控看热榜多样性、内容质量和互动率。

## 7. 关注流分发

### 基础问题

1. 关注流为什么采用推拉结合？

   答：小作者粉丝少，发布时把视频推到粉丝 inbox 成本可控；大 V 粉丝多，同步推送会放大发布成本，所以写 Author Outbox，用户刷关注流时拉取合并。推拉结合让发布延迟和读取成本在不同作者规模下保持可控。

2. 小作者发布视频时，fanout worker 做了什么？

   答：Worker 消费视频发布事件，先预热视频卡片和计数缓存，再统计作者粉丝数。小作者路径会按 follower ID 游标分批取粉丝 ID，并把视频写入每个粉丝的 Redis inbox。inbox 使用 ZSET 保存排序分，并限制最大长度。

3. 大 V 作者为什么写 Author Outbox？

   答：大 V 粉丝量大，发布时推给所有粉丝会让一次发布变成大量写入。Author Outbox 把作者自己的公开视频按时间写入一个 outbox，关注者刷流时合并读取。这样发布接口和 Worker 成本与粉丝数解耦。

4. 用户关注新作者后，为什么需要内容回填？

   答：用户刚关注作者时，希望关注流马上出现该作者近期内容。回填会把作者最近视频写入用户 inbox，提升关注后的即时体验。项目里 `FollowFeedBackfiller` 通过 FeedRepo 读取作者近期视频，再写入 FeedCache inbox。

5. inbox 和 outbox 为什么要限制最大长度？

   答：Redis 关注索引用于近期内容读取，保留无限历史会让内存持续增长。inbox 默认保留 1000 条，outbox 默认保留 500 条，满足近期刷流需求。更久远的内容可以回源数据库或走归档查询。

### 追问

1. 发布接口为什么通过 RabbitMQ 触发 fanout？

   答：发布接口在视频事务成功后投递发布事件，fanout Worker 负责卡片/计数预热和关注流分发，避免发布响应跟粉丝数一起增长。推荐向量由独立的离线训练工具导出并在 API 启动时加载，不属于这个 Worker 的职责。

2. fanout worker 为什么按 follower ID 游标分批处理？

   答：粉丝列表可能很大，批量处理能控制单次 Redis pipeline 和数据库查询大小。游标使用最后一个 follower ID，Worker 可以稳定推进。默认 batch size 是 500，适合在吞吐和单批耗时之间平衡。

3. 大 V 作者如果同步写所有粉丝 inbox，会带来什么问题？

   答：发布耗时会随粉丝数线性增长，Redis 写入和网络 IO 会被一次发布打满。极端情况下，多个大 V 同时发布会造成队列积压和缓存集群压力。Author Outbox 把一次大写入转成读时合并，整体成本更平滑。

4. `ListFollowingIndexPage` 为什么要合并用户 inbox 和关注作者 outbox？

   答：用户 inbox 保存小作者推送内容，关注作者 outbox 保存大 V 拉取内容。关注流要同时覆盖这两类作者，所以读取时会查询 inbox key 和多个 author outbox key，合并、去重并按时间排序。这样用户只看到一个统一的关注流。

5. Worker 重复消费同一个发布事件时，如何保证结果稳定？

   答：Redis ZSET 的 member 使用 video_id、author_id 和发布时间构造，同一视频重复写入会覆盖同一 member 的分数。inbox 和 outbox 还会裁剪长度，重复消费会收敛到同一条内容。数据库侧也可以通过事件 ID 或视频 ID 幂等保护进一步增强。

6. 如果用户关注了大量大 V，读关注流时需要拉取很多 outbox，怎么优化？

   答：当前实现中 `ListFollowingIndexPage` 会把 inbox 和所有关注大 V 的 outbox 通过一次 pipeline 并行拉取，关注几十个大 V 没问题。但关注上百个大 V 时 pipeline 命令数膨胀、内存 merge 量变大，且大部分 outbox 可能没有新内容但白拉了一次。优化方向：① 首屏只拉 inbox + 最近活跃的 Top 20 大 V outbox，凑够一页即返回，后续翻页再拉更多；② 写入侧对大 V 做选择性推送——虽然不推全部粉丝，但推给互动频繁的活跃粉丝，减少读时 outbox 依赖；③ 对合并后的关注流首页做短 TTL 缓存，关注关系变化时主动失效；④ 推拉结合精细化——不做按粉丝数一刀切，而是按"作者粉丝数 × 用户活跃度"二维矩阵决定 push 还是 pull，Twitter 当年的做法就是把活跃用户走 push、不活跃用户走 pull。

## 8. 互动链路

### 基础问题

1. 点赞接口的主流程是什么？

   答：Handler 解析用户 ID、videoId 和幂等键，调用 `InteractionService.Like`。Service 走 `setAction`，在启用异步管线时先通过 Redis `SetActionState` 更新用户行为状态和实时计数，再投递 `ActionChangedEvent` 到 RabbitMQ。Worker 消费事件后把行为事实和统计计数落到 MySQL。

2. `SetActionState` 为什么先写 Redis 状态和计数？

   答：点赞收藏是高频写操作，Redis 能提供更低延迟和更高吞吐。接口可以快速返回最新 like_count 和 favorite_count，用户体验更好。MySQL 作为最终事实由 Worker 异步修正和持久化。

3. 点赞收藏事件为什么还要投递 RabbitMQ？

   答：Redis 状态适合实时读写，MySQL 需要保存长期事实。RabbitMQ 把接口链路和持久化链路解耦，Worker 可以按自身能力消费并重试。它也为消息通知、统计分析和推荐反馈扩展提供事件入口。

4. 评论为什么同步写数据库？

   答：评论是内容事实，创建后需要立即出现在评论列表，并涉及内容文本、作者、删除权限和计数。同步写库能保证评论详情和评论数一致返回。评论成功后再同步计数缓存、热榜分数和站内消息。

5. 互动成功后如何更新 hot feed？

   答：互动 Service 根据行为 delta 和权重调用 `recordHotScore`。FeedCache 实现 `AddHotScore`，把增量写入当前分钟的 Redis ZSET。Hot Feed 读取时合并最近 60 个分钟桶，形成一小时热榜。

### 追问

1. 点赞幂等键如何避免客户端重试导致重复计数？

   答：Redis action key 保存当前 status 和 idempotency_key。相同幂等键重复请求时，`SetActionState` 复用已存状态并让 delta 为 0。Worker 落库时也应依赖唯一键或幂等键，让重复事件收敛为一次计数变化。

2. 点赞状态更新的并发问题怎么解决？为什么用 Lua 脚本？

   答：`SetActionState` 把"读状态 → 算 delta → 写状态与计数"收进一个 Lua 脚本，一次 EVAL 在 Redis 内部原子执行，天然没有竞态窗口，一次往返、无需重试。之前是 WATCH + MULTI 乐观锁：MULTI 只保证排队命令原子执行，但读发生在 MULTI 之前，并发下可能读到旧状态；WATCH 补这段间隙，但冲突时 go-redis 会自动重试整个回调，重试 = 额外 2-3 次往返。演进史：WATCH → Lua（用 Lua 的缺点要主动说：脚本内所有 key 必须同一哈希槽，Redis Cluster 下 CROSSSLOT 不可用，单机 Redis 成立；脚本执行期间阻塞主线程，必须保持轻量；Lua 写死在 Go 字符串里调试难，靠真 Redis gated 集成测试兜底）。

3. 计数为什么要做分片 key？

   答：热点视频会集中收到大量点赞收藏，单个计数 key 的写压力会升高。项目把计数拆成基础计数和 16 个分片增量 key，按用户维度选择分片。读取时聚合基础计数和所有分片增量。

4. Redis 计数和 MySQL 计数出现短暂偏差时，系统怎么恢复？

   答：MySQL 保存最终事实，Worker 会把 Redis 事件异步落库并更新统计表。Redis 计数缓存 TTL 较短，过期后可以回源 MySQL 修正。评论这类同步写库路径会在成功后刷新计数缓存。

5. RabbitMQ 投递失败时，接口应该怎么处理？

   答：对已启用异步管线的点赞收藏，投递失败意味着后续持久化存在风险，接口应返回可识别错误或走同步落库兜底。GCFeed 里可以根据配置选择异步管线，工程演进时可加入本地 outbox 表保证事件最终投递。核心原则是用户看到的状态和最终事实要有补偿路径。

6. Worker ACK/NACK 的策略怎么设计？

   答：消息 JSON 解析失败属于数据格式错误，直接 NACK 丢弃或进入死信队列。业务处理失败可以 NACK 并 requeue，让 RabbitMQ 重新投递。处理成功后 ACK，指标记录 job 耗时和成功率。

## 9. 推荐流

### 基础问题

1. 推荐流的入口接口和 timeline 流有什么差异？

   答：timeline 支持匿名 `GET /api/feed-items?scene=timeline`。推荐支持 `GET /api/feed-items?scene=recommend` 和 `POST /api/feed-queries`，都要求登录用户 ID，用于选择用户向量、过滤曝光。内部候选接口是 `POST /internal/recommendation-candidates`，保留曝光决策和写入接口供内部调用。候选结果经 Feed 模块组装为前端统一条目。

2. 当前推荐为什么使用单路双塔？

   答：离线用户塔与视频塔从有效观看样本中学习匹配关系，同批次导出 128 维用户向量、视频向量及默认用户向量。在线只检索这个向量空间，避免把热门、关注内容混入候选后再用手写权重替代训练结果。hot 和 following 继续作为独立 Feed 频道。特征 hash 是两塔的输入编码，真正的向量还经过可训练投影、偏置、tanh 与归一化。

3. 推荐候选窗口和超采样如何控制？

   答：Service 每次请求固定 500 条窗口预算，召回器最多检索 `min(500×3, 模型视频数)` 条命中，为状态过滤留空间。MySQL 富化只保留已发布视频、有发布时间且作者正常的内容；随后校验分数、按模型相似度稳定排序、视频去重、过滤最近 7 天曝光，再截断到 500 条，最后应用游标和每页 limit。每页默认 10、最大 100；末页的 `has_more=false` 表示当前候选窗口已耗尽，不代表全站没有其他视频。

4. 推荐排序用了哪些分数？

   答：只有训练后用户向量与视频向量的相似度，`RankScore=Similarity`，保留负分，不是点击或完播概率。相似度相同才用发布时间、视频 ID 倒序作为稳定次级排序。热度、新鲜度字段当前不参与推荐计算；没有关注保底配额，也不做全池手工加权重排。每页最后一步保留作者打散。

5. 新用户和缺模型时分别怎么处理？

   答：模型中没有该用户 ID 时，使用同批次训练得到的 `default_user` 检索，原因标为 `cold_start`，仍属于模型推荐。模型文件不存在时，只有不带游标的推荐首屏回退最新流，返回的续页游标带 `latest.`，后续即使部署了模型也沿用时间线顺序。文件损坏、向量不合法、Milvus 未就绪或检索失败返回 503；已经开始的模型分页缺快照也返回 503。

6. 作者打散如何与分页共存？

   答：`interleaveByAuthor` 延后页内连续同作者条目。Service 先在全池相似度顺序上截取一页，用打散前最后一项编码游标，再调整这一页的展示顺序。这样作者打散不会把下一页边界挪到错误位置；同作者内容过多时仍可能相邻，不承诺严格作者配额。

### 追问

1. 双塔具体学习什么，为什么不只是固定 hash 向量？

   答：视频输入包含标题、描述的词/字符 ngram 和作者特征，以 256 桶固定 hash 表达稀疏特征。用户输入是当前训练事件之前的有效观看视频特征之和。两个塔各自学习线性投影与偏置，经过 tanh 和 L2 归一化输出 128 维向量，使用 BPR 损失同时更新两塔。无历史用户的偏置输出也参与训练，形成默认用户向量。实现使用 Python 标准库 CPU 运算，适用于小型项目基线。

2. 观看标签、去重和时间切分怎么避免未来信息泄漏？

   答：`complete`，或已完播/观看至少 3000ms 的 `play`，作为正反馈；一个用户/视频只取第一次合格事件，等权处理，后来的完播不回填早期权重。训练样本的历史只含该事件之前的正反馈，负例只从当时已发布且此前没有正反馈的视频中采样。`created_at<=cutoff` 训练，之后评估；`exposed` 和 `skip` 不作为训练正例，当前也没有把快速划走作为强负例。

3. 曝光过滤、状态过滤与详情组装分别在哪里做？

   答：召回器调用 `PublishedCandidates`，批量按视频 ID 关联 `video` 和 `account`，过滤视频状态与作者状态；没有额外的 30 天内容年龄硬限制。Service 查询 `exposures` 的最近 7 天记录并排除命中项。Feed 再通过 `assembleFeedItems` 读取详情和计数缓存，复用删除标记与批量回源逻辑。候选可能在两次查询间被删除，组装后页长可能减少，当前没有保证补满。

4. 新观看行为能马上改变用户推荐吗？新视频何时可召回？

   答：当前推荐服务加载的是离线向量快照，没有在线用户编码器或增量画像 Worker。观看事件先落库，随后导出、重新训练、生成完整快照并重启 API，才会更新个人向量；新视频也在下一批模型中进入目录。部署前已开启的模型游标带旧摘要，会被新模型拒绝，客户端需清空游标刷新。

5. 作者过于集中、结果不足时会自动补热门或关注内容吗？

   答：不会。当前只允许单路模型候选，过滤后可以返回短页或空页。先检查模型目录覆盖、样本量、状态过滤、7 天曝光窗口和超采样预算；需要改善时评估训练数据、特征或检索参数。修改曝光策略、引入探索或新召回路属于后续产品取舍，不能作为当前已实现行为。

6. Milvus 如何准备索引和处理故障？

   答：API 读取并固定完整快照，按文件 SHA-256 建独立的 `gcfeed_twotower_<摘要>` collection，后台批量导入该批次视频向量并在完成后标记可检索；失败每 5 秒重试。用户向量与索引来自同一快照。Milvus 未就绪或查询失败时返回模型不可用，不切换到热门/关注。配置中 Milvus 地址为空时，才对同一快照做本地精确检索；旧 embedding Worker 不再承担索引维护。

7. 如何实际运行训练与接口验证？

   答：从仓库根目录可执行下面的合成数据演示和回归检查。演示产物在 `/tmp`，不作为真实训练效果证据；API 定向测试包含训练产物契约、冷启动、曝光过滤、作者打散、模型切换与分页边界。

   ```bash
   python3 tools/twotower/train.py --demo --epochs 1 --k 3 --output /tmp/gcfeed-two-tower-interview.json
   python3 -m unittest discover -s tools/twotower -p 'test_*.py' -v
   (cd apps/api && go test ./internal/infra/twotower ./internal/application/recommendation ./internal/domain/recommendation ./test -run 'TestServeTrainedSnapshot|TestRejectMismatchedVectors|TestRecommend|TestRecommendation|TestCursorValid' -count=1)
   ```

8. 真实训练数据从哪里来，导出时需要注意什么？

   答：`tools/twotower/export_videos.sql` 读取当前已发布视频，`export_events.sql` 关联同一目录的 `video_view_events`，分别导出 JSONL。两个客户端连接不保证同一数据库快照，需暂停写入或使用一致备份；MySQL DATETIME 不带时区，必须核对存储时区后传 `--source-timezone`。只导出当前发布视频会有幸存者偏差，当前元数据也不是历史时点版本，不能把它称作完全无泄漏的历史线上回放。完整命令见训练目录 README。

9. 离线指标怎么解释，能证明线上提升吗？

   答：报告包括各 epoch 损失和整体、暖用户、冷用户的 Recall@K、MRR@K、NDCG@K，并与只使用训练期行为的热门基线比较。评估使用冻结用户向量、相同时间点可发布目录和已看过滤，每个评估事件一个相关目标，因此 Recall@K 也等于命中率，按事件平均。要同时看 `mean_candidates` 和样本数；没有评估目标时指标为 null。离线胜出、训练损失下降和合成数据成绩都不能证明线上观看或留存提升。

10. 模型如何上线与更新？

   答：先用留出时间段选定特征和参数，再以 `--full-train` 用全部合格行为训练服务版本；此模式没有留出评估，指标为 null。模型与指标报告各自原子写入，核对两份文件 version 一致。服务使用完整 JSON，包含用户、视频和默认用户向量，不导出在线编码器。默认放到 `apps/api/models/twotower.json`，Compose 只读挂载到 API，文件替换后重启 API；启用 Milvus 时需等待该批次 `two-tower index ready` 日志。文件摘要同时用于索引隔离和分页模型版本校验。

## 10. 曝光、播放与反馈

### 基础问题

1. 曝光日志和曝光聚合表分别记录什么？

   答：`video_view_events` 保存 `exposed/play/complete/skip` 行为流水；`exposures` 按用户/视频聚合首次与最近曝光、次数和场景。只有 `exposed` 更新曝光聚合，play/complete 用于离线训练正例筛选。HTTP 行为接口在同一事务中保存流水和必要聚合；带 request_id 时按用户、视频、场景、请求与事件类型生成唯一去重键。重复上报返回已有事实，不重复增加曝光次数。

2. 为什么推荐决策阶段要过滤近期曝光？

   答：近期重复展示同一视频会消耗体验与内容机会。当前推荐直接查询 MySQL `exposures`，过滤最近 7 天已曝光候选；内部决策 API 对命中项返回 `allowed=false` 和 `recently_exposed`，其他返回 `fresh`。它是过滤规则，不是对模型相似度做降权；数据库查询失败会报错，不能悄悄跳过过滤。

3. 播放 QoS 上报包含哪些指标？

   答：播放 QoS 可以包含首帧耗时、卡顿次数、卡顿时长、观看时长、播放完成状态、网络类型和平台信息。项目中播放模块提供配置读取、预加载建议和 QoS 上报接口。QoS 流水用于播放体验排查；当前推荐训练读取的是独立的 `video_view_events`，不会自动用 QoS 表更新用户向量。

4. 视频预加载建议如何提升连续播放体验？

   答：短视频场景中用户连续滑动，提前给出后续视频列表可以让客户端预取媒体和封面。预加载降低下一条视频首帧等待时间。服务端可以基于当前视频、scene 和用户上下文返回候选列表。

5. Range 请求对视频播放有什么作用？

   答：浏览器播放视频时会通过 Range 请求加载部分字节。支持 Range 后，用户拖动进度、断点加载和分段缓冲都更顺畅。项目里上传静态资源测试覆盖了 Range 行为。

### 追问

1. 曝光事件量很大时，写库链路怎么削峰？

   答：当前实现先同步写 MySQL 流水与曝光聚合，再尽力投递可选 MQ 事件；推荐去重和离线训练不依赖 MQ 消费成功，现有 Worker 也不消费该队列来更新画像。若要改成批量异步落库，必须另行设计可靠交付、消费幂等和曝光去重延迟；这属于演进方案，不能描述成当前已有链路。

2. 曝光去重窗口如何选择？

   答：窗口取决于内容量、用户活跃度和业务目标。内容池充足时窗口可以更长，提升多样性；内容池有限时窗口较短，保证结果填充。可以用重复曝光率、完播率和推荐填充率评估窗口。

3. 完播率、观看时长和跳出行为如何影响用户兴趣向量？

   答：端侧按真实播放进度累计有效观看，合格 play 或 complete 进入行为表；拖动、暂停、隐藏页面不当作有效观看时间。离线训练只取每个用户/视频首次合格正反馈，完整或长观看不再额外手写加权。exposed 和 skip 不作为正例，快速跳出也未实现强负反馈更新。用户向量在下一次训练与部署后更新，不会随每次事件在线重算。

4. 首帧耗时升高时，你会从哪些指标排查？

   答：先看客户端网络类型和平台，再看 API 播放配置耗时、视频文件 Range 响应耗时、Nginx 或静态文件服务吞吐。还要检查视频是否完成 faststart 处理、文件大小和编码格式。Grafana 中关注上传处理耗时、HTTP P95 和错误率。

5. MP4 faststart 为什么能优化首帧体验？

   答：MP4 faststart 会把播放所需的 moov 元数据移动到文件前部。浏览器拿到文件开头后就能更快解析并开始播放。对短视频首帧体验非常关键。

## 11. 上传与媒体处理

### 基础问题

1. 上传接口校验哪些内容？

   答：上传接口会校验文件类型、大小、扩展名、MIME、视频时长、分辨率和编码格式。不同 kind 对应视频、封面和头像等用途。校验通过后保存到 `uploads` 目录，并返回可访问 URL。

2. 文件大小、扩展名、MIME、视频时长、分辨率、编码格式分别防什么风险？

   答：文件大小控制磁盘和带宽消耗；扩展名和 MIME 降低错误文件类型进入系统的风险；视频时长和分辨率控制播放体验和处理成本；编码格式保证浏览器兼容性。组合校验能提升媒体链路稳定性。

3. 本地 `uploads` 目录和对象存储方案有什么差异？

   答：本地目录适合 MVP 和本地开发，部署简单。对象存储适合多副本和生产环境，提供更好的扩展性、持久性和 CDN 接入能力。迁移时可以把上传存储抽象成接口，Handler 只关心保存结果和 URL。

4. 视频和封面 URL 如何返回给前端？

   答：上传接口保存文件后返回 `/uploads/{kind}/{filename}` 形式的 URL。Gin 通过 `g.Static("/uploads", "./uploads")` 暴露静态文件。前端拿到 URL 后在发布视频或展示卡片时使用。

5. 静态文件服务如何支持 Range 请求？

   答：HTTP Range 需要返回 206、Content-Range 和指定字节段内容。项目通过静态文件服务支持浏览器分段请求，并有 `upload_static_range_test.go` 验证行为。这样视频播放和拖动进度更稳定。

### 追问

1. 大文件上传时，API 进程内存如何控制？

   答：上传处理应使用流式读取和大小限制，避免一次性把完整文件读入内存。Gin/HTTP 层可以设置最大 body，业务层按文件头和实际大小校验。大文件处理任务适合异步化，例如转码和 faststart。

2. 文件名如何避免冲突和路径穿越？

   答：文件名使用服务端生成的随机 ID 或哈希，并保留安全扩展名。保存路径只允许落在配置好的 uploads 子目录中，清理用户传入的路径分隔符。返回 URL 由服务端拼接。

3. 视频转码任务应该放在同步链路还是异步链路？

   答：转码通常耗时较长，适合放到异步 Worker。上传接口只负责接收原文件、基础校验和创建任务，前端通过状态查询获取处理进度。短耗时的 faststart 可以根据文件大小和接口 SLA 决定同步或异步。

4. 上传成功但发布视频失败时，残留文件怎么治理？

   答：可以记录上传文件表，标记已绑定视频或待清理状态。定时任务扫描长时间待清理的文件并删除。对象存储场景下可以配置生命周期规则辅助清理。

5. 从本地存储迁移到对象存储，代码需要调整哪些边界？

   答：抽象 Storage 接口，提供保存、读取、删除和生成访问 URL 的能力。Handler 调用接口，具体实现从本地文件切换到对象存储。静态访问路径、鉴权、CDN 缓存和回源策略也需要同步调整。

## 12. 消息中心

### 基础问题

1. 点赞、评论、关注如何生成站内消息？

   答：互动 Service 和关系 Service 注入 `MessageWriter`，业务成功后调用消息服务创建消息。消息可以携带触发用户、消息类型、标题、内容、事件 ID 和幂等键。这样点赞、评论、关注事件能统一进入消息中心。

2. 消息列表如何分页？

   答：消息列表使用游标分页，按创建时间和消息 ID 稳定排序。客户端带 cursor 和 limit 请求下一页。返回 items、next_cursor 和 has_more。

3. 消息待读计数如何维护？

   答：消息表保存读状态，待读计数可以通过查询待读消息数量获得，也可以在规模变大后维护冗余计数。项目提供 `/api/message-stats/unread` 返回待读计数。写入消息和标记已读时需要保证状态更新清晰。

4. 消息写入为什么需要幂等？

   答：点赞、评论、关注事件可能因为重试或 MQ 重复消费触发多次。消息写入使用 event_id 或 idempotency_key 可以保证同一业务事件只生成一条消息。用户侧体验更稳定。

5. 消息事件 ID 的作用是什么？

   答：事件 ID 用来标识一条业务事件，例如某次点赞或评论。消息中心可以基于事件 ID 去重，也可以用于排查从业务事件到站内消息的链路。它还适合做日志关联。

### 追问

1. 如果点赞事件重复消费，如何避免重复消息？

   答：消息表对事件 ID 或幂等键建立唯一约束。重复消费时写入命中已存在记录，服务返回已有结果或忽略重复写。这样 MQ 至少一次投递语义下也能保证消息事实稳定。

2. 消息读状态更新如何保证并发安全？

   答：标记已读可以按用户 ID 和消息 ID 条件更新，只影响当前用户自己的消息。重复标记已读是幂等操作，结果保持已读状态。批量更新时需要限制用户范围，防止越权修改。

3. 消息待读计数用实时查询还是冗余计数？你会怎么选？

   答：MVP 使用实时查询实现简单，消息量大后使用冗余计数提高读取性能。冗余计数需要在消息创建和已读更新时同步维护，并提供定期校准任务。选择依据是待读计数接口 QPS 和消息表规模。

4. 消息中心和互动服务之间如何降低耦合？

   答：互动服务依赖 `MessageWriter` 小接口，只知道创建消息能力。具体消息服务实现和数据库表结构放在 message 模块。后续可以把消息写入改为事件驱动，互动服务只发布事件。

5. 大量历史消息如何归档？

   答：按时间分区或按月份归档历史消息，在线表保留近期数据。归档数据可以放入冷存储或独立历史表。消息列表默认读取近期在线数据，历史查询走专门入口。

## 13. 账号与鉴权

### 基础问题

1. 用户注册、登录和获取当前用户信息的接口是什么？

   答：注册是 `POST /api/users`，登录是 `POST /api/sessions`，获取当前用户是 `GET /api/users/me`。登录成功后返回 access_token，客户端通过 Authorization Bearer 携带；同时通过 httpOnly Cookie 下发 refresh token。更新当前用户资料使用 `PATCH /api/users/me`。

2. JWT 中间件如何把用户身份传给 Handler？

   答：JWT 中间件解析 Authorization header，校验 token 签名和过期时间。解析成功后把 userID、角色等信息写入 Gin context。Handler 从 context 读取身份信息，再调用对应 Service。

3. 可选登录态 Feed 解决什么场景？

   答：游客也可以访问 timeline 或 hot Feed，登录用户可以获得个性化状态和推荐能力。可选 JWT 中间件在 token 存在时解析身份，缺省时继续作为游客请求。这样一个接口同时支持公开 Feed 和登录增强体验。

4. 内部接口为什么使用 `X-Internal-Token`？

   答：`X-Internal-Token` 用于服务间调用，当前内部消息与内部 QoS 路由挂载了该校验。推荐候选、曝光决策和曝光写入三个内部路由目前没有挂载这个中间件，因此不能把所有 `/internal/*` 都描述为已受内部 token 保护。部署时需按实际路由核对访问边界，用户 Feed 推荐入口仍要求 JWT 身份。

5. 用户角色在删除评论或治理场景中有什么作用？

   答：普通用户可以删除自己的评论，管理员或审核角色可以处理违规评论。Service 根据 userID 和 role 判断操作权限。这样内容治理能力可以在现有评论模块上扩展。

### 追问

1. JWT 泄露后如何降低风险？

   答：access token 设置较短有效期（本项目 15 分钟），敏感操作增加二次校验。本项目已落地双 token 方案：refresh token 存服务端会话表（只存 SHA-256 哈希），登出/风控时撤销数据库行即可拒绝续签；每次刷新强制轮换，旧 token 立即失效，已轮换的旧 token 再次出现会被判定重用并整族撤销。HTTPS、HttpOnly cookie 和设备管理也能降低泄露风险。

2. Access token 和 refresh token 如何设计？

   答：本项目已实现：access token 是 15 分钟 HS256 JWT（无状态，业务接口直接用）；refresh token 是 32 字节随机串（opaque，不是 JWT），数据库只存 SHA-256 哈希，属于 `refresh_token` 表。每个 refresh token 属于一个 family（登录创建），刷新走 `POST /api/sessions/refresh`：服务端按哈希查库校验未撤销未过期，然后 CAS 事务轮换——旧行写 revoked_at + 插入同 family 新行；CAS 失败说明被并发刷新过，按重用处理并整族撤销（防止双 token 双活）。refresh token 通过 httpOnly + SameSite=Lax Cookie 下发，前端 JS 读不到。服务端拒绝续签（撤销行）即实现踢设备下线。登出 `DELETE /api/sessions/current` 只凭 Cookie 撤销当前会话，不依赖 access token。

   诚实边界：① 登出/撤族后旧 access token 在剩余 15 分钟内仍可用（无状态 JWT 固有，要立即吊销需引入 access 黑名单）；② 多标签页并发刷新会误杀整个 family，用户需重新登录（严格单活语义，与 Auth0 一致）；③ Cookie Secure 属性生产环境必须开启，本地 http 开发环境不设，否则浏览器拒绝存储。

3. 内部 token 如何轮换？

   答：配置中心或密钥管理系统同时支持当前 token 和过渡 token。服务发布时先让服务端接受双 token，再切换调用方配置，最后移除旧 token。监控内部接口鉴权失败率确认轮换过程。

4. 游客和登录用户看到的 Feed 有哪些差异？

   答：游客可以看公开 timeline 和 hot 内容。登录用户可以访问 following、recommend，并能获得点赞收藏状态、曝光去重和个性化排序。登录态还支持播放 QoS、消息和关系链功能。

5. Handler 中如何区分 401 和 403？

   答：401 表示身份缺失或 token 校验失败，用户需要重新登录。前端收到 401 会先无感调一次 refresh 接口（凭 httpOnly Cookie）换新 access token 并重试原请求，刷新也失败才清登录态跳登录页；sessions 接口自身不做重试，避免刷新循环。403 表示身份有效，当前用户权限等级低于目标操作要求，例如删除他人评论或访问内部资源。Handler 根据中间件结果和领域权限错误映射状态码。

## 14. 数据库与索引

### 基础问题

1. `video`、`video_stat`、`account` 的核心关系是什么？

   答：`account` 表保存用户资料和身份信息，`video` 表保存视频主体并通过 author_id 关联作者，`video_stat` 表按 video_id 保存点赞、评论、收藏等统计。一个用户可以发布多个视频，一个视频对应一条统计记录。Feed 组装时需要同时读取视频、作者和统计。

2. timeline 查询需要什么索引？

   答：timeline 按状态过滤，并按发布时间和 ID 倒序分页，适合建立 `idx_video_timeline(status, published_at DESC, id DESC)`。查询公开视频时可以先定位 status，再按排序字段扫描。项目迁移里通过 `infrafeed.EnsureTimelineIndex` 创建这个索引。

3. 互动事实表和统计表为什么分开？

   答：事实表保存用户点赞、收藏、评论等明细；`video_stat` 保存 Feed 展示和独立 hot 频道所需的聚合计数。当前模型推荐不把这些计数加入排序公式，训练读取观看行为流水。两类数据分开，便于事务维护统计和追溯行为。

4. 曝光日志表和曝光聚合表为什么分开？

   答：曝光日志保存每次事件，适合分析和回放。曝光聚合表保存用户-视频维度的最近曝光状态，适合推荐去重快速查询。读写目的差异明显，分表可以优化各自索引和生命周期。

5. GORM 自动迁移在本地开发中的作用是什么？

   答：自动迁移能根据 model 创建和补齐表结构，降低本地启动成本。项目启动时调用 `migration.AutoMigrate`，同时创建视频统计和 timeline 索引。它适合教学项目和开发环境快速迭代。

### 追问

1. `idx_video_timeline(status, published_at DESC, id DESC)` 为什么适合 timeline？

   答：timeline 查询先过滤公开视频状态，再按发布时间和 ID 倒序返回。这个组合索引的字段顺序匹配 where 和 order by，能减少排序成本和扫描范围。游标条件也能沿用 published_at 和 id 继续扫描。

2. 高频计数直接更新 MySQL 会有什么瓶颈？

   答：热点视频的点赞收藏会集中更新同一行 `video_stat`，造成行锁竞争和写放大。MySQL 写入延迟上升后会影响接口响应。项目用 Redis 实时计数和 RabbitMQ 异步落库来削峰。

3. 幂等键应该建什么唯一索引？

   答：幂等键应和业务主体一起建唯一约束，例如视频发布可用 `author_id + idempotency_key`，评论可用 `user_id + idempotency_key`，消息可用 `event_id` 或 `user_id + idempotency_key`。这样同一用户重复请求返回同一结果，其他用户使用相同字符串也能独立生效。

4. 评论列表按 `created_at DESC, id DESC` 翻页，需要什么索引？

   答：适合建立 `video_id, status, created_at DESC, id DESC` 这类组合索引。先按 video_id 和状态定位评论集合，再按创建时间和 ID 倒序扫描。游标字段与索引排序一致，翻页效率更稳定。

5. 自动迁移在生产环境有哪些风险？

   答：生产环境表规模大，自动变更字段和索引可能造成锁表、耗时过长或变更不可控。更稳妥的方式是使用显式 migration 脚本、灰度执行和回滚方案。GCFeed 当前自动迁移更适合本地和演示环境。

## 15. RabbitMQ 与 Worker

### 基础问题

1. 项目里哪些链路使用 RabbitMQ？

   答：现有 Worker 注册点赞/收藏 ActionWorker 和视频发布 FanoutWorker，负责互动落库、卡片/计数预热及关注流分发。观看事件在同步落库后仍可发到 `gcfeed.exposure`，但没有注册消费它的用户画像 Worker。旧 embedding 队列的配置或兼容接口不是正在运行的推荐任务；双塔训练由 `tools/twotower/train.py` 离线执行，索引由 API 启动后的准备流程导入。

2. 持久化消息解决什么问题？

   答：持久化消息在 RabbitMQ 重启或异常恢复时保留更高可靠性。项目发布消息时设置 `DeliveryMode: amqp.Persistent`，并配置交换机、队列和绑定。这样异步任务在基础设施波动下更容易恢复。

3. 显式 ACK/NACK 的作用是什么？

   答：显式 ACK 表示 Worker 处理成功，RabbitMQ 可以确认删除消息。NACK 表示处理失败，RabbitMQ 可以重新入队或丢弃。项目里 JSON 解析失败会 NACK 丢弃，业务处理失败会 NACK 并重新入队。

4. Worker 处理失败时如何重试？

   答：消费 handler 返回错误后，RabbitMQ 实现会 `Nack(false, true)`，消息重新入队等待再次消费。指标会记录 Worker job 的失败和耗时。更完整的生产方案可以增加最大重试次数、死信队列和延迟重试。

5. API 服务和 Worker 服务为什么分开启动？

   答：API 面向在线请求，关注低延迟；Worker 面向异步任务，关注吞吐和重试。分开启动后可以独立扩容、独立发布和独立监控。项目有 `cmd/feed/main.go` 和 `cmd/worker/main.go` 两个入口。

### 追问

1. 重复消费如何保证业务幂等？

   答：业务表使用唯一键和幂等键保护，例如 action 事实、message event_id、video idempotency key。Redis ZSET member 重复写也会覆盖同一成员。Worker 处理逻辑要以事件 ID 或业务唯一键作为去重边界。

2. 消息堆积时怎么定位瓶颈？

   答：先看 RabbitMQ 队列长度、入队速率和消费速率，再看 Worker job P95、错误率和下游 MySQL/Redis 指标。若消费慢，增加 Worker 副本或优化批处理；若失败多，分析错误类型和重试风暴。队列堆积还要检查是否存在单条毒消息反复重试。

3. 死信队列适合处理哪些失败？

   答：死信队列适合处理格式错误、超过最大重试次数、依赖资源长期失败和业务数据异常的消息。进入死信后由人工或补偿任务分析。这样可以防止同一批失败消息持续挤占正常消费能力。

4. 顺序性在点赞、评论、发布事件中是否重要？

   答：点赞/取消等反向操作需要保证较新的用户意图不被旧事件覆盖；幂等只能处理重复，不能替代顺序控制。评论展示按时间与 ID 排序，视频发布事件用于 fanout 和预热；推荐训练会独立按事件时间构造历史，用户与视频向量同批次切换。

5. Worker 横向扩容后会产生哪些并发问题？

   答：多个 Worker 可能同时处理同一业务对象，造成重复写入、计数竞争或缓存覆盖。解决方式是唯一键、幂等键、Redis 原子操作和数据库事务。还要关注 RabbitMQ prefetch、下游连接池和热点数据锁竞争。

## 16. 监控与压测

### 基础问题

1. 项目暴露了哪些 Prometheus 指标？

   答：项目暴露 HTTP 请求数和耗时、Feed 请求数和耗时、Feed 返回 item 数、Feed 缓存读写命中、上传请求和耗时、视频处理耗时、Worker job 数和耗时。指标命名空间是 `gcfeed`。`/metrics` 通过 Prometheus handler 暴露。

2. Grafana 面板重点看哪些指标？

   答：重点看 API QPS、5xx 错误率、API P95、Feed P95、Feed 缓存命中率、上传处理耗时和 Worker 成功率。Feed 链路关注 scene 维度，Worker 链路关注 job 维度。压测时同时看 Redis、MySQL 和 RabbitMQ 状态。

3. k6 压测 Feed 时关注哪些结果？

   答：关注 QPS、`http_req_duration p(95)`、`http_req_failed` 和业务 check。推荐压测还需登录态、实际可用的模型快照及预期候选，核对返回是否确实来自模型；缺文件时推荐首屏会成功返回最新流，仅 HTTP 200 或非空 items 不能证明个性化链路正常。推荐质量另看时间留出集的召回指标，不与接口吞吐混为一谈。

4. QPS、平均延迟、P95、错误率分别说明什么？

   答：QPS 表示单位时间吞吐，平均延迟表示总体响应水平，P95 表示大多数用户体验上限，错误率表示稳定性。Feed 系统更重视 P95 和错误率，因为刷流体验对尾延迟敏感。压测结果要结合机器配置和数据量说明。

5. Worker 成功率和队列积压为什么重要？

   答：当前 Worker 承担互动落库、Feed fanout 和预热，失败或积压会延后计数持久化与关注流分发。应用指标提供 job 耗时与成功/失败计数，队列长度要另外从 RabbitMQ 管理台或 exporter 获取。双塔离线训练与 API 索引准备不由这些 Worker 执行，不能用 Worker 成功率代替模型就绪、目录覆盖或推荐质量检查。

### 追问

1. Feed P95 升高时，你会按什么顺序排查？

   答：先看是否某个 scene 单独升高，再看缓存命中率和 cache error。然后看 MySQL 慢查询、Redis 命令耗时、API CPU 和连接池。最后结合最近发布、热点内容和 MQ 积压判断是否有突发写入影响读链路。

2. 缓存命中率下降时，可能有哪些原因？

   答：TTL 设置过短、key 设计变动、请求 limit 或 cursor 分布过散、Redis 重启、缓存写入失败都会降低命中率。新发布高峰也会让首页缓存频繁过期。需要按 page、card、stat 三个 area 分开看。

3. `http_req_failed` 为 0，但业务成功率下降，说明什么？

   答：HTTP 层返回了 200，但响应内容偏离业务检查，例如 items 字段缺失、数据为空或 scene 结果异常。k6 的业务 check 可以捕捉这种问题。排查时看 handler 响应结构、Service 错误映射和测试断言。

4. 如何区分 API 慢、MySQL 慢、Redis 慢和 MQ 积压？

   答：API 慢看 HTTP duration 和 CPU；MySQL 慢看慢查询、连接池和回源次数；Redis 慢看 cache read/write error 和命令耗时；MQ 积压看队列长度和 Worker duration。用请求链路指标和日志 request_id 串起来定位瓶颈。

5. 本地压测结果写进简历时，怎么表述才可信？

   答：写清环境、并发、持续时间、接口、数据量和指标，例如 20 VU、60s、timeline Feed、QPS、P95、错误率。强调“本地环境压测”或“Docker Compose 环境压测”。同时说明优化方法，例如页缓存、批量聚合和 singleflight。

## 17. 测试设计

### 基础问题

1. 项目里有哪些 API 流程测试？

   答：API 流程覆盖账号、Feed、推荐、曝光、互动、消息、关系、播放、上传和视频；另有 fanout、缓存、媒体 Range 等测试。当前推荐验证包括 CPU 训练与导出、Go 读取训练产物的契约、单路相似度排序、曝光与状态过滤、作者打散和版本游标；仓库保留的 hash ngram 或 embedding 单元测试不代表生产推荐仍使用那条链路。

2. Feed API 测试应该覆盖哪些场景？

   答：覆盖 timeline 首页、下一页 cursor、limit 裁剪、非法 cursor、hot scene、following 登录要求、recommend 登录要求和缓存缺失回源。还要验证返回 items、next_cursor、has_more 和排序稳定。边界场景包括空列表和同发布时间视频。

3. 推荐 API 测试应该覆盖哪些场景？

   答：覆盖向量单路来源、分数原样保留与负相似度、无热门/关注混入、曝光去重、非法候选、作者打散前生成游标、不同页大小与极接近分数的稳定分页、过滤后 500 条窗口边界、冷启动默认用户、模型版本切换及错误透传。快照缺失首屏与续页、文件损坏和 Milvus 未就绪应验证不同响应；带 request_id 的播放事件重试验证去重。MySQL/Milvus 真实集成测试需要相应环境变量，缺配置时跳过，不能把 mock 测试说成真实服务验证。

4. 上传 Range 测试验证什么？

   答：验证静态视频资源对 Range 请求返回 206、正确 Content-Range 和指定字节内容。还要验证普通 GET 能返回完整文件。这个测试保证浏览器播放和拖动进度可以正常工作。

5. Worker 测试如何验证重复消费？

   答：用同一个事件调用 Worker handler 多次，检查 Redis ZSET、inbox/outbox 或数据库事实只保留稳定结果。fanout worker 可以验证重复发布事件收敛为同一条 Feed 项。互动 worker 可以验证计数和 action 事实保持一致。

### 追问

1. 缓存命中和缓存缺失如何分别测试？

   答：命中测试先写入缓存，再调用 Service，断言 Repository 回源次数为 0 或减少。缺失测试清空缓存，调用 Service 后断言 Repository 被调用，并检查缓存被写回。可以用 mock cache 和 mock repo 精确统计调用次数。

2. 游标分页如何测试重复和漏数？

   答：构造多条同发布时间和不同发布时间的数据，按 limit 连续翻页，收集所有 video_id。断言每个 video_id 只出现一次，并且合并后的结果等于预期排序列表。还要插入新视频后验证旧 cursor 仍能从原边界继续。

3. 幂等接口如何测试重复请求返回稳定结果？

   答：用相同用户、相同业务参数和相同幂等键请求两次。断言返回同一业务结果，数据库只产生一条事实记录，计数只变化一次。再用不同幂等键请求，断言能产生新的业务动作。

4. RabbitMQ 相关逻辑如何用接口 mock？

   答：异步互动和 fanout 通过小接口替换消息实现，测试可记录事件或手动调用消费 Handler。推荐测试直接注入 `CandidateRecaller.Recall(ctx,userID,limit)`，返回训练后分数的固定候选，不再 mock 三路合并；真实训练产物契约单独运行 Python 到 Go 的测试，避免用同一份手写公式既生成期望值又验证实现。

5. 压测和单元测试分别解决什么问题？

   答：单元测试和 API 流程测试验证功能正确性、边界和幂等。压测验证吞吐、延迟、错误率和缓存效果。两者结合才能说明系统既能正确工作，也能在一定压力下稳定运行。

## 18. 部署与工程化

### 基础问题

1. Docker Compose 启动了哪些服务？

   答：Compose 启动 API、Worker、Web、MySQL、Redis、RabbitMQ、Milvus（含 etcd 和 MinIO 两个依赖）、Prometheus 和 Grafana。Web 通过 Vite/Nginx 提供前端，API 暴露业务接口和指标，Worker 消费异步任务。MySQL、Redis 和 RabbitMQ 提供核心基础设施，Milvus 提供视频向量检索。

2. API、Worker、Web、MySQL、Redis、RabbitMQ 在部署中分别承担什么？

   答：API 提供 HTTP 业务并在启动时加载双塔快照、按需准备 Milvus 索引；Worker 执行互动落库与视频 fanout/预热。Web 提供刷流和观看行为上报，MySQL 保存事实与曝光索引，Redis 提供缓存、计数、热榜和关注索引，RabbitMQ 连接现有异步任务。离线训练工具独立运行，不在发布视频或播放事件 Worker 内训练。

3. Kubernetes 清单里需要哪些 Deployment、Service 和 PVC？

   答：API、Worker、Web 适合 Deployment，分别配置 Service 暴露访问或内部调用。MySQL、Redis、RabbitMQ 需要持久化存储，使用 PVC 保存数据。还需要 ConfigMap、Secret、健康检查和资源限制。

4. 健康检查接口有什么作用？

   答：`/health` 用于容器探活、本地调试和负载均衡健康判断。Kubernetes 可以用它做 readiness 和 liveness probe。健康检查保持轻量，返回服务基础可用状态。

5. Web 生产构建如何验证？

   答：运行 `npm --prefix apps/web run build` 生成生产产物。验证构建成功后，再通过 Docker Compose 或 Nginx 容器访问 Web 页面。还应检查 API 地址配置、静态资源加载和主要页面交互。

### 追问

1. API 多副本部署后，Redis singleflight 还能跨实例生效吗？

   答：Go 的 singleflight 是进程内合并，只能合并同一 API 实例内的并发回源。多副本时跨实例仍可能同时回源。进一步优化可以使用 Redis 分布式锁、热点页预热或更长窗口缓存。

2. 本地 uploads 目录在多副本部署下有什么问题？

   答：每个 API 副本的本地磁盘独立，文件可能只存在某一个 Pod 上。负载均衡到其他副本时会访问失败。生产环境适合使用对象存储或共享存储，并通过 CDN 分发媒体。

3. MySQL、Redis、RabbitMQ 的数据卷怎么规划？

   答：MySQL 需要可靠 PVC 和备份策略，Redis 根据持久化需求配置 AOF/RDB 和容量，RabbitMQ 需要保存队列元数据和持久化消息。开发环境可以使用简单 volume，生产环境要考虑备份、恢复和扩容。监控磁盘使用率和 IO 延迟很重要。

4. 配置文件和密钥如何区分管理？

   答：普通配置放 ConfigMap 或配置文件，例如端口、Redis 地址、功能开关。密钥放 Secret，例如 JWT secret、数据库密码、RabbitMQ 密码、内部 token。应用启动时从环境变量或挂载文件读取。

5. 灰度发布时如何保证新旧版本兼容？

   答：接口响应保持向后兼容，新增字段采用可选字段。数据库变更先加字段和索引，再发布读写新字段的代码，最后清理旧字段。消息事件也要保留版本字段，Worker 能处理新旧格式。

## 19. 大厂高频综合追问

1. 你在这个项目里做过最复杂的技术决策是什么？

   答：最复杂的是 Feed 主链路的性能设计：页缓存只保存排序结果，卡片和计数独立缓存，读取时批量聚合并恢复原顺序。这个设计同时处理性能、缓存一致性和业务扩展。它也支撑 timeline、hot、following、recommend 多种 Feed 复用同一响应组装链路。

2. 这个系统如何支撑千万级用户刷 Feed？

   答：核心是把高频读链路缓存化、批量化和分层化。timeline 首页走短 TTL 热缓存，视频卡片和计数独立缓存，热门 Feed 使用 Redis 窗口榜单，关注流使用推拉结合，推荐服务做候选和排序。进一步演进需要多级缓存、分库分表、读写分离、CDN、异步事件管道和容量治理。

3. 如果 Feed 首页突然被打爆，你的应急方案是什么？

   答：先保护 MySQL，增加首页缓存 TTL、启用热点页预热和限流，必要时返回降级缓存。观察 Feed P95、cache hit、MySQL QPS 和 Redis CPU。中长期优化包括跨实例锁、静态热门页、缓存副本和请求合并。

4. 如果 Redis 故障 10 分钟，核心功能如何降级？

   答：timeline 和视频卡片缓存可以回源 MySQL，hot 与 following 的具体行为需看对应索引错误处理；互动支持同步落库兜底。推荐向量来自模型快照或 Milvus，近期曝光过滤读 MySQL，不会因为 Redis 故障改用热门候选。卡片组装会增加数据库压力；模型自身不可用返回 503，只有缺模型文件的首屏允许最新流回退。

5. 如果 RabbitMQ 堆积 100 万条消息，你怎么处理？

   答：先确认堆积队列、入队速率、消费速率和错误类型。增加 Worker 副本、提高批处理效率，修复反复失败的消息并隔离毒消息。业务侧按优先级消费关键队列，例如互动落库优先于低优先级分析任务。

6. 如果 MySQL 主库 CPU 打满，你怎么定位和止血？

   答：先看慢查询、连接数、QPS、锁等待和当前最重 SQL。止血动作包括提高缓存 TTL、关闭低优先级回源、限流热点接口、暂停部分 Worker 写入和扩容只读能力。根因可能是缓存失效、索引缺失、N+1 回源或批量任务冲击。

7. 如果推荐结果重复率很高，你从哪些链路排查？

   答：先核对端侧有效曝光上报、request_id、行为接口响应与 `exposures` 最近曝光记录，再看推荐是否实际运行了缺快照最新流回退。模型正常时检查过滤后的 500 条候选覆盖、默认用户使用情况、模型目录是否过旧、状态过滤和作者集中度，最后验证精确分数排序、版本游标与打散边界。这些检查分别对应当前单路模型、MySQL 曝光和版本快照的实际链路。

8. 如果点赞数短时间显示错误，你怎么解释最终一致性？

   答：点赞接口先更新 Redis 实时状态和计数，MySQL 由 Worker 异步落库。短时间内 Redis、Feed 计数缓存和 MySQL 可能存在延迟差异，TTL 过期和 Worker 落库后会修正。核心要保证幂等和最终事实一致。

9. 如果老板要求热榜更实时，你怎么改架构？

   答：缩短窗口 key TTL，增加后台实时聚合任务，或在互动写入时同时更新当前窗口榜单。还可以把热榜计算拆成流式处理，按秒级窗口维护 TopN。需要同时关注 Redis CPU、榜单抖动和内容质量。

10. 如果要把这个项目讲成简历亮点，你会突出哪些指标和取舍？

   答：突出 Feed 读取性能优化：游标分页、页缓存、ID-Detail 分离、批量聚合、singleflight 和 TTL 抖动。突出异步解耦：RabbitMQ 持久化消息、ACK/NACK、互动异步落库和发布 fanout。指标上写清压测环境、QPS、P95、错误率和缓存命中率。

## 20. 建议练习顺序

1. 先练项目总览、后端分层、Feed 读取链路。
2. 再练游标分页、Feed 缓存、Hot Feed。
3. 然后练关注流分发、互动链路、RabbitMQ Worker。
4. 最后练推荐流、监控压测、部署扩展和故障处理。

每个问题都尽量按“现有实现、为什么这样设计、极端情况下怎么演进”三段回答。
