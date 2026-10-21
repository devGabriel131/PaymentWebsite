//go:build integration

package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"

	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

var htmx = http.Header{"Hx-Request": {"true"}}

// later keeps directly created test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

// unknownID is a well-formed management ID that names no link.
var unknownID = strings.Repeat("00", sha256.Size)

type app struct {
	url   string
	db    *gorm.DB
	store *links.Store
}

// newApp wires routes like main.go: auth owns session middleware and the root
// mux retains outer origin protection.
func newApp(t *testing.T) *app {
	t.Helper()
	db := testdb.Open(t, &auth.User{}, &auth.Session{}, &links.Link{}, &students.Student{}, &settings.Settings{})
	store := links.New(db)
	manager, err := auth.New(db, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	child := http.NewServeMux()
	if err := RegisterRoutes(child, store, students.New(db), settings.New(db), auth.Staff); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	manager.RegisterRoutes(root)
	root.Handle("/management/", manager.Require(child))
	server := httptest.NewServer(http.NewCrossOriginProtection().Handler(root))
	t.Cleanup(server.Close)
	return &app{url: server.URL, db: db, store: store}
}

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// signIn stores an account as the CLI does, then signs in through the real form.
func (a *app) signIn(t *testing.T, username string) *http.Client {
	t.Helper()
	password := rand.Text()
	hash, err := argon2id.CreateHash(password, &argon2id.Params{Memory: 16, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	user := auth.User{Username: username, PasswordHash: hash, AuthVersion: rand.Text()}
	if err := a.db.WithContext(t.Context()).Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	browser := newBrowser(t)
	form := url.Values{"username": {username}, "password": {password}}
	if res, body := a.send(t, browser, http.MethodPost, "/management/login", form, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in %s: status = %d: %s", username, res.StatusCode, body)
	}
	return browser
}

func (a *app) send(t *testing.T, browser *http.Client, method, path string, form url.Values, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, a.url+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	maps.Copy(req.Header, header)
	res, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

func (a *app) list(t *testing.T) []links.Entry {
	t.Helper()
	entries, err := a.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// requireRegistry compares what List can return: never a token, and creation
// times differ in precision from Create's.
func (a *app) requireRegistry(t *testing.T, want ...links.Entry) {
	t.Helper()
	listed := func(entries []links.Entry) []links.Entry {
		out := make([]links.Entry, len(entries))
		for i, e := range entries {
			out[i] = links.Entry{ID: e.ID, Name: e.Name, Status: e.Status, CreatedBy: e.CreatedBy, CanceledBy: e.CanceledBy}
		}
		return out
	}
	if got := a.list(t); !slices.Equal(listed(got), listed(want)) {
		t.Fatalf("registry = %+v, want %+v", got, want)
	}
}

func writePaths(id string) []string {
	return []string{"/management/links", "/management/links/" + id + "/cancel", "/management/links/cancel", "/management/settings", "/management/students/1/fields/name"}
}

// createdToken returns the one customer URL token a creating response shows.
func createdToken(t *testing.T, body string) string {
	t.Helper()
	if strings.Count(body, `href="/customer/`) != 1 {
		t.Fatalf("creating response must show exactly one customer URL: %s", body)
	}
	_, rest, _ := strings.Cut(body, `href="/customer/`)
	token, _, _ := strings.Cut(rest, `"`)
	return token
}

// requirePage checks a full page lists exactly want, in order.
func requirePage(t *testing.T, res *http.Response, body string, want []links.Entry) {
	t.Helper()
	start, end := strings.Index(body, `<ul id="generated-links"`), strings.Index(body, "</ul>")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") || start < 0 || end < start {
		t.Fatalf("status = %d, want full management page: %s", res.StatusCode, body)
	}
	requireRows(t, body[start:end], want)
}

// requireFragment checks an HTMX response is not a page and has exactly
// want's rows, in order.
func requireFragment(t *testing.T, res *http.Response, body string, want []links.Entry) {
	t.Helper()
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `<p id="link-status" hx-swap-oob="innerHTML">`) {
		t.Fatalf("status = %d, want management fragment: %s", res.StatusCode, body)
	}
	requireRows(t, body, want)
}

// requireRows checks status, saved names, audit history, one-time URLs, and
// cancellation availability for exactly the expected rows.
func requireRows(t *testing.T, html string, want []links.Entry) {
	t.Helper()
	rows := strings.Split(html, `<li class="management-link">`)[1:]
	if len(rows) != len(want) {
		t.Fatalf("rendered %d links, want %d: %s", len(rows), len(want), html)
	}
	for i, entry := range want {
		row := rows[i]
		ok := strings.Contains(row, string(entry.Status)) && strings.Contains(row, entry.Name)
		cancelable := strings.Contains(row, `action="/management/links/`+entry.ID+`/cancel"`)
		ok = ok && cancelable == entry.Cancelable()
		if cancelable {
			ok = ok && strings.Contains(row, `hx-target="closest li"`) && strings.Contains(row, `hx-swap="outerHTML"`)
		}
		ok = ok && strings.Contains(row, "</time> by "+entry.CreatedBy) == (entry.CreatedBy != "")
		ok = ok && strings.Contains(row, "canceled by "+entry.CanceledBy) == (entry.CanceledBy != "")
		if entry.SubmittedAt != nil {
			ok = ok && strings.Contains(row, `datetime="`+entry.SubmittedAt.UTC().Format(time.RFC3339)+`"`) &&
				strings.Contains(row, `datetime="`+entry.ExpiresAt.UTC().Format(time.RFC3339)+`"`)
		}
		if entry.Token == "" {
			ok = ok && !strings.Contains(row, "<a ") && !strings.Contains(row, "data-copy-link")
		} else {
			ok = ok && strings.Contains(row, `href="/customer/`+entry.Token+`"`) && strings.Contains(row, "data-copy-link")
		}
		if !ok {
			t.Fatalf("row %d does not render %+v: %s", i, entry, row)
		}
	}
}

func TestUnauthorizedRequestsCannotUseManagement(t *testing.T) {
	a := newApp(t)
	existing, err := a.store.Create(t.Context(), "seed", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), existing.Token, "Private student"); err != nil || saved != links.Saved {
		t.Fatalf("submit private student: saved=%v, err=%v", saved, err)
	}
	existing.Name, existing.Status = "Private student", links.Finished
	expired := a.signIn(t, "staff")
	// Expiry is enforced from the session row, so moving it into the past needs no sleep.
	if err := a.db.WithContext(t.Context()).Session(&gorm.Session{AllowGlobalUpdate: true}).
		Model(&auth.Session{}).Update("expiry", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	browsers := []struct {
		name    string
		browser *http.Client
	}{
		{"signed out", newBrowser(t)},
		{"expired session", expired},
	}
	modes := []struct {
		name, redirectHeader string
		header               http.Header
		status               int
	}{
		{"ordinary", "Location", nil, http.StatusSeeOther},
		{"htmx", "HX-Redirect", htmx, http.StatusUnauthorized},
	}
	for _, b := range browsers {
		t.Run(b.name+"/page", func(t *testing.T) {
			res, body := a.send(t, b.browser, http.MethodGet, "/management/", nil, nil)
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `action="/management/login"`) {
				t.Fatalf("status = %d, want sign-in form: %s", res.StatusCode, body)
			}
			if strings.Contains(body, existing.ID) || strings.Contains(body, existing.Token) {
				t.Fatalf("sign-in page leaked link data: %s", body)
			}
		})
		for _, path := range []string{
			"/management/trash",
			"/management/settings",
			"/management/students",
			"/management/students/1",
			"/management/students/1/fields/name?edit=1",
			"/management/students/1/fields/name",
		} {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+"/read"+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodGet, path, nil, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" ||
						strings.Contains(body, "<form") || strings.Contains(body, existing.ID) ||
						strings.Contains(body, existing.Token) || strings.Contains(body, existing.Name) || strings.Contains(body, "student-details") ||
						strings.Contains(body, "student-table") {
						t.Fatalf("unauthorized management read: status=%d, %s=%q, body=%s",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), body)
					}
				})
			}
		}
		for _, path := range writePaths(existing.ID) {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodPost, path, url.Values{"value": {"Unauthorized name"}}, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" {
						t.Fatalf("status = %d, %s = %q; want %d to /management/",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), mode.status)
					}
					// htmx would swap a returned sign-in form into the link list.
					if strings.Contains(body, "<form") || strings.Contains(body, existing.Token) || strings.Contains(body, existing.Name) {
						t.Fatalf("unauthorized response body = %s", body)
					}
					a.requireRegistry(t, existing)
					student, err := students.New(a.db).Get(t.Context(), 1)
					if err != nil || student == nil || student.Name != existing.Name {
						t.Fatalf("unauthorized write changed student: student=%+v, err=%v", student, err)
					}
				})
			}
		}
	}
}

