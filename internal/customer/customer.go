package customer

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"paymentwebsite/internal/billing"
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
	// Billing is the contract summary; Processing marks a checkout the provider has not finished.
	Billing    *billing.Summary
	Processing bool
	// Return is the token-free outcome shown after Stripe Checkout.
	Return *billing.ReturnStatus
	// ReturnSessionID and ReturnPollsNext rebuild the payment-return poll URL;
	// ReturnStopPoll ends polling after maxReturnPolls requests.
	ReturnSessionID string
	ReturnPollsNext int
	ReturnStopPoll  bool
	// Editing marks the contact form reopened after submit; Funded hides that edit once the first payment is confirmed.
	Editing bool
	Funded  bool
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store, configuration *settings.Store, biller *billing.Service) error {
	page, err := template.New("index.html").Funcs(template.FuncMap{
		"municipalities": students.MunicipalityOptions,
		"trim":           strings.TrimSpace,
		"money":          payments.Money,
		"money64":        payments.Money64,
		"spanishDate":    payments.SpanishDate,
		"billingDate":    func(t time.Time) string { return payments.SpanishDate(t.In(payments.PuertoRico)) },
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
						DateOfBirth: r.PostForm.Get("date_of_birth"),
					}
					data.EmailConfirm = r.PostForm.Get("email_confirm")
					var parsed students.Details
					var draftValue string
					if draft {
						data.FieldErrors = make(map[string]string)
						value, err := draftField.Parse(r.PostForm.Get(draftField.Key), false)
						if err != nil {
							data.FieldErrors[draftField.Key] = fieldError(draftField.Key)
						}
						draftValue = value
					} else {
						parsed, data.FieldErrors, data.EmailMismatch = parseContact(r.PostForm)
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
	registerEditRoutes(mux, store, biller, page)
	registerAgreementRoutes(mux, store, configuration, biller, page)
	registerPaymentRoutes(mux, store, biller, page)
	mux.Handle("GET /payments/return", logging.Operation("link_read", paymentReturn(biller, page)))
	return nil
}

// parseContact validates every contact field and the email confirmation, collecting all errors.
func parseContact(form url.Values) (parsed students.Details, fieldErrors map[string]string, emailMismatch bool) {
	fieldErrors = make(map[string]string)
	for _, field := range students.Fields() {
		value, err := field.Parse(form.Get(field.Key), true)
		if err != nil {
			fieldErrors[field.Key] = fieldError(field.Key)
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
		case "date_of_birth":
			parsed.DateOfBirth = value
		}
	}
	confirmation, err := students.ParseEmail(form.Get("email_confirm"), true)
	if err != nil {
		fieldErrors["email_confirm"] = fieldError("email")
	} else if fieldErrors["email"] == "" && confirmation != parsed.Email {
		emailMismatch = true
		fieldErrors["email_confirm"] = "Los correos electrónicos no coinciden."
	}
	return parsed, fieldErrors, emailMismatch
}

// registerEditRoutes reopens the contact form of a submitted link until the first payment is confirmed.
// The edit has no autosave: one POST updates the link and student together, then returns to the agreement.
// Submission time, expiry and accepted terms never change.
func registerEditRoutes(mux *http.ServeMux, store *links.Store, biller *billing.Service, page *template.Template) {
	handle := func(w http.ResponseWriter, r *http.Request) {
		customerHeaders(w)
		token := r.PathValue("token")
		data := pageData{Path: "/customer/" + url.PathEscape(token), View: "form", Editing: true, FieldErrors: make(map[string]string)}
		show := func(status int) { renderCustomer(w, r, page, data, status) }
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		link, err := store.Lookup(ctx, token)
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
		if link.SubmittedAt == nil {
			redirectCustomer(w, r, data.Path+"/forms", false)
			return
		}
		funded, err := biller.IsFunded(ctx, link.TokenHash)
		if err != nil {
			logging.RecordError(r, "load", err)
			data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
			show(http.StatusInternalServerError)
			return
		}
		if funded {
			redirectCustomer(w, r, data.Path+"/payments", false)
			return
		}

		status := http.StatusOK
		data.Details, data.EmailConfirm = link.Details, link.Email
		if r.Method == http.MethodPost {
			if status = parseCustomerForm(w, r); status != http.StatusOK {
				data.Error = "No pudimos leer tu información. Intenta de nuevo."
			} else {
				data.Details = students.Details{
					Name: r.PostForm.Get("name"), Email: r.PostForm.Get("email"),
					Phone: r.PostForm.Get("phone"), Municipality: r.PostForm.Get("municipality"),
					DateOfBirth: r.PostForm.Get("date_of_birth"),
				}
				data.EmailConfirm = r.PostForm.Get("email_confirm")
				var parsed students.Details
				parsed, data.FieldErrors, data.EmailMismatch = parseContact(r.PostForm)
				if len(data.FieldErrors) != 0 {
					status = http.StatusUnprocessableEntity
				} else if parsed == link.Details {
					// Nothing changed: skip the write, the management refresh and the provider sync.
					redirectCustomer(w, r, data.Path+"/agreement", false)
					return
				} else {
					result, err := store.UpdateContact(ctx, token, parsed)
					switch {
					case err != nil:
						logging.RecordError(r, "save", err)
						data.Error = "No pudimos guardar tu información. Intenta de nuevo."
						status = http.StatusInternalServerError
					case result != links.Saved:
						show(http.StatusNotFound)
						return
					default:
						// The edit is already committed; a failed provider update must not undo it.
						if parsed.Name != link.Name || parsed.Email != link.Email {
							if err := biller.UpdateCustomerContact(ctx, link.TokenHash, parsed.Name, parsed.Email); err != nil {
								logging.RecordError(r, "sync", err)
							}
						}
						redirectCustomer(w, r, data.Path+"/agreement", false)
						return
					}
				}
			}
		}
		show(status)
	}
	mux.Handle("GET /customer/{token}/edit", logging.Operation("link_read", http.HandlerFunc(handle)))
	mux.Handle("POST /customer/{token}/edit", logging.Operation("final_submit", http.HandlerFunc(handle)))
}

func fieldError(key string) string {
	switch key {
	case "name":
		return "Escribe el nombre y los apellidos del estudiante usando solo letras y espacios."
	case "email":
		return "Escribe un correo electrónico válido."
	case "phone":
		return "Escribe un número válido de Puerto Rico o Estados Unidos, con código de área."
	case "municipality":
		return "Selecciona un municipio o “Fuera de Puerto Rico”."
	case "date_of_birth":
		return "Escribe una fecha de nacimiento válida."
	}
	return ""
}
