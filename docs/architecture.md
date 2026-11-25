# 视频 Feed 系统架构

GCFeed 由 Go API、异步 Worker 和 React Web 组成。推荐模型在 CPU 上离线训练，API 加载同批用户和视频向量，在本机或 Milvus 中检索。图中虚线表示离线数据传递或规划中的能力。[训练说明](../tools/twotower/README.md) · [推荐模块](modules/recommendation.md)

## 1. 系统上下文

这张图展示客户端、API、Worker、离线训练与存储的边界。对象存储迁移仍属规划。

```mermaid
---
config:
  theme: base
  layout: dagre
  themeVariables:
    background: transparent
    fontFamily: Inter, PingFang SC, Microsoft YaHei, Arial
    primaryTextColor: "#0F172A"
    lineColor: "#94A3B8"
    clusterBkg: "#F8FAFC"
    clusterBorder: "#CBD5E1"
---
flowchart LR
  %% GCFeed MVP system context
  classDef client fill:#EFF6FF,stroke:#60A5FA,color:#0F172A,stroke-width:1px;
  classDef system fill:#DCFCE7,stroke:#22C55E,color:#0F172A,stroke-width:1px;
  classDef store fill:#F1F5F9,stroke:#64748B,color:#0F172A,stroke-width:1px;
  classDef future fill:#FAE8FF,stroke:#C084FC,color:#0F172A,stroke-width:1px,stroke-dasharray:5 5;

  Web["Web App<br/>React + Vite"]
  Client["移动端 / API 调用方"]
  API["GCFeed API<br/>Go + Gin"]
  MySQL[("MySQL<br/>业务数据")]
  Uploads[("uploads<br/>视频 / 封面 / 头像")]
  Redis[("Redis<br/>缓存与计数")]
  MQ[("MQ<br/>异步事件")]
  Worker["Worker<br/>互动落库 / 视频 fanout"]
  Trainer["CPU 双塔离线训练<br/>Python 标准库"]
  Snapshot[("JSON 快照<br/>同批用户 / 视频向量")]
  Milvus[("Milvus 可选<br/>快照视频索引")]
  ObjectStorage[("对象存储<br/>媒体文件")]

  Web -->|"调用管理与浏览接口"| API
  Client -->|"调用公开 API"| API
  API -->|"读写业务数据和观看行为"| MySQL
  API -->|"保存和读取本地文件"| Uploads
  API -->|"缓存 Feed 与计数"| Redis
  API -->|"投递互动、发布和观看事件"| MQ
  MQ -->|"消费互动和发布事件"| Worker
  Worker -->|"落库 / fanout"| MySQL
  Worker -->|"关注流索引与预热"| Redis
  MySQL -.->|"只读导出视频与行为"| Trainer
  Trainer -->|"全批训练并导出"| Snapshot
  Snapshot -->|"API 启动时加载"| API
  API -->|"完整导入本批索引后检索"| Milvus
  API -.->|"迁移媒体文件"| ObjectStorage

  class Web,Client client;
  class API,Worker,Trainer system;
  class MySQL,Uploads,Redis,MQ,Snapshot,Milvus store;
  class ObjectStorage future;
  linkStyle default stroke:#94A3B8,stroke-width:1.4px
```

## 2. API 内部分层

这张图展示 Go API 单体内部的主要代码层次和依赖方向。

```mermaid
---
config:
  theme: base
  layout: dagre
  themeVariables:
    background: transparent
    fontFamily: Inter, PingFang SC, Microsoft YaHei, Arial
    primaryTextColor: "#0F172A"
    lineColor: "#94A3B8"
    clusterBkg: "#F8FAFC"
    clusterBorder: "#CBD5E1"
---
flowchart LR
  %% Internal layering in apps/api
  classDef entry fill:#F8FAFC,stroke:#94A3B8,color:#0F172A,stroke-width:1px;
  classDef http fill:#DBEAFE,stroke:#3B82F6,color:#0F172A,stroke-width:1px;
  classDef service fill:#DCFCE7,stroke:#22C55E,color:#0F172A,stroke-width:1px;
  classDef domain fill:#CCFBF1,stroke:#14B8A6,color:#0F172A,stroke-width:1px;
  classDef infra fill:#FFEDD5,stroke:#F97316,color:#0F172A,stroke-width:1px;
  classDef store fill:#F1F5F9,stroke:#64748B,color:#0F172A,stroke-width:1px;

  Entry["cmd/feed/main.go<br/>启动装配"]
  Router["Router<br/>路由注册"]
  Auth["JWT Middleware<br/>鉴权上下文"]
  Handlers["HTTP Handlers<br/>account / video / feed / recommendation / exposure 等"]
  Services["Application Services<br/>业务用例与推荐排序"]
  Domains["Domain Models<br/>业务规则与接口"]
  Config["Config Loader<br/>configs/config.yaml"]
  JWT["JWT Manager<br/>签发访问令牌"]
  Repo["GORM Repository<br/>仓储实现"]
  SQL["database/sql<br/>MySQL 连接"]
  MySQL[("MySQL")]
  Uploads[("uploads 目录")]
  Recaller["Two-tower Recaller<br/>快照检索"]
  Model[("JSON 快照 / 可选 Milvus")]

  Entry -->|"加载配置"| Config
  Entry -->|"创建数据库连接"| SQL
  Entry -->|"注册 HTTP 路由"| Router
  Router -->|"校验受保护接口"| Auth
  Auth -->|"解析和签名 Token"| JWT
  Router -->|"分发业务请求"| Handlers
  Handlers -->|"调用用例服务"| Services
  Services -->|"执行领域规则"| Domains
  Services -->|"读写仓储"| Repo
  Services -->|"检索双塔候选"| Recaller
  Recaller -->|"读取同批用户与视频向量"| Model
  Recaller -->|"校验视频与作者状态"| Repo
  Repo -->|"复用连接池"| SQL
  SQL -->|"持久化数据"| MySQL
  Handlers -->|"保存上传文件"| Uploads
  Router -->|"暴露静态文件"| Uploads

  class Entry entry;
  class Router,Auth,Handlers http;
  class Services service;
  class Domains domain;
  class Config,JWT,Repo,SQL,Recaller infra;
  class MySQL,Uploads,Model store;
  linkStyle default stroke:#94A3B8,stroke-width:1.4px
```