func TestCrossOriginWritesAreRejected(t *testing.T) {
	a := newApp(t)
	existing, err := a.store.Create(t.Context(), "seed", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), existing.Token, "Original student"); err != nil || saved != links.Saved {
		t.Fatalf("submit student: saved=%v, err=%v", saved, err)
	}
	existing.Name, existing.Status = "Original student", links.Finished
	staff := a.signIn(t, "staff")

	origins := []struct {
		name   string
		header http.Header
	}{
		{"fetch metadata", http.Header{"Sec-Fetch-Site": {"cross-site"}}},
		{"origin", http.Header{"Origin": {"https://attacker.example"}}},
	}
	for _, origin := range origins {
		for _, path := range writePaths(existing.ID) {
			t.Run(origin.name+path, func(t *testing.T) {
				if res, body := a.send(t, staff, http.MethodPost, path, url.Values{"value": {"Cross-origin name"}}, origin.header); res.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403: %s", res.StatusCode, body)
				}
				a.requireRegistry(t, existing)
				student, err := students.New(a.db).Get(t.Context(), 1)
				if err != nil || student == nil || student.Name != "Original student" {
					t.Fatalf("cross-origin request changed student: student=%+v, err=%v", student, err)
				}
			})
		}
	}

	// The same session succeeds without cross-origin headers, so origin alone caused each 403.
	if res, body := a.send(t, staff, http.MethodPost, "/management/links", nil, htmx); res.StatusCode != http.StatusOK {
		t.Fatalf("same-origin create: status = %d: %s", res.StatusCode, body)
	}
	if entries := a.list(t); len(entries) != 2 {
		t.Fatalf("same-origin create left %d links, want 2", len(entries))
	}
}

