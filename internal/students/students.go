package students

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Details struct {
	Name         string `gorm:"not null;default:''"`
	Email        string `gorm:"not null;default:''"`
	Phone        string `gorm:"not null;default:''"`
	Municipality string `gorm:"not null;default:''"`
	DateOfBirth  string `gorm:"type:text;not null;default:''"`
}

type Student struct {
	ID             uint64 `gorm:"primaryKey"`
	SourceLinkHash []byte `gorm:"not null;uniqueIndex;check:student_source_hash_size,octet_length(source_link_hash) = 32"`
	Details
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	IsActive  bool      `gorm:"not null;default:true"`
}

type Store struct {
	db *gorm.DB
	// changed runs after every committed write, so open management pages refresh.
	changed func()
}

// New returns a store whose committed writes call changed, when it is non-nil.
func New(db *gorm.DB, changed func()) *Store {
	if changed == nil {
		changed = func() {}
	}
	return &Store{db: db, changed: changed}
}

// List returns retained students, newest first, regardless of source access.
// A non-nil active keeps only students whose is_active equals it.
// ponytail: unbounded small mock history; paginate before sustained growth.
func (s *Store) List(ctx context.Context, active *bool) ([]Student, error) {
	query := s.db.WithContext(ctx).Order("created_at DESC, id DESC")
	if active != nil {
		query = query.Where("is_active = ?", *active)
	}
	var records []Student
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// Counts is the roster's headcount.
type Counts struct{ Active, Suspended, Recent int }

// Total is every retained student, active or not.
func (c Counts) Total() int { return c.Active + c.Suspended }

// Count returns the active and suspended headcounts, and how many students enrolled at or after since.
func (s *Store) Count(ctx context.Context, since time.Time) (Counts, error) {
	var c Counts
	err := s.db.WithContext(ctx).Model(&Student{}).Select(
		"count(*) FILTER (WHERE is_active) AS active, count(*) FILTER (WHERE NOT is_active) AS suspended, count(*) FILTER (WHERE created_at >= ?) AS recent", since,
	).Scan(&c).Error
	return c, err
}

// Get returns nil for an absent student, including IDs outside PostgreSQL BIGINT.
func (s *Store) Get(ctx context.Context, id uint64) (*Student, error) {
	if id == 0 || id > 1<<63-1 {
		return nil, nil
	}
	var record Student
	result := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&record)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &record, nil
}

// BySourceLinkHash returns nil for an absent student by the original link's token digest.
func (s *Store) BySourceLinkHash(ctx context.Context, sourceLinkHash []byte) (*Student, error) {
	if len(sourceLinkHash) != sha256.Size {
		return nil, nil
	}
	var record Student
	result := s.db.WithContext(ctx).Where("source_link_hash = ?", sourceLinkHash).Limit(1).Find(&record)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &record, nil
}

var ErrUnknownField = errors.New("students: unknown field")

// UpdateField changes only the registered column and returns its persisted row.
// Normalized no-ops return the current row without writing or notifying.
func (s *Store) UpdateField(ctx context.Context, id uint64, key, raw string) (*Student, error) {
	field, ok := FindField(key)
	if !ok {
		return nil, ErrUnknownField
	}
	value, err := field.Parse(raw, true)
	if err != nil {
		return nil, err
	}
	if id == 0 || id > 1<<63-1 {
		return nil, nil
	}
	var persisted *Student
	wrote := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record Student
		// Lock the current row so comparison and write share one version.
		result := tx.Where("id = ?", id).
			Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Limit(1).Find(&record)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if field.Value(record.Details) == value {
			persisted = &record
			return nil
		}
		result = tx.Model(&record).
			Clauses(clause.Returning{}).Update(field.Key, value)
		// Zero rows after the locked read means the database suppressed the write (a trigger): not a save.
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		persisted, wrote = &record, true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if wrote {
		s.changed()
	}
	return persisted, nil
}
