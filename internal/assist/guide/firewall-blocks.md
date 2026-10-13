# The firewall blocks something that should work
Signs: firewall, ufw, blocked, can't connect, port closed, ssh locked out, game server, app port
Applies: linux

## Why
With the firewall on, only the ports it lists are open. Homeport opens 80 and 443 and keeps SSH open; an app that
needs its own port (a game server, Mumble, Jitsi's video port) needs a rule for it.

## Fix
1. `firewall_status`: whether it's on, and the open ports.
2. The admin can open a port on the Firewall page (it explains each choice). Homeport never closes SSH.
3. Locked out of SSH: use the server's own console and run `sudo ufw allow ssh`.
