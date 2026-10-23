//go:build integration

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"golang.org/x/time/rate"
	"gorm.io/gorm"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/customer"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/management"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

// later keeps the seeded link usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestSignIn(t *testing.T) {
	t.Parallel()
	db := newAuthDatabase(t)
	if err := createUser(t.Context(), db, "alice", "first password"); err != nil {
		t.Fatal(err)
	}
	app, manager := newApp(t, db)

	// The burst of two is spent on failures, so even correct credentials are refused before hashing.
	for _, username := range []string{"alice", "nobody"} {
		rec, cookie := signIn(t, app, nil, username, "wrong password")
		if body := rec.Body.String(); rec.Code != http.StatusUnauthorized || !strings.Contains(body, invalidCredentials) ||
			strings.Contains(body, "wrong password") || cookie != nil {
			t.Fatalf("sign in as %s: status %d, cookie %v, body %q", username, rec.Code, cookie, body)
		}
	}
	if rec, cookie := signIn(t, app, nil, "alice", "first password"); rec.Code != http.StatusTooManyRequests ||
		rec.Header().Get("Retry-After") != "12" || cookie != nil {
		t.Fatalf("rate-limited sign in: status %d, Retry-After %q, cookie %v", rec.Code, rec.Header().Get("Retry-After"), cookie)
	}
	manager.limiter = rate.NewLimiter(rate.Inf, 0)

	requireSignedOut(t, app, nil)
	htmx := newRequest(t, http.MethodPost, "/management/links/cancel")
	htmx.Header.Set("HX-Request", "true")
	if rec := serve(app, htmx, nil); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/management/" {
		t.Fatalf("signed-out htmx request: status %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if rec := serve(app, newRequest(t, http.MethodPost, "/management/links"), nil); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/management/" {
		t.Fatalf("signed-out form post: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}

	first := mustSignIn(t, app, " Alice ", "first password")
	requireSignedIn(t, app, first)

	rec, second := signIn(t, app, first, "alice", "first password")
	if rec.Code != http.StatusSeeOther || second == nil || second.Value == first.Value {
		t.Fatalf("sign in over an existing session: status %d, cookie %v; want a new token", rec.Code, second)
	}
	requireSignedOut(t, app, first)
	manager.Close()
	manager.Close()

	restarted, _ := newApp(t, db)
	requireSignedIn(t, restarted, second)

	expired := db.WithContext(t.Context()).Model(&Session{}).Where("expiry > ?", time.Now()).
		Update("expiry", time.Now().Add(-time.Minute))
	if expired.Error != nil || expired.RowsAffected != 1 {
		t.Fatalf("expire session: %v, rows %d", expired.Error, expired.RowsAffected)
	}
	requireSignedOut(t, restarted, second)
}

func TestCredentialChangesEndSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := newAuthDatabase(t)
	app, manager := newApp(t, db)
	manager.limiter = rate.NewLimiter(rate.Inf, 0)
	if err := createUser(ctx, db, "alice", "first password"); err != nil {
		t.Fatal(err)
	}
	cookie := mustSignIn(t, app, "alice", "first password")

	if err := createUser(ctx, db, "alice", "other password"); err == nil {
		t.Fatal("duplicate add succeeded")
	}
	requireSignedIn(t, app, cookie)
	requireRejected(t, app, "alice", "other password")

	if err := setPassword(ctx, db, "alice", "second password"); err != nil {
		t.Fatal(err)
	}
	requireSignedOut(t, app, cookie)
	requireRejected(t, app, "alice", "first password")
	cookie = mustSignIn(t, app, "alice", "second password")

	if err := RunCLI(ctx, db, []string{"revoke", "alice"}); err != nil {
		t.Fatal(err)
	}
	requireSignedOut(t, app, cookie)
	cookie = mustSignIn(t, app, "alice", "second password")

	if err := RunCLI(ctx, db, []string{"remove", "alice"}); err != nil {
		t.Fatal(err)
	}
	requireRejected(t, app, "alice", "second password")
	if err := RunCLI(ctx, db, []string{"revoke", "alice"}); err == nil {
		t.Fatal("revoked a removed account")
	}

	// The removed account's session row is still stored; recreating the name must not revive it.
	if err := createUser(ctx, db, "alice", "third password"); err != nil {
		t.Fatal(err)
	}
	requireSignedOut(t, app, cookie)
	mustSignIn(t, app, "alice", "third password")
}

func TestSignOutEndsOnlyThisBrowser(t *testing.T) {
	t.Parallel()
	db := newAuthDatabase(t)
	app, manager := newApp(t, db)
	manager.limiter = rate.NewLimiter(rate.Inf, 0)
	if err := createUser(t.Context(), db, "alice", "first password"); err != nil {
		t.Fatal(err)
	}
	laptop := mustSignIn(t, app, "alice", "first password")
	desktop := mustSignIn(t, app, "alice", "first password")

	rec := serve(app, newRequest(t, http.MethodPost, "/management/logout"), laptop)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/management/" {
		t.Fatalf("sign out: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	cleared := false
	for _, issued := range rec.Result().Cookies() {
		cleared = cleared || issued.Name == cookieName && issued.MaxAge < 0
	}
	if !cleared {
		t.Errorf("sign out did not tell the browser to drop its cookie: %v", rec.Result().Cookies())
	}
	// Replaying the old cookie proves the session ended on the server, not just in the browser.
	requireSignedOut(t, app, laptop)
	requireSignedIn(t, app, desktop)

	for name, cookie := range map[string]*http.Cookie{"repeated": laptop, "signed out": nil} {
		if rec := serve(app, newRequest(t, http.MethodPost, "/management/logout"), cookie); rec.Code != http.StatusSeeOther {
			t.Errorf("%s sign out: status %d, want 303", name, rec.Code)
		}
	}
	requireSignedIn(t, app, desktop)
}

func TestPublicCustomerIgnoresStaffSessions(t *testing.T) {
	t.Parallel()
	db := newAuthDatabase(t)
	app, _ := newApp(t, db)
	if err := db.WithContext(t.Context()).Exec("ALTER TABLE sessions RENAME TO unavailable_sessions").Error; err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: "forged-staff-session"}
	request := newRequest(t, http.MethodHead, "/customer/"+strings.Repeat("A", 43))
	rec := serve(app, request, cookie)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 ||
		rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" ||
		strings.Contains(rec.Header().Get("Vary"), "Cookie") || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("public customer with unavailable staff sessions: status %d, headers %v, body %q",
			rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := serve(app, newRequest(t, http.MethodGet, "/management/"), cookie); rec.Code != http.StatusInternalServerError {
		t.Fatalf("protected management with unavailable staff sessions: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestSessionCleanupDeletesOnlyExpired(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	log := logging.New(&out)
	db := testdb.Open(t, &Session{}).Session(&gorm.Session{Logger: logging.NewDBLogger(log)})
	manager := &Manager{db: db}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessions := []Session{
		{Token: "PRIVATE-EXPIRED-TOKEN", Data: []byte("PRIVATE-EXPIRED-DATA"), Expiry: now.Add(-time.Hour)},
		{Token: "PRIVATE-FUTURE-TOKEN", Data: []byte("PRIVATE-FUTURE-DATA"), Expiry: now.Add(time.Hour)},
	}
	if err := db.WithContext(t.Context()).Create(&sessions).Error; err != nil {
		t.Fatal(err)
	}

	manager.deleteExpiredSessions(t.Context(), log)
	var remaining []Session
	if err := db.WithContext(t.Context()).Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Token != sessions[1].Token ||
		!bytes.Equal(remaining[0].Data, sessions[1].Data) || !remaining[0].Expiry.Equal(sessions[1].Expiry) {
		t.Fatalf("cleanup changed future sessions or retained expired sessions: %#v", remaining)
	}
	if out.Len() != 0 {
		t.Fatalf("successful cleanup emitted logs: %s", out.Bytes())
	}

	if err := db.WithContext(t.Context()).Exec("ALTER TABLE sessions RENAME TO unavailable_cleanup_sessions").Error; err != nil {
		t.Fatal(err)
	}
	manager.deleteExpiredSessions(t.Context(), log)
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
		t.Fatalf("cleanup failure emitted %d lines, want one newline-terminated event: %s", len(lines), out.Bytes())
	}
	var event map[string]any
	if err := json.Unmarshal(lines[0], &event); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"message":    "session_cleanup_failed",
		"level":      "error",
		"error_kind": "database",
		"error_type": "*pgconn.PgError",
		"sqlstate":   "42P01",
	} {
		if event[key] != want {
			t.Errorf("%s = %v, want %v", key, event[key], want)
		}
	}
	for key := range event {
		switch key {
		case "time", "message", "level", "error_kind", "error_type", "sqlstate":
		default:
			t.Errorf("unexpected cleanup event field %q", key)
		}
	}
	for _, private := range []string{
		"PRIVATE-EXPIRED-TOKEN", "PRIVATE-EXPIRED-DATA", "PRIVATE-FUTURE-TOKEN", "PRIVATE-FUTURE-DATA",
		"sessions", "unavailable_cleanup_sessions", "expiry", "CURRENT_TIMESTAMP", "DELETE", "relation",
	} {
		if strings.Contains(out.String(), private) {
			t.Errorf("cleanup log exposed private data %q: %s", private, out.Bytes())
		}
	}

	out.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	manager.deleteExpiredSessions(ctx, log)
	if out.Len() != 0 {
		t.Fatalf("canceled cleanup emitted logs: %s", out.Bytes())
	}
}

func newAuthDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	models := append([]any{&User{}, &Session{}, &links.Link{}, &students.Student{}, &settings.Settings{}}, billing.Models()...)
	db := testdb.Open(t, models...)
	if _, err := links.New(db, nil).Create(t.Context(), "seed", later); err != nil {
		t.Fatal(err)
	}
	return db
}

func newTestBiller(t *testing.T, db *gorm.DB) *billing.Service {
	t.Helper()
	cfg := billing.Config{
		BaseURL:                      "http://127.0.0.1:3000",
		WebhookSecret:                "whsec_dummy",
		PaymentMethodConfigurationID: "pmc_dummy",
		PortalConfigurationID:        "bpc_dummy",
		Live:                         false,
	}
	biller, err := billing.New(db, stripe.NewClient("sk_test_dummy"), cfg, billing.Catalog{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return biller
}

// newApp wires auth like main: staff-only sessions, origin protection, and public customer routes.
func newApp(t *testing.T, db *gorm.DB) (http.Handler, *Manager) {
	t.Helper()
	log := logging.New(io.Discard)
	manager, err := New(db, false, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	mux := http.NewServeMux()
	manager.RegisterRoutes(mux)
	store := links.New(db, nil)
	biller := newTestBiller(t, db)
	managementMux := http.NewServeMux()
	if err := management.RegisterRoutes(managementMux, store, students.New(db, nil), settings.New(db), Staff, biller); err != nil {
		t.Fatal(err)
	}
	mux.Handle("/management/", manager.Require(managementMux))
	if err := customer.RegisterRoutes(mux, store, settings.New(db), biller); err != nil {
		t.Fatal(err)
	}
	return logging.Requests(log, http.NewCrossOriginProtection().Handler(mux)), manager
}

func newRequest(t *testing.T, method, path string) *http.Request {
	return httptest.NewRequestWithContext(t.Context(), method, path, nil)
}

func serve(app http.Handler, r *http.Request, cookie *http.Cookie) *httptest.ResponseRecorder {
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, r)
	return rec
}

// signIn returns the session cookie the response issued, if any.
func signIn(t *testing.T, app http.Handler, cookie *http.Cookie, username, password string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/management/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := serve(app, r, cookie)
	for _, issued := range rec.Result().Cookies() {
		if issued.Name == cookieName && issued.MaxAge > 0 {
			return rec, issued
		}
	}
	return rec, nil
}

func mustSignIn(t *testing.T, app http.Handler, username, password string) *http.Cookie {
	t.Helper()
	rec, cookie := signIn(t, app, nil, username, password)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/management/" || cookie == nil {
		t.Fatalf("sign in as %q: status %d, cookie %v", username, rec.Code, cookie)
	}
	return cookie
}

func requireRejected(t *testing.T, app http.Handler, username, password string) {
	t.Helper()
	if rec, cookie := signIn(t, app, nil, username, password); rec.Code != http.StatusUnauthorized || cookie != nil {
		t.Fatalf("sign in as %q: status %d, cookie %v; want rejection", username, rec.Code, cookie)
	}
}

// requireSignedIn also checks that an ordinary visit doesn't reissue the cookie,
// which would extend the fixed 14-day sign-in.
func requireSignedIn(t *testing.T, app http.Handler, cookie *http.Cookie) {
	t.Helper()
	rec := serve(app, newRequest(t, http.MethodGet, "/management/"), cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<li class="management-link">`) {
		t.Fatalf("signed-in visit: status %d, body %q", rec.Code, rec.Body.String())
	}
	if issued := rec.Result().Cookies(); len(issued) != 0 {
		t.Fatalf("signed-in visit reissued cookies %v", issued)
	}
}

func requireSignedOut(t *testing.T, app http.Handler, cookie *http.Cookie) {
	t.Helper()
	rec := serve(app, newRequest(t, http.MethodGet, "/management/"), cookie)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `action="/management/login"`) ||
		strings.Contains(body, `href="/customer/`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("signed-out visit: status %d, Cache-Control %q, body %q", rec.Code, rec.Header().Get("Cache-Control"), body)
	}
}
