package logging

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type httpTestContextKey struct{}

func TestRequestsNestedMuxFreezesFirstFailure(t *testing.T) {
	var out bytes.Buffer
	var leafRequest *http.Request
	leaf := http.NewServeMux()
	leaf.Handle("POST /management/links/{id}/cancel", Operation("link_cancel", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != leafRequest {
			t.Fatal("Operation copied the matched request")
		}
		if r.Context().Value(httpTestContextKey{}) != "leaf" {
			t.Fatal("leaf context copy lost")
		}
	})))
	root := http.NewServeMux()
	root.Handle("/management/", Operation("session_check", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stale := r.WithContext(context.WithValue(r.Context(), httpTestContextKey{}, "session"))
		leafRequest = stale.WithContext(context.WithValue(stale.Context(), httpTestContextKey{}, "leaf"))
		leaf.ServeHTTP(w, leafRequest)
		// Session commit reports through its older request after the leaf returns.
		RecordError(stale, "session", fmt.Errorf("PRIVATE-FIRST-ERROR: %w", sql.ErrTxDone))
		w.WriteHeader(http.StatusInternalServerError)
		Operation("sign_out", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			RecordError(r, "render", fmt.Errorf("PRIVATE-RENDER-ERROR: %w", context.Canceled))
		})).ServeHTTP(w, stale)
		if out.Len() != 0 {
			t.Fatal("failure emitted before handler/session completion")
		}
		w.WriteHeader(http.StatusSeeOther)
	})))

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://private-host.example/management/links/PRIVATE-TOKEN/cancel?private_query=PRIVATE-QUERY",
		strings.NewReader("PRIVATE-BODY"))
	r.Header.Set("X-Railway-Request-Id", "railway-valid_01.X")
	r.Header.Set("Authorization", "Bearer PRIVATE-AUTHORIZATION")
	r.AddCookie(&http.Cookie{Name: "staff", Value: "PRIVATE-COOKIE"})
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Railway-Request-Id", "upstream-response-id")
	Requests(New(&out), root).ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want committed 500 rather than intended 303", rec.Code)
	}
	if rec.Header().Get("X-Request-ID") != "railway-valid_01.X" || rec.Header().Get("X-Railway-Request-Id") != "upstream-response-id" {
		t.Fatalf("request ID headers changed incorrectly: %v", rec.Header())
	}
	event := onlyRequestEvent(t, out.Bytes())
	for key, want := range map[string]any{
		"level": "error", "request_id": "railway-valid_01.X", "method": "POST",
		"route": "POST /management/links/{id}/cancel", "operation": "link_cancel",
		"stage": "session", "status": float64(500), "error_kind": "transaction_done",
	} {
		if event[key] != want {
			t.Errorf("%s = %v, want %v", key, event[key], want)
		}
	}
	if duration, ok := event["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("duration_ms = %v, want nonnegative numeric milliseconds", event["duration_ms"])
	}
	for _, forbidden := range []string{
		"PRIVATE-TOKEN", "private-host.example", "private_query", "PRIVATE-QUERY", "PRIVATE-BODY",
		"PRIVATE-AUTHORIZATION", "PRIVATE-COOKIE", "PRIVATE-FIRST-ERROR", "PRIVATE-RENDER-ERROR",
	} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("event exposed %q", forbidden)
		}
	}
}

func TestRequestsHEADRetainsRegisteredGETPattern(t *testing.T) {
	var out bytes.Buffer
	mux := http.NewServeMux()
	mux.Handle("GET /customer/{token}", Operation("link_read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Fatalf("handler method = %q, want HEAD", r.Method)
		}
		RecordError(r, "lookup", errors.New("PRIVATE-HEAD-ERROR"))
		w.WriteHeader(http.StatusInternalServerError)
	})))
	rec := httptest.NewRecorder()
	Requests(New(&out), mux).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/customer/PRIVATE-HEAD-TOKEN", nil))
	event := onlyRequestEvent(t, out.Bytes())
	if event["method"] != "HEAD" || event["route"] != "GET /customer/{token}" || event["operation"] != "link_read" {
		t.Fatalf("HEAD metadata = %v", event)
	}
	if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 || strings.Contains(out.String(), "PRIVATE-HEAD") {
		t.Fatalf("HEAD response/privacy changed: status %d, body %q, event %s", rec.Code, rec.Body.String(), out.String())
	}
}

