//go:build integration

package links

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestIssuedTokenBoundary(t *testing.T) {
	tx, ctx := testDatabase(t)
	store := New(tx)

	first, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("only the token digest is persisted", func(t *testing.T) {
		digest := sha256.Sum256([]byte(first))
		var record Link
		if err := tx.Where(&Link{TokenHash: digest[:]}).First(&record).Error; err != nil {
			t.Fatalf("issued token has no persisted SHA-256 digest: %v", err)
		}
	})

	tampered := first[:len(first)-1] + "A"
	if tampered == first {
		tampered = first[:len(first)-1] + "B"
	}
	cases := []struct {
		name  string
		token string
		valid bool
	}{
		{"earlier link remains valid", first, true},
		{"newer link is valid", second, true},
		{"unissued token", base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)), false},
		{"tampered token", tampered, false},
		{"missing token", "", false},
		{"SQL-like input", strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, err := store.Exists(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}

	t.Run("failed persistence does not issue a token", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		token, err := store.Create(cancelled)
		if err == nil || token != "" {
			t.Fatal("cancelled database write must return an error and no usable token")
		}
	})
}

func TestCancelAll(t *testing.T) {
	tx, ctx := testDatabase(t)
	store := New(tx)
	var previous [2]string
	for i := range previous {
		token, err := store.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		previous[i] = token
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

	if err := tx.Delete(&Link{}).Error; !errors.Is(err, gorm.ErrMissingWhereClause) {
		t.Fatalf("bulk cancellation disabled the shared database's delete guard: %v", err)
	}
	cases := []struct {
		name  string
		token string
		valid bool
	}{
		{"first canceled link", previous[0], false},
		{"second canceled link", previous[1], false},
		{"new link after cancellation", fresh, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, err := store.Exists(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func testDatabase(t *testing.T) (*gorm.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated PostgreSQL test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := sqlDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&Link{}); err != nil {
		t.Fatal(err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx, ctx
}
