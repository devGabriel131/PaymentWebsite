//go:build integration

package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Open isolates each test in a schema, including queries through db.DB().
func Open(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL for a dedicated test database")
	}
	base, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := base.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	rand.Read(random[:])
	schema := "pwtest_" + hex.EncodeToString(random[:])
	// Schema identifiers contain only this fixed prefix and internally generated hex.
	if err := base.WithContext(ctx).Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := base.WithContext(cleanup).Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	pool.SetMaxOpenConns(10)
	pool.SetMaxIdleConns(5)
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(models) > 0 {
		if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
