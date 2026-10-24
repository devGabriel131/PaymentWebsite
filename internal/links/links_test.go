//go:build integration

package links

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

// staff is the acting account for tests that aren't about the audit trail.
const staff = "tester"

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestIssuedTokenBoundary(t *testing.T) {
	db, store, ctx := testStore(t)

	first, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("only the token digest is stored", func(t *testing.T) {
		if first.ID != hex.EncodeToString(digestOf(first.Token)) {
			t.Fatalf("entry ID %q does not name the token digest", first.ID)
		}
		var row map[string]any
		if err := db.WithContext(ctx).Table("links").Where("token_hash = ?", digestOf(first.Token)).Take(&row).Error; err != nil {
			t.Fatalf("issued token has no persisted SHA-256 digest: %v", err)
		}
		for column, value := range row {
			var stored []byte
			switch v := value.(type) {
			case string:
				stored = []byte(v)
			case []byte:
				stored = v
			}
			if bytes.Contains(stored, []byte(first.Token)) {
				t.Fatalf("column %s stores the usable token", column)
			}
		}
	})

	tampered := first.Token[:len(first.Token)-1] + "A"
	if tampered == first.Token {
		tampered = first.Token[:len(first.Token)-1] + "B"
	}
	cases := []struct {
		name  string
		token string
		valid bool
	}{
		{"earlier link remains valid", first.Token, true},
		{"newer link is valid", second.Token, true},
		{"unissued token", base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)), false},
		{"tampered token", tampered, false},
		{"missing token", "", false},
		{"SQL-like input", strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, err := store.Open(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid := link != nil; valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
			if !tc.valid {
				for _, save := range []func() (SaveResult, error){
					func() (SaveResult, error) { return store.SaveDraft(ctx, tc.token, "name", "Must not save") },
					func() (SaveResult, error) { return store.Submit(ctx, tc.token, contactDetails("Must not save")) },
				} {
					if result, err := save(); err != nil || result != Unavailable {
						t.Fatalf("invalid token write: result=%v, err=%v", result, err)
					}
				}
			}
		})
	}

	t.Run("failed persistence does not issue a token", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		entry, err := store.Create(cancelled, staff, later)
		if err == nil || entry != (Entry{}) {
			t.Fatal("cancelled database write must return an error and no usable link")
		}
		for _, save := range []func() (SaveResult, error){
			func() (SaveResult, error) { return store.SaveDraft(cancelled, first.Token, "name", "Must not save") },
			func() (SaveResult, error) {
				return store.Submit(cancelled, first.Token, contactDetails("Must not save"))
			},
		} {
			if result, err := save(); err == nil || result != Unavailable {
				t.Fatalf("failed database write: result=%v, err=%v", result, err)
			}
		}
	})
}

func TestStatusFollowsCustomerProgress(t *testing.T) {
	db, store, ctx := testStore(t)
	link, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	details := contactDetails("Ana Pérez")
	refused := func(save func() (SaveResult, error)) func() error {
		return func() error {
			if saved, err := save(); err != nil || saved != AlreadySubmitted {
				return fmt.Errorf("submitted form replay: saved=%v, err=%v", saved, err)
			}
			return nil
		}
	}
	lookedUp := func() error {
		// Open's first-visit update scopes by the returned digest.
		if found, err := store.Lookup(ctx, link.Token); err != nil || found == nil || !bytes.Equal(found.TokenHash, digestOf(link.Token)) {
			return fmt.Errorf("lookup did not return the usable link: link=%+v, err=%v", found, err)
		}
		return nil
	}
	// Open returns the first visit immediately and preserves it on later reads.
	var firstVisit *time.Time
	opened := func() error {
		found, err := store.Open(ctx, link.Token)
		if err != nil || found == nil || found.OpenedAt == nil {
			return fmt.Errorf("open did not return its first visit: link=%+v, err=%v", found, err)
		}
		if firstVisit == nil {
			firstVisit = found.OpenedAt
		} else if !found.OpenedAt.Equal(*firstVisit) {
			return fmt.Errorf("open moved first visit from %v to %v", *firstVisit, found.OpenedAt)
		}
		return nil
	}
	saved := func(save func() (SaveResult, error)) func() error {
		return func() error {
			if result, err := save(); err != nil || result != Saved {
				return fmt.Errorf("save: result=%v, err=%v", result, err)
			}
			return nil
		}
	}

	steps := []struct {
		name   string
		act    func() error
		status Status
		saved  string
	}{
		{"issued", func() error { return nil }, NotOpened, ""},
		{"looked up before opening", lookedUp, NotOpened, ""},
		{"opened", opened, Opened, ""},
		{"reopened", opened, Opened, ""},
		{"empty autosave", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "") }), Opened, ""},
		{"autosaved", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "Ana") }), InProgress, "Ana"},
		{"reopened draft", opened, InProgress, "Ana"},
		{"submitted", saved(func() (SaveResult, error) { return store.Submit(ctx, link.Token, details) }), Finished, details.Name},
		{"looked up after submit", lookedUp, Finished, details.Name},
		{"autosave after submit is refused", refused(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "Changed after submit") }), Finished, details.Name},
		{"second submit is refused", refused(func() (SaveResult, error) {
			return store.Submit(ctx, link.Token, contactDetails("Changed after submit"))
		}), Finished, details.Name},
		{"cancel cannot revoke completion", func() error { _, err := store.Cancel(ctx, link.ID, staff); return err }, Finished, details.Name},
		{"finished link still opens success", lookedUp, Finished, details.Name},
	}
	// The first visit is recorded once; no later step may move it.
	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) {
			if err := step.act(); err != nil {
				t.Fatal(err)
			}
			listed := listByID(t, store, ctx)
			got := listed[link.ID]
			if got.Status != step.status || got.Name != step.saved || got.Token != "" {
				t.Fatalf("listed %+v, want status %q, name %q, no token", got, step.status, step.saved)
			}
			if listed[other.ID].Status != NotOpened {
				t.Fatalf("other link changed to %q", listed[other.ID].Status)
			}
			var row Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(link.Token)).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if firstVisit != nil && (row.OpenedAt == nil || !row.OpenedAt.Equal(*firstVisit)) {
				t.Fatalf("first visit %v changed to %v", *firstVisit, row.OpenedAt)
			}
			firstVisit = row.OpenedAt
			enrolled := studentsFor(t, db, ctx, link.Token)
			if step.status == Finished {
				if len(enrolled) != 1 || enrolled[0].Details != details || row.Details != details || got.Phone != details.Phone || got.SubmittedAt == nil ||
					!enrolled[0].CreatedAt.Equal(*got.SubmittedAt) || row.CanceledAt != nil || row.CanceledBy != "" ||
					got.Cancelable() || got.Trashed() {
					t.Fatalf("completion changed or lost enrollment: entry=%+v, row=%+v, students=%+v", got, row, enrolled)
				}
			} else if len(enrolled) != 0 || row.SubmittedAt != nil {
				t.Fatalf("unfinished form enrolled a student: row=%+v, students=%+v", row, enrolled)
			}
		}) {
			return
		}
	}

	// Enrollment retains its source even when staff independently edit students.
	digest := digestOf(link.Token)
	if err := db.WithContext(ctx).Where("token_hash = ?", digest).Delete(&Link{}).Error; err == nil {
		t.Fatal("student enrollment did not restrict deletion of its source link")
	}
	replacementDigest := digestOf("unused replacement source link hash")
	if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digest).
		Update("token_hash", replacementDigest).Error; err == nil {
		t.Fatal("student enrollment did not restrict changing its source link hash")
	}
	var before []Link
	if err := db.WithContext(ctx).Order("token_hash").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, link.Token)
	for range 2 {
		if err := db.WithContext(ctx).AutoMigrate(&Link{}, &students.Student{}); err != nil {
			t.Fatal(err)
		}
		var after []Link
		if err := db.WithContext(ctx).Order("token_hash").Find(&after).Error; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("repeat migration changed links: before=%+v, after=%+v", before, after)
		}
		if after := studentsFor(t, db, ctx, link.Token); !reflect.DeepEqual(after, enrolled) {
			t.Fatalf("repeat migration changed enrollment: before=%+v, after=%+v", enrolled, after)
		}
	}
}

