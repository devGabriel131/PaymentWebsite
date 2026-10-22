//go:build integration

package students_test

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

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
		if !fixture.student.IsActive {
			t.Fatalf("new student inactive: %+v", fixture.student)
		}
		if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", fixture.student.ID).
			Update("created_at", fixture.at).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.WithContext(ctx).Model(&links.Link{}).Where("token_hash = ?", fixture.student.SourceLinkHash).
			Updates(map[string]any{"submitted_at": fixture.at, "expires_at": fixture.at.Add(168 * time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", oldest.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", oldest.ID).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.WithContext(ctx).AutoMigrate(&links.Link{}, &students.Student{}); err != nil {
			t.Fatal(err)
		}
	}
	var sourceAfter links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", oldest.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Fatalf("inactive student changed retained source: before=%+v, after=%+v", sourceBefore, sourceAfter)
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
		if listed[i].ID != id || listed[i].Name != "Same name" || listed[i].IsActive != (id != oldest.ID) {
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

func TestUpdateFieldChangesOnlySelectedCanonicalColumn(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Original submission")
	_, other := enroll(t, db, sources, ctx, "Other student")
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	roster = students.New(db, func() { writes++ })
	for _, tc := range []struct {
		key, raw, want string
	}{
		{"name", " \tMaría\u2003José Pérez Rivera\n", "María José Pérez Rivera"},
		{"email", " Canonical.Name+Student@EXAMPLE.ORG ", "Canonical.Name+Student@example.org"},
		{"phone", "(939) 234-5678", "+19392345678"},
		{"municipality", " \tAñasco \u2003", "Añasco"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			updated, err := roster.UpdateField(ctx, original.ID, tc.key, tc.raw)
			want := original
			switch tc.key {
			case "name":
				want.Name = tc.want
			case "email":
				want.Email = tc.want
			case "phone":
				want.Phone = tc.want
			case "municipality":
				want.Municipality = tc.want
			}
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
			original = want
		})
	}
	if writes != len(students.Fields()) {
		t.Fatalf("committed-write notifications = %d, want %d", writes, len(students.Fields()))
	}
}

func TestUpdateFieldRejectsUnknownFieldsAndInvalidDetailsWithoutWrites(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Unchanged student")
	_, other := enroll(t, db, sources, ctx, "Other student")
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	roster = students.New(db, func() { writes++ })
	for _, key := range []string{"", "Name", "id", "email_confirm", "source_link_hash", "created_at", "is_active", "name = 'changed'; DROP TABLE students; --"} {
		t.Run("unknown field "+key, func(t *testing.T) {
			if got, err := roster.UpdateField(ctx, original.ID, key, "Changed student"); got != nil || !errors.Is(err, students.ErrUnknownField) {
				t.Fatalf("UpdateField(%q) = %+v, %v, want ErrUnknownField", key, got, err)
			}
		})
	}
	for _, tc := range []struct {
		name, key, raw string
	}{
		{"empty name", "name", ""},
		{"blank name", "name", " \t\u2003\n"},
		{"one word name", "name", "Changed"},
		{"punctuated name", "name", "Ana O'Connor"},
		{"markup name", "name", "<b>Ana Rivera</b>"},
		{"invalid UTF8 name", "name", "Ana\xff Rivera"},
		{"NUL name", "name", "Ana\x00 Rivera"},
		{"empty email", "email", ""},
		{"blank email", "email", " \t\u2003\n"},
		{"malformed email", "email", "PRIVATE_EMAIL"},
		{"display name email", "email", "Ada <Ada@example.com>"},
		{"empty phone", "phone", ""},
		{"blank phone", "phone", " \t\u2003\n"},
		{"Canadian phone", "phone", "+15062345678"},
		{"extension phone", "phone", "7872345678 x123"},
		{"empty municipality", "municipality", ""},
		{"blank municipality", "municipality", " \t\u2003\n"},
		{"forged municipality", "municipality", "PRIVATE_FORGED_MUNICIPALITY"},
		{"alternate municipality case", "municipality", "san juan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roster.UpdateField(ctx, original.ID, tc.key, tc.raw)
			var invalid *students.ValidationError
			if got != nil || !errors.As(err, &invalid) {
				t.Fatalf("UpdateField = %+v, %v, want ValidationError", got, err)
			}
		})
	}
	for _, field := range students.Fields() {
		t.Run("missing student "+field.Key, func(t *testing.T) {
			for _, id := range []uint64{0, other.ID + 1, 1 << 63, ^uint64(0)} {
				if got, err := roster.UpdateField(ctx, id, field.Key, field.Value(original)); got != nil || err != nil {
					t.Fatalf("missing UpdateField(%d, %q) = %+v, %v", id, field.Key, got, err)
				}
			}
		})
	}
	for _, want := range []students.Student{original, other} {
		persisted, err := roster.Get(ctx, want.ID)
		if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) {
			t.Fatalf("rejected edits changed student: %+v, %v, want %+v", persisted, err, want)
		}
	}
	var sourceAfter links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceBefore, sourceAfter) {
		t.Fatalf("rejected edits changed source: before %+v, after %+v", sourceBefore, sourceAfter)
	}
	if listed, err := roster.List(ctx); err != nil || len(listed) != 2 {
		t.Fatalf("rejected edits changed roster: %+v, %v", listed, err)
	}
	if writes != 0 {
		t.Fatalf("rejected edits notified %d writes", writes)
	}
}

