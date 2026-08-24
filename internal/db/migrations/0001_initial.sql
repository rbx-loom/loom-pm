CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    github_id  BIGINT UNIQUE NOT NULL,
    login      TEXT NOT NULL,
    avatar_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    hash         BYTEA NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- A scope is an owned namespace. Unscoped package names are first-come-first-served and
-- have scope_id NULL.
CREATE TABLE scopes (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    normalized TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE scope_members (
    scope_id BIGINT NOT NULL REFERENCES scopes (id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    PRIMARY KEY (scope_id, user_id)
);

CREATE TABLE packages (
    id         BIGSERIAL PRIMARY KEY,
    scope_id   BIGINT REFERENCES scopes (id),

    -- name is the segment after the scope, in the casing it was published under; the
    -- display form is scopes.name || '/' || packages.name
    name       TEXT NOT NULL,

    -- the full "scope/name" lowercased, which is how a package is looked up
    normalized TEXT NOT NULL UNIQUE,

    -- the same with '_' folded to '-', so "my-thing" and "my_thing" cannot be registered
    -- by two different people; it gates creation only, never lookup
    squat_key  TEXT NOT NULL UNIQUE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE package_owners (
    package_id BIGINT NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (package_id, user_id)
);

CREATE TABLE versions (
    id             BIGSERIAL PRIMARY KEY,
    package_id     BIGINT NOT NULL REFERENCES packages (id) ON DELETE CASCADE,

    major          INTEGER NOT NULL CHECK (major >= 0),
    minor          INTEGER NOT NULL CHECK (minor >= 0),
    patch          INTEGER NOT NULL CHECK (patch >= 0),
    prerelease     TEXT,
    build_metadata TEXT,

    checksum       BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    size_bytes     BIGINT NOT NULL CHECK (size_bytes > 0),

    edition        TEXT,
    license        TEXT,
    description    TEXT,
    repository     TEXT,
    realm          TEXT NOT NULL DEFAULT 'shared' CHECK (realm IN ('shared', 'client', 'server')),
    authors        TEXT[] NOT NULL DEFAULT '{}',

    yanked_at      TIMESTAMPTZ,
    published_by   BIGINT NOT NULL REFERENCES users (id),
    published_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Version identity excludes build metadata, because semver precedence excludes it: 1.0.0+a
-- and 1.0.0+b are the same version, and a client receiving both could not tell them apart.
CREATE UNIQUE INDEX versions_identity
    ON versions (package_id, major, minor, patch, COALESCE(prerelease, ''));

CREATE INDEX versions_package ON versions (package_id);

CREATE TABLE dependencies (
    version_id  BIGINT NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    requirement TEXT NOT NULL,
    is_dev      BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (version_id, name)
);

CREATE TABLE downloads (
    version_id BIGINT NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    day        DATE NOT NULL,
    count      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, day)
);
