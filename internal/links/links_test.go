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

	"paymentwebsite/internal/auth"
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
		})
	}

	t.Run("failed persistence does not issue a token", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		entry, err := store.Create(cancelled, staff, later)
		if err == nil || entry != (Entry{}) {
			t.Fatal("cancelled database write must return an error and no usable link")
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
	const name = "Ana Pérez"
	refused := func(save func(context.Context, string, string) (bool, error)) func() error {
		return func() error {
			if saved, err := save(ctx, link.Token, "Changed after submit"); err != nil || saved {
				return fmt.Errorf("submitted form accepted a change: saved=%v, err=%v", saved, err)
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

	steps := []struct {
		name   string
		act    func() error
		status Status
		saved  string
	}{
		{"issued", func() error { return nil }, NotOpened, ""},
		{"looked up before opening", lookedUp, NotOpened, ""},
		{"opened", func() error { _, err := store.Open(ctx, link.Token); return err }, Opened, ""},
		{"reopened", func() error { _, err := store.Open(ctx, link.Token); return err }, Opened, ""},
		{"autosaved", func() error { _, err := store.SaveDraft(ctx, link.Token, "Ana"); return err }, InProgress, "Ana"},
		{"reopened draft", func() error { _, err := store.Open(ctx, link.Token); return err }, InProgress, "Ana"},
		{"submitted", func() error { _, err := store.Submit(ctx, link.Token, name); return err }, Finished, name},
		{"looked up after submit", lookedUp, Finished, name},
		{"autosave after submit is refused", refused(store.SaveDraft), Finished, name},
		{"second submit is refused", refused(store.Submit), Finished, name},
		{"cancel cannot revoke completion", func() error { _, err := store.Cancel(ctx, link.ID, staff); return err }, Finished, name},
		{"finished link still opens success", lookedUp, Finished, name},
	}
	// The first visit is recorded once; no later step may move it.
	var firstVisit *time.Time
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
				if len(enrolled) != 1 || enrolled[0].Name != name || got.SubmittedAt == nil ||
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
}

func TestMigrateCutsOverOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  bool
	}{
		{"fresh schema", false},
		{"old mock schema", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Open(t, &auth.User{}, &auth.Session{}, &settings.Settings{})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
			user := auth.User{Username: "retained", PasswordHash: "password sentinel", AuthVersion: "version sentinel"}
			session := auth.Session{Token: "session sentinel", Data: []byte("session data"), Expiry: at.Add(time.Hour)}
			config := settings.Settings{ID: 1, LinkLifetimeDays: 12, UpdatedAt: at, UpdatedBy: user.Username}
			for _, sentinel := range []any{&user, &session, &config} {
				if err := db.WithContext(ctx).Create(sentinel).Error; err != nil {
					t.Fatal(err)
				}
			}
			oldToken := base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))
			if tc.old {
				// Pre-enrollment mock data, including the removed recoverable token.
				if err := db.WithContext(ctx).Exec(`CREATE TABLE links (
					token_hash bytea PRIMARY KEY, name text NOT NULL,
					token_ciphertext bytea, created_at timestamptz NOT NULL
				)`).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.WithContext(ctx).Exec(
					"INSERT INTO links (token_hash, name, token_ciphertext, created_at) VALUES (decode(?, 'hex'), ?, decode(?, 'hex'), ?)",
					hex.EncodeToString(digestOf(oldToken)), "Old mock submission", hex.EncodeToString([]byte("sealed token")), at,
				).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(db.WithContext(ctx)); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasTable(&Link{}) || !db.Migrator().HasTable(&students.Student{}) ||
				db.Migrator().HasColumn(&Link{}, "token_ciphertext") {
				t.Fatal("cutover did not replace the mock schema with links and students")
			}
			store := New(db)
			if old, err := store.Lookup(ctx, oldToken); err != nil || old != nil {
				t.Fatalf("old mock URL survived cutover: link=%+v, err=%v", old, err)
			}
			if records := listByID(t, store, ctx); len(records) != 0 {
				t.Fatalf("cutover kept mock links: %+v", records)
			}
			var initialStudents int64
			if err := db.WithContext(ctx).Model(&students.Student{}).Count(&initialStudents).Error; err != nil || initialStudents != 0 {
				t.Fatalf("cutover enrolled mock submissions: count=%d, err=%v", initialStudents, err)
			}
			draft, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := store.SaveDraft(ctx, draft.Token, "Retained draft"); err != nil || !saved {
				t.Fatalf("save draft: saved=%v, err=%v", saved, err)
			}
			finished, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, finished.Token, "Retained student"); err != nil || !saved {
				t.Fatalf("submit: saved=%v, err=%v", saved, err)
			}
			before := listByID(t, store, ctx)
			enrolled := studentsFor(t, db, ctx, finished.Token)
			if len(enrolled) != 1 {
				t.Fatalf("completion created %d students, want one", len(enrolled))
			}
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(finished.Token)).Delete(&Link{}).Error; err == nil {
				t.Fatal("student enrollment did not restrict deletion of its source link")
			}
			if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digestOf(finished.Token)).
				Update("token_hash", digestOf(oldToken)).Error; err == nil {
				t.Fatal("student enrollment did not restrict changing its source link hash")
			}
			for range 2 {
				if err := Migrate(db.WithContext(ctx)); err != nil {
					t.Fatal(err)
				}
				if after := listByID(t, store, ctx); !reflect.DeepEqual(after, before) {
					t.Fatalf("repeat migration changed links: before=%+v, after=%+v", before, after)
				}
				if after := studentsFor(t, db, ctx, finished.Token); !reflect.DeepEqual(after, enrolled) {
					t.Fatalf("repeat migration changed enrollment: before=%+v, after=%+v", enrolled, after)
				}
				var gotUser auth.User
				var gotSession auth.Session
				var gotConfig settings.Settings
				for _, sentinel := range []any{&gotUser, &gotSession, &gotConfig} {
					if err := db.WithContext(ctx).Take(sentinel).Error; err != nil {
						t.Fatal(err)
					}
				}
				if gotUser != user || gotSession.Token != session.Token || !bytes.Equal(gotSession.Data, session.Data) ||
					!gotSession.Expiry.Equal(session.Expiry) || gotConfig.ID != config.ID ||
					gotConfig.LinkLifetimeDays != config.LinkLifetimeDays || gotConfig.UpdatedBy != config.UpdatedBy ||
					!gotConfig.UpdatedAt.Equal(config.UpdatedAt) {
					t.Fatalf("migration changed auth/session/settings sentinels: user=%+v, session=%+v, settings=%+v",
						gotUser, gotSession, gotConfig)
				}
			}
		})
	}
}

