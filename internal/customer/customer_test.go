//go:build integration

package customer

import (
	"bufio"
	"fmt"
	"html"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestErrorResponsesKeepStudentForm(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	existing := contactDetails("Existing name")
	saveDetails(t, store, entry.Token, existing)
	muxes := map[bool]http.Handler{
		false: newMux(t, db, store),
		true:  newMux(t, db, failingStore(t, db)),
	}
	attempted := contactForm("  Ada Lovelace  ")
	attempted.Set("email", "  Ada+private@EXAMPLE.com  ")
	attempted.Set("email_confirm", " Ada+private@example.COM ")
	attempted.Set("phone", " (202) 555-0123 ")
	attempted.Set("municipality", " Bayamón ")
	cases := []struct {
		name            string
		method          string
		body            string
		status          int
		databaseFailure bool
		values          url.Values
	}{
		{"malformed form", http.MethodPost, "name=%zz", http.StatusBadRequest, false, detailsForm(existing)},
		{"oversized ASCII form", http.MethodPost, "name=" + strings.Repeat("a", 4100), http.StatusRequestEntityTooLarge, false, detailsForm(existing)},
		{"oversized encoded Unicode form", http.MethodPost, url.Values{"name": {strings.Repeat("é", 683)}}.Encode(), http.StatusRequestEntityTooLarge, false, detailsForm(existing)},
		{"database failure on submission", http.MethodPost, attempted.Encode(), http.StatusInternalServerError, true, attempted},
		{"database failure on entry", http.MethodGet, "", http.StatusInternalServerError, true, detailsForm(students.Details{})},
	}
	for _, tc := range cases {
		for _, htmx := range []bool{false, true} {
			t.Run(tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
				path := "/customer/" + entry.Token
				target := path
				if tc.method == http.MethodGet {
					target += "/forms"
				}
				w := customerRequest(t, muxes[tc.databaseFailure], tc.method, target, tc.body, htmx)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				body := w.Body.String()
				requireFormError(t, body, path, !htmx || tc.method != http.MethodPost)
				requireAttempted(t, body, tc.values)
			})
		}
	}
}

