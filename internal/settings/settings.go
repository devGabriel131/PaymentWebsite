// Package settings stores staff-configurable options in a single database row.
package settings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Link lifetimes run from a couple of days up to two weeks.
const (
	MinLinkLifetimeDays     = 2
	MaxLinkLifetimeDays     = 14
	DefaultLinkLifetimeDays = 7
)

// ErrLinkLifetimeRange rejects lifetimes outside the settings slider's range.
var ErrLinkLifetimeRange = fmt.Errorf("settings: link lifetime must be %d-%d days", MinLinkLifetimeDays, MaxLinkLifetimeDays)

var errNoStaff = errors.New("settings: staff username required")

// Settings is the only row, ID 1. The checks repeat the range constants,
// which struct tags can't reference, so a bad value can't be stored even
// by bypassing the Store.
type Settings struct {
	ID               int       `gorm:"primaryKey;autoIncrement:false;check:settings_single_row,id = 1"`
	LinkLifetimeDays int       `gorm:"not null;check:link_lifetime_days_range,link_lifetime_days BETWEEN 2 AND 14"`
	UpdatedAt        time.Time `gorm:"not null"`
	UpdatedBy        string    `gorm:"not null"`
}

// LinkLifetime is how long a newly created link stays usable.
func (s Settings) LinkLifetime() time.Duration {
	return time.Duration(s.LinkLifetimeDays) * 24 * time.Hour
}

type Store struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Current returns the saved settings, or the defaults before anyone has saved.
// Defaults have no UpdatedBy.
func (s *Store) Current(ctx context.Context) (Settings, error) {
	var current Settings
	result := s.db.WithContext(ctx).Limit(1).Find(&current)
	if result.Error != nil {
		return Settings{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Settings{ID: 1, LinkLifetimeDays: DefaultLinkLifetimeDays}, nil
	}
	return current, nil
}

// SetLinkLifetimeDays saves the lifetime for links created from now on, on
// behalf of staff. Links already issued keep their own expiry.
func (s *Store) SetLinkLifetimeDays(ctx context.Context, days int, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	if days < MinLinkLifetimeDays || days > MaxLinkLifetimeDays {
		return ErrLinkLifetimeRange
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&Settings{ID: 1, LinkLifetimeDays: days, UpdatedBy: staff}).Error
}
