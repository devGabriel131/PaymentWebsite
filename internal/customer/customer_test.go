//go:build integration

package customer

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

	failedTx := db.WithContext(t.Context()).Begin()
	if failedTx.Error != nil {
		t.Fatal(failedTx.Error)
	}
	if err := failedTx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	failedStore := links.New(failedTx)
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

	if err := store.Cancel(t.Context(), entry.ID, "staff"); err != nil {
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