func TestAutosaveKeepsDraftWithoutFinishing(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, db, store)
	path := "/customer/" + entry.Token
	want := students.Details{}
	steps := []struct {
		name, key, raw, value string
		code                  int
	}{
		{"partial name", "name", " Ana ", "Ana", http.StatusOK},
		{"email normalization", "email", " Ada+draft@EXAMPLE.com ", "Ada+draft@example.com", http.StatusOK},
		{"phone normalization", "phone", "(787) 234-5678", "+17872345678", http.StatusOK},
		{"municipality", "municipality", " Bayamón ", "Bayamón", http.StatusOK},
		{"invalid name", "name", "<script>alert(1)</script>", "Ana", http.StatusUnprocessableEntity},
		{"invalid email", "email", "invalid", "Ada+draft@example.com", http.StatusUnprocessableEntity},
		{"invalid phone", "phone", "123", "+17872345678", http.StatusUnprocessableEntity},
		{"forged municipality", "municipality", "Atlantis", "Bayamón", http.StatusUnprocessableEntity},
		{"clear name", "name", "", "", http.StatusOK},
		{"clear email", "email", "", "", http.StatusOK},
		{"clear phone", "phone", "", "", http.StatusOK},
		{"clear municipality", "municipality", "", "", http.StatusOK},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			// Unrelated parameters must not be parsed or written by this draft.
			form := url.Values{"name": {"Bad <name>"}, "email": {"invalid"}, "phone": {"invalid"},
				"municipality": {"invalid"}, "email_confirm": {"never persisted"}}
			form.Set(step.key, step.raw)
			w := customerRequest(t, mux, http.MethodPost, path+"/draft/"+step.key, form.Encode(), true)
			if w.Code != step.code || strings.Contains(w.Body.String(), "<input") {
				t.Fatalf("draft status = %d, want %d, fragment %s", w.Code, step.code, w.Body.String())
			}
			marker := `data-validation="valid"`
			if step.code == http.StatusUnprocessableEntity {
				marker = `data-validation="invalid"`
			}
			if !strings.Contains(w.Body.String(), marker) {
				t.Fatalf("draft lacks validation marker %s: %s", marker, w.Body.String())
			}
			switch step.key {
			case "name":
				want.Name = step.value
			case "email":
				want.Email = step.value
			case "phone":
				want.Phone = step.value
			case "municipality":
				want.Municipality = step.value
			}
			link := stored(t, db)[0]
			if link.Details != want || link.SubmittedAt != nil || len(studentRecords(t, db)) != 0 {
				t.Fatalf("draft changed unrelated data: link %+v, want details %+v", link, want)
			}
			if i == 0 && link.OpenedAt != nil {
				t.Fatal("draft POST recorded a first visit")
			}
			reload := customerRequest(t, mux, http.MethodGet, path+"/forms", "", false)
			requireAttempted(t, reload.Body.String(), detailsForm(want))
			entries, err := store.List(t.Context())
			if err != nil || len(entries) != 1 {
				t.Fatalf("list: %+v, %v", entries, err)
			}
			status := links.InProgress
			if want == (students.Details{}) {
				status = links.Opened
			}
			if entries[0].Status != status {
				t.Fatalf("status = %q, want %q", entries[0].Status, status)
			}
		})
	}

	final := contactForm("Ana Pérez")
	w := customerRequest(t, mux, http.MethodPost, path, final.Encode(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("submit: status = %d, body %q", w.Code, w.Body.String())
	}
	requireAdvance(t, w, path+"/agreement", true, privateFormValues(final)...)
	if enrolled := studentRecords(t, db); len(enrolled) != 1 || enrolled[0].Details != contactDetails("Ana Pérez") {
		t.Fatalf("submit students = %+v", enrolled)
	}
	before := stored(t, db)
	for _, suffix := range []string{"/draft/name", "/draft/email", "/draft/phone", "/draft/municipality"} {
		key := strings.TrimPrefix(suffix, "/draft/")
		w := customerRequest(t, mux, http.MethodPost, path+suffix, url.Values{key: {""}}.Encode(), true)
		if w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
			t.Fatalf("draft after submit: status = %d, HX-Refresh = %q", w.Code, w.Header().Get("HX-Refresh"))
		}
		requireNoPrivate(t, w.Body.String(), privateFormValues(final)...)
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("late drafts changed completed details: before %+v, after %+v", before, after)
	}
	if _, err := store.Cancel(t.Context(), entry.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	w = customerRequest(t, mux, http.MethodPost, path+"/draft/name", "name=After+attempted+cancel", true)
	if w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("finished draft after cancel: status = %d, HX-Refresh = %q", w.Code, w.Header().Get("HX-Refresh"))
	}
}

func TestUnusableLinksTakePrecedence(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	expired, err := store.Create(t.Context(), "staff", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cancel(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, db, store)
	before := stored(t, db)
	for _, token := range []string{expired.Token, canceled.Token, strings.Repeat("A", len(expired.Token)), "not-a-token"} {
		path := "/customer/" + token
		for _, tc := range []struct{ name, method, suffix, body string }{
			{"entry", http.MethodGet, "", ""},
			{"forms", http.MethodGet, "/forms", ""},
			{"valid final", http.MethodPost, "", contactForm("Ana Pérez").Encode()},
			{"valid draft", http.MethodPost, "/draft/name", "name=Ana"},
			{"malformed final", http.MethodPost, "", "name=%zz"},
			{"malformed draft", http.MethodPost, "/draft/name", "name=%zz"},
			{"oversized final", http.MethodPost, "", "name=" + strings.Repeat("a", 4100)},
			{"oversized draft", http.MethodPost, "/draft/name", "name=" + strings.Repeat("a", 4100)},
			{"invalid final", http.MethodPost, "", "name=+"},
			{"invalid draft", http.MethodPost, "/draft/name", "name=A%00"},
			{"missing draft parameter", http.MethodPost, "/draft/email", ""},
			{"duplicate draft parameter", http.MethodPost, "/draft/email", "email=&email="},
		} {
			for _, htmx := range []bool{false, true} {
				t.Run(token+"/"+tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
					w := customerRequest(t, mux, tc.method, path+tc.suffix, tc.body, htmx)
					if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") {
						t.Fatalf("status = %d, want branded 404: %s", w.Code, w.Body.String())
					}
					if (w.Header().Get("HX-Refresh") == "true") != htmx {
						t.Fatalf("HX-Refresh = %q, htmx = %v", w.Header().Get("HX-Refresh"), htmx)
					}
				})
			}
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) || len(studentRecords(t, db)) != 0 {
		t.Fatalf("unusable requests changed stored state: before %+v, after %+v", before, after)
	}
}

