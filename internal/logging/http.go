package logging

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

var operationNames = [...]string{
	"link_read",
	"final_submit",
	"draft_save",
	"link_list",
	"trash_list",
	"link_create",
	"link_cancel",
	"link_cancel_all",
	"settings_load",
	"settings_update",
	"student_list",
	"student_get",
	"student_field_get",
	"student_update",
	"live_updates",
	"sign_in",
	"sign_out",
	"session_check",
	"startup_migrate",
	"session_cleanup",
	"cli_user",
	"checkout_start",
	"stripe_webhook",
	"billing_reconcile",
	"billing_reinstatement",
	"billing_portal",
	"billing_invoice_read",
	"billing_worker",
	"other",
}

const otherOperation = len(operationNames) - 1

type requestContextKey struct{}
type operationContextKey struct{}

// Request handlers share this state through context copies, never across goroutines.
type requestState struct {
	started   time.Time
	requestID string
	operation int
	route     string
	status    int

	failure          error
	failureStage     string
	failureRoute     string
	failureOperation int
}

// Requests owns one failure event after the complete handler stack returns normally.
func Requests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestState{
			started:   time.Now(),
			requestID: requestID(r.Header),
			operation: otherOperation,
		}
		r = r.WithContext(context.WithValue(r.Context(), requestContextKey{}, state))
		w.Header().Set("X-Request-ID", state.requestID)
		next.ServeHTTP(&statusWriter{ResponseWriter: w, state: state}, r)

		if state.status == 0 {
			state.status = http.StatusOK
		}
		if state.failure == nil && (state.status < 500 || state.status >= 600) {
			return
		}

		route, operation, stage := state.matchedRoute(r), state.operation, "response"
		if state.failure != nil {
			route, operation, stage = state.failureRoute, state.failureOperation, state.failureStage
		}
		attrs := make([]slog.Attr, 0, 10)
		attrs = append(attrs,
			slog.String("request_id", state.requestID),
			slog.String("method", requestMethod(r.Method)),
			slog.String("route", route),
			slog.String("operation", operationNames[operation]),
			slog.String("stage", stage),
			slog.Int("status", state.status),
			slog.Float64("duration_ms", float64(time.Since(state.started))/float64(time.Millisecond)),
		)
		if state.failure == nil {
			attrs = append(attrs, slog.String("error_kind", "unknown"))
		} else {
			attrs = append(attrs, ErrorAttrs(state.failure)...)
		}
		log.LogAttrs(r.Context(), requestLevel(stage, state.failure), "request_failed", attrs...)
	})
}

// Operation captures the route after its mux matches, without copying the request.
func Operation(name string, next http.Handler) http.Handler {
	operation := operationIndex(name)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := stateFromContext(r.Context())
		state.operation, state.route = operation, r.Pattern
		next.ServeHTTP(w, r)
	})
}

// RecordError freezes the first failure and its current leaf metadata; it emits nothing.
func RecordError(r *http.Request, stage string, err error) {
	state := stateFromContext(r.Context())
	if err == nil || state.failure != nil {
		return
	}
	state.failure = err
	state.failureStage = stage
	state.failureRoute = state.matchedRoute(r)
	state.failureOperation = state.operation
}

// WithOperation labels immutable startup/background contexts for database summaries.
func WithOperation(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, operationContextKey{}, operationIndex(name))
}

func operationIndex(name string) int {
	if index := slices.Index(operationNames[:], name); index >= 0 {
		return index
	}
	return otherOperation
}

func operationFromContext(ctx context.Context) int {
	if state, ok := ctx.Value(requestContextKey{}).(*requestState); ok {
		return state.operation
	}
	if operation, ok := ctx.Value(operationContextKey{}).(int); ok {
		return operation
	}
	return otherOperation
}

func stateFromContext(ctx context.Context) *requestState {
	if state, ok := ctx.Value(requestContextKey{}).(*requestState); ok {
		return state
	}
	panic("logging: Requests middleware is required for Operation and RecordError")
}

func (state *requestState) matchedRoute(r *http.Request) string {
	if state.route != "" {
		return state.route
	}
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}

func requestID(header http.Header) string {
	values := header.Values("X-Railway-Request-Id")
	if len(values) == 1 && validRequestID(values[0]) {
		return values[0]
	}
	return rand.Text()
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func requestMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

type statusWriter struct {
	http.ResponseWriter
	state *requestState
}

func (w *statusWriter) WriteHeader(status int) {
	if w.state.status != 0 {
		return
	}
	if status < 100 || status >= 200 || status == http.StatusSwitchingProtocols {
		w.state.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.state.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
