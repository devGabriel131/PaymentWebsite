package customer

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/payments"
	"paymentwebsite/internal/settings"
)

type planView struct {
	payments.Plan
	Schedule []payments.Installment
}

func customerHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func renderPage(w http.ResponseWriter, r *http.Request, page *template.Template, name string, data pageData, status int) {
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if err := page.ExecuteTemplate(w, name, data); err != nil {
		logging.RecordError(r, "render", err)
	}
}

// Redirects carry no contact data, including the HTMX submission response.
func redirectCustomer(w http.ResponseWriter, r *http.Request, target string, allowHTMX bool) {
	if allowHTMX && r.Method == http.MethodPost && r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}

func parseCustomerForm(w http.ResponseWriter, r *http.Request) int {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusBadRequest
	}
	return http.StatusOK
}

func registerAgreementRoutes(mux *http.ServeMux, store *links.Store, configuration *settings.Store, page *template.Template) {
	handle := func(view string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			customerHeaders(w)
			token := r.PathValue("token")
			data := pageData{Path: "/customer/" + url.PathEscape(token), View: view, FieldErrors: make(map[string]string)}
			show := func(status int) {
				name := "index.html"
				if status == http.StatusNotFound {
					name = "not-found.html"
					if r.Header.Get("HX-Request") == "true" {
						w.Header().Set("HX-Refresh", "true")
					}
				}
				renderPage(w, r, page, name, data, status)
			}
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
			if view == "payments" && link.TermsAcceptedAt == nil {
				redirectCustomer(w, r, data.Path+"/agreement", false)
				return
			}
			if view == "agreement" && r.Method == http.MethodPost && link.TermsAcceptedAt != nil {
				redirectCustomer(w, r, data.Path+"/payments", false)
				return
			}

			status := http.StatusOK
			data.TermsAcceptedAt, data.ProgramStartDate, data.PaymentPlan = link.TermsAcceptedAt, link.ProgramStartDate, link.PaymentPlan
			loadAgreementDate := func() bool {
				current, err := configuration.Current(ctx)
				if err != nil {
					data.ProgramStartDate, data.ProgramStartLabel = "", ""
					logging.RecordError(r, "load", err)
					data.Error = "No pudimos cargar el acuerdo. Intenta de nuevo."
					show(http.StatusInternalServerError)
					return false
				}
				data.ProgramStartDate = current.ProgramStartDate
				return true
			}
			if view == "agreement" && data.TermsAcceptedAt == nil && !loadAgreementDate() {
				return
			}
			if r.Method == http.MethodPost {
				status = parseCustomerForm(w, r)
				if status != http.StatusOK {
					data.Error = "No pudimos leer tu selección. Intenta de nuevo."
				} else if view == "agreement" {
					switch {
					case len(r.PostForm["accept_terms"]) > 1 || len(r.PostForm["program_start_date"]) != 1:
						status = http.StatusBadRequest
						data.Error = "No pudimos leer tu aceptación. Intenta de nuevo."
					case r.PostForm.Get("accept_terms") != "yes":
						status = http.StatusUnprocessableEntity
						data.FieldErrors["accept_terms"] = "Debes aceptar los términos para continuar."
					default:
						accepted, err := store.AcceptTerms(ctx, token, r.PostForm.Get("program_start_date"))
						switch {
						case errors.Is(err, links.ErrProgramStartChanged):
							if !loadAgreementDate() {
								return
							}
							status = http.StatusUnprocessableEntity
							data.Error = "La fecha de inicio cambió. Revisa el acuerdo y acepta los términos de nuevo."
						case err != nil:
							logging.RecordError(r, "save", err)
							data.Error = "No pudimos guardar tu aceptación. Intenta de nuevo."
							status = http.StatusInternalServerError
						case accepted == nil:
							show(http.StatusNotFound)
							return
						default:
							redirectCustomer(w, r, data.Path+"/payments", false)
							return
						}
					}
				} else {
					data.PaymentPlan = r.PostForm.Get("payment_plan")
					_, allowed := payments.Find(data.PaymentPlan)
					switch {
					case len(r.PostForm["payment_plan"]) > 1:
						status = http.StatusBadRequest
						data.Error = "Selecciona una sola opción de pago."
					case len(r.PostForm["payment_plan"]) != 1 || !allowed:
						status = http.StatusUnprocessableEntity
						data.FieldErrors["payment_plan"] = "Selecciona una de las tres opciones de pago."
					default:
						chosen, err := store.ChoosePlan(ctx, token, data.PaymentPlan)
						if err != nil {
							logging.RecordError(r, "save", err)
							data.Error = "No pudimos guardar tu selección. Intenta de nuevo."
							status = http.StatusInternalServerError
						} else if chosen == nil {
							show(http.StatusNotFound)
							return
						} else {
							redirectCustomer(w, r, data.Path+"/payments", false)
							return
						}
					}
				}
			}

			if view == "agreement" {
				date, err := time.Parse(time.DateOnly, data.ProgramStartDate)
				if err != nil || date.Year() == 0 {
					data.ProgramStartDate = ""
					if status != http.StatusInternalServerError {
						data.Error = "El acuerdo no está disponible porque falta confirmar la fecha de inicio. Comunícate con el equipo."
					}
					if r.Method == http.MethodPost && status == http.StatusOK {
						status = http.StatusUnprocessableEntity
					}
				} else {
					data.ProgramStartLabel = payments.SpanishDate(date)
				}
			} else if r.Method != http.MethodHead {
				if selected, ok := payments.Find(link.PaymentPlan); ok {
					data.SelectedPlan = selected.Name
				}
				now := time.Now()
				for _, plan := range payments.Plans() {
					data.Plans = append(data.Plans, planView{Plan: plan, Schedule: plan.Schedule(now)})
				}
			}
			show(status)
		}
	}
	for _, view := range []string{"agreement", "payments"} {
		mux.Handle("GET /customer/{token}/"+view, logging.Operation("link_read", handle(view)))
		mux.Handle("POST /customer/{token}/"+view, logging.Operation("final_submit", handle(view)))
	}
}
