#!/usr/bin/env bash
#
# Everything after `cloudflared tunnel login`, which is the one step that cannot be
# scripted: it opens a browser and authorises against your Cloudflare account.
#
#   ./tunnel.sh packages.example.com
#   ./tunnel.sh packages.example.com packages.loom-lang.org
#
# Several hostnames are allowed and all of them serve. That is the migration path: a
# hostname you have published can never be retired, because lock files name it, but the
# registry returns no URLs of its own, so adding a second name breaks nothing and the old
# one keeps resolving for everybody holding a lock file that names it.
#
# Idempotent. Run it again after editing cloudflared.yml.template and it re-renders,
# re-validates and restarts, without making a second tunnel or duplicate DNS records.
#
#   TUNNEL_NAME=loom-packages    the tunnel to create or reuse
#   LOOM_ORIGIN=http://127.0.0.1:8090   where loomreg is listening

set -euo pipefail

[ $# -ge 1 ] || {
  echo "usage: tunnel.sh <hostname> [hostname...]" >&2
  echo "   eg: tunnel.sh packages.example.com" >&2
  exit 2
}

TUNNEL=${TUNNEL_NAME:-loom-packages}
ORIGIN=${LOOM_ORIGIN:-http://127.0.0.1:8090}
HOSTS=("$@")

CONFIG_DIR=/etc/cloudflared
TEMPLATE=$(dirname "$0")/cloudflared.yml.template

say() { printf '\n== %s\n' "$1"; }
die() { printf '\nerror: %s\n' "$1" >&2; exit 1; }

# A hostname with no dot is a name somebody typed without the domain, and it would create a
# DNS record in a zone they did not mean.
for host in "${HOSTS[@]}"; do
  case "$host" in
    *.*) ;;
    *) die "'$host' is not a fully qualified hostname" ;;
  esac
done

command -v cloudflared >/dev/null || die "cloudflared is not installed.
  Debian/Ubuntu: curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \\
    | sudo tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
    echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \\
    | sudo tee /etc/apt/sources.list.d/cloudflared.list
    sudo apt update && sudo apt install cloudflared"

[ -f "$HOME/.cloudflared/cert.pem" ] || [ -f "$CONFIG_DIR/cert.pem" ] || die "not logged in to Cloudflare.
  Run 'cloudflared tunnel login' first. It opens a browser and asks which zone to
  authorise, which is why this script cannot do it for you."

[ -f "$TEMPLATE" ] || die "$TEMPLATE is missing"

# ---------------------------------------------------------------- the tunnel

# Parsed, not grepped. `--output json` is pretty-printed, so matching "name":"x" against
# it silently finds nothing and the script would try to create a tunnel that exists.
tunnel_id() {
  cloudflared tunnel list --output json 2>/dev/null | python3 -c "
import json, sys
try:
    tunnels = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for t in tunnels:
    if t.get('name') == sys.argv[1] and str(t.get('deleted_at', '')).startswith('0001'):
        print(t['id'])
        break
" "$1"
}

TUNNEL_ID=$(tunnel_id "$TUNNEL")
if [ -n "$TUNNEL_ID" ]; then
  say "tunnel '$TUNNEL' already exists ($TUNNEL_ID), keeping it"
else
  say "creating tunnel '$TUNNEL'"
  cloudflared tunnel create "$TUNNEL"
  TUNNEL_ID=$(tunnel_id "$TUNNEL")
  [ -n "$TUNNEL_ID" ] || die "created '$TUNNEL' but cannot find its id"
fi

# A tunnel created in the dashboard, or on another machine, has no credentials file on this
# one. They are derivable from the account certificate, so this is a fetch rather than a
# copy — nothing secret has to be moved between machines by hand.
CREDENTIALS="$HOME/.cloudflared/$TUNNEL_ID.json"
if [ ! -f "$CREDENTIALS" ]; then
  say "no local credentials for $TUNNEL_ID, fetching them"
  mkdir -p "$HOME/.cloudflared"
  cloudflared tunnel token --cred-file "$CREDENTIALS" "$TUNNEL" >/dev/null \
    || die "could not fetch credentials for '$TUNNEL'.
  The account cert at $HOME/.cloudflared/cert.pem must be for the zone that owns it."
  chmod 600 "$CREDENTIALS"
