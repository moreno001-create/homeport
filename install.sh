#!/usr/bin/env bash
# Homeport installer for Ubuntu 26.04 LTS.
#
#   curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash
#
# Installs Docker, Caddy (from Caddy's own repository, for a current version) and Homeport, then sets up the main
# domain and the first admin. It asks at the terminal; for an unattended install, set these first:
#   HOMEPORT_DOMAIN=home.example.com HOMEPORT_ADMIN=alice HOMEPORT_PASSWORD='…' HOMEPORT_EMAIL=you@example.com
#   HOMEPORT_TLS=internal   (Caddy's own certificates, for a test machine with no public address)
#   (With no domain of your own, HOMEPORT_DOMAIN=homeport.local works on the home network, announced with Avahi; or a
#   free name: HOMEPORT_DDNS=duckdns|desec HOMEPORT_DDNS_NAME=myhome HOMEPORT_DDNS_TOKEN='…')
#   HOMEPORT_VERSION=v0.3.0 (a release tag; default: the latest)
# It also installs Ollama with a model that fits this server's memory, for the assistant chat:
#   HOMEPORT_LOCAL_AI=no (skip it), HOMEPORT_LOCAL_MODEL=qwen3.5:9b (another Ollama model),
#   HOMEPORT_LOCAL_URL=http://192.168.1.20:1234 (use a model server on another computer instead)
# File sharing on the home network (Samba, for the File sharing page): HOMEPORT_FILE_SHARING=yes (asked at the terminal;
#   unattended installs leave it out unless set).
# Running it again updates Homeport and leaves the setup alone.
set -Eeuo pipefail
shopt -s extglob lastpipe

REPO="moreno001-create/homeport"
CADDY_REPO="${HOMEPORT_CADDY_REPO:-https://dl.cloudsmith.io/public/caddy/stable}"
LOG=/var/log/homeport-install.log

[ "$(id -u)" -eq 0 ] || { echo "Run this as root: curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash" >&2; exit 1; }

# MARK: Output
# On a terminal: a progress bar, the command running and its last lines of output. Everything goes to the log in full.
# (No tee or other helper process: on Ubuntu 26.04, sudo-rs can leave a helper paused after the password questions,
# and an installer waiting for it never finishes.) Without a terminal, every line is printed as it comes.
exec 3>&1 4>&2
exec >>"$LOG" 2>&1
echo "Homeport installer, $(date)"

live=false
if [ -t 3 ] && [ "${TERM:-dumb}" != dumb ]; then live=true; fi
TAIL=4          # lines of the running command's output under the bar
step="starting" # the step being done, its share of the bar and when it began
from=0 to=0 pct=0 began=$SECONDS step_ok=true
cmd_shown=""    # the command running
lines=()        # its last lines of output
seen=0          # lines of output so far in this step (to move the bar along)
drawn=0         # lines of the block on screen now
last_draw=0

# The block is drawn 80 columns wide. (Not measured with stty: on Ubuntu 26.04, under "curl … | sudo bash", sudo-rs leaves
# stty paused for good, and the installer would hang before printing anything.)
COLS=80
if $live; then printf '\e[?25l' >&3; fi

# clip VAR TEXT WIDTH: TEXT cut to fit WIDTH columns (long lines would wrap and break the redrawing).
clip() { local _c="$2"; if [ "${#_c}" -gt "$3" ]; then _c="${_c:0:$3-3}..."; fi; printf -v "$1" '%s' "$_c"; }

# draw [force]: (re)draws the bar and the lines under it, at most ~12 times a second unless forced.
draw() {
  if ! $live; then return 0; fi
  local now=${EPOCHREALTIME/./}
  if [ "${1:-}" != force ] && [ $((now - last_draw)) -lt 80000 ]; then return 0; fi
  last_draw=$now
  local w=$((COLS - 1)) barw=$((COLS - 30)) i fill bar="" out="" s
  if [ "$barw" -gt 40 ]; then barw=40; fi
  if [ "$barw" -lt 10 ]; then barw=10; fi
  fill=$((pct * barw / 100))
  for ((i = 0; i < barw; i++)); do if [ "$i" -lt "$fill" ]; then bar+="█"; else bar+="░"; fi; done
  if [ "$drawn" -gt 0 ]; then out+=$'\e['"$drawn"'A'; fi
  clip s "$step" $((w - barw - 7))
  out+=$'\r\e[J\e[1m'"$bar"$(printf ' %3d%%' "$pct")$'\e[0m  '"$s"$'\n'
  if [ -n "$cmd_shown" ]; then clip s "  \$ $cmd_shown" "$w"; else s=""; fi
  out+=$'\e[36m'"$s"$'\e[0m\n'
  for ((i = 0; i < TAIL; i++)); do
    clip s "    ${lines[i]:-}" "$w"
    out+=$'\e[2m'"$s"$'\e[0m\n'
  done
  printf '%s' "$out" >&3
  drawn=$((TAIL + 2))
}

