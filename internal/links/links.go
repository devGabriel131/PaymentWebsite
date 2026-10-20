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

	"paymentwebsite/internal/settings"
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
	// CanceledAt revokes the token but keeps submitted data for staff.
	CanceledAt *time.Time
	// ExpiresAt is fixed at creation; later setting changes don't move it.
	// The default only lets the column be added to existing rows, and fails
	// closed: a row inserted without an expiry is already expired.
	ExpiresAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// CreatedBy and CanceledBy are the staff usernames behind each action.
	// Links from before auditing have neither.
	CreatedBy  string `gorm:"not null;default:''"`
	CanceledBy string `gorm:"not null;default:''"`
}

// Status is a link's customer progress as shown to staff.
type Status string

const (
	NotOpened Status = "Not opened"
	Opened    Status = "Opened"
	// InProgress means autosaved data exists but the customer hasn't submitted.
	InProgress Status = "In progress"
	// Finished means the customer pressed the final submit. Once payments
	// exist it means a completed payment.
	Finished Status = "Finished"
	// Expired means the link lapsed before the customer finished.
	Expired  Status = "Expired"
	Canceled Status = "Canceled"
)

// Entry is a link as shown to staff. ID is the lowercase hex token digest.
// Token is set only by Create: it is never stored, so it can't be shown again.
type Entry struct {
	ID         string
	Token      string
	Name       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	CreatedBy  string
	CanceledBy string
	Status     Status
}

// Cancelable reports whether staff can still revoke the link: its token
// still opens the customer form.
func (e Entry) Cancelable() bool {
	return e.Status != Canceled && time.Now().Before(e.ExpiresAt)
}

func (l Link) entry() Entry {
	e := Entry{
		ID: hex.EncodeToString(l.TokenHash), Name: l.Name, Status: NotOpened,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, CreatedBy: l.CreatedBy, CanceledBy: l.CanceledBy,
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

// Migrate creates the links table, drops the encrypted token copies left by
// the removed URL-recovery feature, and backfills columns added to existing
// rows: submissions from before autosave, when a saved name always meant a
// submitted form, and expiries from before link lifetimes existed.
func Migrate(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		m := tx.Migrator()
		// Only when each column is first added: later values are real data.
		existing := m.HasTable(&Link{})
		backfillSubmitted := existing && !m.HasColumn(&Link{}, "submitted_at")
		backfillExpiry := existing && !m.HasColumn(&Link{}, "expires_at")
		if err := tx.AutoMigrate(&Link{}); err != nil {
			return err
		}
		if backfillSubmitted {
			if err := tx.Model(&Link{}).Where("name <> ''").Update("submitted_at", gorm.Expr("now()")).Error; err != nil {
				return err
			}
		}
		if backfillExpiry {
			expiry := gorm.Expr("created_at + make_interval(days => ?)", settings.DefaultLinkLifetimeDays)
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Model(&Link{}).Update("expires_at", expiry).Error; err != nil {
				return err
			}
		}
		if m.HasColumn(&Link{}, "token_ciphertext") {
			return m.DropColumn(&Link{}, "token_ciphertext")
		}
		return nil
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
func (s *Store) SaveDraft(ctx context.Context, token, name string) (bool, error) {
	return s.update(ctx, token, map[string]any{"name": name})
}

// Submit stores the final name and finishes, and so locks, the form.
func (s *Store) Submit(ctx context.Context, token, name string) (bool, error) {
	return s.update(ctx, token, map[string]any{"name": name, "submitted_at": gorm.Expr("now()")})
}

// update changes the usable link for token, reporting whether one matched.
// Submitted forms never match: the customer's final answers are locked.
func (s *Store) update(ctx context.Context, token string, values map[string]any) (bool, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return false, nil
	}
	result := s.active(ctx, digest).Where("submitted_at IS NULL").Updates(values)
	return result.RowsAffected == 1, result.Error
}

// Cancel revokes the link whose Entry.ID is id on behalf of staff and returns
// it as saved, without its token. Links canceled earlier keep who canceled
// them and when, and expired links stay expired. IDs that cannot name a link
// are already satisfied, so like unknown IDs they return nil.
func (s *Store) Cancel(ctx context.Context, id, staff string) (*Entry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	digest, err := hex.DecodeString(id)
	if err != nil || len(digest) != sha256.Size {
		return nil, nil
	}
	if err := s.active(ctx, digest).Updates(canceledBy(staff)).Error; err != nil {
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

// CancelAll revokes every usable link on behalf of staff; links canceled
// earlier keep who canceled them, and expired links stay expired.
func (s *Store) CancelAll(ctx context.Context, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	return s.usable(ctx).Updates(canceledBy(staff)).Error
}

func canceledBy(staff string) map[string]any {
	return map[string]any{"canceled_at": gorm.Expr("now()"), "canceled_by": staff}
}

// active scopes a query to the usable link with digest.
func (s *Store) active(ctx context.Context, digest []byte) *gorm.DB {
	return s.usable(ctx).Where(&Link{TokenHash: digest})
}

// usable scopes a query to links whose tokens still open the customer form:
// neither canceled nor expired. An expired link is dead even mid-form.
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
