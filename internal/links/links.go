package links

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paymentwebsite/internal/students"
)

const tokenBytes = 32

// Link stores only the token's digest, so a database leak yields no usable URL.
type Link struct {
	TokenHash []byte    `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
	Name      string    `gorm:"not null;default:''"`
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// OpenedAt is the customer's first visit.
	OpenedAt *time.Time
	// SubmittedAt is the customer's final submit; autosave never sets it.
	SubmittedAt *time.Time
	// CanceledAt revokes an unfinished token but keeps its draft for staff.
	CanceledAt *time.Time
	// ExpiresAt starts with the configured lifetime; completion replaces it
	// with seven days from SubmittedAt. Missing expiries fail closed.
	ExpiresAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// CreatedBy and CanceledBy are the staff usernames behind each action.
	// Empty audit values mean the corresponding action has not occurred.
	CreatedBy  string            `gorm:"not null;default:''"`
	CanceledBy string            `gorm:"not null;default:''"`
	Student    *students.Student `gorm:"foreignKey:SourceLinkHash;references:TokenHash;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

// Status is a link's customer progress as shown to staff.
type Status string

const (
	NotOpened Status = "Not opened"
	Opened    Status = "Opened"
	// InProgress means autosaved data exists but the customer hasn't submitted.
	InProgress Status = "In progress"
	// Finished means the final answers and student enrollment committed.
	Finished Status = "Finished"
	// Expired means the link lapsed before the customer finished.
	Expired  Status = "Expired"
	Canceled Status = "Canceled"
)

// Entry is a link as shown to staff. ID is the lowercase hex token digest.
// Token is set only by Create: it is never stored, so it can't be shown again.
type Entry struct {
	ID          string
	Token       string
	Name        string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	SubmittedAt *time.Time
	CreatedBy   string
	CanceledBy  string
	Status      Status
}

// Cancelable reports whether staff can still revoke an unfinished link.
func (e Entry) Cancelable() bool {
	return e.Status != Canceled && e.Status != Finished && !e.AccessExpired()
}

// AccessExpired reports whether the token's access deadline has passed.
func (e Entry) AccessExpired() bool {
	return !time.Now().Before(e.ExpiresAt)
}

// Archived reports whether completion is at least 24 hours old at now.
func (e Entry) Archived(now time.Time) bool {
	return e.Status == Finished && e.SubmittedAt != nil && !e.SubmittedAt.After(now.Add(-24*time.Hour))
}

// Trashed reports whether the link belongs in canceled or expired history.
func (e Entry) Trashed() bool {
	return e.Status == Canceled || e.Status == Expired
}

func (l Link) entry() Entry {
	e := Entry{
		ID: hex.EncodeToString(l.TokenHash), Name: l.Name, Status: NotOpened,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, CreatedBy: l.CreatedBy, CanceledBy: l.CanceledBy,
		SubmittedAt: l.SubmittedAt,
	}
	switch {
	case l.CanceledAt != nil:
		e.Status = Canceled
	case l.SubmittedAt != nil:
		e.Status = Finished
	case !time.Now().Before(l.ExpiresAt):
		e.Status = Expired
	case l.Name != "":
		e.Status = InProgress
	case l.OpenedAt != nil:
		e.Status = Opened
	}
	return e
}

// errNoStaff rejects staff actions that would leave an anonymous audit entry.
var errNoStaff = errors.New("links: staff username required")

type Store struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Migrate atomically replaces pre-enrollment mock links on first cutover.
// Once students exists, later migrations preserve every retained record.
func Migrate(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		m := tx.Migrator()
		if !m.HasTable(&students.Student{}) && m.HasTable(&Link{}) {
			if err := m.DropTable(&Link{}); err != nil {
				return err
			}
		}
		return tx.AutoMigrate(&Link{}, &students.Student{})
	})
}

// Create issues a link that stops working at expiresAt, on behalf of the
// signed-in staff username.
func (s *Store) Create(ctx context.Context, staff string, expiresAt time.Time) (Entry, error) {
	if staff == "" {
		return Entry{}, errNoStaff
	}
	var random [tokenBytes]byte
	// Go 1.26 terminates the process if the OS random source fails.
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	digest, _ := tokenDigest(token)
	link := Link{TokenHash: digest, CreatedBy: staff, ExpiresAt: expiresAt}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		return Entry{}, err
	}
	entry := link.entry()
	entry.Token = token
	return entry, nil
}