func TestRequestsRequestIDs(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		reuse  bool
	}{
		{name: "allowed ASCII", values: []string{"AZaz09._-"}, reuse: true},
		{name: "one character", values: []string{"x"}, reuse: true},
		{name: "maximum length", values: []string{strings.Repeat("a", 128)}, reuse: true},
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "too long", values: []string{strings.Repeat("a", 129)}},
		{name: "multiple values", values: []string{"first", "second"}},
		{name: "duplicate identical values", values: []string{"same", "same"}},
		{name: "comma joined", values: []string{"first,second"}},
		{name: "leading space", values: []string{" valid"}},
		{name: "trailing space", values: []string{"valid "}},
		{name: "tab", values: []string{"a\tb"}},
		{name: "newline", values: []string{"a\nb"}},
		{name: "carriage return", values: []string{"a\rb"}},
		{name: "NUL", values: []string{"a\x00b"}},
		{name: "non ASCII", values: []string{"caf\u00e9"}},
		{name: "slash", values: []string{"a/b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.Header[http.CanonicalHeaderKey("X-Railway-Request-Id")] = tc.values
			r.Header.Set("X-Request-ID", "PRIVATE-CLIENT-FALLBACK")
			rec := httptest.NewRecorder()
			rec.Header().Set("X-Railway-Request-Id", "preserve-upstream")
			Requests(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})).ServeHTTP(rec, r)
			id := rec.Header().Get("X-Request-ID")
			if !validRequestID(id) {
				t.Fatalf("response request ID = %q, want valid ASCII ID", id)
			}
			if tc.reuse {
				if id != tc.values[0] {
					t.Errorf("request ID = %q, want reused %q", id, tc.values[0])
				}
			} else {
				if len(id) < 26 || id == "PRIVATE-CLIENT-FALLBACK" || slices.Contains(tc.values, id) {
					t.Errorf("request ID = %q, want generated ID, not untrusted fallback", id)
				}
			}
			if rec.Header().Get("X-Railway-Request-Id") != "preserve-upstream" {
				t.Error("Railway response header overwritten")
			}
			if event := onlyRequestEvent(t, out.Bytes()); event["request_id"] != id {
				t.Errorf("event ID = %v, want response ID %q", event["request_id"], id)
			}
		})
	}
}

func TestRequestsActualStatus(t *testing.T) {
	cases := []struct {
		name    string
		write   func(http.ResponseWriter)
		status  int
		headers []int
		body    string
	}{
		{name: "no writes", write: func(http.ResponseWriter) {}, status: 200},
		{name: "body implies 200", write: func(w http.ResponseWriter) { _, _ = w.Write([]byte("body")) }, status: 200, headers: []int{200}, body: "body"},
		{name: "first final wins", write: func(w http.ResponseWriter) {
			w.WriteHeader(201)
			w.WriteHeader(500)
			w.WriteHeader(200)
		}, status: 201, headers: []int{201}},
		{name: "informational before failure", write: func(w http.ResponseWriter) {
			w.WriteHeader(103)
			w.WriteHeader(102)
			w.WriteHeader(500)
			w.WriteHeader(303)
		}, status: 500, headers: []int{103, 102, 500}},
		{name: "continue then body", write: func(w http.ResponseWriter) {
			w.WriteHeader(100)
			_, _ = w.Write([]byte("after continue"))
		}, status: 200, headers: []int{100, 200}, body: "after continue"},
		{name: "switching protocols is final", write: func(w http.ResponseWriter) {
			w.WriteHeader(101)
			w.WriteHeader(500)
		}, status: 101, headers: []int{101}},
		{name: "other informational then final", write: func(w http.ResponseWriter) {
			w.WriteHeader(199)
			w.WriteHeader(204)
		}, status: 204, headers: []int{199, 204}},
		{name: "nothing forwarded after final", write: func(w http.ResponseWriter) {
			w.WriteHeader(500)
			w.WriteHeader(103)
			w.WriteHeader(303)
		}, status: 500, headers: []int{500}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			rec := &headerRecorder{ResponseRecorder: httptest.NewRecorder()}
			Requests(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.write(w)
				RecordError(r, "render", errors.New("PRIVATE-STATUS-ERROR"))
			})).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
			if rec.Code != tc.status || !slices.Equal(rec.headers, tc.headers) || rec.Body.String() != tc.body {
				t.Errorf("response = status %d, headers %v, body %q; want %d, %v, %q", rec.Code, rec.headers, rec.Body.String(), tc.status, tc.headers, tc.body)
			}
			if event := onlyRequestEvent(t, out.Bytes()); event["status"] != float64(tc.status) {
				t.Errorf("logged status = %v, want %d", event["status"], tc.status)
			}
		})
	}
}

