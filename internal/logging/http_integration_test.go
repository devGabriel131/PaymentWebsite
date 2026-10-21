//go:build integration

package logging_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"

	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/customer"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/management"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

const (
	httpRequestID = "logging-http-contract-001"
	httpUsername  = "private-http-staff"
	httpPassword  = "PRIVATE_HTTP_PASSWORD"
	httpQuery     = "PRIVATE_HTTP_QUERY"
	httpHeader    = "PRIVATE_HTTP_HEADER"
)

func TestCustomerRolledBackRequestsLogOnce(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry := fixture.createLink(t)
	before := fixture.link(t, entry.Token)
	tx := fixture.db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	handler := fixture.routes(t, links.New(tx))
	for _, tc := range []struct {
		name, suffix, operation string
	}{
		{"final", "", "final_submit"},
		{"draft", "/draft", "draft_save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const attempted = "PRIVATE_ROLLED_BACK_NAME"
			rec := serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token+tc.suffix, url.Values{"name": {attempted}}, nil)
			fixture.requireFailure(t, rec, httpFailure{
				method: http.MethodPost, route: "POST /customer/{token}" + tc.suffix,
				operation: tc.operation, stage: "save", kind: "transaction_done",
			}, attempted, entry.Token, entry.ID, httpUsername)
			if !strings.Contains(rec.Body.String(), "Could not save your name. Please try again.") {
				t.Fatal("rolled-back write lost the existing customer error response")
			}
			fixture.out.Reset()
		})
	}
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, before) {
		t.Fatal("rolled-back customer requests changed the source link")
	}
	fixture.requireStudents(t, nil)
	fixture.requireQuiet(t)
}

func TestCustomerCompletionFailureLogsOnceAndRetryIsSilent(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry := fixture.createLink(t)
	const draft = "PRIVATE_ORIGINAL_DRAFT"
	if saved, err := fixture.store.SaveDraft(t.Context(), entry.Token, draft); err != nil || saved != links.Saved {
		t.Fatalf("save draft: result = %v, error = %v", saved, err)
	}
	before := fixture.link(t, entry.Token)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT customer_submit_rejected CHECK (name <> 'Reject completion')").Error; err != nil {
		t.Fatal(err)
	}
	handler := fixture.routes(t, fixture.store)
	const attempted = "  Reject completion  "
	form := url.Values{"name": {attempted}}
	rec := serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token, form, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}",
		operation: "final_submit", stage: "save", kind: "database", sqlstate: "23514",
	}, strings.TrimSpace(attempted), draft, entry.Token, entry.ID, httpUsername, "customer_submit_rejected")
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, before) {
		t.Fatal("failed completion changed draft, completion, or expiry")
	}
	fixture.requireStudents(t, nil)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students DROP CONSTRAINT customer_submit_rejected").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	rec = serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token, form, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", rec.Code)
	}
	completed := fixture.link(t, entry.Token)
	var enrolled []students.Student
	if err := fixture.db.WithContext(t.Context()).Find(&enrolled).Error; err != nil {
		t.Fatal(err)
	}
	if completed.Name != strings.TrimSpace(attempted) || completed.SubmittedAt == nil || completed.OpenedAt != nil ||
		len(enrolled) != 1 || enrolled[0].Name != completed.Name || !bytes.Equal(enrolled[0].SourceLinkHash, completed.TokenHash) ||
		!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatal("retry did not atomically commit matching completion and student")
	}
	fixture.requireQuiet(t)
}