## 3. 核心请求链路

这张图展示从注册、登录、上传、发布到最新流和热门流的主要链路。推荐流另见下方说明。

```mermaid
---
config:
  theme: base
  themeVariables:
    background: transparent
    fontFamily: Inter, PingFang SC, Microsoft YaHei, Arial
    primaryTextColor: "#0F172A"
    actorBkg: "#EFF6FF"
    actorBorder: "#60A5FA"
    actorTextColor: "#0F172A"
    activationBkgColor: "#DCFCE7"
    activationBorderColor: "#22C55E"
    sequenceNumberColor: "#475569"
    lineColor: "#94A3B8"
---
sequenceDiagram
  autonumber
  participant C as Client
  participant R as Gin Router
  participant H as Handler
  participant S as Service
  participant Repo as GORM Repository
  participant DB as MySQL
  participant Redis as Redis
  participant FS as uploads

  C->>R: POST /api/users
  R->>H: Account.Register
  H->>S: 创建账户
  S->>Repo: Save account
  Repo->>DB: INSERT account
  DB-->>Repo: 返回 user id
  Repo-->>S: 返回用户实体
  S-->>H: 返回 profile
  H-->>C: 201 Created

  C->>R: POST /api/sessions
  R->>H: Account.Login
  H->>S: 校验账号密码
  S->>Repo: FindByAccount
  Repo->>DB: SELECT account
  DB-->>Repo: 返回用户记录
  S-->>H: 返回 Bearer JWT
  H-->>C: 返回 access_token

  C->>R: POST /api/uploads
  R->>H: Upload.Create
  H->>FS: 写入 video / cover / avatar
  FS-->>H: 返回文件路径
  H-->>C: 返回 /uploads/{kind}/{filename}

  C->>R: POST /api/videos
  R->>H: Video.Create
  H->>S: CreatePublished
  S->>Repo: Save video and video_stat
  Repo->>DB: INSERT video, video_stat
  DB-->>Repo: 返回 video id
  Repo-->>S: 返回视频实体
  S-->>H: 返回视频详情
  H-->>C: 201 Created

  C->>R: GET /api/feed-items?scene=timeline&cursor=...&limit=...
  R->>H: Feed.ListFeedItems
  H->>S: GetFeed(scene=timeline)
  S->>Repo: ListTimelinePage
  Repo->>DB: SELECT video_id, published_at
  DB-->>Repo: 返回轻量页
  S->>Redis: MGET video:card / video:stat
  S->>Repo: BatchGetFeedCards / BatchGetFeedStats
  Repo->>DB: 批量回源缺失卡片和计数
  Repo-->>S: 返回缺失数据
  S-->>H: 返回 items + next_cursor + has_more
  H-->>C: 返回 timeline feed items

  C->>R: PUT /api/videos/{videoId}/like
  R->>H: Interaction.Like
  H->>S: SetAction
  S->>Repo: 更新互动和 video_stat
  S->>Redis: ZINCRBY feed:hot:minute:v1:{minute}

  C->>R: GET /api/feed-items?scene=hot&limit=10
  R->>H: Feed.ListFeedItems
  H->>S: GetFeed(scene=hot)
  S->>Redis: ZUNIONSTORE 最近 60 个分钟桶
  S->>Redis: ZREVRANGE 热榜窗口
  S-->>H: 返回一小时热榜 items + cursor
  H-->>C: 返回 hot feed items
```

### 推荐流与离线更新

`RecommendStrategy.List` 需要登录，调用推荐服务和快照召回器；召回器使用快照中的个人向量或默认用户向量检索，再批量校验视频已发布且作者正常。推荐服务按相似度排序、去重和剔除近期曝光，分页后做同作者打散，Feed 最后复用卡片与计数的批量组装。

