package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/ai"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// 本文件是 internal/config 的第一批测试。
//
// 为什么它值得单独补：config 是全项目**唯一连续出过两次回归**的模块，而且两次
// 都不是写错逻辑，是"改动别处时把它带崩了"：
//
//  1. 9559766 引入配置校验与 fail-fast 之后，worker 因为共用的 Validate() 要求
//     jwt.secret 非空而起不来（worker 根本不用 JWT），由 752147b 修掉。
//  2. 同一批改动把 JWT_SECRET 变成必填项，而 start.sh 当时只导出 CONFIG_PATH，
//     于是本地启动与 README 一起失效。
//
// 两次的共同点是：配置项的**角色差异**与**缺失时的行为**没有任何东西在守。
// 所以下面的用例重点不在"字段能不能解析"，而在这些容易再次踩坏的地方：
//
//   - 哪些配置项只有 API 需要、哪些两个角色都要（角色矩阵）
//   - 配置文件缺失时到底是 fail fast 还是静默降级（默认值里已无密码）
//   - 非法环境变量是被静默忽略还是留下警告
//   - 出错的配置项有没有被逐条报出来（运维要能直接定位）
//   - 仓库里实际发布的那几个 yaml 是否真的能加载
//
// 最后一条是"配置漂移"防线：改配置结构却忘了同步 yaml 的话，这里会红。

// overrideEnvVars 是 ApplyEnvOverrides 会读取的全部环境变量。
//
// 每个用例都要先把它们清空：这些变量在真实环境里经常是设置了的
// （docker-compose 与 start.sh 都会注入其中一部分），否则测试结果会取决于
// 跑测试时 shell 里恰好有什么，变成一类很难查的假绿/假红。
var overrideEnvVars = []string{
	"SERVER_PORT",
	"MYSQL_HOST", "MYSQL_PORT", "MYSQL_USER",
	"MYSQL_ROOT_PASSWORD", "MYSQL_PASSWORD", "MYSQL_DATABASE",
	"REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD", "REDIS_DB",
	"RABBITMQ_HOST", "RABBITMQ_PORT", "RABBITMQ_USER", "RABBITMQ_PASS",
	"JWT_SECRET",
	"AI_ENABLED",
	"AI_DEFAULT_MODEL",
}

// isolateEnv 把所有相关环境变量清空。t.Setenv 会在用例结束后自动还原。
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range overrideEnvVars {
		t.Setenv(name, "")
	}
}

// baseConfig 返回一份所有必填项齐全的配置，作为各项断言的基准。
func baseConfig() Config {
	return Config{
		Server:   ServerConfig{Port: 8080},
		Database: DatabaseConfig{Host: "localhost", Port: 3306, User: "root", Password: "pw", DBName: "feedflow"},
		Redis:    RedisConfig{Host: "localhost", Port: 6379, Password: "pw", DB: 0},
		RabbitMQ: RabbitMQConfig{Host: "localhost", Port: 5672, Username: "admin", Password: "pw"},
		JWT:      JWTConfig{Secret: "test-secret", AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour},
	}
}

