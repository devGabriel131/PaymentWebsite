//go:build integration

package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"paymentwebsite/internal/testdb"
)

func TestLinkLifetime(t *testing.T) {
	db := testdb.Open(t, &Settings{})
	store := New(db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	requireLifetime := func(t *testing.T, days int, by string) {
		t.Helper()
		current, err := store.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current.LinkLifetimeDays != days || current.UpdatedBy != by {
			t.Fatalf("current = %d days by %q, want %d by %q", current.LinkLifetimeDays, current.UpdatedBy, days, by)
		}
	}

	requireLifetime(t, DefaultLinkLifetimeDays, "") // Nobody has saved yet.
	for _, days := range []int{MinLinkLifetimeDays, MaxLinkLifetimeDays} {
		if err := store.SetLinkLifetimeDays(ctx, days, "alice"); err != nil {
			t.Fatalf("set %d days: %v", days, err)
		}
		requireLifetime(t, days, "alice")
	}

	for _, tc := range []struct {
		name  string
		days  int
		staff string
		want  error
	}{
		{"below range", MinLinkLifetimeDays - 1, "bob", ErrLinkLifetimeRange},
		{"above range", MaxLinkLifetimeDays + 1, "bob", ErrLinkLifetimeRange},
		{"negative", -3, "bob", ErrLinkLifetimeRange},
		{"anonymous", DefaultLinkLifetimeDays, "", errNoStaff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetLinkLifetimeDays(ctx, tc.days, tc.staff); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			requireLifetime(t, MaxLinkLifetimeDays, "alice")
		})
	}

	t.Run("database rejects bypasses", func(t *testing.T) {
		if err := db.WithContext(ctx).Model(&Settings{}).Where("id = 1").Update("link_lifetime_days", 30).Error; err == nil {
			t.Error("stored a 30-day lifetime")
		}
		if err := db.WithContext(ctx).Create(&Settings{ID: 2, LinkLifetimeDays: 7, UpdatedBy: "bob"}).Error; err == nil {
			t.Error("stored a second settings row")
		}
		requireLifetime(t, MaxLinkLifetimeDays, "alice")
	})
}