# hide: takes the block off the screen, to print above it or ask a question.
hide() {
  if $live && [ "$drawn" -gt 0 ]; then printf '\e[%dA\r\e[J' "$drawn" >&3; fi
  drawn=0
}

# note TEXT: a line worth seeing, printed above the bar (and into the log).
note() {
  echo "$*"
  if $live; then hide; printf '  %s\n' "$*" >&3; draw force; else printf '%s\n' "$*" >&3; fi
}

# say FROM TO "what": a new step, filling the bar from FROM% towards TO% as its commands print.
say() {
  if $live && [ "$step" != starting ]; then
    hide
    if $step_ok; then printf '  \e[32mdone\e[0m  ' >&3; else printf '  \e[31mfailed\e[0m  ' >&3; fi
    printf '%s \e[2m(%ss)\e[0m\n' "$step" $((SECONDS - began)) >&3
  fi
  from=$1 to=$2 pct=$1 step=$3 began=$SECONDS cmd_shown="" lines=() seen=0 step_ok=true
  printf '\n==> %s\n' "$step"
  if $live; then draw force; else printf '\n\033[1m==> %s\033[0m (%d%%)\n' "$step" "$pct" >&3; fi
}

# run CMD…: runs a command with its output into the log and under the bar. Its input is /dev/null (under curl | bash,
# the script itself is on stdin), or the file RUN_IN names.
run() {
  cmd_shown="$*" lines=()
  echo "\$ $*"
  draw force
  local line rc=0 meter=false
  "$@" <"${RUN_IN:-/dev/null}" 2>&1 | tr '\r' '\n' | while IFS= read -r line || [ -n "$line" ]; do
    line="${line//$'\e'\[*([0-9;?])[a-zA-Z]/}"    # colours
    if [[ $line =~ ^[#[:space:]]*([0-9]{1,3})(\.[0-9]+)?%[[:space:]]*$ ]] && [ $((10#${BASH_REMATCH[1]})) -le 100 ]; then
      # A progress meter (curl's "#### 45.2%"): the bar follows it, on one line under it, and it isn't logged.
      pct=$((from + (to - from) * 10#${BASH_REMATCH[1]} / 100))
      if $meter; then lines[${#lines[@]}-1]="$line"; else lines+=("$line"); meter=true; fi
      if [ "${#lines[@]}" -gt "$TAIL" ]; then lines=("${lines[@]:1}"); fi
      draw
      continue
    fi
    meter=false
    printf '%s\n' "$line"
    if ! $live; then printf '%s\n' "$line" >&3; fi
    if [ -n "${line//[[:space:]]/}" ]; then lines+=("$line"); fi
    if [ "${#lines[@]}" -gt "$TAIL" ]; then lines=("${lines[@]:1}"); fi
    seen=$((seen + 1))
    pct=$((from + (to - from) * seen / (seen + 60)))
    draw
  done || rc=$?
  return "$rc"
}

# failure: the step it stopped in and the last lines of the log, on the terminal.
reported=false
failed() {
  if $reported; then return; fi
  reported=true
  hide
  { printf '\n\033[31mThe installer stopped during: %s\033[0m\n' "$step"
    printf 'Last lines of the log:\n'; tail -n 15 "$LOG" | sed 's/^/  /'
    printf 'The whole log is in %s\n' "$LOG"; } >&4
}
die() { printf 'Error: %s\n' "$*"; hide; printf '\033[31mError:\033[0m %s\n' "$*" >&4; failed; exit 1; }
trap failed ERR
tmp="$(mktemp -d)"
# (Also for exits the ERR trap doesn't see, like an unset variable.)
cleanup() { local rc=$?; rm -rf "$tmp"; if [ "$rc" -ne 0 ]; then failed; fi; if $live; then printf '\e[?25h' >&3; fi; }
trap cleanup EXIT

# show: prints what's on stdin (a heredoc) on the terminal and into the log.
show() { local m; m="$(cat)"; printf '%s\n' "$m"; printf '%s\n' "$m" >&3; }

if $live; then printf '\n\e[1mHomeport installer\e[0m \e[2m(the whole log: %s)\e[0m\n\n' "$LOG" >&3; fi

# fetch URL FILE WHAT: download, retrying a few times, and say plainly what failed.
fetch() {
  local code
  cmd_shown="curl $1"; echo "\$ curl $1"; draw force
  code="$(curl -sSL --retry 3 --retry-delay 2 -w '%{http_code}' -o "$2" "$1" 2>/dev/null)" || code="${code:-no answer}"
  case "$code" in
    2??) return 0 ;;
    401|403) note "Couldn't download $3: $1 refused it (HTTP $code). A proxy or filter on this network, or the server limiting this address, can cause that." ;;
    *) note "Couldn't download $3 from $1 (HTTP $code)." ;;
  esac
  return 1
}

# apt-get update; if Caddy's repository (from an earlier run) refuses, drop it and use Ubuntu's Caddy.
apt_update() {
  run apt-get update -q && return 0
  if [ -f /etc/apt/sources.list.d/caddy-stable.list ]; then
    note "Caddy's package repository refused the update; removing it (Ubuntu's own Caddy will be used)."
    rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    run apt-get update -q && return 0
  fi
  return 1
}
. /etc/os-release
[ "${ID:-}" = ubuntu ] || die "Homeport is for Ubuntu (this is ${PRETTY_NAME:-unknown})"
case "${VERSION_ID:-}" in
  26.04) ;;
  *) note "Warning: Homeport is made for Ubuntu 26.04 LTS; this is ${PRETTY_NAME}. Continuing anyway." ;;
esac
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported processor $(uname -m) (Homeport runs on x86-64 and ARM64)" ;;
esac

say 0 30 "Installing Docker and the packages Homeport needs"
export DEBIAN_FRONTEND=noninteractive
apt_update || die "apt-get update failed (see the lines above); check this server's internet connection and apt sources"
# Avahi announces homeport.local (and apps' .local names) on the home network (avahi-utils: avahi-publish); libnss-mdns
# lets this server find them too.
# restic makes the backups (Backups page); zstd unpacks Ollama (the local AI); rsync moves things between drives (Storage page).
pkgs="ca-certificates curl gnupg avahi-daemon avahi-utils libnss-mdns restic zstd rsync poppler-utils"
# Docker: Ubuntu's (docker.io), unless Docker's own (docker-ce, from download.docker.com) is installed; and Compose to
# match, unless a Compose plugin is already installed (Docker's docker-compose-plugin and Ubuntu's docker-compose-v2
# both install the same file, so apt refuses to have both).
installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "ok installed"; }
if installed docker-ce; then
  note "Docker's own Docker (docker-ce) is installed: using it"
else
  pkgs="$pkgs docker.io"
fi
if installed docker-compose-plugin; then
  note "Docker's Compose plugin is installed: using it"
elif ! installed docker-compose-v2; then
  pkgs="$pkgs docker-compose-v2"
fi
# Packages an earlier, interrupted install left half set up are finished first (apt refuses to go on otherwise).
if dpkg --audit 2>/dev/null | grep -q .; then
  note "Finishing packages an earlier install left half set up"
  run dpkg --configure -a || true
fi
# shellcheck disable=SC2086
run apt-get install -y -q $pkgs

say 30 38 "Installing Caddy"
# Caddy's own repository has current versions (Ubuntu's is older); if it can't be used, Ubuntu's Caddy works too.
caddy_repo() {
  fetch "$CADDY_REPO/gpg.key" "$tmp/caddy.key" "Caddy's signing key" || return 1
  run gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg "$tmp/caddy.key" || return 1
  fetch "$CADDY_REPO/debian.deb.txt" "$tmp/caddy.list" "Caddy's package list" || return 1
  cp "$tmp/caddy.list" /etc/apt/sources.list.d/caddy-stable.list
  run apt-get update -q || return 1
}
if ! command -v caddy >/dev/null || ! caddy version | grep -q -E '^v2\.(8|9|[1-9][0-9])'; then
  if ! caddy_repo; then
    note "Using Ubuntu's own (older) Caddy instead of Caddy's repository."
    rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    apt_update || die "apt-get update failed"
  fi
  run apt-get install -y -q caddy
fi
note "Caddy $(caddy version | cut -d' ' -f1)"

say 38 44 "Installing Homeport"
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
note "$(homeport version)"

say 44 50 "Starting Homeport"
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
# Stopping (an update, say) lets an app install or update under way finish first: Homeport waits for it, and its
# docker compose keeps running until then.
KillMode=mixed
TimeoutStopSec=35min
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
run systemctl daemon-reload
run systemctl enable --now docker caddy
run systemctl enable homeport
if busy="$(homeport status 2>/dev/null | sed -n 's/^Working on: //p')" && [ -n "$busy" ]; then
  note "Homeport is busy ($busy): it restarts as soon as that's finished"
fi
run systemctl restart homeport

if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
  note "Opening ports 80 and 443 in the firewall"
  run ufw allow 80/tcp && run ufw allow 443/tcp
fi

cmd_shown="waiting for the Homeport service to start"; draw force
for _ in $(seq 1 60); do homeport status >/dev/null 2>&1 && break; sleep 0.5; done
hp_status="$(homeport status 2>&1)" || die "the Homeport service didn't start: $hp_status (see: journalctl -u homeport)"

# MARK: Local AI
# A local model for the assistant chat: Ollama on this server with a model that fits its memory, downloaded, loaded and
# connected. HOMEPORT_LOCAL_AI=no skips it, HOMEPORT_LOCAL_MODEL picks the model (an Ollama name), and
# HOMEPORT_LOCAL_URL connects a model server on another computer (Ollama, LM Studio…) instead of installing one here.
OLLAMA=http://127.0.0.1:11434
local_model="" # what the assistant ended up with, for the summary

# pick_model: the best Ollama model for this machine, its download in tenths of a GB, and why, as "model|size|why".
# With a graphics card Ollama can use (NVIDIA, or AMD with its own memory), the card's memory decides. On the processor
# alone, speed matters most (every answer reads the whole conversation again), so the models are smaller; with plenty of
# memory, a mixture of experts (big, but only 3B at work per word) is both quick and capable.
pick_model() {
  local ram vram=0 gpu="" v f b
  ram=$(( $(awk '/^MemTotal:/{print $2}' /proc/meminfo) / 1048576 ))
  if command -v nvidia-smi >/dev/null && v="$(nvidia-smi --query-gpu=memory.total,name --format=csv,noheader,nounits 2>/dev/null | head -1)" && [ -n "$v" ]; then
    vram=$(( ${v%%,*} / 1024 )); gpu="${v#*, }"
  fi
  for f in /sys/class/drm/card*/device/mem_info_vram_total; do
    [ -r "$f" ] || continue
    b=$(( $(cat "$f") / 1073741824 ))
    if [ "$b" -gt "$vram" ]; then vram=$b; gpu="AMD graphics"; fi
  done
  if [ "$vram" -ge 4 ]; then
    local why="$gpu with $vram GB of graphics memory"
    if [ "$vram" -ge 28 ]; then echo "qwen3.6:35b-a3b|226|$why"
    elif [ "$vram" -ge 20 ]; then echo "qwen3.5:27b|175|$why"
    elif [ "$vram" -ge 10 ]; then echo "qwen3.5:9b|66|$why"
    elif [ "$vram" -ge 6 ]; then echo "qwen3.5:4b|33|$why"
    else echo "qwen3.5:2b|27|$why"; fi
    return
  fi
  local why="no graphics card Ollama can use; $ram GB of memory and $(nproc) processor cores, so a model that answers quickly on the processor"
  if [ "$ram" -ge 32 ]; then echo "qwen3.6:35b-a3b|226|$why"
  elif [ "$ram" -ge 14 ]; then echo "qwen3.5:4b|33|$why"
  else echo "qwen3.5:2b|27|$why"; fi
}
gb() { printf -v "$1" '%d.%d GB' $(($2 / 1000000000)) $(($2 % 1000000000 / 100000000)); }

# pull_model NAME: downloads a model through Ollama's API, the bar following the download.
pull_model() {
  local line status="" last="" err="" total got big=0 rc=0 a b
  cmd_shown="ollama pull $1" lines=(); echo "\$ ollama pull $1"; draw force
  curl -sSN "$OLLAMA/api/pull" -d "{\"model\":\"$1\"}" 2>&1 | while IFS= read -r line || [ -n "$line" ]; do
    if [ "${line:0:1}" != "{" ]; then echo "$line"; continue; fi
    if [[ $line =~ \"error\":\"([^\"]*)\" ]]; then err=${BASH_REMATCH[1]}; fi
    if [[ $line =~ \"status\":\"([^\"]*)\" ]]; then status=${BASH_REMATCH[1]}; fi
    total=0 got=0
    if [[ $line =~ \"total\":([0-9]+) ]]; then total=${BASH_REMATCH[1]}; fi
    if [[ $line =~ \"completed\":([0-9]+) ]]; then got=${BASH_REMATCH[1]}; fi
    if [ "$status" != "$last" ]; then echo "$status"; last=$status; fi
    if [ "$total" -gt 0 ] && [ "$total" -ge "$big" ]; then
      big=$total; pct=$((from + (to - from) * got / total))
      gb a "$got"; gb b "$total"; lines=("$a of $b")
    elif [ -n "$status" ] && [ "$total" -eq 0 ]; then
      lines=("$status")
    fi
    draw
  done || rc=$?
  if [ -n "$err" ]; then note "Ollama couldn't download $1: $err"; return 1; fi
  [ "$rc" -eq 0 ] && [ "$status" = success ]
}

# local_ai FROM MID: sets up the local model, the bar going FROM → MID while Ollama installs, then on to 100.
# Returns non-zero if it couldn't; Homeport works without it.
local_ai() {
  if [ "${HOMEPORT_LOCAL_AI:-yes}" = no ]; then echo "Local AI skipped (HOMEPORT_LOCAL_AI=no)."; return 0; fi
  local now model size why free need
  # (Homeport before 0.7.1 can't connect a local model.)
  if ! now="$(homeport assistant local 2>/dev/null)"; then note "This Homeport version can't connect a local model yet."; return 1; fi
  if [[ $now == "Local model: "* ]]; then local_model="${now#Local model: }"; note "The assistant already uses $local_model"; return 0; fi
  if [ -n "${HOMEPORT_LOCAL_URL:-}" ]; then
    say "$1" 100 "Connecting the assistant to the model server at $HOMEPORT_LOCAL_URL"
    run homeport assistant local "$HOMEPORT_LOCAL_URL" ${HOMEPORT_LOCAL_MODEL:+--model "$HOMEPORT_LOCAL_MODEL"} || return 1
    local_model="$(homeport assistant local)"; local_model="${local_model#Local model: }"
    return 0
  fi
  IFS='|' read -r model size why <<<"$(pick_model)"
  if [ -n "${HOMEPORT_LOCAL_MODEL:-}" ]; then model=$HOMEPORT_LOCAL_MODEL size=0 why="HOMEPORT_LOCAL_MODEL"; fi
  note "Local AI model: $model ($why)"
  free=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
  need=$((size / 10 + 3))
  if [ "$need" -gt "$free" ]; then note "Not enough disk space for the local AI model $model (it needs about $need GB; $free GB free)."; return 1; fi

  say "$1" "$2" "Installing Ollama (runs AI models on this server)"
  if ! command -v ollama >/dev/null; then
    fetch https://ollama.com/install.sh "$tmp/ollama-install.sh" "Ollama's installer" || return 1
    run sh "$tmp/ollama-install.sh" || return 1
  fi
  # Room for Homeport's instructions and tools (Ollama's default context is smaller), and the model stays loaded a
  # while after each use.
  mkdir -p /etc/systemd/system/ollama.service.d
  printf '[Service]\nEnvironment=OLLAMA_CONTEXT_LENGTH=16384\nEnvironment=OLLAMA_KEEP_ALIVE=30m\n' \
    > /etc/systemd/system/ollama.service.d/homeport.conf
  run systemctl daemon-reload || return 1
  run systemctl enable ollama || return 1
  run systemctl restart ollama || return 1
  cmd_shown="waiting for Ollama to start"; draw force
  for _ in $(seq 1 60); do curl -sf "$OLLAMA/api/version" >/dev/null && break; sleep 0.5; done
  curl -sf "$OLLAMA/api/version" >/dev/null || { note "Ollama didn't start (see: journalctl -u ollama)"; return 1; }

  if [ "$size" -gt 0 ]; then
    say "$2" 97 "Downloading the local AI model $model ($((size / 10)).$((size % 10)) GB)"
  else
    say "$2" 97 "Downloading the local AI model $model"
  fi
  pull_model "$model" || return 1
  say 97 100 "Loading $model and connecting the assistant to it"
  run curl -sS --fail-with-body "$OLLAMA/api/generate" -d "{\"model\":\"$model\",\"keep_alive\":\"30m\"}" || return 1
  run homeport assistant local "$OLLAMA" --model "$model" || return 1
  local_model="$model (Ollama on this server)"
}
local_ai_failed() {
  step_ok=false
  note "The local AI model isn't set up (the log says why). Everything else works: run the installer again to retry, or connect one under Settings → Assistant chat."
}

if [[ $hp_status != *"Not set up"* ]]; then
  if ! local_ai 50 70; then local_ai_failed; fi
  say 100 100 "Homeport is updated"
  hide
  homeport status | show
  if [ -n "$local_model" ]; then echo "Local AI: $local_model" | show; fi
  exit 0
fi

# MARK: First setup (like YunoHost's post-install)
say 50 54 "Setting up this server"
# A terminal to ask at? (/dev/tty can exist without being usable, e.g. over a non-interactive ssh or in CI.)
tty_ok=false
if { exec 5</dev/tty; } 2>/dev/null; then tty_ok=true; exec 5<&-; fi
# The questions replace the bar until they're answered.
if $tty_ok; then hide; if $live; then printf '\e[?25h' >&3; fi; fi
ask() { # ask VAR "question" [default] [secret]
  local var=$1 q=$2 def=${3:-} secret=${4:-} val=""
  if [ -n "${!var:-}" ]; then return 0; fi
  $tty_ok || return 0
  # Questions go straight to the terminal (not through the log), so they always show.
  if [ -n "$secret" ]; then
    printf '%s: ' "$q" >/dev/tty; read -r -s val </dev/tty; echo >/dev/tty
  else
    printf '%s%s: ' "$q" "${def:+ [$def]}" >/dev/tty; read -r val </dev/tty
    printf '%s: %s\n' "$q" "${val:-$def}"   # into the log only (never passwords)
  fi
  printf -v "$var" '%s' "${val:-$def}"
}
ips="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -v -E '^(172\.(1[6-9]|2[0-9]|3[01])|127)\.' | tr '\n' ' ')" || ips=""
first_ip="${ips%% *}"
shown_ip="${first_ip:-THIS-SERVERS-ADDRESS}"
# How people reach this server. homeport.local works on this network with no setup (Homeport announces it, and apps'
# names under it, with Avahi). A free name from DuckDNS or deSEC works from anywhere, with Let's Encrypt certificates
# for it and every app under it; Homeport keeps it pointed at this home's public address. Or a domain of your own.
# A home name (yourname.dedyn.io, free from deSEC) points at this server's home address: every device at home finds it
# with ordinary DNS, and Homeport gets a real certificate for it and every name under it through deSEC's DNS (no ports
# opened, nothing reachable from outside), so nothing warns and signing in works everywhere.
# Unattended: HOMEPORT_DOMAIN, or HOMEPORT_HOME_NAME with HOMEPORT_HOME_TOKEN (and HOMEPORT_HOME_PROVIDER=cloudflare|desec),
# or HOMEPORT_DDNS=duckdns|desec with
# HOMEPORT_DDNS_NAME and HOMEPORT_DDNS_TOKEN.
ddns="" # the free name's provider, once registered
home_name="" # the home name, once set up
# clean_token keeps only what a token is made of (letters, digits, - _ .): a paste can bring hidden characters along
# (the terminal's paste markers, a line break), and Cloudflare refuses those as "invalid request headers".
clean_token() {
  local t=$1
  t="${t//$'\e[200~'/}"; t="${t//$'\e[201~'/}"
  printf '%s' "$t" | LC_ALL=C tr -cd 'A-Za-z0-9._-'
}
home_setup() { # home_setup PROVIDER NAME TOKEN: registers the home name and gets its certificate; on success it's the main domain
  if printf '%s\n' "$(clean_token "$3")" | homeport domain home "$2" --provider "$1" --keep-main; then
    HOMEPORT_DOMAIN="$(printf '%s' "${2%.}" | tr '[:upper:]' '[:lower:]')" HOMEPORT_TLS=letsencrypt
    case "$HOMEPORT_DOMAIN" in *.*) ;; *) HOMEPORT_DOMAIN="$HOMEPORT_DOMAIN.dedyn.io" ;; esac
    home_name=$HOMEPORT_DOMAIN
    return 0
  fi
  return 1
}
guide_home_name() { # asks for a home name, with how to get the token, until it works or they go back
  local name token provider pick
  printf '\n  Where is the name?\n' >/dev/tty
  printf '    1) Cloudflare: a domain of yours there (home.example.com)\n' >/dev/tty
  printf '    2) deSEC: a free name (yourname.dedyn.io), or a domain of yours there\n' >/dev/tty
  pick=""; printf '  Choose 1-2 [1]: ' >/dev/tty; read -r pick </dev/tty
  if [ "${pick:-1}" = 2 ]; then
    provider=desec
    printf '\n  1. Open https://desec.io, choose "Sign up" (email and password) and confirm your email.\n' >/dev/tty
    printf '  2. Open "Token Management", add a token and copy it.\n' >/dev/tty
    printf '  A domain of your own there needs its nameservers set to ns1.desec.io and ns2.desec.org.\n' >/dev/tty
  else
    provider=cloudflare
    printf '\n  1. In Cloudflare (dash.cloudflare.com) your domain must be added and active.\n' >/dev/tty
    printf '  2. My Profile, API Tokens, Create Token: use the "Edit zone DNS" template, choose your domain under\n' >/dev/tty
    printf '     Zone Resources, create it and copy the token.\n' >/dev/tty
    printf '  Homeport only changes the records for the name you choose (and names under it).\n' >/dev/tty
  fi
  printf '  It points the name at this server (%s) and gets real certificates for it.\n\n' "${first_ip:-its home address}" >/dev/tty
  while :; do
    name=""; token=""
    if [ "$provider" = desec ]; then
      printf 'Name (yourname for yourname.dedyn.io, or home.yourdomain.com), or Enter to go back: ' >/dev/tty
    else
      printf 'Name (home.yourdomain.com, or yourdomain.com), or Enter to go back: ' >/dev/tty
    fi
    read -r name </dev/tty
    [ -n "$name" ] || return 1
    printf 'Token: ' >/dev/tty; read -r -s token </dev/tty; echo >/dev/tty
    if home_setup "$provider" "$name" "$token" >/dev/tty 2>&1; then return 0; fi
    printf 'That didn'"'"'t work (see above). Try again.\n' >/dev/tty
  done
}
# local_name: homeport.local, unless another device on this network already has it (another Homeport server): then this
# machine's own name (memorydelta.local).
local_name() {
  local mine theirs
  theirs="$(timeout 4 avahi-resolve -4 -n homeport.local 2>/dev/null | awk '{print $2}')"
  if [ -n "$theirs" ] && ! printf ' %s ' "$ips" | grep -q " $theirs "; then
    mine="$(hostname -s | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-\n' '-')"
    note "homeport.local is already taken on this network (by $theirs): using ${mine:-homeport-2}.local" >/dev/null
    echo "${mine:-homeport-2}.local"
    return
  fi
  echo homeport.local
}
free_name() { # free_name duckdns|desec NAME TOKEN: registers the name with Homeport; on success it's the main domain
  local out
  if out="$(printf '%s\n' "$(clean_token "$3")" | homeport domain free "$1" "$2" 2>&1)"; then
    HOMEPORT_DOMAIN="$(echo "$out" | awk '{print $1; exit}')" ddns=$1 HOMEPORT_TLS=letsencrypt
    echo "$out"
    return 0
  fi
  echo "$out" | sed 's/^Error: //'
  return 1
}
guide_free_name() { # asks for a free name, with how to get one, until it works or they go back
  local p=$1 suffix name token
  suffix=$([ "$p" = duckdns ] && echo .duckdns.org || echo .dedyn.io)
  if [ "$p" = duckdns ]; then
    printf '\n  1. Open https://www.duckdns.org and sign in (with GitHub, Google or another account).\n  2. Under "domains", add the name you want (letters, digits and dashes).\n  3. Copy your token: the long code at the top of the page.\n\n' >/dev/tty
  else
    printf '\n  1. Open https://desec.io, choose "Sign up" (email and password) and confirm your email.\n  2. Open "Token Management", add a token and copy it.\n  Homeport makes the name in your deSEC account for you, if no one has it yet.\n\n' >/dev/tty
  fi
  while :; do
    name=""; token=""
    printf 'Name (yourname, for yourname%s), or Enter to go back: ' "$suffix" >/dev/tty; read -r name </dev/tty
    [ -n "$name" ] || return 1
    printf 'Token: ' >/dev/tty; read -r -s token </dev/tty; echo >/dev/tty
    if free_name "$p" "$name" "$token" >/dev/tty 2>&1; then return 0; fi
    printf 'That didn'"'"'t work (see above). Try again.\n' >/dev/tty
  done
}
if [ -n "${HOMEPORT_HOME_NAME:-}" ] && [ -z "${HOMEPORT_DOMAIN:-}" ]; then
  home_setup "${HOMEPORT_HOME_PROVIDER:-cloudflare}" "$HOMEPORT_HOME_NAME" "${HOMEPORT_HOME_TOKEN:-}" || die "couldn't set up the home name (see above)"
fi
if [ -n "${HOMEPORT_DDNS:-}" ] && [ -z "${HOMEPORT_DOMAIN:-}" ]; then
  free_name "$HOMEPORT_DDNS" "${HOMEPORT_DDNS_NAME:-}" "${HOMEPORT_DDNS_TOKEN:-}" || die "couldn't set up the free name (see above)"
fi
while [ -z "${HOMEPORT_DOMAIN:-}" ] && $tty_ok; do
  printf '\nHow should people reach this server?\n' >/dev/tty
  printf '  1) A home name (recommended): a name in your domain on Cloudflare, or a free one from deSEC. Every device\n' >/dev/tty
  printf '     at home finds it, with real certificates for it and every app: no warnings. Nothing is reachable from\n' >/dev/tty
  printf '     outside until you choose so, app by app.\n' >/dev/tty
  printf '  2) homeport.local: nothing to set up, but browsers warn about every certificate, and some devices\n' >/dev/tty
  printf '     (Android, other subnets) can'"'"'t find it\n' >/dev/tty
  printf '  3) A free name from DuckDNS (yourname.duckdns.org) for the internet: needs ports 80 and 443 on the router\n' >/dev/tty
  printf '  4) A free name from deSEC (yourname.dedyn.io) for the internet: the same, from a non-profit\n' >/dev/tty
  printf '  5) A domain of your own\n' >/dev/tty
  pick=""; printf 'Choose 1-5 [1]: ' >/dev/tty; read -r pick </dev/tty
  case "${pick:-1}" in
    1) guide_home_name || true ;;
    2) HOMEPORT_DOMAIN="$(local_name)" ;;
    3) guide_free_name duckdns || true ;;
    4) guide_free_name desec || true ;;
    5) ask HOMEPORT_DOMAIN "Your domain (its DNS must point at this server's public address)" ;;
  esac
done
echo "Main domain: ${HOMEPORT_DOMAIN:-}${ddns:+ (free name from $ddns)}${home_name:+ (home name)}"
# Names Let's Encrypt can't certify (private addresses, test and local names) use Caddy's own certificates.
tls_default=letsencrypt
case "${HOMEPORT_DOMAIN:-}" in *.sslip.io|*.nip.io|*.test|*.local|*.lan|*.home.arpa|*.internal) tls_default=internal ;; esac
ask HOMEPORT_ADMIN "First admin's user name"
if [ "$tls_default" = letsencrypt ]; then
  ask HOMEPORT_EMAIL "Email for Let's Encrypt notices about the certificates (optional)"
fi
if [ -z "$ddns" ] && [ -z "$home_name" ] && [ "$tls_default" = letsencrypt ]; then
  ask HOMEPORT_TLS "Certificates: letsencrypt (reachable from the internet) or internal (test machine)" "$tls_default"
fi
while [ -z "${HOMEPORT_PASSWORD:-}" ] && $tty_ok; do
  ask HOMEPORT_PASSWORD "Password for ${HOMEPORT_ADMIN:-the admin} (at least 10 characters)" "" secret
  p2=""; printf 'Again: ' >/dev/tty; read -r -s p2 </dev/tty; echo >/dev/tty
  if [ "$HOMEPORT_PASSWORD" != "$p2" ]; then echo "They didn't match; try again." >/dev/tty; HOMEPORT_PASSWORD=""; fi
done
if ! installed samba; then
  ask HOMEPORT_FILE_SHARING "Share folders on the home network too (file sharing for Macs, Windows and phones)? yes or no" "yes"
fi
if $tty_ok && $live; then printf '\e[?25l\n' >&3; draw force; fi

if [ -z "${HOMEPORT_DOMAIN:-}" ] || [ -z "${HOMEPORT_ADMIN:-}" ] || [ -z "${HOMEPORT_PASSWORD:-}" ]; then
  hide
  show <<MSG

Homeport is installed. Finish setting it up with:

  sudo homeport init --domain home.example.com --admin yourname --email you@example.com

(add --tls internal on a test machine without a public address)
MSG
  exit 0
fi

# The password goes to homeport init on its input, from a file only root can read (never on a command line).
( umask 077; printf '%s\n' "$HOMEPORT_PASSWORD" > "$tmp/password" )
RUN_IN="$tmp/password" run homeport init --domain "$HOMEPORT_DOMAIN" --admin "$HOMEPORT_ADMIN" \
  --email "${HOMEPORT_EMAIL:-}" --tls "${HOMEPORT_TLS:-$tls_default}"
rm -f "$tmp/password"
sharing=""
if installed samba; then
  sharing="ready (Samba was already installed)"
elif [[ ${HOMEPORT_FILE_SHARING:-no} =~ ^[Yy] ]]; then
  say 54 56 "Installing file sharing (Samba)"
  if DEBIAN_FRONTEND=noninteractive run apt-get install -y -q samba; then
    sharing="ready"
  else
    step_ok=false
    note "File sharing didn't install (the log says why). Everything else works: install it later on the File sharing page."
  fi
fi
if ! local_ai 56 70; then local_ai_failed; fi
say 100 100 "Done"
hide
show <<MSG

$(printf '\033[1m')Homeport is ready$(printf '\033[0m')

  Your apps (portal):  https://$HOMEPORT_DOMAIN/
  Admin panel:         https://$HOMEPORT_DOMAIN/admin
  …or by address:      https://$shown_ip/admin   (from this network; works before DNS is set up)
  User name:           $HOMEPORT_ADMIN
  Password:            the one you just chose (change it with: sudo homeport user passwd $HOMEPORT_ADMIN)
  This server:         ${ips:-unknown address}
  Assistant's model:   ${local_model:-none yet (Settings → Assistant chat)}
  File sharing:        ${sharing:-not installed (the File sharing page installs it)}${sharing:+: share folders on the File sharing page}
MSG
if [ "${HOMEPORT_TLS:-$tls_default}" = internal ]; then
  show <<MSG

  Certificates: Caddy's own test certificates, so your browser warns the first time; choose to continue.
MSG
  case "$HOMEPORT_DOMAIN" in
    *.local) show <<MSG
  $HOMEPORT_DOMAIN is announced on this network: Macs, iPhones, iPads, Linux and Windows find it with no setup.
  Install apps on names under it, like cloud.$HOMEPORT_DOMAIN (Windows may not find those; Android browsers often find
  no .local names at all: there, use https://$shown_ip/admin).
MSG
    ;;
    *.sslip.io|*.nip.io) show <<MSG
  $HOMEPORT_DOMAIN already leads to this machine from every device on this network: nothing to set up.
  Install apps on names under it, like cloud.$HOMEPORT_DOMAIN.
