# CPU 双塔召回训练

本目录提供一个可实际训练的双塔召回基线，Python 3.9+ 标准库即可运行，不需要 GPU、NumPy、PyTorch 或在线模型服务。目标是让 GCFeed 用自己的观看记录学习用户与视频的匹配关系。小型数据适合直接运行；它不是大规模训练框架。

## 先运行演示

从仓库根目录执行：

```bash
python3 tools/twotower/train.py --demo --epochs 12 --k 3 --output /tmp/gcfeed-two-tower-demo.json
python3 -m unittest discover -s tools/twotower -p 'test_*.py' -v
```

`demo/videos.jsonl` 有 18 个明确标注为合成的篮球、吉他、烘焙视频；`demo/events.jsonl` 有 6 个训练用户、24 个训练正反馈和 13 个评估目标（包含 1 个冷用户），也包含曝光、快速划走和重复观看，用于核对样本筛选。训练分界为 `2026-01-06T00:00:00Z`。演示只证明训练和导出链路可运行，不能证明对真实用户有效。

这些评估目标全部来自训练期间没有正反馈的 6 个视频，但视频内容特征已存在，主题与作者也一一对应。模型可以学习内容/作者关联；热门基线只使用训练期次数，因此这些目标的热度均为 0。样例中相对热门基线的高分不能解释为真实业务提升，也不能单独证明语义理解能力。报告按 13 次目标事件平均，而不是按 13 位用户平均；其中暖用户是 6 位用户各 2 次，冷用户是 1 位用户 1 次。

## 两个塔具体学习什么

- 视频输入：标题、描述的词/字符二元三元组，以及作者 ID；用固定的 256 桶稳定 hash 表达稀疏特征，每条最多保留 64 个特征。这一步不学习，也不会从评估集拟合词表。
- 用户输入：该条训练事件**之前**的有效观看视频特征之和，再做归一化。没有历史时使用空输入。
- 用户塔和视频塔分别有自己的可训练线性投影与偏置，随后经过 `tanh` 和 L2 归一化，得到 128 维向量。两塔共 65,792 个可训练参数；不是直接导出固定 hash 向量。
- 使用 BPR 损失 `softplus(-(u·v_positive - u·v_negative) / temperature)`，同时反向更新两个塔。用户塔的偏置也参与学习，因此 `default_user` 是训练后的无历史用户输出。
- 每条正例采一个当时已发布、用户此前没有正反馈的负例。未观看不能解释为明确不喜欢；当前不把 `skip` 当强负例。

每个 epoch 打乱的是已经按时间构造好的训练样本；每条样本的用户历史特征固定为其事件之前的历史，不会因为打乱训练顺序而混入未来行为。每个用户只保留一份“视频 → 首次正例序号”表，样本按自身序号判断哪些正例属于过去，不逐条复制不断增长的已看集合。未来才正反馈的视频仍可作为早期未知负例。负例采样使用按发布时间排序的目录索引，每次最多 32 次随机尝试和 256 次补查，不保存“事件数 × 视频数”的负例表；用户几乎看遍目录时可能跳过该次更新，报告中保留跳过数。

## 从 MySQL 导出真实样本

先核对本地实际表结构与时区。SQL 对应当前仓库的 `video`、`video_view_events` 表；`video.status=2` 是已发布，事件类型为 `exposed/play/complete/skip`。不导出账号、密码、昵称等非训练必需字段。

小项目可在暂停发布与行为写入时导出，或从同一份数据库只读备份导出。下面两个独立 MySQL 客户端连接**不保证跨文件同一快照**；活跃写入场景请由备份/快照流程保证，避免目录与事件不一致。

```bash
mkdir -p /tmp/gcfeed-two-tower-data
mysql --host=127.0.0.1 --port=3307 --user=root --password --database=GCFeed --default-character-set=utf8mb4 --batch --raw --skip-column-names < tools/twotower/export_videos.sql > /tmp/gcfeed-two-tower-data/videos.jsonl
mysql --host=127.0.0.1 --port=3307 --user=root --password --database=GCFeed --default-character-set=utf8mb4 --batch --raw --skip-column-names < tools/twotower/export_events.sql > /tmp/gcfeed-two-tower-data/events.jsonl
```