func TestOpenObservesConcurrentCancellation(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	blocker := db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	openCtx, cancelOpen := context.WithCancel(ctx)
	type result struct {
		link *Link
		err  error
	}
	var results chan result
	defer func() {
		blocker.Rollback()
		cancelOpen()
		if results != nil {
			for range results {
			}
		}
	}()
	var blockerPID int
	if err := blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error; err != nil {
		t.Fatal(err)
	}
	canceled, err := New(blocker, nil).Cancel(ctx, issued.ID, "canceler")
	if err != nil || canceled == nil || canceled.Status != Canceled {
		t.Fatalf("hold cancellation: entry=%+v, err=%v", canceled, err)
	}

	results = make(chan result, 1)
	go func() {
		defer close(results)
		opened, err := store.Open(openCtx, issued.Token)
		results <- result{link: opened, err: err}
	}()
	// Observe the database lock, not elapsed time, before releasing cancellation.
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := db.WithContext(ctx).Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))",
			blockerPID,
		).Scan(&waiting).Error; err != nil {
			t.Fatalf("observe Open lock wait: %v", err)
		}
		if waiting {
			break
		}
		select {
		case got := <-results:
			t.Fatalf("Open returned before waiting on cancellation: link=%+v, err=%v", got.link, got.err)
		case <-ctx.Done():
			t.Fatalf("Open did not wait on cancellation: %v", ctx.Err())
		case <-poll.C:
		}
	}
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.err != nil {
		t.Fatalf("Open after cancellation: %v", got.err)
	}
	var row Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if got.link != nil || row.CanceledAt == nil || row.CanceledBy != "canceler" ||
		row.OpenedAt != nil || row.SubmittedAt != nil {
		t.Fatalf("Open ignored committed cancellation: link=%+v, row=%+v", got.link, row)
	}
}

func TestConcurrentSubmissionEnrollsOnce(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, issued.Token, "name", "Draft only"); err != nil || saved != Saved {
		t.Fatalf("draft: saved=%v, err=%v", saved, err)
	}
	if enrolled := studentsFor(t, db, ctx, issued.Token); len(enrolled) != 0 {
		t.Fatalf("draft enrolled students: %+v", enrolled)
	}
	type result struct {
		details students.Details
		saved   SaveResult
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, details := range []students.Details{
		{Name: "First final answer", Email: "first@example.com", Phone: "+17872345678", Municipality: "San Juan"},
		{Name: "Second final answer", Email: "second@example.com", Phone: "+12025550123", Municipality: "Fuera de Puerto Rico"},
	} {
		go func() {
			<-start
			saved, err := store.Submit(ctx, issued.Token, details)
			results <- result{details: details, saved: saved, err: err}
		}()
	}
	close(start)
	var winner students.Details
	replays := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		switch got.saved {
		case Saved:
			if winner != (students.Details{}) {
				t.Fatal("competing submissions both succeeded")
			}
			winner = got.details
		case AlreadySubmitted:
			replays++
		default:
			t.Fatalf("competing submission returned %v, want Saved or AlreadySubmitted", got.saved)
		}
	}
	if replays != 1 {
		t.Fatalf("competing submissions returned %d replays, want one", replays)
	}
	completed, err := store.Lookup(ctx, issued.Token)
	enrolled := studentsFor(t, db, ctx, issued.Token)
	if err != nil || winner == (students.Details{}) || completed == nil || completed.Details != winner || completed.SubmittedAt == nil ||
		len(enrolled) != 1 || enrolled[0].Details != winner || !bytes.Equal(enrolled[0].SourceLinkHash, completed.TokenHash) ||
		!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatalf("completion not atomic: winner=%+v, link=%+v, students=%+v, err=%v", winner, completed, enrolled, err)
	}
	before := listByID(t, store, ctx)[issued.ID]
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != AlreadySubmitted {
			t.Fatalf("finished form accepted replacement: saved=%v, err=%v", saved, err)
		}
	}
	if saved, err := store.Submit(ctx, issued.Token, contactDetails("Replacement final")); err != nil || saved != AlreadySubmitted {
		t.Fatalf("finished form accepted replacement: saved=%v, err=%v", saved, err)
	}
	canceled, err := store.Cancel(ctx, issued.ID, "other staff")
	if err != nil || canceled == nil || !reflect.DeepEqual(*canceled, before) || canceled.Token != "" {
		t.Fatalf("single cancel changed completion: entry=%+v, before=%+v, err=%v", canceled, before, err)
	}
	if err := store.CancelAll(ctx, "other staff"); err != nil {
		t.Fatal(err)
	}
	after, err := store.Lookup(ctx, issued.Token)
	if err != nil || !reflect.DeepEqual(after, completed) || !reflect.DeepEqual(studentsFor(t, db, ctx, issued.Token), enrolled) {
		t.Fatalf("replay or cancellation changed completion: before=%+v, after=%+v, err=%v", completed, after, err)
	}
}

func TestSubmissionFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conflict   bool
		constraint string
	}{
		{"student INSERT check", false, "CHECK (name <> 'Rejected submission')"},
		{"deferred commit constraint", true, "UNIQUE (name) DEFERRABLE INITIALLY DEFERRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, ctx := testStore(t)
			details := contactDetails("Rejected submission")
			if tc.conflict {
				existing, err := store.Create(ctx, staff, later)
				if err != nil {
					t.Fatal(err)
				}
				if saved, err := store.Submit(ctx, existing.Token, details); err != nil || saved != Saved {
					t.Fatalf("seed existing student: saved=%v, err=%v", saved, err)
				}
			}
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			for _, field := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(draft)); err != nil || saved != Saved {
					t.Fatalf("save %s draft: saved=%v, err=%v", field.Key, saved, err)
				}
			}
			before, err := store.Lookup(ctx, issued.Token)
			if err != nil || before == nil {
				t.Fatalf("load draft: link=%+v, err=%v", before, err)
			}
			// Constraint SQL is fixed test data, scoped to this test's schema.
			if err := db.WithContext(ctx).Exec("ALTER TABLE students ADD CONSTRAINT test_student_rejection " + tc.constraint).Error; err != nil {
				t.Fatal(err)
			}
			changes := 0
			store = New(db, func() { changes++ })
			if saved, err := store.Submit(ctx, issued.Token, details); err == nil || saved != Unavailable {
				t.Fatalf("failed transaction reported success: saved=%v, err=%v", saved, err)
			}
			after, err := store.Lookup(ctx, issued.Token)
			if err != nil || !reflect.DeepEqual(after, before) || len(studentsFor(t, db, ctx, issued.Token)) != 0 || changes != 0 {
				t.Fatalf("failed transaction changed draft/submission/expiry: before=%+v, after=%+v, err=%v", before, after, err)
			}
			if err := db.WithContext(ctx).Exec("ALTER TABLE students DROP CONSTRAINT test_student_rejection").Error; err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, issued.Token, details); err != nil || saved != Saved {
				t.Fatalf("retry failed: saved=%v, err=%v", saved, err)
			}
			completed, err := store.Lookup(ctx, issued.Token)
			enrolled := studentsFor(t, db, ctx, issued.Token)
			if err != nil || completed == nil || completed.Details != details || completed.SubmittedAt == nil || changes != 1 ||
				len(enrolled) != 1 || enrolled[0].Details != details ||
				!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
				t.Fatalf("retry did not commit completion and student: link=%+v, students=%+v, err=%v", completed, enrolled, err)
			}
		})
	}
}

