#!/usr/bin/env bash
# Homeport installer for macOS (Apple Silicon or Intel), run as yourself (not with sudo):
#
#   curl -fsSL https://moreno001-create.github.io/homeport/install-mac.sh | bash
#
# Sets up Caddy (Homebrew), a Docker runtime (uses OrbStack, Docker Desktop or Colima if one is there, else installs
# Colima), and Homeport, all running as your user with launchd, so they start when you log in. macOS lets any user
# serve on ports 80 and 443, so nothing needs root. Then it sets up the main domain and the first admin, and looks for
# a local AI model for Homeport's assistant: LM Studio's if it has one, else Ollama with a model that fits this Mac.
#
# Unattended: HOMEPORT_DOMAIN, HOMEPORT_ADMIN, HOMEPORT_PASSWORD, HOMEPORT_EMAIL, HOMEPORT_TLS=internal,
# HOMEPORT_VERSION (a release tag). Local AI: HOMEPORT_LOCAL_AI=no (skip), HOMEPORT_LOCAL_MODEL (which model),
# HOMEPORT_LOCAL_URL (a model server on another computer). Running it again updates Homeport.
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
    <key>HOME</key><string>$HOME</string>${AGENT_ENV:-}
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
# A local model for the assistant chat, loaded and connected: LM Studio's if it has a model for it, else Ollama (from
# Homebrew) with a model that fits this Mac's memory. HOMEPORT_LOCAL_AI=no skips it, HOMEPORT_LOCAL_MODEL picks the
# model, and HOMEPORT_LOCAL_URL connects a model server on another computer instead.
OLLAMA=http://127.0.0.1:11434
LMSTUDIO=http://127.0.0.1:1234
local_model=""
mem_gb=$(( $(sysctl -n hw.memsize) / 1073741824 ))
LMS="$(command -v lms || ls "$HOME/.lmstudio/bin/lms" 2>/dev/null || true)"

# lmstudio_pick: the biggest model LM Studio has that's trained for tools and fits in about 60% of memory.
lmstudio_pick() {
  "$LMS" ls --llm --json 2>/dev/null | /usr/bin/python3 -c '
import json, sys
fit = int(sys.argv[1]) * 0.6 * 2**30
ok = [m for m in json.load(sys.stdin) if m.get("trainedForToolUse") and m.get("sizeBytes", 0) <= fit]
if ok: print(max(ok, key=lambda m: m["sizeBytes"])["modelKey"])' "$mem_gb" 2>/dev/null
}

local_ai() {
  if [ "${HOMEPORT_LOCAL_AI:-yes}" = no ]; then return 0; fi
  local now model
  # (Homeport before 0.7.1 can't connect a local model.)
  if ! now="$(homeport assistant local 2>/dev/null)"; then echo "This Homeport version can't connect a local model yet."; return 1; fi
  if [[ $now == "Local model: "* ]]; then local_model="${now#Local model: }"; echo "The assistant already uses $local_model"; return 0; fi
  say "A local AI model for Homeport's assistant"
  if [ -n "${HOMEPORT_LOCAL_URL:-}" ]; then
    homeport assistant local "$HOMEPORT_LOCAL_URL" ${HOMEPORT_LOCAL_MODEL:+--model "$HOMEPORT_LOCAL_MODEL"} || return 1
    local_model="$(homeport assistant local)"; local_model="${local_model#Local model: }"
    return 0
  fi
  echo "This Mac has ${mem_gb} GB of memory."
  if [ -n "$LMS" ] && model="${HOMEPORT_LOCAL_MODEL:-$(lmstudio_pick)}" && [ -n "$model" ] && "$LMS" ls --llm --json 2>/dev/null | grep -q "\"modelKey\":\"$model\""; then
    echo "Using LM Studio's $model."
    "$LMS" server start || return 1
    if ! "$LMS" ps 2>/dev/null | grep -q "$model"; then "$LMS" load "$model" --context-length 16384 --yes || return 1; fi
    homeport assistant local "$LMSTUDIO" --model "$model" || return 1
    local_model="$model (LM Studio on this Mac)"
    return 0
  fi
  if [ "$mem_gb" -ge 44 ]; then model=qwen3.6:35b-a3b    # a mixture of experts: big, but only 3B at work per word
  elif [ "$mem_gb" -ge 22 ]; then model=qwen3.5:9b
  elif [ "$mem_gb" -ge 7 ]; then model=qwen3.5:4b
  else model=qwen3.5:2b; fi
  model="${HOMEPORT_LOCAL_MODEL:-$model}"
  command -v ollama >/dev/null || "$BREW" install ollama || return 1
  if ! curl -sf "$OLLAMA/api/version" >/dev/null && [ ! -d /Applications/Ollama.app ]; then
    # Its own agent, with room for Homeport's instructions and tools, and the model kept loaded a while after use.
    AGENT_ENV='<key>OLLAMA_CONTEXT_LENGTH</key><string>16384</string><key>OLLAMA_KEEP_ALIVE</key><string>30m</string>' \
      agent ollama "$(command -v ollama)" serve
  elif ! curl -sf "$OLLAMA/api/version" >/dev/null; then
    open -a Ollama
  fi
  for _ in $(seq 1 60); do curl -sf "$OLLAMA/api/version" >/dev/null && break; sleep 0.5; done
  curl -sf "$OLLAMA/api/version" >/dev/null || { echo "Ollama didn't start."; return 1; }
  echo "Downloading $model with Ollama:"
  ollama pull "$model" || return 1
  echo "Loading it…"
  curl -sS --fail-with-body "$OLLAMA/api/generate" -d "{\"model\":\"$model\",\"keep_alive\":\"30m\"}" >/dev/null || return 1
  homeport assistant local "$OLLAMA" --model "$model" || return 1
  local_model="$model (Ollama on this Mac)"
}
local_ai_failed() {
  printf '\033[33mThe local AI model isn'"'"'t set up (see above). Everything else works: run the installer again to retry, or connect one under Settings → Assistant chat.\033[0m\n'
}

