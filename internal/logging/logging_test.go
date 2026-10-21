package logging

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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
		{name: "info", level: slog.LevelInfo, want: "info"},
		{name: "warn", level: slog.LevelWarn, want: "warn"},
		{name: "error", level: slog.LevelError, want: "error"},
		{name: "custom level", level: slog.LevelWarn + 1, want: "warn+1"},
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

type privateNetworkError struct {
	TimedOut bool
	Marker   string
}

func (*privateNetworkError) Error() string   { panic("logging must not call Error") }
func (e *privateNetworkError) Timeout() bool { return e.TimedOut }
func (*privateNetworkError) Temporary() bool { return false }

type privateIsError struct {
	Target error
	Marker string
}

func (*privateIsError) Error() string          { panic("logging must not call Error") }
func (e *privateIsError) Is(target error) bool { return target == e.Target }

type privateAsError struct {
	Matched *privateSQLStateError
	Marker  string
}

func (*privateAsError) Error() string { panic("logging must not call Error") }
func (e *privateAsError) As(target any) bool {
	if state, ok := target.(*interface {
		SQLState() string
	}); ok {
		*state = e.Matched
		return true
	}
	return false
}

func TestErrorAttrsClassificationAndPrivacy(t *testing.T) {
	t.Parallel()
	wrap := func(err error) error {
		return &privateLoggingWrapper{Err: &privateLoggingWrapper{Err: err, Marker: loggingPrivateMarker}, Marker: loggingPrivateMarker}
	}
	database := &privateSQLStateError{State: "23505", Marker: loggingPrivateMarker}
	timeout := &privateNetworkError{TimedOut: true, Marker: loggingPrivateMarker}
	network := &privateNetworkError{Marker: loggingPrivateMarker}
	unknown := &privateLoggingError{Marker: loggingPrivateMarker}
	wrappedUnknown := wrap(unknown)
	customCanceled := &privateIsError{Target: context.Canceled, Marker: loggingPrivateMarker}
	customAs := &privateAsError{Matched: database, Marker: loggingPrivateMarker}
	netOperation := &net.OpError{Op: loggingPrivateMarker, Net: loggingPrivateMarker, Err: network}
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
		{name: "network timeout", err: wrap(timeout), kind: "deadline_exceeded", matched: timeout},
		{name: "cancellation priority", err: wrap(errors.Join(database, context.Canceled, context.DeadlineExceeded)), kind: "canceled", matched: context.Canceled},
		{name: "deadline priority", err: wrap(errors.Join(database, context.DeadlineExceeded)), kind: "deadline_exceeded", matched: context.DeadlineExceeded},
		{name: "timeout priority", err: wrap(errors.Join(database, timeout)), kind: "deadline_exceeded", matched: timeout},
		{name: "reset before timeout", err: wrap(errors.Join(syscall.ECONNRESET, timeout)), kind: "deadline_exceeded", matched: timeout},
		{name: "custom Is match", err: wrap(customCanceled), kind: "canceled", matched: customCanceled},
		{name: "custom As match", err: wrap(customAs), kind: "database", matched: database, state: "23505"},
		{name: "database", err: wrap(database), kind: "database", matched: database, state: "23505"},
		{name: "database before transaction", err: wrap(errors.Join(sql.ErrTxDone, database)), kind: "database", matched: database, state: "23505"},
		{name: "database before network", err: wrap(errors.Join(network, database)), kind: "database", matched: database, state: "23505"},
		{name: "transaction done", err: wrap(sql.ErrTxDone), kind: "transaction_done", matched: sql.ErrTxDone},
		{name: "SQL connection done", err: wrap(sql.ErrConnDone), kind: "connection_failure", matched: sql.ErrConnDone},
		{name: "driver connection", err: wrap(driver.ErrBadConn), kind: "connection_failure", matched: driver.ErrBadConn},
		{name: "closed network", err: wrap(net.ErrClosed), kind: "connection_failure", matched: net.ErrClosed},
		{name: "closed pipe", err: wrap(io.ErrClosedPipe), kind: "connection_failure", matched: io.ErrClosedPipe},
		{name: "broken pipe", err: wrap(syscall.EPIPE), kind: "connection_failure", matched: syscall.EPIPE},
		{name: "reset", err: wrap(syscall.ECONNRESET), kind: "connection_failure", matched: syscall.ECONNRESET},
		{name: "refused", err: wrap(syscall.ECONNREFUSED), kind: "connection_failure", matched: syscall.ECONNREFUSED},
		{name: "network", err: wrap(network), kind: "connection_failure", matched: network},
		{name: "network operation", err: wrap(netOperation), kind: "connection_failure", matched: netOperation},
		{name: "EOF", err: wrap(io.EOF), kind: "connection_failure", matched: io.EOF},
		{name: "unexpected EOF", err: wrap(io.ErrUnexpectedEOF), kind: "connection_failure", matched: io.ErrUnexpectedEOF},
		{name: "unknown", err: unknown, kind: "unknown", matched: unknown},
		{name: "unknown retains original type", err: wrappedUnknown, kind: "unknown", matched: wrappedUnknown},
	}
	for _, state := range []string{"08006", "08P01", "57P01", "57P02", "57P03", "57014", "42P01", "ABCDE", "12345", "", "42P0", "42P010", "42p01", "42P0_", "42Pé", loggingPrivateMarker, "42P01\nSELECT private_table"} {
		err := &privateSQLStateError{State: state, Marker: loggingPrivateMarker}
		kind, safeState := "unknown", ""
		switch state {
		case "08006", "08P01", "57P01", "57P02", "57P03":
			kind, safeState = "connection_failure", state
		case "57014":
			kind, safeState = "query_canceled", state
		case "42P01", "ABCDE", "12345":
			kind, safeState = "database", state
		}
		matched := error(err)
		wrapped := wrap(err)
		if kind == "unknown" {
			matched = wrapped
		}
		cases = append(cases, struct {
			name    string
			err     error
			kind    string
			matched error
			state   string
		}{name: "SQLSTATE " + state, err: wrapped, kind: kind, matched: matched, state: safeState})
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
		{name: "nil", stage: "render", want: slog.LevelError},
		{name: "canceled save", stage: "save", err: context.Canceled, want: slog.LevelWarn},
		{name: "canceled render", stage: "render", err: context.Canceled, want: slog.LevelWarn},
		{name: "render broken pipe", stage: "render", err: syscall.EPIPE, want: slog.LevelWarn},
		{name: "render reset", stage: "render", err: syscall.ECONNRESET, want: slog.LevelWarn},
		{name: "render closed connection", stage: "render", err: net.ErrClosed, want: slog.LevelWarn},
		{name: "render closed pipe", stage: "render", err: io.ErrClosedPipe, want: slog.LevelWarn},
		{name: "save reset", stage: "save", err: syscall.ECONNRESET, want: slog.LevelError},
		{name: "save broken pipe", stage: "save", err: syscall.EPIPE, want: slog.LevelError},
		{name: "save closed connection", stage: "save", err: net.ErrClosed, want: slog.LevelError},
		{name: "render refused connection", stage: "render", err: syscall.ECONNREFUSED, want: slog.LevelError},
		{name: "render database connection", stage: "render", err: sql.ErrConnDone, want: slog.LevelError},
		{name: "render EOF", stage: "render", err: io.EOF, want: slog.LevelError},
		{name: "render deadline", stage: "render", err: context.DeadlineExceeded, want: slog.LevelError},
		{name: "render timeout", stage: "render", err: &privateNetworkError{TimedOut: true}, want: slog.LevelError},
		{name: "render unknown network", stage: "render", err: &privateNetworkError{}, want: slog.LevelError},
		{name: "render unknown", stage: "render", err: &privateLoggingError{}, want: slog.LevelError},
		{name: "render query canceled", stage: "render", err: &privateSQLStateError{State: "57014"}, want: slog.LevelError},
		{name: "render SQLSTATE reset", stage: "render", err: errors.Join(syscall.ECONNRESET, &privateSQLStateError{State: "08006"}), want: slog.LevelError},
		{name: "render deadline reset", stage: "render", err: errors.Join(syscall.ECONNRESET, context.DeadlineExceeded), want: slog.LevelError},
		{name: "render timeout reset", stage: "render", err: errors.Join(syscall.ECONNRESET, &privateNetworkError{TimedOut: true}), want: slog.LevelError},
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

func TestServerErrorWriterAcknowledgesWholeRecord(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	record := []byte("http: panic serving 192.0.2.1:4321: " + loggingPrivateMarker + "\nprivate stack\n")
	n, err := ServerErrorLog(New(&out)).Writer().Write(record)
	if n != len(record) || err != nil {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(record))
	}
	if events := decodeEvents(t, out.Bytes()); len(events) != 1 || events[0]["kind"] != "panic" {
		t.Fatalf("unexpected acknowledged record: %v", events)
	}
}
