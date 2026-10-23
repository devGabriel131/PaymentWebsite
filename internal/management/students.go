package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/students"
)

type studentFieldView struct {
	students.Field
	StudentID uint64
	Value     string
	Modal     bool
	Editing   bool
	Error     string
	Saved     bool
}

type studentView struct {
	students.Student
	Fields []studentFieldView
	OOB    bool
}

type studentPageData struct {
	Staff    string
	Fields   []students.Field
	Students []studentView
	Student  studentView
	Field    *studentFieldView
	Modal    bool
	Billing  *billing.Summary // nil when the student has no billing contract.
}

// studentDisplay evaluates the field registry once for each rendered student.
// The same values feed table rows, detail fields, and saved-field responses.
func studentDisplay(record students.Student, modal bool) studentView {
	fields := students.Fields()
	view := studentView{Student: record, Fields: make([]studentFieldView, len(fields))}
	for i, field := range fields {
		view.Fields[i] = studentFieldView{
			Field: field, StudentID: record.ID, Value: field.Value(record), Modal: modal,
		}
	}
	return view
}

func (view *studentView) field(key string) *studentFieldView {
	for i := range view.Fields {
		if view.Fields[i].Key == key {
			return &view.Fields[i]
		}
	}
	return nil
}

// pathID reads a path value as a signed-BIGINT row ID, so no number SQL cannot hold reaches it.
func pathID(r *http.Request, name string) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue(name), 10, 64)
	return id, err == nil && id >= 1 && id <= 1<<63-1
}

// billingBudget stays under the server's ten-second write timeout, so a slow
// provider call answers with an error instead of a cut connection.
const billingBudget = 8 * time.Second

// billingRefusals are the billing errors staff can act on. They keep their
// status, so htmx leaves the page as it is; anything else is a logged 500.
var billingRefusals = []struct {
	err     error
	status  int
	message string
}{
	{billing.ErrStaffRequired, http.StatusBadRequest, "Sign in as staff to do this."},
	{billing.ErrUnavailable, http.StatusNotFound, "Student or billing record not found."},
	{billing.ErrNotSuspended, http.StatusConflict, "This student is not suspended for billing."},
	{billing.ErrNeedsReview, http.StatusConflict, "This student's billing needs review before this can continue."},
	{billing.ErrPaymentPending, http.StatusConflict, "A payment is still pending. Try again shortly."},
	{billing.ErrPlanLocked, http.StatusConflict, "This student's payment plan is locked."},
	{billing.ErrInvoiceNotReady, http.StatusConflict, "This invoice is not ready yet."},
}

func billingFailure(w http.ResponseWriter, r *http.Request, err error) {
	for _, refusal := range billingRefusals {
		if errors.Is(err, refusal.err) {
			http.Error(w, refusal.message, refusal.status)
			return
		}
	}
	logging.RecordError(r, "billing", err)
	http.Error(w, "Could not complete the billing request. Please try again.", http.StatusInternalServerError)
}

