package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// 本包 import internal/ai 只为它的**默认值常量**（模型名、维度、超时）。
//
// 为什么不去复制一份：模型名与维度是三处必须同时成立的约定
// （proto 注释、向量表里记录的 model/dim、以及这里的默认值），
// 复制一份就一定会有一处先漂移，而漂移的后果是"库里的向量与请求的模型
// 不是同一个"，只在检索质量上体现。方向是单向的：internal/ai
// **不** import 本包（见该包顶部的注释），因此不存在循环依赖。

// JWT 的默认有效期。配置文件里可以覆盖（注意 yaml 里必须写成 "15m" 这样的
// 时长字符串，写成整数会导致解析失败）。
const (
	defaultAccessTokenTTL  = 15 * time.Minute
	defaultRefreshTokenTTL = 168 * time.Hour
)

// AI 的默认值，只在 ai 段存在、但字段没写时生效。
//
// 注意 defaultAITimeout 的取值理由：AI 调用是**旁路**，它的超时必须短到来不及
// 拖慢任何一个用户请求。2 秒是"模型偶尔慢一点还能出结果、真慢了立刻放弃"的折中；
// 每个消费方还会在此基础上用 context.WithTimeout 再夹一层。
const (
	defaultAITimeout = 2 * time.Second
	defaultAIModel   = "gpt-4o-mini"
)

type Config struct {
	Server              ServerConfig        `yaml:"server"`
	Database            DatabaseConfig      `yaml:"database"`
	Redis               RedisConfig         `yaml:"redis"`
	RabbitMQ            RabbitMQConfig      `yaml:"rabbitmq"`
	JWT                 JWTConfig           `yaml:"jwt"`
	Log                 LogConfig           `yaml:"log"`
	ObservabilityConfig ObservabilityConfig `yaml:"observability"`
	AI                  AIConfig            `yaml:"ai"`
}

