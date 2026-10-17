package customer

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

func RegisterRoutes(mux *http.ServeMux, store *links.Store) error {
	page, err := template.ParseFS(templates, "templates/index.html")
	if err != nil {
		return err
	}

	mux.HandleFunc("GET /customer/{token}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		exists, err := store.Exists(ctx, r.PathValue("token"))
		if err != nil {
			log.Printf("look up customer link: %v", err)
			http.Error(w, "Could not open this link. Please try again.", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.ExecuteTemplate(w, "index.html", "Hello, world!"); err != nil {
			log.Printf("render customer page: %v", err)
		}
	})
	return nil
}
