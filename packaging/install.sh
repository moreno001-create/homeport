#!/usr/bin/env bash
# Installs Homeport on Ubuntu 26.04 LTS: Docker, Caddy (from Caddy's own repository, for a current version), the
# homeport binary and its service. Run as root from the folder holding this script and the homeport binaries.
set -euo pipefail

say() { printf '\n==> %s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this as root (sudo ./install.sh)"
. /etc/os-release
[ "${ID:-}" = ubuntu ] || die "this installer is for Ubuntu (found ${PRETTY_NAME:-unknown})"
case "${VERSION_ID:-}" in
  26.04) ;;
  *) echo "Warning: Homeport is made for Ubuntu 26.04 LTS; this is ${PRETTY_NAME}. Continuing anyway." ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported processor $(uname -m)" ;;
esac
bin="$here/homeport-linux-$arch"
[ -x "$bin" ] || bin="$here/homeport"
[ -x "$bin" ] || die "can't find the homeport binary next to this script (homeport-linux-$arch)"

say "Installing Docker and the packages Homeport needs"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q ca-certificates curl gnupg docker.io docker-compose-v2

say "Installing Caddy"
if ! command -v caddy >/dev/null || ! caddy version | grep -q '^v2\.[89]\|^v2\.1[0-9]'; then
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
install -m 0755 "$bin" /usr/local/bin/homeport
install -m 0644 "$here/homeport.service" /etc/systemd/system/homeport.service
systemctl daemon-reload
systemctl enable --now docker caddy
systemctl enable homeport
systemctl restart homeport

if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
  say "Opening ports 80 and 443 in the firewall"
  ufw allow 80/tcp && ufw allow 443/tcp
fi

for i in $(seq 1 20); do [ -S /run/homeport/homeport.sock ] && break; sleep 0.5; done
[ -S /run/homeport/homeport.sock ] || die "the homeport service didn't start (journalctl -u homeport)"

say "Done"
if homeport status | grep -q "Not set up"; then
  cat <<'MSG'
Next, set up the main domain (its DNS must point at this server) and the first admin:

  sudo homeport init --domain home.example.com --admin yourname --email you@example.com

On a test machine without a public address, add --tls internal to use Caddy's own certificates.
MSG
else
  homeport status
fi
