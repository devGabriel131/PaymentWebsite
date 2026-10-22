package logging

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/logger"
)

type databasePrivateData struct{}

func (databasePrivateData) Error() string  { panic("PRIVATE database error was formatted") }
func (databasePrivateData) String() string { panic("PRIVATE ORM data was formatted") }

func TestDBLoggerLogMode(t *testing.T) {
	for _, tt := range []struct {
		name    string
		level   logger.LogLevel
		enabled bool
	}{
		{"silent", logger.Silent, false},
		{"error", logger.Error, false},
		{"warn", logger.Warn, true},
		{"info", logger.Info, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			base := NewDBLogger(New(&out))
			view := base.LogMode(tt.level).(*DBLogger)
			muted := view.LogMode(logger.Silent).(*DBLogger)
			ctx := WithOperation(context.Background(), "link_read")

			base.observe(ctx, 250*time.Millisecond, nil)
			view.observe(ctx, 350*time.Millisecond, nil)
			muted.observe(ctx, time.Second, nil)
			view.Info(ctx, "PRIVATE ORM info %v", databasePrivateData{})
			view.Error(ctx, "PRIVATE ORM error %v", databasePrivateData{})
			view.Warn(ctx, "PRIVATE ORM warning %v", databasePrivateData{})
			muted.Warn(ctx, "PRIVATE muted warning %v", databasePrivateData{})
			view.Flush()

			events := decodeEvents(t, out.Bytes())
			wantEvents, wantCount, wantWorst := 1, uint64(1), 250*time.Millisecond
			if tt.enabled {
				wantEvents, wantCount, wantWorst = 2, 2, 350*time.Millisecond
			}
			if len(events) != wantEvents {
				t.Fatalf("events = %d, want %d", len(events), wantEvents)
			}
			if tt.enabled {
				warning := events[0]
				if warning["message"] != "orm_warning" || warning["level"] != "warn" {
					t.Fatalf("warning = %#v", warning)
				}
				for key := range warning {
					if key != "time" && key != "message" && key != "level" {
						t.Errorf("unsafe ORM warning attribute %q", key)
					}
				}
			}
			assertDatabaseSummary(t, events[len(events)-1], "link_read", wantCount, wantWorst)
			if bytes.Contains(out.Bytes(), []byte("PRIVATE")) {
				t.Fatal("private ORM arguments appeared in output")
			}
			before := out.Len()
			base.Flush()
			muted.Flush()
			if out.Len() != before {
				t.Fatal("LogMode views did not share reset counters")
			}
		})
	}
}

func TestDBLoggerTraceNeverEvaluatesSQL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		level   logger.LogLevel
		enabled bool
	}{
		{"silent", logger.Silent, false},
		{"error", logger.Error, false},
		{"warn", logger.Warn, true},
		{"info", logger.Info, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			base := NewDBLogger(New(&out))
			view := base.LogMode(tt.level)
			ctx := WithOperation(context.Background(), "link_read")
			callback := func() (string, int64) {
				t.Fatal("Trace evaluated the SQL callback")
				return "SELECT PRIVATE SQL AND BIND VALUES", 42
			}
			// Dates beyond time.Duration's range make elapsed values deterministic.
			future := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			view.Trace(ctx, future, callback, nil)
			view.Trace(ctx, time.Time{}, callback, databasePrivateData{})
			view.Trace(ctx, time.Time{}, callback, nil)
			base.Flush()

			events := decodeEvents(t, out.Bytes())
			if !tt.enabled {
				if len(events) != 0 {
					t.Fatalf("disabled mode emitted %d events", len(events))
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			assertDatabaseSummary(t, events[0], "link_read", 1, time.Duration(1<<63-1))
		})
	}
}

func TestDBLoggerObserve(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation string
		elapsed   time.Duration
		err       error
		want      bool
		wantOp    string
	}{
		{name: "fast", operation: "link_read", elapsed: 199 * time.Millisecond},
		{name: "exact boundary", operation: "link_read", elapsed: 200 * time.Millisecond},
		{name: "strictly above boundary", operation: "link_read", elapsed: 200*time.Millisecond + time.Nanosecond, want: true, wantOp: "link_read"},
		{name: "slow success", operation: "link_read", elapsed: 750*time.Millisecond + 250*time.Microsecond, want: true, wantOp: "link_read"},
		{name: "slow failure", operation: "link_read", elapsed: time.Second, err: databasePrivateData{}},
		{name: "unknown operation", operation: "PRIVATE_UNKNOWN_OPERATION", elapsed: 400 * time.Millisecond, want: true, wantOp: "other"},
		{name: "missing operation", elapsed: 500 * time.Millisecond, want: true, wantOp: "other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			db := NewDBLogger(New(&out))
			ctx := context.Background()
			if tt.operation != "" {
				ctx = WithOperation(ctx, tt.operation)
			}
			db.observe(ctx, tt.elapsed, tt.err)
			if out.Len() != 0 {
				t.Fatal("observe emitted before Flush")
			}
			db.Flush()
			events := decodeEvents(t, out.Bytes())
			if !tt.want {
				if len(events) != 0 {
					t.Fatalf("events = %d, want none", len(events))
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			assertDatabaseSummary(t, events[0], tt.wantOp, 1, tt.elapsed)
		})
	}
}