if ! homeport status | grep -q "Not set up"; then
  if ! local_ai; then local_ai_failed; fi
  say "Homeport is updated"
  homeport status
  exit 0
fi

# MARK: First setup
say "Setting up Homeport"
tty_ok=false
if { exec 5</dev/tty; } 2>/dev/null; then tty_ok=true; exec 5<&-; fi
ask() {
  local var=$1 q=$2 def=${3:-} secret=${4:-} val=""
  if [ -n "${!var:-}" ]; then return 0; fi
  $tty_ok || return 0
  if [ -n "$secret" ]; then read -r -s -p "$q: " val </dev/tty; echo >/dev/tty
  else read -r -p "$q${def:+ [$def]}: " val </dev/tty; fi
  printf -v "$var" '%s' "${val:-$def}"
}
ips="$(for i in en0 en1 en2; do ipconfig getifaddr "$i" 2>/dev/null; done | tr '\n' ' ')"
first_ip="${ips%% *}"
shown_ip="${first_ip:-THIS-SERVERS-ADDRESS}"
# With no domain yet, <ip>.sslip.io works on this network with no setup: every device resolves it to this machine.
ask HOMEPORT_DOMAIN "Main domain for the portal and admin panel. Press Enter for ${first_ip:+$first_ip.sslip.io, which works on this network with no setup;} or type your own (its DNS must point here)" "${first_ip:+$first_ip.sslip.io}"
# Names Let's Encrypt can't certify (private addresses, test and local names) use Caddy's own certificates.
tls_default=letsencrypt
case "${HOMEPORT_DOMAIN:-}" in *.sslip.io|*.nip.io|*.test|*.local|*.lan|*.home.arpa|*.internal) tls_default=internal ;; esac
ask HOMEPORT_ADMIN "First admin's user name"
ask HOMEPORT_EMAIL "Email for Let's Encrypt notices (optional)"
ask HOMEPORT_TLS "Certificates: letsencrypt (reachable from the internet) or internal (just this network)" "$tls_default"
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
  --email "${HOMEPORT_EMAIL:-}" --tls "${HOMEPORT_TLS:-$tls_default}" >/dev/null
if ! local_ai; then local_ai_failed; fi
cat <<MSG

$(printf '\033[1m')Homeport is ready$(printf '\033[0m')

  Your apps (portal):  https://$HOMEPORT_DOMAIN/
  Admin panel:         https://$HOMEPORT_DOMAIN/admin
  …or by address:      https://$shown_ip/admin   (from this network; works before DNS is set up)
  User name:           $HOMEPORT_ADMIN
  Password:            the one you just chose (change it with: homeport user passwd $HOMEPORT_ADMIN)
  This server:         ${ips:-unknown address}
  Assistant's model:   ${local_model:-none yet (Settings → Assistant chat)}
MSG
if [ "${HOMEPORT_TLS:-$tls_default}" = internal ]; then
  cat <<MSG

  Certificates: Caddy's own test certificates, so your browser warns the first time; choose to continue.
MSG
  case "$HOMEPORT_DOMAIN" in
    *.sslip.io|*.nip.io) cat <<MSG
  $HOMEPORT_DOMAIN already leads to this machine from every device on this network: nothing to set up.
  Install apps on names under it, like cloud.$HOMEPORT_DOMAIN.
MSG
    ;;
    *) cat <<MSG
  Until DNS knows $HOMEPORT_DOMAIN, tell the computer you browse from where it is: add this line to its hosts file
  (Mac/Linux: sudo nano /etc/hosts; Windows: C:\Windows\System32\drivers\etc\hosts, as administrator):

      $shown_ip  $HOMEPORT_DOMAIN

  Add each app's address on the same line as you install them (e.g. cloud.$HOMEPORT_DOMAIN).
MSG
    ;;
  esac
else
  cat <<MSG

  Certificates: Let's Encrypt. $HOMEPORT_DOMAIN must point (DNS) at this server's public address, and ports 80 and
  443 must reach it; the certificate arrives within a minute of the first visit. Trouble: tail "$STATE/logs/caddy.log"
MSG
fi
cat <<MSG

  This Mac must stay awake and logged in to keep serving (System Settings > Energy, and automatic login).

  Next: install apps from the admin panel (Apps), or: homeport app catalog
  Connect an AI agent (opencode, Claude Code…): homeport mcp
MSG
