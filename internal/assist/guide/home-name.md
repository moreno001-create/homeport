# The home name (yourname.dedyn.io, or home.yourdomain.com) doesn't work, or its certificate is missing
Signs: home name, dedyn.io, deSEC, certificate, wildcard, DNS rebinding, rebind protection, can't find the name, NXDOMAIN, at home
Applies: any

## Why
The home name is a free deSEC name that points at this server's address on the home network, with a Let's Encrypt
certificate for it and every name under it, which Homeport gets by putting a proof in the name's DNS (no ports). It
breaks when:
- the router or a DNS filter (Pi-hole, AdGuard, some ISP routers) refuses DNS answers that point at home addresses
  ("DNS rebinding protection"), so devices find nothing;
- the deSEC token was deleted, so Homeport can't update the name or prove it owns it;
- a domain of your own stopped using deSEC's nameservers (ns1.desec.io, ns2.desec.org), or its registration ran out;
- Let's Encrypt limits how often it gives out certificates for the same name (about 5 a week).

## Fix
1. `run_checks` says which: the certificate, deSEC's last update, and whether this network finds the name.
2. "This network doesn't find the home name": allow the name in the router's DNS rebinding settings (often called
   "DNS rebind protection", with a list of allowed domains: add dedyn.io or the name itself), or in Pi-hole/AdGuard.
   Phones on mobile data never find it: that's expected, it's a home-only name.
3. "deSEC refused the token": make a new token at desec.io (Token Management); on the server, as root, put it in
   /var/lib/homeport/dyndns/<name>.token (one line). Homeport uses it from the next update.
4. No certificate yet: Homeport tries twice a day; meanwhile browsers warn as with Caddy's own certificate. A rate
   limit message says when to try again.
5. An app put on the internet under the home name points at the public address instead; reaching it from outside
   needs the router to forward the port (Internet access page).