// AIConfig 是 AI 能力的**统一总开关与连接参数**。
//
// 设计要点（每一条都是硬约束，改之前先看 docs/AI-00-总览与全局约束.md）：
//
//  1. Enabled 默认 false。缺省即"AI 全关"——老配置文件里没有 ai 段时，
//     加载出来的就是零值 AIConfig，行为必须与引入 AI 之前逐字节一致。
//  2. 本结构体的任何字段都**不得**加入 ValidateFor 的必填校验。
//     原因：ValidateFor 失败会让进程启动失败，把可选功能的配置缺失升级成
//     "服务起不来"，这是拿承重墙给锦上添花的功能陪葬。
//  3. GatewayAddr 为空时视为"未配置"，等同于关闭；不会去连一个空地址。
//
// 字段单位统一写进注释，避免 yaml 里写成整数导致时长解析失败（同 JWT.TTL 的坑）。
type AIConfig struct {
	// Enabled 是总开关。false 时不得有任何模型调用、embedding 调用、向量读写。
	// 消费方必须通过 ai.NewClient 注入 noop 实现，而不是"每个调用点自己 if 一下"——
	// 分散的判断迟早会漏掉一处，而"漏掉一处"正是这里最不能接受的失败方式。
	Enabled     bool          `yaml:"enabled"`
	GatewayAddr string        `yaml:"gateway_addr"` // sidecar 的 gRPC 地址，如 sidecar:50051
	Timeout     time.Duration `yaml:"timeout"`      // 单次调用超时，如 "2s"；默认 2s
	Model       string        `yaml:"model"`        // 请求的模型名，透传给 sidecar
	MaxTokens   int           `yaml:"max_tokens"`   // 单次生成上限，0 表示由 sidecar 决定
	// DailyBudget 是预留占位：当前只被日志与 sidecar 计量记录使用，不做硬拦截。
	// 保留字段是为了让配置格式在 P1~P4 期间保持稳定（配置也是对外契约的一部分）。
	DailyBudget float64 `yaml:"daily_budget"`

	// TagMinConfidence 是 P1 采纳 AI 标签的置信度下限（0~1，可选）。
	// 0 或越界时由消费方退回 video.DefaultMinConfidence。
	//
	// 为什么做成可配置：阈值决定"标签多而杂"还是"少而准"，
	// 这属于运营口味而不是工程常量。改它只需要改配置，不必重新发版。
	TagMinConfidence float64 `yaml:"tag_min_confidence"`

	// ---- embedding 通道（P2 用；与 chat 的超时/成本语义完全分开）----
	//
	// 它们全部可选，且任何一项缺失都不会让进程起不来（红线 4）。
	// 一个都没配时的行为 = "向量通道不可用"，语义路静默关闭。

	// EmbeddingModel 是向量化用的模型名。换它必须同时重算全部已入库向量
	// （表里记录了 model/dim，就是为了让这件事可检测而不是靠记忆）。
	EmbeddingModel string `yaml:"embedding_model"`
	// EmbeddingDim 是期望的向量维度。<=0 时由消费方退回 ai.DefaultEmbeddingDim。
	//
	// 声明出来的用途是**校验**：sidecar 返回的维度与它不一致时立刻失败，
	// 而不是把一批维度不对的向量写进库（那种数据只在检索时暴露，
	// 表现为"模型效果差"，几乎不可能反查到是维度问题）。
	EmbeddingDim int `yaml:"embedding_dim"`
	// EmbeddingTimeout 是单次向量调用的兜底超时，如 "3s"。
	EmbeddingTimeout time.Duration `yaml:"embedding_timeout"`
	// EmbeddingNormalize 指向 bool 而不是 bool：yaml 与 proto3 一样，
	// 布尔字段没有"未指定"状态，用普通 bool 时"没写"与"写了 false"无法区分，
	// 而这两者的期望行为完全相反（默认必须是 true，见 ai.EmbeddingRequest 的注释）。
	// 用指针是这里唯一能如实表达"未指定"的写法。
	EmbeddingNormalize *bool `yaml:"embedding_normalize"`

	// ---- P2 语义召回与精排的四个设计参数（B4 / D1~D4）----
	//
	// 这四个字段**在 P2 前置阶段定义、由 P2 的 feed 包消费**。
	// 之所以现在就落成配置而不是只写在文档里：
	//   - D3 明确要求"配额设 0 → 结果与今天逐字节一致"必须是一个**可执行的开关**，
	//     写在文档里的配额设不了 0；
	//   - 取值一旦定下来就不该由实现随手改（它们是成本与延迟的边界），
	//     配置项是唯一能让"改了要 review"这件事成立的地方。
	//
	// 全部可选、全部有安全默认值，且都不进 ValidateFor（红线 4）。

	// RecallQuota 是语义召回在最终列表里的目标占比（0~1）。
	//
	// **缺省 0 = 语义路完全关闭**，这是刻意的：红线 4 要求新增配置项的
	// 缺省必须落在"功能关闭"上，否则一份没改过的老配置会凭空多出一条
	// 会外呼、会改排序的链路。要开启就显式写 `recall_quota: 0.3`
	// （随仓库发布的配置文件里就是这么写的）。
	//
	// 设计取值 0.3 的依据见 AI-P2-设计决定.md：既有排序仍是主体，
	// 语义路只补"老但相关"的那一部分；占比过高会让 Feed 失去新鲜度。
	//
	// 设为 0 时结果与引入 P2 之前**逐字节一致**——这是 P2 的降级等价性
	// 验收项，也是线上出问题时的第一个手段。
	RecallQuota float64 `yaml:"recall_quota"`

	// ExploreQuota 是留给"新内容"的探索配额（0~1）。缺省 0 = 不做探索。
	//
	// 为什么必须有：新视频没有向量、没有交互，纯按相关性排序永远进不了
	// 召回池——这是这类系统最常见的线上事故（内容发出来没人看得到，
	// 创作者流失）。设计取值 0.1：每 10 条里留 1 条给"最近发布且互动很少"的视频。
	ExploreQuota float64 `yaml:"explore_quota"`

	// RerankEnabled 是 LLM 精排的开关（默认 **false**）。
	//
	// 默认关的理由是**没实测过**，不是它不重要：真实模型的 P50/P99 必须在
	// 配了 key 的环境里实测之后才能决定要不要开（步骤见 AI-P2-设计决定.md）。
	// 在实测之前把它打开，等于把一个未知大小的延迟直接放进用户请求路径。
	RerankEnabled bool `yaml:"rerank_enabled"`
	// RerankTimeout 是 LLM 精排的硬超时，超时立刻退回融合分。
	//
	// 它与 RerankCandidates 必须自洽：给模型 20 条候选、120ms 预算，
	// 在真实 provider 上大概率超时——那种配置等于"开了但永远走降级"。
	// 这正是它默认关闭的原因。取值依据见 AI-P2-设计决定.md 的 D1。
	RerankTimeout time.Duration `yaml:"rerank_timeout"`
	// RerankCandidates 是送进 LLM 精排的候选条数上限。
	//
	// 它直接决定 prompt 大小（成本）与模型耗时。20 条标题+摘要约 500~700
	// prompt token，是"还可能在秒级答完"的量级；100 条会先把上下文塞满、
	// 再让延迟翻几倍。取值依据见 AI-P2-设计决定.md 的 D4。
	RerankCandidates int `yaml:"rerank_candidates"`
}

