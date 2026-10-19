//go:build integration

package customer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/testdb"
)

func TestErrorResponsesKeepNameForm(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
	key := make([]byte, 32)
	store, err := links.New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	token := entry.Token
	if saved, err := store.SaveName(t.Context(), token, "Existing name"); err != nil || !saved {
		t.Fatalf("save existing name: saved=%v, err=%v", saved, err)
	}

	failedTx := db.WithContext(t.Context()).Begin()
	if failedTx.Error != nil {
		t.Fatal(failedTx.Error)
	}
	if err := failedTx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	failedStore, err := links.New(failedTx, key)
	if err != nil {
		t.Fatal(err)
	}
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
				if !strings.Contains(body, `role="alert"`) || strings.Contains(body, `role="status"`) {
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
