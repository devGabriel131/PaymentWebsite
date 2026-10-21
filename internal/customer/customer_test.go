//go:build integration

package customer

import (
	"bufio"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestErrorResponsesKeepNameForm(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	store := links.New(db)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	token := entry.Token
	const existing = "Existing name"
	if saved, err := store.SaveDraft(t.Context(), token, existing); err != nil || saved != links.Saved {
		t.Fatalf("save existing name: saved=%v, err=%v", saved, err)
	}

	muxes := map[bool]http.Handler{
		false: newMux(t, store),
		true:  newMux(t, failingStore(t, db)),
	}
	const attempted = "  Ada & co  "
	cases := []struct {
		name            string
		method          string
		body            string
		status          int
		databaseFailure bool
		value           string
	}{
		{"malformed form", http.MethodPost, "name=%zz", http.StatusBadRequest, false, existing},
		{"oversized ASCII form", http.MethodPost, "name=" + strings.Repeat("a", 4100), http.StatusRequestEntityTooLarge, false, existing},
		{"oversized encoded Unicode form", http.MethodPost, url.Values{"name": {strings.Repeat("é", 683)}}.Encode(), http.StatusRequestEntityTooLarge, false, existing},
		{"blank final name", http.MethodPost, "name=+", http.StatusUnprocessableEntity, false, " "},
		{"NUL name", http.MethodPost, "name=A%00", http.StatusUnprocessableEntity, false, "A\x00"},
		{"invalid UTF-8 name", http.MethodPost, "name=%FF", http.StatusUnprocessableEntity, false, "\xff"},
		{"database failure on submission", http.MethodPost, url.Values{"name": {attempted}}.Encode(), http.StatusInternalServerError, true, attempted},
		{"database failure on entry", http.MethodGet, "", http.StatusInternalServerError, true, ""},
	}
	for _, tc := range cases {
		for _, mode := range []string{"ordinary", "htmx"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				path := "/customer/" + token
				r := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if mode == "htmx" {
					r.Header.Set("HX-Request", "true")
				}
				w := httptest.NewRecorder()
				muxes[tc.databaseFailure].ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d", w.Code, tc.status)
				}
				if contentType := w.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
					t.Errorf("Content-Type = %q, want HTML", contentType)
				}
				body := w.Body.String()
				requireFormError(t, body, path, mode == "ordinary" || tc.method != http.MethodPost)
				if !strings.Contains(body, `value="`+template.HTMLEscapeString(tc.value)+`"`) {
					t.Errorf("error response lost the expected name %q: %s", tc.value, body)
				}
			})
		}
	}
}

