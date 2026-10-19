package customer

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"paymentwebsite/internal/links"
)

//go:embed templates/*.html
var templates embed.FS

type pageData struct {
	Path  string
	Name  string
	Error string
	Saved bool
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store) error {
	page, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}

	render := func(w http.ResponseWriter, r *http.Request, status int, data pageData) {
		name := "index.html"
		if status == http.StatusNotFound {
			name = "not-found.html"
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Refresh", "true")
			}
		} else if r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true" {
			name = "name-form"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render customer page: %v", err)
		}
	}

	handle := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		link, err := store.Get(ctx, r.PathValue("token"))
		if err != nil {
			log.Printf("look up customer link: %v", err)
			render(w, r, http.StatusInternalServerError, pageData{
				Path:  r.URL.Path,
				Error: "Could not open this link. Please try again.",
			})
			return
		}
		if link == nil {
			render(w, r, http.StatusNotFound, pageData{})
			return
		}

		data := pageData{Path: r.URL.Path, Name: link.Name, Saved: link.Name != ""}
		if r.Method == http.MethodPost {
			data.Saved = false
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			if err := r.ParseForm(); err != nil {
				status := http.StatusBadRequest
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				data.Error = "Could not read your name. Please try again."
				render(w, r, status, data)
				return
			}
			data.Name = strings.TrimSpace(r.PostForm.Get("name"))
			if data.Name == "" {
				data.Error = "Enter your name."
			} else if !utf8.ValidString(data.Name) || strings.ContainsRune(data.Name, 0) {
				data.Error = "Enter a valid name."
			}
			if data.Error != "" {
				render(w, r, http.StatusUnprocessableEntity, data)
				return
			}

			saved, err := store.SaveName(ctx, r.PathValue("token"), data.Name)
			if err != nil {
				log.Printf("save customer name: %v", err)
				data.Error = "Could not save your name. Please try again."
				render(w, r, http.StatusInternalServerError, data)
				return
			}
			if !saved {
				render(w, r, http.StatusNotFound, pageData{})
				return
			}
			data.Saved = true
		}
		render(w, r, http.StatusOK, data)
	}

	mux.HandleFunc("GET /customer/{token}", handle)
	mux.HandleFunc("POST /customer/{token}", handle)
	return nil
}