// List returns every link, canceled and expired ones included, oldest first.
// ponytail: unbounded retained history suits two staff and small mock data;
// paginate before sustained growth.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	var records []Link
	if err := s.db.WithContext(ctx).Order("created_at, token_hash").Find(&records).Error; err != nil {
		return nil, err
	}
	entries := make([]Entry, len(records))
	for i, link := range records {
		entries[i] = link.entry()
	}
	return entries, nil
}

// Lookup returns the usable link for token, submitted ones included, without
// changing it: it records no visit. Tokens that open nothing return nil.
func (s *Store) Lookup(ctx context.Context, token string) (*Link, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return nil, nil
	}
	var link Link
	result := s.active(ctx, digest).Limit(1).Find(&link)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &link, nil
}

// Open returns the usable link for token, recording the customer's first visit.
func (s *Store) Open(ctx context.Context, token string) (*Link, error) {
	link, err := s.Lookup(ctx, token)
	if err != nil || link == nil {
		return nil, err
	}
	if link.OpenedAt == nil {
		if err := s.active(ctx, link.TokenHash).Where("opened_at IS NULL").Update("opened_at", gorm.Expr("now()")).Error; err != nil {
			return nil, err
		}
	}
	return link, nil
}

// SaveDraft stores an autosaved name without finishing the form.
// Submitted forms never match: the customer's final answers are locked.
func (s *Store) SaveDraft(ctx context.Context, token, name string) (bool, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return false, nil
	}
	result := s.active(ctx, digest).Where("submitted_at IS NULL").Update("name", name)
	return result.RowsAffected == 1 && result.Error == nil, result.Error
}

// Submit commits final answers and a retained student together, locking the form.
func (s *Store) Submit(ctx context.Context, token, name string) (bool, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return false, nil
	}
	saved := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var completed Link
		scoped := Store{db: tx}
		result := scoped.active(ctx, digest).Model(&completed).Where("submitted_at IS NULL").
			Clauses(clause.Returning{}).Updates(map[string]any{
			"name": name, "submitted_at": gorm.Expr("now()"),
			"expires_at": gorm.Expr("now() + interval '168 hours'"),
		})
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		student := students.Student{
			SourceLinkHash: completed.TokenHash, Name: completed.Name, CreatedAt: *completed.SubmittedAt,
		}
		if err := tx.Create(&student).Error; err != nil {
			return err
		}
		saved = true
		return nil
	})
	return saved && err == nil, err
}

// Cancel revokes an unfinished link whose Entry.ID is id and returns its
// persisted entry without a token. Finished, expired and previously canceled
// links stay unchanged, preserving the first cancellation's audit.
// Malformed and unknown IDs return nil.
func (s *Store) Cancel(ctx context.Context, id, staff string) (*Entry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	digest, err := hex.DecodeString(id)
	if err != nil || len(digest) != sha256.Size {
		return nil, nil
	}
	if err := s.active(ctx, digest).Where("submitted_at IS NULL").Updates(canceledBy(staff)).Error; err != nil {
		return nil, err
	}
	var link Link
	result := s.db.WithContext(ctx).Where(&Link{TokenHash: digest}).Limit(1).Find(&link)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	entry := link.entry()
	return &entry, nil
}

// CancelAll revokes usable unfinished links on behalf of staff.
// Finished, expired and previously canceled links stay unchanged.
func (s *Store) CancelAll(ctx context.Context, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	return s.usable(ctx).Where("submitted_at IS NULL").Updates(canceledBy(staff)).Error
}

func canceledBy(staff string) map[string]any {
	return map[string]any{"canceled_at": gorm.Expr("now()"), "canceled_by": staff}
}

// active scopes a query to the usable link with digest.
func (s *Store) active(ctx context.Context, digest []byte) *gorm.DB {
	return s.usable(ctx).Where(&Link{TokenHash: digest})
}

// usable scopes a query to tokens that still open the form or success page:
// neither canceled nor past their access deadline.
func (s *Store) usable(ctx context.Context) *gorm.DB {
	return s.db.WithContext(ctx).Model(&Link{}).Where("canceled_at IS NULL AND expires_at > now()")
}

// tokenDigest rejects tokens of the wrong length before hashing.
func tokenDigest(token string) ([]byte, bool) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], true
}
