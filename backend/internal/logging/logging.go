// Package logging 提供进程内唯一的结构化日志出口。
//
// 为什么要有这个包：项目此前用标准库 log，全仓 123 处调用，没有级别、没有
// 结构化字段、没有请求关联。出问题时只能靠时间戳在海量行里人肉对齐，而
// "某个用户说他操作失败了" 与 "日志里哪几行属于他" 之间没有任何可检索的联系。
//
// 本包解决三件事：
//  1. 唯一出口：所有日志经同一个 zap logger，格式与级别一致。
//  2. 请求关联：RequestID 中间件为每个请求生成/透传 X-Request-ID，
//     错误日志带上它，客户端上报该 ID 即可直接 grep 出整条链路。
//  3. 可替换：apierror.Logger 等钩子指向本包，切换实现不影响任何调用点。
package logging

import (
	"context"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// RequestIDHeader 是请求关联标识所用的 HTTP 头。
// 由 RequestID 中间件写入响应头，便于客户端在报障时提供。
const RequestIDHeader = "X-Request-ID"

// Config 决定日志的级别、格式与输出位置。
// 字段用字符串而非自定义类型，是为了让 config 包不必反向依赖本包。
type Config struct {
	Level  string // debug | info | warn | error（默认 info）
	Format string // json | console（默认 console）
	Output string // stdout | stderr（默认 stderr）
}

var (
	mu     sync.RWMutex
	logger *zap.Logger = newConsoleLogger(zapcore.InfoLevel, os.Stderr)
)

// Init 按配置构建全局 logger，并接管标准库 log 的输出。
//
// 返回 error 而不是在内部 panic：启动期配置写错应当由调用方决定怎么处理，
// 而不是让一个库替进程决定生死。
func Init(cfg Config) error {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}

	var out *os.File
	switch strings.ToLower(strings.TrimSpace(cfg.Output)) {
	case "", "stderr":
		out = os.Stderr
	case "stdout":
		out = os.Stdout
	default:
		return &ConfigError{Field: "output", Value: cfg.Output, Allowed: "stdout|stderr"}
	}

	var l *zap.Logger
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "", "console":
		l = newConsoleLogger(level, out)
	case "json":
		l = newJSONLogger(level, out)
	default:
		return &ConfigError{Field: "format", Value: cfg.Format, Allowed: "json|console"}
	}

	mu.Lock()
	logger = l
	mu.Unlock()

	// 把标准库 log 的输出也接进来，作为迁移期的安全网：
	// 尚未改造的 log.Printf 至少能带上时间戳与统一格式，不会散落在 stdout。
	// log.Fatal* 仍会调用 os.Exit，行为不变。
	zap.RedirectStdLog(l)

	return nil
}

// L 返回全局 logger。永远非 nil，未调用 Init 时是一个 console/info 的默认值，
// 因此调用点不必做 nil 判断。
func L() *zap.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return logger
}

// SetLogger 替换全局 logger 并返回被替换的那个，调用方可用它恢复现场。
//
// 存在的意义是让测试能断言"某个场景记到了哪个级别"——用 zap 的
// zaptest/observer 换入一个可观测的 logger，否则这类断言只能靠解析输出文本，
// 既脆弱又无法验证字段。
func SetLogger(l *zap.Logger) (prev *zap.Logger) {
	mu.Lock()
	defer mu.Unlock()
	prev = logger
	if l != nil {
		logger = l
	}
	return prev
}

// Sync 刷新缓冲。应在进程退出前调用；错误可忽略（stdout/stderr 上 Sync
// 常返回 EINVAL，属已知无害情况）。
func Sync() {
	_ = L().Sync()
}

// ConfigError 描述一处配置取值非法。
type ConfigError struct {
	Field   string
	Value   string
	Allowed string
}

func (e *ConfigError) Error() string {
	return "logging: invalid " + e.Field + " " + quote(e.Value) + ", allowed: " + e.Allowed
}

func quote(s string) string { return `"` + s + `"` }

func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	}
	return 0, &ConfigError{Field: "level", Value: s, Allowed: "debug|info|warn|error"}
}

func newConsoleLogger(level zapcore.Level, out *os.File) *zap.Logger {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	// console 格式下用大写级别，便于人眼快速扫过。
	cfg.EncodeLevel = zapcore.CapitalLevelEncoder
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(cfg), zapcore.AddSync(out), level)
	return zap.New(core, zap.AddCaller())
}

func newJSONLogger(level zapcore.Level, out *os.File) *zap.Logger {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), zapcore.AddSync(out), level)
	return zap.New(core, zap.AddCaller())
}

// ---------- 请求关联 ----------

type requestIDKey struct{}

// WithRequestID 把请求 ID 存入 ctx。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom 取出 ctx 里的请求 ID，没有则返回空串。
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Ctx 返回带 request_id 字段的 logger。
//
// 日志调用点应优先用它而不是 L()：这样"某个请求出了什么问题"就能靠
// 客户端上报的 X-Request-ID 直接检索出来，而不必按时间戳人肉对齐。
func Ctx(ctx context.Context) *zap.Logger {
	base := L()
	if id := RequestIDFrom(ctx); id != "" {
		return base.With(zap.String("request_id", id))
	}
	return base
}
