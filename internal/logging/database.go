package logging

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm/logger"
)

const slowQueryThreshold = 200 * time.Millisecond

type slowQueryBucket struct {
	count uint64
	worst time.Duration
}

// DBLogger reports bounded slow-success summaries without evaluating SQL callbacks.
type DBLogger struct {
	log     *slog.Logger
	mu      sync.Mutex
	buckets map[string]slowQueryBucket
}

var _ logger.Interface = (*DBLogger)(nil)

func NewDBLogger(log *slog.Logger) *DBLogger {
	return &DBLogger{log: log, buckets: map[string]slowQueryBucket{}}
}

// LogMode ignores levels: SQL is never logged.
func (l *DBLogger) LogMode(logger.LogLevel) logger.Interface { return l }

func (l *DBLogger) Info(context.Context, string, ...any)  {}
func (l *DBLogger) Error(context.Context, string, ...any) {}

func (l *DBLogger) Warn(ctx context.Context, _ string, _ ...any) {
	l.log.WarnContext(ctx, "orm_warning")
}

// Trace counts slow successful queries per label; failed ones are reported by their callers.
func (l *DBLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	elapsed := time.Since(begin)
	if err != nil || elapsed <= slowQueryThreshold {
		return
	}
	label := labelFromContext(ctx)
	l.mu.Lock()
	bucket := l.buckets[label]
	bucket.count++
	bucket.worst = max(bucket.worst, elapsed)
	l.buckets[label] = bucket
	l.mu.Unlock()
}

// Flush atomically takes all counters, then emits at most one warning per label.
func (l *DBLogger) Flush() {
	l.mu.Lock()
	buckets := l.buckets
	l.buckets = map[string]slowQueryBucket{}
	l.mu.Unlock()

	for _, label := range slices.Sorted(maps.Keys(buckets)) {
		bucket := buckets[label]
		l.log.LogAttrs(context.Background(), slog.LevelWarn, "database_slow_summary",
			slog.String("operation", label),
			slog.Uint64("count", bucket.count),
			slog.Float64("worst_duration_ms", float64(bucket.worst)/float64(time.Millisecond)),
			slog.Int64("threshold_ms", int64(slowQueryThreshold/time.Millisecond)),
		)
	}
}