func TestSubmissionCancellationRace(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	draft := students.Student{Details: students.Details{
		Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
	}}
	final := contactDetails("Final answer")
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(draft)); err != nil || saved != Saved {
			t.Fatalf("draft %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	type result struct {
		saved SaveResult
		entry *Entry
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		saved, err := store.Submit(ctx, issued.Token, final)
		results <- result{saved: saved, err: err}
	}()
	go func() {
		<-start
		entry, err := store.Cancel(ctx, issued.ID, "canceler")
		results <- result{entry: entry, err: err}
	}()
	close(start)
	var submitted SaveResult
	var canceled *Entry
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.entry != nil {
			canceled = got.entry
		} else {
			submitted = got.saved
		}
	}
	var row Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, issued.Token)
	if canceled == nil || canceled.Token != "" {
		t.Fatalf("cancel did not return a token-free saved entry: %+v", canceled)
	}
	switch {
	case row.SubmittedAt != nil:
		if submitted != Saved || row.CanceledAt != nil || row.CanceledBy != "" || row.Details != final ||
			len(enrolled) != 1 || enrolled[0].Details != final || !enrolled[0].CreatedAt.Equal(*row.SubmittedAt) ||
			canceled.Status != Finished || row.ExpiresAt.Sub(*row.SubmittedAt) != 168*time.Hour {
			t.Fatalf("invalid submission-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	case row.CanceledAt != nil:
		if submitted != Unavailable || row.CanceledBy != "canceler" || row.Details != draft.Details ||
			len(enrolled) != 0 || canceled.Status != Canceled || row.ExpiresAt.Sub(later).Abs() > time.Microsecond {
			t.Fatalf("invalid cancellation-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	default:
		t.Fatalf("race left neither completion nor cancellation: %+v", row)
	}
	want := Unavailable
	if submitted == Saved {
		want = AlreadySubmitted
	}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != want {
			t.Fatalf("write after race: saved=%v, want=%v, err=%v", saved, want, err)
		}
	}
	if saved, err := store.Submit(ctx, issued.Token, contactDetails("Replacement after race")); err != nil || saved != want {
		t.Fatalf("submit after race: saved=%v, want=%v, err=%v", saved, want, err)
	}
	again, err := store.Cancel(ctx, issued.ID, "later canceler")
	if err != nil || again == nil || !reflect.DeepEqual(again, canceled) {
		t.Fatalf("repeat cancel changed race outcome: before=%+v, after=%+v, err=%v", canceled, again, err)
	}
	if err := store.CancelAll(ctx, "later canceler"); err != nil {
		t.Fatal(err)
	}
	var after Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, row) || !reflect.DeepEqual(studentsFor(t, db, ctx, issued.Token), enrolled) {
		t.Fatalf("replay or cancellation changed race outcome: before=%+v, after=%+v", row, after)
	}
}

func TestCustomerDetailsIsolationAndRevocation(t *testing.T) {
	db, store, ctx := testStore(t)
	first, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	details := contactDetails("María Pérez 李")
	saved, err := store.Submit(ctx, first.Token, details)
	if err != nil || saved != Saved {
		t.Fatalf("save issued link's name: saved=%v, err=%v", saved, err)
	}
	for _, tc := range []struct {
		name        string
		token       string
		wantDetails students.Details
	}{
		{"submitted link", first.Token, details},
		{"separate link", second.Token, students.Details{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := store.Open(ctx, tc.token)
			if err != nil || link == nil {
				t.Fatalf("open issued link: link=%v, err=%v", link, err)
			}
			if link.Details != tc.wantDetails {
				t.Fatalf("details = %+v, want %+v", link.Details, tc.wantDetails)
			}
		})
	}

	if err := store.CancelAll(ctx, staff); err != nil {
		t.Fatal(err)
	}
	saved, err = store.Submit(ctx, first.Token, contactDetails("Should not be saved"))
	if err != nil || saved != AlreadySubmitted {
		t.Fatalf("finished link accepted another name: saved=%v, err=%v", saved, err)
	}
	link, err := store.Open(ctx, first.Token)
	if err != nil || link == nil || link.Details != details {
		t.Fatalf("bulk cancellation changed the completed link: link=%+v, err=%v", link, err)
	}
	if got := listByID(t, store, ctx)[first.ID]; got.Name != details.Name || got.Phone != details.Phone || got.Status != Finished || got.CanceledBy != "" {
		t.Fatalf("bulk cancellation changed completion: %+v", got)
	}
	if enrolled := studentsFor(t, db, ctx, first.Token); len(enrolled) != 1 || enrolled[0].Details != details {
		t.Fatalf("bulk cancellation changed enrollment: %+v", enrolled)
	}
	if other, err := store.Lookup(ctx, second.Token); err != nil || other != nil {
		t.Fatalf("unfinished link escaped bulk cancellation: link=%+v, err=%v", other, err)
	}
}

func TestCancel(t *testing.T) {
	db, store, ctx := testStore(t)
	var entries [2]Entry
	for i := range entries {
		entry, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	const canceledName, name = "Canceled customer", "Still active"
	for i, saved := range []string{canceledName, name} {
		if ok, err := store.SaveDraft(ctx, entries[i].Token, "name", saved); err != nil || ok != Saved {
			t.Fatalf("save link %d name: saved=%v, err=%v", i, ok, err)
		}
	}

	t.Run("database failure returns no row", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if got, err := store.Cancel(cancelled, entries[0].ID, staff); err == nil || got != nil {
			t.Fatalf("entry=%+v, err=%v; want an error and no row", got, err)
		}
	})

	// The first cancellation's persisted time; repeats must not move it.
	var canceledAt *time.Time
	for _, tc := range []struct {
		name, id, staff string
		found           bool
	}{
		{"issued link", entries[0].ID, "alice", true},
		{"already canceled link keeps its audit", entries[0].ID, "bob", true},
		{"unknown link", strings.Repeat("0", hex.EncodedLen(sha256.Size)), staff, false},
		{"malformed ID", "not-issued", staff, false},
		{"customer token is not an ID", entries[1].Token, staff, false},
		{"SQL-like input", strings.Repeat("'", hex.EncodedLen(sha256.Size)), staff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Cancel(ctx, tc.id, tc.staff)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.found {
				if got != nil {
					t.Fatalf("returned %+v for an ID naming no link", got)
				}
				return
			}
			if got == nil || got.ID != entries[0].ID || got.Status != Canceled || got.Name != canceledName ||
				got.CreatedBy != staff || got.CanceledBy != "alice" || got.Token != "" || got.Cancelable() {
				t.Fatalf("returned %+v; want the saved row canceled by alice, without its token", got)
			}
			var row Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(entries[0].Token)).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.CanceledAt == nil {
				t.Fatal("canceled link has no persisted cancellation time")
			}
			if canceledAt == nil {
				canceledAt = row.CanceledAt
			}
			if !row.CanceledAt.Equal(*canceledAt) {
				t.Fatalf("canceled_at moved from %v to %v", *canceledAt, *row.CanceledAt)
			}
		})
	}

	canceled, err := store.Open(ctx, entries[0].Token)
	if err != nil || canceled != nil {
		t.Fatalf("canceled link remains usable: link=%v, err=%v", canceled, err)
	}
	active, err := store.Open(ctx, entries[1].Token)
	if err != nil || active == nil {
		t.Fatalf("canceling one link revoked another: link=%v, err=%v", active, err)
	}
	if active.Name != name {
		t.Fatalf("other link's name = %q, want %q", active.Name, name)
	}
	if saved, err := store.SaveDraft(ctx, entries[0].Token, "name", "Must not revive"); err != nil || saved != Unavailable {
		t.Fatalf("canceled link accepted a draft: saved=%v, err=%v", saved, err)
	}
}

