//go:build integration

package management

import (
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/students"
)

func enrollStudent(t *testing.T, a *app, name string) (students.Student, links.Link) {
	t.Helper()
	entry, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := a.store.Submit(t.Context(), entry.Token, name); err != nil || !ok {
		t.Fatalf("enroll student: submitted = %v, error = %v", ok, err)
	}
	hash, err := hex.DecodeString(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	var record students.Student
	if err := a.db.WithContext(t.Context()).Where("source_link_hash = ?", hash).Take(&record).Error; err != nil {
		t.Fatal(err)
	}
	var source links.Link
	if err := a.db.WithContext(t.Context()).Where("token_hash = ?", hash).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	return record, source
}

func requireStudentState(t *testing.T, a *app, original students.Student, source links.Link, other students.Student, name string) {
	t.Helper()
	want := original
	want.Name = name
	got, err := students.New(a.db).Get(t.Context(), original.ID)
	if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("student = %+v, error = %v; want %+v", got, err, want)
	}
	unrelated, err := students.New(a.db).Get(t.Context(), other.ID)
	if err != nil || unrelated == nil || !reflect.DeepEqual(*unrelated, other) {
		t.Fatalf("unrelated student changed: %+v, error = %v; want %+v", unrelated, err, other)
	}
	var snapshot links.Link
	if err := a.db.WithContext(t.Context()).Where("token_hash = ?", source.TokenHash).Take(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, source) {
		t.Fatalf("source link changed: got %+v, want %+v", snapshot, source)
	}
	var count int64
	if err := a.db.WithContext(t.Context()).Model(&students.Student{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("student count = %d, error = %v; want 2", count, err)
	}
}

func sendStudentBody(t *testing.T, a *app, browser *http.Client, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	res, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	content, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(content)
}

func TestStudentReadValidateSaveReload(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"

	res, body := a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `<table class="student-table"`) ||
		!strings.Contains(body, `<th scope="col">Name</th>`) || !strings.Contains(body, `<th scope="col">Submitted</th>`) ||
		!strings.Contains(body, `href="/management/students" aria-current="page"`) {
		t.Fatalf("student archive: status = %d: %s", res.StatusCode, body)
	}
	newer := strings.Index(body, fmt.Sprintf(`id="student-row-%d"`, other.ID))
	older := strings.Index(body, fmt.Sprintf(`id="student-row-%d"`, original.ID))
	if newer < 0 || older < 0 || newer >= older {
		t.Fatalf("archive must contain both students, newest first: %s", body)
	}
	if strings.Contains(body, hex.EncodeToString(original.SourceLinkHash)) || strings.Contains(body, "/customer/") ||
		strings.Contains(body, "/static/management.js") {
		t.Fatalf("archive leaked source access or loaded link-only script: %s", body)
	}

	res, body = a.send(t, staff, http.MethodGet, path, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") ||
		!strings.Contains(body, "Student details") || !strings.Contains(body, "Back to students") ||
		strings.Contains(body, `method="dialog"`) || !strings.Contains(body, `aria-label="Edit Name"`) {
		t.Fatalf("standalone details: status = %d: %s", res.StatusCode, body)
	}
	res, body = a.send(t, staff, http.MethodGet, fieldPath+"?edit=1&value=GET+must+not+save", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") ||
		!strings.Contains(body, `value="Original submission"`) || !strings.Contains(body, `<label for="student-input-`) ||
		!strings.Contains(body, `name="value" type="text"`) || !strings.Contains(body, ">Cancel</a>") {
		t.Fatalf("standalone editor: status = %d: %s", res.StatusCode, body)
	}
	requireStudentState(t, a, original, source, other, original.Name)

	for _, tc := range []struct {
		name, value, message string
	}{
		{"invalid UTF-8", "bad\xffname", "Enter a valid name."},
		{"NUL", "<attempt>\x00", "Enter a valid name."},
		{"blank", "   ", "Enter a name."},
	} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/htmx=%t", tc.name, partial), func(t *testing.T) {
				var headers http.Header
				if partial {
					headers = htmx
				}
				res, body := a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {tc.value}, "modal": {"1"}}, headers)
				if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `class="student-field-editor"`) ||
					!strings.Contains(body, `aria-invalid="true"`) || !strings.Contains(body, `aria-describedby="student-error-`) ||
					!strings.Contains(body, `role="alert">`+tc.message) || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") {
					t.Fatalf("invalid field: status = %d: %s", res.StatusCode, body)
				}
				if partial == strings.HasPrefix(body, "<!doctype html>") || strings.Contains(body, `method="dialog"`) {
					t.Fatalf("validation response has wrong page/field shape: %s", body)
				}
				if tc.name == "blank" && !strings.Contains(body, `value="   "`) ||
					tc.name == "NUL" && !strings.Contains(body, "&lt;attempt&gt;") {
					t.Fatalf("editor lost attempted value: %s", body)
				}
				requireStudentState(t, a, original, source, other, original.Name)
			})
		}
	}

	name := "María O'Connor 李"
	res, body = a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {"  " + name + "  "}, "modal": {"1"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != path+"?saved=name" || strings.Contains(body, "hx-swap-oob") {
		t.Fatalf("ordinary save: status = %d, location = %q: %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	requireStudentState(t, a, original, source, other, name)
	res, body = a.send(t, staff, http.MethodGet, res.Header.Get("Location"), nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, html.EscapeString(name)) || !strings.Contains(body, `role="status">Saved.`) ||
		strings.Contains(body, `class="student-field-editor"`) || strings.Contains(body, `method="dialog"`) {
		t.Fatalf("persisted detail reload: status = %d: %s", res.StatusCode, body)
	}
	_, body = a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if !strings.Contains(body, html.EscapeString(name)) {
		t.Fatalf("archive reload lost persisted name: %s", body)
	}
}

func TestStudentHTMXFragmentsModalAndEscaping(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"
	fieldID := fmt.Sprintf(`id="student-field-%d-name"`, original.ID)

	res, body := a.send(t, staff, http.MethodGet, path, nil, htmx)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `class="student-details" hx-sync="closest .student-details:queue all"`) ||
		!strings.Contains(body, `id="student-heading"`) || !strings.Contains(body, `method="dialog"`) ||
		!strings.Contains(body, "?edit=1&amp;modal=1") || strings.Contains(body, "Back to students") || strings.Contains(body, `<dialog`) {
		t.Fatalf("modal content: status = %d: %s", res.StatusCode, body)
	}

	for _, modal := range []bool{false, true} {
		query := "?edit=1"
		if modal {
			query += "&modal=1"
		}
		res, body = a.send(t, staff, http.MethodGet, fieldPath+query, nil, htmx)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) || !strings.Contains(body, `class="student-field-editor"`) ||
			strings.Contains(body, "student-heading") || strings.Contains(body, `method="dialog"`) ||
			strings.Contains(body, `name="modal" value="1"`) != modal {
			t.Fatalf("single editor modal=%t: status = %d: %s", modal, res.StatusCode, body)
		}
		cancel := fieldPath
		if modal {
			cancel += "?modal=1"
		}
		res, body = a.send(t, staff, http.MethodGet, cancel, nil, htmx)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) || !strings.Contains(body, original.Name) ||
			strings.Contains(body, "student-field-editor") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "student-heading") {
			t.Fatalf("cancel field modal=%t: status = %d: %s", modal, res.StatusCode, body)
		}
	}
	res, body = a.send(t, staff, http.MethodGet, fieldPath+"?edit=1&modal=1", nil, nil)
	if res.StatusCode != http.StatusOK || strings.Contains(body, `method="dialog"`) || strings.Contains(body, `name="modal"`) ||
		!strings.Contains(body, "Back to students") {
		t.Fatalf("presentation flag must not turn fallback into dialog: status = %d: %s", res.StatusCode, body)
	}
	requireStudentState(t, a, original, source, other, original.Name)

	name := `<script>alert("student")</script> O'Connor 李`
	for _, modal := range []bool{false, true} {
		form := url.Values{"value": {name}, "source_link_hash": {"ignored"}, "id": {fmt.Sprint(other.ID)}}
		query := "?modal=1&value=Query+must+not+save"
		if modal {
			form.Set("modal", "1")
			query = "?modal=0"
		}
		res, body = a.send(t, staff, http.MethodPost, fieldPath+query, form, htmx)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) || !strings.Contains(body, html.EscapeString(name)) ||
			!strings.Contains(body, `role="status">Saved.`) || strings.Contains(body, "<!doctype html>") || strings.Contains(body, "student-heading") ||
			strings.Contains(body, `<script>alert`) || strings.Contains(body, "student-field-editor") {
			t.Fatalf("HTMX save modal=%t: status = %d: %s", modal, res.StatusCode, body)
		}
		if strings.Contains(body, `hx-swap-oob="outerHTML"`) != modal || strings.Contains(body, "<template>") != modal {
			t.Fatalf("OOB row must exist only for body modal flag, modal=%t: %s", modal, body)
		}
		_, oob, _ := strings.Cut(body, "<template>")
		if modal && (!strings.Contains(oob, fmt.Sprintf(`<tr id="student-row-%d"`, original.ID)) ||
			!strings.HasSuffix(strings.TrimSpace(oob), "</template>") || !strings.Contains(oob, "data-student-link")) {
			t.Fatalf("modal save must return template-wrapped shared table row: %s", body)
		}
		requireStudentState(t, a, original, source, other, name)
	}
	for _, reload := range []string{path, "/management/students", fieldPath + "?edit=1"} {
		res, body = a.send(t, staff, http.MethodGet, reload, nil, nil)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, html.EscapeString(name)) || strings.Contains(body, `<script>alert`) {
			t.Fatalf("escaped persisted value on %s: status = %d: %s", reload, res.StatusCode, body)
		}
	}
}

