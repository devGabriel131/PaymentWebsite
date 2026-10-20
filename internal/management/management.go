package management

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/settings"
)

//go:embed templates/*.html
var templates embed.FS

type pageData struct {
	Links   []links.Entry
	Created links.Entry
	Notice  string
	Staff   string
}

type settingsData struct {
	Current       settings.Settings
	Min, Max      int
	Error, Notice string
	Staff         string
}

// RegisterRoutes serves the registry and its settings; staff returns the
// verified username of the signed-in account, which every write records for
// the audit trail.
func RegisterRoutes(mux *http.ServeMux, store *links.Store, config *settings.Store, staff func(context.Context) string) error {
	page, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}
	execute := func(w http.ResponseWriter, status int, name string, data any) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		w.WriteHeader(status)
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render management page: %v", err)
		}
	}

	// Full pages and cancel fragments render the whole registry so every status
	// is fresh; an HTMX create returns only its new row. A token is never stored,
	// so only the response that created it can show it.
	render := func(ctx context.Context, w http.ResponseWriter, r *http.Request, data pageData) {
		fragment := r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true"
		name := "index.html"
		if fragment {
			name = "result"
		}
		if !fragment || data.Created.ID == "" {
			var err error
			if data.Links, err = store.List(ctx); err != nil {
				log.Printf("list customer links: %v", err)
				http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
				return
			}
			for i := range data.Links {
				if data.Links[i].ID == data.Created.ID {
					data.Links[i] = data.Created
				}
			}
		}
		data.Staff = staff(r.Context())
		execute(w, http.StatusOK, name, data)
	}

	mux.HandleFunc("GET /management/{$}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		render(ctx, w, r, pageData{})
	})
	mux.HandleFunc("POST /management/links", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current, err := config.Current(ctx)
		var created links.Entry
		if err == nil {
			created, err = store.Create(ctx, staff(r.Context()), time.Now().Add(current.LinkLifetime()))
		}
		if err != nil {
			log.Printf("generate customer link: %v", err)
			http.Error(w, "Could not generate a link. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Created: created, Notice: "Link created."})
	})
	mux.HandleFunc("POST /management/links/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.Cancel(ctx, r.PathValue("id"), staff(r.Context())); err != nil {
			log.Printf("cancel customer link: %v", err)
			http.Error(w, "Could not cancel this link. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "Link canceled."})
	})
	mux.HandleFunc("POST /management/links/cancel", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.CancelAll(ctx, staff(r.Context())); err != nil {
			log.Printf("cancel all customer links: %v", err)
			http.Error(w, "Could not cancel links. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "All links canceled."})
	})

	// showSettings renders the settings page with the saved values.
	showSettings := func(w http.ResponseWriter, r *http.Request, status int, data settingsData) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var err error
		if data.Current, err = config.Current(ctx); err != nil {
			log.Printf("load settings: %v", err)
			http.Error(w, "Could not load settings. Please try again.", http.StatusInternalServerError)
			return
		}
		data.Min, data.Max = settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays
		data.Staff = staff(r.Context())
		execute(w, status, "settings.html", data)
	}
	mux.HandleFunc("GET /management/settings", func(w http.ResponseWriter, r *http.Request) {
		var data settingsData
		if r.URL.Query().Has("saved") {
			data.Notice = "Saved."
		}
		showSettings(w, r, http.StatusOK, data)
	})
	mux.HandleFunc("POST /management/settings", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		// Anything that isn't a whole number is as out of range as 1 or 15.
		err := settings.ErrLinkLifetimeRange
		if days, parseErr := strconv.Atoi(r.PostFormValue("lifetime")); parseErr == nil {
			err = config.SetLinkLifetimeDays(ctx, days, staff(r.Context()))
		}
		switch {
		case err == nil:
			// Redirect so a reload doesn't resubmit the form.
			http.Redirect(w, r, "/management/settings?saved", http.StatusSeeOther)
		case errors.Is(err, settings.ErrLinkLifetimeRange):
			showSettings(w, r, http.StatusUnprocessableEntity, settingsData{
				Error: fmt.Sprintf("Choose between %d and %d days.", settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays),
			})
		default:
			log.Printf("save settings: %v", err)
			http.Error(w, "Could not save settings. Please try again.", http.StatusInternalServerError)
		}
	})
	return nil
}