func TestCancelAll(t *testing.T) {
	_, store, ctx := testStore(t)
	var previous [2]Entry
	for i := range previous {
		entry, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		previous[i] = entry
	}

	// Repeated cancellation must remain safe when no links are active.
	for range 2 {
		if err := store.CancelAll(ctx, staff); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		valid bool
	}{
		{"first canceled link", previous[0].Token, false},
		{"second canceled link", previous[1].Token, false},
		{"new link after cancellation", fresh.Token, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, err := store.Open(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid := link != nil; valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func TestAuditRecordsStaff(t *testing.T) {
	_, store, ctx := testStore(t)
	var issued [3]Entry
	for i, by := range []string{"alice", "bob", "alice"} {
		entry, err := store.Create(ctx, by, later)
		if err != nil {
			t.Fatal(err)
		}
		issued[i] = entry
	}
	if _, err := store.Cancel(ctx, issued[0].ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelAll(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	t.Run("anonymous actions are refused", func(t *testing.T) {
		if _, err := store.Create(ctx, "", later); !errors.Is(err, errNoStaff) {
			t.Errorf("Create: err = %v, want errNoStaff", err)
		}
		if got, err := store.Cancel(ctx, issued[1].ID, ""); !errors.Is(err, errNoStaff) || got != nil {
			t.Errorf("Cancel: entry = %+v, err = %v; want no row, errNoStaff", got, err)
		}
		if err := store.CancelAll(ctx, ""); !errors.Is(err, errNoStaff) {
			t.Errorf("CancelAll: err = %v, want errNoStaff", err)
		}
	})

	listed := listByID(t, store, ctx)
	for i, want := range []struct{ createdBy, canceledBy string }{
		{"alice", "bob"}, // CancelAll must not overwrite an earlier cancellation.
		{"bob", "alice"},
		{"alice", "alice"},
	} {
		got := listed[issued[i].ID]
		if got.CreatedBy != want.createdBy || got.CanceledBy != want.canceledBy {
			t.Errorf("link %d: created by %q, canceled by %q; want %q, %q",
				i, got.CreatedBy, got.CanceledBy, want.createdBy, want.canceledBy)
		}
	}
	if len(listed) != len(issued) {
		t.Errorf("listed %d links, want %d: an anonymous Create persisted", len(listed), len(issued))
	}
}

func TestExpiredLinksStopWorking(t *testing.T) {
	db, store, ctx := testStore(t)
	issuedExpired, err := store.Create(ctx, staff, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	midForm, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, midForm.Token, "name", "Half done"); err != nil || saved != Saved {
		t.Fatalf("draft before expiry: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.Submit(ctx, finished.Token, contactDetails("Done final")); err != nil || saved != Saved {
		t.Fatalf("submit before expiry: saved=%v, err=%v", saved, err)
	}
	enrolled := studentsFor(t, db, ctx, finished.Token)
	if len(enrolled) != 1 {
		t.Fatalf("finished link has %d students, want one", len(enrolled))
	}
	// Both lapse while the customer could still be on the page.
	if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash IN ?", [][]byte{digestOf(midForm.Token), digestOf(finished.Token)}).
		Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"expired at issue": issuedExpired.Token, "mid-form": midForm.Token, "expired success page": finished.Token} {
		t.Run(name, func(t *testing.T) {
			if link, err := store.Open(ctx, token); err != nil || link != nil {
				t.Errorf("open: link=%v, err=%v; want none", link, err)
			}
			if saved, err := store.SaveDraft(ctx, token, "name", "After expiry"); err != nil || saved != Unavailable {
				t.Errorf("draft: saved=%v, err=%v; want refused", saved, err)
			}
			if saved, err := store.Submit(ctx, token, contactDetails("After expiry")); err != nil || saved != Unavailable {
				t.Errorf("submit: saved=%v, err=%v; want refused", saved, err)
			}
		})
	}

	// Cancel returns the expired row as saved; it never turns into Canceled.
	if got, err := store.Cancel(ctx, midForm.ID, "bob"); err != nil || got == nil || got.ID != midForm.ID ||
		got.Status != Expired || got.Name != "Half done" || got.CanceledBy != "" || got.Token != "" {
		t.Fatalf("cancel expired link: entry=%+v, err=%v; want it expired with its saved name, without a token", got, err)
	}
	if err := store.CancelAll(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	listed := listByID(t, store, ctx)
	for _, tc := range []struct {
		name   string
		entry  Entry
		status Status
		saved  string
	}{
		{"expired at issue", issuedExpired, Expired, ""},
		{"expiry keeps saved data", midForm, Expired, "Half done"},
		{"finished stays finished", finished, Finished, "Done final"},
	} {
		got := listed[tc.entry.ID]
		if got.Status != tc.status || got.Name != tc.saved || got.CanceledBy != "" || got.Cancelable() ||
			!got.AccessExpired() || got.Trashed() != (tc.status == Expired) {
			t.Errorf("%s: listed %+v; want status %q, name %q, not canceled or cancelable", tc.name, got, tc.status, tc.saved)
		}
	}
	if after := studentsFor(t, db, ctx, finished.Token); !reflect.DeepEqual(after, enrolled) {
		t.Fatalf("source expiry changed enrollment: before=%+v, after=%+v", enrolled, after)
	}
}

func TestEntryArchiveBoundary(t *testing.T) {
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-24*time.Hour + time.Nanosecond)
	exact := now.Add(-24 * time.Hour)
	old := now.Add(-24*time.Hour - time.Nanosecond)
	for _, tc := range []struct {
		name  string
		entry Entry
		want  bool
	}{
		{"24 hours minus 1ns", Entry{Status: Finished, SubmittedAt: &recent, CreatedAt: now.Add(-365 * 24 * time.Hour)}, false},
		{"exactly 24 hours", Entry{Status: Finished, SubmittedAt: &exact, CreatedAt: now}, true},
		{"24 hours plus 1ns", Entry{Status: Finished, SubmittedAt: &old}, true},
		{"nil completion", Entry{Status: Finished}, false},
		{"unfinished", Entry{Status: InProgress}, false},
		{"unfinished with old timestamp", Entry{Status: Opened, SubmittedAt: &old}, false},
		{"canceled", Entry{Status: Canceled, SubmittedAt: &old}, false},
		{"expired", Entry{Status: Expired, SubmittedAt: &old}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.Archived(now); got != tc.want {
				t.Fatalf("Archived(%v) = %v, want %v", now, got, tc.want)
			}
		})
	}
}

func TestEntryAccessExpiryIsNotCancelability(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		entry      Entry
		expired    bool
		cancelable bool
		trashed    bool
	}{
		{"usable unfinished", Entry{Status: NotOpened, ExpiresAt: now.Add(time.Hour)}, false, true, false},
		{"opened unfinished", Entry{Status: Opened, ExpiresAt: now.Add(time.Hour)}, false, true, false},
		{"draft unfinished", Entry{Status: InProgress, ExpiresAt: now.Add(time.Hour)}, false, true, false},
		{"status projection predates expiry", Entry{Status: NotOpened, ExpiresAt: now.Add(-time.Hour)}, true, true, false},
		{"unknown status", Entry{ExpiresAt: now.Add(time.Hour)}, false, false, false},
		{"finished success access", Entry{Status: Finished, ExpiresAt: now.Add(time.Hour)}, false, false, false},
		{"finished expired access", Entry{Status: Finished, ExpiresAt: now.Add(-time.Hour)}, true, false, false},
		{"deadline reached", Entry{Status: Expired, ExpiresAt: now}, true, false, true},
		{"canceled before deadline", Entry{Status: Canceled, ExpiresAt: now.Add(time.Hour)}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.entry.AccessExpired() != tc.expired || tc.entry.Cancelable() != tc.cancelable || tc.entry.Trashed() != tc.trashed {
				t.Fatalf("entry=%+v: expired=%v, cancelable=%v, trashed=%v; want %v, %v, %v", tc.entry,
					tc.entry.AccessExpired(), tc.entry.Cancelable(), tc.entry.Trashed(), tc.expired, tc.cancelable, tc.trashed)
			}
		})
	}
}

// Open management pages refresh only when told: every committed customer or
// staff write must announce itself, and writes that change nothing must not.
func TestCommittedWritesAnnounceChanges(t *testing.T) {
	db, _, ctx := testStore(t)
	changes := 0
	store := New(db, func() { changes++ })
	step := func(name string, want int, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changes != want {
			t.Fatalf("%s: changes = %d, want %d", name, changes, want)
		}
	}

	first, err := store.Create(ctx, staff, later)
	step("create", 1, err)
	_, err = store.Open(ctx, first.Token)
	step("first visit", 2, err)
	_, err = store.Open(ctx, first.Token)
	step("repeat visit", 2, err)
	_, err = store.SaveDraft(ctx, first.Token, "name", "Draft")
	step("draft", 3, err)
	_, err = store.Submit(ctx, first.Token, contactDetails("Final answer"))
	step("submit", 4, err)
	_, err = store.Submit(ctx, first.Token, contactDetails("Replay answer"))
	step("replayed submit", 4, err)
	_, err = store.Cancel(ctx, first.ID, staff)
	step("cancel finished", 4, err)
	second, err := store.Create(ctx, staff, later)
	step("second create", 5, err)
	_, err = store.Cancel(ctx, second.ID, staff)
	step("cancel", 6, err)
	_, err = store.Create(ctx, staff, later)
	step("third create", 7, err)
	err = store.CancelAll(ctx, staff)
	step("cancel all", 8, err)
	err = store.CancelAll(ctx, staff)
	step("nothing left to cancel", 8, err)
}

func TestDraftNoOpPreservesSaveOutcomes(t *testing.T) {
	phone, err := students.ParsePhone("7872345678", false)
	if err != nil || phone != "+17872345678" {
		t.Fatalf("parse first phone: phone=%q, err=%v", phone, err)
	}
	replay, err := students.ParsePhone("+1 (787) 234-5678", false)
	if err != nil || replay != "+17872345678" {
		t.Fatalf("parse equivalent phone: phone=%q, err=%v", replay, err)
	}
	version := func(t *testing.T, db *gorm.DB, ctx context.Context, token string) string {
		t.Helper()
		var xmin string
		if err := db.WithContext(ctx).Raw("SELECT xmin::text FROM links WHERE token_hash = ?", digestOf(token)).Scan(&xmin).Error; err != nil {
			t.Fatal(err)
		}
		if xmin == "" {
			t.Fatal("link row missing xmin")
		}
		return xmin
	}

	t.Run("normalized replay and clearing", func(t *testing.T) {
		db, store, ctx := testStore(t)
		issued, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		initial := students.Student{Details: contactDetails("Draft owner")}
		for _, field := range students.Fields() {
			if field.Key == "phone" {
				continue
			}
			if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(initial)); err != nil || saved != Saved {
				t.Fatalf("seed %s: saved=%v, err=%v", field.Key, saved, err)
			}
		}
		before, err := store.Open(ctx, issued.Token)
		if err != nil || before == nil || before.OpenedAt == nil {
			t.Fatalf("open draft: link=%+v, err=%v", before, err)
		}
		want := *before
		enrolledBefore := studentsFor(t, db, ctx, issued.Token)
		changes := 0
		store.changed = func() { changes++ }
		for _, step := range []struct {
			name    string
			value   string
			changes int
			update  bool
		}{
			{"first save", phone, 1, true},
			{"normalized replay", replay, 1, false},
			{"populated to empty", "", 2, true},
			{"empty to empty", "", 2, false},
		} {
			t.Run(step.name, func(t *testing.T) {
				xmin := version(t, db, ctx, issued.Token)
				changesBefore := changes
				if saved, err := store.SaveDraft(ctx, issued.Token, "phone", step.value); err != nil || saved != Saved {
					t.Errorf("draft: saved=%v, err=%v, want Saved without error", saved, err)
				}
				delta := 0
				if step.update {
					delta = 1
				}
				if changes != step.changes || changes-changesBefore != delta {
					t.Errorf("changes=%d, delta=%d, want changes=%d, delta=%d", changes, changes-changesBefore, step.changes, delta)
				}
				if after := version(t, db, ctx, issued.Token); (after != xmin) != step.update {
					t.Errorf("row xmin before=%q, after=%q, want UPDATE=%v", xmin, after, step.update)
				}
				want.Phone = step.value
				if after, err := store.Lookup(ctx, issued.Token); err != nil || after == nil || !reflect.DeepEqual(*after, want) {
					t.Errorf("draft changed unrelated fields: link=%+v, want=%+v, err=%v", after, want, err)
				}
				if enrolled := studentsFor(t, db, ctx, issued.Token); !reflect.DeepEqual(enrolled, enrolledBefore) {
					t.Errorf("draft changed enrollment: before=%+v, after=%+v", enrolledBefore, enrolled)
				}
			})
		}
	})

	for _, state := range []struct {
		name string
		want SaveResult
		err  error
	}{
		{"submitted", AlreadySubmitted, nil},
		{"canceled", Unavailable, nil},
		{"expired", Unavailable, nil},
		{"unknown token", Unavailable, nil},
		{"malformed token", Unavailable, nil},
		{"canceled context", Unavailable, context.Canceled},
	} {
		t.Run(state.name, func(t *testing.T) {
			db, store, ctx := testStore(t)
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			initial := students.Student{Details: contactDetails("Retained owner")}
			for _, field := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(initial)); err != nil || saved != Saved {
					t.Fatalf("seed %s: saved=%v, err=%v", field.Key, saved, err)
				}
			}
			token, callCtx := issued.Token, ctx
			switch state.name {
			case "submitted":
				if saved, err := store.Submit(ctx, token, initial.Details); err != nil || saved != Saved {
					t.Fatalf("submit: saved=%v, err=%v", saved, err)
				}
			case "canceled":
				if entry, err := store.Cancel(ctx, issued.ID, staff); err != nil || entry == nil || entry.Status != Canceled {
					t.Fatalf("cancel: entry=%+v, err=%v", entry, err)
				}
			case "expired":
				if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digestOf(token)).
					Update("expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
					t.Fatal(err)
				}
			case "unknown token":
				token = base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))
			case "malformed token":
				token = "malformed"
			case "canceled context":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var before Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&before).Error; err != nil {
				t.Fatal(err)
			}
			enrolledBefore := studentsFor(t, db, ctx, issued.Token)
			if state.want == AlreadySubmitted && len(enrolledBefore) != 1 {
				t.Fatalf("submitted fixture has %d students, want one", len(enrolledBefore))
			}
			xmin := version(t, db, ctx, issued.Token)
			changes := 0
			store.changed = func() { changes++ }
			if saved, err := store.SaveDraft(callCtx, token, "phone", replay); saved != state.want || !errors.Is(err, state.err) {
				t.Errorf("draft: saved=%v, err=%v, want saved=%v, err=%v", saved, err, state.want, state.err)
			}
			if changes != 0 {
				t.Errorf("refused/error draft announced %d changes", changes)
			}
			if after := version(t, db, ctx, issued.Token); after != xmin {
				t.Errorf("refused/error draft updated row: xmin before=%q, after=%q", xmin, after)
			}
			var after Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("refused/error draft changed link: before=%+v, after=%+v", before, after)
			}
			if enrolled := studentsFor(t, db, ctx, issued.Token); !reflect.DeepEqual(enrolled, enrolledBefore) {
				t.Errorf("refused/error draft changed enrollment: before=%+v, after=%+v", enrolledBefore, enrolled)
			}
		})
	}
}

