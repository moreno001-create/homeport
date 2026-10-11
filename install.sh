#!/usr/bin/env bash
# Homeport installer for Ubuntu 26.04 LTS.
#
#   curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash
#
# Installs Docker, Caddy (from Caddy's own repository, for a current version) and Homeport, then sets up the main
# domain and the first admin. It asks at the terminal; for an unattended install, set these first:
#   HOMEPORT_DOMAIN=home.example.com HOMEPORT_ADMIN=alice HOMEPORT_PASSWORD='…' HOMEPORT_EMAIL=you@example.com
#   HOMEPORT_TLS=internal   (Caddy's own certificates, for a test machine with no public address)
#   HOMEPORT_VERSION=v0.3.0 (a release tag; default: the latest)
# Running it again updates Homeport and leaves the setup alone.
set -Eeuo pipefail

REPO="moreno001-create/homeport"
CADDY_REPO="${HOMEPORT_CADDY_REPO:-https://dl.cloudsmith.io/public/caddy/stable}"
LOG=/var/log/homeport-install.log

step="starting"
say() { step="$*"; printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mError:\033[0m %s\n' "$*" >&2; printf 'The whole log is in %s\n' "$LOG" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || { echo "Run this as root: curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash" >&2; exit 1; }
# Everything also goes to a log, and a failure says which step it was in.
# (The terminal's own output is kept as fds 3 and 4, and the installer waits for tee at the end, so the last lines
# always appear before the command prompt comes back.)
exec 3>&1 4>&2
exec > >(tee -a "$LOG") 2>&1
tee_pid=$!
echo "Homeport installer, $(date)"
trap 'printf "\n\033[31mThe installer stopped during: %s\033[0m\nThe whole log is in %s\n" "$step" "$LOG" >&2' ERR
tmp="$(mktemp -d)"
finish() { rm -rf "$tmp"; exec 1>&3 2>&4; wait "$tee_pid" 2>/dev/null || true; }
trap finish EXIT

# fetch URL FILE WHAT: download, retrying a few times, and say plainly what failed.
fetch() {
  local code
  code="$(curl -sSL --retry 3 --retry-delay 2 -w '%{http_code}' -o "$2" "$1" 2>/dev/null)" || code="${code:-no answer}"
  case "$code" in
    2??) return 0 ;;
    401|403) echo "Couldn't download $3: $1 refused it (HTTP $code). A proxy or filter on this network, or the server limiting this address, can cause that." ;;
    *) echo "Couldn't download $3 from $1 (HTTP $code)." ;;
  esac
  return 1
}

# apt-get update; if Caddy's repository (from an earlier run) refuses, drop it and use Ubuntu's Caddy.
apt_update() {
  apt-get update -q && return 0
  if [ -f /etc/apt/sources.list.d/caddy-stable.list ]; then
    echo "Caddy's package repository refused the update; removing it (Ubuntu's own Caddy will be used)."
    rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    apt-get update -q && return 0
  fi
  return 1
}
. /etc/os-release
[ "${ID:-}" = ubuntu ] || die "Homeport is for Ubuntu (this is ${PRETTY_NAME:-unknown})"
case "${VERSION_ID:-}" in
  26.04) ;;
  *) echo "Warning: Homeport is made for Ubuntu 26.04 LTS; this is ${PRETTY_NAME}. Continuing anyway." ;;
esac
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported processor $(uname -m) (Homeport runs on x86-64 and ARM64)" ;;
esac

say "Installing Docker and the packages Homeport needs"
export DEBIAN_FRONTEND=noninteractive
apt_update || die "apt-get update failed (see the lines above); check this server's internet connection and apt sources"
apt-get install -y -q ca-certificates curl gnupg docker.io docker-compose-v2

say "Installing Caddy"
# Caddy's own repository has current versions (Ubuntu's is older); if it can't be used, Ubuntu's Caddy works too.
caddy_repo() {
  fetch "$CADDY_REPO/gpg.key" "$tmp/caddy.key" "Caddy's signing key" || return 1
  gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg "$tmp/caddy.key" || return 1
  fetch "$CADDY_REPO/debian.deb.txt" "$tmp/caddy.list" "Caddy's package list" || return 1
  cp "$tmp/caddy.list" /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q || return 1
}
if ! command -v caddy >/dev/null || ! caddy version | grep -q -E '^v2\.(8|9|[1-9][0-9])'; then
  if ! caddy_repo; then
    echo "Using Ubuntu's own (older) Caddy instead of Caddy's repository."
    rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    apt_update || die "apt-get update failed"
  fi
  apt-get install -y -q caddy
fi
caddy version

say "Installing Homeport"
# A local build is only used when this script is run as a file next to it (curl | bash always downloads).
here=/nonexistent
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi
if [ -x "$here/homeport-linux-$arch" ]; then
  cp "$here/homeport-linux-$arch" "$tmp/homeport"            # a local build next to this script