Web 将有效观看和完播写入 `video_view_events`。新增行为、新用户历史和新视频在下次 CPU 训练、导出完整快照并重启 API 后生效；当前没有消费观看消息来实时修改双塔向量的 Worker。可选 Milvus 使用快照内容摘要隔离集合，完整导入该批视频向量后才接受查询。

快照缺失时，推荐首屏走最新流，续页保留 `latest.` 游标和最新流排序。快照损坏、Milvus 未就绪或索引故障返回 503；有效模型没有候选时返回空列表。模型推荐游标绑定快照版本，升级后需刷新首屏。详细边界见[Feed 模块](modules/feed.md#6-recommend-读取与分页)。

## 4. 数据模型

这张图只展示内容查询依赖的三张核心表。实际自动迁移还覆盖会话、关系、互动、消息、播放、观看行为和曝光等表；完整列表以 `internal/infra/persistence/migration/migration.go` 为准。

```mermaid
---
config:
  theme: base
  themeVariables:
    background: transparent
    fontFamily: Inter, PingFang SC, Microsoft YaHei, Arial
    primaryTextColor: "#0F172A"
    lineColor: "#94A3B8"
---
erDiagram
  ACCOUNT ||--o{ VIDEO : "发布视频"
  VIDEO ||--|| VIDEO_STAT : "拥有计数"

  ACCOUNT {
    bigint id PK
    string account UK
    string password
    string nickname
    string avatar_url
    string role
    int status
  }

  VIDEO {
    bigint id PK
    bigint author_id FK
    string title
    string media_url
    string cover_url
    int status
    datetime published_at
    string idempotency_key
  }

  VIDEO_STAT {
    bigint video_id PK
    int like_count
    int comment_count
    int favorite_count
  }
```

## 5. 演进能力地图

这张图展示已落地模块和规划模块的连接方式。审核、后台运营、完整治理仍属规划；Prometheus/Grafana 基础采集与看板已经接入，完整告警管理仍待补齐。

```mermaid
---
config:
  theme: base
  layout: dagre
  themeVariables:
    background: transparent
    fontFamily: Inter, PingFang SC, Microsoft YaHei, Arial
    primaryTextColor: "#0F172A"
    lineColor: "#94A3B8"
    clusterBkg: "#F8FAFC"
    clusterBorder: "#CBD5E1"
---
flowchart TB
  %% Roadmap map from current MVP to planned modules
  classDef current fill:#DCFCE7,stroke:#22C55E,color:#0F172A,stroke-width:1px;
  classDef growth fill:#FAE8FF,stroke:#C084FC,color:#0F172A,stroke-width:1px,stroke-dasharray:5 5;
  classDef platform fill:#FFEDD5,stroke:#F97316,color:#0F172A,stroke-width:1px,stroke-dasharray:5 5;
  classDef data fill:#F1F5F9,stroke:#64748B,color:#0F172A,stroke-width:1px,stroke-dasharray:5 5;

  Account["账户"]
  Video["视频"]
  Feed["Feed"]
  Upload["上传"]
  Recommendation["推荐"]
  Interaction["互动"]
  Review["审核"]
  Message["消息"]
  Admin["后台运营"]
  Governance["系统治理"]
  Observability["监控基础 / 告警规划"]
  AsyncStore[("Redis / MQ")]
  ObjectStore[("对象存储规划")]

  Account -->|"扩展关注关系"| Interaction
  Video -->|"进入内容审核"| Review
  Video -->|"承载点赞评论收藏"| Interaction
  Upload -.->|"未来迁移媒体文件"| ObjectStore
  Feed -->|"双塔检索与排序"| Recommendation
  Recommendation -->|"返回候选内容"| Feed
  Interaction -->|"投递互动事件"| AsyncStore
  Interaction -->|"生成站内通知"| Message
  Admin -->|"处理审核任务"| Review
  Governance -->|"保护核心接口"| Feed
  Observability -->|"采集服务指标"| Governance

  class Account,Video,Feed,Upload,Recommendation,Interaction,Message current;
  class Review,Admin growth;
  class Governance,Observability platform;
  class AsyncStore current;
  class ObjectStore data;
  linkStyle default stroke:#94A3B8,stroke-width:1.4px
```

## 6. 说明

- 当前代码以 Go API 单体承载用户端业务和推荐检索，内部按接口层、应用层、领域层、基础设施层组织；互动落库和视频 fanout 由独立 Worker 进程消费。
- 对外接口统一挂载在 `/api/*`，静态文件通过 `/uploads/*` 访问，健康检查使用 `/health`。
- MySQL 保存业务事实和观看行为，Redis 保存 Feed 缓存、短期行为状态和热榜；双塔服务读取本批训练快照，旧 `video_embedding` 表不是当前双塔向量来源。
- Feed 通过 `scene` 分发 `timeline`、`hot`、`following`、`recommend` 四个策略，分别对应最新、热门、关注和双塔推荐。关注、热门频道保持独立。
- 审核、后台运营和治理能力仍是演进边界；服务拆分方案见 [evolution.md](evolution.md)，不代表当前已经部署微服务。
