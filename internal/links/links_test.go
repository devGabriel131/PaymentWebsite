//go:build integration

package links

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paymentwebsite/internal/settings"
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
		{"canceled keeps data", func() error { _, err := store.Cancel(ctx, link.ID, staff); return err }, Canceled, name},
		{"canceled link cannot be reopened", func() error { _, err := store.Open(ctx, link.Token); return err }, Canceled, name},
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
		}) {
			return
		}
	}
}

func TestMigrateUpgradesExistingLinks(t *testing.T) {
	// The links schema with the removed encrypted URL recovery.
	type recoveryLink struct {
		TokenHash       []byte `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
		Name            string `gorm:"not null;default:''"`
		TokenCiphertext []byte
		CreatedAt       time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	}
	db := testdb.Open(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.WithContext(ctx).Table("links").AutoMigrate(&recoveryLink{}); err != nil {
		t.Fatal(err)
	}
	var random [tokenBytes]byte
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	existing := recoveryLink{TokenHash: digestOf(token), Name: "Existing customer", TokenCiphertext: []byte("sealed token")}
	if err := db.WithContext(ctx).Table("links").Create(&existing).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 { // Every server start migrates; repeating must be safe.
		if err := Migrate(db.WithContext(ctx)); err != nil {
			t.Fatal(err)
		}
	}
	if db.Migrator().HasColumn(&Link{}, "token_ciphertext") {
		t.Fatal("migration kept the recoverable token column")
	}
	store := New(db)
	if link, err := store.Open(ctx, token); err != nil || link == nil || link.Name != existing.Name {
		t.Fatalf("existing URL stopped working: link=%+v, err=%v", link, err)
	}
	backfilled := listByID(t, store, ctx)[hex.EncodeToString(existing.TokenHash)]
	if backfilled.Status != Finished {
		t.Fatalf("pre-autosave named link status = %q, want %q", backfilled.Status, Finished)
	}
	if lifetime := backfilled.ExpiresAt.Sub(backfilled.CreatedAt); lifetime != settings.DefaultLinkLifetimeDays*24*time.Hour {
		t.Fatalf("pre-expiry link lifetime = %v, want the %d-day default", lifetime, settings.DefaultLinkLifetimeDays)
	}

	// Backfills run only when their column is added; later restarts must
	// not turn drafts into submissions or move expiries.
	draft, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, draft.Token, "Draft only"); err != nil || !saved {
		t.Fatalf("save draft: saved=%v, err=%v", saved, err)
	}
	if err := Migrate(db.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	got := listByID(t, store, ctx)[draft.ID]
	if got.Status != InProgress || got.ExpiresAt.Sub(later).Abs() > time.Millisecond {
		t.Fatalf("restart changed draft: status %q, expires %v; want %q, %v", got.Status, got.ExpiresAt, InProgress, later)
	}
}

func TestCustomerNameIsolationAndRevocation(t *testing.T) {
	_, store, ctx := testStore(t)
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
		t.Fatalf("canceled link accepted a name: saved=%v, err=%v", saved, err)
	}
	link, err := store.Open(ctx, first.Token)
	if err != nil || link != nil {
		t.Fatalf("name submission revived a canceled link: link=%v, err=%v", link, err)
	}
	if got := listByID(t, store, ctx)[first.ID]; got.Name != name {
		t.Fatalf("cancellation changed submitted name to %q", got.Name)
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
	// Both lapse while the customer could still be on the page.
	if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash IN ?", [][]byte{digestOf(midForm.Token), digestOf(finished.Token)}).
		Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"expired at issue": issuedExpired.Token, "mid-form": midForm.Token} {
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
		if got.Status != tc.status || got.Name != tc.saved || got.CanceledBy != "" || got.Cancelable() {
			t.Errorf("%s: listed %+v; want status %q, name %q, not canceled or cancelable", tc.name, got, tc.status, tc.saved)
		}
	}
}

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{})
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