func registerStudentRoutes(mux *http.ServeMux, roster *students.Store, biller *billing.Service, staff func(context.Context) string, execute func(http.ResponseWriter, *http.Request, int, string, any)) {
	notFound := func(w http.ResponseWriter) {
		http.Error(w, "Student or field not found.", http.StatusNotFound)
	}
	load := func(ctx context.Context, w http.ResponseWriter, r *http.Request) *students.Student {
		id, ok := pathID(r, "id")
		if !ok {
			notFound(w)
			return nil
		}
		record, err := roster.Get(ctx, id)
		if err != nil {
			logging.RecordError(r, "load", err)
			http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
			return nil
		}
		if record == nil {
			notFound(w)
		}
		return record
	}
	// withBilling adds the student's billing summary to a page that shows their details.
	withBilling := func(ctx context.Context, w http.ResponseWriter, r *http.Request, data *studentPageData) bool {
		summary, err := biller.ForStudent(ctx, data.Student.ID)
		if err != nil {
			logging.RecordError(r, "billing", err)
			http.Error(w, "Could not load billing. Please try again.", http.StatusInternalServerError)
			return false
		}
		data.Billing = summary
		return true
	}

	mux.Handle("GET /management/students", logging.Operation("student_list", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		records, err := roster.List(ctx)
		if err != nil {
			logging.RecordError(r, "list", err)
			http.Error(w, "Could not load students. Please try again.", http.StatusInternalServerError)
			return
		}
		data := studentPageData{Staff: staff(r.Context()), Fields: students.Fields(), Students: make([]studentView, len(records))}
		for i, record := range records {
			data.Students[i] = studentDisplay(record, false)
		}
		name := "students.html"
		if r.Header.Get("HX-Request") == "true" {
			name = "student-records" // A live refresh of just the records.
		}
		execute(w, r, http.StatusOK, name, data)
	})))

	mux.Handle("GET /management/students/{id}", logging.Operation("student_get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record := load(ctx, w, r)
		if record == nil {
			return
		}
		modal := r.Header.Get("HX-Request") == "true"
		data := studentPageData{Staff: staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
		if field := data.Student.field(r.URL.Query().Get("saved")); field != nil {
			field.Saved = true
		}
		if !withBilling(ctx, w, r, &data) {
			return
		}
		name := "students.html"
		if modal {
			name = "student-detail"
		}
		execute(w, r, http.StatusOK, name, data)
	})))

	fieldHandler := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		field, ok := students.FindField(r.PathValue("field"))
		if !ok {
			notFound(w)
			return
		}
		// Resolve the record before parsing even a malformed or oversized body.
		record := load(ctx, w, r)
		if record == nil {
			return
		}

		partial := r.Header.Get("HX-Request") == "true"
		modal := r.URL.Query().Get("modal") == "1"
		editing := r.URL.Query().Get("edit") == "1"
		status := http.StatusOK
		var attempted, validation string
		saved := false
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			if err := r.ParseForm(); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "This value is too large.", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "Could not read this field. Please try again.", http.StatusBadRequest)
				}
				return
			}
			modal = r.PostForm.Get("modal") == "1"
			attempted = r.PostForm.Get("value")
			updated, err := roster.UpdateField(ctx, record.ID, field.Key, attempted)
			var invalid *students.ValidationError
			switch {
			case errors.As(err, &invalid):
				status, editing, validation = http.StatusUnprocessableEntity, true, invalid.Message
			case err != nil:
				logging.RecordError(r, "save", err)
				http.Error(w, "Could not save this field. Please try again.", http.StatusInternalServerError)
				return
			case updated == nil:
				notFound(w)
				return
			default:
				if !partial {
					path := "/management/students/" + strconv.FormatUint(record.ID, 10) + "?saved=" + url.QueryEscape(field.Key)
					http.Redirect(w, r, path, http.StatusSeeOther)
					return
				}
				record, editing, saved = updated, false, true
			}
		}

		modal = partial && modal
		data := studentPageData{Staff: staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
		data.Field = data.Student.field(field.Key)
		data.Field.Editing, data.Field.Error, data.Field.Saved = editing, validation, saved
		if validation != "" {
			data.Field.Value = attempted
		}
		if !partial {
			if !withBilling(ctx, w, r, &data) {
				return
			}
			execute(w, r, status, "students.html", data)
			return
		}
		if saved {
			data.Student.OOB = modal
			execute(w, r, status, "student-save", data)
			return
		}
		execute(w, r, status, "student-field", data.Field)
	}
	mux.Handle("GET /management/students/{id}/fields/{field}", logging.Operation("student_field_get", http.HandlerFunc(fieldHandler)))
	mux.Handle("POST /management/students/{id}/fields/{field}", logging.Operation("student_update", http.HandlerFunc(fieldHandler)))

	// Billing fragment: read-only, so it never writes to the provider.
	mux.Handle("GET /management/students/{id}/billing", logging.Operation("student_get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record := load(ctx, w, r)
		if record == nil {
			return
		}
		data := studentPageData{Student: studentView{Student: *record}}
		if !withBilling(ctx, w, r, &data) {
			return
		}
		execute(w, r, http.StatusOK, "student-billing", data)
	})))

	// act runs a staff-attributed billing action and answers with the fresh fragment.
	act := func(operation string, do func(context.Context, uint64, string) (*billing.Summary, error)) http.Handler {
		return logging.Operation(operation, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := pathID(r, "id")
			if !ok {
				notFound(w)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), billingBudget)
			defer cancel()
			summary, err := do(ctx, id, staff(r.Context()))
			if err != nil {
				billingFailure(w, r, err)
				return
			}
			execute(w, r, http.StatusOK, "student-billing", studentPageData{
				Student: studentView{Student: students.Student{ID: id}}, Billing: summary,
			})
		}))
	}
	mux.Handle("POST /management/students/{id}/billing/reconcile", act("billing_reconcile", biller.ReconcileStudent))
	mux.Handle("POST /management/students/{id}/billing/reinstatement", act("billing_reinstatement", biller.RequestReinstatement))

	// Hosted invoice links expire and are never stored: fetch one now and send staff there.
	mux.Handle("GET /management/students/{id}/billing/invoices/{obligation}", logging.Operation("billing_invoice_read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, studentOK := pathID(r, "id")
		obligation, obligationOK := pathID(r, "obligation")
		if !studentOK || !obligationOK {
			notFound(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), billingBudget)
		defer cancel()
		invoice, err := biller.InvoiceURL(ctx, id, obligation)
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, invoice, http.StatusSeeOther)
	})))

	// The portal link is a bearer credential for the payer: shown once, never cached or referred.
	mux.Handle("POST /management/students/{id}/billing/portal", logging.Operation("billing_portal", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			notFound(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), billingBudget)
		defer cancel()
		portal, err := biller.PortalURL(ctx, id, staff(r.Context()))
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		execute(w, r, http.StatusOK, "student-billing-portal", portal)
	})))
}