MSG
    ;;
    *) show <<MSG
  Until DNS knows $HOMEPORT_DOMAIN, tell the computer you browse from where it is: add this line to its hosts file
  (Mac/Linux: sudo nano /etc/hosts; Windows: C:\Windows\System32\drivers\etc\hosts, as administrator):

      $shown_ip  $HOMEPORT_DOMAIN

  Add each app's address on the same line as you install them (e.g. cloud.$HOMEPORT_DOMAIN).
MSG
    ;;
  esac
elif [ -n "$home_name" ]; then
  show <<MSG

  $HOMEPORT_DOMAIN is your home name: it points at this server ($shown_ip), so every device at home (phones and
  other subnets too) finds it, with a real certificate for it and every app under it, like photos.$HOMEPORT_DOMAIN.
  Nothing is reachable from the internet. To put an app on the internet later: Internet access, in the admin panel.
  If a device can't find it, its router may block DNS answers with home addresses: Overview → Check now says so.
MSG
elif [ -n "$ddns" ]; then
  show <<MSG

  $HOMEPORT_DOMAIN is your free name; Homeport keeps it pointed at your home's public address.
  On your router, forward ports 80 and 443 to $shown_ip (often under "Port forwarding" or "NAT"). Then
  Let's Encrypt gives each name its certificate within a minute of its first visit, and it works from anywhere.
  Apps get names under it, like cloud.$HOMEPORT_DOMAIN, each with its own certificate.
  Some routers don't let devices at home reach the home's own public address: there, use https://$shown_ip/admin.
MSG
else
  show <<MSG

  Certificates: Let's Encrypt. $HOMEPORT_DOMAIN must point (DNS) at this server's public address, and ports 80 and
  443 must reach it; the certificate arrives within a minute of the first visit. Trouble: sudo journalctl -u caddy
MSG
fi
show <<MSG

  Next: install apps from the admin panel (Apps), or: sudo homeport app catalog
  Connect an AI agent (opencode, Claude Code…): sudo homeport mcp
MSG
