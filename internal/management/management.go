package management

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"

	"paymentwebsite/internal/links"
)

//go:embed templates/index.html
var templates embed.FS

type pageData struct {
	Links   []links.Entry
	Created links.Entry
	Notice  string
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store) error {
	page, err := template.ParseFS(templates, "templates/index.html")
	if err != nil {
		return err
	}

	// Full pages always show the shared registry; HTMX POSTs return only their fragment.
	render := func(ctx context.Context, w http.ResponseWriter, r *http.Request, data pageData) {
		name := "index.html"
		if r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true" {
			name = "result"
		} else {
			var err error
			if data.Links, err = store.List(ctx); err != nil {
				log.Printf("list customer links: %v", err)
				http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render management page: %v", err)
		}
	}

	mux.HandleFunc("GET /management/{$}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		render(ctx, w, r, pageData{})
	})
	mux.HandleFunc("POST /management/links", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		created, err := store.Create(ctx)
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
		if err := store.Cancel(ctx, r.PathValue("id")); err != nil {
			log.Printf("cancel customer link: %v", err)
			http.Error(w, "Could not cancel this link. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "Link canceled."})
	})
	mux.HandleFunc("POST /management/links/cancel", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.CancelAll(ctx); err != nil {
			log.Printf("cancel all customer links: %v", err)
			http.Error(w, "Could not cancel links. Please try again.", http.StatusInternalServerError)
			return
		}

		render(ctx, w, r, pageData{Notice: "All links canceled."})
	})
	return nil
}
