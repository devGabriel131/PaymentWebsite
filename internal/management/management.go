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
	Link   string
	Notice string
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store) error {
	page, err := template.ParseFS(templates, "templates/index.html")
	if err != nil {
		return err
	}

	render := func(w http.ResponseWriter, r *http.Request, data pageData) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		name := "index.html"
		if r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true" {
			name = "result"
		}
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render management page: %v", err)
		}
	}

	mux.HandleFunc("GET /management/{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, pageData{})
	})
	mux.HandleFunc("POST /management/links", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		token, err := store.Create(ctx)
		if err != nil {
			log.Printf("generate customer link: %v", err)
			http.Error(w, "Could not generate a link. Please try again.", http.StatusInternalServerError)
			return
		}

		render(w, r, pageData{Link: "/customer/" + token})
	})
	mux.HandleFunc("POST /management/links/cancel", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.CancelAll(ctx); err != nil {
			log.Printf("cancel all customer links: %v", err)
			http.Error(w, "Could not cancel links. Please try again.", http.StatusInternalServerError)
			return
		}

		render(w, r, pageData{Notice: "All links canceled."})
	})
	return nil
}
