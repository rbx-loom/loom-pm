package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/publish"
)

// user creates a login and answers its row id, so an ownership test has somebody to hand
// a package to.
func user(t *testing.T, pool *pgxpool.Pool, login string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (login) VALUES ($1) RETURNING id`,
		login).Scan(&id)
	if err != nil {
		t.Fatalf("creating %q: %v", login, err)
	}

	return id
}

// own makes login the sole owner of an already-seeded package.
func own(t *testing.T, pool *pgxpool.Pool, name string, userID int64) {
	t.Helper()

	parsed := mustName(t, name)

	_, err := pool.Exec(context.Background(), `
		INSERT INTO package_owners (package_id, user_id)
		SELECT id, $2 FROM packages WHERE normalized = $1
		ON CONFLICT DO NOTHING`, parsed.Normalized(), userID)
	if err != nil {
		t.Fatalf("making %d an owner of %s: %v", userID, name, err)
	}
}

func TestOwnersLifecycle(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	grace := user(t, pool, "grace")
	own(t, pool, "serio", ada)

	ctx := context.Background()
	name := mustName(t, "serio")

	owners, err := store.Owners(ctx, name)
	if err != nil {
		t.Fatalf("Owners: %v", err)
	}
	if strings.Join(owners, ",") != "ada" {
		t.Fatalf("owners = %v, want ada", owners)
	}

	if err := store.AddOwner(ctx, name, "grace", ada); err != nil {
		t.Fatalf("AddOwner: %v", err)
	}

	owners, err = store.Owners(ctx, name)
	if err != nil {
		t.Fatalf("Owners: %v", err)
	}
	if strings.Join(owners, ",") != "ada,grace" {
		t.Fatalf("owners = %v, want ada and grace ordered", owners)
	}

	// adding twice is adding once
	if err := store.AddOwner(ctx, name, "grace", ada); err != nil {
		t.Fatalf("AddOwner a second time: %v", err)
	}

	owners, _ = store.Owners(ctx, name)
	if len(owners) != 2 {
		t.Fatalf("owners = %v, want the second add to change nothing", owners)
	}

	if err := store.RemoveOwner(ctx, name, "ada", grace); err != nil {
		t.Fatalf("RemoveOwner: %v", err)
	}

	owners, _ = store.Owners(ctx, name)
	if strings.Join(owners, ",") != "grace" {
		t.Errorf("owners = %v, want grace alone", owners)
	}
}

// A package with no owner can never be published to or yanked again, so the last one
// cannot leave.
func TestRemoveOwnerRefusesTheLast(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	own(t, pool, "serio", ada)

	err := store.RemoveOwner(context.Background(), mustName(t, "serio"), "ada", ada)
	if !errors.Is(err, publish.ErrLastOwner) {
		t.Errorf("RemoveOwner = %v, want publish.ErrLastOwner", err)
	}
}

// Removing somebody who is not an owner leaves the state the caller asked for.
func TestRemoveOwnerIsIdempotent(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	user(t, pool, "grace")
	own(t, pool, "serio", ada)

	if err := store.RemoveOwner(context.Background(), mustName(t, "serio"), "grace", ada); err != nil {
		t.Fatalf("RemoveOwner of a non-owner: %v", err)
	}

	owners, _ := store.Owners(context.Background(), mustName(t, "serio"))
	if strings.Join(owners, ",") != "ada" {
		t.Errorf("owners = %v, want ada untouched", owners)
	}
}

func TestOwnerChangeRefusals(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	mallory := user(t, pool, "mallory")
	own(t, pool, "serio", ada)

	ctx := context.Background()
	name := mustName(t, "serio")

	if err := store.AddOwner(ctx, name, "mallory", mallory); !errors.Is(err, publish.ErrNotOwned) {
		t.Errorf("a non-owner adding themselves = %v, want publish.ErrNotOwned", err)
	}

	if err := store.AddOwner(ctx, name, "nobody", ada); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Errorf("adding an unknown login = %v, want auth.ErrNoSuchUser", err)
	}

	if _, err := store.Owners(ctx, mustName(t, "missing")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Owners of an unknown package = %v, want index.ErrNotFound", err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	pool := testPool(t)

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	ctx := context.Background()

	tokens, err := store.ListTokens(ctx, ada)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("listed %d tokens for a new user, want none", len(tokens))
	}

	presented, summary, err := store.CreateToken(ctx, ada, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if !strings.HasPrefix(presented, auth.TokenPrefix) {
		t.Errorf("token = %q, want a prefixed one", presented)
	}
	if summary.Name != "laptop" || summary.ID == 0 || summary.CreatedAt.IsZero() {
		t.Errorf("summary = %+v, want it filled in", summary)
	}

	// the token has to actually authenticate, which is the whole point of storing it
	resolved, err := store.UserByTokenHash(ctx, auth.HashToken(presented))
	if err != nil {
		t.Fatalf("UserByTokenHash: %v", err)
	}
	if resolved.ID != ada {
		t.Errorf("the token resolved to %d, want %d", resolved.ID, ada)
	}

	if err := store.RevokeToken(ctx, ada, summary.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if _, err := store.UserByTokenHash(ctx, auth.HashToken(presented)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("a revoked token authenticated: %v", err)
	}

	tokens, _ = store.ListTokens(ctx, ada)
	if len(tokens) != 0 {
		t.Errorf("listed %d tokens after revoking, want none", len(tokens))
	}
}

// Whose token an id belongs to is not something to confirm to whoever does not hold it, so
// revoking somebody else's answers as one that does not exist.
func TestRevokeTokenRefusesSomebodyElses(t *testing.T) {
	pool := testPool(t)

	store := NewStore(pool)
	ada := user(t, pool, "ada")
	mallory := user(t, pool, "mallory")
	ctx := context.Background()

	_, summary, err := store.CreateToken(ctx, ada, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := store.RevokeToken(ctx, mallory, summary.ID); !errors.Is(err, auth.ErrNoSuchToken) {
		t.Errorf("RevokeToken = %v, want auth.ErrNoSuchToken", err)
	}

	if err := store.RevokeToken(ctx, ada, summary.ID); err != nil {
		t.Errorf("the owner could not revoke their own token: %v", err)
	}

	// revoking twice is not revoking again
	if err := store.RevokeToken(ctx, ada, summary.ID); !errors.Is(err, auth.ErrNoSuchToken) {
		t.Errorf("revoking twice = %v, want auth.ErrNoSuchToken", err)
	}
}
