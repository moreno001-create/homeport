# The admin panel or portal doesn't open
Signs: admin panel down, portal down, homeport not running, 502 on the main address, can't sign in, socket
Applies: any

## Why
Homeport itself stopped (crashed, or failed to start after an update), or Caddy isn't sending the main address to it.

## Fix
1. If this assistant answers, Homeport is running: check `services_status` and `service_logs` homeport for errors.
2. On the server's console: `systemctl status homeport` (Linux) and `journalctl -u homeport -n 100`; on a Mac,
   `launchctl print gui/$(id -u)/dev.homeport.homeport` and ~/Library/Application Support/Homeport/logs/.
3. `sudo systemctl restart homeport` (Linux) restarts it; running the installer again repairs a broken install
   without losing anything.
4. The main address shows Caddy's error: ask to `rebuild_proxy_config`.