func TestDBLoggerFlushBoundsAndResets(t *testing.T) {
	var out bytes.Buffer
	db := NewDBLogger(New(&out))
	db.Flush()
	if out.Len() != 0 {
		t.Fatal("initial empty flush emitted an event")
	}
	for _, operation := range operationNames {
		ctx := WithOperation(context.Background(), operation)
		for _, elapsed := range []time.Duration{201 * time.Millisecond, 450*time.Millisecond + 250*time.Microsecond, 300 * time.Millisecond} {
			db.observe(ctx, elapsed, nil)
		}
	}
	unknown := WithOperation(context.Background(), "PRIVATE_UNKNOWN_OPERATION")
	for range 1000 {
		db.observe(unknown, 600*time.Millisecond+500*time.Microsecond, nil)
	}
	if out.Len() != 0 {
		t.Fatal("slow observations emitted before Flush")
	}
	db.Flush()
	events := decodeEvents(t, out.Bytes())
	buckets := len(operationNames)
	if len(events) != buckets {
		t.Fatalf("summary events = %d, want exactly %d bounded buckets", len(events), buckets)
	}
	seen := make(map[string]bool)
	for _, event := range events {
		operation, ok := event["operation"].(string)
		if !ok || seen[operation] {
			t.Fatalf("missing or duplicate operation: %#v", event)
		}
		seen[operation] = true
		count, worst := uint64(3), 450*time.Millisecond+250*time.Microsecond
		if operation == "other" {
			count, worst = 1003, 600*time.Millisecond+500*time.Microsecond
		}
		assertDatabaseSummary(t, event, operation, count, worst)
	}
	for _, operation := range operationNames {
		if !seen[operation] {
			t.Errorf("missing summary for %q", operation)
		}
	}
	before := out.Len()
	db.Flush()
	if out.Len() != before {
		t.Fatal("empty flush repeated previous summaries")
	}

	db.observe(WithOperation(context.Background(), "link_read"), 201*time.Millisecond+750*time.Microsecond, nil)
	db.Flush()
	events = decodeEvents(t, out.Bytes())
	if len(events) != buckets+1 {
		t.Fatalf("events after final partial flush = %d, want %d", len(events), buckets+1)
	}
	assertDatabaseSummary(t, events[buckets], "link_read", 1, 201*time.Millisecond+750*time.Microsecond)
	before = out.Len()
	db.Flush()
	if out.Len() != before {
		t.Fatal("empty final flush emitted an event")
	}
}

func TestDBLoggerConcurrentObserveAndFlush(t *testing.T) {
	var out bytes.Buffer
	db := NewDBLogger(New(&out))
	const workers, observations = 8, 200
	type total struct {
		count uint64
		worst time.Duration
	}
	want := make(map[string]total)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := range workers {
		operation := operationNames[worker%3]
		elapsed := time.Duration(250+worker) * time.Millisecond
		ctx := WithOperation(context.Background(), operation)
		expected := want[operation]
		expected.count += observations
		if elapsed > expected.worst {
			expected.worst = elapsed
		}
		want[operation] = expected
		wg.Go(func() {
			<-start
			for observation := range observations {
				db.observe(ctx, elapsed, nil)
				if observation%25 == 0 {
					db.Flush()
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			<-start
			for range 100 {
				db.Flush()
			}
		})
	}
	close(start)
	wg.Wait()
	db.Flush()

	got := make(map[string]total)
	for _, event := range decodeEvents(t, out.Bytes()) {
		operation, ok := event["operation"].(string)
		if !ok {
			t.Fatalf("operation missing: %#v", event)
		}
		if _, ok := want[operation]; !ok {
			t.Fatalf("unexpected operation %q", operation)
		}
		count, countOK := event["count"].(float64)
		worst, worstOK := event["worst_duration_ms"].(float64)
		if !countOK || count <= 0 || count != float64(uint64(count)) || !worstOK {
			t.Fatalf("invalid summary values: %#v", event)
		}
		duration := time.Duration(worst * float64(time.Millisecond))
		assertDatabaseSummary(t, event, operation, uint64(count), duration)
		if duration < 250*time.Millisecond || duration > want[operation].worst {
			t.Errorf("summary worst = %v, outside observed range for %s", duration, operation)
		}
		total := got[operation]
		total.count += uint64(count)
		if duration > total.worst {
			total.worst = duration
		}
		got[operation] = total
	}
	for operation, expected := range want {
		if got[operation] != expected {
			t.Errorf("aggregate for %s = %#v, want %#v", operation, got[operation], expected)
		}
	}
	before := out.Len()
	db.Flush()
	if out.Len() != before {
		t.Fatal("final empty flush repeated concurrent observations")
	}
}

func assertDatabaseSummary(t *testing.T, event map[string]any, operation string, count uint64, worst time.Duration) {
	t.Helper()
	for key, want := range map[string]any{
		"message":           "database_slow_summary",
		"level":             "warn",
		"operation":         operation,
		"count":             float64(count),
		"worst_duration_ms": float64(worst) / float64(time.Millisecond),
		"threshold_ms":      float64(200),
	} {
		if event[key] != want {
			t.Errorf("%s = %#v, want %#v", key, event[key], want)
		}
	}
	for key := range event {
		switch key {
		case "time", "message", "level", "operation", "count", "worst_duration_ms", "threshold_ms":
		default:
			t.Errorf("unexpected database summary attribute %q", key)
		}
	}
}