func TestAutosaveKeepsDraftWithoutFinishing(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	store := links.New(db)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, store)
	path := "/customer/" + entry.Token
	send := func(method, target, name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(url.Values{"name": {name}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	status := func() links.Status {
		entries, err := store.List(t.Context())
		if err != nil || len(entries) != 1 {
			t.Fatalf("list: %+v, %v", entries, err)
		}
		return entries[0].Status
	}

	steps := []struct {
		name, method, target, value string
		code                        int
		// field is the reloaded input value.
		field  string
		status links.Status
	}{
		{"blur saves a draft", http.MethodPost, path + "/draft", "Ana", http.StatusOK, "Ana", links.InProgress},
		{"invalid draft is not saved", http.MethodPost, path + "/draft", "A\x00", http.StatusUnprocessableEntity, "Ana", links.InProgress},
		{"cleared box is mirrored", http.MethodPost, path + "/draft", " ", http.StatusOK, "", links.Opened},
	}
	for i, step := range steps {
		w := send(step.method, step.target, step.value)
		if w.Code != step.code {
			t.Fatalf("%s: status = %d, want %d: %s", step.name, w.Code, step.code, w.Body.String())
		}
		if step.target != path && strings.Contains(w.Body.String(), "<input") {
			t.Fatalf("%s: autosave redrew the form: %s", step.name, w.Body.String())
		}
		if i == 0 && stored(t, db)[0].OpenedAt != nil {
			t.Fatal("draft POST recorded a first visit")
		}
		reload := httptest.NewRecorder()
		mux.ServeHTTP(reload, httptest.NewRequest(http.MethodGet, path, nil))
		if !strings.Contains(reload.Body.String(), `value="`+template.HTMLEscapeString(step.field)+`"`) {
			t.Fatalf("%s: reload lost the saved value %q: %s", step.name, step.field, reload.Body.String())
		}
		if got := status(); got != step.status {
			t.Fatalf("%s: status = %q, want %q", step.name, got, step.status)
		}
	}

	if enrolled := studentRecords(t, db); len(enrolled) != 0 {
		t.Fatalf("drafts created students: %+v", enrolled)
	}

	// Final submission replaces the whole question and form with confirmation.
	w := send(http.MethodPost, path, "Ana Pérez")
	if w.Code != http.StatusOK {
		t.Fatalf("submit: status = %d, body %q", w.Code, w.Body.String())
	}
	requireCompletion(t, w.Body.String(), false, "Ana Pérez")
	if enrolled := studentRecords(t, db); len(enrolled) != 1 || enrolled[0].Name != "Ana Pérez" {
		t.Fatalf("submit students = %+v, want one completed answer", enrolled)
	}
	if w := send(http.MethodPost, path+"/draft", "Changed after submit"); w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("draft after submit: status = %d, HX-Refresh = %q; want 409 page refresh", w.Code, w.Header().Get("HX-Refresh"))
	}
	reload := httptest.NewRecorder()
	mux.ServeHTTP(reload, httptest.NewRequest(http.MethodGet, path, nil))
	requireCompletion(t, reload.Body.String(), true, "Ana Pérez")
	if got := status(); got != links.Finished {
		t.Fatalf("status after locked changes = %q, want %q", got, links.Finished)
	}

	if _, err := store.Cancel(t.Context(), entry.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	if w := send(http.MethodPost, path+"/draft", "After attempted cancel"); w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("finished draft after cancel: status = %d, HX-Refresh = %q; want 409 page refresh", w.Code, w.Header().Get("HX-Refresh"))
	}
	canceled, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cancel(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	if w := send(http.MethodPost, "/customer/"+canceled.Token+"/draft", "After cancel"); w.Code != http.StatusNotFound || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("canceled unfinished draft: status = %d, HX-Refresh = %q; want 404 page refresh", w.Code, w.Header().Get("HX-Refresh"))
	}
}

func TestExpiredLinkShowsNotFound(t *testing.T) {
	store := links.New(testdb.Open(t, &links.Link{}, &students.Student{}))
	entry, err := store.Create(t.Context(), "staff", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, store)
	path := "/customer/" + entry.Token
	for _, tc := range []struct{ name, method, target, body string }{
		{"entry", http.MethodGet, path, ""},
		{"autosave", http.MethodPost, path + "/draft", "name=Ana"},
		{"submit", http.MethodPost, path, "name=Ana"},
		{"malformed final", http.MethodPost, path, "name=%zz"},
		{"malformed draft", http.MethodPost, path + "/draft", "name=%zz"},
		{"oversized final", http.MethodPost, path, "name=" + strings.Repeat("a", 4100)},
		{"oversized draft", http.MethodPost, path + "/draft", "name=" + strings.Repeat("a", 4100)},
		{"invalid final name", http.MethodPost, path, "name=+"},
		{"invalid draft name", http.MethodPost, path + "/draft", "name=A%00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") {
				t.Fatalf("status = %d, want the 404 page: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSubmissionFailureRollsBackAndAllowsRetry(t *testing.T) {
	for _, mode := range []string{"ordinary", "htmx"} {
		t.Run(mode, func(t *testing.T) {
			db := testdb.Open(t, &links.Link{}, &students.Student{})
			store := links.New(db)
			entry, err := store.Create(t.Context(), "staff", later)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := store.SaveDraft(t.Context(), entry.Token, "Original draft"); err != nil || saved != links.Saved {
				t.Fatalf("save draft: saved=%v, err=%v", saved, err)
			}
			if err := db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT customer_submit_rejected CHECK (name <> 'Reject completion')").Error; err != nil {
				t.Fatal(err)
			}
			mux := newMux(t, store)
			path := "/customer/" + entry.Token
			attempted := "  Reject completion  "
			send := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"name": {attempted}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if mode == "htmx" {
					r.Header.Set("HX-Request", "true")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			before := stored(t, db)
			w := send()
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("rejected insert: status = %d, body %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			requireFormError(t, body, path, mode == "ordinary")
			if !strings.Contains(body, `value="`+template.HTMLEscapeString(attempted)+`"`) {
				t.Fatalf("failed completion lost its attempted name: %s", body)
			}
			if after := stored(t, db); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed student insert changed link from %+v to %+v", before, after)
			}
			if enrolled := studentRecords(t, db); len(enrolled) != 0 {
				t.Fatalf("failed student insert retained students: %+v", enrolled)
			}

			if err := db.WithContext(t.Context()).Exec("ALTER TABLE students DROP CONSTRAINT customer_submit_rejected").Error; err != nil {
				t.Fatal(err)
			}
			w = send()
			if w.Code != http.StatusOK {
				t.Fatalf("retry: status = %d, body %s", w.Code, w.Body.String())
			}
			requireCompletion(t, w.Body.String(), mode == "ordinary", attempted, strings.TrimSpace(attempted))
			completed := stored(t, db)[0]
			enrolled := studentRecords(t, db)
			if completed.Name != strings.TrimSpace(attempted) || completed.SubmittedAt == nil || completed.OpenedAt != nil ||
				len(enrolled) != 1 || enrolled[0].Name != completed.Name ||
				!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) ||
				completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
				t.Fatalf("retry did not commit matching completion and student: link %+v, students %+v", completed, enrolled)
			}
		})
	}
}

func TestCompletedAccessAndReplay(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	store := links.New(db)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	const submittedName = "Submission snapshot"
	const canonicalName = "Edited student name"
	// Submit directly so replay must not even fill in the absent first-visit time.
	if saved, err := store.Submit(t.Context(), entry.Token, submittedName); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	enrolled := studentRecords(t, db)
	if len(enrolled) != 1 {
		t.Fatalf("students = %+v, want one", enrolled)
	}
	if _, err := students.New(db).UpdateField(t.Context(), enrolled[0].ID, "name", canonicalName); err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, store)
	path := "/customer/" + entry.Token
	before, studentsBefore := stored(t, db), studentRecords(t, db)
	for _, mode := range []string{"ordinary", "htmx"} {
		for _, tc := range []struct {
			name, suffix, body string
			status             int
		}{
			{"valid final replay", "", "name=Replacement", http.StatusOK},
			{"malformed final replay", "", "name=%zz", http.StatusOK},
			{"oversized final replay", "", "name=" + strings.Repeat("x", 5000), http.StatusOK},
			{"valid stale draft", "/draft", "name=Replacement", http.StatusConflict},
			{"malformed stale draft", "/draft", "name=%zz", http.StatusConflict},
			{"oversized stale draft", "/draft", "name=" + strings.Repeat("x", 5000), http.StatusConflict},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, path+tc.suffix, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if mode == "htmx" {
					r.Header.Set("HX-Request", "true")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				refresh := ""
				if mode == "htmx" && tc.status == http.StatusConflict {
					refresh = "true"
				}
				if got := w.Header().Get("HX-Refresh"); got != refresh {
					t.Fatalf("HX-Refresh = %q, want %q", got, refresh)
				}
				requireCompletion(t, w.Body.String(), mode == "ordinary" || tc.status == http.StatusConflict, submittedName, canonicalName)
				if after := stored(t, db); !reflect.DeepEqual(after, before) {
					t.Fatalf("completed POST changed link from %+v to %+v", before, after)
				}
				if after := studentRecords(t, db); !reflect.DeepEqual(after, studentsBefore) {
					t.Fatalf("completed POST changed students from %+v to %+v", studentsBefore, after)
				}
			})
		}
	}
	for _, htmx := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if htmx {
			r.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("completed GET (htmx=%v): status = %d", htmx, w.Code)
		}
		requireCompletion(t, w.Body.String(), true, submittedName, canonicalName)
	}
	server := serve(t, store)
	headKeeps(t, db, server, path, http.StatusOK)

	if err := db.WithContext(t.Context()).Model(&links.Link{}).Where("token_hash = ?", before[0].TokenHash).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodPost, ""},
		{http.MethodPost, "/draft"},
	} {
		r := httptest.NewRequest(tc.method, path+tc.suffix, strings.NewReader("name=Replacement"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expired completion %s %s: status = %d", tc.method, tc.suffix, w.Code)
		}
	}
	headKeeps(t, db, server, path, http.StatusNotFound)
	if after := studentRecords(t, db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatalf("completion access or expiry changed retained students from %+v to %+v", studentsBefore, after)
	}
}

