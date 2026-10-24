package logging

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"syscall"
	"testing"
)

func decodeEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("log line %d is not a JSON object: %v", i+1, err)
		}
		if event == nil {
			t.Fatalf("log line %d is not a JSON object", i+1)
		}
		events = append(events, event)
	}
	return events
}

func TestNewJSONSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		level slog.Level
		want  string
	}{
		{name: "debug filtered", level: slog.LevelDebug},
		{name: "warn", level: slog.LevelWarn, want: "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			New(&out).LogAttrs(t.Context(), tc.level, "database_slow_summary", slog.Float64("duration_ms", 12.5))
			events := decodeEvents(t, out.Bytes())
			if tc.want == "" {
				if len(events) != 0 || out.Len() != 0 {
					t.Fatal("debug output must be filtered")
				}
				return
			}
			if len(events) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
				t.Fatal("event must be one newline-terminated JSON object")
			}
			event := events[0]
			if event["message"] != "database_slow_summary" || event["level"] != tc.want || event["duration_ms"] != float64(12.5) {
				t.Fatalf("unexpected event semantics: %v", event)
			}
			for _, forbidden := range []string{"event", "msg", "source"} {
				if _, exists := event[forbidden]; exists {
					t.Errorf("unexpected %q field", forbidden)
				}
			}
		})
	}
}

const loggingPrivateMarker = "PRIVATE_sql_cookie_password_token@example.invalid"

type privateLoggingError struct {
	Marker string
}

func (*privateLoggingError) Error() string { panic("logging must not call Error") }

type privateLoggingWrapper struct {
	Err    error
	Marker string
}

func (*privateLoggingWrapper) Error() string   { panic("logging must not call Error") }
func (e *privateLoggingWrapper) Unwrap() error { return e.Err }

type privateSQLStateError struct {
	State  string
	Marker string
}

func (*privateSQLStateError) Error() string      { panic("logging must not call Error") }
func (e *privateSQLStateError) SQLState() string { return e.State }

func TestErrorAttrsClassificationAndPrivacy(t *testing.T) {
	t.Parallel()
	wrap := func(err error) error {
		return &privateLoggingWrapper{Err: &privateLoggingWrapper{Err: err, Marker: loggingPrivateMarker}, Marker: loggingPrivateMarker}
	}
	database := &privateSQLStateError{State: "23505", Marker: loggingPrivateMarker}
	wrappedUnknown := wrap(&privateLoggingError{Marker: loggingPrivateMarker})
	connectionFailure := &privateSQLStateError{State: "08006", Marker: loggingPrivateMarker}
	queryCanceled := &privateSQLStateError{State: "57014", Marker: loggingPrivateMarker}
	markerState := wrap(&privateSQLStateError{State: loggingPrivateMarker, Marker: loggingPrivateMarker})
	injectedState := wrap(&privateSQLStateError{State: "42P01\nSELECT private_table", Marker: loggingPrivateMarker})
	cases := []struct {
		name    string
		err     error
		kind    string
		matched error
		state   string
	}{
		{name: "nil"},
		{name: "wrapped cancellation", err: wrap(context.Canceled), kind: "canceled", matched: context.Canceled},
		{name: "wrapped deadline", err: wrap(context.DeadlineExceeded), kind: "deadline_exceeded", matched: context.DeadlineExceeded},
		{name: "database", err: wrap(database), kind: "database", matched: database, state: "23505"},
		{name: "transaction done", err: wrap(sql.ErrTxDone), kind: "transaction_done", matched: sql.ErrTxDone},
		{name: "driver connection", err: wrap(driver.ErrBadConn), kind: "connection_failure", matched: driver.ErrBadConn},
		{name: "unknown retains original type", err: wrappedUnknown, kind: "unknown", matched: wrappedUnknown},
		{name: "SQLSTATE 08006", err: wrap(connectionFailure), kind: "connection_failure", matched: connectionFailure, state: "08006"},
		{name: "SQLSTATE 57014", err: wrap(queryCanceled), kind: "query_canceled", matched: queryCanceled, state: "57014"},
		{name: "SQLSTATE marker", err: markerState, kind: "unknown", matched: markerState},
		{name: "SQLSTATE injection", err: injectedState, kind: "unknown", matched: injectedState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attrs := ErrorAttrs(tc.err)
			if tc.err == nil && attrs != nil {
				t.Fatal("nil error must produce nil attributes")
			}
			var out bytes.Buffer
			New(&out).LogAttrs(t.Context(), requestLevel("save", tc.err), "request_failed", attrs...)
			events := decodeEvents(t, out.Bytes())
			if len(events) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
				t.Fatal("failure must produce one newline-terminated JSON event")
			}
			event := events[0]
			wantLevel := "error"
			if tc.kind == "canceled" {
				wantLevel = "warn"
			}
			if event["message"] != "request_failed" || event["level"] != wantLevel {
				t.Fatalf("unexpected event semantics: %v", event)
			}
			wantFields := 3
			if tc.err != nil {
				wantFields += 2
				if event["error_kind"] != tc.kind || event["error_type"] != reflect.TypeOf(tc.matched).String() {
					t.Fatalf("wrong safe classification: %v", event)
				}
			}
			if tc.state != "" {
				wantFields++
				if event["sqlstate"] != tc.state {
					t.Fatalf("sqlstate = %v, want %q", event["sqlstate"], tc.state)
				}
			} else if _, exists := event["sqlstate"]; exists {
				t.Fatal("invalid or unmatched SQLSTATE must be omitted")
			}
			if len(event) != wantFields {
				t.Fatalf("unexpected attributes: %v", event)
			}
			for _, forbidden := range []string{loggingPrivateMarker, "SELECT", "private_table", "logging must not call Error"} {
				if bytes.Contains(out.Bytes(), []byte(forbidden)) {
					t.Errorf("serialized private error content %q", forbidden)
				}
			}
		})
	}
}

func TestRequestLevelBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage string
		err   error
		want  slog.Level
	}{
		{name: "canceled save", stage: "save", err: context.Canceled, want: slog.LevelWarn},
		{name: "render broken pipe", stage: "render", err: syscall.EPIPE, want: slog.LevelWarn},
		{name: "save broken pipe", stage: "save", err: syscall.EPIPE, want: slog.LevelError},
		{name: "render SQLSTATE reset", stage: "render", err: errors.Join(syscall.ECONNRESET, &privateSQLStateError{State: "08006"}), want: slog.LevelError},
		{name: "render deadline reset", stage: "render", err: errors.Join(syscall.ECONNRESET, context.DeadlineExceeded), want: slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.err
			if err != nil {
				err = &privateLoggingWrapper{Err: err, Marker: loggingPrivateMarker}
			}
			if got := requestLevel(tc.stage, err); got != tc.want {
				t.Errorf("level = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestServerErrorLogPrivacy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		record string
		kind   string
	}{
		{name: "panic", record: "http: panic serving 192.0.2.1:4321: " + loggingPrivateMarker + "\ngoroutine 45 [running]:\nprivateHandler()\n\tprivate.go:42\n", kind: "panic"},
		{name: "accept", record: "http: Accept error: " + loggingPrivateMarker + " 192.0.2.1:4321; retrying in 5ms\nprivate.go:42", kind: "accept"},
		{name: "superfluous header", record: "http: superfluous response.WriteHeader call from " + loggingPrivateMarker + " (private.go:42)\nprivate stack", kind: "response_header"},
		{name: "hijacked header", record: "http: response.WriteHeader on hijacked connection from " + loggingPrivateMarker + " (private.go:42)\nprivate stack", kind: "response_header"},
		{name: "content length", record: "http: invalid Content-Length of " + loggingPrivateMarker + "\nprivate stack", kind: "response_header"},
		{name: "transfer encoding", record: "http: WriteHeader called with both Transfer-Encoding of " + loggingPrivateMarker + " and a Content-Length of 99\nprivate stack", kind: "response_header"},
		{name: "TLS", record: "http: TLS handshake error from 192.0.2.1:4321: " + loggingPrivateMarker + "\nprivate.go:42", kind: "other"},
		{name: "embedded prefix", record: loggingPrivateMarker + "\nhttp: panic serving 192.0.2.1:4321: panic\nprivate.go:42", kind: "other"},
		{name: "unknown", record: loggingPrivateMarker + "\nprivate.go:42", kind: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			serverLog := ServerErrorLog(New(&out))
			if serverLog.Prefix() != "" || serverLog.Flags() != 0 {
				t.Fatal("stdlib logger must add no prefix or flags")
			}
			serverLog.Print(tc.record)
			events := decodeEvents(t, out.Bytes())
			if len(events) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
				t.Fatal("one stdlib record must emit one JSON line")
			}
			event := events[0]
			if len(event) != 4 || event["level"] != "error" || event["message"] != "http_server_error" || event["kind"] != tc.kind {
				t.Fatalf("unexpected server diagnostic: %v", event)
			}
			for _, forbidden := range []string{loggingPrivateMarker, "192.0.2.1", "private.go", "goroutine", "privateHandler", "private stack"} {
				if bytes.Contains(out.Bytes(), []byte(forbidden)) {
					t.Errorf("serialized raw diagnostic content %q", forbidden)
				}
			}
		})
	}
}
