# Every site is down, or Caddy won't load its configuration
Signs: all sites down, connection refused, caddy failed, Caddyfile, adapting config, reload failed, port 443 in use
Applies: any

## Why
Caddy is the front door for every address. If it stopped, or refused a configuration, nothing answers. Another
program on ports 80/443 (another web server, a router app) stops it from starting.

## Fix
1. `services_status`: is Caddy running?
2. `service_logs` caddy: the reason ("address already in use", an error in the Caddyfile).
3. `proxy_config` shows the configuration Homeport wrote. Ask to `rebuild_proxy_config` to write it again and
   reload Caddy, then `restart_service` caddy if it's still down.
4. "Address already in use": another program has the port; with shell commands on, `ss -ltnp | grep -E ':80|:443'`
   says which. The admin decides what to stop.
