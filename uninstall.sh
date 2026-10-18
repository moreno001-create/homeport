#!/usr/bin/env bash
# Homeport uninstaller (for a server set up with install.sh).
#
#   sudo bash uninstall.sh                 # removes Homeport; keeps every app's data as a copy
#   sudo bash uninstall.sh --delete-data   # …and deletes the apps' data too (asks twice)
#
# What it does: stops and removes each app's containers (their data stays in their folders), stops and removes the
# Homeport service and program, removes Caddy (Homeport installed it) and its package source, takes Homeport's block
# out of /etc/hosts, and moves /var/lib/homeport aside as /var/lib/homeport-removed-<date> (users, settings, the
# certificates, the Cloudflare or deSEC token, and every app's folder). It asks before doing anything.
# What it leaves: Docker, Ollama and its models, Samba and shares set up by hand, the firewall's rules (ports with
# nothing behind them are harmless; SSH stays open), and anything not made by Homeport. DNS records at Cloudflare or
# deSEC stay as they are (another Homeport using the same name points them at itself).
set -euo pipefail

delete_data=false
for a in "$@"; do
  case "$a" in
    --delete-data) delete_data=true ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "Unknown option: $a" >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" != 0 ]; then echo "Run it as root: sudo bash $0" >&2; exit 1; fi
if ! { exec 5</dev/tty; } 2>/dev/null; then echo "Run it at a terminal: it asks before removing anything." >&2; exit 1; fi
exec 5<&-

state=/var/lib/homeport
aside="/var/lib/homeport-removed-$(date +%Y%m%d-%H%M%S)"
projects=()
if command -v docker >/dev/null 2>&1; then
  while IFS= read -r p; do [ -n "$p" ] && projects+=("$p"); done < <(docker compose ls --all --format json 2>/dev/null |
    python3 -c 'import json,sys
for p in json.load(sys.stdin):
    if p["Name"].startswith("hp-") and "/homeport/apps/" in p["ConfigFiles"]:
        print(p["Name"] + "\t" + p["ConfigFiles"].split(",")[0])' 2>/dev/null || true)
fi

echo
echo "This removes Homeport from $(hostname):"
echo "  - stops and removes the containers of ${#projects[@]} app(s):"
for p in "${projects[@]}"; do echo "      ${p%%$'\t'*}"; done
echo "  - stops and removes the Homeport service and /usr/local/bin/homeport"
echo "  - removes Caddy and its package source (Homeport installed it)"
echo "  - takes Homeport's names out of /etc/hosts"
if $delete_data; then
  echo "  - DELETES $state: every app's data, users, settings and certificates"
else
  echo "  - moves $state to $aside"
  echo "    (every app's data, users, settings, certificates and the DNS token: root can read it; delete it when sure)"
fi
echo "It leaves Docker, Ollama and its models, Samba, the firewall rules and everything else."
echo
printf 'Type yes to go on: ' >/dev/tty; read -r ok </dev/tty
[ "$ok" = yes ] || { echo "Nothing was changed."; exit 0; }
if $delete_data; then
  printf 'The apps'"'"' data (photos, files, notes…) will be gone for good. Type delete to confirm: ' >/dev/tty; read -r ok </dev/tty
  [ "$ok" = delete ] || { echo "Nothing was changed."; exit 0; }
fi

step() { printf '\n==> %s\n' "$*"; }

step "Stopping Homeport"
systemctl stop homeport 2>/dev/null || true
systemctl disable homeport 2>/dev/null || true

step "Removing the apps' containers (their data stays in their folders)"
for p in "${projects[@]}"; do
  name="${p%%$'\t'*}" file="${p#*$'\t'}"
  echo "  $name"
  docker compose -p "$name" -f "$file" down --remove-orphans 2>&1 | sed 's/^/    /' ||
    echo "    (couldn't remove all of $name's containers: see docker ps -a)"
done

step "Removing Homeport's service and program"
rm -f /etc/systemd/system/homeport.service /etc/systemd/system/homeport-terminal.service /usr/local/bin/homeport
for f in /etc/systemd/system/*.d/homeport.conf; do
  # Ollama keeps its settings (context length, keep-alive): only Homeport's own units' drop-ins go.
  case "$f" in */ollama.service.d/*) ;; *) [ -e "$f" ] && rm -f "$f" ;; esac
done
systemctl daemon-reload

step "Removing Caddy"
systemctl disable --now caddy 2>/dev/null || true
DEBIAN_FRONTEND=noninteractive apt-get purge -y -q caddy 2>&1 | tail -2 || true
rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg

step "Taking Homeport's names out of /etc/hosts"
if grep -q '^# Homeport: this server' /etc/hosts; then
  cp /etc/hosts /etc/hosts.before-homeport-removal
  sed -i '/^# Homeport: this server/,/^# Homeport: end/d' /etc/hosts
  echo "  done (the old file is /etc/hosts.before-homeport-removal)"
else
  echo "  nothing to take out"
fi
rm -f /etc/avahi/services/homeport-smb.service

step "Homeport's folder"
if [ -d "$state" ]; then
  if $delete_data; then
    rm -rf "$state"
    echo "  deleted $state"
  else
    mv "$state" "$aside"
    chmod 700 "$aside"
    echo "  moved to $aside ($(du -sh "$aside" 2>/dev/null | cut -f1))"
  fi
fi
rm -rf /run/homeport

echo
echo "Homeport is removed from $(hostname)."
$delete_data || echo "The apps' data is in $aside/apps (one folder per app). Delete it when you're sure: sudo rm -rf $aside"
echo "Docker images the apps used are still there; to free that space: sudo docker image prune -a"
