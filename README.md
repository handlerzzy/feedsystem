# FeedFlow

基于 Go + Vue 3 的短视频 Feed 系统，含账号、视频、点赞、评论、关注、Feed 流、私信、通知，支持 Redis 缓存、RabbitMQ 异步 Worker、分片上传、SSE 实时推送、Docker Compose 部署。

## 来源与二次开发说明

本项目基于开源项目 [LeoninCS/feedsystem_video_go](https://github.com/LeoninCS/feedsystem_video_go)
二次开发，原项目采用 Go + Vue 3 实现账号、视频、点赞、评论、关注、Feed 与私信等基础能力。

在此基础上重新设计并实现的部分包括：

- **Feed 读路径**：Redis ZSET 全局时间线 + 冷热拼接 + 三级缓存，并加入时间线自愈
- **发布链路**：outbox 事务性消息 + `SKIP LOCKED` 抢占，替代同步写时间线
- **一致性**：点赞/关注全链路幂等、缓存防击穿与按模式失效、统一错误契约
- **AI 能力（可选层）**：独立 gRPC sidecar、语义标签与摘要、语义召回与多路配额融合、
  推荐理由，以及一套按用户口径的离线评测
- **工程质量**：zap 结构化日志与请求 id 关联、`/livez` `/readyz` 分级健康检查、
  分片上传断点续传、端到端回归脚本与 CI

感谢原作者将项目开源。

## 功能

| 模块 | 功能 |
|------|------|
| 账号 | 注册、登录、Refresh Token、改名、改密、登出、头像上传、个人简介、主页统计 |
| 视频 | 普通上传、5MB 分片上传、断点续传、封面上传、发布、作者作品、详情缓存、#话题标签 |
| 点赞 | 点赞、取消点赞、是否已赞、已赞列表、RabbitMQ 异步落库、热度更新、SSE 通知 |
| 评论 | 发布、删除、列表、@username 提及通知、RabbitMQ 异步落库、热度更新 |
| 关注 | 关注、取关、粉丝列表、关注列表、粉丝/关注计数、SSE 通知 |
| Feed | 推荐流、关注流、点赞榜、热榜、话题流、冷热分离、游标分页、短视频沉浸播放 |
| 私信 | 发送私信、按对端用户查看最近 50 条会话 |
| 通知 | 点赞/评论/关注事件通知、提及通知、SSE 实时推送、通知列表、未读计数、已读标记 |
| 工程 | Docker Compose、`start.sh`、API/Worker 拆分运行、限流、pprof、健康检查、zap 结构化日志与请求 id 关联、端到端回归脚本 |

## Docker Compose 一键启动

```bash
docker compose up -d --build
```

访问：
- 前端：`http://localhost:5173`
- 后端 API：`http://localhost:8080`
- RabbitMQ 管理台：`http://localhost:15672`（`admin` / `password123`）

Docker Compose 会读取 `.env`，缺省使用 `feedflow-dev-secret-key`。生产环境请修改 `JWT_SECRET`。

## 脚本启动

```bash
./start.sh
```

`start.sh` 默认启动 RabbitMQ、Redis、后端 API、Worker 与前端。常用开关：

```bash
START_FRONTEND=0 ./start.sh       # API + Worker
START_WORKER=0 ./start.sh         # API + 前端
STOP_DOCKER=1 ./start.sh          # 退出时停止脚本拉起的 compose 服务
CONFIG_PATH=configs/config.yaml ./start.sh
```

## 本地开发

`JWT_SECRET` 是必填项（缺失时后端启动即失败）。下面的值仅用于本地开发，**生产环境必须换成随机强密钥**。
Worker 不使用 JWT，因此不需要 `JWT_SECRET`。

注意 compose 把 MySQL 暴露在宿主机 **3307**（不是 3306，避免与本机已有实例冲突）；
`configs/config.compose-local.yaml` 已按 3307 配好，直接用即可。

```bash
# 启动依赖
docker compose up -d mysql redis rabbitmq

# 后端
cd backend
JWT_SECRET=feedflow-dev-secret-key CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd

# Worker（不需要 JWT_SECRET）
cd backend
CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd/worker

# 前端
cd frontend
npm install && npm run dev
```

## 测试

单元测试不需要任何外部依赖：

```bash
cd backend && go test ./...
```

集成测试用环境变量门控，**未设置时自动跳过**（因此默认不会被 CI 之外的偶然因素影响）：

```bash
cd backend
TEST_MYSQL_DSN='root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \
TEST_RABBITMQ_URL='amqp://admin:password123@127.0.0.1:5672/' \
  go test -count=1 ./...
```

- `TEST_MYSQL_DSN` 覆盖事务语义（回滚、幂等去重、`GREATEST` 兜底）——这类性质在 mock 上不成立，必须用真库。
- `TEST_RABBITMQ_URL` 覆盖连接自愈（把已建立的连接弄死后能否重连）。

仓库根目录另有两个脚本：

| 脚本 | 用途 | 依赖 |
|------|------|------|
| `./e2e_regression.sh` | 端到端功能回归：注册 → 登录 → 发布 → outbox → 时间线 → 点赞 → 评论 → 通知，并校验错误契约与请求 id 贯穿 | 整套服务跑着（默认打 `127.0.0.1:8080`，可用 `BASE=` 改） |
| `./verify.sh` | 只读「体检」：查索引、看执行计划、grep 代码，逐条给出 ✅/⚠️/❌ 与修法 | 只需要 MySQL（脚本头部那句 `docker compose up -d mysql redis rabbitmq` 是通用提示，实际不碰 Redis / RabbitMQ） |

`e2e_regression.sh` 会创建测试账号与视频，结束时按 id 精确清理，并顺带清掉注册限流键
（否则连跑两次就会撞上 5 次/时 的上限）。`verify.sh` 全程只读，跑一百遍也不会改变任何状态。

## CI

GitHub Actions 配置位于 `.github/workflows/ci.yml`，在 Pull Request 以及推送到 `main`、`master` 时运行。

- 后端：Go 1.24.x，执行 `go mod download`、`go vet ./...`、`go test -race -count=1 ./...`
- 前端：Node.js 22，执行 `npm ci`、`npm run build`

## 接口清单

### 账号 `/account`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/register` | 否 | 注册（限流 5次/时/IP） |
| POST | `/login` | 否 | 登录，返回 access_token + refresh_token |
| POST | `/refresh` | 否 | 刷新 access_token（用 refresh_token） |
| POST | `/changePassword` | JWT | 需登录，主体取自 token（限流 5次/时/账号） |
| POST | `/findByID` | 否 | 按 ID 查用户 |
| POST | `/findByUsername` | 否 | 按用户名查 |
| POST | `/logout` | JWT | 登出（同时失效双 token） |
| POST | `/rename` | JWT | 改名 |
| POST | `/uploadAvatar` | JWT | 上传头像（jpg/png/webp，≤10MB） |
| POST | `/updateProfile` | JWT | 更新简介/头像 |

### 视频 `/video`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/publish` | JWT | 发布视频（自动提取 #话题） |
| POST | `/uploadVideo` | JWT | 上传视频文件（mp4，≤200MB） |
| POST | `/uploadCover` | JWT | 上传封面（jpg/png/webp，≤10MB） |
| POST | `/chunk/init` | JWT | 初始化分片上传（文件 MD5、大小、分片数） |
| POST | `/chunk/upload` | JWT | 上传单个分片（multipart，含分片 MD5 校验） |
| POST | `/chunk/status` | JWT | 查询已上传分片 |
| POST | `/chunk/complete` | JWT | 合并分片并返回 play_url |
| POST | `/listByAuthorID` | 否 | 按作者查视频 |
| POST | `/getDetail` | 否 | 视频详情缓存 |

### 点赞 `/like`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/like` | JWT | 点赞 |
| POST | `/unlike` | JWT | 取消点赞 |
| POST | `/isLiked` | JWT | 是否已赞 |
| POST | `/listMyLikedVideos` | JWT | 我赞过的视频 |

### 评论 `/comment`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/listAll` | 否 | 评论列表（分页200，按时间升序） |
| POST | `/publish` | JWT | 发布评论（支持 @username 提及） |
| POST | `/delete` | JWT | 删除评论 |

### 关注 `/social`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/follow` | JWT | 关注 |
| POST | `/unfollow` | JWT | 取关 |
| POST | `/getAllFollowers` | JWT | 粉丝列表（含粉丝数） |
| POST | `/getAllVloggers` | JWT | 关注列表（含关注数） |
| POST | `/getCounts` | JWT | 粉丝/关注计数 |

### Feed `/feed`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/listLatest` | 软鉴权 | 最新视频（游标分页） |
| POST | `/listLikesCount` | 软鉴权 | 点赞排行（复合游标） |
| POST | `/listByPopularity` | 软鉴权 | 热度榜（快照分页） |
| POST | `/listByFollowing` | JWT | 关注流 |
| POST | `/listByTag` | 软鉴权 | 按 #话题 浏览 |

### 通知 `/notification`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/stream?token=<access_token>` | 是 | SSE 实时推送，也支持 `Authorization: Bearer <token>`；多副本部署下经 Redis Pub/Sub 扇出 |
| POST | `/list` | 是 | 最近 50 条通知 |
| POST | `/markRead` | 是 | 标记已读；传 `id` 标记单条，省略 `id` 标记全部 |
| POST | `/unreadCount` | 是 | 未读计数 |

### 私信 `/message`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/send` | JWT | 发送私信 |
| POST | `/list` | JWT | 对话列表 |

## AI 能力（可选）

AI 是可选层，默认关闭。`ai.enabled=false` 时不产生任何模型调用、embedding 调用
与向量读写，系统行为与引入 AI 之前一致。模型能力放在一个**可独立部署、可整体删除**
的 Node sidecar 里，父进程只通过 gRPC（`ModelGateway`）访问它，没有任何服务
`depends_on` 它——停掉 sidecar 不影响主链路。

| 能力 | 说明 |
|---|---|
| 语义标签与摘要 | 发布后异步投递分析事件 → worker 调模型 → 结构化校验 → 幂等入库 |
| 语义召回与精排 | 向量回填 → 语义召回 → 配额融合 → LLM 精排（默认关）→ 推荐理由 |
| 内容审核 / 运营 Agent | 预留，可复用同一条流水线 |

| 开关 | 关掉之后 |
|---|---|
| `ai.enabled=false` | 零模型调用、零 embedding 调用、零向量读写 |
| `ai.recall_quota: 0` | 不召回、不算相似度，结果与基准线一致 |
| `ai.rerank_enabled: false` | 退回融合分排序 |
| `ai.explore_quota: 0` | 不做冷启动兜底 |

开关的缺省值一律是 0 / false；配置里的 0.3 / 0.1 / 20 是设计取值，不是默认值
——一份没改过的老配置不会凭空多出一条会外呼的链路。

`/feed/listLatest` 按**槽位配额**融合时序、语义召回与冷启动探索三路，再可选过一层
LLM 精排。配额是槽位分配而不是分数加权，因此有一条可验证的性质：`recall_quota=0`
且 `explore_quota=0` 时，输出与引入语义召回之前逐字节一致。

**降级**：sidecar 未启动、模型超时、返回非法 JSON、向量不可用，全部静默退回原有
行为，只留一条分级正确的日志。

**向量生成**：worker 每 30 秒回填一轮，按标题+描述的指纹（`content_hash`）判断是否
需要重算；模型名以提供方回报的为准，零向量不入库、维度不符整批丢弃。`reason` 字段
只允许三种真实信号（命中的兴趣标签、与近期点赞内容相似、新发布），没有信号就返回空。

**离线评测**：`backend/testdata/eval/` 提供评测工具与一条可复现的生成命令。
评测集与基线报告都是**产物**，不入库——克隆下来的仓库不含任何评测数据：

```bash
cd backend
./testdata/eval/regenerate.sh synthetic   # 合成集；不需要数据库、不需要 API key
```

按用户口径 NDCG@20（500 视频 / 60 用户，本机跑上面这条命令得到）：

| 排序策略 | NDCG@20 | 相对基线 |
|---|---|---|
| `popularity`（热度排序） | 0.120 | — |
| `p2_semantic_only`（只加语义召回） | 0.162 | **+35%** |
| `p2_explore_only`（只加冷启动探索） | 0.108 | −10% |
| `p2_fused`（语义 + 探索） | 0.150 | **+25%** |

三条限制必须与数字一起读：标签由合成器按规则生成，**不能**当作线上数字；
离线用词法向量（与 sidecar 的 `faux-lexical` 逐位一致），只捕捉字面重叠，
所以这是效果**下界**；探索配额在合成数据上是负收益，它买的是"新内容不会永不曝光"，
不是相关性。

精排默认关闭：真实 provider 的 P99 未实测，把未知延迟放进用户请求路径与
"不让用户请求等待模型"冲突。只打开开关而不调 `ai.rerank_timeout` 会立刻看到
100% 降级率（Warn 日志 + `degraded_total`），而不是一个悄悄变慢的 Feed。

**已知限制**：语义位可能让游标分页漏掉一条基础视频（推荐位与游标翻页未解耦）；
召回池只有最近 500 条有向量的视频，超出窗口的内容结构上不可能被捞到。

## 环境变量

环境变量**只做覆盖**：优先取环境变量，没设就用配置文件里的值；配置文件也不存在时才用下面的内置默认值。

| 变量 | 内置默认值 | 说明 |
|------|------------|------|
| `JWT_SECRET` | **必填，无默认值** | JWT 签名密钥；API 未设置时启动即失败（Worker 不使用 JWT，不需要它） |
| `SERVER_PORT` | `8080` | 后端监听端口 |
| `MYSQL_HOST` | `localhost` | MySQL 地址 |
| `MYSQL_PORT` | `3306` | MySQL 端口（compose 映射到宿主机 `3307`） |
| `MYSQL_USER` | `root` | MySQL 账号 |
| `MYSQL_PASSWORD` | **无默认值** | MySQL 密码；优先级高于 `MYSQL_ROOT_PASSWORD` |
| `MYSQL_ROOT_PASSWORD` | **无默认值** | MySQL root 密码（compose / `.env.example` 里是 `123456`） |
| `MYSQL_DATABASE` | `feedflow` | MySQL 数据库名 |
| `REDIS_HOST` | `localhost` | Redis 地址 |
| `REDIS_PORT` | `6379` | Redis 端口 |
| `REDIS_PASSWORD` | **无默认值** | Redis 密码（compose / `.env.example` 里是 `123456`） |
| `REDIS_DB` | `0` | Redis DB |
| `RABBITMQ_HOST` | `localhost` | RabbitMQ 地址 |
| `RABBITMQ_PORT` | `5672` | RabbitMQ 端口 |
| `RABBITMQ_USER` | `admin` | RabbitMQ 账号 |
| `RABBITMQ_PASS` | **无默认值** | RabbitMQ 密码（compose / `.env.example` 里是 `password123`） |

**密码类变量刻意没有内置默认值。** 这是一条安全取舍：`CONFIG_PATH` 打错一个字母时，
进程应该 fail fast 报出缺了哪一项，而不是拿一个硬编码密码去连本机数据库——
后者会让"配置写错了"表现成"连上了但连的是别的库"。因此裸跑（既没有配置文件、
也没有密码环境变量）会直接启动失败，这是预期行为。

值无法解析的环境变量（例如 `MYSQL_PORT=abc`）会被忽略并保留配置文件里的值，
同时打一条带变量名的 `WARN`，不会静默吞掉。

详见 `.env.example`。

JWT 有效期可在配置文件中覆盖：`jwt.access_token_ttl`（默认 `15m`）、`jwt.refresh_token_ttl`（默认 `168h`）。
注意必须写成 `15m` / `168h` 这样的时长字符串，写成整数会导致配置解析失败。
配置缺失必填项时进程会 fail fast 并打印具体缺失路径（不再静默使用内置密码）。

日志可在配置文件中调整，三项都可选，留空即用默认值：

```yaml
log:
  level: info    # debug | info | warn | error，默认 info
  format: json   # json | console，默认 console
  output: stderr # stdout | stderr，默认 stderr
```

容器里看结构化日志建议把 `format` 设为 `json`；取值非法时进程启动即报错并列出允许值。

注意：**日志配置生效之前的少数几行仍是 console 格式**（例如 `.env not found`、
`loading config`）——配置得先读出来才知道日志该怎么输出。想把这几行也变成 JSON，
需要给日志设置单独的引导环境变量，当前没有这么做。

## 运维与可观测性

- 健康检查三件套：`/healthz` 固定返回 ok 且不查依赖；`/livez` 是进程存活探测，
  compose 的 api healthcheck 用它做重启判据；`/readyz` 聚合 MySQL / Redis / RabbitMQ，
  MySQL 不可用返回 `503`，Redis 或 RabbitMQ 不可用返回 `200` 且 `degraded: true`
  （二者在本项目中是可降级依赖，返回 503 会让编排层反复重启本来能工作的实例）。
- **结构化日志**：zap 输出，5xx 记 `ERROR`、4xx 记 `WARN`、其余 `INFO`；每个请求带一个
  请求 id，同时写入响应头 `X-Request-ID` 与日志字段 `request_id`，可以拿客户端看到的
  那个值直接去日志里捞整条链路（客户端自带的 id 会经字符白名单与长度清洗后透传）。
  GORM 的 SQL 日志走同一套输出，慢查询按 `WARN` 记录。
- 本地配置默认开启 pprof：API `localhost:6060`，Worker `localhost:6061`。
  上传文件写入 `backend/.run/uploads`，Docker 环境挂载到 `backend_uploads` volume。
- Redis 用于 Token 缓存、视频实体缓存、Feed 时间线、热榜窗口、分片上传会话，
  以及多副本部署时的通知广播频道 `v1:sse:notify`。
- **全局时间线会自愈**：`/feed/listLatest` 会核对"数据库里最新的一批视频是否都在
  时间线 ZSET 里"，发现缺人就整批补写并记一条 INFO（探测结论按时间线头部成员缓存
  并走 singleflight，稳态下一次库都不打）。日志里这条信息**持续出现**才说明消费者真的挂了。
- RabbitMQ Topic Exchange 覆盖点赞、评论、关注、热度、视频时间线事件，并配置 DLX。
- **多副本语义**：通知的消费者队列在副本间是竞争消费，而 SSE 连接由每个副本各自持有，
  因此推送经 Redis Pub/Sub 广播，每个副本转发给自己持有的连接。多副本部署时**必须共用
  同一个 Redis**，否则连接在 A 副本上的用户收不到落在 B 副本上的推送（通知本身仍会落库，
  客户端轮询 `/notification/list` 可以补齐）。
- **前端 nginx 按请求解析后端地址**（`frontend/nginx.conf` 用变量 + Docker 内嵌 DNS，
  缓存 10 秒），因此重建后端容器后前端最多 10 秒内自动跟上，不需要重启前端。
  若出现「后端 healthy、前端接口全部 502」，先看前端容器日志里 nginx 报的上游地址。
