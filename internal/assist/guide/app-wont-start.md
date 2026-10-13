# An app is stopped, keeps restarting, or shows "partly running"
Signs: app down, 502, bad gateway, restarting, exited, crash loop, container, unhealthy, partly
Applies: any

## Why
Usually the app's own container: a wrong setting, a database that isn't ready yet, a full disk, or an update that
needs a migration. A 502 page means Caddy is fine but the app behind it isn't answering.

## Fix
1. `app_details` and `services_status`: which of the app's containers are down.
2. `app_logs` for that app: the last lines usually say why (look for "error", "permission", "no space", "migration").
3. `system_health`: a full disk stops every app; free space first (see "The disk is full").
4. If nothing points to a cause, ask to `restart_app`. If it fails again the same way, an `update_app` may fix a
   known bug; tell the admin what the logs say before changing settings.
5. An app stopped on purpose (Stop button on the Apps page) stays stopped until someone presses Start.