func TestRegistrySeparatesTrash(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	past := time.Now().Add(-time.Minute)
	var main, trash, all []links.Entry
	for _, tc := range []struct {
		name   string
		row    links.Link
		status links.Status
		trash  bool
	}{
		{"not opened", links.Link{ExpiresAt: later}, links.NotOpened, false},
		{"opened", links.Link{ExpiresAt: later, OpenedAt: &past}, links.Opened, false},
		{"in progress", links.Link{ExpiresAt: later, Name: "Draft"}, links.InProgress, false},
		{"completed", links.Link{ExpiresAt: past.Add(7 * 24 * time.Hour), Name: "Completed student", SubmittedAt: &past}, links.Finished, false},
		{"expired draft", links.Link{ExpiresAt: past, Name: "Expired draft"}, links.Expired, true},
		{"canceled draft", links.Link{ExpiresAt: later, CanceledAt: &past, CanceledBy: "staff"}, links.Canceled, true},
	} {
		digest := sha256.Sum256([]byte(tc.name))
		tc.row.TokenHash, tc.row.CreatedBy = digest[:], "staff"
		if err := a.db.WithContext(t.Context()).Create(&tc.row).Error; err != nil {
			t.Fatal(err)
		}
		if tc.row.SubmittedAt != nil {
			student := students.Student{SourceLinkHash: digest[:], Name: tc.row.Name, CreatedAt: *tc.row.SubmittedAt}
			if err := a.db.WithContext(t.Context()).Create(&student).Error; err != nil {
				t.Fatal(err)
			}
		}
		entry := links.Entry{
			ID: fmt.Sprintf("%x", digest), Name: tc.row.Name, Status: tc.status,
			ExpiresAt: tc.row.ExpiresAt, SubmittedAt: tc.row.SubmittedAt, CreatedBy: tc.row.CreatedBy, CanceledBy: tc.row.CanceledBy,
		}
		all = append(all, entry)
		if tc.trash {
			trash = append(trash, entry)
		} else {
			main = append(main, entry)
		}
	}
	for _, tc := range []struct {
		name, path string
		want       []links.Entry
	}{
		{"active and completed", "/management/", main},
		{"canceled and expired", "/management/trash", trash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := a.send(t, staff, http.MethodGet, tc.path, nil, nil)
			requirePage(t, res, body, tc.want)
		})
	}
	a.requireRegistry(t, all...)
}

