package students

import (
	"context"
	"errors"
	"strings"
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
	Gender       string `gorm:"not null;default:''"`
}

type Student struct {
	ID             uint64 `gorm:"primaryKey"`
	SourceLinkHash []byte `gorm:"not null;uniqueIndex;check:student_source_hash_size,octet_length(source_link_hash) = 32"`
	Details
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	IsActive  bool      `gorm:"not null;default:true"`
	// Pin is Socrative's credential for the student. A Link holds it from submit until payment;
	// Enroll then moves it here. The partial index keeps rows without a PIN from colliding.
	// The index name is pinIndex.
	Pin string `gorm:"not null;default:'';uniqueIndex:idx_student_pin,where:pin <> ''"`
	// Group is the program stage staff edit through groupField. Enroll leaves it to the column
	// default, so every student starts in the first group; the default and check repeat groups.
	Group string `gorm:"not null;default:'Básico';check:student_group_valid,\"group\" IN ('Básico', 'Regular')"`
	// Cohort is the month, YYYY-MM, of the agreement start date the student accepted. Enroll sets it
	// and no edit reaches it, so it stays the month the student started after any promotion.
	Cohort string `gorm:"type:text;not null;check:student_cohort_month,cohort ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'"`
}

// CohortLabel names the cohort's month, such as "November 2026".
func (s Student) CohortLabel() string {
	month, err := time.Parse("2006-01", s.Cohort)
	if err != nil {
		return s.Cohort // the check constraint makes this unreachable for stored rows
	}
	return month.Format("January 2006")
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
type Counts struct{ Active, Suspended int }

// Total is every retained student, active or not.
func (c Counts) Total() int { return c.Active + c.Suspended }

// Count returns the active and suspended headcounts.
func (s *Store) Count(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.db.WithContext(ctx).Model(&Student{}).Select(
		"count(*) FILTER (WHERE is_active) AS active, count(*) FILTER (WHERE NOT is_active) AS suspended",
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


// pinHeld reports whether a Student or a submitted Link still awaiting payment holds pin.
// links imports this package, so the table is queried by name without importing links.
func pinHeld(db *gorm.DB, pin string) (bool, error) {
	var held bool
	err := db.Raw("SELECT EXISTS (SELECT 1 FROM students WHERE pin = ?) OR EXISTS (SELECT 1 FROM links WHERE pin = ?)", pin, pin).Scan(&held).Error
	return held, err
}

// PinTaken reports whether pin is held by an enrolled student or a submitted link awaiting payment.
// The unique indexes are the authority at write time; this lets the customer form warn before final submit.
func (s *Store) PinTaken(ctx context.Context, pin string) (bool, error) {
	return pinHeld(s.db.WithContext(ctx), pin)
}

const (
	pinIndex     = "idx_student_pin" // Student.Pin
	linkPinIndex = "idx_link_pin"    // links.Link.Pin; keep in sync with its tag
)

// IsPinTaken reports whether err is the unique violation on either PIN index. The driver error
// exposes its SQLSTATE through a method but the constraint name only in its message, which
// carries no row values.
func IsPinTaken(err error) bool {
	var database interface{ SQLState() string }
	return errors.As(err, &database) && database.SQLState() == "23505" &&
		(strings.Contains(err.Error(), pinIndex) || strings.Contains(err.Error(), linkPinIndex))
}

var ErrUnknownField = errors.New("students: unknown field")

// UpdateField changes only the registered column and returns its persisted row.
// Normalized no-ops return the current row without writing or notifying.
func (s *Store) UpdateField(ctx context.Context, id uint64, key, raw string) (*Student, error) {
	field, ok := FindStaffField(key)
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
		if field.Value(record) == value {
			persisted = &record
			return nil
		}
		if field.Key == pinField.Key && value != "" {
			held, err := pinHeld(tx, value)
			if err != nil {
				return err
			}
			if held {
				return &ValidationError{Message: "That PIN is already in use."}
			}
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
	if IsPinTaken(err) {
		return nil, &ValidationError{Message: "That PIN is already in use."}
	}
	if err != nil {
		return nil, err
	}
	if wrote {
		s.changed()
	}
	return persisted, nil
}