// ResponseRecorder commits informational statuses; retain them separately like net/http.
type headerRecorder struct {
	*httptest.ResponseRecorder
	headers []int
}

func (rec *headerRecorder) WriteHeader(status int) {
	rec.headers = append(rec.headers, status)
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		return
	}
	rec.ResponseRecorder.WriteHeader(status)
}

func TestRequestsRenderFailureAfter200(t *testing.T) {
	var out bytes.Buffer
	rec := httptest.NewRecorder()
	Requests(New(&out), Operation("link_read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("already rendered"))
		RecordError(r, "render", fmt.Errorf("PRIVATE-RENDER-WRITE: %w", syscall.EPIPE))
	}))).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-RENDER-TOKEN", nil))
	event := onlyRequestEvent(t, out.Bytes())
	if rec.Code != 200 || rec.Body.String() != "already rendered" || event["status"] != float64(200) || event["level"] != "warn" || event["error_kind"] != "connection_failure" {
		t.Fatalf("render failure changed response/classification: status %d, body %q, event %v", rec.Code, rec.Body.String(), event)
	}
	if strings.Contains(out.String(), "PRIVATE-RENDER") {
		t.Fatalf("render failure exposed private data: %s", out.String())
	}
}

func TestRequestsSuccessfulAndExpectedResponsesAreSilent(t *testing.T) {
	for _, status := range []int{0, 200, 204, 303, 400, 401, 403, 404, 409, 422, 429} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var out bytes.Buffer
			rec := httptest.NewRecorder()
			Requests(New(&out), Operation("link_read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				RecordError(r, "lookup", nil)
				if status != 0 {
					w.WriteHeader(status)
				}
			}))).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-SUCCESS-TOKEN", nil))
			want := status
			if want == 0 {
				want = 200
			}
			if rec.Code != want || len(decodeEvents(t, out.Bytes())) != 0 {
				t.Fatalf("status %d, events %s; want status %d and silence", rec.Code, out.String(), want)
			}
		})
	}
}

func TestRequestsBare5xxAndRouteFallback(t *testing.T) {
	var out bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /customer/{token}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	Requests(New(&out), mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-BARE-TOKEN?value=PRIVATE-BARE-QUERY", nil))
	event := onlyRequestEvent(t, out.Bytes())
	if event["level"] != "error" || event["stage"] != "response" || event["operation"] != "other" || event["route"] != "GET /customer/{token}" || event["status"] != float64(502) || event["error_kind"] != "unknown" {
		t.Fatalf("bare 5xx metadata = %v", event)
	}
	if _, exists := event["error_type"]; exists {
		t.Error("bare response invented an error type")
	}
	if strings.Contains(out.String(), "PRIVATE-BARE") {
		t.Errorf("bare 5xx exposed URL values: %s", out.String())
	}
}

func TestRequestsMethodNormalizationAndUnmatchedRoute(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch,
		"PRIVATE-METHOD", "get"} {
		t.Run(method, func(t *testing.T) {
			var out bytes.Buffer
			Requests(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, "/PRIVATE-UNMATCHED?value=PRIVATE-QUERY", nil))
			want := method
			if method == "PRIVATE-METHOD" || method == "get" {
				want = "OTHER"
			}
			event := onlyRequestEvent(t, out.Bytes())
			if event["method"] != want || event["route"] != "unmatched" {
				t.Fatalf("method/route = %v/%v, want %s/unmatched", event["method"], event["route"], want)
			}
			if strings.Contains(out.String(), "PRIVATE-") {
				t.Errorf("unmatched request exposed method/URL: %s", out.String())
			}
		})
	}
}