func TestRegistryArchivesCompletedSubmissions(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, fixture := range []struct {
		name                     string
		createdAge, submittedAge time.Duration
	}{
		{"Recent completion", 30 * 24 * time.Hour, 23 * time.Hour},
		{"Archived completion", 26 * time.Hour, 25 * time.Hour},
		{"Expired completion", 10 * 24 * time.Hour, 8 * 24 * time.Hour},
	} {
		created, err := a.store.Create(t.Context(), "staff", later)
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := a.store.Submit(t.Context(), created.Token, fixture.name); err != nil || saved != links.Saved {
			t.Fatalf("submit %s: saved=%v, err=%v", fixture.name, saved, err)
		}
		submitted := now.Add(-fixture.submittedAge)
		if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&links.Link{}).Where("token_hash = decode(?, 'hex')", created.ID).
				Updates(map[string]any{
					"created_at": now.Add(-fixture.createdAge), "submitted_at": submitted,
					"expires_at": submitted.Add(7 * 24 * time.Hour),
				}).Error; err != nil {
				return err
			}
			return tx.Model(&students.Student{}).Where("source_link_hash = decode(?, 'hex')", created.ID).
				Update("created_at", submitted).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	draft, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.SaveDraft(t.Context(), draft.Token, "Old unfinished form"); err != nil || saved != links.Saved {
		t.Fatalf("save old draft: saved=%v, err=%v", saved, err)
	}
	if err := a.db.WithContext(t.Context()).Model(&links.Link{}).
		Where("token_hash = decode(?, 'hex')", draft.ID).Update("created_at", now.Add(-28*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	all := a.list(t)
	byName := make(map[string]links.Entry, len(all))
	for _, entry := range all {
		byName[entry.Name] = entry
	}
	res, body := a.send(t, staff, http.MethodGet, "/management/", nil, nil)
	// Old creation dates do not archive a recent completion or unfinished form.
	requirePage(t, res, body, []links.Entry{byName["Recent completion"], byName["Old unfinished form"]})
	res, body = a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	requirePage(t, res, body, nil)
	res, body = a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "student-table") ||
		strings.Contains(body, "Old unfinished form") {
		t.Fatalf("Students must retain completions, not drafts: status=%d, body=%s", res.StatusCode, body)
	}
	for _, name := range []string{"Recent completion", "Archived completion", "Expired completion"} {
		if !strings.Contains(body, name) {
			t.Errorf("Students omitted %s after archival or access expiry: %s", name, body)
		}
	}

	// A cancel from an old page cannot reinsert an archived finished row.
	for _, name := range []string{"Archived completion", "Expired completion"} {
		res, body = a.send(t, staff, http.MethodPost, "/management/links/"+byName[name].ID+"/cancel", nil, htmx)
		requireFragment(t, res, body, nil)
		if strings.Contains(body, name) || strings.Contains(body, "/customer/") {
			t.Fatalf("stale cancel reinserted archived completion: %s", body)
		}
	}
	a.requireRegistry(t, all...)
	res, body = a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	requirePage(t, res, body, nil)
	retained, err := students.New(a.db).List(t.Context())
	if err != nil || len(retained) != 3 {
		t.Fatalf("stale cancel lost retained students: students=%+v, err=%v", retained, err)
	}
}

func TestStaffShareLinkStatuses(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	worker := a.signIn(t, "worker")
	cancel := func(t *testing.T, browser *http.Client, id string, header http.Header) (*http.Response, string) {
		t.Helper()
		return a.send(t, browser, http.MethodPost, "/management/links/"+id+"/cancel", nil, header)
	}
	var firstToken, secondToken, thirdToken string
	var entries []links.Entry

	if !t.Run("creation full page and one-time tokens", func(t *testing.T) {
		res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmx)
		firstToken = createdToken(t, body)
		created := a.list(t)
		if len(created) != 1 {
			t.Fatalf("HTMX create left %d links, want 1", len(created))
		}
		created[0].Token = firstToken
		requireFragment(t, res, body, created)

		res, body = a.send(t, admin, http.MethodPost, "/management/links", nil, nil)
		secondToken = createdToken(t, body)
		created = a.list(t)
		if len(created) != 2 {
			t.Fatalf("ordinary create left %d links, want 2", len(created))
		}
		created[1].Token = secondToken
		requirePage(t, res, body, created)
	}) {
		return
	}

	if !t.Run("shared customer statuses", func(t *testing.T) {
		if saved, err := a.store.Submit(t.Context(), firstToken, "Ana Pérez"); err != nil || saved != links.Saved {
			t.Fatalf("submit name: saved=%v, err=%v", saved, err)
		}
		if link, err := a.store.Open(t.Context(), secondToken); err != nil || link == nil {
			t.Fatalf("open link: link=%v, err=%v", link, err)
		}
		entries = a.list(t)
		if entries[0].Status != links.Finished || entries[0].Name != "Ana Pérez" || entries[1].Status != links.Opened {
			t.Fatalf("submitted and opened customer statuses: %+v", entries)
		}
		for name, browser := range map[string]*http.Client{"admin": admin, "worker": worker} {
			t.Run(name+" fresh page", func(t *testing.T) {
				res, body := a.send(t, browser, http.MethodGet, "/management/", nil, nil)
				requirePage(t, res, body, entries)
			})
		}
		res, body := cancel(t, worker, entries[0].ID, nil)
		a.requireRegistry(t, entries...)
		requirePage(t, res, body, entries)
	}) {
		return
	}

	if !t.Run("canceled draft retained without replacing other rows", func(t *testing.T) {
		res, body := a.send(t, worker, http.MethodPost, "/management/links", nil, htmx)
		thirdToken = createdToken(t, body)
		created := a.list(t)
		if len(created) != 3 {
			t.Fatalf("worker create left %d links, want 3", len(created))
		}
		created[2].Token = thirdToken
		requireFragment(t, res, body, created[2:])
		if link, err := a.store.Open(t.Context(), thirdToken); err != nil || link == nil {
			t.Fatalf("open worker's link: link=%v, err=%v", link, err)
		}
		if saved, err := a.store.SaveDraft(t.Context(), secondToken, "Retained draft"); err != nil || saved != links.Saved {
			t.Fatalf("save draft: saved=%v, err=%v", saved, err)
		}
		entries = a.list(t)
		if entries[1].Status != links.InProgress || entries[1].Name != "Retained draft" || entries[2].Status != links.Opened {
			t.Fatalf("draft and unrelated customer statuses: %+v", entries)
		}
		entries[1].Status, entries[1].CanceledBy = links.Canceled, "worker"
		res, body = cancel(t, worker, entries[1].ID, htmx)
		requireFragment(t, res, body, nil)
		a.requireRegistry(t, entries...)
		res, body = a.send(t, admin, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:2])
	}) {
		return
	}

	if !t.Run("first canceler audit preserved", func(t *testing.T) {
		res, body := cancel(t, admin, entries[1].ID, htmx)
		requireFragment(t, res, body, nil)
		a.requireRegistry(t, entries...)
	}) {
		return
	}

	if !t.Run("expired HTMX row removed without cancellation", func(t *testing.T) {
		expired, err := a.store.Create(t.Context(), "admin", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		expired.Token, expired.Status = "", links.Expired
		entries = append(entries, expired)
		res, body := cancel(t, worker, expired.ID, htmx)
		requireFragment(t, res, body, nil)
		a.requireRegistry(t, entries...)
	}) {
		return
	}

	for _, tc := range []struct{ name, id string }{
		{"unknown", unknownID},
		{"malformed", "not-issued"},
		{"raw customer token", thirdToken},
	} {
		t.Run(tc.name+" cancel ID", func(t *testing.T) {
			if res, body := cancel(t, admin, tc.id, htmx); res.StatusCode != http.StatusNoContent || body != "" {
				t.Fatalf("HTMX: status = %d, want 204 so nothing is swapped: %s", res.StatusCode, body)
			}
			res, body := cancel(t, admin, tc.id, nil)
			requirePage(t, res, body, []links.Entry{entries[0], entries[2]})
			a.requireRegistry(t, entries...)
		})
	}

	if !t.Run("ordinary cancel all retains completed and earlier audit", func(t *testing.T) {
		entries[2].Status, entries[2].CanceledBy = links.Canceled, "admin"
		res, body := a.send(t, admin, http.MethodPost, "/management/links/cancel", nil, nil)
		requirePage(t, res, body, entries[:1])
		a.requireRegistry(t, entries...)
		res, body = a.send(t, worker, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:])
	}) {
		return
	}

	t.Run("HTMX cancel all retains completed and earlier audit", func(t *testing.T) {
		_, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmx)
		createdToken(t, body)
		created := a.list(t)
		if len(created) != len(entries)+1 {
			t.Fatalf("create before cancel all left %d links, want %d", len(created), len(entries)+1)
		}
		newest := created[len(created)-1]
		newest.Status, newest.CanceledBy = links.Canceled, "worker"
		entries = append(entries, newest)
		res, body := a.send(t, worker, http.MethodPost, "/management/links/cancel", nil, htmx)
		requireFragment(t, res, body, entries[:1])
		a.requireRegistry(t, entries...)
		res, body = a.send(t, worker, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:])
		for i, want := range []struct{ createdBy, canceledBy string }{
			{"admin", ""},
			{"admin", "worker"},
			{"worker", "admin"},
			{"admin", ""},
			{"admin", "worker"},
		} {
			if entries[i].CreatedBy != want.createdBy || entries[i].CanceledBy != want.canceledBy {
				t.Errorf("link %d: created by %q, canceled by %q; want %q, %q",
					i, entries[i].CreatedBy, entries[i].CanceledBy, want.createdBy, want.canceledBy)
			}
		}
	})
}

