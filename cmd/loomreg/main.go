// Command loomreg is the Loom package registry.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/api"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/db"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/storage"
	"github.com/rbx-loom/loom-pm/internal/usage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// the subcommands are what an operator runs against a registry rather than through it:
	// bootstrapping a credential and a scope, and the two maintenance jobs
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "token":
			if err := issueToken(os.Args[2:]); err != nil {
				logger.Error("could not issue a token", "error", err)
				os.Exit(1)
			}
			return
		case "scope":
			if err := createScope(os.Args[2:]); err != nil {
				logger.Error("could not create the scope", "error", err)
				os.Exit(1)
			}
			return
		case "sweep":
			if err := sweepBlobs(os.Args[2:]); err != nil {
				logger.Error("could not sweep", "error", err)
				os.Exit(1)
			}
			return
		case "verify":
			if err := verifyBlobs(os.Args[2:]); err != nil {
				logger.Error("the store is not sound", "error", err)
				os.Exit(1)
			}
			return
		}
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

	baseURL      string
	clientID     string
	clientSecret string
	metricsToken string
}

func configure() (config, error) {
	loaded := config{
		address:     valueOr("LOOM_ADDRESS", ":8080"),
		databaseURL: os.Getenv("LOOM_DATABASE_URL"),
		blobRoot:    valueOr("LOOM_BLOB_ROOT", "var/blobs"),

		baseURL:      os.Getenv("LOOM_BASE_URL"),
		clientID:     os.Getenv("LOOM_GITHUB_CLIENT_ID"),
		clientSecret: os.Getenv("LOOM_GITHUB_CLIENT_SECRET"),
		metricsToken: os.Getenv("LOOM_METRICS_TOKEN"),
	}

	if loaded.databaseURL == "" {
		return config{}, errors.New("LOOM_DATABASE_URL is not set")
	}

	// all three or none: a half-configured sign-in fails at the callback, which is after
	// somebody has already been sent to GitHub
	configured := 0
	for _, value := range []string{loaded.baseURL, loaded.clientID, loaded.clientSecret} {
		if value != "" {
			configured++
		}
	}

	if configured != 0 && configured != 3 {
		return config{}, errors.New("set all of LOOM_BASE_URL, LOOM_GITHUB_CLIENT_ID and LOOM_GITHUB_CLIENT_SECRET, or none")
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

	// nil when unconfigured, which the sign-in endpoints answer as 503 rather than failing
	var provider auth.Provider
	if settings.clientID != "" {
		provider = auth.NewGitHub(settings.clientID, settings.clientSecret,
			strings.TrimSuffix(settings.baseURL, "/")+"/v1/auth/github/callback")
		logger.Info("sign-in configured", "base", settings.baseURL)
	}

	recorder := usage.NewRecorder(store, usageInterval, logger)
	go recorder.Run(ctx)

	server := &http.Server{
		Addr: settings.address,
		Handler: api.New(api.Dependencies{
			Store:         store,
			Blobs:         blobs,
			Publisher:     publish.NewService(store, blobs, limits),
			Authenticator: auth.New(store),
			Yanker:        store,
			Owners:        store,
			Tokens:        store,
			Provider:      provider,
			Users:         store,
			Catalog:       store,
			Usage:         recorder,
			Limits:        limits,
			MetricsToken:  settings.metricsToken,
			Logger:        logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,

		// net/http's own default, stated: the registry takes no header worth a megabyte,
		// and a default that only exists in the standard library is one nobody reviews
		MaxHeaderBytes: http.DefaultMaxHeaderBytes,
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

// usageInterval is how often download and token tallies are written out. Long enough that
// a busy registry writes to those tables a couple of times a minute, short enough that a
// crash loses a statistic rather than a day of them.
const usageInterval = 30 * time.Second

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

func createScope(arguments []string) error {
	if len(arguments) < 2 {
		return errors.New("usage: loomreg scope <name> <login>")
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

	return db.NewStore(pool).CreateScope(ctx, arguments[0], arguments[1])
}
