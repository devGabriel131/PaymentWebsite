//go:build integration

package management

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"paymentwebsite/internal/testdb"
)

var (
	testKey = bytes.Repeat([]byte{1}, 32)
	htmx    = http.Header{"Hx-Request": {"true"}}
)

type app struct {
	url   string
	db    *gorm.DB
	store *links.Store
}

// newApp wires routes like main.go: auth's login route on the root mux and the
// whole management subtree behind Require, inside origin and session middleware.
func newApp(t *testing.T) *app {
	t.Helper()
	db := testdb.Open(t, &auth.User{}, &auth.Session{}, &links.Link{})
	store, err := links.New(db, testKey)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := RegisterRoutes(child, store); err != nil {
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

func (a *app) requireRegistry(t *testing.T, want ...links.Entry) {
	t.Helper()
	if got := a.list(t); !slices.Equal(got, want) {
		t.Fatalf("registry = %+v, want %+v", got, want)
	}
}

func writePaths(id string) []string {
	return []string{"/management/links", "/management/links/" + id + "/cancel", "/management/links/cancel"}
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

// requireRows checks rendered rows match want, in order, and that legacy rows
// never get a fabricated URL or copy control.
func requireRows(t *testing.T, html string, want []links.Entry) {
	t.Helper()
	rows := strings.Split(html, `<li class="management-link">`)[1:]
	if len(rows) != len(want) {
		t.Fatalf("rendered %d links, want %d: %s", len(rows), len(want), html)
	}
	for i, entry := range want {
		row := rows[i]
		ok := strings.Contains(row, `action="/management/links/`+entry.ID+`/cancel"`) && strings.Contains(row, entry.Name)
		if entry.Token == "" {
			ok = ok && strings.Contains(row, "Original URL unavailable for this existing link.") &&
				!strings.Contains(row, "<a ") && !strings.Contains(row, "data-copy-link")
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
	existing, err := a.store.Create(t.Context())
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
	existing, err := a.store.Create(t.Context())
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

func TestStaffShareLinkList(t *testing.T) {
	a := newApp(t)
	digest := sha256.Sum256([]byte("link issued before encrypted recovery"))
	if err := a.db.WithContext(t.Context()).Create(&links.Link{TokenHash: digest[:], Name: "Legacy customer"}).Error; err != nil {
		t.Fatal(err)
	}
	legacy := links.Entry{ID: hex.EncodeToString(digest[:]), Name: "Legacy customer"}
	admin := a.signIn(t, "admin")
	worker := a.signIn(t, "worker")

	res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmx)
	entries := a.list(t)
	if len(entries) != 2 || entries[0] != legacy {
		t.Fatalf("registry after HTMX create = %+v", entries)
	}
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `hx-swap-oob="innerHTML">Link created.</p>`) {
		t.Fatalf("HTMX create: status = %d, want status fragment: %s", res.StatusCode, body)
	}
	requireRows(t, body, entries[1:]) // Only the appended row, never the whole list.

	res, body = a.send(t, admin, http.MethodPost, "/management/links", nil, nil)
	previous, entries := entries, a.list(t)
	if len(entries) != 3 || !slices.Equal(entries[:2], previous) {
		t.Fatalf("ordinary create did not append: %+v", entries)
	}
	requirePage(t, res, body, "Link created.", entries)

	for name, browser := range map[string]*http.Client{"admin": admin, "worker": worker} {
		t.Run(name+" fresh page", func(t *testing.T) {
			res, body := a.send(t, browser, http.MethodGet, "/management/", nil, nil)
			requirePage(t, res, body, "", entries)
		})
	}

	res, body = a.send(t, worker, http.MethodPost, "/management/links/"+entries[1].ID+"/cancel", nil, nil)
	requirePage(t, res, body, "Link canceled.", []links.Entry{legacy, entries[2]})

	res, body = a.send(t, admin, http.MethodPost, "/management/links/"+legacy.ID+"/cancel", nil, htmx)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<li") ||
		!strings.Contains(body, `hx-swap-oob="innerHTML">Link canceled.</p>`) {
		t.Fatalf("HTMX cancel: status = %d, want status-only fragment: %s", res.StatusCode, body)
	}
	a.requireRegistry(t, entries[2])

	res, body = a.send(t, worker, http.MethodPost, "/management/links/cancel", nil, nil)
	requirePage(t, res, body, "All links canceled.", nil)
	a.requireRegistry(t)
}

func TestListFailureIsServerError(t *testing.T) {
	db := testdb.Open(t, &links.Link{})
	issuer, err := links.New(db, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Create(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Under another key the saved URL fails authentication, so List fails as a whole.
	wrongKey, err := links.New(db, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, wrongKey); err != nil {
		t.Fatal(err)
	}

	requests := []struct{ name, method, path string }{
		{"page", http.MethodGet, "/management/"},
		{"ordinary create", http.MethodPost, "/management/links"},
		{"ordinary cancel", http.MethodPost, "/management/links/" + strings.Repeat("0", 64) + "/cancel"},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "generated-links") {
				t.Fatalf("status = %d, want 500 without a link list: %s", w.Code, w.Body.String())
			}
		})
	}
}