func TestSubmissionFailureRollsBackAndAllowsRetry(t *testing.T) {
	for _, mode := range []string{"ordinary", "htmx"} {
		t.Run(mode, func(t *testing.T) {
			db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
			store := links.New(db, nil)
			entry, err := store.Create(t.Context(), "staff", later)
			if err != nil {
				t.Fatal(err)
			}
			saveDetails(t, store, entry.Token, contactDetails("Original draft"))
			if err := db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT customer_submit_rejected CHECK (name <> 'Reject completion')").Error; err != nil {
				t.Fatal(err)
			}
			mux := newMux(t, db, store)
			path := "/customer/" + entry.Token
			attempted := "  Reject completion  "
			form := contactForm(attempted)
			form.Set("email", " Retry+private@EXAMPLE.COM ")
			form.Set("email_confirm", "Retry+private@example.com")
			form.Set("phone", " (202) 555-0123 ")
			form.Set("municipality", " Bayamón ")
			send := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
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
			requireAttempted(t, body, form)
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
			requireAdvance(t, w, path+"/agreement", mode == "htmx", privateFormValues(form)...)
			completed := stored(t, db)[0]
			enrolled := studentRecords(t, db)
			want := students.Details{Name: "Reject completion", Email: "Retry+private@example.com", Phone: "+12025550123", Municipality: "Bayamón"}
			if completed.Details != want || completed.SubmittedAt == nil || completed.OpenedAt != nil ||
				len(enrolled) != 1 || enrolled[0].Details != completed.Details ||
				!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) ||
				completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
				t.Fatalf("retry did not commit matching completion and student: link %+v, students %+v", completed, enrolled)
			}
		})
	}
}

func TestCompletedAccessAndReplay(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	const submittedName = "Submission snapshot"
	const canonicalName = "Edited student name"
	// Submit directly so replay must not even fill in the absent first-visit time.
	if saved, err := store.Submit(t.Context(), entry.Token, contactDetails(submittedName)); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	enrolled := studentRecords(t, db)
	if len(enrolled) != 1 {
		t.Fatalf("students = %+v, want one", enrolled)
	}
	if _, err := students.New(db, nil).UpdateField(t.Context(), enrolled[0].ID, "name", canonicalName); err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, db, store)
	path := "/customer/" + entry.Token
	before, studentsBefore := stored(t, db), studentRecords(t, db)
	replay := contactForm("Replacement private")
	replay.Set("email", "Replay+private@example.com")
	replay.Set("email_confirm", " Replay+private@EXAMPLE.COM ")
	mismatched := contactForm("Private mismatch")
	mismatched.Set("email_confirm", "Mismatch+private@example.com")
	private := append(privateFormValues(replay), privateFormValues(contactForm(submittedName))...)
	private = append(private, canonicalName)
	private = append(private, privateFormValues(mismatched)...)
	for _, mode := range []string{"ordinary", "htmx"} {
		for _, tc := range []struct {
			name, suffix, body string
			status             int
		}{
			{"valid final replay", "", replay.Encode(), http.StatusOK},
			{"mismatched final replay", "", mismatched.Encode(), http.StatusOK},
			{"invalid final replay", "", "name=%3Cscript%3E&email=invalid&email_confirm=other&phone=123&municipality=Atlantis", http.StatusOK},
			{"malformed final replay", "", "name=%zz", http.StatusOK},
			{"oversized final replay", "", "name=" + strings.Repeat("x", 5000), http.StatusOK},
			{"valid stale draft", "/draft/name", "name=Replacement", http.StatusConflict},
			{"invalid stale draft", "/draft/phone", "phone=123", http.StatusConflict},
			{"missing stale draft", "/draft/email", "", http.StatusConflict},
			{"duplicate stale draft", "/draft/email", "email=&email=", http.StatusConflict},
			{"malformed stale draft", "/draft/name", "name=%zz", http.StatusConflict},
			{"oversized stale draft", "/draft/name", "name=" + strings.Repeat("x", 5000), http.StatusConflict},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, path+tc.suffix, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if mode == "htmx" {
					r.Header.Set("HX-Request", "true")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				wantStatus := tc.status
				if tc.status == http.StatusOK && mode == "ordinary" {
					wantStatus = http.StatusSeeOther
				}
				if w.Code != wantStatus {
					t.Fatalf("status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
				}
				refresh := ""
				if mode == "htmx" && tc.status == http.StatusConflict {
					refresh = "true"
				}
				if got := w.Header().Get("HX-Refresh"); got != refresh {
					t.Fatalf("HX-Refresh = %q, want %q", got, refresh)
				}
				if tc.status == http.StatusOK {
					requireAdvance(t, w, path+"/agreement", mode == "htmx", private...)
				} else {
					requireNoPrivate(t, w.Body.String(), private...)
				}
				if after := stored(t, db); !reflect.DeepEqual(after, before) {
					t.Fatalf("completed POST changed link from %+v to %+v", before, after)
				}
				if after := studentRecords(t, db); !reflect.DeepEqual(after, studentsBefore) {
					t.Fatalf("completed POST changed students from %+v to %+v", studentsBefore, after)
				}
			})
		}
	}
	for _, target := range []string{path, path + "/forms"} {
		for _, htmx := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, target, nil)
			if htmx {
				r.Header.Set("HX-Request", "true")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			requireAdvance(t, w, path+"/agreement", false, private...)
		}
	}
	server := serve(t, db, store)
	headKeeps(t, db, server, path, http.StatusOK)

	if err := db.WithContext(t.Context()).Model(&links.Link{}).Where("token_hash = ?", before[0].TokenHash).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodGet, "/forms"},
		{http.MethodPost, ""},
		{http.MethodPost, "/draft/name"},
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
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, db, store)
	path := "/customer/" + entry.Token
	client := &http.Client{Timeout: httpTimeout}
	// send makes a request carrying a name, as a stray edit would.
	send := func(t *testing.T, method, target string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, strings.NewReader(contactForm("Changed student").Encode()))
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
	headKeeps(t, db, server, path+"/forms", http.StatusOK)
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

	saveDetails(t, store, entry.Token, contactDetails("Draft student"))
	headKeeps(t, db, server, path, http.StatusOK)

	// Autosave and the page answer no other methods, so none can edit the draft.
	before := stored(t, db)
	for _, tc := range []struct{ name, method, target string }{
		{"HEAD autosave", http.MethodHead, path + "/draft/name"},
		{"GET autosave", http.MethodGet, path + "/draft/name"},
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
	if saved, err := store.Submit(t.Context(), entry.Token, contactDetails("Ana Pérez")); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	headKeeps(t, db, server, path, http.StatusOK)

	// A failed lookup is a server error, never a guess about the link.
	headKeeps(t, db, serve(t, db, failingStore(t, db)), path, http.StatusInternalServerError)
}

