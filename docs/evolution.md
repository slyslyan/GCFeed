# 微服务演进方向（规划）

当前 GCFeed 是 DDD 四层单体（一个 Go 二进制服务所有 API 路由），通过 RabbitMQ 解耦异步事件。本文记录未来拆分为独立微服务时的架构设想，不作为近期开发计划。

## 1. 拆分动机

单体在 GCFeed 当前规模下完全够用。以下场景出现时才值得拆：

| 触发条件 | 说明 |
|---|---|
| **推荐模型变重** | 推荐服务需要独立扩缩容（GPU/CPU 需求不同） |
| **Feed RT 要求与后台任务冲突** | 运营批量操作导致 DB 连接池被打满，影响用户 Feed 请求 |
| **独立迭代速度需求** | 推荐团队和 Feed 团队需要独立的发布节奏 |
| **技术栈分化** | 推荐模块采用 Python/ML 栈，与 Go 主服务不同 |

## 2. 服务边界划分

按 bounded context 拆分为四个服务：

```
┌──────────────────────────────────────────────────┐
│                   API Gateway                     │
│        (路由分发 / 认证 / 限流 / 协议转换)          │
└────┬─────────┬──────────┬──────────┬─────────────┘
     │         │          │          │
     ▼         ▼          ▼          ▼
┌─────────┐ ┌────────┐ ┌────────┐ ┌──────────────┐
│  Feed   │ │Recommend│ │Interact│ │  Account     │
│ Service │ │ Service │ │Service │ │  Service     │
├─────────┤ ├────────┤ ├────────┤ ├──────────────┤
│ • Feed  │ │ • Recall│ │ • Like │ │ • Register   │
│  read   │ │ • Rank  │ │ • Fav  │ │ • Login      │
│ • Fan-  │ │ • E-   │ │ • Com- │ │ • Profile    │
│  out    │ │  xpose  │ │  ment  │ │ • Follow     │
│ • Cache │ │ • Can-  │ │ • Msg  │ │ • Token      │
│         │ │  didate │ │        │ │              │
└─────────┘ └────────┘ └────────┘ └──────────────┘
     │         │          │
     └─────────┴──────────┘
               │
               ▼
     ┌─────────────────┐
     │  Video Service  │
     │  (视频元数据)     │
     └─────────────────┘
```

### 每个服务的职责与数据主权

| 服务 | 拥有表 | 暴露接口 | 备注 |
|---|---|---|---|
| **Account Service** | `account` | 注册/登录/用户资料/关注关系 | 可直接暴露 JWT |
| **Video Service** | `video`, `video_stat` | 视频 CRUD、媒体元数据 | 纯数据服务，无业务逻辑 |
| **Feed Service** | 无（只读 video） | Feed 列表（timeline/hot/following） | 读缓存为主，数据从 Video Service 来 |
| **Recommend Service** | `exposure`, `recommendation` | 召回、排序、曝光控制 | 可独立使用 Python/ML |
| **Interaction Service** | `interaction`, `comment` | 点赞/收藏/评论 | 事件驱动写入，RPC 读取 |
| **Message Service** | `message` | 站内信通知 | 纯事件驱动 + 查询 |
| **Playback Service** | `playback_config`, `qos_report` | 播放配置、QoS上报 | |

### 数据所有权原则

- 每个服务独享自己的数据库（或 schema），**不允许跨服务直接查询 DB**
- 跨服务的数据需求通过 RPC 调用或事件订阅解决
- 例：Feed Service 需要 video 标题 → 调用 Video Service 的 `BatchGetVideos` RPC，而非直连 Video 的 DB

## 3. 服务间通信协议

### 3.1 同步调用：gRPC

Feed 请求的典型链路（最重的同步路径）：

```
Client
  │
  ▼
Feed Service
  │
  ├── gRPC ──► Recommend Service ── "ListCandidates(user_id, scene, cursor)"
  │              返回候选视频 ID + rank score 列表
  │
  ├── gRPC ──► Video Service ── "BatchGetVideos(video_ids)"
  │              返回视频标题、封面、作者等元数据
  │
  ├── gRPC ──► Interaction Service ── "BatchGetUserActions(user_id, video_ids)"
  │              返回当前用户是否点赞/收藏（个性化标记）
  │
  └── 组装 FeedItem 列表 → 返回 Client
```

选择 gRPC 的理由：

| 场景 | gRPC 收益 |
|---|---|
| **Feed 链路过长（3 次串行 RPC）** | Protobuf 序列化减少字节，降低多次 RPC 的累积开销 |
| **召回排序需要流式返回** | `ListCandidates` 可改造为 server-side streaming，候选视频逐个推 |
| **跨团队接口契约** | proto 文件是双方签字的接口定义，避免 JSON 字段名/类型隐式约定 |
| **多语言推荐服务** | Recommend Service 可用 Python，gRPC 跨语言代码生成成熟 |

proto 定义示例：