func TestStudentSaveAndPreliminaryLoadKeepSafeLeafLogging(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry := fixture.createLink(t)
	const originalName = "PRIVATE_ORIGINAL_STUDENT"
	if saved, err := fixture.store.Submit(t.Context(), entry.Token, originalName); err != nil || saved != links.Saved {
		t.Fatalf("enroll student: result = %v, error = %v", saved, err)
	}
	source := fixture.link(t, entry.Token)
	var original students.Student
	if err := fixture.db.WithContext(t.Context()).Where("source_link_hash = ?", source.TokenHash).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	user := fixture.createUser(t)
	handler := fixture.routes(t, fixture.store)
	login := serveHTTPLogging(t, handler, http.MethodPost, "/management/login", url.Values{"username": {user.Username}, "password": {httpPassword}}, nil)
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/management/" {
		t.Fatalf("sign in status = %d, want 303 to management", login.Code)
	}
	var cookie *http.Cookie
	for _, issued := range login.Result().Cookies() {
		if issued.Name == "management_session" && issued.MaxAge > 0 {
			cookie = issued
		}
	}
	if cookie == nil {
		t.Fatal("successful sign-in did not issue a staff session")
	}
	fixture.requireQuiet(t)
	cookieDigest := sha256.Sum256([]byte(cookie.Value))
	private := []string{httpUsername, httpPassword, user.PasswordHash, user.AuthVersion, cookie.Value, hex.EncodeToString(cookieDigest[:]),
		entry.Token, entry.ID, originalName, "Rejected by database", "reject_test_student_name", "unavailable_students"}
	path := fmt.Sprintf("/management/students/%d/fields/name", original.ID)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT reject_test_student_name CHECK (name <> 'Rejected by database')").Error; err != nil {
		t.Fatal(err)
	}
	rec := serveHTTPLogging(t, handler, http.MethodPost, path, url.Values{"value": {"Rejected by database"}}, cookie)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/students/{id}/fields/{field}",
		operation: "student_update", stage: "save", kind: "database", sqlstate: "23514",
	}, private...)
	if rec.Body.String() != "Could not save this field. Please try again.\n" {
		t.Fatal("student save failure changed the safe HTTP response")
	}
	fixture.requireStudents(t, []students.Student{original})
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, source) {
		t.Fatal("failed student save changed the source submission")
	}
	fixture.out.Reset()
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students RENAME TO unavailable_students").Error; err != nil {
		t.Fatal(err)
	}
	rec = serveHTTPLogging(t, handler, http.MethodPost, path, url.Values{"value": {"Rejected by database"}}, cookie)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/students/{id}/fields/{field}",
		operation: "student_update", stage: "load", kind: "database", sqlstate: "42P01",
	}, private...)
	if rec.Body.String() != "Could not load student. Please try again.\n" {
		t.Fatal("preliminary load failure changed the safe HTTP response")
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE unavailable_students RENAME TO students").Error; err != nil {
		t.Fatal(err)
	}
	fixture.requireStudents(t, []students.Student{original})
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, source) {
		t.Fatal("failed preliminary student load changed the source submission")
	}
}

func TestSessionLoadFailureKeepsPublicHeadQuiet(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	handler := fixture.routes(t, fixture.store)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE sessions RENAME TO unavailable_sessions").Error; err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "management_session", Value: "PRIVATE_FORGED_STAFF_SESSION"}
	token := strings.Repeat("A", 43)
	rec := serveHTTPLogging(t, handler, http.MethodHead, "/customer/"+token, nil, cookie)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Referrer-Policy") != "no-referrer" || strings.Contains(rec.Header().Get("Vary"), "Cookie") || len(rec.Result().Cookies()) != 0 {
		t.Fatal("public HEAD consulted staff sessions or changed its existing quiet 404")
	}
	fixture.requireQuiet(t)
	rec = serveHTTPLogging(t, handler, http.MethodGet, "/management/", nil, cookie)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodGet, route: "/management/",
		operation: "session_check", stage: "session", kind: "database", sqlstate: "42P01",
	}, cookie.Value, token, "unavailable_sessions")
	if rec.Body.String() != "Internal Server Error\n" {
		t.Fatal("session load failure exposed database detail or changed the SCS 500")
	}
}

func TestSignInCommitFailureLogsActualStatusWithoutCredentials(t *testing.T) {
	// A fresh, empty session store and no incoming cookie make the failure occur
	// in redirect commit, not session load or RenewToken's old-token deletion.
	fixture := newHTTPLoggingFixture(t)
	user := fixture.createUser(t)
	handler := fixture.routes(t, fixture.store)
	var count int64
	if err := fixture.db.WithContext(t.Context()).Model(&auth.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("fresh sessions: count = %d, error = %v", count, err)
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE sessions ADD CONSTRAINT reject_session_commit CHECK (false)").Error; err != nil {
		t.Fatal(err)
	}
	rec := serveHTTPLogging(t, handler, http.MethodPost, "/management/login", url.Values{"username": {user.Username}, "password": {httpPassword}}, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/login",
		operation: "sign_in", stage: "session", kind: "database", sqlstate: "23514",
	}, httpUsername, httpPassword, user.PasswordHash, user.AuthVersion, "reject_session_commit")
	if !strings.Contains(rec.Body.String(), "Internal Server Error") {
		t.Fatal("session commit failure lost the SCS error response")
	}
	if err := fixture.db.WithContext(t.Context()).Model(&auth.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed session commit persisted a session: count = %d, error = %v", count, err)
	}
}

type httpLoggingFixture struct {
	db    *gorm.DB
	store *links.Store
	log   *slog.Logger
	out   bytes.Buffer
}

func newHTTPLoggingFixture(t *testing.T) *httpLoggingFixture {
	t.Helper()
	fixture := &httpLoggingFixture{}
	fixture.log = logging.New(&fixture.out)
	db := testdb.Open(t, &auth.User{}, &auth.Session{}, &links.Link{}, &students.Student{}, &settings.Settings{})
	fixture.db = db.Session(&gorm.Session{Logger: logging.NewDBLogger(fixture.log)})
	fixture.store = links.New(fixture.db)
	return fixture
}