func TestHeadRejectsUnusableLinks(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
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
	server := serve(t, db, store)
	for _, tc := range []struct{ name, token string }{
		{"unknown", strings.Repeat("A", len(canceled.Token))},
		{"malformed", "not-a-token"},
		{"canceled", canceled.Token},
		{"expired", expired.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headKeeps(t, db, server, "/customer/"+tc.token, http.StatusNotFound)
			headKeeps(t, db, server, "/customer/"+tc.token+"/forms", http.StatusNotFound)
		})
	}
}

func TestConcurrentDraftFieldsRemainIsolated(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	want := contactDetails("Original student")
	saveDetails(t, store, entry.Token, want)
	mux := newMux(t, db, store)
	responses := make(chan *httptest.ResponseRecorder, 2)
	for key, form := range map[string]url.Values{
		"name":  {"name": {"Updated name"}, "phone": {"invalid unrelated phone"}},
		"phone": {"phone": {"(202) 555-0123"}, "name": {"invalid <name>"}},
	} {
		go func() {
			r := httptest.NewRequest(http.MethodPost, "/customer/"+entry.Token+"/draft/"+key, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			responses <- w
		}()
	}
	for range 2 {
		w := <-responses
		if w.Code != http.StatusOK {
			t.Errorf("concurrent draft status = %d: %s", w.Code, w.Body.String())
		}
	}
	want.Name, want.Phone = "Updated name", "+12025550123"
	if link := stored(t, db)[0]; link.Details != want || link.SubmittedAt != nil || len(studentRecords(t, db)) != 0 {
		t.Fatalf("concurrent drafts lost an independent update: %+v, want %+v", link, want)
	}
}

func TestDraftCardinalityAllowlistAndFailures(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	saveDetails(t, store, entry.Token, contactDetails("Saved student"))
	path := "/customer/" + entry.Token
	before := stored(t, db)
	mux := newMux(t, db, store)
	for _, field := range students.Fields() {
		for _, tc := range []struct{ name, suffix, body string }{
			{"missing", "", "unrelated=ignored"},
			{"query cannot supply missing POST value", "?" + field.Key + "=ignored", ""},
			{"duplicate", "", url.Values{field.Key: {"", ""}}.Encode()},
		} {
			for _, htmx := range []bool{false, true} {
				t.Run(field.Key+"/"+tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
					w := customerRequest(t, mux, http.MethodPost, path+"/draft/"+field.Key+tc.suffix, tc.body, htmx)
					if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "data-validation=") ||
						strings.Contains(w.Body.String(), "<input") {
						t.Fatalf("malformed draft status %d, fragment %s", w.Code, w.Body.String())
					}
				})
			}
		}
	}
	// Unknown keys are rejected before lookup, even when the database is down.
	for _, key := range []string{"email_confirm", "Name", "unknown"} {
		w := customerRequest(t, newMux(t, db, failingStore(t, db)), http.MethodPost, path+"/draft/"+key, "name=Ignored", true)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") ||
			w.Header().Get("HX-Refresh") != "true" {
			t.Fatalf("unknown key %q: status %d, body %s", key, w.Code, w.Body.String())
		}
	}
	if w := customerRequest(t, mux, http.MethodPost, path+"/draft", "name=Ignored", false); w.Code != http.StatusNotFound {
		t.Fatalf("obsolete draft endpoint still accepted: %d", w.Code)
	}
	for _, tc := range []struct {
		name, body string
		failing    bool
		status     int
	}{
		{"malformed", "name=%zz", false, http.StatusBadRequest},
		{"oversized", "name=" + strings.Repeat("a", 4100), false, http.StatusRequestEntityTooLarge},
		{"save failure", "name=Valid+student", true, http.StatusInternalServerError},
		{"validation lookup failure", "name=%3Cscript%3E", true, http.StatusInternalServerError},
		{"parse lookup failure", "name=%zz", true, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := mux
			if tc.failing {
				handler = newMux(t, db, failingStore(t, db))
			}
			w := customerRequest(t, handler, http.MethodPost, path+"/draft/name", tc.body, true)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "data-validation=") {
				t.Fatalf("draft error status %d, want %d; body %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) || len(studentRecords(t, db)) != 0 {
		t.Fatalf("rejected drafts mutated state: before %+v, after %+v", before, after)
	}
}

func TestFinalValidationAndEmailConfirmation(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	saveDetails(t, store, entry.Token, contactDetails("Saved student"))
	mux := newMux(t, db, store)
	path := "/customer/" + entry.Token
	before := stored(t, db)
	cases := []struct {
		name     string
		replace  url.Values
		errors   []string
		mismatch bool
	}{
		{"missing confirmation", url.Values{"email_confirm": {""}}, []string{"email_confirm"}, false},
		{"malformed confirmation", url.Values{"email_confirm": {"Display <private@example.com>"}}, []string{"email_confirm"}, false},
		{"mismatch", url.Values{"email_confirm": {"other@example.com"}}, []string{"email_confirm"}, true},
		{"local part remains case significant", url.Values{"email_confirm": {"ada+private@example.COM"}}, []string{"email_confirm"}, true},
		{"invalid email is not a mismatch", url.Values{"email": {"bad"}, "email_confirm": {"other@example.com"}}, []string{"email"}, false},
		{"collect every error", url.Values{"name": {"Single"}, "email": {"bad"}, "email_confirm": {"bad"},
			"phone": {"123"}, "municipality": {"Atlantis"}}, []string{"name", "email", "email_confirm", "phone", "municipality"}, false},
		{"rejected name is escaped", url.Values{"name": {`<script>alert("private")</script>`}}, []string{"name"}, false},
		{"NUL name", url.Values{"name": {"A\x00"}}, []string{"name"}, false},
		{"invalid UTF-8 name", url.Values{"name": {"\xff"}}, []string{"name"}, false},
	}
	for _, tc := range cases {
		for _, htmx := range []bool{false, true} {
			t.Run(tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
				form := contactForm("  María   Pérez  ")
				form.Set("email", " Ada+private@EXAMPLE.com ")
				form.Set("email_confirm", "Ada+private@example.com")
				form.Set("phone", " (202) 555-0123 ")
				form.Set("municipality", "Bayamón")
				for key, values := range tc.replace {
					form[key] = values
				}
				w := customerRequest(t, mux, http.MethodPost, path, form.Encode(), htmx)
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
				}
				body := w.Body.String()
				requireFormError(t, body, path, !htmx)
				requireAttempted(t, body, form)
				for _, key := range tc.errors {
					control := regexp.MustCompile(`<(?:input|select)\b[^>]*\bid="` + key + `"[^>]*>`).FindString(body)
					if !strings.Contains(control, `aria-invalid="true"`) {
						t.Errorf("missing invalid control %q: %s", key, body)
					}
					target := regexp.MustCompile(`(?s)<[^>]*\bid="save-status-` + key + `"[^>]*>(.*?)</(?:p|div)>`).FindString(body)
					if !strings.Contains(target, `role="alert"`) {
						t.Errorf("missing associated error for %q: %s", key, body)
					}
				}
				if strings.Contains(body, `data-confirm-error="mismatch"`) != tc.mismatch {
					t.Errorf("mismatch marker disagrees with parse result: %s", body)
				}
				if strings.Contains(body, `<script>alert`) {
					t.Fatal("rejected attempt rendered executable markup")
				}
				if after := stored(t, db); !reflect.DeepEqual(after, before) || len(studentRecords(t, db)) != 0 {
					t.Fatalf("invalid final mutated persisted state: before %+v, after %+v", before, after)
				}
			})
		}
	}
	// Reload restores saved contact values, but never infers email confirmation.
	reload := customerRequest(t, mux, http.MethodGet, path+"/forms", "", false)
	requireAttempted(t, reload.Body.String(), detailsForm(contactDetails("Saved student")))
	valid := contactForm("  María   Pérez  ")
	valid.Set("email", " Ada+private@EXAMPLE.com ")
	valid.Set("email_confirm", " Ada+private@example.COM ")
	valid.Set("phone", " (202) 555-0123 ")
	valid.Set("municipality", " Bayamón ")
	w := customerRequest(t, mux, http.MethodPost, path, valid.Encode(), false)
	requireAdvance(t, w, path+"/agreement", false, privateFormValues(valid)...)
	want := students.Details{Name: "María Pérez", Email: "Ada+private@example.com", Phone: "+12025550123", Municipality: "Bayamón"}
	link, enrolled := stored(t, db)[0], studentRecords(t, db)
	if link.Details != want || len(enrolled) != 1 || enrolled[0].Details != want {
		t.Fatalf("completion did not copy all normalized fields: link %+v, students %+v", link, enrolled)
	}
}