// 精排参数的默认值（P2 前置 D1/D4）。
//
// 注意两个**配额**没有在这里给默认值：它们的缺省必须是 0（= 关闭），
// 设计取值写在随仓库发布的配置文件里。原因见 RecallQuota 的注释——
// "新增配置项的缺省 = 功能关闭"是红线，而"设计上推荐 0.3"是另一件事。
const (
	defaultRerankTimeout    = 120 * time.Millisecond
	defaultRerankCandidates = 20
)

// RecallQuotaOr 返回生效的语义路配额。
//
// 越界值（负数或 >1）一律按 0 处理，而不是退回一个非零默认值：
// 越界时唯一安全的方向是"关掉"，而"按 0.3 跑"会让一个手滑的 -1
// 变成一条谁也没打算开启的外呼链路。
// 0 本身是合法取值（它的语义就是"关"），因此这里判的是范围而不是零值。
func (a AIConfig) RecallQuotaOr() float64 {
	if a.RecallQuota < 0 || a.RecallQuota > 1 {
		return 0
	}
	return a.RecallQuota
}

// ExploreQuotaOr 返回生效的探索配额。越界值同样按 0 处理（安全方向）。
func (a AIConfig) ExploreQuotaOr() float64 {
	if a.ExploreQuota < 0 || a.ExploreQuota > 1 {
		return 0
	}
	return a.ExploreQuota
}

// RerankTimeoutOr 返回生效的精排硬超时。
//
// 上限夹到 2 秒：精排跑在**用户请求路径**上，任何超过 2 秒的取值都意味着
// "用户已经在等模型了"，那与"不得让用户请求等待模型"这条红线直接冲突。
// 夹住而不是报错：配置写错时应当退到一个安全值并让运维从日志里看到，
// 而不是让服务起不来（红线 4）。
func (a AIConfig) RerankTimeoutOr() time.Duration {
	if a.RerankTimeout <= 0 {
		return defaultRerankTimeout
	}
	if a.RerankTimeout > 2*time.Second {
		return 2 * time.Second
	}
	return a.RerankTimeout
}

// RerankCandidatesOr 返回生效的精排候选数上限。
//
// 上限夹到 64：候选数直接乘上 prompt 大小，一个手滑多写的 0 会让
// 单次请求的成本涨十倍，而"成本"这类问题只在账单上暴露。
func (a AIConfig) RerankCandidatesOr() int {
	if a.RerankCandidates <= 0 {
		return defaultRerankCandidates
	}
	if a.RerankCandidates > 64 {
		return 64
	}
	return a.RerankCandidates
}

// EmbeddingDimOr 返回生效的期望维度；未配置时退回默认值。
func (a AIConfig) EmbeddingDimOr() int {
	if a.EmbeddingDim <= 0 {
		return ai.DefaultEmbeddingDim
	}
	return a.EmbeddingDim
}

// EmbeddingModelOr 返回生效的向量模型名。
func (a AIConfig) EmbeddingModelOr() string {
	if strings.TrimSpace(a.EmbeddingModel) == "" {
		return ai.DefaultEmbeddingModel
	}
	return strings.TrimSpace(a.EmbeddingModel)
}

// EmbeddingTimeoutOr 返回生效的向量调用超时。
func (a AIConfig) EmbeddingTimeoutOr() time.Duration {
	if a.EmbeddingTimeout <= 0 {
		return ai.DefaultEmbeddingTimeout
	}
	return a.EmbeddingTimeout
}