// fileConfigYAML 是一份"除 jwt 外都齐全"的配置，用来把 jwt 相关行为隔离出来。
// 刻意不含 jwt 段：仓库里实际发布的三个 yaml 也都不含，JWT_SECRET 由部署环境注入。
const fileConfigYAML = `
server:
  port: 8080
database:
  host: localhost
  port: 3306
  user: root
  password: file-pw
  dbname: feedflow
redis:
  host: localhost
  port: 6379
  password: file-pw
  db: 0
rabbitmq:
  host: localhost
  port: 5672
  username: admin
  password: file-pw
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错并包含 %q，实际 err == nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误信息里没有 %q：%v", want, err)
	}
}

// observeLogs 换入一个可观测 logger，并在用例结束时还原现场。
func observeLogs(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(level)
	prev := logging.SetLogger(zap.New(core))
	t.Cleanup(func() { logging.SetLogger(prev) })
	return logs
}

// ---------------------------------------------------------------------------
// 角色矩阵：哪些配置项只有 API 需要
// ---------------------------------------------------------------------------

// TestValidateForGatesJWTByRole 是 752147b 的回归测试。
//
// 当时的故障是：worker 完全不使用 JWT（cmd/worker/main.go 零处引用），却因为
// 与 API 共用 Validate() 而要求 jwt.secret 非空，直接起不来。修法是引入 Role，
// 把 jwt.* 的校验限制在 RoleAPI。
//
// 这里的每一格都在守那个矩阵：jwt 只对 API 生效，通用项对两个角色都生效。
// 只测"worker 能过"是不够的——如果实现变成"RoleWorker 跳过全部校验"，
// 那种改法同样能让 worker 起来，却会把真正的配置错误放进生产。
func TestValidateForGatesJWTByRole(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(c *Config)
		wantAPI  bool // true 表示 API 角色应当报错
		wantWkr  bool // true 表示 worker 角色应当报错
		contains string
	}{
		{
			name:    "缺 jwt.secret：只有 API 拦",
			mutate:  func(c *Config) { c.JWT.Secret = "" },
			wantAPI: true, wantWkr: false, contains: "jwt.secret",
		},
		{
			name:    "jwt.access_token_ttl 为零：只有 API 拦",
			mutate:  func(c *Config) { c.JWT.AccessTokenTTL = 0 },
			wantAPI: true, wantWkr: false, contains: "jwt.access_token_ttl",
		},
		{
			name:    "jwt.refresh_token_ttl 为负：只有 API 拦",
			mutate:  func(c *Config) { c.JWT.RefreshTokenTTL = -time.Hour },
			wantAPI: true, wantWkr: false, contains: "jwt.refresh_token_ttl",
		},
		{
			name:    "server.port 为 0：两个角色都拦",
			mutate:  func(c *Config) { c.Server.Port = 0 },
			wantAPI: true, wantWkr: true, contains: "server.port",
		},
		{
			name:    "server.port 超范围：两个角色都拦",
			mutate:  func(c *Config) { c.Server.Port = 70000 },
			wantAPI: true, wantWkr: true, contains: "server.port",
		},
		{
			name:    "database.host 为空：两个角色都拦",
			mutate:  func(c *Config) { c.Database.Host = "" },
			wantAPI: true, wantWkr: true, contains: "database.host",
		},
		{
			name:    "database.password 为空：两个角色都拦",
			mutate:  func(c *Config) { c.Database.Password = "" },
			wantAPI: true, wantWkr: true, contains: "database.password",
		},
		{
			name:    "redis.host 为空：两个角色都拦",
			mutate:  func(c *Config) { c.Redis.Host = "" },
			wantAPI: true, wantWkr: true, contains: "redis.host",
		},
		{
			name:    "rabbitmq.username 为空：两个角色都拦",
			mutate:  func(c *Config) { c.RabbitMQ.Username = "" },
			wantAPI: true, wantWkr: true, contains: "rabbitmq.username",
		},
		{
			name:    "全部齐全：两个角色都放行",
			mutate:  func(c *Config) {},
			wantAPI: false, wantWkr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(&cfg)

			errAPI := cfg.ValidateFor(RoleAPI)
			errWorker := cfg.ValidateFor(RoleWorker)

			if tc.wantAPI {
				requireErrContains(t, errAPI, tc.contains)
			} else if errAPI != nil {
				t.Errorf("API 角色不该报错，实际: %v", errAPI)
			}
			if tc.wantWkr {
				requireErrContains(t, errWorker, tc.contains)
			} else if errWorker != nil {
				t.Errorf("worker 角色不该报错，实际: %v", errWorker)
			}
		})
	}
}

// TestValidateEqualsAPIrole 钉住 Validate() == ValidateFor(RoleAPI) 这个契约。
//
// 这不是形式主义：752147b 的教训正是"Validate() 的调用点不只在 LoadLocalDev，
// Load 内部也校验"，所以当时只把 LoadLocalDev 角色化是不够的。Validate() 是
// 无参入口，一旦有人让它与 RoleAPI 分叉，就会重新出现"某条路径按 API 校验
// worker"的漏洞。
func TestValidateEqualsAPIrole(t *testing.T) {
	for _, cfg := range []Config{
		baseConfig(),
		func() Config { c := baseConfig(); c.JWT.Secret = ""; return c }(),
		func() Config { c := baseConfig(); c.Server.Port = 0; return c }(),
		Config{},
	} {
		viaValidate := cfg.Validate()
		viaRole := cfg.ValidateFor(RoleAPI)
		if (viaValidate == nil) != (viaRole == nil) {
			t.Errorf("Validate() 与 ValidateFor(RoleAPI) 结论不一致: %v vs %v", viaValidate, viaRole)
		}
		if viaValidate != nil && viaValidate.Error() != viaRole.Error() {
			t.Errorf("Validate() 与 ValidateFor(RoleAPI) 文案不一致:\n  %v\n  %v", viaValidate, viaRole)
		}
	}
}

// ---------------------------------------------------------------------------
// 校验：把问题逐条报出来，而不是只报第一个
// ---------------------------------------------------------------------------

// TestValidateReportsEveryProblemAtOnce 保证运维一眼能看全所有缺失项。
//
// 只报第一个的话，修完 server.port 再跑、再报 database.host……在容器编排里
// 就是反复重启反复猜。这里直接断言条目数，任何"提前 return"的改动都会红。
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	err := (Config{}).Validate()
	if err == nil {
		t.Fatal("空配置必须报错，实际 nil")
	}
	msg := err.Error()

	wantPaths := []string{
		"server.port",
		"database.host",
		"database.port",
		"database.user",
		"database.dbname",
		"database.password",
		"redis.host",
		"rabbitmq.host",
		"rabbitmq.username",
		"jwt.secret",
		"jwt.access_token_ttl",
		"jwt.refresh_token_ttl",
	}
	for _, p := range wantPaths {
		if !strings.Contains(msg, p) {
			t.Errorf("错误信息里缺少配置路径 %q：\n%s", p, msg)
		}
	}

	// 期望 12 条；条目数不符说明有校验被漏掉或被合并了。
	//
	// 这里用精确计数是刻意的：新增/删除一条校验规则时测试会红，逼着人来看一眼
	// 是不是有意的。对一个连续出过两次回归的模块，这个摩擦力是划算的。
	if got := strings.Count(msg, "\n  - "); got != len(wantPaths) {
		t.Errorf("报出的问题条数 = %d, want %d：\n%s", got, len(wantPaths), msg)
	}
}

// TestWorkerRoleStillReportsCommonProblems 防止"让 worker 能起来"被做成
// "worker 跳过全部校验"。这条路径坏了不会立刻暴露——配置错了照样启动，
// 直到运行中连不上库才报错，那时已经晚了。
func TestWorkerRoleStillReportsCommonProblems(t *testing.T) {
	cfg := Config{} // 连 jwt 带通用项全缺
	err := cfg.ValidateFor(RoleWorker)
	if err == nil {
		t.Fatal("worker 角色对通用必填项仍应报错，实际 nil")
	}
	for _, p := range []string{"server.port", "database.host", "redis.host", "rabbitmq.host"} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("worker 角色的错误信息里缺少 %q：%v", p, err)
		}
	}
	// 反向：worker 不该因为 jwt 报错。
	if strings.Contains(err.Error(), "jwt.") {
		t.Errorf("worker 角色不应校验 jwt.*：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 环境变量覆盖
// ---------------------------------------------------------------------------

// TestApplyEnvOverridesMapsEveryVariable 逐个变量验证"改的是哪个字段"。
//
// 每个用例都把结果与"只改了预期字段"的期望值整体比较（reflect.DeepEqual），
// 而不是只看目标字段。只断言目标字段的话，"SERVER_PORT 写进了 RabbitMQ.Port"
// 这类串字段的错会被漏掉——那正是最需要自动化来发现的一类错。
func TestApplyEnvOverridesMapsEveryVariable(t *testing.T) {
	cases := []struct {
		env   string
		value string
		want  func(c *Config)
	}{
		{"SERVER_PORT", "9999", func(c *Config) { c.Server.Port = 9999 }},
		{"MYSQL_HOST", "db.example", func(c *Config) { c.Database.Host = "db.example" }},
		{"MYSQL_PORT", "3307", func(c *Config) { c.Database.Port = 3307 }},
		{"MYSQL_USER", "appuser", func(c *Config) { c.Database.User = "appuser" }},
		{"MYSQL_ROOT_PASSWORD", "rootpw", func(c *Config) { c.Database.Password = "rootpw" }},
		{"MYSQL_PASSWORD", "apppw", func(c *Config) { c.Database.Password = "apppw" }},
		{"MYSQL_DATABASE", "otherdb", func(c *Config) { c.Database.DBName = "otherdb" }},
		{"REDIS_HOST", "redis.example", func(c *Config) { c.Redis.Host = "redis.example" }},
		{"REDIS_PORT", "6380", func(c *Config) { c.Redis.Port = 6380 }},
		{"REDIS_PASSWORD", "redispw", func(c *Config) { c.Redis.Password = "redispw" }},
		{"REDIS_DB", "3", func(c *Config) { c.Redis.DB = 3 }},
		{"RABBITMQ_HOST", "mq.example", func(c *Config) { c.RabbitMQ.Host = "mq.example" }},
		{"RABBITMQ_PORT", "5673", func(c *Config) { c.RabbitMQ.Port = 5673 }},
		{"RABBITMQ_USER", "mquser", func(c *Config) { c.RabbitMQ.Username = "mquser" }},
		{"RABBITMQ_PASS", "mqpw", func(c *Config) { c.RabbitMQ.Password = "mqpw" }},
		{"JWT_SECRET", "from-env", func(c *Config) { c.JWT.Secret = "from-env" }},
		{"AI_ENABLED", "true", func(c *Config) { c.AI.Enabled = true }},
		{"AI_ENABLED", "false", func(c *Config) { c.AI.Enabled = false }},
		{"AI_ENABLED", "1", func(c *Config) { c.AI.Enabled = true }},
	}

	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(tc.env, tc.value)

			cfg := baseConfig()
			if ignored := ApplyEnvOverrides(&cfg); len(ignored) != 0 {
				t.Fatalf("合法值 %s=%s 不该被忽略，实际 ignored=%v", tc.env, tc.value, ignored)
			}

			want := baseConfig()
			tc.want(&want)
			if !reflect.DeepEqual(cfg, want) {
				t.Errorf("%s=%s 之后配置与期望不符（只看目标字段会漏掉串字段的错）:\n got=%+v\nwant=%+v",
					tc.env, tc.value, cfg, want)
			}
		})
	}
}

// TestApplyEnvOverridesPasswordPrecedence 同时设置两个密码变量时的优先级。
// MYSQL_PASSWORD 在 MYSQL_ROOT_PASSWORD 之后读取，因此后者优先——compose 对
// 两个服务都注入了 MYSQL_ROOT_PASSWORD，应用专属密码需要能覆盖它。
func TestApplyEnvOverridesPasswordPrecedence(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MYSQL_ROOT_PASSWORD", "rootpw")
	t.Setenv("MYSQL_PASSWORD", "apppw")

	cfg := baseConfig()
	ApplyEnvOverrides(&cfg)
	if cfg.Database.Password != "apppw" {
		t.Errorf("database.password = %q, want %q（MYSQL_PASSWORD 应覆盖 MYSQL_ROOT_PASSWORD）",
			cfg.Database.Password, "apppw")
	}
}

// TestApplyEnvOverridesIgnoresBadIntegers 覆盖 9559766 修掉的"静默吞掉"。
//
// 非法值既不能生效，也不能悄悄消失：必须原样出现在返回的 ignored 列表里，
// 由调用方打印。这里同时断言配置**完全没变**——如果实现改成"解析失败就写 0"，
// 端口会变成 0，配置看起来还在但连不上任何东西。
func TestApplyEnvOverridesIgnoresBadIntegers(t *testing.T) {
	cases := []struct{ env, value string }{
		{"SERVER_PORT", "abc"},
		{"MYSQL_PORT", "abc"},
		{"REDIS_PORT", "abc"},
		{"REDIS_DB", "abc"},
		{"RABBITMQ_PORT", "abc"},
		{"SERVER_PORT", "8080.5"},
		{"MYSQL_PORT", " 80"}, // 带前导空格：Atoi 不接受
		{"REDIS_DB", ""},
		// 布尔值的"写错"比整数更危险：一个拼错的值若被当成真，
		// 一次手滑就会静默打开模型调用，而 AI 的默认状态是关。
		// 注意 yes/on/off 与首尾空白是**合法**写法（见
		// TestAIEnabledAcceptsEveryHumanSpelling），这里只放真正非法的值。
		{"AI_ENABLED", "maybe"},
		{"AI_ENABLED", "tru"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			isolateEnv(t)
			if tc.value == "" {
				// 空值等同于未设置，不应被当成"非法"而报警。
				cfg := baseConfig()
				if ignored := ApplyEnvOverrides(&cfg); len(ignored) != 0 {
					t.Fatalf("空值不该进入 ignored: %v", ignored)
				}
				return
			}
			t.Setenv(tc.env, tc.value)

			cfg := baseConfig()
			ignored := ApplyEnvOverrides(&cfg)

			if want := []string{tc.env + "=" + tc.value}; !reflect.DeepEqual(ignored, want) {
				t.Errorf("ignored = %v, want %v", ignored, want)
			}
			if !reflect.DeepEqual(cfg, baseConfig()) {
				t.Errorf("非法值不得改动任何字段:\n got=%+v\nwant=%+v", cfg, baseConfig())
			}
		})
	}
}

// TestApplyEnvOverridesNilIsSafe 防御 nil 入参：ApplyEnvOverrides 在
// DefaultLocalConfig 与 LoadFor 两处被调用，传 nil 不该 panic。
func TestApplyEnvOverridesNilIsSafe(t *testing.T) {
	isolateEnv(t)
	if ignored := ApplyEnvOverrides(nil); ignored != nil {
		t.Errorf("nil 入参应返回 nil，实际 %v", ignored)
	}
}

// TestIgnoredEnvVarLeavesAWarning 是"不再静默吞掉"的端到端证明。
//
// 9559766 之前，MYSQL_PORT=abc 是彻底无声的：配置照用文件里的值，日志一个字
// 都没有。现在必须留下一条 Warn，且带上具体的变量与取值——只断言"有日志"
// 不够，运维需要知道是哪个变量坏了。
func TestIgnoredEnvVarLeavesAWarning(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MYSQL_PORT", "abc")
	logs := observeLogs(t, zapcore.WarnLevel)

	cfg, err := LoadFor(writeConfig(t, fileConfigYAML), RoleWorker)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	// 非法值被忽略：保留文件里的 3306。
	if cfg.Database.Port != 3306 {
		t.Errorf("database.port = %d, want 3306（非法环境变量应被忽略、保留文件值）", cfg.Database.Port)
	}

	// 注意变量名在**字段**里而不在消息里，所以要按字段找：
	// 写成 FilterMessageSnippet("MYSQL_PORT") 会是 0 条（第一版就错在这里）。
	var found []observer.LoggedEntry
	for _, e := range logs.All() {
		if e.ContextMap()["env"] == "MYSQL_PORT=abc" {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("期望恰好一条 env=MYSQL_PORT=abc 的日志，实际 %d 条: %v", len(found), logs.All())
	}
	if got := found[0].Level; got != zap.WarnLevel {
		t.Errorf("日志级别 = %v, want Warn（坏掉的环境变量常是运维事故的起点）", got)
	}
	// 告警必须能定位到具体变量，而不是一句无信息量的话。
	if !strings.Contains(found[0].Message, "忽略非法环境变量") {
		t.Errorf("日志消息 = %q，应当说明是环境变量非法被忽略", found[0].Message)
	}
}

// TestValidEnvVarLeavesNoWarning 反向守一遍：正常覆盖不该产生告警，
// 否则运维会被无意义的 Warn 淹没，真正的告警就没人看了。
func TestValidEnvVarLeavesNoWarning(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MYSQL_PORT", "3307")
	logs := observeLogs(t, zapcore.WarnLevel)

	cfg, err := LoadFor(writeConfig(t, fileConfigYAML), RoleWorker)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Database.Port != 3307 {
		t.Errorf("database.port = %d, want 3307", cfg.Database.Port)
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("合法覆盖不该产生告警，实际 %d 条: %v", n, logs.All())
	}
}

// TestEnvOverrideStillGoesThroughValidation 环境变量覆盖之后必须重新校验。
//
// 覆盖发生在 Validate 之前，所以 MYSQL_PORT=-1 这种"能解析但非法"的值必须被
// 校验拦下。若哪天把顺序改成先校验后覆盖，非法值就会绕过校验直接进连接串。
func TestEnvOverrideStillGoesThroughValidation(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MYSQL_PORT", "-1")

	_, err := LoadFor(writeConfig(t, fileConfigYAML), RoleWorker)
	requireErrContains(t, err, "database.port")
}

// ---------------------------------------------------------------------------
// 加载：缺失文件、坏文件、默认值
// ---------------------------------------------------------------------------

// TestLoadForMissingFileIsErrNotExist 守一个隐蔽但关键的耦合：
// loadLocalDev 靠 errors.Is(err, os.ErrNotExist) 判断"该不该降级到默认值"。
//
// LoadFor 用 fmt.Errorf("...: %w") 包了三层，errors.Is 仍然成立才让降级生效。
// 一旦有人把 %w 改成 %v，错误文本看起来没变，但降级路径会整体失效——
// 缺配置文件时不再走默认值，而是直接报"读不到文件"。
func TestLoadForMissingFileIsErrNotExist(t *testing.T) {
	isolateEnv(t)
	_, err := LoadFor(filepath.Join(t.TempDir(), "nope.yaml"), RoleWorker)
	if err == nil {
		t.Fatal("文件不存在时必须报错")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) = false，降级路径会失效: %v", err)
	}
}

// TestLoadForRejectsMalformedYAML 坏 yaml 必须 fail fast，不能带着半份配置继续。
func TestLoadForRejectsMalformedYAML(t *testing.T) {
	isolateEnv(t)
	for _, body := range []string{
		"server: [unclosed\n",
		"server:\n  port: \"not-a-number\"\n",
	} {
		if _, err := LoadFor(writeConfig(t, body), RoleWorker); err == nil {
			t.Errorf("坏 yaml 必须报错，实际 nil；输入:\n%s", body)
		}
	}
}

// TestLoadForDurationMustBeString 钉住一个很容易踩的书写陷阱。
//
// time.Duration 在 yaml 里必须写成 "15m" 这样的时长字符串。写整数 15 会解析
// 失败（不是被当成 15ns），写加引号的 "15" 同样失败——因为 "15" 不是合法时长。
// 这三种写法已实测确认，注释里也写了，这里把它变成可执行的断言。
func TestLoadForDurationMustBeString(t *testing.T) {
	isolateEnv(t)
	mk := func(ttl string) string {
		return fileConfigYAML + "jwt:\n  secret: test-secret\n  access_token_ttl: " + ttl + "\n"
	}

	t.Run("15m 可解析", func(t *testing.T) {
		cfg, err := LoadFor(writeConfig(t, mk("15m")), RoleAPI)
		if err != nil {
			t.Fatalf("15m 应当可解析: %v", err)
		}
		if cfg.JWT.AccessTokenTTL != 15*time.Minute {
			t.Errorf("access_token_ttl = %v, want 15m", cfg.JWT.AccessTokenTTL)
		}
	})
	t.Run("整数 15 被拒绝", func(t *testing.T) {
		_, err := LoadFor(writeConfig(t, mk("15")), RoleAPI)
		if err == nil {
			t.Error("整数时长必须被拒绝，否则 15 会被当成 15 纳秒，token 立即过期")
		}
	})
	t.Run("加引号的 \"15\" 被拒绝", func(t *testing.T) {
		if _, err := LoadFor(writeConfig(t, mk(`"15"`)), RoleAPI); err == nil {
			t.Error(`"15" 不是合法时长，必须被拒绝`)
		}
	})
}

// TestLoadForAppliesTokenTTLDefaults 覆盖 applyDefaults 的存在理由。
//
// 配置文件里不写 jwt.access_token_ttl 时 yaml 会留 0，若直接使用则签出的
// token 立即过期——表现为"登录成功但下一个请求就 401"。默认值必须在
// Unmarshal 之后、Validate 之前填上（顺序错了 Validate 会先把它拦下来）。
func TestLoadForAppliesTokenTTLDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := LoadFor(writeConfig(t, fileConfigYAML), RoleWorker)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.JWT.AccessTokenTTL != defaultAccessTokenTTL {
		t.Errorf("access_token_ttl = %v, want %v", cfg.JWT.AccessTokenTTL, defaultAccessTokenTTL)
	}
	if cfg.JWT.RefreshTokenTTL != defaultRefreshTokenTTL {
		t.Errorf("refresh_token_ttl = %v, want %v", cfg.JWT.RefreshTokenTTL, defaultRefreshTokenTTL)
	}
	if defaultAccessTokenTTL != 15*time.Minute || defaultRefreshTokenTTL != 168*time.Hour {
		t.Errorf("默认时长被改动了: %v / %v；这是对外承诺的 token 有效期，改动需同步文档",
			defaultAccessTokenTTL, defaultRefreshTokenTTL)
	}
}

// TestApplyDefaultsKeepsExplicitValues 反向：显式配置的值不能被默认值覆盖。
func TestApplyDefaultsKeepsExplicitValues(t *testing.T) {
	isolateEnv(t)
	cfg, err := LoadFor(writeConfig(t, fileConfigYAML+
		"jwt:\n  secret: test-secret\n  access_token_ttl: 30m\n  refresh_token_ttl: 1h\n"), RoleAPI)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.JWT.AccessTokenTTL != 30*time.Minute {
		t.Errorf("access_token_ttl = %v, want 30m（显式值被默认值覆盖了）", cfg.JWT.AccessTokenTTL)
	}
	if cfg.JWT.RefreshTokenTTL != time.Hour {
		t.Errorf("refresh_token_ttl = %v, want 1h（显式值被默认值覆盖了）", cfg.JWT.RefreshTokenTTL)
	}
}

// ---------------------------------------------------------------------------
// LoadLocalDev：缺配置文件时到底降级还是 fail fast
// ---------------------------------------------------------------------------

// TestDefaultLocalConfigCarriesNoPasswords 是 9559766 的核心承诺。
//
// 改之前 DefaultLocalConfig 内置了 DB/Redis/RabbitMQ 的密码（DB 是 123456），
// 于是 CONFIG_PATH 打错一个字母就会拿硬编码密码去连本机库，而且 err 为 nil。
// 现在默认值里不能有任何密码，且必须**通不过校验**——否则静默降级会复活。
func TestDefaultLocalConfigCarriesNoPasswords(t *testing.T) {
	isolateEnv(t)
	cfg := DefaultLocalConfig()

	if cfg.Database.Password != "" {
		t.Errorf("database.password = %q, want 空（默认值不得内置密码）", cfg.Database.Password)
	}
	if cfg.Redis.Password != "" {
		t.Errorf("redis.password = %q, want 空", cfg.Redis.Password)
	}
	if cfg.RabbitMQ.Password != "" {
		t.Errorf("rabbitmq.password = %q, want 空", cfg.RabbitMQ.Password)
	}
	if cfg.JWT.Secret != "" {
		t.Errorf("jwt.secret = %q, want 空", cfg.JWT.Secret)
	}

	// 关键：默认值本身必须是不完整的，这样"文件缺失 -> 用默认值静默启动"就不可能发生。
	if err := cfg.ValidateFor(RoleAPI); err == nil {
		t.Error("DefaultLocalConfig 必须通不过 API 角色校验，否则出错路径会静默降级成默认配置启动")
	}
}

// TestLoadLocalDevFallbackMatrix 把"什么时候降级、什么时候报错"四种组合钉死。
func TestLoadLocalDevFallbackMatrix(t *testing.T) {
	missing := func(t *testing.T) string {
		return filepath.Join(t.TempDir(), "does-not-exist.yaml")
	}

	t.Run("文件缺失且无密码环境变量：报错，不静默降级", func(t *testing.T) {
		isolateEnv(t)
		cfg, usedDefault, err := LoadLocalDevForWorker(missing(t))
		if err == nil {
			t.Fatal("默认值不完整时必须报错，不能返回可用的配置")
		}
		if !usedDefault {
			t.Error("用了默认值时 usedDefault 应为 true")
		}
		if cfg != (Config{}) {
			t.Errorf("报错时应返回零值配置，实际 %+v", cfg)
		}
	})

	t.Run("文件缺失但环境变量补齐密码：降级为默认值", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MYSQL_ROOT_PASSWORD", "from-env")
		cfg, usedDefault, err := LoadLocalDevForWorker(missing(t))
		if err != nil {
			t.Fatalf("环境变量补齐后应能降级成功: %v", err)
		}
		if !usedDefault {
			t.Error("降级路径下 usedDefault 应为 true")
		}
		if cfg.Database.Password != "from-env" {
			t.Errorf("database.password = %q, want from-env（默认值也必须走环境变量覆盖）", cfg.Database.Password)
		}
	})

	t.Run("文件存在但非法：直接报错，绝不降级", func(t *testing.T) {
		isolateEnv(t)
		// 语法合法但缺必填项：这是"配置写错了"，不是"没配"。
		path := writeConfig(t, "server:\n  port: 0\n")
		_, usedDefault, err := LoadLocalDevForWorker(path)
		if err == nil {
			t.Fatal("非法配置必须报错")
		}
		if usedDefault {
			t.Error("文件存在时不得降级到默认值——那会把'配置写错'变成'用别的配置启动'")
		}
		if errors.Is(err, os.ErrNotExist) {
			t.Error("这是校验错误，不该被识别为文件不存在")
		}
	})

	t.Run("文件存在且合法：正常加载", func(t *testing.T) {
		isolateEnv(t)
		cfg, usedDefault, err := LoadLocalDevForWorker(writeConfig(t, fileConfigYAML))
		if err != nil {
			t.Fatalf("加载失败: %v", err)
		}
		if usedDefault {
			t.Error("加载成功时 usedDefault 应为 false")
		}
		if cfg.Database.Port != 3306 {
			t.Errorf("database.port = %d, want 3306", cfg.Database.Port)
		}
	})
}

// TestLoadLocalDevForWorkerFreesWorkerFromJWT 是 752147b 的端到端形态。
//
// 这正是当时 worker 起不来的完整场景：仓库自带的配置都没有 jwt 段、环境里也
// 没有 JWT_SECRET。同一份配置下，worker 必须能加载，API 必须被明确拦住并指向
// jwt.secret（start.sh / compose 负责注入）。
func TestLoadLocalDevForWorkerFreesWorkerFromJWT(t *testing.T) {
	isolateEnv(t)
	path := writeConfig(t, fileConfigYAML)

	cfg, usedDefault, err := LoadLocalDevForWorker(path)
	if err != nil {
		t.Fatalf("worker 不该被 jwt.secret 拦住（752147b 的回归）: %v", err)
	}
	if usedDefault {
		t.Error("usedDefault 应为 false")
	}
	// worker 用不到 jwt，缺省时保持零值即可，不该被填上任何东西。
	if cfg.JWT.Secret != "" {
		t.Errorf("worker 的 jwt.secret = %q, want 空", cfg.JWT.Secret)
	}

	// 同一份文件走 API 角色必须被拦，且错误要指向 jwt.secret。
	if _, _, err := LoadLocalDev(path); err == nil {
		t.Fatal("API 角色缺少 jwt.secret 时必须报错")
	} else {
		requireErrContains(t, err, "jwt.secret")
	}

	// 注入 JWT_SECRET 后 API 角色即可加载——这就是 start.sh / compose 的做法。
	t.Setenv("JWT_SECRET", "injected-by-deploy")
	if _, _, err := LoadLocalDev(path); err != nil {
		t.Fatalf("注入 JWT_SECRET 后 API 角色应能加载: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 配置漂移防线
// ---------------------------------------------------------------------------

// TestShippedConfigFilesLoad 直接加载仓库里实际发布的 yaml。
//
// 这是防"配置漂移"的：改了 Config 结构或校验规则却忘了同步这几个文件时，这里
// 会红。历史上两次回归都发生在这个交界处（走的是 Load 而不是 LoadLocalDev，
// 所以只测 LoadLocalDev 覆盖不到）。
//
// go test 的工作目录是包目录，故用 ../../configs 回到 backend/configs。
func TestShippedConfigFilesLoad(t *testing.T) {
	files := []string{"config.yaml", "config.compose-local.yaml", "config.docker.yaml"}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			path := filepath.Join("..", "..", "configs", name)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("仓库里应当存在 %s: %v", path, err)
			}

			// worker 角色：这些文件都不含 jwt 段，worker 必须能直接加载。
			if _, err := LoadFor(path, RoleWorker); err != nil {
				t.Errorf("worker 角色加载 %s 失败: %v", name, err)
			}

			// API 角色：文件里没有 jwt.secret，必须被明确拦住并指向该字段
			// （而不是因为别的原因失败）。JWT_SECRET 由 start.sh / compose 注入。
			_, err := LoadFor(path, RoleAPI)
			requireErrContains(t, err, "jwt.secret")

			// 注入之后必须能加载。
			t.Setenv("JWT_SECRET", "test-secret")
			if _, err := LoadFor(path, RoleAPI); err != nil {
				t.Errorf("注入 JWT_SECRET 后 API 角色加载 %s 失败: %v", name, err)
			}
		})
	}
}

// TestShippedConfigFilesHaveNoHardcodedSecrets 防止把真实密码再写回仓库。
//
// 9559766 把硬编码密码从 Go 默认值里去掉了，但 yaml 里仍然是明文
// password: 123456 / password123。这些是本地开发用的，可以接受；但一旦有人
// 把生产凭据提交进来，这个用例会立刻发现——它只允许出现已知的本地开发值。
func TestShippedConfigFilesHaveNoHardcodedSecrets(t *testing.T) {
	allowed := map[string]bool{"123456": true, "password123": true}
	for _, name := range []string{"config.yaml", "config.compose-local.yaml", "config.docker.yaml"} {
		path := filepath.Join("..", "..", "configs", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "password:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "password:"))
			if value == "" || allowed[value] {
				continue
			}
			t.Errorf("%s:%d 出现了非本地开发用的密码字面量（值已隐去）："+
				"配置文件会进版本库，请改用环境变量注入", name, i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// AI 配置：全可选，缺省即全关
// ---------------------------------------------------------------------------

// aiConfigYAML 是一份"把 ai 段写全"的配置，用来验证字段能被解析。
// 时长必须写成 "2s" 这种字符串——与 jwt.* 同一个坑（写成整数会解析失败）。
const aiConfigYAML = fileConfigYAML + `
ai:
  enabled: true
  gateway_addr: sidecar:50051
  timeout: 2s
  model: gpt-4o-mini
  max_tokens: 512
  daily_budget: 5