按提示输入密码；地址、端口和用户应替换为实际环境。`--batch --raw --skip-column-names` 都需要保留，让每行保持一条 JSON 而不是客户端转义后的文本。SQL 是只读查询。

视频每行格式：

```json
{"video_id":1,"author_id":10,"title":"篮球训练","description":"基础运球","published_at":"2026-11-01T08:00:00"}
```

事件每行格式：

```json
{"event_id":1,"user_id":42,"video_id":1,"event_type":"play","watch_ms":8000,"completed":0,"created_at":"2026-11-02T12:00:00"}
```

MySQL `DATETIME` 本身没有时区。上面的 SQL 特意不附加猜测的时区；需要结合应用/容器时区和实际事件时间确认存储的是 UTC 还是本地时间，再给训练命令传 `--source-timezone UTC` 或 `--source-timezone +08:00`。只看 MySQL 会话 `time_zone` 不能证明历史 DATETIME 是按哪个时区写入的。自备数据也可以直接提供带 `Z` 或 `+08:00` 的 ISO 时间。

确认数据后，一条命令训练、评估和导出。例如数据截止到 11 月 25 日，使用 11 月 24 日之前的行为训练，后面的行为评估：

```bash
python3 tools/twotower/train.py --videos /tmp/gcfeed-two-tower-data/videos.jsonl --events /tmp/gcfeed-two-tower-data/events.jsonl --source-timezone +08:00 --cutoff 2026-11-24T00:00:00+08:00 --epochs 20 --k 10 --output /tmp/gcfeed-two-tower.json
```

真实数据必须包含足够的有效观看，以及至少两个当时已发布的视频。只有曝光或快速划走时会明确报“没有训练正例”，不会伪造训练结果。

## 标签与时间切分

1. `complete`，或 `play` 且已完播/观看至少 `--min-watch-ms`（默认 3000ms），算一个正反馈。
2. 每个用户/视频只保留**第一次合格事件**，等权作为一个目标。常见“先有效 play、后 complete”不会加两次，也不会读取后来的完播给早期样本补权重。重复观看的额外兴趣强度暂不建模。
3. `created_at <= cutoff` 用于训练，之后只用于评估。曝光与 `skip` 不参与正例；用户输入排除当前目标及之后的事件；负例候选排除尚未发布的视频。
4. 导出的用户向量只使用 cutoff 之前的历史。评估使用相同的冻结用户向量、参数和默认用户向量；后续评估目标可以更新“已看过滤”，不会反向更新用户画像。冷用户单独统计。

输入会校验 ID、重复事件 ID、缺失视频、事件类型、负观看时长、时区及“行为早于发布时间”等问题。标题/描述/作者是当前导出快照，而不是历史版本；严格历史无泄漏回测需要相应时点的元数据与上下架快照。当前 SQL 仅导出现在仍发布的视频，具有幸存者偏差。这里保证的是交互标签、画像和采样的时间隔离，不能把当前快照回放称为完全的历史线上复现。

## 导出与接入契约

模型是一个完整 JSON 文件：

```json
{"version":"two-tower-<UTC生成时刻>-<随机批次>","dimension":128,"default_user":[0.1],"users":{"42":[0.1]},"videos":{"1":[0.1]}}
```

上例向量仅示意结构，实际 `default_user`、每个 `users` 和 `videos` 向量都是 128 个有限浮点数、L2 长度为 1；ID 是十进制字符串。`version` 每次生成不同，用户塔与视频塔必须作为同一批次同时加载，不能跨版本混用。文件在同目录写临时文件、`fsync` 后原子替换，读取方不会看到半个 JSON。