func (fixture *httpLoggingFixture) routes(t *testing.T, store *links.Store) http.Handler {
	t.Helper()
	manager, err := auth.New(fixture.db, false, fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	child := http.NewServeMux()
	if err := management.RegisterRoutes(child, store, students.New(fixture.db), settings.New(fixture.db), auth.Staff); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	manager.RegisterRoutes(root)
	root.Handle("/management/", manager.Require(child))
	if err := customer.RegisterRoutes(root, store); err != nil {
		t.Fatal(err)
	}
	return logging.Requests(fixture.log, http.NewCrossOriginProtection().Handler(root))
}

func (fixture *httpLoggingFixture) createLink(t *testing.T) links.Entry {
	t.Helper()
	entry, err := fixture.store.Create(t.Context(), httpUsername, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func (fixture *httpLoggingFixture) link(t *testing.T, token string) links.Link {
	t.Helper()
	link, err := fixture.store.Lookup(t.Context(), token)
	if err != nil || link == nil {
		t.Fatalf("read source link: present = %t, error = %v", link != nil, err)
	}
	return *link
}

func (fixture *httpLoggingFixture) createUser(t *testing.T) auth.User {
	t.Helper()
	hash, err := argon2id.CreateHash(httpPassword, &argon2id.Params{Memory: 16, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	user := auth.User{Username: httpUsername, PasswordHash: hash, AuthVersion: "PRIVATE_HTTP_AUTH_VERSION"}
	if err := fixture.db.WithContext(t.Context()).Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func (fixture *httpLoggingFixture) requireStudents(t *testing.T, want []students.Student) {
	t.Helper()
	got, err := students.New(fixture.db).List(t.Context())
	if err != nil || len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("student rows changed: count = %d, want %d, error = %v", len(got), len(want), err)
	}
}

func serveHTTPLogging(t *testing.T, handler http.Handler, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path+"?private="+httpQuery, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Railway-Request-Id", httpRequestID)
	r.Header.Set("X-Private-Header", httpHeader)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

type httpFailure struct {
	method, route, operation, stage, kind, sqlstate string
}

func (fixture *httpLoggingFixture) requireFailure(t *testing.T, rec *httptest.ResponseRecorder, want httpFailure, private ...string) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Request-ID") != httpRequestID {
		t.Fatalf("failure response: status = %d, request ID = %q", rec.Code, rec.Header().Get("X-Request-ID"))
	}
	data := fixture.out.Bytes()
	if !bytes.HasSuffix(data, []byte{'\n'}) || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("request must emit exactly one newline-terminated event")
	}
	for _, value := range append([]string{httpQuery, httpHeader, "management_session", "SELECT ", "UPDATE ", "INSERT INTO ", "DELETE FROM ", "ALTER TABLE "}, private...) {
		if value != "" && (bytes.Contains(data, []byte(value)) || bytes.Contains(data, []byte(url.QueryEscape(value)))) {
			t.Fatalf("request event contains forbidden private value %q", value)
		}
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("request event is not JSON: %v", err)
	}
	for name, expected := range map[string]any{
		"message": "request_failed", "level": "error", "request_id": httpRequestID,
		"method": want.method, "route": want.route, "operation": want.operation,
		"stage": want.stage, "status": float64(http.StatusInternalServerError), "error_kind": want.kind,
	} {
		if event[name] != expected {
			t.Errorf("event %s = %v, want %v", name, event[name], expected)
		}
	}
	if elapsed, ok := event["duration_ms"].(float64); !ok || elapsed < 0 {
		t.Error("request event lacks a nonnegative numeric elapsed duration")
	}
	wantType := "*pgconn.PgError"
	if want.kind == "transaction_done" {
		wantType = "*errors.errorString"
	}
	if event["error_type"] != wantType {
		t.Errorf("error_type = %v, want concrete type %s", event["error_type"], wantType)
	}
	if want.sqlstate != "" {
		if event["sqlstate"] != want.sqlstate {
			t.Errorf("SQLSTATE = %v, want %s", event["sqlstate"], want.sqlstate)
		}
	} else if _, exists := event["sqlstate"]; exists {
		t.Error("non-PostgreSQL failure acquired a SQLSTATE")
	}
	for name := range event {
		switch name {
		case "time", "message", "level", "request_id", "method", "route", "operation", "stage", "status", "duration_ms", "error_kind", "error_type", "sqlstate":
		default:
			t.Errorf("request event has unexpected field %q", name)
		}
	}
}

func (fixture *httpLoggingFixture) requireQuiet(t *testing.T) {
	t.Helper()
	if fixture.out.Len() != 0 {
		t.Fatal("successful or expected request emitted an event")
	}
}
