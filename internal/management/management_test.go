//go:build integration

package management

import (
	"crypto/rand"
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
	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"gorm.io/gorm"

	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/testdb"
)

var htmx = http.Header{"Hx-Request": {"true"}}

// later keeps directly created test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

type app struct {
	url   string
	db    *gorm.DB
	store *links.Store
}

// newApp wires routes like main.go: auth's login route on the root mux and the
// whole management subtree behind Require, inside origin and session middleware.
func newApp(t *testing.T) *app {
	t.Helper()
	db := testdb.Open(t, &auth.User{}, &auth.Session{}, &links.Link{}, &settings.Settings{})
	store := links.New(db)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sessions := scs.New()
	sessions.Store = postgresstore.NewWithCleanupInterval(sqlDB, 0)
	manager, err := auth.New(db, sessions)
	if err != nil {
		t.Fatal(err)
	}
	child := http.NewServeMux()
	if err := RegisterRoutes(child, store, settings.New(db), auth.Staff); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	manager.RegisterRoutes(root)
	root.Handle("/management/", manager.Require(child))
	server := httptest.NewServer(http.NewCrossOriginProtection().Handler(sessions.LoadAndSave(root)))
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
	hash, err := argon2id.CreateHash(password, argon2id.DefaultParams)
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
	return []string{"/management/links", "/management/links/" + id + "/cancel", "/management/links/cancel", "/management/settings"}
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

// requirePage checks a full page lists exactly want, in order, plus notice.
func requirePage(t *testing.T, res *http.Response, body, notice string, want []links.Entry) {
	t.Helper()
	start, end := strings.Index(body, `<ul id="generated-links"`), strings.Index(body, "</ul>")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") || start < 0 || end < start {
		t.Fatalf("status = %d, want full management page: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `role="status">`+notice+"</p>") {
		t.Errorf("page status does not show %q: %s", notice, body)
	}
	requireRows(t, body[start:end], want)
}

// requireRows checks rendered rows match want, in order: status, name, audit
// names, and expiry are shown, only an entry's creating response shows its
// URL, and only usable links offer a cancel control.
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
		if entry.Status != links.Canceled {
			expiry := " · expired"
			if entry.Cancelable() {
				expiry = " · expires"
			}
			ok = ok && strings.Contains(row, expiry)
		}
		ok = ok && strings.Contains(row, "</time> by "+entry.CreatedBy) == (entry.CreatedBy != "")
		ok = ok && strings.Contains(row, "canceled by "+entry.CanceledBy) == (entry.CanceledBy != "")
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
		for _, path := range writePaths(existing.ID) {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodPost, path, nil, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" {
						t.Fatalf("status = %d, %s = %q; want %d to /management/",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), mode.status)
					}
					// htmx would swap a returned sign-in form into the link list.
					if strings.Contains(body, "<form") || strings.Contains(body, existing.Token) {
						t.Fatalf("unauthorized response body = %s", body)
					}
					a.requireRegistry(t, existing)
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
				if res, body := a.send(t, staff, http.MethodPost, path, nil, origin.header); res.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403: %s", res.StatusCode, body)
				}
				a.requireRegistry(t, existing)
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

func TestStaffShareLinkStatuses(t *testing.T) {
	a := newApp(t)
	ctx := t.Context()
	admin := a.signIn(t, "admin")
	worker := a.signIn(t, "worker")

	res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmx)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `hx-swap-oob="innerHTML">Link created.</p>`) {
		t.Fatalf("HTMX create: status = %d, want row fragment: %s", res.StatusCode, body)
	}
	firstToken := createdToken(t, body)
	created := a.list(t)
	created[0].Token = firstToken
	requireRows(t, body, created) // Only the appended row, never the whole list.

	res, body = a.send(t, admin, http.MethodPost, "/management/links", nil, nil)
	secondToken := createdToken(t, body)
	created = a.list(t)
	if len(created) != 2 {
		t.Fatalf("ordinary create left %d links, want 2", len(created))
	}
	created[1].Token = secondToken // The first row's URL is gone after its creating response.
	requirePage(t, res, body, "Link created.", created)

	// Customers progress through their own links.
	if saved, err := a.store.Submit(ctx, firstToken, "Ana Pérez"); err != nil || !saved {
		t.Fatalf("save name: saved=%v, err=%v", saved, err)
	}
	if link, err := a.store.Open(ctx, secondToken); err != nil || link == nil {
		t.Fatalf("open link: link=%v, err=%v", link, err)
	}
	entries := a.list(t)
	if entries[0].Status != links.Finished || entries[1].Status != links.Opened {
		t.Fatalf("statuses = %q, %q; want Finished, Opened", entries[0].Status, entries[1].Status)
	}
	for name, browser := range map[string]*http.Client{"admin": admin, "worker": worker} {
		t.Run(name+" fresh page", func(t *testing.T) {
			res, body := a.send(t, browser, http.MethodGet, "/management/", nil, nil)
			requirePage(t, res, body, "", entries)
			if !strings.Contains(body, "<span>"+name+"</span>") || !strings.Contains(body, `action="/management/logout"`) {
				t.Errorf("header must show %s and a sign-out form: %s", name, body)
			}
		})
	}

	res, body = a.send(t, worker, http.MethodPost, "/management/links/"+entries[0].ID+"/cancel", nil, nil)
	entries = a.list(t)
	if entries[0].Status != links.Canceled || entries[0].Name != "Ana Pérez" || entries[1].Status != links.Opened {
		t.Fatalf("cancel one: registry = %+v", entries)
	}
	requirePage(t, res, body, "Link canceled.", entries)

	// An HTMX cancel refreshes every row, not just the canceled one.
	res, body = a.send(t, admin, http.MethodPost, "/management/links/"+entries[1].ID+"/cancel", nil, htmx)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `hx-swap-oob="innerHTML">Link canceled.</p>`) {
		t.Fatalf("HTMX cancel: status = %d, want registry fragment: %s", res.StatusCode, body)
	}
	entries = a.list(t)
	requireRows(t, body, entries)

	if res, body := a.send(t, worker, http.MethodPost, "/management/links", nil, htmx); res.StatusCode != http.StatusOK {
		t.Fatalf("worker create: status = %d: %s", res.StatusCode, body)
	}
	res, body = a.send(t, worker, http.MethodPost, "/management/links/cancel", nil, nil)
	entries = a.list(t)
	for _, entry := range entries {
		if entry.Status != links.Canceled {
			t.Fatalf("cancel all left %+v", entry)
		}
	}
	if len(entries) != 3 || entries[0].Name != "Ana Pérez" {
		t.Fatalf("cancel all must keep submitted data: %+v", entries)
	}
	requirePage(t, res, body, "All links canceled.", entries)

	// Each action records the session's account, and cancel all keeps earlier cancelers.
	for i, want := range []struct{ createdBy, canceledBy string }{
		{"admin", "worker"},
		{"admin", "admin"},
		{"worker", "worker"},
	} {
		if entries[i].CreatedBy != want.createdBy || entries[i].CanceledBy != want.canceledBy {
			t.Errorf("link %d: created by %q, canceled by %q; want %q, %q",
				i, entries[i].CreatedBy, entries[i].CanceledBy, want.createdBy, want.canceledBy)
		}
	}
}

func TestListFailureIsServerError(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
	failed := db.WithContext(t.Context()).Begin()
	if err := failed.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, links.New(failed), settings.New(failed), auth.Staff); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/management/", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "generated-links") {
		t.Fatalf("status = %d, want 500 without a link list: %s", w.Code, w.Body.String())
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
