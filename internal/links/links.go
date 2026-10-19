package links

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const tokenBytes = 32

type Link struct {
	TokenHash []byte `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
	Name      string `gorm:"not null;default:''"`
	// NULL marks links issued before encrypted recovery; only their digest exists.
	TokenCiphertext []byte
	CreatedAt       time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// Entry is an active link as shown to staff. ID is the lowercase hex token
// digest; Token is empty for links issued before encrypted recovery.
type Entry struct {
	ID    string
	Token string
	Name  string
}

type Store struct {
	db   *gorm.DB
	aead cipher.AEAD
}

func New(db *gorm.DB, key []byte) (*Store, error) {
	if len(key) != 32 { // AES-256; NewCipher would silently accept shorter AES keys.
		return nil, errors.New("link encryption key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
}

func (s *Store) Create(ctx context.Context) (Entry, error) {
	var random [tokenBytes]byte
	// Go 1.26 terminates the process if the OS random source fails.
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(token))
	link := Link{
		TokenHash: digest[:],
		// The digest as associated data binds each ciphertext to its own row.
		TokenCiphertext: s.aead.Seal(nil, nil, []byte(token), digest[:]),
	}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		return Entry{}, err
	}
	return Entry{ID: hex.EncodeToString(digest[:]), Token: token, Name: link.Name}, nil
}

// List returns every active link, oldest first. Any unreadable ciphertext fails
// the whole list so a wrong key or corruption never hides an active link.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	var records []Link
	if err := s.db.WithContext(ctx).Order("created_at, token_hash").Find(&records).Error; err != nil {
		return nil, err
	}
	entries := make([]Entry, len(records))
	for i, link := range records {
		entries[i] = Entry{ID: hex.EncodeToString(link.TokenHash), Name: link.Name}
		if link.TokenCiphertext == nil {
			continue
		}
		token, err := s.aead.Open(nil, nil, link.TokenCiphertext, link.TokenHash)
		if err != nil {
			return nil, fmt.Errorf("decrypt link %s: %w", entries[i].ID, err)
		}
		entries[i].Token = string(token)
	}
	return entries, nil
}

func (s *Store) Get(ctx context.Context, token string) (*Link, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return nil, nil
	}

	digest := sha256.Sum256([]byte(token))
	var link Link
	result := s.db.WithContext(ctx).Where(&Link{TokenHash: digest[:]}).Limit(1).Find(&link)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &link, nil
}

func (s *Store) SaveName(ctx context.Context, token, name string) (bool, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false, nil
	}

	digest := sha256.Sum256([]byte(token))
	result := s.db.WithContext(ctx).Model(&Link{}).
		Where(&Link{TokenHash: digest[:]}).Update("name", name)
	return result.RowsAffected == 1, result.Error
}

// Cancel revokes the link whose Entry.ID is id. IDs that cannot name a link are
// already satisfied, so they are no-ops like unknown or canceled IDs.
func (s *Store) Cancel(ctx context.Context, id string) error {
	digest, err := hex.DecodeString(id)
	if err != nil || len(digest) != sha256.Size {
		return nil
	}
	return s.db.WithContext(ctx).Where(&Link{TokenHash: digest}).Delete(&Link{}).Error
}

func (s *Store) CancelAll(ctx context.Context) error {
	return s.db.WithContext(ctx).
		Session(&gorm.Session{AllowGlobalUpdate: true}).
		Delete(&Link{}).Error
}