// EmbeddingNormalizeOr 返回是否要求归一化向量；未配置时**默认 true**。
//
// 默认 true 的理由：没归一化的向量在余弦检索里会让更长的文本系统性占优，
// 而这种偏差在结果里看起来像"模型偏好长内容"，几乎不可能反查到配置问题。
func (a AIConfig) EmbeddingNormalizeOr() bool {
	if a.EmbeddingNormalize == nil {
		return true
	}
	return *a.EmbeddingNormalize
}

// TimeoutOr 返回可用的调用超时：配置为 0 或负数时退回默认值。
//
// 为什么不在 applyDefaults 里统一填：sidecar 侧（Node）读的是同一份 yaml，
// 它按自己的方式取默认值；Go 这边把它做成"取值时兜底"而不是"加载时改写"，
// 这样直接构造 Config{} 的单测也拿得到安全值，不会出现 0 超时 = 立即超时的坑。
func (a AIConfig) TimeoutOr() time.Duration {
	if a.Timeout <= 0 {
		return defaultAITimeout
	}
	return a.Timeout
}

// LogConfig 决定日志的级别、格式与输出位置，三项全部可选：
// 留空即 level=info、format=console、output=stderr。
//
// 刻意不纳入 Validate 的必填校验：它们都有安全的默认值，
// 日志配置不该成为进程起不来的原因。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | console
	Output string `yaml:"output"` // stdout | stderr
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

type RedisConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type RabbitMQConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// JWTConfig 承载 JWT 密钥与双 token 的有效期。
// Secret 留空会被 Validate 拦截；TTL 为 0 时由 applyDefaults 填默认值。
type JWTConfig struct {
	Secret          string        `yaml:"secret"`
	AccessTokenTTL  time.Duration `yaml:"access_token_ttl"`  // 默认 15m
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl"` // 默认 168h（7 天）
}

type ObservabilityConfig struct {
	Pprof PprofConfig `yaml:"pprof"`
}
type PprofConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ApiAddr    string `yaml:"api_addr"`
	WorkerAddr string `yaml:"worker_addr"`
}

// Load 按 API 角色加载配置，等价于 LoadFor(filename, RoleAPI)。
func Load(filename string) (Config, error) {
	return LoadFor(filename, RoleAPI)
}

// LoadFor 按进程角色加载并校验配置。
func LoadFor(filename string, role Role) (Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", filename, err)
	}

	cfg.applyDefaults()
	warnIgnoredEnvVars(ApplyEnvOverrides(&cfg))
	if err := cfg.ValidateFor(role); err != nil {
		return Config{}, fmt.Errorf("配置 %s 非法: %w", filename, err)
	}
	return cfg, nil
}

// applyDefaults 填充"可选但有默认值"的字段。
// 必须在 Unmarshal 之后调用：配置文件里没写 jwt.access_token_ttl 时
// yaml 会留 0，若直接使用会让签出的 token 立即过期。
func (c *Config) applyDefaults() {
	if c.JWT.AccessTokenTTL == 0 {
		c.JWT.AccessTokenTTL = defaultAccessTokenTTL
	}
	if c.JWT.RefreshTokenTTL == 0 {
		c.JWT.RefreshTokenTTL = defaultRefreshTokenTTL
	}
}

// Role 标识加载配置的进程类型。不同进程对配置项的需求不同
// （例如只有 API 需要 jwt.secret，worker 完全不用 JWT）。
type Role int

const (
	RoleAPI Role = iota
	RoleWorker
)

// Validate 检查配置的必填项，等价于 ValidateFor(RoleAPI)。
// 返回的错误必须包含出错的配置路径，便于运维直接定位。
func (c Config) Validate() error {
	return c.ValidateFor(RoleAPI)
}