// Database failures answer 500 with no list, row, or notice that htmx could
// swap in as if the request had worked.
func TestStoreFailuresAreServerErrors(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	failed := db.WithContext(t.Context()).Begin()
	if err := failed.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// A signed-in username lets cancellation reach the database instead of
	// stopping at the audit check.
	staff := func(context.Context) string { return "staff" }
	if err := RegisterRoutes(mux, links.New(failed), students.New(failed), settings.New(failed), staff); err != nil {
		t.Fatal(err)
	}

	cancel := "/management/links/" + unknownID + "/cancel" // Well-formed, so it reaches the database.
	for _, tc := range []struct {
		name, method, path string
		header             http.Header
	}{
		{"list", http.MethodGet, "/management/", nil},
		{"trash", http.MethodGet, "/management/trash", nil},
		{"students", http.MethodGet, "/management/students", nil},
		{"cancel", http.MethodPost, cancel, nil},
		{"htmx cancel", http.MethodPost, cancel, htmx},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			maps.Copy(req.Header, tc.header)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			body := w.Body.String()
			if w.Code != http.StatusInternalServerError || strings.Contains(body, "generated-links") ||
				strings.Contains(body, "management-link") || strings.Contains(body, "student-table") ||
				strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("status = %d, want 500 without a list, row, or notice: %s", w.Code, body)
			}
		})
	}
}

