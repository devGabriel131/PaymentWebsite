// Package auth manages staff accounts and their management sessions.
package auth

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const dbTimeout = 5 * time.Second

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// User is a staff account. AuthVersion is random, never a counter: rotating it
// signs out existing sessions, and a recreated username can't match an old one.
type User struct {
	Username     string `gorm:"primaryKey"`
	PasswordHash string `gorm:"not null"`
	AuthVersion  string `gorm:"not null"`
}

// Session is the sessions table expected by scs postgresstore.
type Session struct {
	Token  string    `gorm:"primaryKey"`
	Data   []byte    `gorm:"not null"`
	Expiry time.Time `gorm:"not null;index:sessions_expiry_idx"`
}

// normalizeUsername validates before lowercasing so non-ASCII input such as
// the Kelvin sign can't fold into an ASCII username.
func normalizeUsername(username string) (string, bool) {
	username = strings.TrimSpace(username)
	if !usernamePattern.MatchString(username) {
		return "", false
	}
	return strings.ToLower(username), true
}

func findUser(ctx context.Context, db *gorm.DB, username string) (*User, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var user User
	result := db.WithContext(ctx).Where("username = ?", username).Limit(1).Find(&user)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &user, nil
}

func createUser(ctx context.Context, db *gorm.DB, username, password string) error {
	hash, err := argon2id.CreateHash(password, argon2id.DefaultParams)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&User{Username: username, PasswordHash: hash, AuthVersion: rand.Text()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("user %q already exists", username)
	}
	return nil
}

func setPassword(ctx context.Context, db *gorm.DB, username, password string) error {
	hash, err := argon2id.CreateHash(password, argon2id.DefaultParams)
	if err != nil {
		return err
	}
	return updateUser(ctx, db, username, map[string]any{"password_hash": hash, "auth_version": rand.Text()})
}

func revokeSessions(ctx context.Context, db *gorm.DB, username string) error {
	return updateUser(ctx, db, username, map[string]any{"auth_version": rand.Text()})
}

func updateUser(ctx context.Context, db *gorm.DB, username string, values map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return existingUser(db.WithContext(ctx).Model(&User{}).Where("username = ?", username).Updates(values), username)
}

func removeUser(ctx context.Context, db *gorm.DB, username string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return existingUser(db.WithContext(ctx).Where("username = ?", username).Delete(&User{}), username)
}

func existingUser(result *gorm.DB, username string) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("user %q does not exist", username)
	}
	return nil
}
