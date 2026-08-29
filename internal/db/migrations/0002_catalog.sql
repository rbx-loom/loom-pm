-- Browsing needs what resolution does not: a description to read, and a way to find a
-- package by something other than its exact name.

-- Description belongs to a version, but searching and listing want one per package, so the
-- most recent publish's is kept here. A package that then publishes an old patch release
-- carries that release's description until the next one, which is a staler subtitle rather
-- than a wrong answer.
ALTER TABLE packages ADD COLUMN description TEXT;

-- Generated rather than maintained, so it cannot drift from the columns it summarises. The
-- name is weighted above the description: somebody typing "serio" wants the package called
-- serio before every package that mentions it.
ALTER TABLE packages ADD COLUMN search tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(normalized, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(description, '')), 'B')
    ) STORED;

CREATE INDEX packages_search ON packages USING GIN (search);

-- A prefix match is what a search box needs and full text does not give: "ser" finds
-- nothing in a tsvector, and everybody types a prefix before they type a word.
CREATE INDEX packages_prefix ON packages (normalized text_pattern_ops);

-- Both listings order by it.
CREATE INDEX packages_updated ON packages (updated_at DESC);