// TestHeadChecksLinkWithoutChangingIt follows one link through its life. HEAD
// only reports whether it opens; GET still records the customer's first visit.
func TestHeadChecksLinkWithoutChangingIt(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	store := links.New(db)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, store)
	path := "/customer/" + entry.Token
	client := &http.Client{Timeout: httpTimeout}
	// send makes a request carrying a name, as a stray edit would.
	send := func(t *testing.T, method, target string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, strings.NewReader("name=Changed"))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	headKeeps(t, db, server, path, http.StatusOK)
	if entries, err := store.List(t.Context()); err != nil || len(entries) != 1 || entries[0].Status != links.NotOpened {
		t.Fatalf("after HEAD: entries %+v, err %v; want one %q", entries, err, links.NotOpened)
	}

	if status := send(t, http.MethodGet, path); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if stored(t, db)[0].OpenedAt == nil {
		t.Fatal("GET did not record the first visit")
	}
	headKeeps(t, db, server, path, http.StatusOK)

	if saved, err := store.SaveDraft(t.Context(), entry.Token, "Ana"); err != nil || saved != links.Saved {
		t.Fatalf("save draft: saved=%v, err=%v", saved, err)
	}
	headKeeps(t, db, server, path, http.StatusOK)

	// Autosave and the page answer no other methods, so none can edit the draft.
	before := stored(t, db)
	for _, tc := range []struct{ name, method, target string }{
		{"HEAD autosave", http.MethodHead, path + "/draft"},
		{"GET autosave", http.MethodGet, path + "/draft"},
		{"PUT page", http.MethodPut, path},
		{"DELETE page", http.MethodDelete, path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status := send(t, tc.method, tc.target); status != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", status)
			}
		})
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("unsupported methods changed stored links from %+v to %+v", before, after)
	}

	// A submitted link still opens read-only: HEAD answers 200, not the 409 that locks edits.
	if saved, err := store.Submit(t.Context(), entry.Token, "Ana Pérez"); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	headKeeps(t, db, server, path, http.StatusOK)

	// A failed lookup is a server error, never a guess about the link.
	headKeeps(t, db, serve(t, failingStore(t, db)), path, http.StatusInternalServerError)
}

