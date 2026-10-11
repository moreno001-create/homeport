#!/usr/bin/env bash
# Homeport installer for macOS (Apple Silicon or Intel), run as yourself (not with sudo):
#
#   curl -fsSL https://moreno001-create.github.io/homeport/install-mac.sh | bash
#
# Sets up Caddy (Homebrew), a Docker runtime (uses OrbStack, Docker Desktop or Colima if one is there, else installs
# Colima), and Homeport, all running as your user with launchd, so they start when you log in. macOS lets any user
# serve on ports 80 and 443, so nothing needs root. Then it sets up the main domain and the first admin, and looks for
# a local AI model runtime (Ollama or LM Studio) for Homeport's assistant.
#
# Unattended: HOMEPORT_DOMAIN, HOMEPORT_ADMIN, HOMEPORT_PASSWORD, HOMEPORT_EMAIL, HOMEPORT_TLS=internal,
# HOMEPORT_VERSION (a release tag), HOMEPORT_INSTALL_OLLAMA=yes. Running it again updates Homeport.
set -euo pipefail

REPO="moreno001-create/homeport"
LABEL="dev.homeport"
STATE="$HOME/Library/Application Support/Homeport"
AGENTS="$HOME/Library/LaunchAgents"
UID_NUM="$(id -u)"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mError:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Darwin ] || die "this installer is for macOS; on Ubuntu use install.sh"
[ "$UID_NUM" -ne 0 ] || die "run this as yourself, not with sudo: Homeport runs as your user on a Mac"
major="$(sw_vers -productVersion | cut -d. -f1)"
[ "$major" -ge 13 ] || die "Homeport needs macOS 13 or newer (this is $(sw_vers -productVersion))"
case "$(uname -m)" in
  arm64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) die "unsupported processor $(uname -m)" ;;
esac

# MARK: Homebrew and Caddy
command -v brew >/dev/null || [ -x /opt/homebrew/bin/brew ] || [ -x /usr/local/bin/brew ] \
  || die "Homeport uses Homebrew to install Caddy. Install it from https://brew.sh, then run this again."
BREW="$(command -v brew || ls /opt/homebrew/bin/brew /usr/local/bin/brew 2>/dev/null | head -1)"
BREW_PREFIX="$("$BREW" --prefix)"
export PATH="$BREW_PREFIX/bin:$HOME/.orbstack/bin:/usr/local/bin:$PATH"

say "Installing Caddy"
command -v caddy >/dev/null || "$BREW" install caddy
caddy version

# MARK: Docker runtime
say "Checking for a Docker runtime"
docker_ok() { command -v docker >/dev/null && docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; }
if ! docker_ok; then
  if [ -d /Applications/OrbStack.app ]; then
    echo "Starting OrbStack…"; open -ga OrbStack
  elif [ -d /Applications/Docker.app ]; then
    echo "Starting Docker Desktop…"; open -ga Docker
  else
    echo "No Docker runtime found: installing Colima (open source) with Homebrew."
    "$BREW" install colima docker docker-compose
    mkdir -p "$HOME/.docker"
    if ! grep -q cliPluginsExtraDirs "$HOME/.docker/config.json" 2>/dev/null; then
      python3 - "$HOME/.docker/config.json" "$BREW_PREFIX/lib/docker/cli-plugins" <<'PY'
import json, os, sys
p, plugins = sys.argv[1], sys.argv[2]
cfg = json.load(open(p)) if os.path.exists(p) else {}
cfg.setdefault("cliPluginsExtraDirs", [])
if plugins not in cfg["cliPluginsExtraDirs"]:
    cfg["cliPluginsExtraDirs"].append(plugins)
json.dump(cfg, open(p, "w"), indent=2)
PY
    fi
    mem=$(( $(sysctl -n hw.memsize) / 1073741824 / 4 )); [ "$mem" -ge 4 ] || mem=4
    colima start --cpu "$(( $(sysctl -n hw.ncpu) / 2 ))" --memory "$mem" --vm-type vz --mount-type virtiofs \
      --mount "$HOME:w"
    "$BREW" services start colima >/dev/null 2>&1 || true   # start again at login
  fi
  for _ in $(seq 1 60); do docker_ok && break; sleep 2; done
