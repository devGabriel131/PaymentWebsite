package management

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"paymentwebsite/internal/students"
)

type studentFieldView struct {
	StudentID uint64
	Key       string
	Label     string
	InputType string
	Required  bool
	Value     string
	Modal     bool
	Editing   bool
	Error     string
	Saved     bool
}

type studentView struct {
	ID        uint64
	Fields    []studentFieldView
	CreatedAt time.Time
	OOB       bool
}

type studentListData struct {
	Staff    string
	Fields   []students.Field
	Students []studentView
}

type studentPageData struct {
	Staff   string
	Student studentView
	Field   *studentFieldView
	Modal   bool
}

// studentDisplay evaluates the field registry once for each rendered student.
// The same values feed table rows, detail fields, and saved-field responses.
func studentDisplay(record students.Student, modal bool) studentView {
	fields := students.Fields()
	view := studentView{ID: record.ID, CreatedAt: record.CreatedAt, Fields: make([]studentFieldView, len(fields))}
	for i, field := range fields {
		view.Fields[i] = studentFieldView{
			StudentID: record.ID, Key: field.Key(), Label: field.Label(),
			InputType: field.InputType(), Required: field.Required(),
			Value: field.Value(record), Modal: modal,
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

func registerStudentRoutes(mux *http.ServeMux, roster *students.Store, staff func(context.Context) string, execute func(http.ResponseWriter, int, string, any)) {
	notFound := func(w http.ResponseWriter) {
		http.Error(w, "Student or field not found.", http.StatusNotFound)
	}
	load := func(ctx context.Context, w http.ResponseWriter, r *http.Request) *students.Student {
		id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil || id == 0 {
			notFound(w)
			return nil
		}
		record, err := roster.Get(ctx, id)
		if err != nil {
			log.Printf("load student %d: %v", id, err)
			http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
			return nil
		}
		if record == nil {
			notFound(w)
		}
		return record
	}

	mux.HandleFunc("GET /management/students", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		records, err := roster.List(ctx)
		if err != nil {
			log.Printf("list students: %v", err)
			http.Error(w, "Could not load students. Please try again.", http.StatusInternalServerError)
			return
		}
		data := studentListData{Staff: staff(r.Context()), Fields: students.Fields(), Students: make([]studentView, len(records))}
		for i, record := range records {
			data.Students[i] = studentDisplay(record, false)
		}
		execute(w, http.StatusOK, "students.html", data)
	})

	mux.HandleFunc("GET /management/students/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		name := "student.html"
		if modal {
			name = "student-detail"
		}
		execute(w, http.StatusOK, name, data)
	})

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
			updated, err := roster.UpdateField(ctx, record.ID, field.Key(), attempted)
			var invalid *students.ValidationError
			switch {
			case errors.As(err, &invalid):
				status, editing, validation = http.StatusUnprocessableEntity, true, invalid.Message
			case errors.Is(err, students.ErrUnknownField):
				notFound(w)
				return
			case err != nil:
				// Database error strings may contain the rejected submitted value.
				log.Printf("save student %d field %s: %T", record.ID, field.Key(), err)
				http.Error(w, "Could not save this field. Please try again.", http.StatusInternalServerError)
				return
			case updated == nil:
				notFound(w)
				return
			default:
				if !partial {
					path := "/management/students/" + strconv.FormatUint(record.ID, 10) + "?saved=" + url.QueryEscape(field.Key())
					http.Redirect(w, r, path, http.StatusSeeOther)
					return
				}
				record, editing, saved = updated, false, true
			}
		}

		modal = partial && modal
		data := studentPageData{Staff: staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
		data.Field = data.Student.field(field.Key())
		data.Field.Editing, data.Field.Error, data.Field.Saved = editing, validation, saved
		if validation != "" {
			data.Field.Value = attempted
		}
		if !partial {
			execute(w, status, "student.html", data)
			return
		}
		if saved {
			data.Student.OOB = modal
			execute(w, status, "student-save", data)
			return
		}
		execute(w, status, "student-field", data.Field)
	}
	mux.HandleFunc("GET /management/students/{id}/fields/{field}", fieldHandler)
	mux.HandleFunc("POST /management/students/{id}/fields/{field}", fieldHandler)
}