func TestDraftFieldIsolationAndConcurrentClearing(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	initial := students.Student{Details: contactDetails("Original draft")}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(initial)); err != nil || saved != Saved {
			t.Fatalf("seed %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	before, err := store.Open(ctx, issued.Token)
	if err != nil || before == nil || before.OpenedAt == nil {
		t.Fatalf("open draft: link=%+v, err=%v", before, err)
	}
	otherBefore, err := store.Lookup(ctx, other.Token)
	if err != nil || otherBefore == nil {
		t.Fatalf("load other link: link=%+v, err=%v", otherBefore, err)
	}
	type write struct{ key, value string }
	for _, step := range []struct {
		name   string
		writes []write
		want   students.Details
		status Status
	}{
		{
			name: "concurrent name and email", writes: []write{{"name", "Updated"}, {"email", "updated@example.com"}},
			want:   students.Details{Name: "Updated", Email: "updated@example.com", Phone: initial.Phone, Municipality: initial.Municipality, DateOfBirth: initial.DateOfBirth},
			status: InProgress,
		},
		{
			name: "clear name while changing phone", writes: []write{{"name", ""}, {"phone", "+12025550123"}},
			want:   students.Details{Email: "updated@example.com", Phone: "+12025550123", Municipality: initial.Municipality, DateOfBirth: initial.DateOfBirth},
			status: InProgress,
		},
		{
			name: "clear email while changing municipality", writes: []write{{"email", ""}, {"municipality", "Fuera de Puerto Rico"}},
			want:   students.Details{Phone: "+12025550123", Municipality: "Fuera de Puerto Rico", DateOfBirth: initial.DateOfBirth},
			status: InProgress,
		},
		{
			name: "clear last fields", writes: []write{{"phone", ""}, {"municipality", ""}, {"date_of_birth", ""}},
			want: students.Details{}, status: Opened,
		},
	} {
		if !t.Run(step.name, func(t *testing.T) {
			type result struct {
				key   string
				saved SaveResult
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, len(step.writes))
			for _, change := range step.writes {
				go func() {
					<-start
					saved, err := store.SaveDraft(ctx, issued.Token, change.key, change.value)
					results <- result{key: change.key, saved: saved, err: err}
				}()
			}
			close(start)
			for range step.writes {
				got := <-results
				if got.err != nil || got.saved != Saved {
					t.Errorf("save %s: result=%v, err=%v", got.key, got.saved, got.err)
				}
			}
			if t.Failed() {
				return
			}
			after, err := store.Lookup(ctx, issued.Token)
			want := *before
			want.Details = step.want
			if err != nil || after == nil || !reflect.DeepEqual(*after, want) {
				t.Fatalf("draft changed unrelated data: link=%+v, want=%+v, err=%v", after, want, err)
			}
			if entry := listByID(t, store, ctx)[issued.ID]; entry.Status != step.status ||
				entry.Name != step.want.Name || entry.Phone != step.want.Phone || entry.Token != "" {
				t.Fatalf("draft projection: %+v, want status=%v, details=%+v", entry, step.status, step.want)
			}
			otherAfter, err := store.Lookup(ctx, other.Token)
			if err != nil || !reflect.DeepEqual(otherAfter, otherBefore) {
				t.Fatalf("draft changed another link: before=%+v, after=%+v, err=%v", otherBefore, otherAfter, err)
			}
			if enrolled := studentsFor(t, db, ctx, issued.Token); len(enrolled) != 0 {
				t.Fatalf("draft created students: %+v", enrolled)
			}
		}) {
			return
		}
	}
}

func TestDraftRejectsUnknownFieldBeforeSQL(t *testing.T) {
	changes := 0
	store := New(nil, func() { changes++ })
	token := base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))
	for _, key := range []string{"", "Name", "email_confirm", "token_hash", "submitted_at", "name; DROP TABLE links"} {
		t.Run(key, func(t *testing.T) {
			if saved, err := store.SaveDraft(t.Context(), token, key, "Must not save"); saved != Unavailable ||
				!errors.Is(err, students.ErrUnknownField) {
				t.Fatalf("unknown field reached persistence: result=%v, err=%v", saved, err)
			}
		})
	}
	if changes != 0 {
		t.Fatalf("rejected fields announced %d changes", changes)
	}
}

