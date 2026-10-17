package links

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"gorm.io/gorm"
)

const tokenBytes = 32

type Link struct {
	TokenHash []byte `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
}

type Store struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Create(ctx context.Context) (string, error) {
	var random [tokenBytes]byte
	// Go 1.26 terminates the process if the OS random source fails.
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(token))
	if err := s.db.WithContext(ctx).Create(&Link{TokenHash: digest[:]}).Error; err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) Exists(ctx context.Context, token string) (bool, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false, nil
	}

	digest := sha256.Sum256([]byte(token))
	var count int64
	err := s.db.WithContext(ctx).Model(&Link{}).Where(&Link{TokenHash: digest[:]}).Count(&count).Error
	return count == 1, err
}

func (s *Store) CancelAll(ctx context.Context) error {
	return s.db.WithContext(ctx).
		Session(&gorm.Session{AllowGlobalUpdate: true}).
		Delete(&Link{}).Error
}
