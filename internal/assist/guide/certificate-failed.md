# A Let's Encrypt certificate isn't issued or doesn't renew
Signs: certificate, Let's Encrypt, ACME, expired, renew, challenge failed, too many certificates, rate limit
Applies: any

## Why
Let's Encrypt checks the name by connecting to it on port 80 or 443 from the internet. It fails when the name doesn't
point here yet, the ports aren't forwarded, or after too many attempts (rate limits, for an hour to a week).
Names that only work at home (.local and the like) never get one: Caddy's own certificate is used.

## Fix
1. `internet_status`: the name must point here and both ports must be forwarded first.
2. `service_logs` caddy: look for "challenge failed", "rateLimited", "NXDOMAIN".
3. Once everything is in place, Caddy retries by itself; `rebuild_proxy_config` makes it try now.
4. Rate limited: wait as long as the error says; don't keep retrying.