func TestRequestsRetainsOriginProtectionAndUnwrap(t *testing.T) {
	for _, site := range []string{"same-origin", "cross-site"} {
		t.Run(site, func(t *testing.T) {
			var out bytes.Buffer
			rec := httptest.NewRecorder()
			called := false
			mux := http.NewServeMux()
			mux.Handle("POST /management/{$}", Operation("settings_update", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
				if !ok || unwrapper.Unwrap() != rec {
					t.Fatal("response writer does not unwrap to original writer")
				}
				w.Header().Set("X-Application-Header", "preserved")
				w.WriteHeader(http.StatusOK)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Fatalf("ResponseController could not reach original writer: %v", err)
				}
				_, _ = w.Write([]byte("unchanged body"))
			})))
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://payment.example/management/", nil)
			r.Header.Set("Sec-Fetch-Site", site)
			r.Header.Set("Origin", "https://outside.example")
			Requests(New(&out), http.NewCrossOriginProtection().Handler(mux)).ServeHTTP(rec, r)
			if site == "cross-site" {
				if called || rec.Code != http.StatusForbidden {
					t.Fatalf("cross-origin protection lost: called %v, status %d", called, rec.Code)
				}
			} else if !called || rec.Code != http.StatusOK || !rec.Flushed || rec.Header().Get("X-Application-Header") != "preserved" || rec.Body.String() != "unchanged body" {
				t.Fatalf("allowed response changed: called %v, status %d, headers %v, body %q, flushed %v", called, rec.Code, rec.Header(), rec.Body.String(), rec.Flushed)
			}
			if !validRequestID(rec.Header().Get("X-Request-ID")) || len(decodeEvents(t, out.Bytes())) != 0 {
				t.Fatalf("root correlation/silence lost: headers %v, events %s", rec.Header(), out.String())
			}
		})
	}
}

func TestRequestsDoesNotInferPanicOutcome(t *testing.T) {
	var out bytes.Buffer
	marker := &struct{}{}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		Requests(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			RecordError(r, "save", errors.New("PRIVATE-PRE-PANIC-ERROR"))
			panic(marker)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil))
	}()
	if recovered != marker || len(decodeEvents(t, out.Bytes())) != 0 {
		t.Fatalf("panic changed or request outcome inferred: panic %v, events %s", recovered, out.String())
	}
}

func TestRequestStateRequiresRequests(t *testing.T) {
	cases := []struct {
		name string
		call func(*http.Request)
	}{
		{name: "Operation", call: func(r *http.Request) {
			Operation("link_read", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), r)
		}},
		{name: "RecordError", call: func(r *http.Request) { RecordError(r, "lookup", errors.New("failure")) }},
		{name: "nil RecordError", call: func(r *http.Request) { RecordError(r, "lookup", nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				value := recover()
				message, ok := value.(string)
				if !ok || !strings.Contains(message, "Requests") {
					t.Errorf("panic = %v, want clear Requests invariant", value)
				}
			}()
			tc.call(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		})
	}
}

func TestWithOperationContextOwnership(t *testing.T) {
	root := t.Context()
	startup := WithOperation(root, "startup_migrate")
	cleanup := WithOperation(startup, "session_cleanup")
	unknown := WithOperation(cleanup, "PRIVATE-UNKNOWN-OPERATION")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "unlabeled", ctx: root, want: "other"},
		{name: "startup", ctx: startup, want: "startup_migrate"},
		{name: "cleanup", ctx: cleanup, want: "session_cleanup"},
		{name: "unknown", ctx: unknown, want: "other"},
		{name: "context copy", ctx: context.WithValue(startup, httpTestContextKey{}, true), want: "startup_migrate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationNames[operationFromContext(tc.ctx)]; got != tc.want {
				t.Errorf("operation = %q, want %q", got, tc.want)
			}
		})
	}
	Requests(New(io.Discard), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := operationFromContext(r.Context()); got != otherOperation {
			t.Fatalf("fresh request inherited background operation %q", operationNames[got])
		}
		Operation("student_get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := operationNames[operationFromContext(WithOperation(r.Context(), "cli_user"))]; got != "student_get" {
				t.Errorf("background context replaced request operation: %q", got)
			}
		})).ServeHTTP(w, r)
		Operation("PRIVATE-UNKNOWN-ROUTE", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := operationFromContext(r.Context()); got != otherOperation {
				t.Errorf("unknown route operation = %q, want other", operationNames[got])
			}
		})).ServeHTTP(w, r)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(startup, http.MethodGet, "/", nil))
}

func onlyRequestEvent(t *testing.T, data []byte) map[string]any {
	t.Helper()
	events := decodeEvents(t, data)
	if len(events) != 1 {
		t.Fatalf("got %d events, want one: %s", len(events), data)
	}
	if events[0]["message"] != "request_failed" {
		t.Fatalf("event message = %v, want request_failed", events[0]["message"])
	}
	return events[0]
}