`

// aiBlockTestYAML 在 fileConfigYAML 之上补了 jwt 段。
//
// 为什么两套都用：fileConfigYAML 刻意不含 jwt 段（仓库里发布的三个 yaml 都不含，
// JWT_SECRET 由部署环境注入），而本组用例要**同时**断言 API 与 Worker 两个角色
// 都不被 ai 配置影响。只补 jwt 才能把"别的必填项"这个变量消除掉，
// 否则 API 角色会先被 jwt.secret 拦下，测不到 ai 段。
const aiBlockTestYAML = fileConfigYAML + `
jwt:
  secret: test-secret
  access_token_ttl: 15m
  refresh_token_ttl: 168h
`

// TestAIBlockIsFullyOptional 是 P0 验收清单"删掉配置中的 ai: 段，服务照常启动"的实现。
//
// 为什么这条要单独测：配置校验失败会让进程**启动失败**，把可选功能的配置缺失
// 升级成"服务起不来"。历史上这个模块已经因为同类问题出过两次回归
// （worker 被 jwt.secret 卡住、JWT_SECRET 变必填），所以这次先把阀门焊死。
//
// 三种"没配 AI"的形态都必须放行，且两个角色都一样：
//  1. 整个 ai 段不存在（老配置文件）；
//  2. ai 段存在但为空；
//  3. ai 段只写了 enabled。
//
// 并且结果必须是 Enabled=false（零值），不能因为 yaml 里出现了 ai: 就变成开。
func TestAIBlockIsFullyOptional(t *testing.T) {
	bodies := map[string]string{
		"整个 ai 段不存在":     aiBlockTestYAML,
		"ai 段存在但为空":      aiBlockTestYAML + "ai:\n",
		"ai 段只有 enabled": aiBlockTestYAML + "ai:\n  enabled: false\n",
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			for _, role := range []Role{RoleAPI, RoleWorker} {
				isolateEnv(t)
				cfg, err := LoadFor(writeConfig(t, body), role)
				if err != nil {
					t.Fatalf("role=%v 时缺少 ai 配置不该导致加载失败: %v", role, err)
				}
				if cfg.AI.Enabled {
					t.Errorf("role=%v: 未显式打开时 AI.Enabled 必须为 false", role)
				}
				// 零值配置也必须能取到安全的超时：0 超时会让
				// context.WithTimeout(ctx, 0) 立刻超时，表现为"AI 永远超时"。
				if got := cfg.AI.TimeoutOr(); got != defaultAITimeout {
					t.Errorf("role=%v: 未配置超时时应取默认值 %v，实际 %v", role, defaultAITimeout, got)
				}
			}
		})
	}
}

// TestAIBlockParses 反向守一遍：写全了就必须真的解析进来。
//
// 只测"缺省能用"是不够的——如果 yaml tag 写错（比如 max_tokens 拼成 maxTokens），
// 缺省场景照样通过，而线上会发现"配置改了没生效"。
func TestAIBlockParses(t *testing.T) {
	isolateEnv(t)
	cfg, err := LoadFor(writeConfig(t, aiConfigYAML), RoleWorker)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	want := AIConfig{
		Enabled:     true,
		GatewayAddr: "sidecar:50051",
		Timeout:     2 * time.Second,
		Model:       "gpt-4o-mini",
		MaxTokens:   512,
		DailyBudget: 5,
	}
	if cfg.AI != want {
		t.Errorf("ai 段解析结果不符:\n got=%+v\nwant=%+v", cfg.AI, want)
	}
}

// TestAITimeoutMustBeDurationString 沿用 jwt.* 的教训：
// time.Duration 在 yaml 里写整数会解析失败。
//
// 这里不是重复劳动：ai.timeout 是**可选**字段，而"可选字段解析失败"的
// 表现形式是进程起不来——比必填字段更让人意外。
func TestAITimeoutMustBeDurationString(t *testing.T) {
	isolateEnv(t)
	for _, bad := range []string{"2", `"2"`} {
		body := fileConfigYAML + "ai:\n  enabled: true\n  timeout: " + bad + "\n"
		if _, err := LoadFor(writeConfig(t, body), RoleWorker); err == nil {
			t.Errorf("ai.timeout: %s 是非法时长写法，应当被拒绝", bad)
		}
	}
}

// TestAIEnabledCanBeTurnedOnByEnv 覆盖部署侧的开关入口。
//
// 价值在于"同一份镜像在不同环境开关 AI 不需要重建镜像"。
func TestAIEnabledCanBeTurnedOnByEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AI_ENABLED", "true")

	cfg, err := LoadFor(writeConfig(t, fileConfigYAML), RoleWorker)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.AI.Enabled {
		t.Error("AI_ENABLED=true 应当打开总开关")
	}
}

// TestAIDefaultsAreOff 钉住"缺省 = AI 全关"这条项目级承诺。
//
// 默认值是整个 AI 改造的安全底座：任何一处把默认值改成 true，
// 都会让所有未显式配置的环境开始真的调模型。这里把它变成断言。
func TestAIDefaultsAreOff(t *testing.T) {
	isolateEnv(t)
	if cfg := DefaultLocalConfig(); cfg.AI.Enabled {
		t.Error("DefaultLocalConfig 里 AI 必须是关的")
	}
	if (AIConfig{}).Enabled {
		t.Error("AIConfig 零值必须是关的（老配置文件据此降级）")
	}
}

// TestAIEnabledAcceptsEveryHumanSpelling 守住 Go 与 sidecar 的取值集合一致。
//
// 这是 review 抓到的真实分歧：Go 侧用 strconv.ParseBool（只认 1/0/t/f/true/false），
// sidecar 侧认 yes/no/on/off。于是运维敲 AI_ENABLED=off 时，sidecar 关了、
// Go 侧却认为"值非法、保留配置里的 true"，模型调用继续发生——
// 总开关的全部意义就是"关得掉"，两个进程的取值集合不能有任何差别。
//
// 期望值必须与 sidecar/src/config.ts 的 parseBool 一一对应。
func TestAIEnabledAcceptsEveryHumanSpelling(t *testing.T) {
	on := []string{"1", "t", "T", "true", "TRUE", "True", "yes", "YES", "on", "ON", " on "}
	off := []string{"0", "f", "F", "false", "FALSE", "no", "NO", "off", "OFF", " off "}

	for _, raw := range on {
		t.Run("on:"+raw, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("AI_ENABLED", raw)
			cfg := baseConfig()
			if ignored := ApplyEnvOverrides(&cfg); len(ignored) != 0 {
				t.Fatalf("%q 应当被接受，实际 ignored=%v", raw, ignored)
			}
			if !cfg.AI.Enabled {
				t.Errorf("%q 应当打开总开关", raw)
			}
		})
	}
	for _, raw := range off {
		t.Run("off:"+raw, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("AI_ENABLED", raw)
			cfg := baseConfig()
			cfg.AI.Enabled = true // 先打开，验证 off 真的能关掉
			if ignored := ApplyEnvOverrides(&cfg); len(ignored) != 0 {
				t.Fatalf("%q 应当被接受，实际 ignored=%v", raw, ignored)
			}
			if cfg.AI.Enabled {
				t.Errorf("%q 必须关掉总开关（关不掉的总开关等于没有）", raw)
			}
		})
	}
}

// TestAIEnabledInvalidValueKeepsStateAndWarns 反向：写错值既不生效也不静默。
func TestAIEnabledInvalidValueKeepsStateAndWarns(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AI_ENABLED", "maybe")
	logs := observeLogs(t, zapcore.WarnLevel)

	cfg := baseConfig()
	cfg.AI.Enabled = true
	ignored := ApplyEnvOverrides(&cfg)

	if !cfg.AI.Enabled {
		t.Error("非法值不该改动配置（保留原值）")
	}
	if len(ignored) != 1 || ignored[0] != "AI_ENABLED=maybe" {
		t.Errorf("非法值必须进入 ignored 列表，实际 %v", ignored)
	}
	warnIgnoredEnvVars(ignored)
	if logs.Len() != 1 {
		t.Errorf("非法值必须留下一条 Warn，实际 %d 条", logs.Len())
	}
}

// TestAIDefaultModelEnvOverride 覆盖模型名的部署侧入口。
//
// 为什么需要它：换模型属于运维动作，不该要求改配置文件并重启。
// 演示与联调环境也要能用另一个模型名（例如离线演示模式下的 faux 模型）。
func TestAIDefaultModelEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AI_DEFAULT_MODEL", "faux-1")

	cfg := baseConfig()
	if ignored := ApplyEnvOverrides(&cfg); len(ignored) != 0 {
		t.Fatalf("合法值不该被忽略: %v", ignored)
	}
	if cfg.AI.Model != "faux-1" {
		t.Errorf("AI.Model = %q, want faux-1", cfg.AI.Model)
	}

	// 空值等同未设置：不能把配置文件里的模型名清成空串。
	isolateEnv(t)
	cfg2 := baseConfig()
	cfg2.AI.Model = "gpt-4o-mini"
	ApplyEnvOverrides(&cfg2)
	if cfg2.AI.Model != "gpt-4o-mini" {
		t.Errorf("空环境变量不该清掉配置里的模型名，实际 %q", cfg2.AI.Model)
	}
}

// ---------------------------------------------------------------------------
// embedding 通道的配置（P2 前置 B3）
// ---------------------------------------------------------------------------

// TestEmbeddingConfigDefaults 守住"未配置时的取值与 sidecar 完全一致"。
//
// 两边不一致的后果不是报错，而是 Go 侧的维度校验永远失败（或更糟：
// 校验被关掉，两种维度的向量混进同一张表）。
func TestEmbeddingConfigDefaults(t *testing.T) {
	var cfg AIConfig
	if got := cfg.EmbeddingModelOr(); got != ai.DefaultEmbeddingModel {
		t.Errorf("模型默认值 = %q, want %q", got, ai.DefaultEmbeddingModel)
	}
	if got := cfg.EmbeddingDimOr(); got != ai.DefaultEmbeddingDim {
		t.Errorf("维度默认值 = %d, want %d", got, ai.DefaultEmbeddingDim)
	}
	if got := cfg.EmbeddingTimeoutOr(); got != ai.DefaultEmbeddingTimeout {
		t.Errorf("超时默认值 = %v, want %v", got, ai.DefaultEmbeddingTimeout)
	}
	// 默认必须归一化：没归一化的向量会让更长的文本在余弦检索里系统性占优。
	if !cfg.EmbeddingNormalizeOr() {
		t.Error("未配置时 embedding_normalize 必须默认为 true")
	}
}

// TestEmbeddingNormalizeTriState 守住"没写"与"写了 false"必须能区分。
//
// yaml 与 proto3 一样，布尔字段没有"未指定"状态；用普通 bool 时
// `embedding_normalize: false` 与"整段配置缺失"会得到同一个零值，
// 而这两者的期望行为完全相反。
func TestEmbeddingNormalizeTriState(t *testing.T) {
	cases := map[string]bool{
		"":                           true, // 未写 → 默认归一化
		"embedding_normalize: true":  true,
		"embedding_normalize: false": false, // 显式关掉
	}
	for body, want := range cases {
		var cfg AIConfig
		if body != "" {
			if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
				t.Fatalf("解析 %q 失败: %v", body, err)
			}
		}
		if got := cfg.EmbeddingNormalizeOr(); got != want {
			t.Errorf("%q: EmbeddingNormalizeOr = %v, want %v", body, got, want)
		}
	}
}

// TestEmbeddingEnvOverrides 守住部署侧的入口。
//
// compose 用同一组变量喂三个服务，正是为了让"两边不一致"结构上不可能发生；
// 因此这条路径必须有测试。
func TestEmbeddingEnvOverrides(t *testing.T) {
	t.Setenv("AI_EMBEDDING_MODEL", "bge-m3")
	t.Setenv("AI_EMBEDDING_DIM", "1024")
	cfg := Config{}
	_ = ApplyEnvOverrides(&cfg)
	if cfg.AI.EmbeddingModelOr() != "bge-m3" {
		t.Errorf("模型未被子覆盖: %q", cfg.AI.EmbeddingModelOr())
	}
	if cfg.AI.EmbeddingDimOr() != 1024 {
		t.Errorf("维度未被覆盖: %d", cfg.AI.EmbeddingDimOr())
	}

	// 非法整数保留原值并留下 warn，而不是写入 0（0 会被当成"不校验"）。
	t.Setenv("AI_EMBEDDING_DIM", "abc")
	cfg2 := Config{}
	ignored := ApplyEnvOverrides(&cfg2)
	if cfg2.AI.EmbeddingDim != 0 {
		t.Errorf("非法值不该被写入，实际 %d", cfg2.AI.EmbeddingDim)
	}
	if len(ignored) == 0 {
		t.Error("非法环境变量必须留下告警，否则运维只能猜为什么配置没生效")
	}
}

// TestShippedConfigsCarryEmbeddingBlock 守住"随仓库发布的配置文件里
// embedding 与 sidecar 的默认值一致"。
func TestShippedConfigsCarryEmbeddingBlock(t *testing.T) {
	// 用 RoleWorker 读取：这几个文件里的 jwt.secret 是空的（由部署环境注入），
	// 用 API 角色读取会因为"缺 secret"而失败，那条校验与本节要验证的内容无关。
	for _, f := range []string{"config.yaml", "config.docker.yaml", "config.compose-local.yaml"} {
		cfg, err := LoadFor(filepath.Join("..", "..", "configs", f), RoleWorker)
		if err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if cfg.AI.EmbeddingModelOr() != ai.DefaultEmbeddingModel {
			t.Errorf("%s 的 embedding_model = %q，与 sidecar 的默认值不一致", f, cfg.AI.EmbeddingModelOr())
		}
		if cfg.AI.EmbeddingDimOr() != ai.DefaultEmbeddingDim {
			t.Errorf("%s 的 embedding_dim = %d，与 sidecar 的默认值不一致", f, cfg.AI.EmbeddingDimOr())
		}
		if !cfg.AI.EmbeddingNormalizeOr() {
			t.Errorf("%s 显式关掉了归一化；那会让长文本在余弦检索里系统性占优", f)
		}
	}
}

// ---------------------------------------------------------------------------
// P2 语义召回与精排的设计参数（D1~D4）
// ---------------------------------------------------------------------------

// TestRerankAndQuotaDefaults 钉住 D1~D4 的取值。
//
// 这些数字是延迟与成本的边界，不是随手可调的常量：它们同时出现在
// AI-P2-设计决定.md、随仓库发布的配置文件与代码默认值里，
// 三处必须一致，而能被测试钉住的是后两处。
func TestRerankAndQuotaDefaults(t *testing.T) {
	var cfg AIConfig

	// 两个配额的**缺省必须是 0（关闭）**：红线 4 要求新增配置项的缺省
	// 落在"功能关闭"上。设计取值 0.3 / 0.1 写在发布的配置文件里，
	// 由 TestShippedConfigsCarrySemanticDefaults 钉住。
	if got := cfg.RecallQuotaOr(); got != 0 {
		t.Errorf("recall_quota 缺省 = %v, want 0（缺省即关闭）", got)
	}
	if got := cfg.ExploreQuotaOr(); got != 0 {
		t.Errorf("explore_quota 缺省 = %v, want 0（缺省即关闭）", got)
	}
	if got := cfg.RerankTimeoutOr(); got != 120*time.Millisecond {
		t.Errorf("rerank_timeout 默认 = %v, want 120ms", got)
	}
	if got := cfg.RerankCandidatesOr(); got != 20 {
		t.Errorf("rerank_candidates 默认 = %d, want 20", got)
	}
	// LLM 精排默认关闭：真实模型的延迟没实测过，就不该放进用户请求路径。
	if cfg.RerankEnabled {
		t.Error("rerank_enabled 必须默认 false")
	}
}

// TestQuotaZeroIsARealValue 守住 D3 的核心：**配额设 0 必须可表达**。
//
// "0 与今天的排序逐字节一致"是 P2 的降级等价性验收项。
// 如果 0 被当成"未配置"而回退到一个非零默认值，这个开关就根本设不了 0，
// 那条验收项也就无从执行——这是最容易写错的一处（<=0 vs <0）。
func TestQuotaZeroIsARealValue(t *testing.T) {
	for _, q := range []float64{0, 1} {
		cfg := AIConfig{RecallQuota: q, ExploreQuota: q}
		if got := cfg.RecallQuotaOr(); got != q {
			t.Errorf("显式设 %v 时必须原样保留，实际 %v", q, got)
		}
		if got := cfg.ExploreQuotaOr(); got != q {
			t.Errorf("显式设 %v 时必须原样保留，实际 %v", q, got)
		}
	}
	// 越界值按 0（关闭）处理：唯一安全的方向。
	for _, bad := range []float64{-1, 1.5, 100} {
		if got := (AIConfig{RecallQuota: bad}).RecallQuotaOr(); got != 0 {
			t.Errorf("越界值 %v 应按 0（关闭）处理，实际 %v", bad, got)
		}
		if got := (AIConfig{ExploreQuota: bad}).ExploreQuotaOr(); got != 0 {
			t.Errorf("越界值 %v 应按 0 处理，实际 %v", bad, got)
		}
	}
	// 中间值原样保留。
	if got := (AIConfig{RecallQuota: 0.3}).RecallQuotaOr(); got != 0.3 {
		t.Errorf("中间值应原样保留，实际 %v", got)
	}
}

// TestShippedConfigsCarrySemanticDefaults 守住"随仓库发布的配置文件里
// 写的是设计取值，而不是缺省值"。
//
// 两者不一致时，运维看到的 yaml 与代码实际行为会差一截，
// 而"语义路到底开没开"是一个会直接影响 Feed 排序的问题。
func TestShippedConfigsCarrySemanticDefaults(t *testing.T) {
	for _, f := range []string{"config.yaml", "config.docker.yaml", "config.compose-local.yaml"} {
		cfg, err := LoadFor(filepath.Join("..", "..", "configs", f), RoleWorker)
		if err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if got := cfg.AI.RecallQuotaOr(); got != 0.3 {
			t.Errorf("%s 的 recall_quota = %v, want 0.3（设计取值）", f, got)
		}
		if got := cfg.AI.ExploreQuotaOr(); got != 0.1 {
			t.Errorf("%s 的 explore_quota = %v, want 0.1（设计取值）", f, got)
		}
		// 精排必须显式写成关闭：它的开销与真实模型延迟挂钩。
		if cfg.AI.RerankEnabled {
			t.Errorf("%s 打开了 LLM 精排；在实测回填延迟之前它必须保持关闭", f)
		}
	}
}

// TestRerankBoundsAreClamped 守住"配置写错时退到安全值而不是让服务起不来"。
func TestRerankBoundsAreClamped(t *testing.T) {
	// 超过 2 秒的精排超时会被夹住：用户请求不能等模型超过 2 秒。
	if got := (AIConfig{RerankTimeout: 30 * time.Second}).RerankTimeoutOr(); got != 2*time.Second {
		t.Errorf("超时上限应夹到 2s，实际 %v", got)
	}
	// 候选数上限夹到 64：它直接乘上 prompt 大小，多一个 0 就是十倍成本。
	if got := (AIConfig{RerankCandidates: 500}).RerankCandidatesOr(); got != 64 {
		t.Errorf("候选上限应夹到 64，实际 %d", got)
	}
	// 正常的中间值原样保留。
	if got := (AIConfig{RerankTimeout: 800 * time.Millisecond}).RerankTimeoutOr(); got != 800*time.Millisecond {
		t.Errorf("中间值应原样保留，实际 %v", got)
	}
	if got := (AIConfig{RerankCandidates: 30}).RerankCandidatesOr(); got != 30 {
		t.Errorf("中间值应原样保留，实际 %d", got)
	}
}
