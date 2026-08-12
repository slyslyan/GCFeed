# 推荐模块设计

## 1. 模块职责

推荐模块负责候选召回、排序打散和曝光去重，为 Feed 提供可下发的视频列表。

## 2. 接口设计

| 方法 | 接口路径 | 作用 | 鉴权 | 幂等键 |
| --- | --- | --- | --- | --- |
| POST | `/internal/recommendation-candidates` | 一次完成召回、排序、打散 | 服务鉴权 | 支持 |
| POST | `/internal/exposure-decisions` | 判断候选是否近期曝光 | 服务鉴权 | 支持 |
| POST | `/internal/exposures` | 写入曝光记录 | 服务鉴权 | 支持 |

## 3. 数据表设计

### 3.1 `reco_rule`

| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| `id` | BIGINT | PK | 规则 ID |
| `scene` | VARCHAR(32) | NOT NULL | 场景，如 `feed` |
| `config_json` | JSON | NOT NULL | 召回、排序、打散参数 |
| `enabled` | TINYINT | NOT NULL, DEFAULT 1 | 是否启用 |
| `updated_at` | DATETIME | NOT NULL | 更新时间 |

索引建议：`uk_scene(scene)`。

### 3.2 `reco_exposure_log`

| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| `id` | BIGINT | PK | 记录 ID |
| `user_id` | BIGINT | NOT NULL | 用户 ID |
| `video_id` | BIGINT | NOT NULL | 视频 ID |
| `scene` | VARCHAR(32) | NOT NULL | 场景 |
| `request_id` | VARCHAR(64) | NOT NULL | 请求 ID |
| `exposed_at` | DATETIME | NOT NULL | 曝光时间 |

索引建议：`idx_user_scene_time(user_id, scene, exposed_at)`、`idx_request_id(request_id)`。

## 4. 业务规则

| 规则 | 说明 |
| --- | --- |
| 候选只返回上线视频 | 下架、删除和异常视频不进入候选 |
| 曝光去重按用户生效 | 同一用户近期曝光过的视频降低或移除优先级 |
| 打散避免同作者集中 | 同一作者的视频在单页中保持间隔 |
| 请求携带 scene | 不同 Feed 场景可使用不同策略 |
| 内部接口支持幂等 | 重复曝光写入不会产生重复事实 |

## 5. 测试建议

| 场景 | 期望 |
| --- | --- |
| 请求推荐候选 | 返回上线视频列表 |
| 同一作者候选过多 | 结果被打散 |
| 判断近期曝光视频 | 返回已曝光状态 |
| 写入曝光记录 | 记录 request_id 和曝光时间 |
| 重复写入曝光 | 结果稳定 |

## 6. 前端接入点

推荐模块主要服务后端 Feed。前端通过 Feed 接口间接使用推荐结果。

## 7. 多路召回（当前实现）

推荐链路采用三路召回 + 合并器的分层结构：

| 召回路 | 来源 | 解决什么问题 |
| --- | --- | --- |
| 热度召回 | `video` + `video_stat`，hot_score 倒序 | 普适兜底，冷启动用户保障 |
| 关注召回 | `user_follow` JOIN `video`，关注作者 72h 内新视频 | 新视频冷启动（关系链社交信号） |
| 向量召回 | Milvus 2.4 `video_embedding` collection（128 维 hash-ngram + HNSW/COSINE 索引） | 个性化（用户兴趣向量 ANN topK） |

- 召回层抽象：`Recaller` 接口 + `Merger` 合并器（去重、热度补齐、关注路配额保底、曝光剔除）。
- 三路并发执行，单路超时（500ms）或失败只丢弃该路结果，不影响其他路；无召回器注册时回退单路热度池。
- 向量路选型：MySQL 8.4 Community 实测不支持 VECTOR 类型（1064 语法错误、无 STRING_TO_VECTOR），改选 Milvus standalone（etcd + minio + milvus docker compose 部署，`infra/vector/milvus.go` 封装 `VectorStore` 接口）。
- 向量写入：embedding worker 生成向量后 MySQL 落库 + Milvus 双写（失败只记日志不阻塞消息流），worker 启动时对存量向量按主键 upsert 幂等回填；Milvus 未就绪不阻塞服务启动（`LazyStore` 后台重试连接 + 建集合），向量路就绪前自动降级。
- 向量路 ANN 查询不带业务过滤，状态/时间过滤在应用层完成（`enrichVideos` 批量校验 status + 30 天窗口）。
- Milvus COSINE 距离即 1 - 余弦相似度，召回距离直接换算为粗排 similarity 特征；关注路候选有新鲜度加成，避免低热度新视频被公式压底。
- 关注路双保险：72h 时间窗口 + 条数硬上限（200），防止大 V 刷屏撑爆候选池。

## 8. 未来规划：内容审核与黑名单

当前项目没有审核功能，因此没有黑名单机制（仅热度/关注/向量三路按状态过滤）。后续如果加入内容审核，建议路径：

1. 新增审核能力（人工/机审判定视频违规），违规结果落库（`video.status` 或独立审核表）。
2. 违规事件触发写 Redis 黑名单集合（如 `ban:video:{id}`，带 TTL）。
3. 多路召回统一接入：热度/关注路 SQL 增加黑名单过滤，向量路 `enrichVideos` 应用层批量校验（`SMISMEMBER`）。
4. 兜底：黑名单 Redis 不可用时跳过校验（与向量路降级同策略），优先保证可用性。