func TestHeadRejectsUnusableLinks(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	store := links.New(db)
	canceled, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cancel(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	expired, err := store.Create(t.Context(), "staff", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, store)
	for _, tc := range []struct{ name, token string }{
		{"unknown", strings.Repeat("A", len(canceled.Token))},
		{"malformed", "not-a-token"},
		{"canceled", canceled.Token},
		{"expired", expired.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headKeeps(t, db, server, "/customer/"+tc.token, http.StatusNotFound)
		})
	}
}

// httpTimeout bounds each HTTP exchange, so a stuck handler fails its test.
const httpTimeout = 10 * time.Second

func newMux(t *testing.T, store *links.Store) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store); err != nil {
		t.Fatal(err)
	}
	return logging.Requests(logging.New(io.Discard), mux)
}

// serve runs the customer routes behind a real HTTP server.
func serve(t *testing.T, store *links.Store) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newMux(t, store))
	t.Cleanup(server.Close)
	return server
}

// failingStore returns a store whose every query fails: its transaction is
// already rolled back.
func failingStore(t *testing.T, db *gorm.DB) *links.Store {
	t.Helper()
	tx := db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	return links.New(tx)
}

// stored reads every link directly, never through Open, so checks record no visit.
func stored(t *testing.T, db *gorm.DB) []links.Link {
	t.Helper()
	var all []links.Link
	if err := db.WithContext(t.Context()).Order("token_hash").Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	return all
}

