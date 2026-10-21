package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm/logger"
)

const slowQueryThreshold = 200 * time.Millisecond

type slowQueryBucket struct {
	count uint64
	worst time.Duration
}

type databaseAggregate struct {
	mu      sync.Mutex
	buckets [len(operationNames)]slowQueryBucket
}

// DBLogger reports bounded slow-success summaries without evaluating SQL callbacks.
type DBLogger struct {
	log       *slog.Logger
	level     logger.LogLevel
	aggregate *databaseAggregate
}

var _ logger.Interface = (*DBLogger)(nil)

// NewDBLogger creates a Warn-level GORM logger with shared summary counters.
func NewDBLogger(log *slog.Logger) *DBLogger {
	return &DBLogger{log: log, level: logger.Warn, aggregate: &databaseAggregate{}}
}

// LogMode returns an immutable level view sharing the original summary counters.
func (l *DBLogger) LogMode(level logger.LogLevel) logger.Interface {
	return &DBLogger{log: l.log, level: level, aggregate: l.aggregate}
}

func (l *DBLogger) Info(ctx context.Context, format string, data ...interface{}) {
	logger.Discard.Info(ctx, format, data...)
}

func (l *DBLogger) Warn(ctx context.Context, _ string, _ ...interface{}) {
	if l.level >= logger.Warn {
		l.log.WarnContext(ctx, "orm_warning")
	}
}

func (l *DBLogger) Error(ctx context.Context, format string, data ...interface{}) {
	logger.Discard.Error(ctx, format, data...)
}

func (l *DBLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	if err != nil || l.level < logger.Warn {
		return
	}
	l.observe(ctx, time.Since(begin), err)
}

func (l *DBLogger) observe(ctx context.Context, elapsed time.Duration, err error) {
	if err != nil || l.level < logger.Warn || elapsed <= slowQueryThreshold {
		return
	}
	operation := operationFromContext(ctx)
	l.aggregate.mu.Lock()
	bucket := &l.aggregate.buckets[operation]
	bucket.count++
	if elapsed > bucket.worst {
		bucket.worst = elapsed
	}
	l.aggregate.mu.Unlock()
}

// Flush atomically takes all counters, then emits at most one warning per operation.
func (l *DBLogger) Flush() {
	l.aggregate.mu.Lock()
	buckets := l.aggregate.buckets
	l.aggregate.buckets = [len(operationNames)]slowQueryBucket{}
	l.aggregate.mu.Unlock()

	for operation, bucket := range buckets {
		if bucket.count == 0 {
			continue
		}
		l.log.LogAttrs(context.Background(), slog.LevelWarn, "database_slow_summary",
			slog.String("operation", operationNames[operation]),
			slog.Uint64("count", bucket.count),
			slog.Float64("worst_duration_ms", float64(bucket.worst)/float64(time.Millisecond)),
			slog.Int64("threshold_ms", int64(slowQueryThreshold/time.Millisecond)),
		)
	}
}
