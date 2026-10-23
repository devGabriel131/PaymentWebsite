package customer

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/payments"
)

type planView struct {
	payments.Plan
	Schedule []payments.Installment
}

// registerPaymentRoutes serves plan selection and starts the hosted first payment.
// Link usability, contact and consent gate every request before billing is touched;
// only POST can create or resume a checkout.
func registerPaymentRoutes(mux *http.ServeMux, store *links.Store, biller *billing.Service, page *template.Template) {
	handle := func(w http.ResponseWriter, r *http.Request) {
		customerHeaders(w)
		token := r.PathValue("token")
		data := pageData{Path: "/customer/" + url.PathEscape(token), View: "payments", FieldErrors: make(map[string]string)}
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
		if link.TermsAcceptedAt == nil {
			redirectCustomer(w, r, data.Path+"/agreement", false)
			return
		}

		status := http.StatusOK
		data.PaymentPlan = link.PaymentPlan
		if r.Method == http.MethodPost {
			status = parseCustomerForm(w, r)
			if status != http.StatusOK {
				data.Error = "No pudimos leer tu selección. Intenta de nuevo."
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
					// StartCheckout bounds its own provider work; the request context only carries disconnects.
					result, err := biller.StartCheckout(r.Context(), link.TokenHash, data.PaymentPlan)
					switch {
					case errors.Is(err, billing.ErrUnavailable):
						show(http.StatusNotFound)
						return
					case errors.Is(err, billing.ErrPlanLocked):
						status = http.StatusConflict
						data.Error = "Tu plan de pago ya está confirmado y no se puede cambiar."
					case err != nil:
						logging.RecordError(r, "checkout", err)
						status = http.StatusInternalServerError
						data.Error = "No pudimos iniciar el pago seguro. Intenta de nuevo."
					case result.URL != "":
						redirectCustomer(w, r, result.URL, true)
						return
					case result.Funded:
						redirectCustomer(w, r, data.Path+"/payments", true)
						return
					default:
						status = http.StatusAccepted
						data.Processing = true
					}
				}
			}
		}

		summaryCtx, cancelSummary := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancelSummary()
		summary, err := biller.ForLink(summaryCtx, link.TokenHash)
		switch {
		case errors.Is(err, billing.ErrUnavailable):
			show(http.StatusNotFound)
			return
		case err != nil:
			logging.RecordError(r, "load", err)
			status = http.StatusInternalServerError
			data.Error = "No pudimos cargar tu información de pago. Intenta de nuevo."
		default:
			data.Billing = summary
		}
		// A funded contract shows its persisted plan instead of a way to pay again.
		if data.Billing == nil || !data.Billing.Funded {
			// While a checkout is processing, the pre-request choice may already be stale.
			if selected, ok := payments.Find(link.PaymentPlan); ok && !data.Processing {
				data.SelectedPlan = selected.Name
			}
			now := time.Now()
			for _, plan := range payments.Plans() {
				data.Plans = append(data.Plans, planView{Plan: plan, Schedule: plan.Schedule(now)})
			}
		}
		show(status)
	}
	mux.Handle("GET /customer/{token}/payments", logging.Operation("link_read", http.HandlerFunc(handle)))
	mux.Handle("POST /customer/{token}/payments", logging.Operation("checkout_start", http.HandlerFunc(handle)))
}

// paymentReturn shows the outcome of a hosted Checkout redirect. It is token-free: it reads one
// persisted attempt and never exposes student identity, contact, or payment history. Visiting it
// (or a cancelled return) never changes financial state.
func paymentReturn(biller *billing.Service, page *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		customerHeaders(w)
		data := pageData{View: "payment-return"}
		query := r.URL.Query()
		switch sessionID := query.Get("session_id"); {
		case sessionID != "":
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			result, err := biller.ReturnStatus(ctx, sessionID)
			switch {
			case errors.Is(err, billing.ErrUnavailable):
				renderCustomer(w, r, page, data, http.StatusNotFound)
				return
			case err != nil:
				logging.RecordError(r, "load", err)
				data.Error = "No pudimos confirmar el estado de tu pago. Intenta de nuevo."
				renderCustomer(w, r, page, data, http.StatusInternalServerError)
				return
			}
			data.Return = &result
		case query.Get("cancelled") == "1":
			data.Return = &billing.ReturnStatus{Status: "not_completed"}
		default:
			renderCustomer(w, r, page, data, http.StatusNotFound)
			return
		}
		renderCustomer(w, r, page, data, http.StatusOK)
	}
}
