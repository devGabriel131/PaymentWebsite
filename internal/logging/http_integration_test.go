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
	handler := fixture.routes(t, links.New(tx, nil))
	for _, tc := range []struct {
		name, suffix, route, operation string
	}{
		{"final", "", "POST /customer/{token}", "final_submit"},
		{"name draft", "/draft/name", "POST /customer/{token}/draft/{field}", "draft_save"},
		{"email draft", "/draft/email", "POST /customer/{token}/draft/{field}", "draft_save"},
		{"phone draft", "/draft/phone", "POST /customer/{token}/draft/{field}", "draft_save"},
		{"municipality draft", "/draft/municipality", "POST /customer/{token}/draft/{field}", "draft_save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := httpContactForm("Private rolled back name")
			rec := serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token+tc.suffix, form, nil)
			private := append(httpContactValues(form), entry.Token, entry.ID, httpUsername)
			fixture.requireFailure(t, rec, httpFailure{
				method: http.MethodPost, route: tc.route,
				operation: tc.operation, stage: "save", kind: "transaction_done",
			}, private...)
			if !strings.Contains(rec.Body.String(), `role="alert"`) || strings.Contains(rec.Body.String(), "data-validation=") {
				t.Fatal("rolled-back write lost its save-error response")
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
	draft := httpContactDetails("Private original draft")
	for _, field := range students.Fields() {
		value := field.Value(students.Student{Details: draft})
		if saved, err := fixture.store.SaveDraft(t.Context(), entry.Token, field.Key, value); err != nil || saved != links.Saved {
			t.Fatalf("save %s draft: result = %v, error = %v", field.Key, saved, err)
		}
	}
	before := fixture.link(t, entry.Token)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT customer_submit_rejected CHECK (name <> 'Reject completion')").Error; err != nil {
		t.Fatal(err)
	}
	handler := fixture.routes(t, fixture.store)
	const attempted = "  Reject completion  "
	form := httpContactForm(attempted)
	private := append(httpContactValues(form), draft.Name, draft.Email, draft.Phone, draft.Municipality,
		entry.Token, entry.ID, httpUsername, "customer_submit_rejected")
	rec := serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token, form, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}",
		operation: "final_submit", stage: "save", kind: "database", sqlstate: "23514",
	}, private...)
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, before) {
		t.Fatal("failed completion changed draft, completion, or expiry")
	}
	fixture.requireStudents(t, nil)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students DROP CONSTRAINT customer_submit_rejected").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	rec = serveHTTPLogging(t, handler, http.MethodPost, "/customer/"+entry.Token, form, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/customer/"+entry.Token+"/agreement" {
		t.Fatalf("retry status = %d, want 303 to agreement", rec.Code)
	}
	completed := fixture.link(t, entry.Token)
	var enrolled []students.Student
	if err := fixture.db.WithContext(t.Context()).Find(&enrolled).Error; err != nil {
		t.Fatal(err)
	}
	want := students.Details{Name: "Reject completion", Email: "Private+logging@example.com", Phone: "+12025550123", Municipality: "Bayamón"}
	if completed.Details != want || completed.SubmittedAt == nil || completed.OpenedAt != nil ||
		len(enrolled) != 1 || enrolled[0].Details != completed.Details || !bytes.Equal(enrolled[0].SourceLinkHash, completed.TokenHash) ||
		!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatal("retry did not atomically commit matching completion and student")
	}
	for _, value := range httpContactValues(form) {
		if strings.Contains(rec.Body.String(), value) {
			t.Fatalf("completion exposed contact value %q", value)
		}
	}
	fixture.requireQuiet(t)
}