fi

# ------------------------------------------------------------------ the DNS

for host in "${HOSTS[@]}"; do
  say "routing $host to '$TUNNEL'"
  route_log=$(mktemp)
  if ! cloudflared tunnel route dns "$TUNNEL" "$host" >"$route_log" 2>&1; then
    cat "$route_log"
    if grep -qi "already exists\|record with that host" "$route_log"; then
      echo "a DNS record for $host already exists."
      echo "if it is this tunnel's, nothing to do. if it points somewhere else, delete it"
      echo "in the Cloudflare dashboard, or re-run that one command with --overwrite-dns."
    else
      rm -f "$route_log"
      die "could not route $host"
    fi
  fi
  rm -f "$route_log"
done

# --------------------------------------------------------------- the config

# The per-hostname rule lives between the markers in the template, so its shape stays
# editable in one place rather than being spelled out here.
say "rendering $CONFIG_DIR/config.yml for ${#HOSTS[@]} hostname(s)"

head=$(sed -n '1,/^# >>>/p' "$TEMPLATE" | sed '$d')
rule=$(sed -n '/^# >>>/,/^# <<</p' "$TEMPLATE" | sed '1d;$d')
tail=$(sed -n '/^# <<</,$p' "$TEMPLATE" | sed '1d')

[ -n "$rule" ] || die "$TEMPLATE has no rule between its # >>> and # <<< markers"

rendered=$(mktemp)
{
  printf '%s\n' "$head"
  for host in "${HOSTS[@]}"; do
    printf '%s\n' "$rule" | sed -e "s|__HOSTNAME__|$host|g" -e "s|__ORIGIN__|$ORIGIN|g"
  done
  printf '%s\n' "$tail"
} | sed -e "s|__TUNNEL_NAME__|$TUNNEL|g" \
        -e "s|__CREDENTIALS_FILE__|$CONFIG_DIR/$TUNNEL_ID.json|g" > "$rendered"

grep -q '__' "$rendered" && { cat "$rendered"; rm -f "$rendered"; die "a placeholder was left unsubstituted"; }

sudo mkdir -p "$CONFIG_DIR"
sudo install -m 0600 "$CREDENTIALS" "$CONFIG_DIR/$TUNNEL_ID.json"
sudo install -m 0644 "$rendered" "$CONFIG_DIR/config.yml"
rm -f "$rendered"

# Validated before anything is started. An ingress rule that does not parse takes the
# tunnel down at restart, which is after the DNS records already point at it.
say "validating ingress"
cloudflared tunnel --config "$CONFIG_DIR/config.yml" ingress validate

# The origin is checked too, because a tunnel in front of nothing answers 502 to the world
# rather than failing where somebody is watching.
say "checking the origin at $ORIGIN"
if curl -fsS --max-time 5 "$ORIGIN/healthz" >/dev/null 2>&1; then
  echo "origin is up"
else
  echo "WARNING: $ORIGIN/healthz did not answer."
  echo "start loomreg first (systemctl start loomreg); the tunnel will 502 until it is up."
fi

# --------------------------------------------------------------- the service

say "installing the cloudflared service"
if systemctl list-unit-files | grep -q '^cloudflared\.service'; then
  sudo systemctl restart cloudflared
else
  sudo cloudflared service install
  sudo systemctl enable --now cloudflared
fi

sleep 3
systemctl is-active --quiet cloudflared || die "cloudflared did not stay up; journalctl -u cloudflared -n 50"

say "done"
echo "  tunnel:   $TUNNEL ($TUNNEL_ID)"
echo "  origin:   $ORIGIN"
for host in "${HOSTS[@]}"; do echo "  serving:  https://$host"; done
cat <<DONE

Check it from somewhere that is not this box:

  curl -fsS https://${HOSTS[0]}/healthz

Then set LOOM_BASE_URL=https://${HOSTS[0]} in /etc/loomreg.env, add
https://${HOSTS[0]}/v1/auth/github/callback to the GitHub OAuth app, and restart loomreg.
LOOM_BASE_URL picks the one hostname sign-in returns to; every hostname above still serves
packages. DNS can take a minute or two to propagate the first time.
DONE
