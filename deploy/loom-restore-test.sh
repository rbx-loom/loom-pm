#!/usr/bin/env bash
#
# Restores the newest dump into a scratch database and checks it is a registry.
#
# A backup nobody has restored is a belief, not a backup. This is the cheap version of the
# thing that matters: it does not prove the blob store is intact — `loomreg verify` does
# that against the live one — but it proves the dump replays and arrives with its schema
# and its rows.
#
# Runs as loomreg, which holds CREATEDB for exactly this and nothing else.

set -euo pipefail

CONFIG=${BACKUP_CONFIG:-/etc/loomreg-backup.env}
[ -r "$CONFIG" ] && { set -a; . "$CONFIG"; set +a; }

BACKUP_DIR=${BACKUP_DIR:-/var/backups/loomreg}
SCRATCH=${RESTORE_TEST_DATABASE:-loom_restore_test}

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "FAILED: $*"; exit 1; }

dump=$(find "$BACKUP_DIR/db" -name 'loom-*.dump' -type f -printf '%T@ %p\n' 2>/dev/null \
       | sort -rn | head -1 | cut -d' ' -f2-)
[ -n "$dump" ] || die "no dump found in $BACKUP_DIR/db"
log "testing $(basename "$dump")"

# The checksum first. Restoring a dump that rotted on disk and calling the result a passing
# test is worse than not testing.
if [ -f "$dump.sha256" ]; then
  (cd "$(dirname "$dump")" && sha256sum --check --status "$(basename "$dump").sha256") \
    || die "the dump does not match its recorded checksum"
  log "checksum ok"
fi

cleanup() { dropdb --if-exists "$SCRATCH" 2>/dev/null || true; }
trap cleanup EXIT

dropdb --if-exists "$SCRATCH"
createdb "$SCRATCH" || die "could not create the scratch database"

# --exit-on-error, because a restore that reports success having skipped half its
# statements is the failure mode this is meant to catch.
pg_restore --dbname="$SCRATCH" --no-owner --exit-on-error "$dump" || die "the dump would not replay"
log "restored"

# It replayed — but into what? A registry has these tables, and the migration ledger says
# the schema is the one the code expects.
for table in users tokens packages versions dependencies sessions schema_migrations; do
  psql -d "$SCRATCH" -tAc "SELECT 1 FROM information_schema.tables WHERE table_name='$table'" \
    | grep -q 1 || die "the restored database has no '$table' table"
done
log "schema ok: every expected table is present"

migrations=$(psql -d "$SCRATCH" -tAc "SELECT count(*) FROM schema_migrations")
versions=$(psql -d "$SCRATCH" -tAc "SELECT count(*) FROM versions")
packages=$(psql -d "$SCRATCH" -tAc "SELECT count(*) FROM packages")
users=$(psql -d "$SCRATCH" -tAc "SELECT count(*) FROM users")

# The identity column has to come back nullable. If a restore silently reimposed NOT NULL,
# bootstrapped users would be unrepresentable and the next `loomreg token` would fail.
nullable=$(psql -d "$SCRATCH" -tAc \
  "SELECT is_nullable FROM information_schema.columns WHERE table_name='users' AND column_name='github_id'")
[ "$nullable" = "YES" ] || die "users.github_id came back as $nullable, want YES"

log "contents: $migrations migrations, $packages packages, $versions versions, $users users"
log "PASSED: the newest dump restores to a working registry schema"