func TestSettingsLifetimeAppliesOnlyToNewLinks(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	create := func() links.Entry {
		t.Helper()
		if res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmx); res.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d: %s", res.StatusCode, body)
		}
		entries := a.list(t)
		return entries[len(entries)-1]
	}
	requireLifetime := func(entry links.Entry, days int) {
		t.Helper()
		want := time.Duration(days) * 24 * time.Hour
		if got := entry.ExpiresAt.Sub(entry.CreatedAt); (got - want).Abs() > time.Second {
			t.Fatalf("link lifetime = %v, want %v", got, want)
		}
	}
	requireSlider := func(body string, days int) {
		t.Helper()
		if !strings.Contains(body, `type="range" min="2" max="14" step="1"`) || !strings.Contains(body, fmt.Sprintf(`value="%d"`, days)) {
			t.Fatalf("settings page must show a 2-14 day slider at %d: %s", days, body)
		}
	}

	sent := create()
	requireLifetime(sent, settings.DefaultLinkLifetimeDays)
	res, body := a.send(t, admin, http.MethodGet, "/management/settings", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings page: status = %d: %s", res.StatusCode, body)
	}
	requireSlider(body, settings.DefaultLinkLifetimeDays)

	res, body = a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {"10"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/management/settings?saved" {
		t.Fatalf("save: status = %d, Location %q: %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	_, body = a.send(t, admin, http.MethodGet, "/management/settings?saved", nil, nil)
	requireSlider(body, 10)
	if !strings.Contains(body, "Saved.") || !strings.Contains(body, "Last changed by admin") {
		t.Fatalf("saved page must confirm and name who changed it: %s", body)
	}

	for _, bad := range []string{"1", "15", "ten", ""} {
		t.Run("rejects "+bad, func(t *testing.T) {
			res, body := a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {bad}}, nil)
			if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status = %d, want 422 with an error: %s", res.StatusCode, body)
			}
			requireSlider(body, 10)
		})
	}

	requireLifetime(create(), 10)
	if already := a.list(t)[0]; already.ID != sent.ID {
		t.Fatalf("registry order changed: %+v", already)
	} else {
		requireLifetime(already, settings.DefaultLinkLifetimeDays) // Moving the slider never changes links already sent.
	}
}
