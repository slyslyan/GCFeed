# GCFeed

GCFeed 是一个面向短视频场景的 Feed 系统工程。项目用 Go API 单体、React Web 客户端、MySQL、Redis 和 RabbitMQ 承载内容供给、分发、消费、互动和治理链路。

## 当前状态

已实现能力：

- 后端分层结构：Domain、Application、Infrastructure、Interfaces。
- Gin HTTP 服务入口和 REST API 路由。
- MySQL + GORM 持久化。
- JWT 登录态。
- Redis Feed 缓存、热榜和互动计数。
- RabbitMQ 异步互动落库和视频发布事件。
- 推荐频道使用 CPU 离线训练的双塔向量单路召回，使用个人或默认用户向量；模型文件不存在时推荐首屏展示最新流。
- React + Vite Web 客户端。
- 消息中心和播放优化接入。
- API 流程测试和 Web 生产构建。
- Prometheus 指标和 Grafana 监控面板。

重点待补能力：

- 审核后台。
- 后台运营。
- 系统治理。

## 快速启动

### Docker Compose

前置依赖：

- Docker
- Docker Compose

启动：

```bash
cd apps
docker compose up --build
```

后台启动：

```bash
cd apps
docker compose up -d --build
```

查看日志：

```bash
cd apps
docker compose logs -f api web
```

停止：

```bash
cd apps
docker compose down
```

清理数据库、Redis 和上传文件数据卷：

```bash
cd apps
docker compose down -v
```

服务地址：

| 服务 | 地址 |
| --- | --- |
| Web | `http://127.0.0.1:5173` |
| API 健康检查 | `http://127.0.0.1:8080/health` |
| API 指标 | `http://127.0.0.1:8080/metrics` |
| MySQL | `127.0.0.1:3307` |
| Redis | `127.0.0.1:6379` |
| RabbitMQ 管理台 | `http://127.0.0.1:15672` |
| Prometheus | `http://127.0.0.1:9090` |
| Grafana 面板 | `http://127.0.0.1:3000/d/gcfeed-overview/gcfeed-overview` |

### 启用双塔推荐

本节训练和模型重启命令均从仓库根目录执行。

登录后可访问推荐频道；模型文件不存在时，首屏按发布时间倒序展示，后续分页继续沿用同一最新流顺序。启用个性化需要从当前 GCFeed 实例导出视频和有效观看行为，按[训练说明](tools/twotower/README.md)完成时间切分评估和全量训练，将完整快照放在 `apps/api/models/twotower.json`，然后执行：

```bash
docker compose -f apps/docker-compose.yml restart api
docker compose -f apps/docker-compose.yml logs --tail=100 api
```

Python 3.9+ 标准库即可训练，不需要 GPU 或额外机器学习依赖。默认 Compose 使用 Milvus 检索，需等本批次索引加载完成；小型本地目录可将 API 配置中的 `milvus.address` 设为空字符串，使用内存精确余弦检索。模型在 API 启动时加载，用户画像和新视频向量随下一份快照更新。本地运行时重启 Go API 即可。

演示训练可单独运行，不依赖数据库：

```bash
python3 tools/twotower/train.py --demo --epochs 12 --k 3 --output /tmp/gcfeed-two-tower-demo.json
```

演示数据的 ID 仅适用于配套样例；部署快照应由本实例的数据生成。合成样例的离线指标不能代表真实用户效果。

### 本地开发

```bash
./scripts/start.sh
```

默认地址：

| 服务 | 地址 |
| --- | --- |
| Web | `http://127.0.0.1:5173` |
| API | `http://127.0.0.1:8080` |

## 验证与指标

### 自动化测试

后端测试：

```bash
cd apps/api
go test ./...
```

前端生产构建：

```bash
npm --prefix apps/web run build
```

Compose 配置校验：

```bash
cd apps
docker compose config
```

### Feed 压测

详细步骤见 [docs/performance-testing.md](docs/performance-testing.md)，包括最新流、热门榜单、推荐流、极限 QPS 和 Grafana 指标解读。

重点查看 `http_reqs` 后面的 `/s`、`http_req_duration p(95)`、`http_req_failed` 和 `feed_success_rate`。

### 监控面板

Docker Compose 会启动 Prometheus 和 Grafana：

```bash
cd apps
docker compose up -d --build
```

Grafana 默认账号密码：

```text
admin / admin
```

内置面板：`GCFeed / GCFeed Overview`

```text
http://127.0.0.1:3000/d/gcfeed-overview/gcfeed-overview
```

面板覆盖 API QPS、5xx 错误率、API P95、Feed P95、Feed 缓存命中率、上传处理耗时和 Worker 成功率。

Prometheus 抓取目标：

- `gcfeed-api`：`api:8080/metrics`
- `gcfeed-worker`：`worker:9091/metrics`

## 文档地图

| 文档 | 用途 |
| --- | --- |
| [docs/product.md](docs/product.md) | 产品范围、模块地图、P0/P1 功能清单 |
| [docs/quickread.md](docs/quickread.md) | 新读者代码阅读路线 |
| [docs/architecture.md](docs/architecture.md) | 系统架构、分层、核心链路、数据模型 |
| [docs/engineering.md](docs/engineering.md) | 工程规范、目录规则、API 风格、测试约定 |
| [docs/optimization.md](docs/optimization.md) | Feed 性能和稳定性专题 |
| [docs/performance-testing.md](docs/performance-testing.md) | k6 压测、QPS/P95 解读、Grafana 指标观察 |
| [从一次观看到百万视频召回](docs/two-tower-hnsw.md) | 用样例理解双塔、HNSW 建图，以及用户、视频或模型变化时该更新什么 |
| [tools/twotower/README.md](tools/twotower/README.md) | CPU 双塔训练、留出评估、全量导出和模型上线 |
| [docs/uiux.md](docs/uiux.md) | Web 客户端 UI/UX 规格 |
| [docs/modules/](docs/modules/README.md) | 各业务模块设计 |
| [openspec/](openspec/) | OpenSpec 项目基线和变更规格 |

## 开发方式

新增功能优先按 OpenSpec 建 change，再按工程规范实现：

```bash
openspec list
openspec validate --all --strict
```

新增后端模块时参考 [docs/engineering.md](docs/engineering.md) 的分层模板和 [docs/modules/README.md](docs/modules/README.md) 的模块规格入口。