elif [ -x "$here/dist/homeport-linux-$arch" ]; then
  cp "$here/dist/homeport-linux-$arch" "$tmp/homeport"
else
  if [ -n "${HOMEPORT_VERSION:-}" ]; then
    base="https://github.com/$REPO/releases/download/$HOMEPORT_VERSION"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  fetch "$base/homeport-linux-$arch" "$tmp/homeport" "Homeport" || die "couldn't download Homeport (see above)"
  if fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" "Homeport's checksums"; then
    want="$(awk -v f="homeport-linux-$arch" '$2==f{print $1}' "$tmp/SHA256SUMS")"
    have="$(sha256sum "$tmp/homeport" | awk '{print $1}')"
    [ -n "$want" ] && [ "$want" = "$have" ] || die "the download's checksum doesn't match; not installing it"
    echo "Checksum OK."
  fi
fi
install -m 0755 "$tmp/homeport" /usr/local/bin/homeport
homeport version

cat > /etc/systemd/system/homeport.service <<'UNIT'
[Unit]
Description=Homeport (apps, users and domains for this server)
Documentation=https://github.com/moreno001-create/homeport
After=network-online.target docker.service caddy.service
Wants=network-online.target docker.service caddy.service

[Service]
Type=simple
ExecStart=/usr/local/bin/homeport serve
Restart=on-failure
RestartSec=3
RuntimeDirectory=homeport
RuntimeDirectoryMode=0700
StateDirectory=homeport
StateDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now docker caddy
systemctl enable homeport
systemctl restart homeport

if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
  say "Opening ports 80 and 443 in the firewall"
  ufw allow 80/tcp && ufw allow 443/tcp
fi

for _ in $(seq 1 40); do [ -S /run/homeport/homeport.sock ] && break; sleep 0.5; done
[ -S /run/homeport/homeport.sock ] || die "the Homeport service didn't start (see: journalctl -u homeport)"

if ! homeport status | grep -q "Not set up"; then
  say "Homeport is updated"
  homeport status
  exit 0
fi

# MARK: First setup (like YunoHost's post-install)
say "Setting up this server"
# A terminal to ask at? (/dev/tty can exist without being usable, e.g. over a non-interactive ssh or in CI.)
tty_ok=false
if { exec 3</dev/tty; } 2>/dev/null; then tty_ok=true; exec 3<&-; fi
ask() { # ask VAR "question" [default] [secret]
  local var=$1 q=$2 def=${3:-} secret=${4:-} val=""
  if [ -n "${!var:-}" ]; then return 0; fi
  $tty_ok || return 0
  # Questions go straight to the terminal (not through the log), so they always show.
  if [ -n "$secret" ]; then
    printf '%s: ' "$q" >/dev/tty; read -r -s val </dev/tty; echo >/dev/tty
  else
    printf '%s%s: ' "$q" "${def:+ [$def]}" >/dev/tty; read -r val </dev/tty
    printf '%s: %s\n' "$q" "${val:-$def}" >>"$LOG"   # into the log only (never passwords)
  fi
  printf -v "$var" '%s' "${val:-$def}"
}
ask HOMEPORT_DOMAIN "Main domain (portal and admin panel; its DNS must point here), e.g. home.example.com"
ask HOMEPORT_ADMIN "First admin's user name"
ask HOMEPORT_EMAIL "Email for Let's Encrypt notices (optional)"
ask HOMEPORT_TLS "Certificates: letsencrypt (public server) or internal (test machine)" "letsencrypt"
while [ -z "${HOMEPORT_PASSWORD:-}" ] && $tty_ok; do
  ask HOMEPORT_PASSWORD "Password for ${HOMEPORT_ADMIN:-the admin} (at least 10 characters)" "" secret
  p2=""; printf 'Again: ' >/dev/tty; read -r -s p2 </dev/tty; echo >/dev/tty
  if [ "$HOMEPORT_PASSWORD" != "$p2" ]; then echo "They didn't match; try again."; HOMEPORT_PASSWORD=""; fi
done

if [ -z "${HOMEPORT_DOMAIN:-}" ] || [ -z "${HOMEPORT_ADMIN:-}" ] || [ -z "${HOMEPORT_PASSWORD:-}" ]; then
  cat <<MSG

Homeport is installed. Finish setting it up with:

  sudo homeport init --domain home.example.com --admin yourname --email you@example.com

(add --tls internal on a test machine without a public address)
MSG
  exit 0
fi

printf '%s\n' "$HOMEPORT_PASSWORD" | homeport init --domain "$HOMEPORT_DOMAIN" --admin "$HOMEPORT_ADMIN" \
  --email "${HOMEPORT_EMAIL:-}" --tls "${HOMEPORT_TLS:-letsencrypt}"
cat <<MSG

Done. Sign in at https://$HOMEPORT_DOMAIN/admin as $HOMEPORT_ADMIN.
Install apps from the admin panel, or: sudo homeport app catalog
Connect an AI agent (opencode, Claude Code…): sudo homeport mcp
MSG
