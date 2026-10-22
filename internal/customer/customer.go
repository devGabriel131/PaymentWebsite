package customer

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/payments"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
)

//go:embed templates/*.html
var templates embed.FS

type pageData struct {
	students.Details
	Path              string
	View              string
	Error             string
	EmailConfirm      string
	EmailMismatch     bool
	FieldErrors       map[string]string
	DraftField        string
	ProgramStartDate  string
	ProgramStartLabel string
	TermsAcceptedAt   *time.Time
	PaymentPlan       string
	SelectedPlan      string
	Plans             []planView
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store, configuration *settings.Store) error {
	page, err := template.New("index.html").Funcs(template.FuncMap{
		"municipalities": students.MunicipalityOptions,
		"trim":           strings.TrimSpace,
		"money":          payments.Money,
		"spanishDate":    payments.SpanishDate,
	}).ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}

	// handle serves the welcome, form, final submit, and blur autosave.
	// Autosave responds only with status so it never redraws fields in use.
	// Submission advances to the agreement. GET records a visit;
	// HEAD only checks usability; POST writes without recording a visit.
	handle := func(view string) http.HandlerFunc {
		draft := view == "save-status"
		return func(w http.ResponseWriter, r *http.Request) {
			customerHeaders(w)
			token := r.PathValue("token")
			data := pageData{Path: "/customer/" + url.PathEscape(token), View: view}
			show := func(status int) {
				name := "index.html"
				switch {
				case status == http.StatusNotFound:
					name = "not-found.html"
					if r.Header.Get("HX-Request") == "true" {
						w.Header().Set("HX-Refresh", "true")
					}
				case draft:
					name = "save-status"
				case r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true":
					name = "customer-response"
				}
				renderPage(w, r, page, name, data, status)
			}
			advance := func() {
				if draft {
					if r.Header.Get("HX-Request") == "true" {
						w.Header().Set("HX-Refresh", "true")
					}
					w.WriteHeader(http.StatusConflict)
					return
				}
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				redirectCustomer(w, r, data.Path+"/agreement", true)
			}

			var draftField students.Field
			if draft {
				var known bool
				data.DraftField = r.PathValue("field")
				draftField, known = students.FindField(data.DraftField)
				if !known {
					show(http.StatusNotFound)
					return
				}
			}

			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			status := http.StatusOK
			if r.Method == http.MethodPost {
				status = parseCustomerForm(w, r)
				if status != http.StatusOK {
					data.Error = "No pudimos leer tu información. Intenta de nuevo."
				} else if draft && len(r.PostForm[draftField.Key]) != 1 {
					status = http.StatusBadRequest
					data.Error = "No pudimos leer tu información. Intenta de nuevo."
				} else {
					data.Details = students.Details{
						Name: r.PostForm.Get("name"), Email: r.PostForm.Get("email"),
						Phone: r.PostForm.Get("phone"), Municipality: r.PostForm.Get("municipality"),
					}
					data.EmailConfirm = r.PostForm.Get("email_confirm")
					data.FieldErrors = make(map[string]string)
					var parsed students.Details
					var draftValue string
					if draft {
						value, err := draftField.Parse(r.PostForm.Get(draftField.Key), false)
						if err != nil {
							data.FieldErrors[draftField.Key] = fieldError(draftField.Key)
						}
						draftValue = value
					} else {
						for _, field := range students.Fields() {
							value, err := field.Parse(r.PostForm.Get(field.Key), true)
							if err != nil {
								data.FieldErrors[field.Key] = fieldError(field.Key)
							}
							switch field.Key {
							case "name":
								parsed.Name = value
							case "email":
								parsed.Email = value
							case "phone":
								parsed.Phone = value
							case "municipality":
								parsed.Municipality = value
							}
						}
						confirmation, err := students.ParseEmail(data.EmailConfirm, true)
						if err != nil {
							data.FieldErrors["email_confirm"] = fieldError("email")
						} else if data.FieldErrors["email"] == "" && confirmation != parsed.Email {
							data.EmailMismatch = true
							data.FieldErrors["email_confirm"] = "Los correos electrónicos no coinciden."
						}
					}
					if len(data.FieldErrors) != 0 {
						status = http.StatusUnprocessableEntity
					} else {
						var result links.SaveResult
						var err error
						if draft {
							result, err = store.SaveDraft(ctx, token, draftField.Key, draftValue)
						} else {
							result, err = store.Submit(ctx, token, parsed)
						}
						switch {
						case err != nil:
							logging.RecordError(r, "save", err)
							data.Error = "No pudimos guardar tu información. Intenta de nuevo."
							show(http.StatusInternalServerError)
						case result == links.AlreadySubmitted:
							advance()
						case result == links.Saved:
							if draft {
								show(http.StatusOK)
							} else {
								advance()
							}
						default:
							show(http.StatusNotFound)
						}
						return
					}
				}
			}

			// Invalid input still observes link usability and completion first.
			lookup := store.Lookup
			if r.Method == http.MethodGet {
				lookup = store.Open
			}
			link, err := lookup(ctx, token)
			if err != nil {
				logging.RecordError(r, "lookup", err)
				data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
				show(http.StatusInternalServerError)
				return
			}
			if link == nil {
				show(http.StatusNotFound)
				return
			}
			if link.SubmittedAt != nil {
				advance()
				return
			}
			if status != http.StatusUnprocessableEntity {
				data.Details = link.Details
			}
			show(status)
		}
	}

	mux.Handle("GET /customer/{token}", logging.Operation("link_read", handle("welcome")))
	mux.Handle("GET /customer/{token}/forms", logging.Operation("link_read", handle("form")))
	mux.Handle("POST /customer/{token}", logging.Operation("final_submit", handle("form")))
	mux.Handle("POST /customer/{token}/draft/{field}", logging.Operation("draft_save", handle("save-status")))
	registerAgreementRoutes(mux, store, configuration, page)
	return nil
}

func fieldError(key string) string {
	switch key {
	case "name":
		return "Escribe tu nombre y apellidos usando solo letras y espacios."
	case "email":
		return "Escribe un correo electrónico válido."
	case "phone":
		return "Escribe un número válido de Puerto Rico o Estados Unidos, con código de área."
	case "municipality":
		return "Selecciona un municipio o “Fuera de Puerto Rico”."
	}
	return ""
}
