-- A user bootstrapped with `loomreg token ada` has no GitHub identity yet, and used to
-- carry a negative placeholder in the column that holds one. The placeholder was a value
-- standing in for the absence of a value, which is what NULL is for: it leaked into every
-- query that had to know what "no identity yet" looked like, and nothing in the schema
-- said the number was not an identity.
ALTER TABLE users ALTER COLUMN github_id DROP NOT NULL;

UPDATE users SET github_id = NULL WHERE github_id < 0;

-- Stated rather than assumed: a real GitHub id is positive, so anything else in this
-- column is a placeholder somebody reintroduced.
ALTER TABLE users ADD CONSTRAINT users_github_id_real
    CHECK (github_id IS NULL OR github_id > 0);

-- UNIQUE already permits many NULLs, which is exactly the rule wanted here: any number of
-- users may be waiting for an identity, and no two may share one.