func TestConcurrentSubmissionEnrollsOnce(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, issued.Token, "Draft only"); err != nil || !saved {
		t.Fatalf("draft: saved=%v, err=%v", saved, err)
	}
	if enrolled := studentsFor(t, db, ctx, issued.Token); len(enrolled) != 0 {
		t.Fatalf("draft enrolled students: %+v", enrolled)
	}
	type result struct {
		name  string
		saved bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, name := range []string{"First final answer", "Second final answer"} {
		go func() {
			<-start
			saved, err := store.Submit(ctx, issued.Token, name)
			results <- result{name: name, saved: saved, err: err}
		}()
	}
	close(start)
	var winner string
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.saved {
			if winner != "" {
				t.Fatal("competing submissions both succeeded")
			}
			winner = got.name
		}
	}
	completed, err := store.Lookup(ctx, issued.Token)
	enrolled := studentsFor(t, db, ctx, issued.Token)
	if err != nil || winner == "" || completed == nil || completed.Name != winner || completed.SubmittedAt == nil ||
		len(enrolled) != 1 || enrolled[0].Name != winner || !bytes.Equal(enrolled[0].SourceLinkHash, completed.TokenHash) ||
		!enrolled[0].CreatedAt.Equal(*completed.SubmittedAt) || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatalf("completion not atomic: winner=%q, link=%+v, students=%+v, err=%v", winner, completed, enrolled, err)
	}
	before := listByID(t, store, ctx)[issued.ID]
	for _, save := range []func(context.Context, string, string) (bool, error){store.SaveDraft, store.Submit} {
		if saved, err := save(ctx, issued.Token, "Replacement"); err != nil || saved {
			t.Fatalf("finished form accepted replacement: saved=%v, err=%v", saved, err)
		}
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
			const name = "Rejected submission"
			if tc.conflict {
				existing, err := store.Create(ctx, staff, later)
				if err != nil {
					t.Fatal(err)
				}
				if saved, err := store.Submit(ctx, existing.Token, name); err != nil || !saved {
					t.Fatalf("seed existing student: saved=%v, err=%v", saved, err)
				}
			}
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := store.SaveDraft(ctx, issued.Token, "Original draft"); err != nil || !saved {
				t.Fatalf("save draft: saved=%v, err=%v", saved, err)
			}
			before, err := store.Lookup(ctx, issued.Token)
			if err != nil || before == nil {
				t.Fatalf("load draft: link=%+v, err=%v", before, err)
			}
			// Constraint SQL is fixed test data, scoped to this test's schema.
			if err := db.WithContext(ctx).Exec("ALTER TABLE students ADD CONSTRAINT test_student_rejection " + tc.constraint).Error; err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, issued.Token, name); err == nil || saved {
				t.Fatalf("failed transaction reported success: saved=%v, err=%v", saved, err)
			}
			after, err := store.Lookup(ctx, issued.Token)
			if err != nil || !reflect.DeepEqual(after, before) || len(studentsFor(t, db, ctx, issued.Token)) != 0 {
				t.Fatalf("failed transaction changed draft/submission/expiry: before=%+v, after=%+v, err=%v", before, after, err)
			}
			if err := db.WithContext(ctx).Exec("ALTER TABLE students DROP CONSTRAINT test_student_rejection").Error; err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, issued.Token, name); err != nil || !saved {
				t.Fatalf("retry failed: saved=%v, err=%v", saved, err)
			}
			completed, err := store.Lookup(ctx, issued.Token)
			enrolled := studentsFor(t, db, ctx, issued.Token)
			if err != nil || completed == nil || completed.Name != name || completed.SubmittedAt == nil ||
				len(enrolled) != 1 || enrolled[0].Name != name ||
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
	if saved, err := store.SaveDraft(ctx, issued.Token, "Original draft"); err != nil || !saved {
		t.Fatalf("draft: saved=%v, err=%v", saved, err)
	}
	type result struct {
		saved bool
		entry *Entry
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		saved, err := store.Submit(ctx, issued.Token, "Final answer")
		results <- result{saved: saved, err: err}
	}()
	go func() {
		<-start
		entry, err := store.Cancel(ctx, issued.ID, "canceler")
		results <- result{entry: entry, err: err}
	}()
	close(start)
	var submitted bool
	var canceled *Entry
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		submitted = submitted || got.saved
		if got.entry != nil {
			canceled = got.entry
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
		if !submitted || row.CanceledAt != nil || row.CanceledBy != "" || row.Name != "Final answer" ||
			len(enrolled) != 1 || enrolled[0].Name != row.Name || !enrolled[0].CreatedAt.Equal(*row.SubmittedAt) ||
			canceled.Status != Finished || row.ExpiresAt.Sub(*row.SubmittedAt) != 168*time.Hour {
			t.Fatalf("invalid submission-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	case row.CanceledAt != nil:
		if submitted || row.CanceledBy != "canceler" || row.Name != "Original draft" ||
			len(enrolled) != 0 || canceled.Status != Canceled || row.ExpiresAt.Sub(later).Abs() > time.Microsecond {
			t.Fatalf("invalid cancellation-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	default:
		t.Fatalf("race left neither completion nor cancellation: %+v", row)
	}
}

func TestCustomerNameIsolationAndRevocation(t *testing.T) {
	db, store, ctx := testStore(t)
	first, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	const name = "María O'Connor 李"
	saved, err := store.Submit(ctx, first.Token, name)
	if err != nil || !saved {
		t.Fatalf("save issued link's name: saved=%v, err=%v", saved, err)
	}
	for _, tc := range []struct {
		name     string
		token    string
		wantName string
	}{
		{"submitted link", first.Token, name},
		{"separate link", second.Token, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := store.Open(ctx, tc.token)
			if err != nil || link == nil {
				t.Fatalf("open issued link: link=%v, err=%v", link, err)
			}
			if link.Name != tc.wantName {
				t.Fatalf("name = %q, want %q", link.Name, tc.wantName)
			}
		})
	}

	if err := store.CancelAll(ctx, staff); err != nil {
		t.Fatal(err)
	}
	saved, err = store.Submit(ctx, first.Token, "Should not be saved")
	if err != nil || saved {
		t.Fatalf("finished link accepted another name: saved=%v, err=%v", saved, err)
	}
	link, err := store.Open(ctx, first.Token)
	if err != nil || link == nil || link.Name != name {
		t.Fatalf("bulk cancellation changed the completed link: link=%+v, err=%v", link, err)
	}
	if got := listByID(t, store, ctx)[first.ID]; got.Name != name || got.Status != Finished || got.CanceledBy != "" {
		t.Fatalf("bulk cancellation changed completion: %+v", got)
	}
	if enrolled := studentsFor(t, db, ctx, first.Token); len(enrolled) != 1 || enrolled[0].Name != name {
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
		if ok, err := store.SaveDraft(ctx, entries[i].Token, saved); err != nil || !ok {
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
	if saved, err := store.SaveDraft(ctx, entries[0].Token, "Must not revive"); err != nil || saved {
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
	if saved, err := store.SaveDraft(ctx, midForm.Token, "Half done"); err != nil || !saved {
		t.Fatalf("draft before expiry: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.Submit(ctx, finished.Token, "Done"); err != nil || !saved {
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
			if saved, err := store.SaveDraft(ctx, token, "After expiry"); err != nil || saved {
				t.Errorf("draft: saved=%v, err=%v; want refused", saved, err)
			}
			if saved, err := store.Submit(ctx, token, "After expiry"); err != nil || saved {
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
		{"finished stays finished", finished, Finished, "Done"},
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

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{}, &students.Student{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, New(db), ctx
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