fi
docker_ok || die "Docker isn't answering. Start your Docker runtime (OrbStack, Docker Desktop or Colima), then run this again."
docker info --format 'Docker {{.ServerVersion}} ({{.Name}})'
DOCKER_DIR="$(dirname "$(command -v docker)")"

# MARK: Ports 80 and 443
say "Checking the ports Homeport needs"
# 80/443 for the web, 2019 for Caddy's admin port, 8090 for Homeport itself (all but the first two on localhost).
holders="$(lsof -nP -iTCP:80 -iTCP:443 -iTCP:2019 -iTCP:8090 -sTCP:LISTEN 2>/dev/null | awk 'NR>1 && $1!="caddy" && $1!="homeport" {print $1}' | sort -u | tr '\n' ' ')"
if [ -n "$holders" ] && [ -z "${HOMEPORT_SKIP_PORT_CHECK:-}" ]; then
  msg="Ports Homeport needs (80, 443, 2019, 8090) are in use by: $holders."
  case "$holders" in
    *OrbStack*) msg="$msg
OrbStack holds them for its *.orb.local addresses, or for a Linux machine it forwards ports from. Turn that off:
  orb config set network.https false
  orb config set machines.forward_ports false
and restart OrbStack, then run this again." ;;
  esac
  die "$msg"
fi

# MARK: Homeport
say "Installing Homeport"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
here=/nonexistent
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"; fi
if [ -x "$here/homeport-darwin-$arch" ]; then
  cp "$here/homeport-darwin-$arch" "$tmp/homeport"
elif [ -x "$here/dist/homeport-darwin-$arch" ]; then
  cp "$here/dist/homeport-darwin-$arch" "$tmp/homeport"
else
  if [ -n "${HOMEPORT_VERSION:-}" ]; then base="https://github.com/$REPO/releases/download/$HOMEPORT_VERSION"
  else base="https://github.com/$REPO/releases/latest/download"; fi
  curl -fsSL "$base/homeport-darwin-$arch" -o "$tmp/homeport" || die "couldn't download Homeport from $base"
  if curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
    want="$(awk -v f="homeport-darwin-$arch" '$2==f{print $1}' "$tmp/SHA256SUMS")"
    have="$(shasum -a 256 "$tmp/homeport" | awk '{print $1}')"
    [ -n "$want" ] && [ "$want" = "$have" ] || die "the download's checksum doesn't match; not installing it"
    echo "Checksum OK."
  fi
fi
install -m 0755 "$tmp/homeport" "$BREW_PREFIX/bin/homeport"
homeport version

mkdir -p "$STATE/logs" "$AGENTS"
chmod 700 "$STATE"
[ -f "$STATE/Caddyfile" ] || printf '{\n\tadmin localhost:2019\n}\n' > "$STATE/Caddyfile"
chmod 644 "$STATE/Caddyfile"

PATH_FOR_AGENTS="$BREW_PREFIX/bin:$DOCKER_DIR:$HOME/.orbstack/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
agent() { # agent NAME PROGRAM ARGS...
  local name=$1; shift
  local plist="$AGENTS/$LABEL.$name.plist" args=""
  for a in "$@"; do args="$args<string>$a</string>"; done
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL.$name</string>
  <key>ProgramArguments</key><array>$args</array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>$PATH_FOR_AGENTS</string>
    <key>HOME</key><string>$HOME</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$STATE/logs/$name.log</string>
  <key>StandardErrorPath</key><string>$STATE/logs/$name.log</string>
</dict></plist>
PLIST
  launchctl bootout "gui/$UID_NUM/$LABEL.$name" 2>/dev/null || true
  launchctl bootstrap "gui/$UID_NUM" "$plist"
}
agent caddy "$(command -v caddy)" run --config "$STATE/Caddyfile" --adapter caddyfile
agent homeport "$BREW_PREFIX/bin/homeport" serve

