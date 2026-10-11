# Homeport

Your own apps, users and domains on one Ubuntu server, in the spirit of YunoHost, built on Ubuntu 26.04 LTS,
Docker and Caddy. (Working name.)

- **Apps** from a catalog, each a Docker Compose project listening only on 127.0.0.1, each on its own domain.
- **One sign-in** (single sign-on) for every app: a portal on the main domain, and Caddy asks Homeport before letting
  anyone into an app. Access is per app, by group. Apps that trust it get the user in `Remote-User`,
  `Remote-Name`, `Remote-Email` and `Remote-Groups` (headers a browser sends with those names are removed first).
- **Domains and HTTPS**: Caddy gets Let's Encrypt certificates for every domain on its own.
- **Sign in with Homeport (OpenID Connect)**: Homeport is an OIDC provider on the main domain, so apps can use
  Homeport accounts directly. Nextcloud ("Log in with Homeport", groups included) and Vaultwarden (single sign-on)
  are set up for it on install. An app only gets people whose groups may open it.
- **Admin panel** at `https://<main domain>/admin`, and the `homeport` command for the same things over SSH.

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

```sh
./build.sh 0.1.0                       # on a dev machine: dist/ has the binaries and the installer
scp -r dist server: && ssh server
sudo ./dist/install.sh                 # Docker, Caddy (from Caddy's repository), the service
sudo homeport init --domain home.example.com --admin alice --email you@example.com
sudo homeport app install nextcloud --domain cloud.example.com
```

Point each domain's DNS at the server first. For a test machine with no public address, use `--tls internal`
(Caddy's own certificates).

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

LDAP for apps that can't do OIDC, email, backups, apps on a path (`example.com/app`), the YunoHost-style
diagnosis page, updating Homeport itself from the panel, and more catalog apps.
