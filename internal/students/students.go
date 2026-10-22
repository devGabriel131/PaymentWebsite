package students

import (
	"context"
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

// List returns all retained students, newest first, regardless of source access.
// ponytail: unbounded small mock history; paginate before sustained growth.
func (s *Store) List(ctx context.Context) ([]Student, error) {
	var records []Student
	if err := s.db.WithContext(ctx).Order("created_at DESC, id DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
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

var ErrUnknownField = errors.New("students: unknown field")

// UpdateField changes only the registered column and returns its persisted row.
func (s *Store) UpdateField(ctx context.Context, id uint64, key, raw string) (*Student, error) {
	field, ok := FindField(key)
	if !ok {
		return nil, ErrUnknownField
	}
	value, err := field.Parse(raw, field.Required)
	if err != nil {
		return nil, err
	}
	if id == 0 || id > 1<<63-1 {
		return nil, nil
	}
	var record Student
	result := s.db.WithContext(ctx).Model(&record).Where("id = ?", id).
		Clauses(clause.Returning{}).Update(field.Key, value)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	s.changed()
	return &record, nil
}
