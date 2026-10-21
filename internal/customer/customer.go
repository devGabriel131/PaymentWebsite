package customer

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"time"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/students"
)

//go:embed templates/*.html
var templates embed.FS

type pageData struct {
	Path      string
	Name      string
	Error     string
	Submitted bool
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store) error {
	page, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}

	// handle serves the page, its final submit, and (when draft) blur autosave,
	// which responds only with save status so it never redraws fields in use.
	// Completion replaces the form with confirmation. GET records a visit;
	// HEAD and POST lookup without mutating the link before any save.
	handle := func(draft bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Vary", "HX-Request")
			token := r.PathValue("token")
			data := pageData{Path: "/customer/" + url.PathEscape(token)}
			show := func(status int) {
				name := "index.html"
				switch {
				case status == http.StatusNotFound:
					name = "not-found.html"
					if r.Header.Get("HX-Request") == "true" {
						w.Header().Set("HX-Refresh", "true")
					}
				case status == http.StatusConflict: // Stale draft: reload into confirmation.
					if r.Header.Get("HX-Request") == "true" {
						w.Header().Set("HX-Refresh", "true")
					}
				case draft:
					name = "save-status"
				case r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true":
					name = "customer-response"
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(status)
				if r.Method == http.MethodHead {
					return
				}
				if err := page.ExecuteTemplate(w, name, data); err != nil {
					log.Printf("render customer page: %v", err)
				}
			}
			showCompletion := func() {
				data.Name, data.Submitted = "", true
				status := http.StatusOK
				if draft {
					status = http.StatusConflict
				}
				show(status)
			}

			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			lookup := store.Lookup
			if r.Method == http.MethodGet {
				lookup = store.Open
			}
			link, err := lookup(ctx, token)
			if err != nil {
				log.Printf("look up customer link: %v", err)
				data.Error = "Could not open this link. Please try again."
				show(http.StatusInternalServerError)
				return
			}
			if link == nil {
				show(http.StatusNotFound)
				return
			}
			data.Submitted = link.SubmittedAt != nil
			if !data.Submitted {
				data.Name = link.Name
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				show(http.StatusOK)
				return
			}
			if data.Submitted {
				showCompletion()
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			if err := r.ParseForm(); err != nil {
				status := http.StatusBadRequest
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				data.Error = "Could not read your name. Please try again."
				show(status)
				return
			}
			data.Name = r.PostForm.Get("name")
			name, err := students.ParseName(data.Name, !draft)
			if err != nil {
				data.Error = err.Error()
				show(http.StatusUnprocessableEntity)
				return
			}

			save := store.Submit
			if draft {
				save = store.SaveDraft
			}
			saved, err := save(ctx, token, name)
			if err != nil {
				log.Printf("save customer name: %v", err)
				data.Error = "Could not save your name. Please try again."
				show(http.StatusInternalServerError)
				return
			}
			if !saved {
				// Since lookup, another tab submitted, staff canceled, or the link expired.
				link, err = store.Lookup(ctx, token)
				switch {
				case err != nil:
					log.Printf("look up customer link: %v", err)
					data.Error = "Could not save your name. Please try again."
					show(http.StatusInternalServerError)
				case link != nil && link.SubmittedAt != nil:
					showCompletion()
				default:
					show(http.StatusNotFound)
				}
				return
			}
			if draft {
				show(http.StatusOK)
			} else {
				showCompletion()
			}
		}
	}

	mux.HandleFunc("GET /customer/{token}", handle(false))
	mux.HandleFunc("POST /customer/{token}", handle(false))
	mux.HandleFunc("POST /customer/{token}/draft", handle(true))
	return nil
}
