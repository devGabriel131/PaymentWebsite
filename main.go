package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/customer"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/live"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/management"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
)

//go:embed static
var assets embed.FS

func main() {
	out := os.Stdout
	if len(os.Args) > 1 {
		out = os.Stderr
	}
	service := os.Getenv("RAILWAY_SERVICE_NAME")
	if service == "" {
		service = "PaymentWebsite"
	}
	environment := os.Getenv("RAILWAY_ENVIRONMENT_NAME")
	if environment == "" {
		environment = "local"
	}
	version := os.Getenv("RAILWAY_GIT_COMMIT_SHA")
	if version == "" {
		version = "unknown"
	}
	log := logging.New(out).With("service", service, "environment", environment, "version", version)
	if id := os.Getenv("RAILWAY_DEPLOYMENT_ID"); id != "" {
		log = log.With("deployment_id", id)
	}
	if id := os.Getenv("RAILWAY_REPLICA_ID"); id != "" {
		log = log.With("replica_id", id)
	}
	if err := run(log); err != nil {
		os.Exit(1)
	}
}

func run(log *slog.Logger) (err error) {
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
			err = fmt.Errorf("unknown command %q; use --help", args[0])
			fmt.Fprintln(os.Stderr, err)
			return err
		}
	}

	stage := "configuration"
	setupOwned := true
	defer func() {
		if err != nil && setupOwned {
			log.LogAttrs(context.Background(), slog.LevelError, "startup_failed",
				append(logging.ErrorAttrs(err), slog.String("stage", stage))...)
		}
	}()
	if len(args) == 0 {
		log.Info("startup_started")
	}
	slow := logging.NewDBLogger(log)
	defer slow.Flush()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	stage = "database_open"
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 slow,
	})
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	stage = "database_pool"
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(10)

	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	stage = "database_connect"
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	stage = "database_migrate"
	migrateModels := append([]any{
		&auth.User{},
		&auth.Session{},
		&settings.Settings{},
		&links.Link{},
		&students.Student{},
	}, billing.Models()...)
	err = db.WithContext(logging.WithOperation(ctx, "startup_migrate")).AutoMigrate(migrateModels...)
	slow.Flush()
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	if len(args) > 0 {
		setupOwned = false
		err = auth.RunCLI(logging.WithOperation(signalContext, "cli_user"), db, args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return err
	}

	stage = "configuration"
	secureCookies := true
	if value, ok := os.LookupEnv("SESSION_COOKIE_SECURE"); ok {
		secureCookies, err = strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("SESSION_COOKIE_SECURE must be true or false: %w", err)
		}
	}
	// Committed writes tell open management pages to refresh themselves.
	feed := live.New()
	store := links.New(db, feed.Changed)

	stage = "billing_configuration"
	billingCfg, stripeAPIKey, err := billing.LoadConfigFromEnv()
	if err != nil {
		return fmt.Errorf("configure stripe: %w", err)
	}
	stripeClient := billing.NewStripeClient(stripeAPIKey)
	catalogCtx, cancelCatalog := context.WithTimeout(signalContext, 15*time.Second)
	defer cancelCatalog()
	stage = "billing_catalog"
	catalog, err := billing.LoadCatalog(catalogCtx, stripeClient, billingCfg.Live)
	if err != nil {
		return fmt.Errorf("load stripe catalog: %w", err)
	}
	stage = "billing_initialize"
	biller, err := billing.New(db, stripeClient, billingCfg, catalog, feed.Changed, log)
	if err != nil {
		return fmt.Errorf("initialize billing: %w", err)
	}

	stage = "auth_initialize"
	authentication, err := auth.New(db, secureCookies, log)
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	defer authentication.Close()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/favicon.ico")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/management/", http.StatusFound)
	})
	authentication.RegisterRoutes(mux)
	managementMux := http.NewServeMux()
	managementMux.Handle("GET /management/events", logging.Operation("live_updates", feed))
	stage = "management_routes"
	if err := management.RegisterRoutes(managementMux, store, students.New(db, feed.Changed), settings.New(db), auth.Staff, biller); err != nil {
		return fmt.Errorf("register management routes: %w", err)
	}
	mux.Handle("/management/", authentication.Require(managementMux))
	stage = "customer_routes"
	if err := customer.RegisterRoutes(mux, store, settings.New(db), biller); err != nil {
		return fmt.Errorf("register customer routes: %w", err)
	}
	mux.Handle("POST /webhooks/stripe", logging.Operation("stripe_webhook", http.HandlerFunc(biller.Webhook)))

	workerCtx, cancelWorker := context.WithCancel(signalContext)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		biller.Run(workerCtx)
	}()
	defer func() {
		cancelWorker()
		<-workerDone
	}()
	addr := "127.0.0.1:3000"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           logging.Requests(log, http.NewCrossOriginProtection().Handler(mux)),
		ErrorLog:          logging.ServerErrorLog(log),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Event streams never finish on their own; end them once shutdown starts.
	server.RegisterOnShutdown(feed.Close)

	stage = "listen"
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	log.Info("server_listening", "addr", listener.Addr().String())
	setupOwned = false
	return serve(signalContext, server, listener, log, slow)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, log *slog.Logger, slow *logging.DBLogger) error {
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-result:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			log.LogAttrs(context.Background(), slog.LevelError, "server_failed",
				append(logging.ErrorAttrs(err), slog.String("stage", "serve"))...)
			return err
		case <-ticker.C:
			slow.Flush()
		case <-ctx.Done():
			started := time.Now()
			log.Info("shutdown_requested", "drain_timeout_ms", 10000)
			shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := server.Shutdown(shutdownContext)
			if err != nil {
				err = errors.Join(err, server.Close())
			}
			servingErr := <-result
			if errors.Is(servingErr, http.ErrServerClosed) {
				servingErr = nil
			}
			err = errors.Join(err, servingErr)
			duration := slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond))
			if err != nil {
				log.LogAttrs(context.Background(), slog.LevelError, "shutdown_failed",
					append(logging.ErrorAttrs(err), duration)...)
				return err
			}
			log.LogAttrs(context.Background(), slog.LevelInfo, "shutdown_completed", duration)
			return nil
		}
	}
}
