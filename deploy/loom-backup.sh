#!/usr/bin/env bash
#
# Nightly backup of the registry: the database as a dump, the blob store as an add-only
# copy. Both halves, because neither restores anything on its own — blobs are
# content-addressed, which makes them verifiable, not re-derivable. A dump restored beside
# an empty blob root is a registry whose every version 500s.
#
# Config comes from /etc/loomreg-backup.env. With no BACKUP_DESTINATION set it still runs
# and still keeps local dumps, but it says clearly that a copy on the same disk is not a
# backup.

set -euo pipefail

CONFIG=${BACKUP_CONFIG:-/etc/loomreg-backup.env}
if [ -r "$CONFIG" ]; then
  # The environment wins over the file. A value passed on the command line is somebody
  # overriding a setting on purpose, usually to test one; a file that quietly won would let
  # that override look applied when it was not, which is how a test passes against the
  # wrong database.
  overrides=$(declare -p BACKUP_DATABASE BACKUP_DESTINATION BACKUP_DIR BACKUP_KEEP_DAYS \
                         BACKUP_LOCAL_ONLY LOOM_BLOB_ROOT 2>/dev/null || true)
  set -a; . "$CONFIG"; set +a
  [ -n "$overrides" ] && eval "$overrides"
fi

DATABASE=${BACKUP_DATABASE:-loom}
BLOB_ROOT=${LOOM_BLOB_ROOT:-/var/lib/loomreg/blobs}
BACKUP_DIR=${BACKUP_DIR:-/var/backups/loomreg}
KEEP_DAYS=${BACKUP_KEEP_DAYS:-30}
DESTINATION=${BACKUP_DESTINATION:-}

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "FAILED: $*"; exit 1; }

mkdir -p "$BACKUP_DIR/db"

# Two backups overlapping would have them writing the same paths, and a slow ship must not
# be joined by the next night's. Non-blocking: if one is still running, this run steps aside.
#
# No `2>/dev/null` on this exec: a redirection given to `exec` applies to the shell itself,
# so silencing it here would discard the stderr of everything that follows — pg_dump, rsync,
# rclone — and leave a backup that fails every night without saying so.
exec 9>"$BACKUP_DIR/.lock"
flock -n 9 || die "another backup is still running"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump="$BACKUP_DIR/db/loom-$stamp.dump"

# ------------------------------------------------------------------ the dump

log "dumping $DATABASE"
# Custom format: compresses, restores selectively, and can be listed without replaying.
pg_dump --format=custom --file="$dump" "$DATABASE" || die "pg_dump"

# Read back before it is trusted. A dump nobody can open is one whose unreadability is
# discovered on the day it is needed.
pg_restore --list "$dump" >/dev/null || die "the dump just written cannot be read"
sha256sum "$dump" > "$dump.sha256"

size=$(du -h "$dump" | cut -f1)
log "dump ok: $(basename "$dump") ($size)"

# ----------------------------------------------------------------- the blobs

# Counted rather than assumed: a blob root that has silently become empty, or unreadable,
# is the failure this whole script exists to survive. Its stderr is deliberately not
# discarded — with `pipefail` a swallowed find failure would end the run with no reason
# given, which is the one outcome worse than the failure itself.
[ -d "$BLOB_ROOT" ] || die "the blob root $BLOB_ROOT does not exist"
[ -r "$BLOB_ROOT" ] || die "the blob root $BLOB_ROOT is not readable by $(id -un)"
blobs=$(find "$BLOB_ROOT" -type f | wc -l) || die "could not read the blob root $BLOB_ROOT"
log "blob root holds $blobs files"

# ---------------------------------------------------------------- the pruning

# Local dumps only. Blobs are never pruned anywhere: every one of them is referenced by a
# version that is immutable and may be resolved by a lock file written years ago.
pruned=$(find "$BACKUP_DIR/db" -name 'loom-*.dump' -mtime "+$KEEP_DAYS" -print -delete | wc -l)
find "$BACKUP_DIR/db" -name 'loom-*.dump.sha256' -mtime "+$KEEP_DAYS" -delete
[ "$pruned" -gt 0 ] && log "pruned $pruned dump(s) older than $KEEP_DAYS days"

# --------------------------------------------------------------- the shipping

if [ -z "$DESTINATION" ]; then
  log "WARNING: BACKUP_DESTINATION is not set."
  log "WARNING: dumps are on the same disk as the thing they back up, which survives a"
  log "WARNING: mistaken DELETE but not a lost disk. This is not yet a backup."

  # An empty registry loses nothing, so local-only is a fair choice while it is empty. Once
  # somebody has published, that choice has different consequences and stops being one
  # anybody made deliberately — so it is escalated from a line in a log nobody reads to a
  # unit that shows up in `systemctl --failed`.
  #
  # Set BACKUP_LOCAL_ONLY=yes to say you meant it. That is a decision worth having to write
  # down, rather than one that arrives by nobody noticing.
  published=$(psql -d "$DATABASE" -tAc 'SELECT count(*) FROM versions' 2>/dev/null || echo 0)
  if [ "${published:-0}" -gt 0 ] && [ "${BACKUP_LOCAL_ONLY:-}" != "yes" ]; then
    log "$published published version(s) now exist and there is no copy off this box."
    die "set BACKUP_DESTINATION, or BACKUP_LOCAL_ONLY=yes to accept losing them with the disk"
  fi

  log "done (local only)"
  exit 0
fi

case "$DESTINATION" in
  rclone:*)
    # rclone remote — R2, S3, B2, anything it speaks. `copy` never deletes at the far end.
    remote=${DESTINATION#rclone:}
    command -v rclone >/dev/null || die "BACKUP_DESTINATION names rclone but it is not installed"
    log "shipping the dump to $remote"
    rclone copy "$dump" "$remote/db/" || die "rclone: dump"
    rclone copy "$dump.sha256" "$remote/db/" || die "rclone: checksum"
    log "shipping the blob store to $remote"
    rclone copy "$BLOB_ROOT/" "$remote/blobs/" || die "rclone: blobs"
    ;;
  *:*)
    # rsync over ssh — user@host:/path
    command -v rsync >/dev/null || die "rsync is not installed"
    log "shipping the dump to $DESTINATION"
    rsync --archive --partial "$dump" "$dump.sha256" "$DESTINATION/db/" || die "rsync: dump"
    log "shipping the blob store to $DESTINATION"
    # No --delete, deliberately: a sweep on the live box must never propagate into the
    # backup, which is the copy that has to survive `loomreg sweep --delete` run in anger.
    rsync --archive --partial "$BLOB_ROOT/" "$DESTINATION/blobs/" || die "rsync: blobs"
    ;;
  *)
    die "BACKUP_DESTINATION '$DESTINATION' is neither rclone:remote:path nor user@host:/path"
    ;;
esac

log "done: dump + $blobs blobs shipped to $DESTINATION"