func TestAnyDraftFieldMarksProgressAndClears(t *testing.T) {
	for _, field := range students.Fields() {
		t.Run(field.Key, func(t *testing.T) {
			for _, state := range []struct {
				name   string
				opened bool
				status Status
			}{
				{"unopened", false, NotOpened},
				{"opened", true, Opened},
			} {
				t.Run(state.name, func(t *testing.T) {
					db, store, ctx := testStore(t)
					issued, err := store.Create(ctx, staff, later)
					if err != nil {
						t.Fatal(err)
					}
					if state.opened {
						if opened, err := store.Open(ctx, issued.Token); err != nil || opened == nil || opened.OpenedAt == nil {
							t.Fatalf("open: link=%+v, err=%v", opened, err)
						}
					}
					if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(students.Student{Details: contactDetails("Ana Pérez")})); err != nil || saved != Saved {
						t.Fatalf("draft: result=%v, err=%v", saved, err)
					}
					if entry := listByID(t, store, ctx)[issued.ID]; entry.Status != InProgress {
						t.Fatalf("%s alone did not mark progress: %+v", field.Key, entry)
					}
					if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != Saved {
						t.Fatalf("clear: result=%v, err=%v", saved, err)
					}
					link, err := store.Lookup(ctx, issued.Token)
					if err != nil || link == nil || link.Details != (students.Details{}) || (link.OpenedAt != nil) != state.opened ||
						link.SubmittedAt != nil || len(studentsFor(t, db, ctx, issued.Token)) != 0 {
						t.Fatalf("clear changed lifecycle or left data: link=%+v, err=%v", link, err)
					}
					if entry := listByID(t, store, ctx)[issued.ID]; entry.Status != state.status {
						t.Fatalf("clear did not restore %s: %+v", state.status, entry)
					}
				})
			}
		})
	}
}

func TestEntryContactProjectionPreservesStatusPrecedence(t *testing.T) {
	for _, name := range []string{"Email", "Municipality", "Details"} {
		if _, exists := reflect.TypeFor[Entry]().FieldByName(name); exists {
			t.Fatalf("link registry exposes %s", name)
		}
	}
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name                string
		canceled, submitted *time.Time
		expiry              time.Time
		status              Status
	}{
		{"active details", nil, nil, future, InProgress},
		{"expired details", nil, nil, past, Expired},
		{"finished expired details", nil, &past, past, Finished},
		{"canceled finished expired details", &past, &past, past, Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := contactDetails("Ana Pérez")
			entry := (Link{Details: details, CanceledAt: tc.canceled, SubmittedAt: tc.submitted, ExpiresAt: tc.expiry}).entry(now)
			if entry.Status != tc.status || entry.Name != details.Name || entry.Phone != details.Phone || entry.Token != "" {
				t.Fatalf("wrong contact projection or precedence: %+v", entry)
			}
		})
	}
}

func TestDraftSubmissionRaceKeepsFinalSnapshot(t *testing.T) {
	for _, field := range students.Fields() {
		t.Run(field.Key, func(t *testing.T) {
			db, store, ctx := testStore(t)
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			final := contactDetails("Final answer")
			for _, seed := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, issued.Token, seed.Key, seed.Value(draft)); err != nil || saved != Saved {
					t.Fatalf("seed %s: result=%v, err=%v", seed.Key, saved, err)
				}
			}
			type result struct {
				draft bool
				saved SaveResult
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			go func() {
				<-start
				saved, err := store.SaveDraft(ctx, issued.Token, field.Key, "")
				results <- result{draft: true, saved: saved, err: err}
			}()
			go func() {
				<-start
				saved, err := store.Submit(ctx, issued.Token, final)
				results <- result{saved: saved, err: err}
			}()
			close(start)
			for range 2 {
				got := <-results
				if got.err != nil || (!got.draft && got.saved != Saved) ||
					(got.draft && got.saved != Saved && got.saved != AlreadySubmitted) {
					t.Errorf("draft=%v: result=%v, err=%v", got.draft, got.saved, got.err)
				}
			}
			if t.Failed() {
				return
			}
			completed, err := store.Lookup(ctx, issued.Token)
			enrolled := studentsFor(t, db, ctx, issued.Token)
			if err != nil || completed == nil || completed.Details != final || completed.SubmittedAt == nil ||
				completed.CanceledAt != nil || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour ||
				len(enrolled) != 1 || enrolled[0].Details != final || !bytes.Equal(enrolled[0].SourceLinkHash, completed.TokenHash) ||
				!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) {
				t.Fatalf("racing draft changed final snapshot: link=%+v, students=%+v, err=%v", completed, enrolled, err)
			}
			if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != AlreadySubmitted {
				t.Fatalf("late clear: result=%v, err=%v", saved, err)
			}
			if saved, err := store.Submit(ctx, issued.Token, draft.Details); err != nil || saved != AlreadySubmitted {
				t.Fatalf("final replay: result=%v, err=%v", saved, err)
			}
			after, err := store.Lookup(ctx, issued.Token)
			if err != nil || !reflect.DeepEqual(after, completed) || !reflect.DeepEqual(studentsFor(t, db, ctx, issued.Token), enrolled) {
				t.Fatalf("replay changed racing completion: before=%+v, after=%+v, err=%v", completed, after, err)
			}
		})
	}
}