此文件导出的是向量，不包含训练器参数或可在线编码器。新视频、新用户行为要在下一次训练导出后才能更新对应向量；有个人向量时使用个人向量，没有个人向量时使用本批次的 `default_user`。向量相似度只是召回分数，不是校准后的点击/完播概率。模型文件不存在时，推荐首屏回退到最新视频流。

训练还生成 `<output>.metrics.json`，包含切分时刻、超参、各 epoch 损失、跳过负例的次数、整体/暖用户/冷用户的 Recall@K、MRR@K、NDCG@K，以及只用训练期行为计算的热门排序基线。每个评估事件只有一个相关目标，所以此处 Recall@K 也等于命中率；指标按事件平均。评估会遍历目标时刻已发布的完整目录，排除该用户之前的正反馈视频。候选过少时指标容易偏高，应同时检查 `mean_candidates`；没有评估目标时指标为 `null`。

指标报告与模型各自原子写入，模型最后替换。部署或归档时核对两份文件的 `version` 一致。不要用演示集分数、训练损失下降或离线指标直接宣称线上提升。

## 评估通过后全量训练并上线

前面的 `--cutoff` 模式用于比较模型与热门基线，不应用评估集反复调参后再把同一评估集当作独立效果证据。确定特征和超参之后，保留回测报告；需要上线的版本另用同一份数据的全部合格行为训练，以包含最新用户历史：

```bash
python3 tools/twotower/train.py --videos /tmp/gcfeed-two-tower-data/videos.jsonl --events /tmp/gcfeed-two-tower-data/events.jsonl --source-timezone +08:00 --full-train --epochs 20 --k 10 --output apps/api/models/twotower.json
```

`--full-train` 与 `--cutoff` 互斥，以输入事件的最大时间为训练截止点。其报告明确标记 `mode=full_train_no_held_out_evaluation`，评估目标为 0、指标为 `null`；它用于生成服务快照，不提供新的效果证明。以后每次重跑都从头训练并输出完整的新批次，不在旧向量上增量拼接。

默认本地配置 `recommendation_model: "./models/twotower.json"` 相对 API 进程工作目录读取；从仓库根目录生成到 `apps/api/models/twotower.json` 即与之匹配。Docker Compose 已将此目录只读挂载到 API 的 `/app/models`。模型在 API 启动时加载，替换文件后重启 API：

```bash
docker compose -f apps/docker-compose.yml up -d --build api
docker compose -f apps/docker-compose.yml logs --tail=100 api
```

已运行容器且镜像无需重建时，使用 `docker compose -f apps/docker-compose.yml restart api`。Milvus 配置启用时，等待日志出现本批次 `two-tower index ready` 后再检查推荐结果；本批次视频向量与用户向量始终一起切换。仅模型文件不存在时推荐首屏回退到最新流；文件损坏或 Milvus 故障返回 503，需检查日志处理。不能把最新流回退结果当成模型推荐验证。

不运行 Milvus 的小型本地验证，可将 API 实际加载的配置中的 `milvus.address` 设为空字符串，重启后使用内存精确余弦检索；训练器和模型文件格式均不变。默认 Compose 仍会启动 Milvus。模型已经加载但候选都因发布状态/曝光被过滤时，返回的是空的推荐结果，不会补入热榜或关注流。

## 使用范围与更新

纯 Python 稀疏投影适合先跑通小型项目；耗时取决于样本量、每条特征数和 epoch 数。训练前先在导出子集上测耗时与内存，避免直接把大规模生产流水交给本工具。当前内存保留视频目录、每用户一份历史索引和每事件最多 64 个非零特征，不保存每事件全目录或完整历史副本。训练数据很大时仍需分批/流式训练改造；本版本没有分布式训练、断点续训或超参搜索。

确定接入后，按业务数据积累速度手动或定时重跑，先观察真实留出集相对热门基线的变化，再做受控用户实验。可以用固定种子比较训练改动；每次部署仍生成独立版本。所有导出数据和向量都应按用户行为数据管理，不要提交真实用户数据到仓库。
