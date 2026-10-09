package db

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/logging"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm/logger"
)

// zapGormLogger 把 GORM 的 SQL 日志接进项目的结构化日志。
//
// 为什么必须替换默认 logger：GORM 的 logger.Default 自己 new 了一个
// log.Logger 直接写 stdout，**绕过了标准库的默认 logger**，因此
// zap.RedirectStdLog 根本抓不到它。后果有两条：
//
//  1. 结构化日志里混进多行、带 ANSI 颜色的输出，解析器直接崩；
//  2. 每条查询的**原始 SQL 与参数值**都进了日志——里面可能有用户名、
//     手机号这类个人信息，属于不该无条件落盘的内容。
//
// 实现完整的 logger.Interface（而不是只提供一个 Writer）是必要的：
// Writer 只有一个 Printf，拿不到级别信息，所有内容都会变成同一个级别；
// 而 GORM 的级别语义（慢查询 vs 报错）恰恰是我们想保留的。
type zapGormLogger struct {
	level          logger.LogLevel
	slow           time.Duration
	ignoreNotFound bool
}

// newGormLogger 构造接进 zap 的 GORM logger。
//
// ignoreNotFound 为 true 时不记录 gorm.ErrRecordNotFound：在本项目里
// "账号/视频不存在"是正常业务结果（会返回 404），每次登录失败都记一条
// error 级 SQL 日志，会把真正的问题淹没。
func newGormLogger(level logger.LogLevel, slow time.Duration, ignoreNotFound bool) logger.Interface {
	return &zapGormLogger{level: level, slow: slow, ignoreNotFound: ignoreNotFound}
}

// gormLevelForAppLog 让 GORM 的 SQL 日志跟随应用的日志级别：
// 只有把 log.level 调到 debug 时才输出逐条 SQL，其余情况只记慢查询与错误。
// 这样"排查时才打开 SQL 日志"成为配置行为，而不是改代码。
func gormLevelForAppLog() logger.LogLevel {
	if logging.L().Core().Enabled(zapcore.DebugLevel) {
		return logger.Info
	}
	return logger.Warn
}

func (l *zapGormLogger) LogMode(level logger.LogLevel) logger.Interface {
	cp := *l
	cp.level = level
	return &cp
}

func (l *zapGormLogger) Info(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Info {
		logging.Ctx(ctx).Sugar().Debugf(msg, args...)
	}
}

func (l *zapGormLogger) Warn(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Warn {
		logging.Ctx(ctx).Sugar().Warnf(msg, args...)
	}
}

func (l *zapGormLogger) Error(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Error {
		logging.Ctx(ctx).Sugar().Errorf(msg, args...)
	}
}

// Trace 是 SQL 日志的唯一入口，级别判定都在这里。
func (l *zapGormLogger) Trace(ctx context.Context, begin time.Time,
	fc func() (sql string, rowsAffected int64), err error) {

	if l.level <= logger.Silent {
		return
	}

	elapsed := time.Since(begin)
	// 只有真的要看 SQL 时才调用 fc()：它负责把参数渲染进语句，
	// 在默认级别下不调用就不会产生这笔开销，也不会让参数进入日志。
	logSQL := func() string {
		sql, rows := fc()
		return strings.TrimSpace(sql) + " rows=" + strconv.FormatInt(rows, 10)
	}

	switch {
	case err != nil && l.level >= logger.Error &&
		!(l.ignoreNotFound && errors.Is(err, logger.ErrRecordNotFound)):
		logging.Ctx(ctx).Error("database error",
			zap.Error(err), zap.Duration("elapsed", elapsed), zap.String("sql", logSQL()))

	case l.slow > 0 && elapsed > l.slow && l.level >= logger.Warn:
		logging.Ctx(ctx).Warn("slow query",
			zap.Duration("elapsed", elapsed), zap.Duration("threshold", l.slow),
			zap.String("sql", logSQL()))

	case l.level >= logger.Info:
		// 常规查询落到 Debug：默认 info 级别下不输出，需要排查时调
		// log.level=debug 才打开。SQL 与参数不应无条件落盘。
		logging.Ctx(ctx).Debug("sql", zap.Duration("elapsed", elapsed), zap.String("sql", logSQL()))
	}
}
