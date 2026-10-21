//go:build integration

package students_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

func TestListRetainsExpiredSourcesAndOrdersNewestFirst(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	if listed, err := roster.List(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("empty roster = %+v, %v", listed, err)
	}

	_, oldest := enroll(t, db, sources, ctx, "Same name")
	_, firstTie := enroll(t, db, sources, ctx, "Same name")
	_, lastTie := enroll(t, db, sources, ctx, "Same name")
	oldTime := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	newTime := oldTime.Add(time.Hour)
	for _, fixture := range []struct {
		student students.Student
		at      time.Time
	}{{oldest, oldTime}, {firstTie, newTime}, {lastTie, newTime}} {
		if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", fixture.student.ID).
			Update("created_at", fixture.at).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.WithContext(ctx).Model(&links.Link{}).Where("token_hash = ?", fixture.student.SourceLinkHash).
			Updates(map[string]any{"submitted_at": fixture.at, "expires_at": fixture.at.Add(168 * time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}

	listed, err := roster.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []uint64{lastTie.ID, firstTie.ID, oldest.ID}
	if len(listed) != len(wantIDs) {
		t.Fatalf("retained %d students, want %d", len(listed), len(wantIDs))
	}
	for i, id := range wantIDs {
		if listed[i].ID != id || listed[i].Name != "Same name" {
			t.Fatalf("row %d = %+v, want student %d", i, listed[i], id)
		}
		got, err := roster.Get(ctx, id)
		if err != nil || got == nil || !reflect.DeepEqual(*got, listed[i]) {
			t.Fatalf("Get(%d) = %+v, %v, want listed student %+v", id, got, err, listed[i])
		}
	}
	entries, err := sources.List(ctx)
	if err != nil || len(entries) != 3 {
		t.Fatalf("source history = %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Status != links.Finished || !entry.AccessExpired() {
			t.Fatalf("source %+v is not an expired completion", entry)
		}
	}
	for _, id := range []uint64{0, lastTie.ID + 1, 1 << 63, ^uint64(0)} {
		if got, err := roster.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("missing Get(%d) = %+v, %v", id, got, err)
		}
	}
}

func TestUpdateFieldChangesOnlyCanonicalStudentName(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Original submission")
	_, other := enroll(t, db, sources, ctx, "Other student")
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"Unicode and apostrophe", " \tMaría O'Connor 李\u2003", "María O'Connor 李"},
		{"markup remains data", `"><script>alert('x')</script> & 李`, `"><script>alert('x')</script> & 李`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := roster.UpdateField(ctx, original.ID, "name", tc.raw)
			want := original
			want.Name = tc.want
			if err != nil || updated == nil || !reflect.DeepEqual(*updated, want) {
				t.Fatalf("UpdateField = %+v, %v, want %+v", updated, err, want)
			}
			persisted, err := roster.Get(ctx, original.ID)
			if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) {
				t.Fatalf("persisted student = %+v, %v, want %+v", persisted, err, want)
			}
			unrelated, err := roster.Get(ctx, other.ID)
			if err != nil || unrelated == nil || !reflect.DeepEqual(*unrelated, other) {
				t.Fatalf("other student changed: %+v, %v", unrelated, err)
			}
			var sourceAfter links.Link
			if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sourceBefore, sourceAfter) {
				t.Fatalf("immutable source changed: before %+v, after %+v", sourceBefore, sourceAfter)
			}
		})
	}
}

func TestUpdateFieldRejectsUnknownFieldsAndInvalidNamesWithoutWrites(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Unchanged")
	for _, key := range []string{"", "Name", "id", "source_link_hash", "created_at", "name = 'changed'; DROP TABLE students; --"} {
		t.Run("unknown field "+key, func(t *testing.T) {
			if got, err := roster.UpdateField(ctx, original.ID, key, "Changed"); got != nil || !errors.Is(err, students.ErrUnknownField) {
				t.Fatalf("UpdateField(%q) = %+v, %v, want ErrUnknownField", key, got, err)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		raw     string
		message string
	}{
		{"empty", "", "Enter a name."},
		{"Unicode whitespace", " \t\u2003\n", "Enter a name."},
		{"invalid UTF8", "Ana\xff", "Enter a valid name."},
		{"NUL", "Ana\x00", "Enter a valid name."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roster.UpdateField(ctx, original.ID, "name", tc.raw)
			var invalid *students.ValidationError
			if got != nil || !errors.As(err, &invalid) || invalid.Message != tc.message {
				t.Fatalf("UpdateField = %+v, %v, want ValidationError %q", got, err, tc.message)
			}
		})
	}
	for _, id := range []uint64{0, original.ID + 1, 1 << 63, ^uint64(0)} {
		if got, err := roster.UpdateField(ctx, id, "name", "Missing student"); got != nil || err != nil {
			t.Fatalf("missing UpdateField(%d) = %+v, %v", id, got, err)
		}
	}
	persisted, err := roster.Get(ctx, original.ID)
	if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, original) {
		t.Fatalf("rejected edits changed student: %+v, %v", persisted, err)
	}
	listed, err := roster.List(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("rejected edits changed roster: %+v, %v", listed, err)
	}
}

func TestStorePropagatesDatabaseFailuresAfterFieldValidation(t *testing.T) {
	db, _, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Unchanged")
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	sqlLogger := logger.New(log.New(&logged, "", 0), logger.Config{LogLevel: logger.Info})
	failed := students.New(tx.Session(&gorm.Session{Logger: sqlLogger}))
	if got, err := failed.List(ctx); got != nil || err == nil {
		t.Fatalf("failed List = %+v, %v", got, err)
	}
	if got, err := failed.Get(ctx, original.ID); got != nil || err == nil {
		t.Fatalf("failed Get = %+v, %v", got, err)
	}
	const privateName = "Private student value"
	if got, err := failed.UpdateField(ctx, original.ID, "name", privateName); got != nil || err == nil {
		t.Fatalf("failed UpdateField = %+v, %v", got, err)
	}
	if bytes.Contains(logged.Bytes(), []byte(privateName)) {
		t.Fatal("failed field update leaked submitted contents into SQL logs")
	}
	if got, err := failed.Get(ctx, 0); got != nil || err != nil {
		t.Fatalf("zero Get queried failed database: %+v, %v", got, err)
	}
	if got, err := failed.UpdateField(ctx, original.ID, "name; DROP TABLE students", "Valid name"); got != nil || !errors.Is(err, students.ErrUnknownField) {
		t.Fatalf("unknown field was not rejected before SQL: %+v, %v", got, err)
	}
	got, err := failed.UpdateField(ctx, original.ID, "name", " \u2003 ")
	var invalid *students.ValidationError
	if got != nil || !errors.As(err, &invalid) {
		t.Fatalf("invalid value was not rejected before SQL: %+v, %v", got, err)
	}
}

func studentStores(t *testing.T) (*gorm.DB, *students.Store, *links.Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, students.New(db), links.New(db), ctx
}

func enroll(t *testing.T, db *gorm.DB, sources *links.Store, ctx context.Context, name string) (links.Entry, students.Student) {
	t.Helper()
	entry, err := sources.Create(ctx, "student-test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := sources.Submit(ctx, entry.Token, name); err != nil || !saved {
		t.Fatalf("enrollment saved=%v, err=%v", saved, err)
	}
	digest, err := hex.DecodeString(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	var student students.Student
	if err := db.WithContext(ctx).Where("source_link_hash = ?", digest).Take(&student).Error; err != nil {
		t.Fatal(err)
	}
	return entry, student
}