for _ in $(seq 1 40); do [ -S "$STATE/homeport.sock" ] && break; sleep 0.5; done
[ -S "$STATE/homeport.sock" ] || die "Homeport didn't start (see $STATE/logs/homeport.log)"

# MARK: Local AI
say "Local AI for Homeport's assistant"
if command -v ollama >/dev/null || [ -d /Applications/Ollama.app ]; then
  echo "Ollama is installed. Models: $(ollama list 2>/dev/null | awk 'NR>1{print $1}' | tr '\n' ' ')"
elif command -v lms >/dev/null || [ -x "$HOME/.lmstudio/bin/lms" ] || [ -d "/Applications/LM Studio.app" ]; then
  echo "LM Studio is installed: start its local server (lms server start) for Homeport's assistant."
elif [ "${HOMEPORT_INSTALL_OLLAMA:-}" = yes ]; then
  "$BREW" install ollama && "$BREW" services start ollama
else
  echo "No local model runtime found. For the assistant to use local models, install Ollama (brew install ollama) or LM Studio."
fi
gb=$(( $(sysctl -n hw.memsize) / 1073741824 ))
echo "This Mac has ${gb} GB of memory: models up to about $(( gb * 6 / 10 )) GB fit comfortably."

if ! homeport status | grep -q "Not set up"; then
  say "Homeport is updated"
  homeport status
  exit 0
fi

# MARK: First setup
say "Setting up Homeport"
tty_ok=false
if { exec 3</dev/tty; } 2>/dev/null; then tty_ok=true; exec 3<&-; fi
ask() {
  local var=$1 q=$2 def=${3:-} secret=${4:-} val=""
  if [ -n "${!var:-}" ]; then return 0; fi
  $tty_ok || return 0
  if [ -n "$secret" ]; then read -r -s -p "$q: " val </dev/tty; echo >/dev/tty
  else read -r -p "$q${def:+ [$def]}: " val </dev/tty; fi
  printf -v "$var" '%s' "${val:-$def}"
}
ask HOMEPORT_DOMAIN "Main domain (portal and admin panel; its DNS must point at this Mac's public address), e.g. home.example.com"
ask HOMEPORT_ADMIN "First admin's user name"
ask HOMEPORT_EMAIL "Email for Let's Encrypt notices (optional)"
ask HOMEPORT_TLS "Certificates: letsencrypt (reachable from the internet) or internal (just this network)" "letsencrypt"
while [ -z "${HOMEPORT_PASSWORD:-}" ] && $tty_ok; do
  ask HOMEPORT_PASSWORD "Password for ${HOMEPORT_ADMIN:-the admin} (at least 10 characters)" "" secret
  p2=""; read -r -s -p "Again: " p2 </dev/tty; echo >/dev/tty
  if [ "$HOMEPORT_PASSWORD" != "$p2" ]; then echo "They didn't match; try again."; HOMEPORT_PASSWORD=""; fi
done
if [ -z "${HOMEPORT_DOMAIN:-}" ] || [ -z "${HOMEPORT_ADMIN:-}" ] || [ -z "${HOMEPORT_PASSWORD:-}" ]; then
  printf '\nHomeport is installed. Finish with:\n\n  homeport init --domain home.example.com --admin yourname --email you@example.com\n'
  exit 0
fi
printf '%s\n' "$HOMEPORT_PASSWORD" | homeport init --domain "$HOMEPORT_DOMAIN" --admin "$HOMEPORT_ADMIN" \
  --email "${HOMEPORT_EMAIL:-}" --tls "${HOMEPORT_TLS:-letsencrypt}"
cat <<MSG

Done. Sign in at https://$HOMEPORT_DOMAIN/admin as $HOMEPORT_ADMIN.
To keep serving, this Mac must stay awake and logged in (System Settings > Energy: prevent automatic sleeping;
Users & Groups: automatic login). Ports 80 and 443 must be forwarded to it from your router.
MSG