// ValidateFor 按进程角色校验配置。
// 通用必填项（server/database/redis/rabbitmq）两个角色都查；
// jwt.* 只有 API 需要——worker 只消费 MQ、写 MySQL、更新 Redis 热度，
// 不使用 JWT，不应被一个自己用不到的配置项挡在门外。
func (c Config) ValidateFor(role Role) error {
	var errs []string

	// ---- 通用必填项（API 与 worker 都需要）----
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, "server.port 必须在 1-65535")
	}
	if c.Database.Host == "" {
		errs = append(errs, "database.host 不能为空")
	}
	if c.Database.Port <= 0 {
		errs = append(errs, "database.port 必须为正整数")
	}
	if c.Database.User == "" {
		errs = append(errs, "database.user 不能为空")
	}
	if c.Database.DBName == "" {
		errs = append(errs, "database.dbname 不能为空")
	}
	if c.Database.Password == "" {
		errs = append(errs, "database.password 不能为空")
	}
	if c.Redis.Host == "" {
		errs = append(errs, "redis.host 不能为空")
	}
	if c.RabbitMQ.Host == "" {
		errs = append(errs, "rabbitmq.host 不能为空")
	}
	if c.RabbitMQ.Username == "" {
		errs = append(errs, "rabbitmq.username 不能为空")
	}

	// ---- 仅 API 需要 ----
	if role == RoleAPI {
		if c.JWT.Secret == "" {
			errs = append(errs, "jwt.secret 不能为空（必须显式设置，不再允许随机生成）")
		}
		if c.JWT.AccessTokenTTL <= 0 {
			errs = append(errs, "jwt.access_token_ttl 必须为正的时长（如 15m）")
		}
		if c.JWT.RefreshTokenTTL <= 0 {
			errs = append(errs, "jwt.refresh_token_ttl 必须为正的时长（如 168h）")
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ApplyEnvOverrides 用环境变量覆盖配置，返回"值非法被忽略"的变量描述列表
// （例如 MYSQL_PORT=abc），由调用方打印警告，不再静默吞掉。
func ApplyEnvOverrides(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var ignored []string
	atoi := func(name, v string, dst *int) {
		port, err := strconv.Atoi(v)
		if err != nil {
			ignored = append(ignored, fmt.Sprintf("%s=%s", name, v))
			return
		}
		*dst = port
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		atoi("SERVER_PORT", v, &cfg.Server.Port)
	}
	if v := os.Getenv("MYSQL_HOST"); v != "" {
		cfg.Database.Host = v
	}
	if v := os.Getenv("MYSQL_PORT"); v != "" {
		atoi("MYSQL_PORT", v, &cfg.Database.Port)
	}
	if v := os.Getenv("MYSQL_USER"); v != "" {
		cfg.Database.User = v
	}
	if v := os.Getenv("MYSQL_ROOT_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("MYSQL_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("MYSQL_DATABASE"); v != "" {
		cfg.Database.DBName = v
	}
	if v := os.Getenv("REDIS_HOST"); v != "" {
		cfg.Redis.Host = v
	}
	if v := os.Getenv("REDIS_PORT"); v != "" {
		atoi("REDIS_PORT", v, &cfg.Redis.Port)
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		atoi("REDIS_DB", v, &cfg.Redis.DB)
	}
	if v := os.Getenv("RABBITMQ_HOST"); v != "" {
		cfg.RabbitMQ.Host = v
	}
	if v := os.Getenv("RABBITMQ_PORT"); v != "" {
		atoi("RABBITMQ_PORT", v, &cfg.RabbitMQ.Port)
	}
	if v := os.Getenv("RABBITMQ_USER"); v != "" {
		cfg.RabbitMQ.Username = v
	}
	if v := os.Getenv("RABBITMQ_PASS"); v != "" {
		cfg.RabbitMQ.Password = v
	}
	// JWT_SECRET 一直由部署环境注入（docker-compose.yml 的 api/worker 服务），
	// 这里映射进配置，使 Validate 能统一校验。
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JWT.Secret = v
	}
	// AI_ENABLED 是 AI 总开关的部署侧入口：让它能被环境变量覆盖，
	// 是为了"同一份镜像在不同环境开关 AI"不需要改配置文件。
	//
	// 取值集合必须与 sidecar/src/config.ts 的 parseBool **完全一致**，
	// 否则会出现最危险的一种状态：运维敲了 AI_ENABLED=off，sidecar 关了，
	// 而 Go 侧认为"值非法、保留配置里的 true"，于是模型调用继续发生，
	// 只是多了一条 Warn。总开关的全部意义就是"关得掉"，这里不能有分歧。
	if v := os.Getenv("AI_DEFAULT_MODEL"); v != "" {
		// 模型名的部署侧入口。存在的理由与 AI_ENABLED 相同：
		// 换模型属于运维动作，不该要求改配置文件并重启所有进程。
		// 它也顺带解决了"演示/联调环境要用另一个模型名"的场景。
		cfg.AI.Model = v
	}
	if v := os.Getenv("AI_ENABLED"); v != "" {
		enabled, ok := parseBoolLoose(v)
		if !ok {
			ignored = append(ignored, fmt.Sprintf("AI_ENABLED=%s", v))
		} else {
			cfg.AI.Enabled = enabled
		}
	}
	// embedding 的模型名与维度同样允许从环境变量覆盖：
	// 它们的取值必须与 sidecar 的 AI_EMBEDDING_MODEL / AI_EMBEDDING_DIM
	// 完全一致（一边换了一边没换时，维度校验会立刻失败——这是刻意设计的
	// 快速失败，而不是让两种维度的向量混进同一张表）。
	if v := os.Getenv("AI_EMBEDDING_MODEL"); v != "" {
		cfg.AI.EmbeddingModel = v
	}
	if v := os.Getenv("AI_EMBEDDING_DIM"); v != "" {
		atoi("AI_EMBEDDING_DIM", v, &cfg.AI.EmbeddingDim)
	}
	return ignored
}

// parseBoolLoose 解析"人写的布尔值"：接受 1/0、t/f、true/false、yes/no、on/off
// （大小写与首尾空白宽松），其他取值返回 ok=false。
//
// 为什么不用 strconv.ParseBool：它只认 1/0/t/f/true/false，会把运维最常用的
// yes/no/on/off 判成非法。这个函数必须与 sidecar/src/config.ts 的 parseBool
// 保持同一套取值，两边各写一套正是 review 抓到的分歧来源。
func parseBoolLoose(raw string) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "yes", "on":
		return true, true
	case "0", "f", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func warnIgnoredEnvVars(ignored []string) {
	for _, v := range ignored {
		// 配置装载期无 ctx；环境变量写错却被静默忽略是运维事故的常见来源，故为 Warn。
		logging.L().Warn("忽略非法环境变量（值无法解析，保留配置文件中的值）", zap.String("env", v))
	}
}

// bool用来表示是否使用了默认配置，true表示使用了默认配置
func LoadLocalDev(filename string) (Config, bool, error) {
	return loadLocalDev(filename, RoleAPI)
}

// LoadLocalDevForWorker 与 LoadLocalDev 相同，但校验时跳过仅 API 需要的配置项
// （jwt.*）——worker 不使用 JWT。
func LoadLocalDevForWorker(filename string) (Config, bool, error) {
	return loadLocalDev(filename, RoleWorker)
}

func loadLocalDev(filename string, role Role) (Config, bool, error) {
	cfg, err := LoadFor(filename, role)
	if err == nil {
		return cfg, false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		cfg := DefaultLocalConfig() // 默认值里已无密码，缺失项由 ValidateFor 拦截
		if err := cfg.ValidateFor(role); err != nil {
			return Config{}, true, fmt.Errorf(
				"配置文件 %s 不存在，且内置默认值不完整: %w", filename, err)
		}
		return cfg, true, nil
	}
	return Config{}, false, err
}

func DefaultLocalConfig() Config {
	cfg := Config{
		Server: ServerConfig{
			Port: 8080,
		},
		Database: DatabaseConfig{
			Host: "localhost",
			Port: 3306,
			User: "root",
			// 不内置密码：CONFIG_PATH 打错一个字母时不应该用硬编码密码连上本机库，
			// 而是由 Validate 报错 fail fast（可用 MYSQL_ROOT_PASSWORD 等环境变量提供）。
			Password: "",
			DBName:   "feedflow",
		},
		Redis: RedisConfig{
			Host:     "localhost",
			Port:     6379,
			Password: "",
			DB:       0,
		},
		RabbitMQ: RabbitMQConfig{
			Host:     "localhost",
			Port:     5672,
			Username: "admin",
			Password: "",
		},
		ObservabilityConfig: ObservabilityConfig{
			Pprof: PprofConfig{
				Enabled:    true,
				ApiAddr:    "localhost:6060",
				WorkerAddr: "localhost:6061",
			},
		},
	}
	cfg.applyDefaults()
	warnIgnoredEnvVars(ApplyEnvOverrides(&cfg))
	return cfg
}