```protobuf
// 每个服务独立的 proto 包，由对应团队维护
service FeedService {
  rpc ListFeed(ListFeedRequest) returns (ListFeedResponse);
}

service RecommendService {
  rpc ListCandidates(CandidateRequest) returns (CandidateResponse);
  rpc DecideExposures(ExposureDecisionRequest) returns (ExposureDecisionResponse);
}

service VideoService {
  rpc BatchGetVideos(BatchGetVideosRequest) returns (BatchGetVideosResponse);
  rpc BatchGetStats(BatchGetStatsRequest) returns (BatchGetStatsResponse);
}

service InteractionService {
  rpc BatchGetUserActions(BatchGetUserActionsRequest) returns (BatchGetUserActionsResponse);
  rpc Like(LikeRequest) returns (LikeResponse);
  rpc CreateComment(CreateCommentRequest) returns (CreateCommentResponse);
}
```

### 3.2 异步事件：RabbitMQ（保留不变）

现有的事件驱动路径保留，无需改为 gRPC：

```
Video Published ──► RabbitMQ ──► FanoutWorker (feed)
                              ──► EmbeddingWorker (recommend)

Action Changed ──► RabbitMQ ──► ActionWorker (interaction → DB 落库)

View Event Recorded ──► RabbitMQ ──► (推荐画像更新)
```

事件传递适合最终一致性场景：写入后可接受秒级延迟，不需要同步等待。

### 3.3 数据同步：CDC / 事件订阅

部分服务（如 Feed Service）需要读 Video 的元数据，但不应直连 Video Service 的 DB。方案：

```
Video Service DB
  │
  └── CDC (Debezium / Binlog) ──► Kafka / RabbitMQ ──► Feed Service 本地缓存
```

在 GCFeed 规模下，更现实的方案是 **Video Service 在视频变更后发事件**，其他服务自行维护本地只读副本：

```
Video Service 创建视频
  │
  ├── gRPC 同步写入自身 DB
  └── RabbitMQ ── "VideoUpdated" ──► Feed Service 更新本地只读缓存
                                    ──► Recommend Service 更新索引
```

## 4. 调用链路对比

### 4.1 单体现状

```
Feed 一次请求 = 1 次 TCP 连接（Client → API）
              + 1 次 DB 查询（timeline 页）
              + 1 次 Redis MGET（卡片缓存）
              + n 次回源 DB（缓存未命中）
```

调用路径都在进程内，延迟低，但不可独立扩缩容。

### 4.2 微服务后

```
Feed 一次请求 = 1 次 TCP 连接（Client → Feed Service）
              + 1 次 gRPC（Feed → Recommend, <5ms）
              + 1 次 gRPC（Feed → Video, <5ms）
              + 1 次 gRPC（Feed → Interaction, <3ms）
              + 本地 Redis 查询（Feed Service 自管缓存）
```

网络调用次数从 0 次服务间 RPC 变为 3 次。为保证延迟不劣化：

- **Feed Service 本地缓存**：video 卡片、互动状态的热数据缓存在 Feed Service 本地 Redis，减少穿透 RPC
- **并行请求**：Video 和 Interaction 的查询可并行发起
- **gRPC 连接复用**：同一服务间的多个请求共享 HTTP/2 连接

## 5. 迁移策略

### 阶段一：代码边界提取（不动部署）

```
当前: 一个 Go module，所有 package 在同一个进程
目标: 每个 bounded context 独立 Go module，但仍编译进同一二进制

好处: 数据主权清晰，但部署不变，风险最低
```

### 阶段二：独立部署，HTTP 通信

```
把 Recommend Service 拆出为独立进程
通信方式: JSON over HTTP（最简单，方便调试）

先不用 gRPC，等第二个服务拆分时再引入
```

### 阶段三：引入 gRPC

```
Feed Service ← gRPC → Recommend Service
Feed Service ← gRPC → Interaction Service

API Gateway 统一对外暴露 HTTP，内部使用 gRPC
```

### 阶段四：独立数据存储

```
每个服务拥有独立 DB schema，通过事件订阅同步跨服务数据
不再有跨服务 DB 直连
```

## 6. 不会拆分的内容

以下保持单体架构即可，拆分带来的复杂度 > 收益：

- **Playback Service**：配置 + QoS 上报，数据量小，无独立扩缩容需求
- **Admin 运营接口**：管理后台接口直接部署在 Account Service 或 Video Service 中
- **Upload 服务**：如果对象存储迁移完成，Upload 退化为预签名 URL 生成，无状态，可合并到 Video Service

## 7. 选型权衡

| 方案 | 优点 | 缺点 | 适用阶段 |
|---|---|---|---|
| **继续单体** | 零运维成本，部署简单 | 无法独立扩缩容 | 当前（~万级 DAU） |
| **单体 + 独立 Go module** | 代码主权清晰，编译部署不变 | 还是单体，治理麻烦 | 阶段一 |
| **HTTP + JSON 拆分** | 调试方便（curl），迁移成本低 | 序列化效率低，无契约校验 | 阶段二 |
| **gRPC 拆分** | 强契约，高性能，多语言 | 协议文件管理成本，调试复杂 | 阶段三以后 |
| **gRPC + 事件驱动** | 完整的同步/异步解耦 | 架构复杂度最高 | 阶段四 |
