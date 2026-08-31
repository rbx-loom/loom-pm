package db

import (
	"context"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

// loginAndGitHubID reads back what a user row now says, which is how adoption is checked.
// The id is a pointer because a user who has never signed in has none: that absence is the
// thing UpsertGitHubUser looks for.
func loginAndGitHubID(t *testing.T, store *Store, userID int64) (string, *int64) {
	t.Helper()

	var (
		login string
		id    *int64
	)

	err := store.pool.QueryRow(context.Background(),
		`SELECT login, github_id FROM users WHERE id = $1`, userID).Scan(&login, &id)
	if err != nil {
		t.Fatalf("reading user %d: %v", userID, err)
	}

	return login, id
}

func TestUpsertGitHubUserCreates(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)

	identity := auth.Identity{GitHubID: 4242, Login: "ada", AvatarURL: "https://avatars.example/ada.png"}

	userID, err := store.UpsertGitHubUser(context.Background(), identity)
	if err != nil {
		t.Fatalf("UpsertGitHubUser: %v", err)
	}

	login, id := loginAndGitHubID(t, store, userID)
	if id == nil || login != "ada" || *id != 4242 {
		t.Errorf("stored %q/%v, want ada/4242", login, id)
	}
}

// A GitHub login is a display name its owner may change, so the id is the identity and the
// login follows it.
func TestUpsertGitHubUserFollowsARename(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	first, err := store.UpsertGitHubUser(ctx, auth.Identity{GitHubID: 4242, Login: "ada"})
	if err != nil {
		t.Fatalf("UpsertGitHubUser: %v", err)
	}

	second, err := store.UpsertGitHubUser(ctx, auth.Identity{GitHubID: 4242, Login: "ada-lovelace"})
	if err != nil {
		t.Fatalf("UpsertGitHubUser again: %v", err)
	}

	if first != second {
		t.Fatalf("the rename made a second user: %d then %d", first, second)
	}

	if login, _ := loginAndGitHubID(t, store, second); login != "ada-lovelace" {
		t.Errorf("login = %q, want the new one", login)
	}
}

// A registry bootstrapped with `loomreg token ada` has a user with no GitHub identity yet.
// Signing in has to become that user, or their packages are stranded under a login they
// can no longer authenticate as.
func TestUpsertGitHubUserAdoptsABootstrappedUser(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	bootstrapped, err := store.IssueToken(ctx, "ada", "bootstrap")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	before, err := store.UserByTokenHash(ctx, auth.HashToken(bootstrapped))
	if err != nil {
		t.Fatalf("UserByTokenHash: %v", err)
	}

	// no identity yet, and that absence is NULL rather than a number standing in for one:
	// a placeholder is a value every query then has to know how to disbelieve
	if _, id := loginAndGitHubID(t, store, before.ID); id != nil {
		t.Errorf("a bootstrapped user carries github_id %d, want none at all", *id)
	}

	adopted, err := store.UpsertGitHubUser(ctx, auth.Identity{GitHubID: 4242, Login: "Ada"})
	if err != nil {
		t.Fatalf("UpsertGitHubUser: %v", err)
	}

	if adopted != before.ID {
		t.Fatalf("signing in made a second user: bootstrapped %d, signed in %d", before.ID, adopted)
	}

	login, id := loginAndGitHubID(t, store, adopted)
	if id == nil || *id != 4242 {
		t.Errorf("github_id = %v, want the real one", id)
	}

	if login != "Ada" {
		t.Errorf("login = %q, want the casing GitHub gave", login)
	}

	// the bootstrap token still works, because it is the same user
	if _, err := store.UserByTokenHash(ctx, auth.HashToken(bootstrapped)); err != nil {
		t.Errorf("the bootstrap token stopped working after sign-in: %v", err)
	}
}

// Only a user still waiting for an identity is adopted; somebody already signed in keeps
// their row whatever login a newcomer arrives with.
func TestUpsertGitHubUserDoesNotAdoptASignedInUser(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	first, err := store.UpsertGitHubUser(ctx, auth.Identity{GitHubID: 4242, Login: "ada"})
	if err != nil {
		t.Fatalf("UpsertGitHubUser: %v", err)
	}

	second, err := store.UpsertGitHubUser(ctx, auth.Identity{GitHubID: 9999, Login: "ada"})
	if err != nil {
		t.Fatalf("UpsertGitHubUser for a second identity: %v", err)
	}

	if first == second {
		t.Fatal("two GitHub identities were folded into one user")
	}

	if _, id := loginAndGitHubID(t, store, first); id == nil || *id != 4242 {
		t.Errorf("the first user's github_id changed to %v", id)
	}
}
