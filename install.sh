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
from=0 to=0 pct=0 began=$SECONDS
cmd_shown=""    # the command running
lines=()        # its last lines of output
seen=0          # lines of output so far in this step (to move the bar along)
drawn=0         # lines of the block on screen now
last_draw=0

term_cols() { local c; c="$(stty size <&3 2>/dev/null)" || c=""; c="${c##* }"; COLS="${c:-80}"; [ "$COLS" -ge 20 ] || COLS=80; }
COLS=80
if $live; then term_cols; trap term_cols WINCH; printf '\e[?25l' >&3; fi

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
    hide; printf '  \e[32mdone\e[0m  %s \e[2m(%ss)\e[0m\n' "$step" $((SECONDS - began)) >&3
  fi
  from=$1 to=$2 pct=$1 step=$3 began=$SECONDS cmd_shown="" lines=() seen=0
  printf '\n==> %s\n' "$step"
  if $live; then draw force; else printf '\n\033[1m==> %s\033[0m (%d%%)\n' "$step" "$pct" >&3; fi
}

# run CMD…: runs a command with its output into the log and under the bar. Its input is /dev/null (under curl | bash,
# the script itself is on stdin), or the file RUN_IN names.
run() {
  cmd_shown="$*" lines=()
  echo "\$ $*"
  draw force
  local line rc=0
  "$@" <"${RUN_IN:-/dev/null}" 2>&1 | while IFS= read -r line || [ -n "$line" ]; do
    line="${line##*$'\r'}"                         # progress meters: only their last state
    line="${line//$'\e'\[*([0-9;?])[a-zA-Z]/}"    # colours
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

say 0 45 "Installing Docker and the packages Homeport needs"
export DEBIAN_FRONTEND=noninteractive
apt_update || die "apt-get update failed (see the lines above); check this server's internet connection and apt sources"
run apt-get install -y -q ca-certificates curl gnupg docker.io docker-compose-v2

say 45 65 "Installing Caddy"
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

say 65 80 "Installing Homeport"
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

say 80 92 "Starting Homeport"
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
run systemctl daemon-reload
run systemctl enable --now docker caddy
run systemctl enable homeport
run systemctl restart homeport

if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
  note "Opening ports 80 and 443 in the firewall"
  run ufw allow 80/tcp && run ufw allow 443/tcp
fi

cmd_shown="waiting for the Homeport service to start"; draw force
for _ in $(seq 1 40); do [ -S /run/homeport/homeport.sock ] && break; sleep 0.5; done
[ -S /run/homeport/homeport.sock ] || die "the Homeport service didn't start (see: journalctl -u homeport)"

if ! homeport status | grep -q "Not set up"; then
  say 100 100 "Homeport is updated"
  hide
  homeport status | show
  exit 0
fi

# MARK: First setup (like YunoHost's post-install)
say 92 100 "Setting up this server"
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
# With no domain yet, <ip>.sslip.io works on this network with no setup: every device resolves it to this machine.
ask HOMEPORT_DOMAIN "Main domain for the portal and admin panel. Press Enter for ${first_ip:+$first_ip.sslip.io, which works on this network with no setup;} or type your own (its DNS must point here)" "${first_ip:+$first_ip.sslip.io}"
# Names Let's Encrypt can't certify (private addresses, test and local names) use Caddy's own certificates.
tls_default=letsencrypt
case "${HOMEPORT_DOMAIN:-}" in *.sslip.io|*.nip.io|*.test|*.local|*.lan|*.home.arpa|*.internal) tls_default=internal ;; esac
ask HOMEPORT_ADMIN "First admin's user name"
ask HOMEPORT_EMAIL "Email for Let's Encrypt notices (optional)"
ask HOMEPORT_TLS "Certificates: letsencrypt (public server) or internal (test machine)" "$tls_default"
while [ -z "${HOMEPORT_PASSWORD:-}" ] && $tty_ok; do
  ask HOMEPORT_PASSWORD "Password for ${HOMEPORT_ADMIN:-the admin} (at least 10 characters)" "" secret
  p2=""; printf 'Again: ' >/dev/tty; read -r -s p2 </dev/tty; echo >/dev/tty
  if [ "$HOMEPORT_PASSWORD" != "$p2" ]; then echo "They didn't match; try again." >/dev/tty; HOMEPORT_PASSWORD=""; fi
done
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
MSG
if [ "${HOMEPORT_TLS:-$tls_default}" = internal ]; then
  show <<MSG

  Certificates: Caddy's own test certificates, so your browser warns the first time; choose to continue.
MSG
  case "$HOMEPORT_DOMAIN" in
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
