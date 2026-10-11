#!/usr/bin/env bash
# Homeport installer for Ubuntu 26.04 LTS.
#
#   curl -fsSL https://raw.githubusercontent.com/moreno001-create/homeport/main/install.sh | sudo bash
#
# Installs Docker, Caddy (from Caddy's own repository, for a current version) and Homeport, then sets up the main
# domain and the first admin. It asks at the terminal; for an unattended install, set these first:
#   HOMEPORT_DOMAIN=home.example.com HOMEPORT_ADMIN=alice HOMEPORT_PASSWORD='…' HOMEPORT_EMAIL=you@example.com
#   HOMEPORT_TLS=internal   (Caddy's own certificates, for a test machine with no public address)
#   HOMEPORT_VERSION=v0.3.0 (a release tag; default: the latest)
# Running it again updates Homeport and leaves the setup alone.
set -euo pipefail

REPO="moreno001-create/homeport"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mError:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this as root: curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo bash"
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
apt-get update -q
apt-get install -y -q ca-certificates curl gnupg docker.io docker-compose-v2

say "Installing Caddy"
if ! command -v caddy >/dev/null || ! caddy version | grep -q -E '^v2\.(8|9|[1-9][0-9])'; then
  if curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
     && curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt -o /etc/apt/sources.list.d/caddy-stable.list; then
    apt-get update -q
  else
    echo "Caddy's repository couldn't be reached; using Ubuntu's (older) Caddy instead."
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
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
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
  curl -fsSL "$base/homeport-linux-$arch" -o "$tmp/homeport" || die "couldn't download Homeport from $base"
  if curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
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
  if [ -n "$secret" ]; then
    read -r -s -p "$q: " val </dev/tty; echo >/dev/tty
  else
    read -r -p "$q${def:+ [$def]}: " val </dev/tty
  fi
  printf -v "$var" '%s' "${val:-$def}"
}
ask HOMEPORT_DOMAIN "Main domain (portal and admin panel; its DNS must point here), e.g. home.example.com"
ask HOMEPORT_ADMIN "First admin's user name"
ask HOMEPORT_EMAIL "Email for Let's Encrypt notices (optional)"
ask HOMEPORT_TLS "Certificates: letsencrypt (public server) or internal (test machine)" "letsencrypt"
while [ -z "${HOMEPORT_PASSWORD:-}" ] && $tty_ok; do
  ask HOMEPORT_PASSWORD "Password for ${HOMEPORT_ADMIN:-the admin} (at least 10 characters)" "" secret
  p2=""; read -r -s -p "Again: " p2 </dev/tty; echo >/dev/tty
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
