package management

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"time"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
)

//go:embed templates/*.html
var templates embed.FS

type pageData struct {
	Links  []links.Entry
	Notice string
	Staff  string
	Trash  bool
}

type settingsData struct {
	Current       settings.Settings
	Min, Max      int
	Error, Notice string
	Staff         string
}

// RegisterRoutes serves customer links, Students, Trash, and settings; staff
// returns the verified username of the signed-in account for the audit trail.
func RegisterRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, config *settings.Store, staff func(context.Context) string) error {
	page, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}
	execute := func(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		w.WriteHeader(status)
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			logging.RecordError(r, "render", err)
		}
	}
	registerStudentRoutes(mux, roster, staff, execute)

	// fragment reports whether r is an HTMX action, answered with only the
	// rows it changed rather than a full page.
	fragment := func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true"
	}

	// Full pages and cancel-all fragments filter the registry by tab. Single-row
	// fragments leave other rows alone so they keep their one-time URLs.
	render := func(ctx context.Context, w http.ResponseWriter, r *http.Request, data pageData, single *links.Entry) {
		now := time.Now()
		visible := func(e links.Entry) bool {
			return e.Trashed() == data.Trash && (data.Trash || !e.Archived(now))
		}
		partial := fragment(r)
		if partial && single != nil {
			if visible(*single) {
				data.Links = []links.Entry{*single}
			}
		} else {
			var err error
			if data.Links, err = store.List(ctx); err != nil {
				logging.RecordError(r, "list", err)
				http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
				return
			}
			data.Links = slices.DeleteFunc(data.Links, func(e links.Entry) bool {
				return !visible(e)
			})
			if single != nil && single.Token != "" {
				for i := range data.Links {
					if data.Links[i].ID == single.ID {
						data.Links[i].Token = single.Token
					}
				}
			}
		}
		name := "result"
		if !partial {
			name = "index.html"
			data.Staff = staff(r.Context())
		}
		execute(w, r, http.StatusOK, name, data)
	}

	mux.Handle("GET /management/{$}", logging.Operation("link_list", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		render(ctx, w, r, pageData{}, nil)
	})))
	mux.Handle("GET /management/trash", logging.Operation("trash_list", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		render(ctx, w, r, pageData{Trash: true}, nil)
	})))
	mux.Handle("POST /management/links", logging.Operation("link_create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current, err := config.Current(ctx)
		var created links.Entry
		if err == nil {
			created, err = store.Create(ctx, staff(r.Context()), time.Now().Add(current.LinkLifetime()))
		}
		if err != nil {
			logging.RecordError(r, "create", err)
			http.Error(w, "Could not generate a link. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "Link created."}, &created)
	})))
	mux.Handle("POST /management/links/{id}/cancel", logging.Operation("link_cancel", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		canceled, err := store.Cancel(ctx, r.PathValue("id"), staff(r.Context()))
		if err != nil {
			logging.RecordError(r, "cancel", err)
			http.Error(w, "Could not cancel this link. Please try again.", http.StatusInternalServerError)
			return
		}

		data := pageData{Notice: "Link canceled."}
		if canceled != nil {
			if canceled.Status != links.Canceled {
				data.Notice = "Link can no longer be canceled."
			}
		} else if fragment(r) {
			// The ID names no link, so there is no row to replace.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		render(ctx, w, r, data, canceled)
	})))
	mux.Handle("POST /management/links/cancel", logging.Operation("link_cancel_all", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.CancelAll(ctx, staff(r.Context())); err != nil {
			logging.RecordError(r, "cancel_all", err)
			http.Error(w, "Could not cancel links. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "All unfinished links canceled."}, nil)
	})))

	// showSettings renders the settings page with the saved values.
	showSettings := func(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, data settingsData) {
		var err error
		if data.Current, err = config.Current(ctx); err != nil {
			logging.RecordError(r, "load", err)
			http.Error(w, "Could not load settings. Please try again.", http.StatusInternalServerError)
			return
		}
		data.Min, data.Max = settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays
		data.Staff = staff(r.Context())
		execute(w, r, status, "settings.html", data)
	}
	mux.Handle("GET /management/settings", logging.Operation("settings_load", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var data settingsData
		if r.URL.Query().Has("saved") {
			data.Notice = "Saved."
		}
		showSettings(ctx, w, r, http.StatusOK, data)
	})))
	mux.Handle("POST /management/settings", logging.Operation("settings_update", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			showSettings(ctx, w, r, http.StatusUnprocessableEntity, settingsData{
				Error: fmt.Sprintf("Choose between %d and %d days.", settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays),
			})
		default:
			logging.RecordError(r, "save", err)
			http.Error(w, "Could not save settings. Please try again.", http.StatusInternalServerError)
		}
	})))
	return nil
}
