# The browser warns about the certificate, or won't open a page
Signs: certificate warning, NET::ERR_CERT, "not private", HSTS, can't open the admin page, .local, after changing the certificate
Applies: any

## Why
Names that only work at home (.local, .lan, .home.arpa, an IP address) get certificates from Caddy's own authority,
which browsers don't know, so they warn once per name. If a page was ever served with a Let's Encrypt certificate and
HSTS, the browser remembers that and refuses Caddy's certificate later without a way to click through.

## Fix
1. Check the name: `check_domain` shows who issued its certificate.
2. Home-only names: the warning is expected. Click through ("Advanced → continue"), or install Caddy's root
   certificate on the device (on Linux at /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt).
3. The browser refuses with no way through (HSTS): clear it in the browser. Chrome: chrome://net-internals/#hsts →
   "Delete domain security policies" for the name. Safari: quit it, delete ~/Library/Cookies/HSTS.plist, reopen.
   Firefox: History → forget about this site.
4. Since Homeport 0.7.7, HSTS is only sent with certificates browsers trust, so this doesn't come back.
