package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/customer"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/management"
	"paymentwebsite/internal/settings"
)

//go:embed static/styles.css static/logo.png static/management.js
var assets embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			fmt.Print(auth.Usage)
			return nil
		case "user":
			if len(args) == 1 || len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Print(auth.Usage)
				return nil
			}
		default:
			return fmt.Errorf("unknown command %q; use --help", args[0])
		}
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&auth.User{}, &auth.Session{}, &settings.Settings{}); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := links.Migrate(db.WithContext(ctx)); err != nil {
		return fmt.Errorf("migrate links: %w", err)
	}

	if len(args) > 0 {
		return auth.RunCLI(context.Background(), db, args[1:])
	}

	secureCookies := true
	if value, ok := os.LookupEnv("SESSION_COOKIE_SECURE"); ok {
		secureCookies, err = strconv.ParseBool(value)
		if err != nil {
			return errors.New("SESSION_COOKIE_SECURE must be true or false")
		}
	}
	store := links.New(db)
	sessionStore := postgresstore.New(sqlDB)
	defer sessionStore.StopCleanup()
	sessions := scs.New()
	sessions.Store = sessionStore
	sessions.Lifetime = 14 * 24 * time.Hour
	sessions.IdleTimeout = 0
	sessions.Cookie.Name = "management_session"
	sessions.Cookie.Path = "/management/"
	sessions.Cookie.Domain = ""
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.SameSite = http.SameSiteLaxMode
	sessions.Cookie.Persist = true
	sessions.Cookie.Secure = secureCookies
	authentication, err := auth.New(db, sessions)
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/management/", http.StatusFound)
	})
	authentication.RegisterRoutes(mux)
	managementMux := http.NewServeMux()
	if err := management.RegisterRoutes(managementMux, store, settings.New(db), auth.Staff); err != nil {
		return fmt.Errorf("register management routes: %w", err)
	}
	mux.Handle("/management/", authentication.Require(managementMux))
	if err := customer.RegisterRoutes(mux, store); err != nil {
		return fmt.Errorf("register customer routes: %w", err)
	}

	addr := "127.0.0.1:3000"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(sessions.LoadAndSave(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Listening on %s", server.Addr)
	return server.ListenAndServe()
}