func TestUpdateFieldSuppressedWritesAreNotReportedAsSaved(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Original student")
	if err := db.WithContext(ctx).Exec(`CREATE FUNCTION suppress_student_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Exec(`CREATE TRIGGER suppress_student_update BEFORE UPDATE ON students FOR EACH ROW EXECUTE FUNCTION suppress_student_update()`).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	roster = students.New(db, func() { writes++ })
	attempt := students.Student{Details: students.Details{
		Name: "Changed student", Email: "Changed@example.org", Phone: "+19392345678", Municipality: "Añasco",
	}}
	for _, field := range students.Fields() {
		t.Run(field.Key, func(t *testing.T) {
			if got, err := roster.UpdateField(ctx, original.ID, field.Key, field.Value(attempt)); got != nil || err != nil {
				t.Fatalf("suppressed UpdateField = %+v, %v, want absent row without error", got, err)
			}
		})
	}
	if got, err := roster.Get(ctx, original.ID); err != nil || got == nil || !reflect.DeepEqual(*got, original) {
		t.Fatalf("suppressed edits changed student: %+v, %v", got, err)
	}
	if writes != 0 {
		t.Fatalf("suppressed edits notified %d writes", writes)
	}
}

func TestStorePropagatesDatabaseFailuresAfterFieldValidation(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Unchanged student")
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	failed := students.New(tx, nil)
	if got, err := failed.List(ctx); got != nil || err == nil {
		t.Fatalf("failed List = %+v, %v", got, err)
	}
	if got, err := failed.Get(ctx, original.ID); got != nil || err == nil {
		t.Fatalf("failed Get = %+v, %v", got, err)
	}
	for _, field := range students.Fields() {
		t.Run(field.Key, func(t *testing.T) {
			got, err := failed.UpdateField(ctx, original.ID, field.Key, field.Value(original))
			var invalid *students.ValidationError
			if got != nil || err == nil || errors.As(err, &invalid) {
				t.Fatalf("valid UpdateField did not reach failed SQL: %+v, %v", got, err)
			}
			if got, err := failed.UpdateField(ctx, original.ID, field.Key, " \u2003 "); got != nil || !errors.As(err, &invalid) {
				t.Fatalf("invalid value was not rejected before SQL: %+v, %v", got, err)
			}
		})
	}
	if got, err := failed.Get(ctx, 0); got != nil || err != nil {
		t.Fatalf("zero Get queried failed database: %+v, %v", got, err)
	}
	if got, err := failed.UpdateField(ctx, original.ID, "name; DROP TABLE students", "Valid name"); got != nil || !errors.Is(err, students.ErrUnknownField) {
		t.Fatalf("unknown field was not rejected before SQL: %+v, %v", got, err)
	}
	if got, err := roster.Get(ctx, original.ID); err != nil || got == nil || !reflect.DeepEqual(*got, original) {
		t.Fatalf("failed edits changed student: %+v, %v", got, err)
	}
}

func studentStores(t *testing.T) (*gorm.DB, *students.Store, *links.Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, students.New(db, nil), links.New(db, nil), ctx
}

func enroll(t *testing.T, db *gorm.DB, sources *links.Store, ctx context.Context, name string) (links.Entry, students.Student) {
	t.Helper()
	entry, err := sources.Create(ctx, "student-test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	details := students.Details{
		Name: name, Email: "Original.Student@example.com", Phone: "+17872345678", Municipality: "San Juan",
	}
	if saved, err := sources.Submit(ctx, entry.Token, details); err != nil || saved != links.Saved {
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
