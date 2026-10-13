# Apps can't be reached from outside the home
Signs: from outside, from my phone on mobile data, internet, port forwarding, 80, 443, DuckDNS, router, timeout from outside
Applies: any

## Why
Reaching an app from the internet needs every link: the home's address on the internet (not shared by the provider),
ports 80 and 443 forwarded by the router to this server, an internet name pointing at the home, and a certificate.

## Fix
1. `internet_status` checks every link and lists what's left, in order.
2. Ports going to another device: the router already forwards 80/443 elsewhere. That has to be changed in the router's
   own app by the admin; Homeport never takes a forwarding over.
3. Ports Homeport opened that stopped working (the router restarted): ask to `reopen_router_ports`.
4. "Shared address" (CGNAT): nothing from outside can come in through this connection; the provider can give a
   public address, or a tunnel (Cloudflare Tunnel, Tailscale) is needed.
5. The name pointing at an old address: Homeport updates DuckDNS/deSEC by itself; `service_logs` homeport shows the
   updates and any error (a wrong token, say).
6. Testing from inside the home: many routers don't loop back to the home's own address; test from mobile data.
