package auth

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/alexedwards/scs/v2"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

//go:embed templates/login.html
var templates embed.FS

const (
	usernameKey        = "username"
	versionKey         = "auth_version"
	loginInterval      = 12 * time.Second
	invalidCredentials = "Invalid username or password."
)

type loginPage struct {
	Username string
	Error    string
}

type Manager struct {
	db        *gorm.DB
	sessions  *scs.SessionManager
	page      *template.Template
	dummyHash string
	// ponytail: global login limit; use per-account limits if staff concurrency grows
	limiter *rate.Limiter
}

// New expects sessions to wrap every request through LoadAndSave.
func New(db *gorm.DB, sessions *scs.SessionManager) (*Manager, error) {
	page, err := template.ParseFS(templates, "templates/login.html")
	if err != nil {
		return nil, err
	}
	// Unknown usernames are verified against this hash so they cost the same as wrong passwords.
	dummyHash, err := argon2id.CreateHash(rand.Text(), argon2id.DefaultParams)
	if err != nil {
		return nil, err
	}
	return &Manager{
		db:        db,
		sessions:  sessions,
		page:      page,
		dummyHash: dummyHash,
		limiter:   rate.NewLimiter(rate.Every(loginInterval), 2),
	}, nil
}

func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /management/login", m.login)
}

// Require serves next only for a session whose account still exists with the
// auth version recorded at sign-in.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := m.authenticated(r.Context())
		if err != nil {
			log.Printf("check management session: %v", err)
			http.Error(w, "Could not check your sign-in. Please try again.", http.StatusInternalServerError)
			return
		}
		switch {
		case ok:
			next.ServeHTTP(w, r)
		case r.Header.Get("HX-Request") == "true":
			// A swapped-in sign-in form would replace the link list, so send htmx to the full page.
			w.Header().Set("HX-Redirect", "/management/")
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/management/":
			// 200 keeps the Railway health check on /management/ passing while signed out.
			m.render(w, http.StatusOK, loginPage{})
		default:
			http.Redirect(w, r, "/management/", http.StatusSeeOther)
		}
	})
}

func (m *Manager) authenticated(ctx context.Context) (bool, error) {
	username := m.sessions.GetString(ctx, usernameKey)
	if username == "" {
		return false, nil
	}
	user, err := findUser(ctx, m.db, username)
	if err != nil {
		return false, err
	}
	if user != nil && user.AuthVersion == m.sessions.GetString(ctx, versionKey) {
		return true, nil
	}
	return false, m.sessions.Destroy(ctx)
}

func (m *Manager) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		m.render(w, status, loginPage{Error: "Could not read your sign-in. Please try again."})
		return
	}
	page := loginPage{Username: r.PostForm.Get("username")}
	if !m.limiter.Allow() {
		w.Header().Set("Retry-After", "12")
		page.Error = "Too many sign-in attempts. Please wait a few seconds and try again."
		m.render(w, http.StatusTooManyRequests, page)
		return
	}

	ctx := r.Context()
	user, err := m.verify(ctx, page.Username, r.PostForm.Get("password"))
	if err == nil && user != nil {
		// A fresh token prevents session fixation.
		err = m.sessions.RenewToken(ctx)
	}
	if err != nil {
		log.Printf("sign in: %v", err)
		page.Error = "Could not sign in. Please try again."
		m.render(w, http.StatusInternalServerError, page)
		return
	}
	if user == nil {
		page.Error = invalidCredentials
		m.render(w, http.StatusUnauthorized, page)
		return
	}
	m.sessions.Put(ctx, usernameKey, user.Username)
	m.sessions.Put(ctx, versionKey, user.AuthVersion)
	http.Redirect(w, r, "/management/", http.StatusSeeOther)
}

// verify returns nil without an error for unknown usernames and wrong passwords.
func (m *Manager) verify(ctx context.Context, submittedUsername, password string) (*User, error) {
	var user *User
	if username, ok := normalizeUsername(submittedUsername); ok {
		var err error
		if user, err = findUser(ctx, m.db, username); err != nil {
			return nil, err
		}
	}
	hash := m.dummyHash
	if user != nil {
		hash = user.PasswordHash
	}
	match, err := argon2id.ComparePasswordAndHash(password, hash)
	if err != nil || !match {
		return nil, err
	}
	return user, nil
}

func (m *Manager) render(w http.ResponseWriter, status int, page loginPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := m.page.Execute(w, page); err != nil {
		log.Printf("render sign-in page: %v", err)
	}
}