func TestStudentSaveAndPreliminaryLoadKeepSafeLeafLogging(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry := fixture.createLink(t)
	originalDetails := httpContactDetails("Private original student")
	if saved, err := fixture.store.Submit(t.Context(), entry.Token, originalDetails); err != nil || saved != links.Saved {
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
		entry.Token, entry.ID, originalDetails.Name, originalDetails.Email, originalDetails.Phone, originalDetails.Municipality,
		"Rejected by database", "reject_test_student_name", "unavailable_students"}
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

func TestAgreementAndChoiceFailuresLogOnceWithoutContactData(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry := fixture.createLink(t)
	details := httpContactDetails("Private agreement student")
	if result, err := fixture.store.Submit(t.Context(), entry.Token, details); err != nil || result != links.Saved {
		t.Fatalf("submit: %v, %v", result, err)
	}
	const date = "2027-02-28"
	if err := settings.New(fixture.db).SetProgramStartDate(t.Context(), date, httpUsername); err != nil {
		t.Fatal(err)
	}
	handler := fixture.routes(t, fixture.store)
	path := "/customer/" + entry.Token
	private := []string{details.Name, details.Email, details.Phone, details.Municipality, entry.Token, entry.ID,
		httpUsername, "reject_agreement", "reject_payment_choice", "unavailable_settings"}
	before := fixture.link(t, entry.Token)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE settings RENAME TO unavailable_settings").Error; err != nil {
		t.Fatal(err)
	}
	rec := serveHTTPLogging(t, handler, http.MethodGet, path+"/agreement", nil, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodGet, route: "GET /customer/{token}/agreement",
		operation: "link_read", stage: "load", kind: "database", sqlstate: "42P01",
	}, private...)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE unavailable_settings RENAME TO settings").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE links ADD CONSTRAINT reject_agreement CHECK (terms_accepted_at IS NULL)").Error; err != nil {
		t.Fatal(err)
	}
	consent := url.Values{"accept_terms": {"yes"}, "program_start_date": {date}}
	rec = serveHTTPLogging(t, handler, http.MethodPost, path+"/agreement", consent, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}/agreement",
		operation: "final_submit", stage: "save", kind: "database", sqlstate: "23514",
	}, private...)
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, before) {
		t.Fatal("failed agreement changed contact or consent")
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE links DROP CONSTRAINT reject_agreement").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	rec = serveHTTPLogging(t, handler, http.MethodPost, path+"/agreement", consent, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/payments" || rec.Body.Len() != 0 {
		t.Fatal("agreement retry did not advance quietly")
	}
	fixture.requireQuiet(t)
	before = fixture.link(t, entry.Token)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE links ADD CONSTRAINT reject_payment_choice CHECK (payment_plan = '')").Error; err != nil {
		t.Fatal(err)
	}
	rec = serveHTTPLogging(t, handler, http.MethodPost, path+"/payments", url.Values{"payment_plan": {"six"}}, nil)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}/payments",
		operation: "final_submit", stage: "save", kind: "database", sqlstate: "23514",
	}, private...)
	if after := fixture.link(t, entry.Token); !reflect.DeepEqual(after, before) {
		t.Fatal("failed choice changed contact, consent, or saved choice")
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE links DROP CONSTRAINT reject_payment_choice").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	for range 2 {
		rec = serveHTTPLogging(t, handler, http.MethodPost, path+"/payments", url.Values{"payment_plan": {"six"}}, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/payments" || rec.Body.Len() != 0 {
			t.Fatal("choice retry or replay did not advance quietly")
		}
		fixture.requireQuiet(t)
	}
}

func httpContactDetails(name string) students.Details {
	return students.Details{Name: name, Email: "Private+retained@example.com", Phone: "+17872345678", Municipality: "Río Grande"}
}

func httpContactForm(name string) url.Values {
	return url.Values{"name": {name}, "email": {" Private+logging@EXAMPLE.COM "},
		"email_confirm": {" Private+logging@example.com "}, "phone": {"(202) 555-0123"}, "municipality": {"Bayamón"}}
}

func httpContactValues(form url.Values) []string {
	var private []string
	for key, values := range form {
		for _, value := range values {
			if value == "" {
				continue
			}
			private = append(private, value, strings.TrimSpace(value))
			if field, ok := students.FindField(key); ok {
				if normalized, err := field.Parse(value, true); err == nil {
					private = append(private, normalized)
				}
			}
		}
	}
	return private
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
	fixture.store = links.New(fixture.db, nil)
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
	if err := management.RegisterRoutes(child, store, students.New(fixture.db, nil), settings.New(fixture.db), auth.Staff); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	manager.RegisterRoutes(root)
	root.Handle("/management/", manager.Require(child))
	if err := customer.RegisterRoutes(root, store, settings.New(fixture.db)); err != nil {
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
	got, err := students.New(fixture.db, nil).List(t.Context())
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