func studentRecords(t *testing.T, db *gorm.DB) []students.Student {
	t.Helper()
	all, err := students.New(db).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func requireFormError(t *testing.T, body, path string, fullPage bool) {
	t.Helper()
	if strings.Count(body, "<form ") != 1 || !strings.Contains(body, `action="`+path+`"`) ||
		!strings.Contains(body, `name="name"`) || !strings.Contains(body, `type="submit"`) {
		t.Fatalf("error response lost the retryable name form: %s", body)
	}
	if strings.Count(body, `<section id="customer-content">`) != 1 ||
		!strings.Contains(body, `hx-target="#customer-content"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Fatalf("error response lost its retryable section target: %s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Errorf("error response lost its inline error: %s", body)
	}
	if got := strings.Contains(body, "<!doctype html>"); got != fullPage {
		t.Errorf("error response full page = %v, want %v", got, fullPage)
	}
}

func requireCompletion(t *testing.T, body string, fullPage bool, private ...string) {
	t.Helper()
	if strings.Count(body, `<section id="customer-content">`) != 1 ||
		strings.Count(body, "<title>") != 1 {
		t.Fatalf("completion lost its section or single title: %s", body)
	}
	if got := strings.Contains(body, "<!doctype html>"); got != fullPage {
		t.Fatalf("completion full page = %v, want %v: %s", got, fullPage, body)
	}
	for _, hidden := range append(private, "<input", "<form") {
		if strings.Contains(body, hidden) {
			t.Fatalf("completion exposed %q: %s", hidden, body)
		}
	}
}

// headKeeps sends HEAD for path, plain and from HTMX, expecting want each
// time, and fails if any stored link changed.
func headKeeps(t *testing.T, db *gorm.DB, server *httptest.Server, path string, want int) {
	t.Helper()
	before := stored(t, db)
	for _, htmx := range []bool{false, true} {
		if got := head(t, server, path, htmx); got != want {
			t.Fatalf("HEAD %s (htmx=%v): status = %d, want %d", path, htmx, got, want)
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("HEAD %s changed stored links from %+v to %+v", path, before, after)
	}
}

// head sends one HEAD request for path, checks the customer response headers,
// and returns the status. http.Client never reads a HEAD response body, so
// head reads the raw connection: the request asks the server to close it after
// replying, and any byte after the headers would be a body.
func head(t *testing.T, server *httptest.Server, path string, htmx bool) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), httpTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(httpTimeout)); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close = true
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	if err := r.Write(conn); err != nil {
		t.Fatal(err)
	}
	wire := bufio.NewReader(conn)
	resp, err := http.ReadResponse(wire, r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(wire); err != nil || len(body) != 0 {
		t.Fatalf("HEAD %s (htmx=%v) sent body %q, read error %v", path, htmx, body, err)
	}
	refresh := ""
	if htmx && resp.StatusCode == http.StatusNotFound {
		refresh = "true"
	}
	for name, want := range map[string]string{
		"Cache-Control":   "no-store",
		"Referrer-Policy": "no-referrer",
		"Vary":            "HX-Request",
		"Content-Type":    "text/html; charset=utf-8",
		"HX-Refresh":      refresh,
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("HEAD %s (htmx=%v): %s = %q, want %q", path, htmx, name, got, want)
		}
	}
	return resp.StatusCode
}