func TestStudentRejectedRequestsCannotMutate(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"

	for _, key := range []string{"id", "source_link_hash", "created_at", "missing", "name = 'changed'; DROP TABLE students;--"} {
		t.Run("unknown field "+key, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				res, body := a.send(t, staff, method, path+"/fields/"+url.PathEscape(key), url.Values{"value": {"changed"}}, htmx)
				if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "Student or field not found.") {
					t.Fatalf("unknown field: status = %d: %s", res.StatusCode, body)
				}
			}
			requireStudentState(t, a, original, source, other, original.Name)
		})
	}
	for _, id := range []string{"0", "-1", "+1", "not-a-number", "18446744073709551616", "18446744073709551615", "1 OR 1=1"} {
		t.Run("invalid student "+id, func(t *testing.T) {
			missing := "/management/students/" + url.PathEscape(id)
			for _, suffix := range []string{"", "/fields/name?edit=1"} {
				res, body := a.send(t, staff, http.MethodGet, missing+suffix, nil, htmx)
				if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "Student or field not found.") {
					t.Fatalf("invalid student GET: status = %d: %s", res.StatusCode, body)
				}
			}
			for _, raw := range []string{"value=", "value=%zz", "value=" + strings.Repeat("x", 4096)} {
				res, body := sendStudentBody(t, a, staff, missing+"/fields/name", raw)
				if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "Student or field not found.") {
					t.Fatalf("missing student must precede body validation: status = %d: %s", res.StatusCode, body)
				}
			}
			requireStudentState(t, a, original, source, other, original.Name)
		})
	}
	for _, tc := range []struct {
		name, raw, message string
		status             int
	}{
		{"malformed escape", "value=%zz", "Could not read this field. Please try again.", http.StatusBadRequest},
		{"malformed delimiter", "value=changed;other=value", "Could not read this field. Please try again.", http.StatusBadRequest},
		{"oversized", "value=" + strings.Repeat("x", 4096), "This value is too large.", http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := sendStudentBody(t, a, staff, fieldPath, tc.raw)
			if res.StatusCode != tc.status || !strings.Contains(body, tc.message) || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("rejected body: status = %d, want %d: %s", res.StatusCode, tc.status, body)
			}
			requireStudentState(t, a, original, source, other, original.Name)
		})
	}

	// A concurrent disappearance (or a database-suppressed update) is not a save.
	if err := a.db.WithContext(t.Context()).Exec(`CREATE FUNCTION suppress_student_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.WithContext(t.Context()).Exec(`CREATE TRIGGER suppress_student_update BEFORE UPDATE ON students FOR EACH ROW EXECUTE FUNCTION suppress_student_update()`).Error; err != nil {
		t.Fatal(err)
	}
	res, body := a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {"changed"}, "modal": {"1"}}, htmx)
	if res.StatusCode != http.StatusNotFound || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") {
		t.Fatalf("zero-row save: status = %d: %s", res.StatusCode, body)
	}
	requireStudentState(t, a, original, source, other, original.Name)
}

func TestStudentEmptyAndDatabaseFailures(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	res, body := a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "No students yet") ||
		!strings.Contains(body, "Completed customer forms will appear here.") || strings.Contains(body, `<table`) {
		t.Fatalf("empty archive: status = %d: %s", res.StatusCode, body)
	}
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"
	if err := a.db.WithContext(t.Context()).Exec(`ALTER TABLE students ADD CONSTRAINT reject_test_student_name CHECK (name <> 'Rejected by database')`).Error; err != nil {
		t.Fatal(err)
	}
	for _, headers := range []http.Header{nil, htmx} {
		res, body = a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {"Rejected by database"}, "modal": {"1"}}, headers)
		if res.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "Could not save this field. Please try again.") ||
			strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "Rejected by database") {
			t.Fatalf("failed database write: status = %d: %s", res.StatusCode, body)
		}
		requireStudentState(t, a, original, source, other, original.Name)
	}
	if err := a.db.WithContext(t.Context()).Exec(`ALTER TABLE students RENAME TO unavailable_students`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, message string
	}{
		{http.MethodGet, "/management/students", "Could not load students. Please try again."},
		{http.MethodGet, path + "?saved=name", "Could not load student. Please try again."},
		{http.MethodGet, fieldPath + "?edit=1", "Could not load student. Please try again."},
		{http.MethodPost, fieldPath, "Could not load student. Please try again."},
	} {
		for _, headers := range []http.Header{nil, htmx} {
			res, body = a.send(t, staff, tc.method, tc.path, url.Values{"value": {"changed"}, "modal": {"1"}}, headers)
			if res.StatusCode != http.StatusInternalServerError || !strings.Contains(body, tc.message) ||
				strings.Contains(body, "No students yet") || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "Original submission") {
				t.Fatalf("failed database read %s %s: status = %d: %s", tc.method, tc.path, res.StatusCode, body)
			}
		}
	}
}