func TestDraftCancellationRaceKeepsRevocation(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	draft := students.Student{Details: contactDetails("Original draft")}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(draft)); err != nil || saved != Saved {
			t.Fatalf("seed %s: result=%v, err=%v", field.Key, saved, err)
		}
	}
	type result struct {
		entry *Entry
		saved SaveResult
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		saved, err := store.SaveDraft(ctx, issued.Token, "email", "")
		results <- result{saved: saved, err: err}
	}()
	go func() {
		<-start
		entry, err := store.Cancel(ctx, issued.ID, "canceler")
		results <- result{entry: entry, err: err}
	}()
	close(start)
	var cleared SaveResult
	var canceled *Entry
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Errorf("race: %v", got.err)
		}
		if got.entry != nil {
			canceled = got.entry
		} else {
			cleared = got.saved
		}
	}
	if t.Failed() {
		return
	}
	want := draft.Details
	switch cleared {
	case Saved:
		want.Email = ""
	case Unavailable:
	default:
		t.Fatalf("draft racing cancellation returned %v", cleared)
	}
	var row Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if canceled == nil || canceled.Status != Canceled || canceled.Token != "" || canceled.Phone != want.Phone ||
		row.Details != want || row.CanceledAt == nil || row.CanceledBy != "canceler" || row.SubmittedAt != nil ||
		row.ExpiresAt.Sub(later).Abs() > time.Microsecond || len(studentsFor(t, db, ctx, issued.Token)) != 0 {
		t.Fatalf("draft changed revocation or unrelated fields: clear=%v, row=%+v, cancel=%+v", cleared, row, canceled)
	}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != Unavailable {
			t.Fatalf("revoked draft %s: result=%v, err=%v", field.Key, saved, err)
		}
	}
	if saved, err := store.Submit(ctx, issued.Token, contactDetails("Final answer")); err != nil || saved != Unavailable {
		t.Fatalf("revoked submit: result=%v, err=%v", saved, err)
	}
	again, err := store.Cancel(ctx, issued.ID, "later canceler")
	if err != nil || !reflect.DeepEqual(again, canceled) {
		t.Fatalf("repeat cancellation changed race outcome: before=%+v, after=%+v, err=%v", canceled, again, err)
	}
	var after Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, row) || len(studentsFor(t, db, ctx, issued.Token)) != 0 {
		t.Fatalf("replay changed revoked draft: before=%+v, after=%+v", row, after)
	}
	if opened, err := store.Open(ctx, issued.Token); err != nil || opened != nil {
		t.Fatalf("revoked draft remains accessible: link=%+v, err=%v", opened, err)
	}
}

func TestAcceptTermsRequiresContactAndCurrentDisplayedDate(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	const firstDate, currentDate = "2026-11-10", "2026-11-17"
	options := settings.New(db)
	if err := options.SetProgramStartDate(ctx, firstDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"unopened", "draft"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "draft" {
				if saved, err := store.SaveDraft(ctx, entry.Token, "name", "Draft contact"); err != nil || saved != Saved {
					t.Fatalf("draft: saved=%v, err=%v", saved, err)
				}
			}
			before, err := store.Lookup(ctx, entry.Token)
			if err != nil || before == nil {
				t.Fatalf("load contact: link=%+v, err=%v", before, err)
			}
			if got, err := store.AcceptTerms(ctx, entry.Token, firstDate); err != nil || got != nil {
				t.Fatalf("accept before submission: link=%+v, err=%v", got, err)
			}
			if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, before) ||
				len(studentsFor(t, db, ctx, entry.Token)) != 0 {
				t.Fatalf("premature agreement changed contact: link=%+v, err=%v", after, err)
			}
		})
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact")); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	before, err := store.Lookup(ctx, entry.Token)
	if err != nil || before == nil {
		t.Fatalf("load submission: link=%+v, err=%v", before, err)
	}
	enrolled := studentsFor(t, db, ctx, entry.Token)
	writes := 0
	store = New(db, func() { writes++ })
	if err := options.SetProgramStartDate(ctx, currentDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{"", "not-a-date", "2026-02-30", firstDate} {
		t.Run("reject displayed "+shown, func(t *testing.T) {
			if got, err := store.AcceptTerms(ctx, entry.Token, shown); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("stale acceptance: link=%+v, err=%v", got, err)
			}
		})
	}
	if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, before) || writes != 0 {
		t.Fatalf("rejected agreement changed link: link=%+v, writes=%d, err=%v", after, writes, err)
	}
	accepted, err := store.AcceptTerms(ctx, entry.Token, currentDate)
	if err != nil || accepted == nil || accepted.TermsAcceptedAt == nil || accepted.ProgramStartDate != currentDate || writes != 1 {
		t.Fatalf("accept: link=%+v, writes=%d, err=%v", accepted, writes, err)
	}
	want := *before
	want.TermsAcceptedAt, want.ProgramStartDate = accepted.TermsAcceptedAt, currentDate
	if !reflect.DeepEqual(*accepted, want) || accepted.TermsAcceptedAt.Before(*accepted.SubmittedAt) ||
		!reflect.DeepEqual(studentsFor(t, db, ctx, entry.Token), enrolled) {
		t.Fatalf("acceptance changed submission or enrollment: link=%+v, want=%+v", accepted, want)
	}
	if persisted, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(persisted, accepted) {
		t.Fatalf("acceptance not persisted: link=%+v, err=%v", persisted, err)
	}
	if err := options.SetProgramStartDate(ctx, "2027-01-05", staff); err != nil {
		t.Fatal(err)
	}
	if _, err := students.New(db, nil).UpdateField(ctx, enrolled[0].ID, "name", "Canonical contact"); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{currentDate, "2027-01-05", "", "invalid replay"} {
		if got, err := store.AcceptTerms(ctx, entry.Token, shown); err != nil || !reflect.DeepEqual(got, accepted) || writes != 1 {
			t.Fatalf("accepted replay changed agreement: link=%+v, writes=%d, err=%v", got, writes, err)
		}
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Replacement contact")); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted replay: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.SaveDraft(ctx, entry.Token, "name", "Replacement draft"); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted draft: saved=%v, err=%v", saved, err)
	}
	if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, accepted) || writes != 1 {
		t.Fatalf("contact replay changed agreement: link=%+v, writes=%d, err=%v", after, writes, err)
	}
}

func TestAcceptTermsRejectsUnconfiguredStartDate(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact")); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	for _, stage := range []string{"missing settings row", "empty date"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "empty date" {
				if err := settings.New(db).SetLinkLifetimeDays(ctx, 7, staff); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10"); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("unconfigured date accepted: link=%+v, err=%v", got, err)
			}
		})
	}
}

func TestAcceptTermsObservesConcurrentStartDateChange(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact")); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	blocker := db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	acceptCtx, cancelAccept := context.WithCancel(ctx)
	type result struct {
		link *Link
		err  error
	}
	var results chan result
	defer func() {
		blocker.Rollback()
		cancelAccept()
		if results != nil {
			for range results {
			}
		}
	}()
	var blockerPID int
	if err := blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error; err != nil {
		t.Fatal(err)
	}
	if err := settings.New(blocker).SetProgramStartDate(ctx, "2026-11-17", staff); err != nil {
		t.Fatal(err)
	}
	results = make(chan result, 1)
	go func() {
		defer close(results)
		link, err := store.AcceptTerms(acceptCtx, entry.Token, "2026-11-10")
		results <- result{link: link, err: err}
	}()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := db.WithContext(ctx).Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))",
			blockerPID,
		).Scan(&waiting).Error; err != nil {
			t.Fatalf("observe acceptance lock wait: %v", err)
		}
		if waiting {
			break
		}
		select {
		case got := <-results:
			t.Fatalf("acceptance bypassed pending date change: link=%+v, err=%v", got.link, got.err)
		case <-ctx.Done():
			t.Fatalf("acceptance did not wait on settings: %v", ctx.Err())
		case <-poll.C:
		}
	}
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.link != nil || !errors.Is(got.err, ErrProgramStartChanged) {
		t.Fatalf("acceptance ignored committed date change: link=%+v, err=%v", got.link, got.err)
	}
	if persisted, err := store.Lookup(ctx, entry.Token); err != nil || persisted == nil || persisted.TermsAcceptedAt != nil || persisted.ProgramStartDate != "" {
		t.Fatalf("stale agreement persisted: link=%+v, err=%v", persisted, err)
	}
}

