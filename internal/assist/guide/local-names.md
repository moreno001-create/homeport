# homeport.local (or an app's .local name) doesn't open
Signs: .local, mDNS, Bonjour, avahi, name not found, server not found, DNS_PROBE, homeport.local
Applies: any

## Why
.local names aren't in DNS: Homeport announces each one on the home network (Avahi on Linux, dns-sd on a Mac). They
don't work across VPNs or from other networks, some routers block the announcements between Wi-Fi and wired, and
Android only learned .local names recently.

## Fix
1. `services_status`: Avahi ("avahi-daemon") must be running on Linux. If it isn't, ask to `restart_service` avahi-daemon.
2. `service_logs` homeport: look for "Announcing … stopped" lines; a name announced twice on the network gets a
   suffix ("homeport-2.local").
3. From the device, try the server's IP address instead (shown on the Overview page). If the IP works and the name
   doesn't, it's the name announcement or the device; if neither works, it's the network or the server.
4. Windows and Android without .local support: use the IP address, or a free internet name (Internet access page).