func TestHostilePersistedDetailsRemainEscaped(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db, nil)
	entry, err := store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	hostile := `<script>alert("persisted")</script>`
	if result, err := store.SaveDraft(t.Context(), entry.Token, "name", hostile); err != nil || result != links.Saved {
		t.Fatalf("persist hostile fixture: %v, %v", result, err)
	}
	w := customerRequest(t, newMux(t, db, store), http.MethodGet, "/customer/"+entry.Token+"/forms", "", false)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), hostile) ||
		!strings.Contains(w.Body.String(), template.HTMLEscapeString(hostile)) {
		t.Fatalf("persisted hostile fixture not safely escaped: status %d, %s", w.Code, w.Body.String())
	}
}

func contactDetails(name string) students.Details {
	return students.Details{Name: name, Email: "Ada+private@example.com", Phone: "+17872345678", Municipality: "San Juan"}
}

func detailsForm(details students.Details) url.Values {
	return url.Values{"name": {details.Name}, "email": {details.Email}, "email_confirm": {""},
		"phone": {details.Phone}, "municipality": {details.Municipality}}
}

func contactForm(name string) url.Values {
	form := detailsForm(contactDetails(name))
	form.Set("email_confirm", form.Get("email"))
	return form
}

func privateFormValues(form url.Values) []string {
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

func saveDetails(t *testing.T, store *links.Store, token string, details students.Details) {
	t.Helper()
	for key, values := range detailsForm(details) {
		if key == "email_confirm" {
			continue
		}
		if result, err := store.SaveDraft(t.Context(), token, key, values[0]); err != nil || result != links.Saved {
			t.Fatalf("save %s draft: result %v, error %v", key, result, err)
		}
	}
}

func customerRequest(t *testing.T, handler http.Handler, method, path, body string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	// Unregistered obsolete routes use ServeMux's own 404 response.
	if !strings.HasSuffix(path, "/draft") {
		for name, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer",
			"Vary": "HX-Request", "Content-Type": "text/html; charset=utf-8"} {
			if got := w.Header().Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	}
	return w
}

func requireAttempted(t *testing.T, body string, form url.Values) {
	t.Helper()
	for _, key := range []string{"name", "email", "email_confirm", "phone"} {
		control := regexp.MustCompile(`<input\b[^>]*\bid="` + key + `"[^>]*>`).FindString(body)
		value := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(control)
		want := html.UnescapeString(template.HTMLEscapeString(form.Get(key)))
		if len(value) == 0 || html.UnescapeString(value[1]) != want {
			t.Errorf("%s lost attempted value %q: %s", key, form.Get(key), control)
		}
	}
	selectHTML := regexp.MustCompile(`(?s)<select\b[^>]*\bid="municipality"[^>]*>.*?</select>`).FindString(body)
	want, _ := students.ParseMunicipality(form.Get("municipality"), false)
	selected := regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*\bselected\b[^>]*>`).FindStringSubmatch(selectHTML)
	got := ""
	if len(selected) != 0 {
		got = html.UnescapeString(selected[1])
	}
	if len(selected) == 0 || got != want || selectHTML == "" {
		t.Errorf("municipality selection = %q, want %q: %s", got, want, selectHTML)
	}
	if want == "" && form.Get("municipality") != "" {
		forged := html.UnescapeString(template.HTMLEscapeString(form.Get("municipality")))
		for _, option := range regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*>`).FindAllStringSubmatch(selectHTML, -1) {
			if html.UnescapeString(option[1]) == forged {
				t.Errorf("forged municipality became an option: %s", selectHTML)
			}
		}
	}
}

