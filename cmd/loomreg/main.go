// Command loomreg is the Loom package registry.
//
// See DESIGN.md for the API surface and the schema.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/api"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/db"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// `loomreg token <login> [name]` mints a credential so a self-hosted registry has a
	// way to publish its first package before anyone can sign in through the web
	if len(os.Args) > 1 && os.Args[1] == "token" {
		if err := issueToken(os.Args[2:]); err != nil {
			logger.Error("could not issue a token", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(logger); err != nil {
		logger.Error("loomreg stopped", "error", err)
		os.Exit(1)
	}
}

type config struct {
	address     string
	databaseURL string
	blobRoot    string
}

func configure() (config, error) {
	loaded := config{
		address:     valueOr("LOOM_ADDRESS", ":8080"),
		databaseURL: os.Getenv("LOOM_DATABASE_URL"),
		blobRoot:    valueOr("LOOM_BLOB_ROOT", "var/blobs"),
	}

	if loaded.databaseURL == "" {
		return config{}, errors.New("LOOM_DATABASE_URL is not set")
	}

	return loaded, nil
}

func run(logger *slog.Logger) error {
	settings, err := configure()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, settings.databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("reaching the database: %w", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	if err := os.MkdirAll(settings.blobRoot, 0o755); err != nil {
		return fmt.Errorf("creating the blob store at %s: %w", settings.blobRoot, err)
	}

	store := db.NewStore(pool)
	blobs := storage.NewFilesystem(settings.blobRoot)
	limits := publish.DefaultLimits()

	server := &http.Server{
		Addr: settings.address,
		Handler: api.New(api.Dependencies{
			Store:         store,
			Blobs:         blobs,
			Publisher:     publish.NewService(store, blobs, limits),
			Authenticator: auth.New(store),
			Yanker:        store,
			Limits:        limits,
			Logger:        logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	failed := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", settings.address, "blobs", settings.blobRoot)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")

	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}

	return nil
}

func valueOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func issueToken(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: loomreg token <login> [name]")
	}

	name := "bootstrap"
	if len(arguments) > 1 {
		name = arguments[1]
	}

	settings, err := configure()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, settings.databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	token, err := db.NewStore(pool).IssueToken(ctx, arguments[0], name)
	if err != nil {
		return err
	}

	// stdout, and once: this is the only time the token exists outside its hash
	fmt.Println(token)
	return nil
}
