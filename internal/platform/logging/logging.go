// internal/logger/logger.go
//
// Structured JSON logger (Zap + Lumberjack) **plus** tiny adapter so call-sites
// can use logger.FromContext(ctx).Error(…) / Warn(…) without depending on Zap.
//
// Context
//   • Zap core writes one JSON file per day under logs/YYYY-MM-DD.log.
//   • Colored tee to stdout when running in an interactive TTY.
//   • logger.WithContext(ctx, l) embeds a request-scoped logger.
//   • logger.FromContext(ctx) fetches that or falls back to the global.
//
// Two-space sentence spacing, Oxford comma per style guide.
//
//------------------------------------------------------------------------------

package logging

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/natefinch/lumberjack"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

//
// Core setup (unchanged)
//

func New(rootDir string, tee bool) (*zap.SugaredLogger, error) {
	logDir := filepath.Join(rootDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}

	fileName := time.Now().Format("2006-01-02") + ".log"
	fileSink := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, fileName),
		MaxSize:    50,
		MaxBackups: 7,
		MaxAge:     14,
		Compress:   true,
	}

	encCfg := zapcore.EncoderConfig{
		TimeKey:      "ts",
		LevelKey:     "level",
		MessageKey:   "msg",
		CallerKey:    "caller",
		EncodeTime:   zapcore.ISO8601TimeEncoder,
		EncodeLevel:  zapcore.LowercaseLevelEncoder,
		EncodeCaller: zapcore.ShortCallerEncoder,
	}

	jsonCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encCfg),
		zapcore.AddSync(fileSink),
		zap.InfoLevel,
	)

	cores := []zapcore.Core{jsonCore}
	if tee {
		consoleCore := zapcore.NewCore(
			zapcore.NewConsoleEncoder(encCfg),
			zapcore.AddSync(os.Stdout),
			zap.InfoLevel,
		)
		cores = append(cores, consoleCore)
	}

	z := zap.New(
		zapcore.NewTee(cores...),
		zap.ErrorOutput(zapcore.AddSync(fileSink)),
	).Sugar()

	zap.ReplaceGlobals(z.Desugar()) // global fallback

	z.Infow("logger online", "tee", tee)
	return z, nil
}

//
// Adapter layer: Logger interface + context helpers
//

// Logger is the minimal interface expected by call-sites.
type Logger interface {
	Error(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

// zapAdapter wraps *zap.SugaredLogger to satisfy Logger.
type zapAdapter struct{ *zap.SugaredLogger }

func (l zapAdapter) Error(msg string, kv ...any) { l.Errorw(msg, kv...) }
func (l zapAdapter) Warn(msg string, kv ...any)  { l.Warnw(msg, kv...) }

type ctxKey struct{}

// WithContext embeds l into ctx, returning the derived context.
func WithContext(ctx context.Context, l Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the Logger stored in ctx, or the global zap.S() adapter.
func FromContext(ctx context.Context) Logger {
	if ctx != nil {
		if v, ok := ctx.Value(ctxKey{}).(Logger); ok {
			return v
		}
	}
	return zapAdapter{zap.S()}
}