// httpTimeout bounds each HTTP exchange, so a stuck handler fails its test.
const httpTimeout = 10 * time.Second

func newMux(t *testing.T, db *gorm.DB, store *links.Store) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store, settings.New(db)); err != nil {
		t.Fatal(err)
	}
	return logging.Requests(logging.New(io.Discard), mux)
}

// serve runs the customer routes behind a real HTTP server.
func serve(t *testing.T, db *gorm.DB, store *links.Store) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newMux(t, db, store))
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
	return links.New(tx, nil)
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
	all, err := students.New(db, nil).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func requireFormError(t *testing.T, body, path string, fullPage bool) {
	t.Helper()
	if strings.Count(body, "<form ") != 1 || !strings.Contains(body, `action="`+path+`"`) ||
		!strings.Contains(body, `type="submit"`) {
		t.Fatalf("error response lost the retryable student form: %s", body)
	}
	for _, key := range []string{"name", "email", "email_confirm", "phone", "municipality"} {
		if !strings.Contains(body, `name="`+key+`"`) {
			t.Errorf("error response lost %s control: %s", key, body)
		}
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

func requireAdvance(t *testing.T, w *httptest.ResponseRecorder, target string, htmx bool, private ...string) {
	t.Helper()
	want, header := http.StatusSeeOther, "Location"
	if htmx {
		want, header = http.StatusOK, "HX-Redirect"
	}
	if w.Code != want || w.Header().Get(header) != target {
		t.Fatalf("advance status = %d, %s = %q, want %d to %s", w.Code, header, w.Header().Get(header), want, target)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("advance exposed a response body: %s", w.Body.String())
	}
	requireNoPrivate(t, w.Header().Get(header), private...)
}

func requireNoPrivate(t *testing.T, body string, private ...string) {
	t.Helper()
	for _, hidden := range private {
		if hidden != "" && strings.Contains(body, hidden) {
			t.Fatalf("response exposed %q: %s", hidden, body)
		}
	}
}

// headKeeps sends HEAD for path, plain and from HTMX, expecting want each
// time, and fails if any stored link changed.
func headKeeps(t *testing.T, db *gorm.DB, server *httptest.Server, path string, want int) {
	t.Helper()
	before := stored(t, db)
	studentsBefore := studentRecords(t, db)
	for _, htmx := range []bool{false, true} {
		if got := head(t, server, path, htmx); got != want {
			t.Fatalf("HEAD %s (htmx=%v): status = %d, want %d", path, htmx, got, want)
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("HEAD %s changed stored links from %+v to %+v", path, before, after)
	}
	if after := studentRecords(t, db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatalf("HEAD %s changed students from %+v to %+v", path, studentsBefore, after)
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
