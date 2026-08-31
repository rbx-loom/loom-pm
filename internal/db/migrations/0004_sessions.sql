-- A session is what a browser holds after signing in, and it is what minting a token now
-- requires. A token cannot create one, so a leaked token can no longer mint tokens that
-- outlive the revocation of itself.
--
-- The secret is stored only as a sha256 hash, for the same reason tokens are: a copy of
-- the database is not a set of credentials.
CREATE TABLE sessions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    hash       BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- unlike a token, a session expires: it is held by a browser rather than typed into a
    -- CI configuration, and nobody notices one that outlives its use
    expires_at TIMESTAMPTZ NOT NULL
);

-- Signing out deletes the row rather than marking it, because there is no audit that wants
-- to know a browser was closed. This index is what keeps the expiry sweep off a table scan.
CREATE INDEX sessions_expiry ON sessions (expires_at);