func TestAgreementWritesRejectUnusableTokens(t *testing.T) {
	db, store, ctx := testStore(t)
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	var unusable []string
	for _, stage := range []string{"expired submission", "expired acceptance", "canceled acceptance"} {
		entry, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact")); err != nil || saved != Saved {
			t.Fatalf("submit: saved=%v, err=%v", saved, err)
		}
		if stage != "expired submission" {
			if got, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10"); err != nil || got == nil {
				t.Fatalf("accept: link=%+v, err=%v", got, err)
			}
		}
		column := "expires_at"
		if stage == "canceled acceptance" {
			column = "canceled_at"
		}
		if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digestOf(entry.Token)).Update(column, time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		unusable = append(unusable, entry.Token)
	}
	canceled, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Cancel(ctx, canceled.ID, staff); err != nil || got == nil || got.Status != Canceled {
		t.Fatalf("cancel unfinished contact: entry=%+v, err=%v", got, err)
	}
	unusable = append(unusable, canceled.Token)
	unusable = append(unusable, "", "malformed", strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)),
		base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)))
	var linksBefore []Link
	var studentsBefore []students.Student
	if err := db.WithContext(ctx).Order("token_hash").Find(&linksBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	store = New(db, func() { writes++ })
	for _, token := range unusable {
		if got, err := store.AcceptTerms(ctx, token, "2026-11-10"); err != nil || got != nil {
			t.Fatalf("unusable acceptance: link=%+v, err=%v", got, err)
		}
	}
	var linksAfter []Link
	var studentsAfter []students.Student
	if err := db.WithContext(ctx).Order("token_hash").Find(&linksAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if writes != 0 || !reflect.DeepEqual(linksAfter, linksBefore) || !reflect.DeepEqual(studentsAfter, studentsBefore) {
		t.Fatalf("unusable agreement writes changed retained rows: writes=%d", writes)
	}
}

func TestAgreementDatabaseFailuresDoNotAnnounceWrites(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact")); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	before, err := store.Lookup(ctx, entry.Token)
	if err != nil || before == nil {
		t.Fatalf("load submission: link=%+v, err=%v", before, err)
	}
	writes := 0
	store = New(db, func() { writes++ })
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := store.AcceptTerms(cancelled, entry.Token, "2026-11-10"); got != nil || err == nil {
		t.Fatalf("failed acceptance: link=%+v, err=%v", got, err)
	}
	if err := db.WithContext(ctx).Exec("ALTER TABLE links ADD CONSTRAINT test_acceptance_failure CHECK (terms_accepted_at IS NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if got, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10"); got != nil || err == nil {
		t.Fatalf("rejected acceptance: link=%+v, err=%v", got, err)
	}
	if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, before) || writes != 0 {
		t.Fatalf("failed acceptance changed submission: link=%+v, writes=%d, err=%v", after, writes, err)
	}
	if err := db.WithContext(ctx).Exec("ALTER TABLE links DROP CONSTRAINT test_acceptance_failure").Error; err != nil {
		t.Fatal(err)
	}
	accepted, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10")
	if err != nil || accepted == nil || writes != 1 {
		t.Fatalf("acceptance retry: link=%+v, writes=%d, err=%v", accepted, writes, err)
	}
}

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{}, &students.Student{}, &settings.Settings{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, New(db, nil), ctx
}

func listByID(t *testing.T, store *Store, ctx context.Context) map[string]Entry {
	t.Helper()
	entries, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	return byID
}

func digestOf(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func studentsFor(t *testing.T, db *gorm.DB, ctx context.Context, token string) []students.Student {
	t.Helper()
	var enrolled []students.Student
	if err := db.WithContext(ctx).Where("source_link_hash = ?", digestOf(token)).Find(&enrolled).Error; err != nil {
		t.Fatal(err)
	}
	return enrolled
}

func contactDetails(name string) students.Details {
	return students.Details{
		Name: name, Email: "student@example.com", Phone: "+17872345678", Municipality: "San Juan", DateOfBirth: "2005-06-15",
	}
}

func TestUpdateContact(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Unsubmitted link cannot be updated
	res, err := store.UpdateContact(ctx, entry.Token, contactDetails("Unsubmitted"))
	if err != nil || res != Unavailable {
		t.Fatalf("unsubmitted update = %v, %v; want Unavailable, nil", res, err)
	}

	// 2. Submit link
	initial := contactDetails("Initial Name")
	res, err = store.Submit(ctx, entry.Token, initial)
	if err != nil || res != Saved {
		t.Fatalf("submit = %v, %v; want Saved, nil", res, err)
	}

	// Accept terms to verify terms acceptance is preserved
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-01", staff); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.AcceptTerms(ctx, entry.Token, "2026-11-01")
	if err != nil || accepted.TermsAcceptedAt == nil {
		t.Fatalf("accept terms: %v, %v", accepted, err)
	}
	acceptedTermsAt := *accepted.TermsAcceptedAt
	submittedAt := *accepted.SubmittedAt
	expiresAt := accepted.ExpiresAt

	// 3. Update contact on submitted link
	updated := students.Details{
		Name:         "Updated Name",
		Email:        "updated@example.com",
		Phone:        "+17875559999",
		Municipality: "Ponce",
		DateOfBirth:  "2004-05-14",
	}
	res, err = store.UpdateContact(ctx, entry.Token, updated)
	if err != nil || res != Saved {
		t.Fatalf("update contact = %v, %v; want Saved, nil", res, err)
	}

	// Verify link row
	current, err := store.Lookup(ctx, entry.Token)
	if err != nil || current == nil {
		t.Fatalf("lookup: %v, %v", current, err)
	}
	if current.Name != updated.Name || current.Email != updated.Email ||
		current.Phone != updated.Phone || current.Municipality != updated.Municipality ||
		current.DateOfBirth != updated.DateOfBirth {
		t.Fatalf("link details = %+v, want %+v", current.Details, updated)
	}
	if !current.SubmittedAt.Equal(submittedAt) {
		t.Fatalf("submitted_at changed: %v, want %v", current.SubmittedAt, submittedAt)
	}
	if !current.TermsAcceptedAt.Equal(acceptedTermsAt) {
		t.Fatalf("terms_accepted_at changed: %v, want %v", current.TermsAcceptedAt, acceptedTermsAt)
	}
	if !current.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at changed: %v, want %v", current.ExpiresAt, expiresAt)
	}

	// Verify students row
	enrolled := studentsFor(t, db, ctx, entry.Token)
	if len(enrolled) != 1 {
		t.Fatalf("enrolled students count = %d, want 1", len(enrolled))
	}
	if enrolled[0].Name != updated.Name || enrolled[0].Email != updated.Email ||
		enrolled[0].Phone != updated.Phone || enrolled[0].Municipality != updated.Municipality ||
		enrolled[0].DateOfBirth != updated.DateOfBirth {
		t.Fatalf("student row = %+v, want %+v", enrolled[0].Details, updated)
	}

	// 4. Unknown or expired token returns Unavailable
	res, err = store.UpdateContact(ctx, strings.Repeat("a", 43), updated)
	if err != nil || res != Unavailable {
		t.Fatalf("unknown token = %v, %v; want Unavailable, nil", res, err)
	}
}
