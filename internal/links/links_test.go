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
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paymentwebsite/internal/testdb"
)

var (
	testKey  = bytes.Repeat([]byte{1}, 32)
	wrongKey = bytes.Repeat([]byte{2}, 32)
)

func TestIssuedTokenBoundary(t *testing.T) {
	db, store, ctx := testStore(t)

	first, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("issued token is stored by digest", func(t *testing.T) {
		var record Link
		if err := db.WithContext(ctx).Where(&Link{TokenHash: digestOf(first.Token)}).First(&record).Error; err != nil {
			t.Fatalf("issued token has no persisted SHA-256 digest: %v", err)
		}
		if first.ID != hex.EncodeToString(record.TokenHash) {
			t.Fatalf("entry ID %q does not name the stored digest", first.ID)
		}
		if bytes.Contains(record.TokenCiphertext, []byte(first.Token)) {
			t.Fatal("issued token is stored in plaintext")
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
			link, err := store.Get(ctx, tc.token)
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
		entry, err := store.Create(cancelled)
		if err == nil || entry != (Entry{}) {
			t.Fatal("cancelled database write must return an error and no usable link")
		}
	})
}

func TestListRecoversIssuedLinks(t *testing.T) {
	db, store, ctx := testStore(t)
	issued := make([]Entry, 3)
	for i := range issued {
		entry, err := store.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		issued[i] = entry
	}
	const name = "Ana Pérez"
	if saved, err := store.SaveName(ctx, issued[1].Token, name); err != nil || !saved {
		t.Fatalf("save issued link's name: saved=%v, err=%v", saved, err)
	}
	issued[1].Name = name

	// A fresh Store over the same database and key models a server restart.
	listed, err := newStore(t, db, testKey).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(listed, issued) {
		t.Fatalf("listed %+v, want %+v", listed, issued)
	}
}

func TestListRejectsUnreadableLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
		// replace returns the first link's new stored ciphertext; nil keeps storage intact.
		replace func(own, other []byte) []byte
	}{
		{"wrong key", wrongKey, nil},
		{"modified ciphertext", testKey, func(own, _ []byte) []byte {
			modified := bytes.Clone(own)
			modified[len(modified)-1] ^= 1
			return modified
		}},
		{"ciphertext moved from another link", testKey, func(_, other []byte) []byte { return other }},
		{"empty ciphertext is not a legacy link", testKey, func(_, _ []byte) []byte { return []byte{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, ctx := testStore(t)
			var records [2]Link
			for i := range records {
				entry, err := store.Create(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.WithContext(ctx).Where(&Link{TokenHash: digestOf(entry.Token)}).First(&records[i]).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.replace != nil {
				ciphertext := tc.replace(records[0].TokenCiphertext, records[1].TokenCiphertext)
				if err := db.WithContext(ctx).Model(&records[0]).Update("token_ciphertext", ciphertext).Error; err != nil {
					t.Fatal(err)
				}
			}

			listed, err := newStore(t, db, tc.key).List(ctx)
			if err == nil || listed != nil {
				t.Fatalf("unreadable link must fail the whole list: listed=%+v, err=%v", listed, err)
			}
		})
	}
}

func TestLegacyLinksSurviveMigration(t *testing.T) {
	// The links schema before encrypted recovery.
	type legacyLink struct {
		TokenHash []byte `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
		Name      string `gorm:"not null;default:''"`
	}
	db := testdb.Open(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.WithContext(ctx).Table("links").AutoMigrate(&legacyLink{}); err != nil {
		t.Fatal(err)
	}
	var random [tokenBytes]byte
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	if err := db.WithContext(ctx).Table("links").Create(&legacyLink{TokenHash: digestOf(token), Name: "Existing customer"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.WithContext(ctx).AutoMigrate(&Link{}); err != nil {
		t.Fatal(err)
	}
	store := newStore(t, db, testKey)
	fresh, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const renamed = "Renamed customer"
	if saved, err := store.SaveName(ctx, token, renamed); err != nil || !saved {
		t.Fatalf("original URL stopped accepting names: saved=%v, err=%v", saved, err)
	}
	if link, err := store.Get(ctx, token); err != nil || link == nil || link.Name != renamed {
		t.Fatalf("original URL stopped working: link=%+v, err=%v", link, err)
	}
	legacy := Entry{ID: hex.EncodeToString(digestOf(token)), Name: renamed}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{legacy, fresh}; !slices.Equal(listed, want) {
		t.Fatalf("listed %+v, want %+v", listed, want)
	}

	if err := store.Cancel(ctx, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if link, err := store.Get(ctx, token); err != nil || link != nil {
		t.Fatalf("canceled legacy link remains usable: link=%+v, err=%v", link, err)
	}
	if link, err := store.Get(ctx, fresh.Token); err != nil || link == nil {
		t.Fatalf("canceling a legacy link revoked another: link=%+v, err=%v", link, err)
	}
}

func TestCustomerNameIsolationAndRevocation(t *testing.T) {
	_, store, ctx := testStore(t)
	first, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const name = "María O'Connor 李"
	saved, err := store.SaveName(ctx, first.Token, name)
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
			link, err := store.Get(ctx, tc.token)
			if err != nil || link == nil {
				t.Fatalf("get issued link: link=%v, err=%v", link, err)
			}
			if link.Name != tc.wantName {
				t.Fatalf("name = %q, want %q", link.Name, tc.wantName)
			}
		})
	}

	if err := store.CancelAll(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err = store.SaveName(ctx, first.Token, "Should not be saved")
	if err != nil || saved {
		t.Fatalf("canceled link accepted a name: saved=%v, err=%v", saved, err)
	}
	link, err := store.Get(ctx, first.Token)
	if err != nil || link != nil {
		t.Fatalf("name submission revived a canceled link: link=%v, err=%v", link, err)
	}
}

func TestCancel(t *testing.T) {
	_, store, ctx := testStore(t)
	var entries [2]Entry
	for i := range entries {
		entry, err := store.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	const name = "Still active"
	if saved, err := store.SaveName(ctx, entries[1].Token, name); err != nil || !saved {
		t.Fatalf("save other link's name: saved=%v, err=%v", saved, err)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"issued link", entries[0].ID},
		{"already canceled link", entries[0].ID},
		{"unknown link", strings.Repeat("0", hex.EncodedLen(sha256.Size))},
		{"malformed ID", "not-issued"},
		{"customer token is not an ID", entries[1].Token},
		{"SQL-like input", strings.Repeat("'", hex.EncodedLen(sha256.Size))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.Cancel(ctx, tc.id); err != nil {
				t.Fatal(err)
			}
		})
	}

	canceled, err := store.Get(ctx, entries[0].Token)
	if err != nil || canceled != nil {
		t.Fatalf("canceled link remains usable: link=%v, err=%v", canceled, err)
	}
	active, err := store.Get(ctx, entries[1].Token)
	if err != nil || active == nil {
		t.Fatalf("canceling one link revoked another: link=%v, err=%v", active, err)
	}
	if active.Name != name {
		t.Fatalf("other link's name = %q, want %q", active.Name, name)
	}
	if saved, err := store.SaveName(ctx, entries[0].Token, "Must not revive"); err != nil || saved {
		t.Fatalf("canceled link accepted a name: saved=%v, err=%v", saved, err)
	}
}

func TestCancelAll(t *testing.T) {
	db, store, ctx := testStore(t)
	var previous [2]Entry
	for i := range previous {
		entry, err := store.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		previous[i] = entry
	}

	// Repeated cancellation must remain safe when there are no links left.
	for range 2 {
		if err := store.CancelAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.WithContext(ctx).Delete(&Link{}).Error; !errors.Is(err, gorm.ErrMissingWhereClause) {
		t.Fatalf("bulk cancellation disabled the shared database's delete guard: %v", err)
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
			link, err := store.Get(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid := link != nil; valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, newStore(t, db, testKey), ctx
}

func newStore(t *testing.T, db *gorm.DB, key []byte) *Store {
	t.Helper()
	store, err := New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func digestOf(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}
