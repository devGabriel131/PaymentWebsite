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
	"paymentwebsite/internal/testdb"
)

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestErrorResponsesKeepNameForm(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
	store := links.New(db)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	token := entry.Token
	if saved, err := store.SaveDraft(t.Context(), token, "Existing name"); err != nil || !saved {
		t.Fatalf("save existing name: saved=%v, err=%v", saved, err)
	}

	failedStore := failingStore(t, db)
	muxes := map[bool]*http.ServeMux{}
	for failed, s := range map[bool]*links.Store{false: store, true: failedStore} {
		mux := http.NewServeMux()
		if err := RegisterRoutes(mux, s); err != nil {
			t.Fatal(err)
		}
		muxes[failed] = mux
	}

	cases := []struct {
		name         string
		method       string
		body         string
		status       int
		failedLookup bool
	}{
		{"malformed form", http.MethodPost, "name=%zz", http.StatusBadRequest, false},
		{"oversized ASCII form", http.MethodPost, "name=" + strings.Repeat("a", 4100), http.StatusRequestEntityTooLarge, false},
		{"oversized encoded Unicode form", http.MethodPost, url.Values{"name": {strings.Repeat("é", 683)}}.Encode(), http.StatusRequestEntityTooLarge, false},
		{"lookup failure on submission", http.MethodPost, "name=Ada", http.StatusInternalServerError, true},
		{"lookup failure on entry", http.MethodGet, "", http.StatusInternalServerError, true},
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
				muxes[tc.failedLookup].ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d", w.Code, tc.status)
				}
				if contentType := w.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
					t.Errorf("Content-Type = %q, want HTML", contentType)
				}
				body := w.Body.String()
				if strings.Count(body, "<form ") != 1 || !strings.Contains(body, `action="`+path+`"`) ||
					!strings.Contains(body, `name="name"`) || !strings.Contains(body, `type="submit"`) {
					t.Fatalf("error response lost the retryable name form: %s", body)
				}
				if !strings.Contains(body, `role="alert"`) || strings.Contains(body, "has been saved") {
					t.Errorf("error response must show an inline error, not saved confirmation: %s", body)
				}
				wantPage := mode == "ordinary" || tc.method != http.MethodPost
				if fullPage := strings.Contains(body, "<!doctype html>"); fullPage != wantPage {
					t.Errorf("full page = %v, want %v", fullPage, wantPage)
				}
			})
		}
	}
}

func TestAutosaveKeepsDraftWithoutFinishing(t *testing.T) {
	store := links.New(testdb.Open(t, &links.Link{}))
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store); err != nil {
		t.Fatal(err)
	}
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
		// body must appear in the response; field is the reloaded input value.
		body, field string
		status      links.Status
	}{
		{"blur saves a draft", http.MethodPost, path + "/draft", "Ana", http.StatusOK, "Saved.", "Ana", links.InProgress},
		{"invalid draft is not saved", http.MethodPost, path + "/draft", "A\x00", http.StatusUnprocessableEntity, "Not saved.", "Ana", links.InProgress},
		{"cleared box is mirrored", http.MethodPost, path + "/draft", " ", http.StatusOK, "Saved.", "", links.Opened},
	}
	for _, step := range steps {
		w := send(step.method, step.target, step.value)
		if w.Code != step.code || !strings.Contains(w.Body.String(), step.body) {
			t.Fatalf("%s: status = %d, body %q; want %d containing %q", step.name, w.Code, w.Body.String(), step.code, step.body)
		}
		if step.target != path && strings.Contains(w.Body.String(), "<input") {
			t.Fatalf("%s: autosave redrew the form: %s", step.name, w.Body.String())
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

	// Submit finishes and locks the form; reload shows it read-only.
	if w := send(http.MethodPost, path, "Ana Pérez"); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "Thanks, Ana Pérez") || strings.Contains(w.Body.String(), "<input") {
		t.Fatalf("submit: status = %d, body %q; want read-only thanks", w.Code, w.Body.String())
	}
	for name, target := range map[string]string{"autosave": path + "/draft", "submit": path} {
		if w := send(http.MethodPost, target, "Changed after submit"); w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
			t.Fatalf("%s after submit: status = %d, HX-Refresh = %q; want 409 page refresh", name, w.Code, w.Header().Get("HX-Refresh"))
		}
	}
	reload := httptest.NewRecorder()
	mux.ServeHTTP(reload, httptest.NewRequest(http.MethodGet, path, nil))
	if body := reload.Body.String(); !strings.Contains(body, "Thanks, Ana Pérez") || strings.Contains(body, "<input") {
		t.Fatalf("reload after submit must be read-only: %s", body)
	}
	if got := status(); got != links.Finished {
		t.Fatalf("status after locked changes = %q, want %q", got, links.Finished)
	}

	if _, err := store.Cancel(t.Context(), entry.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	if w := send(http.MethodPost, path+"/draft", "After cancel"); w.Code != http.StatusNotFound || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("canceled draft: status = %d, HX-Refresh = %q; want 404 page refresh", w.Code, w.Header().Get("HX-Refresh"))
	}
}

func TestExpiredLinkShowsNotFound(t *testing.T) {
	store := links.New(testdb.Open(t, &links.Link{}))
	entry, err := store.Create(t.Context(), "staff", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store); err != nil {
		t.Fatal(err)
	}
	path := "/customer/" + entry.Token
	for _, tc := range []struct{ name, method, target string }{
		{"entry", http.MethodGet, path},
		{"autosave", http.MethodPost, path + "/draft"},
		{"submit", http.MethodPost, path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader("name=Ana"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") {
				t.Fatalf("status = %d, want the 404 page: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestHeadChecksLinkWithoutChangingIt follows one link through its life. HEAD
// only reports whether it opens; GET still records the customer's first visit.
func TestHeadChecksLinkWithoutChangingIt(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
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

	if saved, err := store.SaveDraft(t.Context(), entry.Token, "Ana"); err != nil || !saved {
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
	if saved, err := store.Submit(t.Context(), entry.Token, "Ana Pérez"); err != nil || !saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	headKeeps(t, db, server, path, http.StatusOK)

	// A failed lookup is a server error, never a guess about the link.
	headKeeps(t, db, serve(t, failingStore(t, db)), path, http.StatusInternalServerError)
}

func TestHeadRejectsUnusableLinks(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
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

// serve runs the customer routes behind a real HTTP server.
func serve(t *testing.T, store *links.Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
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
