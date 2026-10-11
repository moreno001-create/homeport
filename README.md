# Homeport

Website: https://moreno001-create.github.io/homeport/

Your own apps, users and domains on one Ubuntu server, in the spirit of YunoHost, built on Ubuntu 26.04 LTS,
Docker and Caddy. (Working name.)

- **Apps** from a catalog, each a Docker Compose project listening only on 127.0.0.1, each on its own domain.
- **One sign-in** (single sign-on) for every app: a portal on the main domain, and Caddy asks Homeport before letting
  anyone into an app. Access is per app, by group. Apps that trust it get the user in `Remote-User`,
  `Remote-Name`, `Remote-Email` and `Remote-Groups` (headers a browser sends with those names are removed first).
- **Domains and HTTPS**: Caddy gets Let's Encrypt certificates for every domain on its own. Each domain can instead
  use Caddy's own certificates or one you upload (`homeport domain cert`), and the Domains page shows the certificate
  each site really serves and when it runs out.
- **Free domains**, like YunoHost's nohost.me: `name.dedyn.io` from [deSEC](https://desec.io) or `name.duckdns.org`
  from [DuckDNS](https://www.duckdns.org), with a token from your free account there (`homeport domain free`).
  Homeport keeps the name pointed at your public address when it changes, and apps get subdomains of it.
- **Sign in with Homeport (OpenID Connect)**: Homeport is an OIDC provider on the main domain, so apps can use
  Homeport accounts directly. Nextcloud ("Log in with Homeport", groups included) and Vaultwarden (single sign-on)
  are set up for it on install. An app only gets people whose groups may open it.
- **Admin panel** at `https://<main domain>/admin`, and the `homeport` command for the same things over SSH.
- **A web terminal** (off by default: Settings, or `homeport terminal on`): admins only, then the Linux login prompt.
- **An assistant** in the admin panel that can look after the server for you (see below).

Apps in the catalog: Nextcloud, Vaultwarden, Jellyfin, Immich, Audiobookshelf, Navidrome, Home Assistant,
AdGuard Home, Paperless-ngx, Stirling PDF, Actual Budget, FreshRSS, Memos, Mealie, Forgejo, Syncthing, Uptime Kuma
and whoami (a test page).

## How it fits together

```
browser ──https──> Caddy ──> main domain: Homeport (portal /, sign-in /login, admin /admin, API /api)
                        └──> app domain: forward_auth → Homeport /auth/verify → the app on 127.0.0.1:18xxx
homeport (CLI) ──/run/homeport/homeport.sock (root only)──> Homeport service
```

- `homeport serve` (systemd) keeps its state in `/var/lib/homeport/state.json` (root only) and each app in
  `/var/lib/homeport/apps/<id>/` (`compose.yaml` and `data/`).
- It writes `/etc/caddy/Caddyfile` (checked with `caddy validate` first) and reloads Caddy.
- Signing in on an app's own domain: the app's check sends the browser to the portal, which hands the sign-in back with
  a one-time code to `https://<app domain>/.homeport/callback`, so it works across unrelated domains.
  Signing out of the portal signs out of every app domain it reached.
- Passwords are Argon2id; session tokens are stored only as hashes; wrong passwords are rate-limited per address.

## Install

On a fresh Ubuntu 26.04 LTS server, with your main domain's DNS pointing at it:

```sh
curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash
```

(Served by GitHub Pages straight from this repository's `install.sh`.)

It installs Docker, Caddy (from Caddy's own repository) and Homeport, then asks for the main domain, the first
admin and their password. For an unattended install, set `HOMEPORT_DOMAIN`, `HOMEPORT_ADMIN`, `HOMEPORT_PASSWORD`
and optionally `HOMEPORT_EMAIL`, `HOMEPORT_TLS=internal` (test machines: Caddy's own certificates) and
`HOMEPORT_VERSION` (a release tag). Running it again updates Homeport.

```sh
sudo homeport app catalog
sudo homeport app install nextcloud --domain cloud.example.com
```

From source: `./build.sh 0.3.0`, copy `dist/` to the server, `sudo ./dist/install.sh`.

### On a Mac

Apple Silicon Macs make good home servers, with room for large local AI models. Run as yourself (not with sudo):

```sh
curl -fsSL https://moreno001-create.github.io/homeport/install-mac.sh | bash
```

(Served by GitHub Pages straight from this repository's `install-mac.sh`.)

It needs Homebrew; it installs Caddy, uses your Docker runtime (OrbStack, Docker Desktop or Colima; it installs
Colima if there's none), and runs Caddy and Homeport as your user with launchd (`~/Library/LaunchAgents/dev.homeport.*`),
so they start when you log in. Everything lives in `~/Library/Application Support/Homeport`, and `homeport` commands
need no sudo. It also looks for Ollama or LM Studio for local models (`HOMEPORT_INSTALL_OLLAMA=yes` installs Ollama).
The Mac must stay awake and logged in, with ports 80 and 443 forwarded to it from your router. If OrbStack holds
80/443 (its HTTPS domains, or ports forwarded from its Linux machines), the installer says how to turn that off.

## AI helpers

**The chat** (admin panel → Assistant) answers with Claude, a local model, or both:

- **Claude** with an Anthropic API key (Settings → Assistant chat; kept in `/var/lib/homeport/anthropic-api-key`,
  root-only). Default model `claude-opus-5-5` at medium effort. If a safety check declines a request, Anthropic re-runs
  it on its recommended fallback model (server-side fallbacks, Opus models).
- **A local model** on any OpenAI-compatible server: LM Studio, Ollama, llama.cpp. Homeport doesn't run one itself;
  a server without a strong graphics card points at a computer that has one (a Mac with Apple silicon works well).
- **Automatic** mode sends easy questions to the local model and troubleshooting or bigger jobs to Claude; the local
  model can also hand a conversation to Claude. Optional: ask before anything goes to Claude, and a monthly dollar
  limit (an estimate, in `/var/lib/homeport/assistant-usage.json`).
- Conversations are kept in memory, per admin; a restart of Homeport starts afresh.

The chat uses the same tools and rules as outside agents. Homeport's abilities are tools for AI agents, over MCP: `sudo homeport mcp` connects opencode, Claude Code or any
MCP client (on the server, or over `ssh`).

- **Look** tools (apps, logs, health, users, domains, Caddy config, domain checks) run straight away.
- **Change** tools (install, update, restart, remove, access, domains, groups, a command inside an app) wait for an
  admin: the admin panel's Assistant page, or `homeport approvals` / `homeport approve ID` / `homeport deny ID`.
- **Shell** (any command as root) is off until `homeport assistant shell on`, and each command is approved.
- Secrets are hidden from everything the AI sees; every call is in `/var/lib/homeport/audit.log` (`homeport audit`).
- **Shared memory** (`homeport memory`, `/var/lib/homeport/assistant-memory.md`): what was done to what, notes per
  app, and a hand-off of unfinished work, so the next agent carries on where the last one stopped. It also shows the
  latest changes from the audit log, which agents can't forget to record.

## The catalog

`internal/catalog/apps/*.yaml`, built into the binary. Each entry has a Compose template with `{{.Port}}`,
`{{.Domain}}`, `{{.Data}}`, `{{.AdminUser}}`, `{{.TimeZone}}` and `{{secret "name"}}` (random, made once at install,
listed under `secrets:`). `signIn` is `headers` (trusts Homeport's headers), `gate` (Homeport decides who may open it;
the app keeps its own accounts) or `own` (its own accounts and phone apps; public by default).

## Develop and test

```sh
go test ./...                                        # unit tests, including the whole sign-in flow
orb create ubuntu:resolute homeport-test             # an Ubuntu 26.04 machine in OrbStack
./build.sh && orb run -m homeport-test -u root bash dist/install.sh
```

## OpenID Connect

Discovery at `https://<main domain>/.well-known/openid-configuration`; code flow with PKCE, refresh tokens (each
works once), RS256 tokens signed with `/var/lib/homeport/oidc-signing-key.pem`. Scopes `openid profile email groups`;
`sub` and `preferred_username` are the user name, `groups` includes `admins` for admins. A manifest opts in with
`oidc: redirectPaths: [...]`; Homeport registers the app (client id = app id, secret `oidc_client_secret`) and fills in
`{{.Issuer}}` and `{{.ClientID}}`. Apps reach the main domain through `host-gateway` and trust
`/var/lib/homeport/ca-bundle.crt` (the system's roots plus Caddy's own on a `--tls internal` server).

## Not done yet

Saved chat history, LDAP for apps that can't do OIDC, email, backups, apps on a path (`example.com/app`), the YunoHost-style
diagnosis page, updating Homeport itself from the panel, and more catalog apps.
