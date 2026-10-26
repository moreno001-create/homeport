// Homeport admin panel. Plain DOM building (no innerHTML with data), talking to /api with the X-Homeport header.
"use strict";

const view = document.getElementById("view");
const dialog = document.getElementById("dialog");
let me = { settings: {} };

async function api(method, path, body) {
  const res = await fetch("/api" + path, {
    method,
    headers: { "X-Homeport": "1", ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  if (res.status === 401) { location.href = "/login?return=/admin"; throw new Error("Signed out"); }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

let toastTimer;
function toast(msg) {
  const t = document.getElementById("toast");
  t.textContent = msg; t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 4000);
}

function when(iso) {
  return iso ? new Date(iso).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "";
}

// appIcon: an app's logo, or its initial in one of six colours.
function appIcon(a) {
  if (a.icon) return h("img", { class: "glyph logo", src: a.icon, alt: "" });
  const hue = [...a.name].reduce((n, c) => n + c.codePointAt(0), 0) % 6;
  return h("span", { class: "glyph c" + hue, "aria-hidden": "true" }, a.name[0].toUpperCase());
}

function accessText(a) {
  return a.public ? "Anyone" : (a.groups || []).map(g => (g === "all" ? "All users" : g === "admins" ? "Admins" : g)).join(", ");
}

function openDialog(...content) {
  dialog.replaceChildren(...content);
  dialog.showModal();
}

// MARK: Views

const views = {
  async overview() {
    const o = await api("GET", "/overview");
    return [
      h("div", { class: "head" }, h("h1", {}, "Overview"), h("a", { class: "button", href: "https://" + (o.address || o.settings.mainDomain) + "/" }, "Open the portal")),
      h("div", { class: "stats" },
        stat(o.apps, "apps installed", "#apps"), stat(o.users, "users", "#users"), stat(o.domains, "domains", "#domains")),
      h("div", { class: "card stack" },
        h("h2", {}, "This server"),
        h("p", {}, "Portal and admin panel at ", h("b", {}, o.settings.mainDomain), "."),
        h("p", {}, "Certificates: ", o.settings.tls === "internal"
          ? "Caddy's own test certificates (browsers will warn; fine for a private network)."
          : "Let's Encrypt" + (o.settings.acmeEmail ? ", notices to " + o.settings.acmeEmail : "") + "."),
        h("div", {}, h("button", { onclick: setupDialog }, "How this server is set up"))),
      checksCard(),
      reachSpot(),
      updateCard(),
      systemCard(),
    ];
    function stat(n, label, href) { return h("a", { class: "stat", href, style: null }, h("b", {}, n), h("span", { class: "muted" }, label)); }
  },

  // (systemCard fills itself in: reading the machine takes a moment.)
  async apps() {
    const [installed, catalog] = await Promise.all([api("GET", "/apps"), api("GET", "/catalog")]);
    const rows = installed.map(a => h("tr", {},
      h("td", {}, h("div", { class: "app-name" }, appIcon(a), h("div", {}, h("b", {}, a.name), a.description ? h("div", { class: "small app-desc" }, a.description) : null, h("div", { class: "muted small" }, a.id, a.source && [" · from ", h("a", { href: a.source.url, target: "_blank", rel: "noopener" }, a.source.url.replace("https://github.com/", ""))])))),
      h("td", {}, h("a", { href: "https://" + (a.address || a.domain) + "/", target: "_blank", rel: "noopener" }, a.address || a.domain), " ",
        h("span", { class: "pill " + (a.internet ? "internet" : "home") }, a.internet ? "internet" : "home")),
      h("td", {}, accessText(a.access)),
      h("td", {}, h("span", { class: "pill " + a.status.split(" ")[0] }, a.status)),
      h("td", { class: "actions" },
        appRunButtons(a),
        h("button", { onclick: () => addressDialog(a) }, "Address"),
        h("button", { onclick: () => accessDialog(a) }, "Access"),
        h("button", { onclick: () => notesDialog(a) }, "Notes"),
        a.files ? h("button", { onclick: () => filesDialog(a) }, "Files") : null,
        h("button", { onclick: () => runJob("Updating " + a.name, () => api("POST", "/apps/" + a.id + "/update")) }, "Update"),
        h("button", { class: "danger", onclick: () => removeDialog(a) }, "Remove"))));
    return [
      h("div", { class: "head" }, h("h1", {}, "Apps"), h("button", { onclick: fromGitHubDialog }, "Install from GitHub")),
      installed.length
        ? h("div", { class: "list" }, h("table", {}, h("thead", {}, h("tr", {}, ["App", "Address", "Who can open it", "Status", ""].map(t => h("th", {}, t)))), h("tbody", {}, rows)))
        : h("p", { class: "muted" }, "Nothing installed yet. Pick an app below."),
      h("h2", {}, "Catalog"),
      h("div", { class: "catalog" }, catalog.map(c => h("div", { class: "entry" },
        h("div", { class: "top" }, appIcon(c), h("div", {}, h("b", {}, c.name), h("div", { class: "muted small" }, c.category))),
        h("p", {}, c.description),
        h("p", { class: "small" }, signInText(c.signIn)),
        h("div", {}, h("button", { class: "primary", onclick: () => installDialog(c) }, "Install"))))),
    ];
  },

  // Services: each app's containers and the server's own services, with restart, stop/start and logs.
  async services() {
    const data = await api("GET", "/services");
    clearTimeout(window.servicesTimer);
    window.servicesTimer = setTimeout(() => { if (location.hash === "#services" && !dialog.open) route(); }, 10000);
    const statePill = (state, label) => h("span", { class: "pill " + ({ running: "running", exited: "stopped", dead: "stopped", failed: "failed", stopped: "stopped" }[state] || "unknown") }, label || state);
    const appCards = data.apps.map(a => h("div", { class: "card stack svc" },
      h("div", { class: "head" },
        h("div", {}, h("h2", {}, a.name), h("a", { href: "https://" + (a.address || a.domain) + "/", target: "_blank", rel: "noopener", class: "small" }, a.address || a.domain)),
        h("div", { class: "row" }, appRunButtons({ id: a.id, name: a.name,
          status: a.containers.every(c => c.setup || c.state === "running") ? "running" : a.containers.some(c => !c.setup && c.state === "running") ? "partly running" : "stopped" }))),
      a.error ? h("p", { class: "error" }, a.error) : null,
      h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Container", "State", "CPU", "Memory", "Restarts", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, a.containers.map(c => h("tr", {},
          h("td", {}, h("b", {}, c.service), h("div", { class: "muted small mono" }, c.image)),
          h("td", {}, c.setup ? statePill("running", "setup done") : statePill(c.state), h("div", { class: "muted small" }, c.status)),
          h("td", {}, c.cpu || "–"),
          h("td", { class: "small" }, c.memory ? c.memory.split(" / ")[0] : "–"),
          h("td", {}, c.restarts ? h("span", { class: "pill unknown", title: "Docker restarted it after it stopped" }, String(c.restarts)) : "0"),
          h("td", { class: "actions" },
            h("button", { onclick: () => logsDialog(a.name + " · " + c.service, "/services/apps/" + a.id + "/" + c.service + "/logs") }, "Logs"),
            c.setup ? null : h("button", { onclick: () => containerAction(a, c, "restart") }, "Restart"),
            c.setup ? null : c.state === "running" || c.state === "restarting"
              ? h("button", { class: "danger", onclick: () => containerAction(a, c, "stop") }, "Stop")
              : h("button", { onclick: () => containerAction(a, c, "start") }, "Start")))))))));
    const since = t => { if (!t) return ""; const m = Math.round((Date.now() - new Date(t)) / 60000); return m < 60 ? m + " min" : m < 2880 ? Math.round(m / 60) + " h" : Math.round(m / 1440) + " days"; };
    const mb = n => n ? (n > 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GiB" : Math.round(n / (1 << 20)) + " MiB") : "–";
    const sysRows = data.system.map(s => h("tr", {},
      h("td", {}, h("b", {}, s.name), h("div", { class: "muted small" }, s.about)),
      h("td", {}, statePill(s.state), s.detail ? h("div", { class: "muted small" }, s.detail) : s.since ? h("div", { class: "muted small" }, "for " + since(s.since)) : null),
      h("td", { class: "small" }, mb(s.memory)),
      h("td", { class: "actions" },
        s.hasLogs ? h("button", { onclick: () => logsDialog(s.name, "/services/system/" + s.id + "/logs") }, "Logs") : null,
        s.canRestart ? h("button", { onclick: () => restartSystem(s) }, "Restart") : null)));
    return [
      h("div", { class: "head" }, h("h1", {}, "Services"), h("button", { onclick: () => route() }, "Refresh")),
      h("h2", {}, "Apps"),
      data.apps.length ? h("div", { class: "stack" }, appCards) : h("p", { class: "muted" }, "No apps installed yet."),
      h("h2", {}, "This server"),
      data.system.length
        ? h("div", { class: "list" }, h("table", {}, h("thead", {}, h("tr", {}, ["Service", "State", "Memory", ""].map(t => h("th", {}, t)))), h("tbody", {}, sysRows)))
        : h("p", { class: "muted" }, "Not available on this system."),
    ];
  },

  // Firewall: ufw on or off, and which ports are open. SSH, 80 and 443 always stay open.
  async firewall() {
    const fw = await api("GET", "/firewall");
    if (!fw.available) return [h("div", { class: "head" }, h("h1", {}, "Firewall")), h("div", { class: "card" }, h("p", {}, fw.why))];
    const protoText = p => ({ tcp: "TCP", udp: "UDP" }[p] || "TCP and UDP");
    const turn = async on => {
      const msg = on
        ? "Turn the firewall on? SSH (port " + fw.sshPort + "), 80 and 443 stay open; everything else coming in is refused, apart from the ports below."
        : "Turn the firewall off? Every port that something listens on can then be reached from your network (and the internet, if your router forwards it).";
      if (!confirm(msg)) return;
      try { await api("POST", "/firewall/" + (on ? "on" : "off")); toast(on ? "The firewall is on" : "The firewall is off"); } catch (err) { toast(err.message); }
      route();
    };
    const close = async r => {
      if (!confirm("Close " + r.port + " (" + protoText(r.proto) + ")" + (r.from ? " from " + r.from : "") + "?")) return;
      try { await api("DELETE", "/firewall/rules", { port: r.port, proto: r.proto || "", from: r.from || "" }); toast("Closed"); } catch (err) { toast(err.message); }
      route();
    };
    const port = h("input", { required: true, placeholder: "8080, or 6000:6010", inputmode: "numeric", "aria-label": "Port" });
    const proto = h("select", { "aria-label": "Protocol" }, h("option", { value: "tcp" }, "TCP"), h("option", { value: "udp" }, "UDP"), h("option", { value: "" }, "TCP and UDP"));
    const from = h("select", { "aria-label": "From" }, h("option", { value: "" }, "From anywhere"),
      fw.networks.map(n => h("option", { value: n }, "Only from this network (" + n + ")")));
    const label = h("input", { placeholder: "What it's for (optional)", maxlength: "60", "aria-label": "What it's for" });
    const open = h("form", { class: "card stack", onsubmit: async e => {
      e.preventDefault();
      try { await api("POST", "/firewall/rules", { port: port.value.trim(), proto: proto.value, from: from.value, label: label.value }); toast("Opened " + port.value); route(); } catch (err) { toast(err.message); }
    } },
      h("h2", {}, "Open a port"),
      h("div", { class: "row" }, h("label", {}, "Port", port), h("label", {}, "Protocol", proto), h("label", {}, "From", from)),
      h("label", {}, "What it's for", label),
      h("div", {}, h("button", { class: "primary", type: "submit" }, "Open it")));
    return [
      h("div", { class: "head" }, h("h1", {}, "Firewall"), h("span", { class: "pill " + (fw.active ? "running" : "stopped") }, fw.active ? "on" : "off")),
      h("div", { class: "card stack" },
        h("p", {}, fw.active
          ? "The firewall is on: only the ports below can be reached from outside this server."
          : "The firewall is off: anything listening on this server can be reached from your network (and from the internet, if your router forwards it there)."),
        h("p", { class: "muted small" }, "SSH (port " + fw.sshPort + "), 80 and 443 always stay open, so you can't lock yourself out and every site keeps working."),
        h("div", {}, fw.active ? h("button", { class: "danger", onclick: () => turn(false) }, "Turn it off") : h("button", { class: "primary", onclick: () => turn(true) }, "Turn it on"))),
      h("h2", {}, "Open ports"),
      fw.rules.length
        ? h("div", { class: "list" }, h("table", {},
            h("thead", {}, h("tr", {}, ["Port", "Protocol", "From", "What", ""].map(t => h("th", {}, t)))),
            h("tbody", {}, fw.rules.map(r => h("tr", {},
              h("td", {}, h("b", { class: "mono" }, r.port)), h("td", {}, protoText(r.proto)), h("td", {}, r.from || "anywhere"),
              h("td", { class: "small" }, (r.comment || "").replace(/^Homeport: /, "")),
              h("td", { class: "actions" }, r.protected ? h("span", { class: "muted small" }, "always open") : h("button", { class: "danger", onclick: () => close(r) }, "Close")))))))
        : h("p", { class: "muted" }, "No ports opened yet. Turning the firewall on opens SSH, 80 and 443."),
      open,
      fw.published.length ? h("div", { class: "stack" },
        h("h2", {}, "Opened by apps"),
        h("p", { class: "muted small" }, "These apps publish a port on the network themselves (Syncthing's sync port, say). Docker opens them whatever the firewall says; to close one, remove the app or change its setup."),
        h("div", { class: "list" }, h("table", {}, h("tbody", {}, fw.published.map(p => h("tr", {}, h("td", { class: "mono" }, p.port), h("td", {}, p.container.replace(/^hp-/, "").replace(/-\d+$/, "")))))))) : null,
    ];
  },

  // Backups: when, what, where to (SFTP/NAS, S3, another Homeport, a drive), and restoring an app.
  async backups() {
    const b = await api("GET", "/backup");
    const kindName = { sftp: "SSH / NAS", s3: "S3 storage", homeport: "Another Homeport", local: "Drive" };
    const when = d => d ? new Date(d).toLocaleString() : "";
    const password = h("span", { class: "mono secret" }, "••••••••••••");
    const showPw = h("button", { onclick: async () => {
      if (showPw.textContent === "Hide") { password.textContent = "••••••••••••"; showPw.textContent = "Show"; return; }
      try { password.textContent = (await api("GET", "/backup/password")).password; showPw.textContent = "Hide"; } catch (err) { toast(err.message); }
    } }, "Show");
    const copyPw = h("button", { onclick: async () => {
      try { await navigator.clipboard.writeText((await api("GET", "/backup/password")).password); toast("Copied the password"); } catch { toast("Couldn't copy: use Show"); }
    } }, "Copy");

    const time = h("input", { type: "time", value: b.time || "", "aria-label": "Daily at" });
    const keep = (v, l) => h("input", { type: "number", min: "0", max: "400", value: v, "aria-label": l });
    const daily = keep(b.keepDaily, "Daily backups to keep"), weekly = keep(b.keepWeekly, "Weekly backups to keep"), monthly = keep(b.keepMonthly, "Monthly backups to keep");
    const pause = h("input", { type: "checkbox", checked: !b.noPause });
    const appBoxes = b.apps.map(a => ({ id: a.id, box: h("input", { type: "checkbox", checked: !b.skip.includes(a.id) }), name: a.name }));
    const settings = h("form", { class: "card stack", onsubmit: async e => {
      e.preventDefault();
      try {
        await api("PUT", "/backup/settings", { time: time.value, keepDaily: +daily.value, keepWeekly: +weekly.value, keepMonthly: +monthly.value,
          noPause: !pause.checked, skip: appBoxes.filter(x => !x.box.checked).map(x => x.id) });
        toast("Saved");
      } catch (err) { toast(err.message); }
    } },
      h("h2", {}, "When and what"),
      h("div", { class: "row" }, h("label", {}, "Every day at (empty: only when you choose Back up now)", time)),
      h("div", { class: "row" }, h("label", {}, "Keep daily", daily), h("label", {}, "Keep weekly", weekly), h("label", {}, "Keep monthly", monthly)),
      h("label", { class: "check" }, pause, "Pause each app for the moment its files are read (keeps databases consistent)"),
      b.apps.length ? h("fieldset", { class: "apps-pick" }, h("legend", {}, "Apps to back up (Homeport's own settings always are)"),
        appBoxes.map(x => h("label", { class: "check" }, x.box, x.name))) : null,
      h("div", {}, h("button", { class: "primary", type: "submit" }, "Save")));

    const destCards = b.destinations.map(d => {
      const off = h("input", { type: "checkbox", checked: !d.off, onchange: async () => {
        try { await api("PATCH", "/backup/destinations/" + d.id, { off: !off.checked }); toast(off.checked ? "In the daily backup" : "Left out of the daily backup"); } catch (err) { toast(err.message); }
      } });
      const status = d.lastRun
        ? (d.lastOK ? h("span", { class: "pill running" }, "backed up") : h("span", { class: "pill failed" }, "didn't work"))
        : h("span", { class: "pill unknown" }, "not yet");
      return h("div", { class: "card stack" },
        h("div", { class: "head" }, h("div", {}, h("h3", {}, d.name), h("div", { class: "muted small" }, kindName[d.kind] + " · " + d.where)), status),
        d.lastRun ? h("p", { class: "small" }, "Last: " + when(d.lastRun) + (d.lastOK ? " · " + sizeText(d.lastAdded) + " new" : "")) : null,
        d.lastError ? h("p", { class: "error small" }, d.lastError) : null,
        h("label", { class: "check" }, off, "In the daily backup"),
        h("div", { class: "row" },
          h("button", { onclick: () => runJob("Backing up to " + d.name, () => api("POST", "/backup/run", { destination: d.id })) }, "Back up now"),
          h("button", { onclick: () => snapshotsDialog(d, b.apps) }, "Backups and restore"),
          h("button", { class: "danger", onclick: async () => {
            if (!confirm("Stop backing up to " + d.name + "? The backups already there stay there.")) return;
            try { await api("DELETE", "/backup/destinations/" + d.id); route(); } catch (err) { toast(err.message); }
          } }, "Remove")));
    });

    return [
      h("div", { class: "head" }, h("h1", {}, "Backups"),
        h("button", { class: "primary", disabled: !b.destinations.length || !!b.running, onclick: () => runJob("Backing up", () => api("POST", "/backup/run", {})) }, b.running ? "Busy: " + b.running : "Back up now")),
      h("div", { class: "card stack" },
        h("h2", {}, "Encryption password"),
        h("p", {}, "Every backup is encrypted with this password before it leaves the server. ", h("b", {}, "Keep a copy somewhere else"), " (a password manager): if this server is lost, the backups can only be read with it."),
        h("div", { class: "row pw" }, password, showPw, copyPw)),
      h("div", { class: "head" }, h("h2", {}, "Where backups go"), h("button", { onclick: () => destinationDialog(b) }, "Add a place")),
      b.destinations.length ? h("div", { class: "stack" }, destCards) : h("p", { class: "muted" }, "Nowhere yet: add a NAS or SSH server, S3 storage, another Homeport, or a drive."),
      settings,
      b.lastLog ? h("details", { class: "card" }, h("summary", {}, "The last daily backup (" + (b.lastScheduled || "") + ")"), h("pre", { class: "log" }, b.lastLog)) : null,
      h("details", { class: "card" }, h("summary", {}, "If this server is lost"),
        h("div", { class: "stack small" },
          h("p", {}, "Install Homeport on a new server (the same main address), then as root, with ", h("code", {}, "RESTIC_REPOSITORY"), " (the place) and ", h("code", {}, "RESTIC_PASSWORD"), " (the password above) set:"),
          h("pre", { class: "log" }, "systemctl stop homeport\nrestic restore latest --tag homeport --target /\nrestic restore latest --tag app:ID --target /    # for each app\nsystemctl start homeport"),
          h("p", {}, "Homeport's settings bring back the users, domains and each app's passwords; then choose Update on each app (Apps page) to start it. Restic's place names: ", h("code", {}, "sftp:user@host:/folder"), ", ", h("code", {}, "s3:https://address/bucket/homeport"), ", ", h("code", {}, "rest:https://address/user/"), " or the drive's folder."),
          h("p", {}, "To keep backing up to places that have backups made with another password: ", h("button", { onclick: () => usePasswordDialog() }, "Use the password of earlier backups")))),
    ];
  },

  // Internet access: every link from the internet to this server, checked, and what to do about the missing ones.
  async internet() {
    const st = await api("GET", "/internet");
    const mark = (ok, wait) => h("span", { class: "tick " + (ok ? "ok" : wait ? "wait" : "bad"), "aria-hidden": "true" }, ok ? "✓" : wait ? "…" : "✗");
    const item = (ok, title, detail, wait) => h("li", {}, mark(ok, wait), h("div", {}, h("b", {}, title), detail ? h("div", { class: "muted small" }, detail) : null));
    const fw = st.forwards || [];
    const forwarded = fw.length > 0 && fw.every(f => f.mine);
    const ports = st.homeName ? "port " + st.port : "ports 80 and 443";
    const portSuffix = st.port && st.port !== 443 ? ":" + st.port : "";
    const elsewhere = fw.filter(f => f.to && !f.mine);
    const certText = c => c.trusted ? "valid until " + new Date(c.notAfter).toLocaleDateString() + " (" + (c.issuer || "") + ")"
      : c.error ? c.error : c.issuer ? "only Caddy's own certificate so far" : "none yet";
    const openPorts = async port => {
      const which = port ? "port " + port + " (to this server's 443)" : ports;
      if (!confirm("Ask the router (" + st.router.name + ") to send " + which + " to this server? Apps you put on the internet become reachable by anyone (each still behind its sign-in, unless you made it public).")) return;
      try { await api("POST", "/internet/router", { open: true, port: port || 0 }); toast("The router forwards " + which + " here now"); } catch (err) { toast(err.message); }
      route();
    };
    const everything = st.homeName ? h("div", { class: "card stack" },
      h("h2", {}, "Everything on the internet"),
      st.everything
        ? h("p", {}, h("span", { class: "pill internet" }, "on"), " The portal, admin panel, AI page and every app under " + st.homeName + " are reachable from anywhere, at https://" + st.homeName + portSuffix + "/ and the same with each app's name" + (portSuffix ? ". Every address ends in " + portSuffix + ", at home too." : "."))
        : h("p", {}, "Put the portal, admin panel, AI page and every app under " + st.homeName + " on the internet at once" + (portSuffix ? ", at addresses ending in " + portSuffix + " (at home too)" : "") + ". Everyone still has to sign in, but the sign-in page is open to the whole internet: use strong passwords."),
      h("div", { class: "row" }, h("button", { class: st.everything ? "" : "primary", onclick: async () => {
        if (!confirm(st.everything ? "Make everything under " + st.homeName + " home-only again? Apps you put on the internet one by one stay there." : "Put everything under " + st.homeName + " on the internet?")) return;
        const j = await runJob(st.everything ? "Back to home only" : "Everything on the internet", () => api("POST", "/internet/everything", { on: !st.everything }));
        if (j && j.status === "done") toast("Done");
      } }, st.everything ? "Back to home only" : "Put everything on the internet"))) : null;
    const altPort = st.router.found && !forwarded && elsewhere.length && st.homeName && st.port === 443
      ? h("button", { class: "primary", onclick: () => openPorts(8443) }, "Use port 8443 instead (beside the other device)") : null;
    const routerBtn = st.router.found && !forwarded && !elsewhere.length
      ? h("button", { class: "primary", onclick: () => openPorts(0) }, "Open " + ports + " on the router")
      : st.routerPorts ? h("button", { onclick: async () => {
          if (!confirm("Stop forwarding " + ports + " to this server? Apps on the internet become unreachable from outside.")) return;
          try { await api("POST", "/internet/router", { open: false }); toast("Closed"); } catch (err) { toast(err.message); }
          route();
        } }, "Close them again") : null;
    return [
      h("div", { class: "head" }, h("h1", {}, "Internet access"), h("span", { class: "pill " + (st.ready ? "running" : "unknown") }, st.ready ? "ready" : "not yet"), h("button", { onclick: () => route() }, "Check again")),
      h("div", { class: "card stack" },
        h("ul", { class: "checks" },
          item(!!st.publicIP && !st.shared, "Internet connection", st.publicIP
            ? (st.shared ? "Shared address (" + (st.router.outside || st.publicIP) + "): nothing from outside can come in through this connection." : "This home is at " + st.publicIP + " on the internet.")
            : "Can't reach the internet."),
          item(st.router.found, "Router", st.router.found ? st.router.name + ": takes automatic port forwarding (UPnP). This server is " + st.router.local + " to it." : st.router.error),
          item(forwarded, ports[0].toUpperCase() + ports.slice(1), forwarded ? "Forwarded to this server" + (portSuffix ? " (to its 443)" : "") + (st.routerPorts ? " (Homeport keeps it open)." : ".")
            : elsewhere.length ? "The router already sends them to another device: " + elsewhere.map(f => f.port + " → " + f.to + (f.what ? " (" + f.what + ")" : "")).join(", ") + ". Change that in the router's app (port forwarding), then check again."
            : "Not forwarded yet."),
          st.homeName
            ? item(true, "Home name", st.homeName + ": apps keep their addresses; putting one on the internet points its name (and signin." + st.homeName + ", for signing in) at this home's public address." + (portSuffix ? " From anywhere, add " + portSuffix + " to their addresses." : ""))
            : item(!!st.name && st.namePoints, "Internet name", st.name ? st.name + (st.namePoints ? " points here." : " points at " + ((st.nameIPs || []).join(", ") || "nothing") + ", not here yet.") : "None yet.", st.name && !st.namePoints),
          item(!!st.cert.trusted, "Certificate", st.name ? certText(st.cert) : "Comes with the internet name.", st.name && !st.cert.trusted)),
        h("div", { class: "row" }, routerBtn, altPort, st.name ? null : h("button", { class: "primary", onclick: freeDomainDialog }, "Get a free internet name"))),
      everything,
      (st.steps || []).length ? h("div", { class: "card stack" }, h("h2", {}, "What's left"), h("ol", { class: "steps" }, st.steps.map(t => h("li", {}, t)))) : h("p", {}, "Everything's in place: apps you put on the internet are reachable from anywhere, with real certificates."),
      h("h2", {}, "Apps"),
      st.apps.length ? h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["App", "Address", "Certificate", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, st.apps.map(a => h("tr", {},
          h("td", {}, h("b", {}, a.name)),
          h("td", {}, h("a", { href: "https://" + a.domain + (a.internet ? portSuffix : "") + "/", target: "_blank", rel: "noopener" }, a.domain + (a.internet ? portSuffix : "")), " ", h("span", { class: "pill " + (a.internet ? "internet" : "home") }, a.internet ? "internet" : "home")),
          h("td", { class: "small" }, a.internet || st.homeName ? certText(a.cert) : "Caddy's own (home network)"),
          h("td", { class: "actions" }, st.everything ? h("span", { class: "muted small" }, "with everything") : h("button", { onclick: () => {
            if (!a.internet && !st.name) { toast("Get a free internet name first"); return; }
            runJob((a.internet ? "Taking " : "Putting ") + a.name + (a.internet ? " off" : " on") + " the internet", () => api("POST", "/apps/" + a.id + "/internet", { on: !a.internet }));
          } }, a.internet ? "Take off the internet" : "Put on the internet")))))))
        : h("p", { class: "muted" }, "No apps yet."),
    ];
  },

  // AI models: what runs well on this server, downloads with a progress bar, and the one the assistant uses.
  async models() {
    const st = await api("GET", "/ai-models");
    const head = h("div", { class: "head" }, h("h1", {}, "AI models"),
      h("a", { class: "button", href: "https://" + (me.aiAddress || "ai." + me.settings.mainDomain) + "/", target: "_blank", rel: "noopener" }, "Open the AI page ↗"));
    if (st.error) return [head, h("div", { class: "card stack" }, h("p", {}, st.error),
      h("p", { class: "muted small" }, "The local model server is set under ", h("a", { href: "#settings" }, "Settings → Assistant chat"), "."))];
    return [head, h("div", { class: "stack", id: "models-top" }, ...modelsTop(st)), findModels(st)];
  },

  // Storage: a switchboard of what lives on which drive. Flip switches, then move them all in one go.
  async storage() {
    const st = await api("GET", "/storage");
    const drives = st.drives;
    const targets = drives.filter(d => d.mounted && d.usable);
    const chosen = {}; // item id → drive mount
    const head = h("div", { class: "head" }, h("h1", {}, "Storage"), h("button", { onclick: () => { route(); } }, "Check again"));
    const driveCard = d => {
      const used = d.size ? Math.round((d.size - d.free) * 100 / d.size) : 0;
      const actions = [];
      if (d.why && !d.mounted && d.uuid && ["ext4", "ext3", "xfs", "btrfs", "f2fs"].includes(d.fs)) actions.push(h("button", { class: "primary", onclick: () => mountDrive(d) }, "Mount it"));
      else if (d.mounted && !d.lasting) actions.push(h("button", { class: "primary", onclick: () => mountDrive(d) }, "Keep it mounted"));
      return h("div", { class: "card stack drive" + (d.usable && d.mounted ? "" : " off") },
        h("div", { class: "row" }, h("b", {}, driveName(d)), d.system ? h("span", { class: "pill" }, "system") : null, h("span", { class: "muted small" }, d.fs)),
        d.mounted ? [h("span", { class: "meter-bar long" }, h("span", { class: "meter-fill w" + Math.min(100, Math.round(used / 5) * 5) + (used > 90 ? " hot" : "") })),
          h("div", { class: "muted small" }, gbText(d.free) + " free of " + gbText(d.size))] : h("div", { class: "muted small" }, gbText(d.size) + " · not mounted"),
        d.why ? h("p", { class: "small " + (d.usable ? "muted" : "warn-text") }, d.why) : null,
        actions.length ? h("div", { class: "row" }, actions) : null);
    };
    const summary = h("div", { class: "card stack move-summary", hidden: true });
    const rows = st.items.map(it => {
      const cells = targets.map(d => {
        const here = it.drive === d.mount;
        const input = h("input", { type: "radio", name: "to-" + it.id, value: d.mount, checked: here, disabled: !!it.fixed || !it.drive,
          onchange: () => { if (here) delete chosen[it.id]; else chosen[it.id] = d.mount; tr.classList.toggle("moving", !!chosen[it.id]); drawSummary(); } });
        return h("td", { class: "switch-cell" }, h("label", { class: "switch-opt", title: here ? "It's here now" : "Move it here" }, input, h("span", {}, here ? "here" : "move here")));
      });
      const tr = h("tr", {},
        h("td", {}, h("b", {}, it.name), h("div", { class: "muted small" }, it.about),
          it.fixed ? h("div", { class: "muted small" }, it.fixed) : !it.drive ? h("div", { class: "error small" }, "Its drive isn't there") : null),
        h("td", { class: "small" }, gbText(it.size)),
        ...cells);
      return tr;
    });
    function drawSummary() {
      const ids = Object.keys(chosen);
      summary.hidden = !ids.length;
      if (!ids.length) return;
      const byDrive = {};
      for (const id of ids) {
        const it = st.items.find(x => x.id === id);
        (byDrive[chosen[id]] ||= []).push(it);
      }
      summary.replaceChildren(
        h("h2", {}, "Ready to move"),
        ...Object.entries(byDrive).map(([mount, its]) => {
          const d = drives.find(x => x.mount === mount);
          const total = its.reduce((n, it) => n + it.size, 0);
          return h("p", {}, its.map(it => it.name).join(", "), " → ", h("b", {}, driveName(d)), ": " + gbText(total) + " (" + gbText(d.free) + " free there)",
            total * 1.05 > d.free ? h("span", { class: "error" }, " Not enough room.") : null);
        }),
        h("ul", { class: "small muted" }, ids.map(id => h("li", {}, st.items.find(x => x.id === id).down))),
        h("p", { class: "muted small" }, "Each one is copied while it keeps running, stopped only for a last copy of what changed, then started from its new place and checked. The old copy is deleted only once it works; if anything goes wrong it goes back where it was. Big things take a while (about an hour per 500 GB on a typical hard drive)."),
        h("div", { class: "row" }, h("button", { class: "primary", onclick: () => startMove(ids.map(id => ({ item: id, drive: chosen[id] }))) }, "Move " + ids.length + (ids.length > 1 ? " things" : " thing")),
          h("button", { onclick: () => route() }, "Start over")));
    }
    const newApps = h("select", { onchange: async () => {
      try { await api("PUT", "/storage/new-apps", { drive: newApps.value }); toast("New apps go to " + newApps.selectedOptions[0].textContent); } catch (err) { toast(err.message); route(); }
    } }, targets.map(d => h("option", { value: d.mount, selected: st.newApps ? d.mount === st.newApps : d.system }, driveName(d))));
    const progress = h("div", { class: "stack" });
    const running = (st.busy || []).find(b => b.startsWith("move "));
    let saved = null;
    try { saved = sessionStorage.getItem("homeport-move-job"); } catch { /* private window */ }
    if (saved) followMove(saved, progress);
    return [
      head,
      st.missing && st.missing.length ? h("p", { class: "error" }, "These apps' drive isn't there (unplugged, or not mounted): " + st.missing.join(", ") + ". Plug it back in, or mount it, and start them again.") : null,
      running && !saved ? h("p", { class: "warn-text" }, "A move is under way (" + running.slice(5) + ").") : null,
      progress,
      h("div", { class: "drives" }, drives.map(driveCard)),
      targets.length < 2
        ? h("div", { class: "card stack" }, h("p", {}, "There's only one drive to put things on. Plug in or mount another drive (formatted as ext4 on Linux), then check again."))
        : [h("div", { class: "list switchboard" }, h("table", {},
            h("thead", {}, h("tr", {}, h("th", {}, "What"), h("th", {}, "Size"), ...targets.map(d => h("th", {}, driveName(d))))),
            h("tbody", {}, rows))),
          summary,
          h("div", { class: "card stack" }, h("label", {}, "Where new apps go (the install window asks too)", newApps))],
    ];

    async function mountDrive(d) {
      if (!confirm((d.mounted ? "Keep " : "Mount ") + driveName(d) + " mounted, also after restarts? Homeport adds it to /etc/fstab (with nofail, so the server still starts if the drive is missing).")) return;
      try { const r = await api("POST", "/storage/mount", { device: d.device }); toast("Mounted at " + r.mount); } catch (err) { toast(err.message); }
      route();
    }
    async function startMove(moves) {
      if (!confirm("Move " + moves.length + (moves.length > 1 ? " things" : " thing") + " now? Each stops briefly near the end of its copy.")) return;
      try {
        const j = await api("POST", "/storage/move", { moves });
        try { sessionStorage.setItem("homeport-move-job", j.id); } catch { /* private window */ }
        summary.hidden = true;
        followMove(j.id, progress);
      } catch (err) { toast(err.message); }
    }
  },

  // Maintenance: Homeport's own updates, the system's (Ubuntu's packages) and the apps', and old versions' disk space.
  async maintenance() {
    const m = await api("GET", "/maintenance");
    const sys = m.system;
    const done = j => { if (j) route(); };
    const at = d => new Date(d).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
    let sysCard;
    if (m.systemError) {
      sysCard = h("div", { class: "card stack" }, h("h2", {}, "System updates"), h("p", { class: "error" }, m.systemError),
        h("div", {}, h("button", { onclick: () => runJob("Checking for system updates", () => api("POST", "/maintenance/system/check")).then(done) }, "Check now")));
    } else if (!sys || !sys.supported) {
      sysCard = h("div", { class: "card stack" }, h("h2", {}, "System updates"), h("p", { class: "muted" }, (sys && sys.why) || "Not available on this server."));
    } else {
      const n = sys.packages.length;
      sysCard = h("div", { class: "card stack" },
        h("div", { class: "head" }, h("h2", {}, "System updates"),
          h("button", { onclick: () => runJob("Checking for system updates", () => api("POST", "/maintenance/system/check")).then(done) }, "Check now")),
        h("p", {}, n
          ? [h("span", { class: "pill " + (sys.security ? "failed" : "partly") }, n + " update" + (n > 1 ? "s" : "")), " ",
             sys.security ? h("b", {}, sys.security + " for security") : null, " ",
             h("span", { class: "muted small" }, "as of " + at(sys.checked))]
          : [h("span", { class: "pill done" }, "Up to date"), " ", h("span", { class: "muted small" }, "as of " + at(sys.checked) + " (Check now asks the package servers)")]),
        h("p", { class: "muted small" }, sys.autoSecurity
          ? "Security updates install by themselves every day (unattended upgrades)."
          : "Security updates don't install by themselves on this server: install them here now and then."),
        n ? h("details", {}, h("summary", { class: "small" }, "Which packages"),
          h("div", { class: "list" }, h("table", {}, h("thead", {}, h("tr", {}, ["Package", "Now", "New", ""].map(t => h("th", {}, t)))),
            h("tbody", {}, sys.packages.map(p => h("tr", {}, h("td", {}, p.name), h("td", { class: "small" }, p.from), h("td", { class: "small" }, p.to),
              h("td", {}, p.security ? h("span", { class: "pill failed" }, "security") : null))))))) : null,
        sys.restarts && sys.restarts.length ? h("p", { class: "small" }, "These updates restart " + sys.restarts.join(", ") +
          (sys.restarts.includes("docker") || sys.restarts.includes("containerd") ? ": the apps stop for a minute while Docker restarts." : ".")) : null,
        sys.rebootNeeded ? h("p", {}, h("span", { class: "pill partly" }, "restart needed"), " ",
          "The server needs a restart to finish earlier updates" + (sys.rebootFor && sys.rebootFor.length ? " (" + sys.rebootFor.slice(0, 5).join(", ") + ")" : "") + ".") : null,
        h("div", { class: "row" },
          n ? h("button", { class: "primary", onclick: () => {
            if (!confirm("Install " + n + " system update" + (n > 1 ? "s" : "") + "? It takes a few minutes" + (sys.restarts && sys.restarts.length ? "; " + sys.restarts.join(", ") + " restart" : "") + ".")) return;
            runJob("Installing system updates", () => api("POST", "/maintenance/system/upgrade")).then(done);
          } }, "Install updates") : null,
          h("button", { class: sys.rebootNeeded ? "primary" : "", onclick: restart }, "Restart the server")));
    }

    const list = m.apps || [];
    const waiting = list.filter(a => a.available);
    const appsCard = h("div", { class: "card stack" },
      h("div", { class: "head" }, h("h2", {}, "App updates"),
        h("button", { onclick: () => runJob("Checking apps for newer versions", () => api("POST", "/maintenance/apps/check")).then(done) }, "Check now")),
      !m.appsChecked
        ? h("p", { class: "muted" }, "Not checked yet. Check now asks each app's registry whether its version is still the newest (nothing is downloaded).")
        : h("p", {}, waiting.length
          ? [h("span", { class: "pill internet" }, waiting.length + " app" + (waiting.length > 1 ? "s" : "") + " with a newer version"), " "]
          : [h("span", { class: "pill done" }, "All up to date"), " "],
          h("span", { class: "muted small" }, "checked " + at(m.appsChecked))),
      list.length ? h("div", { class: "list" }, h("table", {}, h("thead", {}, h("tr", {}, ["App", "Version", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, list.map(a => h("tr", {},
          h("td", {}, h("b", {}, a.name), h("div", { class: "muted small" }, a.images.map(i => i.image).join(", "))),
          h("td", {}, a.error ? h("span", { class: "muted small" }, a.error)
            : a.available ? h("span", { class: "pill internet" }, "newer version")
            : a.images.some(i => i.error) ? h("span", { class: "muted small" }, a.images.filter(i => i.error).map(i => i.error).join("; "))
            : h("span", { class: "pill done" }, "newest")),
          h("td", { class: "actions" }, a.available ? h("button", { onclick: () => runJob("Updating " + a.name, () => api("POST", "/maintenance/apps/update", { ids: [a.id] })).then(done) }, "Update") : null)))))) : null,
      h("div", { class: "row" },
        waiting.length > 1 ? h("button", { class: "primary", onclick: () => {
          if (!confirm("Update " + waiting.map(a => a.name).join(", ") + "? Each restarts for a moment, one after another.")) return;
          runJob("Updating apps", () => api("POST", "/maintenance/apps/update", { ids: waiting.map(a => a.id) })).then(done);
        } }, "Update all") : null,
        h("button", { onclick: () => {
          if (!confirm("Remove app versions nothing uses any more? Running apps aren't touched.")) return;
          runJob("Freeing disk space", () => api("POST", "/maintenance/prune"));
        } }, "Free disk space")),
      h("p", { class: "muted small" }, "Free disk space removes the old versions updates leave behind."));

    return [h("div", { class: "head" }, h("h1", {}, "Maintenance")), updateCard(), sysCard, appsCard];

    async function restart() {
      if (!confirm("Restart the server now? Every app and this page are away for a few minutes.")) return;
      try { await api("POST", "/maintenance/reboot"); } catch (err) { toast(err.message); return; }
      const status = h("p", {}, "Restarting… this page reloads once the server is back.");
      openDialog(h("div", { class: "stack" }, h("h2", {}, "Restarting the server"), status));
      const end = Date.now() + 15 * 60 * 1000;
      let wentAway = false;
      const poll = async () => {
        try { await api("GET", "/overview"); if (wentAway) { location.reload(); return; } } catch { wentAway = true; }
        if (Date.now() < end) setTimeout(poll, 3000); else status.textContent = "It's taking long: check the server.";
      };
      setTimeout(poll, 5000);
    }
  },

  // File sharing: folders shared on the home network (SMB), the accounts that sign in, and who may read or write.
  async shares() {
    const x = await api("GET", "/shares");
    const st = x.status;
    const head = h("div", { class: "head" }, h("h1", {}, "File sharing"));
    if (!st.supported) return [head, h("div", { class: "card" }, h("p", { class: "muted" }, st.why))];
    if (!st.installed) return [head, h("div", { class: "card stack" },
      h("p", {}, "Share folders on the home network, for Finder on a Mac, File Explorer on Windows and phones' Files apps (SMB)."),
      h("div", {}, h("button", { class: "primary", onclick: () => runJob("Installing file sharing", () => api("POST", "/shares/install")) }, "Install file sharing")))];

    const accounts = st.accounts;
    const svc = h("div", { class: "card stack" },
      h("div", { class: "head" }, h("h2", {}, "File sharing is ", st.running ? "on" : "off"),
        h("button", { class: st.running ? "" : "primary", onclick: async () => {
          if (st.running && !confirm("Turn file sharing off? Everyone connected loses their shares until it's back on.")) return;
          try { await api("POST", "/shares/service", { on: !st.running }); route(); } catch (err) { toast(err.message); }
        } }, st.running ? "Turn off" : "Turn on")),
      x.addresses.length ? h("div", { class: "small stack" },
        h("p", {}, h("b", {}, "Mac: "), "Finder → Go → Connect to Server → ", h("code", {}, "smb://" + x.addresses[0])),
        h("p", {}, h("b", {}, "Windows: "), "File Explorer's address bar → ", h("code", {}, "\\\\" + (x.addresses[1] || x.addresses[0]))),
        h("p", {}, h("b", {}, "iPhone: "), "Files → ⋯ → Connect to Server → ", h("code", {}, "smb://" + (x.addresses[1] || x.addresses[0])))) : null);

    const who = sh => {
      const parts = Object.entries(sh.access || {}).sort().map(([n, a]) => n + (a === "write" ? " (read and write)" : " (read)"));
      if (sh.guests) parts.unshift("anyone on the home network (read)");
      return parts.length ? parts.join(", ") : "nobody yet";
    };
    const nobody = sh => !sh.guests && !Object.keys(sh.access || {}).length;
    const sharesCard = h("div", { class: "card stack" },
      h("div", { class: "head" }, h("h2", {}, "Shared folders"), h("button", { class: "primary", onclick: () => shareDialog(null, x, accounts) }, "Share a folder")),
      x.shares.length ? h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Share", "Who", "", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, x.shares.map(sh => h("tr", {},
          h("td", {}, h("b", {}, sh.name), h("div", { class: "muted small" }, sh.path)),
          h("td", { class: "small" }, who(sh)),
          h("td", {}, h("span", { class: "pill " + (sh.off ? "stopped" : nobody(sh) ? "partly" : "running") }, sh.off ? "off" : nobody(sh) ? "no one has access" : "shared")),
          h("td", { class: "actions" },
            h("button", { onclick: () => shareDialog(sh, x, accounts) }, "Who can use it"),
            h("button", { onclick: async () => {
              try { await api("PUT", "/shares/" + encodeURIComponent(sh.name), { ...sh, off: !sh.off }); route(); } catch (err) { toast(err.message); }
            } }, sh.off ? "Turn on" : "Turn off"),
            h("button", { class: "danger", onclick: () => confirmDialog("Stop sharing " + sh.name + "?",
              "The folder stops being shared. Its files stay where they are (" + sh.path + ").", "Stop sharing",
              () => api("DELETE", "/shares/" + encodeURIComponent(sh.name))) }, "Remove"))))))) 
        : h("p", { class: "muted" }, "Nothing shared yet."),
      st.others.length ? h("details", {}, h("summary", { class: "small" }, st.others.length + " shared outside Homeport"),
        h("p", { class: "muted small" }, "Set up by hand in /etc/samba/smb.conf: they keep working, and change there."),
        h("ul", { class: "small" }, st.others.map(o => h("li", {}, h("b", {}, o.name), " ", h("span", { class: "muted" }, o.path || ""))))) : null);

    const accountsCard = h("div", { class: "card stack" },
      h("div", { class: "head" }, h("h2", {}, "Accounts"), h("button", { onclick: () => shareAccountDialog() }, "Add an account")),
      h("p", { class: "muted small" }, "Sign-ins for shared folders, with their own passwords (separate from Homeport's)."),
      accounts.length ? h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Name", "", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, accounts.map(a => h("tr", {},
          h("td", {}, h("b", {}, a.name), h("div", { class: "muted small" }, a.ours ? "for file sharing only" : "also signs in to this server")),
          h("td", {}, h("span", { class: "pill " + (a.disabled ? "stopped" : "running") }, a.disabled ? "off" : "on")),
          h("td", { class: "actions" },
            h("button", { onclick: () => shareAccountDialog(a.name) }, "Password"),
            h("button", { onclick: async () => {
              try { await api("POST", "/shares/accounts/" + encodeURIComponent(a.name) + "/enable", { on: a.disabled }); route(); } catch (err) { toast(err.message); }
            } }, a.disabled ? "Turn on" : "Turn off"),
            h("button", { class: "danger", onclick: () => confirmDialog("Remove " + a.name + "?",
              a.ours ? "It can't open shared folders any more. Files it wrote stay." : "It can't open shared folders any more. Its login to this server stays.",
              "Remove", () => api("DELETE", "/shares/accounts/" + encodeURIComponent(a.name))) }, "Remove"))))))) 
        : h("p", { class: "muted" }, "No accounts yet: add one, then give it a shared folder."));

    return [head, svc, sharesCard, accountsCard];
  },

  async users() {
    const users = await api("GET", "/users");
    return [
      h("div", { class: "head" }, h("h1", {}, "Users"), h("button", { class: "primary", onclick: () => userDialog() }, "Add a user")),
      h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Name", "Groups", "Email", "Added", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, users.map(u => h("tr", {},
          h("td", {}, h("b", {}, u.displayName), h("div", { class: "muted small" }, u.name, u.admin ? " · admin" : "")),
          h("td", {}, u.groups.join(", ") || h("span", { class: "muted" }, "none")),
          h("td", {}, u.email || ""),
          h("td", {}, when(u.created)),
          h("td", { class: "actions" },
            h("button", { onclick: () => userDialog(u) }, "Edit"),
            h("button", { class: "danger", onclick: () => confirmDialog("Delete " + u.name + "?", "They're signed out everywhere and can't sign in again. Their data inside apps stays.", "Delete", async () => { await api("DELETE", "/users/" + u.name); toast("Deleted " + u.name); }) }, "Delete"))))))),
      h("p", { class: "muted small" }, "Groups decide who can open which app. \"All users\" and \"Admins\" are built in."),
    ];
  },

  async domains() {
    const domains = await api("GET", "/domains");
    const input = h("input", { name: "name", placeholder: "cloud.example.com", autocapitalize: "none", spellcheck: "false", required: true });
    // Each served domain's real certificate is looked up after the page shows (it connects to each one).
    const live = {};
    api("GET", "/certificates").then(list => list.forEach(c => live[c.name] && live[c.name].replaceChildren(liveText(c)))).catch(() => {});
    return [
      h("div", { class: "head" }, h("h1", {}, "Domains"), h("div", { class: "row" },
        domains.some(d => d.dyndns && d.dyndns.home) ? moveToHomeButton(domains) : h("button", { class: "primary", onclick: homeNameDialog }, "Set up a home name"),
        h("button", { onclick: freeDomainDialog }, "Get a free domain"))),
      h("form", { class: "card row", onsubmit: async e => { e.preventDefault(); try { await api("POST", "/domains", { name: input.value.trim() }); toast("Added " + input.value.trim()); route(); } catch (err) { toast(err.message); } } },
        h("label", {}, "Add a domain", input), h("button", { class: "primary", type: "submit" }, "Add")),
      h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Domain", "Used for", "Certificate", "Added", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, domains.map(d => h("tr", {},
          h("td", {}, h("b", {}, d.name), d.dyndns && h("div", { class: "muted small" }, freeText(d.dyndns))),
          h("td", {}, d.main ? "Portal and admin panel" : d.app || h("span", { class: "muted" }, "nothing yet")),
          h("td", {}, certNames[d.cert] || d.cert, !d.ownCert && h("span", { class: "muted small" }, " (server setting)"),
            (d.main || d.app) && (live[d.name] = h("div", { class: "small muted" }, "Checking…"))),
          h("td", {}, when(d.added)),
          h("td", { class: "actions" },
            h("button", { onclick: () => certDialog(d) }, "Certificate"),
            !d.main && !d.app && h("button", { class: "danger", onclick: async () => { try { await api("DELETE", "/domains/" + d.name); toast("Removed " + d.name); route(); } catch (err) { toast(err.message); } } }, "Remove"))))))),
      h("p", { class: "muted small" }, "Point each domain's DNS (an A and AAAA record) at this server. Subdomains of a domain listed here are added for you when you install an app on one. Let's Encrypt needs ports 80 and 443 open to this server from the internet (forward them on a home router)."),
    ];
  },

  async assistant() {
    const [list, cfg] = await Promise.all([api("GET", "/chat"), api("GET", "/chat-settings")]);
    return chatView(list, cfg);
  },

  async activity() {
    const [pending, audit, mem] = await Promise.all([api("GET", "/approvals"), api("GET", "/audit?limit=60"), api("GET", "/memory")]);
    const list = pending.length
      ? h("div", { class: "list" }, h("table", {}, h("tbody", {}, pending.map(p => h("tr", {},
          h("td", {}, h("b", {}, p.summary), h("div", { class: "muted small" }, p.client + " · " + p.tool + " · waits until " + new Date(p.expires).toLocaleTimeString())),
          h("td", { class: "actions" },
            h("button", { class: "primary", onclick: () => decide(p, "approve") }, "Approve"),
            h("button", { class: "danger", onclick: () => decide(p, "deny") }, "Decline")))))))
      : h("p", { class: "muted" }, "Nothing is waiting for approval.");
    clearTimeout(window.approvalTimer);
    window.approvalTimer = setTimeout(() => { if (location.hash === "#activity" && !dialog.open) route(); }, 4000);
    return [
      h("div", { class: "head" }, h("h1", {}, "Approvals and activity"), h("a", { class: "button", href: "#assistant" }, "Back to the chat")),
      h("p", { class: "muted" }, "AI helpers (Homeport's assistant, or agents like opencode and Claude Code connected with ", h("code", {}, "homeport mcp"),
        ") can look around freely, but every change waits for your approval here or with ", h("code", {}, "homeport approve"), "."),
      h("h2", {}, "Waiting for approval"), list,
      h("h2", {}, "Shared memory"),
      h("p", { class: "muted small" }, "What the helpers have done and learned, and work in progress, so the next one carries on. Kept in /var/lib/homeport/assistant-memory.md."),
      h("pre", { class: "log" }, mem.memory),
      h("h2", {}, "Recent activity"),
      audit.length ? h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["When", "Who", "What", "Outcome"].map(t => h("th", {}, t)))),
        h("tbody", {}, audit.map(e => h("tr", {},
          h("td", { class: "small" }, new Date(e.time).toLocaleString()),
          h("td", { class: "small" }, e.client),
          h("td", {}, h("b", {}, e.tool), h("div", { class: "muted small mono" }, argsText(typeof e.args === "string" ? safeJSON(e.args) : e.args))),
          h("td", {}, h("span", { class: "pill " + (e.ok ? "done" : "failed") }, e.decision + (e.by ? " by " + e.by : "")),
            e.error ? h("div", { class: "muted small" }, e.error.slice(0, 160)) : null))))))
        : h("p", { class: "muted" }, "No tool calls yet."),
    ];
    async function decide(p, d) {
      try { await api("POST", "/approvals/" + p.id + "/" + d); toast(d === "approve" ? "Approved: running now" : "Declined"); route(); }
      catch (err) { toast(err.message); route(); }
    }
  },

  async settings() {
    const [o, cfg] = await Promise.all([api("GET", "/overview"), api("GET", "/chat-settings")]);
    const tls = h("select", { name: "tls" },
      h("option", { value: "letsencrypt", selected: o.settings.tls !== "internal" }, "Let's Encrypt (public server)"),
      h("option", { value: "internal", selected: o.settings.tls === "internal" }, "Caddy's own certificates (test or private network)"));
    const email = h("input", { name: "email", type: "email", value: o.settings.acmeEmail || "", placeholder: "you@example.com" });
    const shell = h("input", { type: "checkbox", checked: !!o.settings.assistantShell });
    const daily = h("input", { type: "checkbox", checked: !o.settings.noDailyCheck });
    const minutes = h("input", { type: "number", min: "1", max: "240", value: o.settings.approvalMinutes || 10 });
    return [
      h("div", { class: "head" }, h("h1", {}, "Settings")),
      h("form", { class: "card stack", onsubmit: async e => {
        e.preventDefault();
        try { await api("PUT", "/settings", { tls: tls.value, acmeEmail: email.value.trim() }); toast("Saved. Caddy has the new settings."); } catch (err) { toast(err.message); }
      } },
        h("h2", {}, "Certificates"),
        h("label", {}, "Where certificates come from", tls),
        h("label", {}, "Email for Let's Encrypt notices", email),
        h("div", {}, h("button", { class: "primary", type: "submit" }, "Save"))),
      h("form", { class: "card stack", onsubmit: async e => {
        e.preventDefault();
        try { await api("PUT", "/settings", { assistantShell: shell.checked, approvalMinutes: +minutes.value, dailyCheck: daily.checked }); toast("Saved"); } catch (err) { toast(err.message); }
      } },
        h("h2", {}, "AI helpers"),
        h("label", { class: "check" }, shell, "Let helpers ask to run shell commands as root (each one still needs your approval)"),
        h("label", {}, "Minutes a change waits for approval before it's dropped", minutes),
        h("label", { class: "check" }, daily, "Check the server every day (results on the Overview page, and in the assistants' notebook)"),
        h("div", {}, h("button", { class: "primary", type: "submit" }, "Save"))),
      terminalForm(o.settings),
      chatSettingsForm(cfg),
    ];
  },
};

function signInText(s) {
  return s === "headers" ? "Signs people in with their Homeport account."
    : s === "own" ? "Has its own accounts; its phone apps connect directly, so it's public by default."
    : "Only the groups you choose can open it.";
}

// MARK: Dialogs

// driveField: where a new app's files go, when there's more than one drive to choose from (filled in when the drives
// are known; hidden otherwise).
function driveField() {
  const select = h("select", { name: "drive" });
  const label = h("label", { hidden: true }, "Where its files go", select,
    h("span", { class: "muted small" }, "Its settings, database and files (for Jellyfin, all the media) live on this drive. It can move later on the Storage page."));
  api("GET", "/storage/drives").then(x => {
    if (x.drives.length < 2) return;
    select.replaceChildren(...x.drives.map(d => h("option", { value: d.mount, selected: d.mount === (x.newApps || "") || (!x.newApps && d.system) },
      driveName(d) + " · " + gbText(d.free) + " free")));
    label.hidden = false;
  }).catch(() => {});
  return { el: label, value: () => label.hidden ? "" : select.value };
}

function driveName(d) {
  if (d.system) return "System drive" + (d.model ? " (" + d.model + ")" : "");
  return (d.label || d.model || d.device) + " (" + (d.mount || d.device) + ")";
}

function installDialog(c) {
  const main = me.settings.mainDomain;
  const drive = driveField();
  const domain = h("input", { name: "domain", value: c.id + "." + main, required: true, autocapitalize: "none", spellcheck: "false" });
  const pub = h("input", { type: "checkbox", name: "public", checked: c.signIn === "own" });
  const groups = h("input", { name: "groups", value: "all", placeholder: "all" });
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const access = pub.checked ? { public: true } : { groups: splitGroups(groups.value) };
    dialog.close();
    runJob("Installing " + c.name, () => api("POST", "/apps", { catalog: c.id, domain: domain.value.trim(), access, drive: drive.value() }));
  } },
    h("h2", {}, "Install " + c.name),
    h("label", {}, "Address", domain),
    h("label", { class: "check" }, pub, "Anyone can open it (no Homeport sign-in in front)"),
    h("label", {}, "Or only these groups (comma-separated; \"all\" is every user)", groups),
    drive.el,
    h("p", { class: "muted small" }, signInText(c.signIn), " Its DNS must point at this server."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Install"))));
}

// addressDialog: where an app is. The switch moves it between the home network (app.homeport.local) and the internet
// (app.yourname.duckdns.org); or type any address.
async function addressDialog(a) {
  let bases = { public: "", home: "" };
  try { bases = await api("GET", "/addresses"); } catch (err) { toast(err.message); }
  const label = a.domain.split(".")[0];
  const sw = h("input", { type: "checkbox", checked: a.internet, disabled: !bases.public && !a.internet });
  const custom = h("input", { value: a.domain, autocapitalize: "none", spellcheck: "false", "aria-label": "Address" });
  const move = (title, start) => { dialog.close(); runJob(title, start); };
  openDialog(h("div", { class: "body" },
    h("h2", {}, a.name + "'s address"),
    h("p", {}, "Now: ", h("a", { href: "https://" + a.domain + "/", target: "_blank", rel: "noopener" }, "https://" + a.domain)),
    h("label", { class: "check switch" }, sw, h("span", {}, h("b", {}, "On the internet"),
      h("span", { class: "muted small" }, bases.public
        ? " — at " + label + "." + bases.public + " with a real certificate (when Internet access is set up)"
        : " — get a free internet name first (Internet access page)"))),
    h("p", { class: "muted small" }, "Off: only on your home network, at " + label + "." + (bases.home || "homeport.local") + ". Moving restarts it for a moment; apps that know their own address are told the new one."),
    h("details", {}, h("summary", {}, "Or another address"),
      h("div", { class: "row" }, custom, h("button", { onclick: () => move("Moving " + a.name, () => api("POST", "/apps/" + a.id + "/address", { domain: custom.value.trim() })) }, "Move it there"))),
    h("div", { class: "buttons" },
      h("button", { onclick: () => dialog.close() }, "Cancel"),
      h("button", { class: "primary", onclick: () => {
        if (sw.checked === a.internet) { dialog.close(); return; }
        move((sw.checked ? "Putting " : "Taking ") + a.name + (sw.checked ? " on the internet" : " off the internet"),
          () => api("POST", "/apps/" + a.id + "/internet", { on: sw.checked }));
      } }, "Save"))));
}

function accessDialog(a) {
  const pub = h("input", { type: "checkbox", checked: !!a.access.public });
  const groups = h("input", { value: (a.access.groups || ["all"]).join(", ") });
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    try {
      await api("PUT", "/apps/" + a.id + "/access", pub.checked ? { public: true } : { groups: splitGroups(groups.value) });
      dialog.close(); toast("Saved"); route();
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Who can open " + a.name),
    h("label", { class: "check" }, pub, "Anyone (no Homeport sign-in in front)"),
    h("label", {}, "Or only these groups (comma-separated; \"all\" is every user)", groups),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Save"))));
}

const certNames = { letsencrypt: "Let's Encrypt", internal: "Caddy's own", custom: "Uploaded", dns: "Let's Encrypt, through DNS (it and every name under it)" };

// What a browser gets from a domain right now.
function liveText(c) {
  if (c.live.error) return h("span", { class: "pill unknown" }, c.dns.length ? c.live.error : "No DNS for this name yet");
  const days = Math.floor((new Date(c.live.notAfter) - Date.now()) / 86400000);
  return [h("span", { class: "pill " + (c.live.trusted ? (days < 14 ? "partly" : "done") : "partly") },
    c.live.trusted ? (days < 0 ? "Expired" : "Valid") : "Not trusted by browsers"),
    " ", c.live.issuer + ", until " + when(c.live.notAfter)];
}

function freeText(dd) {
  const who = dd.provider === "desec" ? "deSEC" : "DuckDNS";
  if (dd.home) return "Home name from deSEC" + (dd.error ? ": " + dd.error : dd.ipv4 ? ", pointed at this server (" + dd.ipv4 + ")" : "") +
    (dd.certError ? " · certificate: " + dd.certError : dd.certUntil && !dd.certUntil.startsWith("0001") ? " · certificate until " + when(dd.certUntil) : "");
  if (dd.error) return "Free from " + who + ": " + dd.error;
  return "Free from " + who + (dd.ipv4 || dd.ipv6 ? ", pointed at " + [dd.ipv4, dd.ipv6].filter(Boolean).join(" and ") : "");
}

function certDialog(d) {
  const opt = (value, label) => h("option", { value, selected: (d.ownCert ? d.cert : "") === value }, label);
  const mode = h("select", {}, opt("", "The server's setting (Settings)"), opt("letsencrypt", "Let's Encrypt (free, renewed for you)"),
    opt("internal", "Caddy's own (private network; browsers warn)"), opt("custom", "Upload my own"));
  const cert = h("textarea", { rows: "5", placeholder: "-----BEGIN CERTIFICATE-----", spellcheck: "false" });
  const key = h("textarea", { rows: "5", placeholder: "-----BEGIN PRIVATE KEY-----", spellcheck: "false" });
  const upload = h("div", { class: "stack" },
    h("label", {}, "Certificate with its chain (fullchain.pem)", cert),
    h("label", {}, "Private key (key.pem)", key),
    h("p", { class: "muted small" }, "It's checked first: it must be for " + d.name + " and in date. You renew it yourself."));
  const show = () => (upload.hidden = mode.value !== "custom");
  mode.addEventListener("change", show);
  show();
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    try {
      await api("PUT", "/domains/" + d.name + "/certificate", mode.value === "custom" ? { mode: "custom", cert: cert.value, key: key.value } : { mode: mode.value });
      dialog.close(); toast("Saved. A new Let's Encrypt certificate can take a minute."); route();
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Certificate for " + d.name),
    h("label", {}, "Where it comes from", mode),
    upload,
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Save"))));
}

// homeNameDialog sets up a home name: a free deSEC name pointed at this server's home address, with real certificates
// for it and every name under it, so every device at home finds the server and trusts it.
function homeNameDialog() {
  const provider = h("select", {}, h("option", { value: "cloudflare" }, "Cloudflare: a domain of yours there"),
    h("option", { value: "desec" }, "deSEC: a free name (myhome.dedyn.io) or a domain of yours there"));
  const name = h("input", { required: true, placeholder: "home.example.com", autocapitalize: "none", spellcheck: "false" });
  const token = h("input", { type: "password", required: true, autocomplete: "off" });
  const main = h("input", { type: "checkbox", checked: true });
  const suffix = h("span", { class: "muted", hidden: true }, ".dedyn.io");
  const steps = h("ol", { class: "small" });
  const tokenLabel = h("span", {});
  const full = () => {
    const v = name.value.trim().toLowerCase().replace(/\.$/, "");
    if (provider.value === "desec" && (!v || !v.includes("."))) return (v || "myhome") + ".dedyn.io";
    return v || "home.example.com";
  };
  const link = (href, text) => h("a", { href, target: "_blank", rel: "noopener" }, text);
  const show = () => {
    const cf = provider.value === "cloudflare";
    name.placeholder = cf ? "home.example.com" : "myhome, or home.example.com";
    suffix.hidden = cf || name.value.includes(".");
    tokenLabel.textContent = cf ? "Cloudflare API token" : "deSEC token";
    steps.replaceChildren(...(cf ? [
      h("li", {}, "In ", link("https://dash.cloudflare.com", "Cloudflare"), ", your domain must be added and active (its nameservers set to Cloudflare's at the registrar)."),
      h("li", {}, "My Profile → API Tokens → Create Token → use the ", h("b", {}, "Edit zone DNS"), " template; under Zone Resources choose your domain; create it and copy the token."),
      h("li", {}, "Type a name in the domain, like ", h("b", {}, "home.example.com"), " (apps become photos.home.example.com), or the domain itself. Homeport only changes that name's records; the rest of the domain (website, mail) is left alone."),
    ] : [
      h("li", {}, "Open ", link("https://desec.io", "desec.io"), ", sign up (email and password) and confirm your email."),
      h("li", {}, "Under Token Management, add a token and copy it."),
      h("li", {}, "Type a free name (myhome, for myhome.dedyn.io), or a name in a domain of yours on deSEC (its nameservers set to ns1.desec.io and ns2.desec.org)."),
    ]));
  };
  provider.addEventListener("change", show);
  name.addEventListener("input", show);
  show();
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const body = { provider: provider.value, name: name.value.trim(), token: token.value, main: main.checked };
    dialog.close();
    const j = await runJob("Setting up " + full(), () => api("POST", "/domains/home", body));
    if (j && j.status === "done") toast("Home name ready");
  } },
    h("h2", {}, "Set up a home name"),
    h("p", {}, "A name that points at this server's address on your home network. Every device at home finds it (phones, Android, other subnets, the server itself), and Homeport gets a real certificate for it and every app under it: no more warnings, and signing in works everywhere. Nothing becomes reachable from the internet; you can still put apps there one by one."),
    h("label", {}, "DNS service", provider),
    steps,
    h("label", {}, "Name", h("span", { class: "suffixed" }, name, suffix)),
    h("label", {}, tokenLabel, token),
    h("label", { class: "check" }, main, "Make it the main address (portal and admin panel)"),
    h("p", { class: "muted small" }, "Getting the certificate takes a minute or two. Apps you already have keep their addresses: move them to the new name afterwards (Domains → Move apps to the home name)."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Set it up"))));
}

// moveToHomeButton moves every app not yet under the home name to the same first name under it
// (librespeed.homeport.local → librespeed.myhome.dedyn.io), one after another.
function moveToHomeButton(domains) {
  const home = domains.find(d => d.dyndns && d.dyndns.home).name;
  return h("button", { class: "primary", onclick: async () => {
    const list = (await api("GET", "/apps")).filter(a => a.domain !== home && !a.domain.endsWith("." + home));
    // The same first name under the home name, unless that one's taken (ai and signin are Homeport's own): the app's id.
    const taken = new Set(["ai", "signin", ...(await api("GET", "/apps")).filter(a => a.domain.endsWith("." + home)).map(a => a.domain.slice(0, -home.length - 1))]);
    const label = a => { const l = a.domain.split(".")[0]; return taken.has(l) ? a.id : l; };
    if (!list.length) { toast("Every app is under " + home + " already"); return; }
    if (!confirm("Move " + list.map(a => a.name).join(", ") + " to names under " + home + "? Each restarts briefly; old bookmarks stop working.")) return;
    for (const a of list) {
      const name = label(a) + "." + home;
      taken.add(label(a));
      const j = await runJob("Moving " + a.name + " to " + name, () => api("POST", "/apps/" + a.id + "/address", { domain: name }));
      if (!j || j.status !== "done") return;
    }
    toast("All apps are under " + home);
  } }, "Move apps to the home name");
}

function freeDomainDialog() {
  const provider = h("select", {}, h("option", { value: "desec" }, "deSEC: name.dedyn.io"), h("option", { value: "duckdns" }, "DuckDNS: name.duckdns.org"));
  const name = h("input", { required: true, placeholder: "myhome", autocapitalize: "none", spellcheck: "false" });
  const suffix = h("span", { class: "muted" });
  const token = h("input", { type: "password", required: true, autocomplete: "off" });
  const help = h("p", { class: "muted small" });
  const show = () => {
    const desec = provider.value === "desec";
    suffix.textContent = desec ? ".dedyn.io" : ".duckdns.org";
    help.replaceChildren(desec
      ? "Make a free account at desec.io, then under Token management make a token and paste it here. Homeport makes the name in your account."
      : "Sign in at duckdns.org, add the name there, then paste the token shown at the top of the page.",
      " Homeport points the name at this server and keeps it up to date when your address changes. Apps get subdomains of it, like cloud." + (name.value.trim() || "myhome") + suffix.textContent + ".");
  };
  provider.addEventListener("change", show);
  name.addEventListener("input", show);
  show();
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    try {
      const out = await api("POST", "/domains/free", { provider: provider.value, name: name.value.trim(), token: token.value });
      dialog.close(); toast(out.name + " points here now"); route();
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Get a free domain"),
    h("label", {}, "From", provider),
    h("label", {}, "Name", h("span", { class: "suffixed" }, name, suffix)),
    h("label", {}, "Token", token),
    help,
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Get it"))));
}

// notesDialog is everything to look back at for an installed app: its setup notes, the passwords and keys Homeport made
// for it (hidden until shown), and the admin's own notes (passwords from the app's own setup, say).
async function notesDialog(a) {
  let n;
  try { n = await api("GET", "/apps/" + a.id + "/notes"); } catch (err) { toast(err.message); return; }
  const secretRow = ([k, v]) => {
    const value = h("span", { class: "mono secret" }, "••••••••••");
    const show = h("button", { type: "button", onclick: () => {
      const hidden = show.textContent === "Show";
      value.textContent = hidden ? v : "••••••••••"; show.textContent = hidden ? "Hide" : "Show";
    } }, "Show");
    const copy = h("button", { type: "button", onclick: async () => {
      try { await navigator.clipboard.writeText(v); toast("Copied " + k); } catch { toast("Couldn't copy: select it after Show instead"); }
    } }, "Copy");
    return h("tr", {}, h("td", {}, k.replace(/_/g, " ")), h("td", {}, value), h("td", { class: "actions" }, show, copy));
  };
  const secrets = Object.entries(n.secrets || {}).sort(([x], [y]) => x.localeCompare(y));
  const mine = h("textarea", { rows: "6", placeholder: "Passwords you set during its own setup, accounts you made, anything to remember…", "aria-label": "Your notes" });
  mine.value = n.notes || "";
  const saved = h("span", { class: "muted small" });
  mine.addEventListener("input", () => { saved.textContent = "Not saved yet"; });
  openDialog(h("form", { method: "dialog", class: "wide", onsubmit: async e => {
    e.preventDefault();
    try { await api("PUT", "/apps/" + a.id + "/notes", { notes: mine.value }); saved.textContent = "Saved"; toast("Notes saved"); } catch (err) { toast(err.message); }
  } },
    h("h2", {}, a.name + " notes"),
    h("p", { class: "muted small" }, h("a", { href: "https://" + n.domain + "/", target: "_blank", rel: "noopener" }, "https://" + n.domain), " · data in ", h("code", {}, n.data)),
    n.setup ? h("div", { class: "stack" }, h("h3", {}, "Setting it up"), h("div", { class: "card notes" }, rich(n.setup))) : null,
    secrets.length ? h("div", { class: "stack" },
      h("h3", {}, "Passwords and keys Homeport made for it"),
      h("div", { class: "list" }, h("table", {}, h("tbody", {}, secrets.map(secretRow))))) : null,
    h("label", {}, h("h3", {}, "Your notes"), mine),
    h("p", { class: "muted small" }, "Kept on this server only, like the passwords above (never shown to AI helpers). Keep them private."),
    h("div", { class: "buttons" }, saved, h("button", { type: "button", onclick: () => dialog.close() }, "Close"), h("button", { class: "primary", type: "submit" }, "Save notes"))));
}

// appRunButtons: Start when an app (or part of it) isn't running, Stop and Restart when it is.
function appRunButtons(a) {
  const status = a.status || "";
  const btns = [];
  if (["installing", "updating", "moving", "removing"].includes(status)) return btns; // a job is on it
  if (!status.startsWith("running")) btns.push(h("button", { class: "primary", onclick: () => appAction(a, "start") }, "Start"));
  if (status.startsWith("running")) btns.push(h("button", { onclick: () => appAction(a, "restart") }, "Restart"));
  if (status.startsWith("running") || status.startsWith("partly")) btns.push(h("button", { class: "danger", onclick: () => appAction(a, "stop") }, "Stop"));
  return btns;
}

async function appAction(a, action) {
  if (action === "stop" && !confirm("Stop " + a.name + "? It's unreachable until you start it again.")) return;
  toast({ start: "Starting ", stop: "Stopping ", restart: "Restarting " }[action] + a.name + "…");
  try { await api("POST", "/apps/" + a.id + "/" + action); toast(a.name + ": " + { start: "started", stop: "stopped", restart: "restarted" }[action]); } catch (err) { toast(err.message); }
  route();
}

async function containerAction(a, c, action) {
  if (action === "stop" && !confirm("Stop " + a.name + "'s " + c.service + "? " + a.name + " may stop working until it's started again.")) return;
  try { await api("POST", "/services/apps/" + a.id + "/" + c.service + "/" + action); toast(a.name + " · " + c.service + ": " + action + " done"); } catch (err) { toast(err.message); }
  route();
}

async function restartSystem(s) {
  if (!confirm("Restart " + s.name + "? " + s.restart)) return;
  try { await api("POST", "/services/system/" + s.id + "/restart"); toast("Restarting " + s.name); } catch (err) { toast(err.message); }
  setTimeout(route, s.id === "homeport" ? 4000 : 1500);
}

// logsDialog shows a service's recent log lines, newest at the bottom.
function logsDialog(title, path) {
  const log = h("pre", { class: "log tall" }, "Loading…");
  const count = h("select", { "aria-label": "How many lines" }, [200, 1000, 2000].map(n => h("option", { value: n }, n + " lines")));
  const load = async () => {
    try { const r = await api("GET", path + "?lines=" + count.value); log.textContent = r.log || "(nothing logged)"; log.scrollTop = log.scrollHeight; }
    catch (err) { log.textContent = err.message; }
  };
  count.addEventListener("change", load);
  openDialog(h("div", { class: "body wide-body" },
    h("div", { class: "head" }, h("h2", {}, title), h("div", { class: "row" }, count, h("button", { onclick: load }, "Refresh"))),
    log,
    h("div", { class: "buttons" }, h("button", { onclick: () => dialog.close() }, "Close"))));
  load();
}

// systemCard: what this machine is, and how busy it is now.
function systemCard() {
  const body = h("p", { class: "muted" }, "Reading this machine…");
  const card = h("div", { class: "card stack" }, h("h2", {}, "System"), body);
  api("GET", "/system").then(({ specs: s, load: l, addresses }) => {
    const gb = n => (n / 1073741824).toFixed(n >= 100 * 1073741824 ? 0 : 1) + " GB";
    const days = d => { const m = Math.round(d / 6e10); return m < 60 ? m + " minutes" : m < 2880 ? Math.round(m / 60) + " hours" : Math.round(m / 1440) + " days"; };
    const row = (k, v, extra) => v ? h("tr", {}, h("th", { scope: "row" }, k), h("td", {}, v, extra ? h("span", { class: "muted small" }, " " + extra) : null)) : null;
    const usedDisk = s.disk.total - s.disk.free;
    body.replaceWith(h("div", { class: "list" }, h("table", { class: "specs" }, h("tbody", {},
      row("Operating system", s.os, s.arch),
      row("Kernel", s.kernel),
      row("Computer name", s.hostname, addresses.length ? "· " + addresses.join(", ") : ""),
      row("Processor", s.cpu || s.cores + " cores", s.cpu ? "· " + s.cores + " threads · " + l.cpu + "% busy" : l.cpu + "% busy"),
      row("Memory", gb(s.memory), "· " + gb(l.memUsed) + " in use"),
      row("Disk", gb(s.disk.total), "· " + gb(s.disk.free) + " free (" + Math.round(usedDisk * 100 / s.disk.total) + "% used)"),
      row("Graphics", s.gpu, l.gpu ? "· " + l.gpu.percent + "% busy" + (l.gpu.memTotal ? " · " + sizeText(l.gpu.memUsed) + " of " + sizeText(l.gpu.memTotal) + " graphics memory in use" : "") : ""),
      row("Up for", days(s.uptime)),
      row("Docker", s.docker),
      row("Caddy", s.caddy),
      row("Homeport", me.version)))));
  }).catch(err => body.replaceWith(h("p", { class: "error" }, err.message)));
  return card;
}

function sizeText(n) {
  if (!n) return "0 bytes";
  if (n >= 2 ** 40) return (n / 2 ** 40).toFixed(1) + " TB";
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + " kB";
  return n + " bytes";
}

// MARK: Storage

// followMove shows a move's progress until it ends, then shows the switchboard again.
async function followMove(id, box) {
  let j;
  try { j = await api("GET", "/jobs/" + id); } catch {
    try { sessionStorage.removeItem("homeport-move-job"); } catch { /* private window */ }
    box.replaceChildren(); return;
  }
  const p = j.result || {};
  const pct = p.total ? Math.min(100, Math.floor(p.done * 100 / p.total)) : 0;
  const lastLine = (j.log.trim().split("\n").pop() || "");
  box.replaceChildren(h("div", { class: "card stack" },
    h("div", { class: "head" }, h("h2", {}, j.status === "running" ? "Moving" : j.status === "done" ? "Moved" : "Some things weren't moved"),
      j.status !== "running" ? h("button", { onclick: () => { try { sessionStorage.removeItem("homeport-move-job"); } catch { /* */ } route(); } }, "Done") : null),
    j.status === "running" && p.name ? [h("p", {}, h("b", {}, p.name), " (" + p.step + " of " + p.of + ")" + (p.total ? ": " + gbText(p.done) + " of " + gbText(p.total) + " copied" : "")),
      h("span", { class: "meter-bar long" }, h("span", { class: "meter-fill w" + Math.floor(pct / 5) * 5 })),
      h("p", { class: "muted small" }, lastLine)] : null,
    j.error ? h("p", { class: "error" }, j.error) : null,
    h("details", {}, h("summary", { class: "muted small" }, "Details"), h("pre", { class: "log" }, j.log))));
  if (j.status === "running" && location.hash === "#storage") setTimeout(() => followMove(id, box), 1500);
}

// MARK: Updating Homeport

// newer says whether version a is newer than b (v0.7.11 > v0.7.11-rc8 > v0.7.10).
function newer(a, b) {
  const parse = v => { const [main, pre] = String(v || "").replace(/^v/, "").split("-"); return { n: main.split(".").map(Number), pre: pre || "" }; };
  const x = parse(a), y = parse(b);
  for (let i = 0; i < 3; i++) if ((x.n[i] || 0) !== (y.n[i] || 0)) return (x.n[i] || 0) > (y.n[i] || 0);
  if (!x.pre !== !y.pre) return !x.pre; // a release is newer than its test builds
  return x.pre.localeCompare(y.pre, undefined, { numeric: true }) > 0;
}

// reachSpot fills in "How to reach this server" (the same card as on the portal, open here).
function reachSpot() {
  const spot = h("div", {});
  fetch("/api/me/reach", { credentials: "same-origin" }).then(r => r.ok ? r.json() : Promise.reject())
    .then(x => spot.replaceChildren(reachCard(x, true))).catch(() => spot.remove());
  return spot;
}

// updateCard: the version, newer releases on GitHub, and updating with one click.
function updateCard() {
  const body = h("div", { class: "stack" }, h("p", { class: "muted" }, "Checking for updates…"));
  const card = h("div", { class: "card stack" }, h("h2", {}, "Homeport"), body);
  const tests = h("input", { type: "checkbox" });
  try { tests.checked = localStorage.getItem("homeport-test-builds") === "1"; } catch { /* private window */ }
  let info;
  const again = h("button", { type: "button", class: "w-change", onclick: () => check(true) }, "Check again");
  const draw = () => {
    const pick = [info.latest, tests.checked ? info.test : null].filter(Boolean).filter(r => newer(r.tag, info.current))
      .sort((a, b) => newer(a.tag, b.tag) ? -1 : 1)[0];
    body.replaceChildren(...[
      h("p", {}, "This server runs ", h("b", {}, info.current), "."),
      info.error ? h("p", { class: "muted small" }, "Couldn't check for updates: " + info.error) : null,
      pick ? [h("p", {}, h("span", { class: "pill internet" }, pick.prerelease ? "test build" : "update"), " ", h("b", {}, pick.tag), " is out (" + when(pick.published) + ")."),
        pick.notes ? h("details", {}, h("summary", { class: "small" }, "What's new"), h("div", { class: "stack small guide" }, ...doc(pick.notes))) : null,
        info.canUpdate
          ? h("div", { class: "row" }, h("button", { class: "primary", onclick: () => update(pick.tag) }, "Update to " + pick.tag))
          : [h("p", { class: "muted small" }, (info.why ? info.why + ". " : "") + "Update with this command on the server:"), h("pre", { class: "log" }, info.command)]]
        : info.error ? null : h("p", { class: "muted small" }, "It's up to date."),
      info.test && newer(info.test.tag, info.current) ? h("label", { class: "check small" }, tests, "Offer test builds too (" + info.test.tag + ")") : null,
      h("div", {}, again),
    ].flat().filter(x => x != null && x !== false));
  };
  tests.addEventListener("change", () => {
    try { localStorage.setItem("homeport-test-builds", tests.checked ? "1" : ""); } catch { /* private window */ }
    draw();
  });
  const check = fresh => {
    again.disabled = true; again.textContent = "Checking…";
    return api("GET", "/update" + (fresh ? "?fresh=1" : "")).then(x => { info = x; draw(); })
      .catch(err => body.replaceChildren(h("p", { class: "error" }, err.message)))
      .finally(() => { again.disabled = false; again.textContent = "Check again"; });
  };
  check(false);

  async function update(tag) {
    if (!confirm("Update Homeport to " + tag + "? It restarts for a moment; apps keep running.")) return;
    const log = h("pre", { class: "log" }, "Starting the installer…");
    const status = h("p", {}, "Updating to " + tag + "…");
    openDialog(h("div", { class: "stack" }, h("h2", {}, "Updating Homeport"), status, log));
    try { await api("POST", "/update", { version: tag }); } catch (err) { status.textContent = err.message; status.className = "error"; return; }
    const from = info.current, end = Date.now() + 20 * 60 * 1000;
    const poll = async () => {
      try {
        const x = await api("GET", "/update/log");
        log.textContent = x.log || "…";
        log.scrollTop = log.scrollHeight;
        if (x.version !== from) {
          status.textContent = "Done: Homeport " + x.version + " is running. Reloading…";
          setTimeout(() => location.reload(), 2500);
          return;
        }
      } catch { status.textContent = "Homeport is restarting…"; }
      if (Date.now() < end) setTimeout(poll, 2000);
      else status.textContent = "It's taking long: look at /var/log/homeport-install.log on the server.";
    };
    setTimeout(poll, 2000);
  }
  return card;
}

// MARK: Check and fix

const ASK = "homeport-ask"; // a question to put in the assistant's box (from "Ask the assistant")

// checksCard shows the last round of checks, with a repair for what Homeport can fix and the guide for the rest.
function checksCard() {
  const body = h("div", { class: "stack" }, h("p", { class: "muted" }, "Loading…"));
  const runBtn = h("button", { class: "primary", onclick: () => draw(api("POST", "/checks"), true) }, "Check now");
  const card = h("div", { class: "card stack checks-card" }, h("div", { class: "head" }, h("h2", {}, "Check and fix"), runBtn), body);
  function draw(p, running) {
    if (running) { runBtn.disabled = true; body.replaceChildren(h("p", { class: "muted" }, "Checking services, the main address, disk, apps, internet access, the local AI and backups… (up to a minute)")); }
    p.then(x => {
      runBtn.disabled = false;
      const run = x.run;
      if (!run) { body.replaceChildren(h("p", { class: "muted" }, "Not checked yet" + (x.daily ? " (the daily check runs a few minutes after Homeport starts)" : "") + ".")); return; }
      const bad = run.checks.filter(c => c.state !== "ok"), ok = run.checks.filter(c => c.state === "ok");
      body.replaceChildren(
        h("p", {}, h("span", { class: "pill " + (run.problems ? "failed" : run.warnings ? "partly" : "done") },
          run.problems ? run.problems + " problem" + (run.problems > 1 ? "s" : "") : run.warnings ? run.warnings + " to look at" : "All fine"),
          " ", h("span", { class: "muted small" }, "Checked " + new Date(run.at).toLocaleString() + (x.daily ? " · every day" : " · daily check off (Settings)"))),
        bad.length ? h("ul", { class: "checks" }, bad.map(c => h("li", {},
          h("span", { class: "tick " + (c.state === "bad" ? "bad" : "wait"), "aria-hidden": "true" }, c.state === "bad" ? "✗" : "!"),
          h("div", { class: "stack" }, h("div", {}, h("b", {}, c.title), h("div", { class: "muted small" }, c.detail)),
            h("div", { class: "row" },
              c.fix ? h("button", { class: "primary", onclick: e => fix(c, e.target) }, c.fixLabel || "Fix it") : null,
              c.guide ? h("button", { onclick: () => guideDialog(c.guide) }, "How to fix") : null,
              h("button", { onclick: () => askAssistant("The server check found: " + c.title + ". " + c.detail + " Can you look into it and fix it?") }, "Ask the assistant")))))) : null,
        ok.length ? h("details", {}, h("summary", { class: "muted small" }, ok.length + " fine"),
          h("ul", { class: "checks" }, ok.map(c => h("li", {}, h("span", { class: "tick ok", "aria-hidden": "true" }, "✓"), h("div", {}, h("b", {}, c.title), h("div", { class: "muted small" }, c.detail)))))) : null);
    }).catch(err => { runBtn.disabled = false; body.replaceChildren(h("p", { class: "error" }, err.message)); });
  }
  async function fix(c, btn) {
    if (!confirm(c.fixLabel + "?")) return;
    btn.disabled = true; btn.textContent = "Working…";
    try { await api("POST", "/checks/fix", { fix: c.fix }); toast("Done. Checking again…"); draw(api("POST", "/checks"), true); }
    catch (err) { toast(err.message); btn.disabled = false; btn.textContent = c.fixLabel; }
  }
  draw(api("GET", "/checks"));
  return card;
}

async function guideDialog(id) {
  try {
    const [e] = await api("GET", "/guide?id=" + encodeURIComponent(id));
    if (!e) return;
    openDialog(h("form", { method: "dialog", class: "stack guide" }, ...doc(e.text.replace(/^Signs:.*$/m, "").replace(/^Applies:.*$/m, "")),
      h("div", { class: "row" }, h("button", { class: "primary" }, "Close"))));
  } catch (err) { toast(err.message); }
}

async function setupDialog() {
  openDialog(h("div", { class: "stack" }, h("p", { class: "muted" }, "Reading this server…")));
  try {
    const { text } = await api("GET", "/setup");
    openDialog(h("form", { method: "dialog", class: "stack guide" },
      h("p", { class: "muted small" }, "Homeport writes this from the machine itself, so assistants (and anyone fixing this server later) know how it's set up. It's also saved as server-setup.md in Homeport's folder."),
      ...doc(text),
      h("div", { class: "row" }, h("button", { class: "primary" }, "Close"))));
  } catch (err) { dialog.close(); toast(err.message); }
}

// doc shows a Markdown page with headings and lists (the guide, the setup page).
function doc(text) {
  const inline = t => t.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/).map(bit =>
    bit.startsWith("`") && bit.endsWith("`") && bit.length > 2 ? h("code", {}, bit.slice(1, -1))
    : bit.startsWith("**") && bit.endsWith("**") && bit.length > 4 ? h("b", {}, bit.slice(2, -2)) : bit);
  const out = [];
  let list = null, para = [];
  const flush = () => { if (para.length) out.push(h("p", {}, inline(para.join(" ")))); para = []; };
  for (const line of text.split("\n")) {
    const head = /^(#{1,3})\s+(.*)$/.exec(line), item = /^(\s*)(?:-|\d+\.)\s+(.*)$/.exec(line);
    if (head) { flush(); list = null; out.push(h(head[1].length === 1 ? "h2" : "h3", {}, head[2])); continue; }
    if (item) {
      flush();
      if (!list) { list = h(/^\s*\d/.test(line) ? "ol" : "ul", { class: "doc-list" }); out.push(list); }
      list.append(h("li", { class: item[1].length ? "sub" : null }, inline(item[2])));
      continue;
    }
    if (!line.trim()) { flush(); list = null; continue; }
    if (list && /^\s+\S/.test(line)) { list.lastChild.append(" ", ...inline(line.trim())); continue; }
    list = null; para.push(line.trim());
  }
  flush();
  return out;
}

function askAssistant(text) {
  try { sessionStorage.setItem(ASK, text); } catch { /* private window */ }
  location.hash = "#assistant";
}

// MARK: AI models

const modelState = { installed: new Set(), pulling: new Set(), hardware: null };

// modelsTop is the part of the AI models page that follows downloads: this server, downloads, installed models.
function modelsTop(st) {
  modelState.installed = new Set((st.installed || []).map(m => m.model));
  const wasPulling = modelState.pulling;
  modelState.pulling = new Set((st.pulls || []).filter(p => !p.finished).map(p => p.model));
  modelState.hardware = st.hardware;
  clearTimeout(window.modelsTimer);
  if (modelState.pulling.size) window.modelsTimer = setTimeout(() => { if (location.hash === "#models") refreshModels(); }, 1000);
  if ([...wasPulling].some(m => !modelState.pulling.has(m)) && modelState.redraw) modelState.redraw();
  const cur = (st.installed || []).find(m => m.current);
  return [
    h("div", { class: "card stack" },
      h("h2", {}, "This server"),
      st.hardware ? h("p", {}, hardwareText(st.hardware)) : h("p", {}, "The models run on ", h("b", {}, st.where), ", so Homeport can't tell how fast they'd be there."),
      h("p", {}, "The assistant and the AI page use ", cur ? [h("b", {}, cur.name), " (", h("code", {}, cur.model), ")"] : h("b", {}, "no model yet"),
        cur && cur.words ? ", which wrote about " + cur.words.toFixed(1) + " words a second in its last answer." : "."),
      h("p", { class: "muted small" }, "Speeds are for writing the answer. Before that a model reads the conversation, which on a processor can take a minute or more.")),
    (st.pulls || []).length ? h("div", { class: "card stack" }, h("h2", {}, "Downloads"), ...st.pulls.map(pullRow)) : null,
    h("h2", {}, "On this server"),
    st.installed.length ? h("div", { class: "list" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Model", "Size", "Here", ""].map(t => h("th", {}, t)))),
      h("tbody", {}, st.installed.map(m => h("tr", {},
        h("td", {}, h("b", {}, m.name), h("div", { class: "muted small" }, m.model, m.params ? " · " + m.params : "", m.quant ? " · " + m.quant : "")),
        h("td", {}, gbText(m.size)),
        h("td", {}, speedPill(m.estimate, m.words)),
        h("td", { class: "actions" }, m.current ? h("span", { class: "pill running" }, "In use")
          : [h("button", { onclick: () => useModel(m) }, "Use"), " ", h("button", { class: "danger", onclick: () => removeModel(m) }, "Remove")]))))))
      : h("p", { class: "muted" }, "No models yet: find one below."),
  ];
}

async function refreshModels() {
  const holder = document.getElementById("models-top");
  if (!holder) return;
  try { holder.replaceChildren(...modelsTop(await api("GET", "/ai-models"))); } catch (err) { toast(err.message); }
}

function hardwareText(x) {
  const hw = x.hardware, bits = [];
  bits.push((hw.cpu || "Processor") + " (" + hw.cores + " threads" + (hw.apple ? "" : hw.avx2 ? "" : ", an older kind without AVX2, so models run slower") + ")");
  bits.push(gbText(hw.memory, true) + " of memory" + (hw.apple ? ", shared with the graphics" : ""));
  const gpu = (/\[([^\]]+)\]\s*$/.exec(hw.gpu || "") || [])[1] || hw.gpu; // "…Tahiti LE [Radeon HD 7870 XT]"
  if (hw.gpu) bits.push(gpu + " with " + gbText(hw.gpuMemory, true) + (x.gpuWorks ? ": Ollama runs models on it" : ": Ollama doesn't use it (models run on the processor)"));
  else if (!hw.apple) bits.push("no graphics card for models (they run on the processor)");
  return bits.join(" · ") + ".";
}

// gbText is a size as Ollama's library gives it (1 GB = 10⁹ bytes); memory (binary) rounds to whole GB.
function gbText(n, memory) {
  if (memory) return Math.round(n / (1 << 30)) + " GB";
  return n >= 1e9 ? (n / 1e9).toFixed(1) + " GB" : Math.round(n / 1e6) + " MB";
}

function speedPill(e, measured) {
  if (!e || !e.label) return h("span", { class: "muted small" }, measured ? "about " + measured.toFixed(1) + " words/s" : "");
  const cls = { Fast: "running", Good: "running", Usable: "partly", Slow: "stopped", "Too big": "stopped" }[e.label] || "";
  const words = measured || e.words;
  return h("span", {}, h("span", { class: "pill " + cls }, e.label), e.place ? h("div", { class: "muted small" },
    "about " + (words < 10 ? words.toFixed(1) : Math.round(words)) + " words/s" + (measured || e.measured ? "" : " (a guess)") + " · " + e.place) : null);
}

function pullRow(p) {
  const pct = p.total ? Math.floor(p.done * 100 / p.total) : 0;
  const what = p.error ? p.error : p.status === "success" ? "Done" : p.total ? gbText(p.done) + " of " + gbText(p.total) + " (" + pct + "%)" : p.status;
  return h("div", { class: "pull" },
    h("div", { class: "row" }, h("b", {}, p.model), h("span", { class: p.error ? "error small" : "muted small" }, what),
      !p.finished ? h("button", { onclick: async () => { try { await api("POST", "/ai-models/stop", { model: p.model }); } catch (err) { toast(err.message); } refreshModels(); } }, "Stop")
        : p.status === "success" && !p.error ? h("button", { class: "primary", onclick: () => useModel({ model: p.model, name: p.model }) }, "Use this one") : null),
    h("span", { class: "meter-bar long" }, h("span", { class: "meter-fill w" + (p.status === "success" ? 100 : Math.floor(pct / 5) * 5) })));
}

async function installModel(model) {
  try { await api("POST", "/ai-models/pull", { model }); toast("Downloading " + model); } catch (err) { toast(err.message); return; }
  modelState.pulling.add(model);
  if (modelState.redraw) modelState.redraw();
  refreshModels();
}

async function useModel(m) {
  try { await api("POST", "/ai-models/use", { model: m.model }); toast("The assistant and the AI page use " + m.model + " now (loading it)"); } catch (err) { toast(err.message); }
  refreshModels();
}

async function removeModel(m) {
  if (!confirm("Remove " + m.model + " from this server? It frees " + gbText(m.size) + "; you can download it again later.")) return;
  try { await api("POST", "/ai-models/remove", { model: m.model }); toast("Removed"); } catch (err) { toast(err.message); }
  refreshModels();
}

// findModels searches Ollama's library: each model's sizes, how they'd run here, and the best fit.
function findModels(st) {
  const input = h("input", { type: "search", placeholder: "Search Ollama's library: qwen, llama, gemma, coder… or a name like qwen3.5:4b", "aria-label": "Search models" });
  const box = h("div", { class: "stack" });
  const note = h("p", { class: "muted small" });
  let results = [], query = "";
  const familyCard = f => {
    const tools = f.caps.includes("tools");
    return h("div", { class: "card stack model-family" },
      h("div", { class: "head" }, h("div", {}, h("h3", {}, f.name), h("p", { class: "muted small" }, f.description)),
        h("div", { class: "row" }, ...f.caps.map(c => h("span", { class: "pill" + (c === "tools" ? " internet" : "") }, c)), f.downloads ? h("span", { class: "muted small" }, f.downloads + " downloads") : null)),
      tools ? null : h("p", { class: "muted small" }, "Can't use Homeport's tools: fine on the AI page for chatting, but the assistant can't look after the server with it."),
      f.error ? h("p", { class: "error small" }, f.error) : null,
      f.variants.length ? h("div", { class: "list" }, h("table", {}, h("tbody", {}, f.variants.map(v => h("tr", {},
        h("td", {}, h("b", {}, v.model), v.best ? [" ", h("span", { class: "pill running" }, "Best fit here")] : null,
          h("div", { class: "muted small" }, gbText(v.download) + " download" + (v.context ? " · reads up to " + v.context + " tokens" : "") + (v.active ? " · " + v.active + "B at work per word" : ""))),
        h("td", {}, speedPill(v.estimate)),
        h("td", { class: "actions" }, modelState.installed.has(v.model) ? h("span", { class: "pill done" }, "Installed")
          : modelState.pulling.has(v.model) ? h("span", { class: "pill partly" }, "Downloading")
          : h("button", { class: v.best ? "primary" : "", disabled: v.estimate && v.estimate.label === "Too big",
              title: v.estimate && v.estimate.label === "Too big" ? "Too big for this server's memory" : null,
              onclick: () => installModel(v.model) }, "Install")))))))
        : f.error ? null : h("p", { class: "muted small" }, "No sizes to download (it may only run in Ollama's cloud)."));
  };
  function redraw() {
    const exact = /^[a-z0-9][a-z0-9._-]*(\/[a-z0-9][a-z0-9._-]*)?:[A-Za-z0-9][A-Za-z0-9._-]*$/.test(query) && !results.some(f => f.variants.some(v => v.model === query));
    box.replaceChildren(
      ...(exact ? [h("div", { class: "card row" }, h("span", {}, "Install ", h("code", {}, query), " by its exact name?"),
        modelState.installed.has(query) ? h("span", { class: "pill done" }, "Installed") : h("button", { class: "primary", onclick: () => installModel(query) }, "Install"))] : []),
      ...results.map(familyCard));
  }
  modelState.redraw = redraw;
  async function search() {
    query = input.value.trim();
    note.textContent = query ? "Looking in Ollama's library…" : "Loading the most popular models…";
    box.replaceChildren();
    try {
      results = await api("GET", "/ai-models/search?q=" + encodeURIComponent(query));
      note.textContent = results.length ? (query ? "" : "The most popular models in Ollama's library. ") + (modelState.hardware ? "“Best fit here” is the most capable size that still answers at a usable speed on this server." : "")
        : "Nothing found for “" + query + "”.";
    } catch (err) { results = []; note.textContent = err.message; }
    redraw();
  }
  search();
  return h("section", { class: "stack" },
    h("h2", {}, "Find more models"),
    h("form", { class: "row", onsubmit: e => { e.preventDefault(); search(); } }, input, h("button", { class: "primary", type: "submit" }, "Search")),
    note, box);
}

// destinationDialog adds a place for backups: its fields follow the kind chosen.
function destinationDialog(b) {
  const kind = h("select", { "aria-label": "Kind" },
    h("option", { value: "sftp" }, "An SSH server or NAS (SFTP)"), h("option", { value: "s3" }, "S3-compatible storage (Backblaze B2, Wasabi, AWS, R2…)"),
    h("option", { value: "homeport" }, "Another Homeport (its Backup space app)"), h("option", { value: "local" }, "A drive plugged into this server"));
  const name = h("input", { required: true, placeholder: "My NAS", "aria-label": "Name" });
  const f = {};
  const field = (key, label, attrs = {}) => { f[key] = h("input", { "aria-label": label, autocapitalize: "none", spellcheck: "false", ...attrs }); return h("label", {}, label, f[key]); };
  const key = h("pre", { class: "log" }, "Making Homeport's SSH key…");
  const parts = {
    sftp: h("div", { class: "stack" },
      h("div", { class: "row" }, field("host", "Host", { placeholder: "nas.local or 192.168.0.20" }), field("port", "Port", { type: "number", value: "22" }), field("user", "User", { placeholder: "backup" })),
      field("path", "Folder there", { placeholder: "/volume1/backups/homeport" }),
      h("p", { class: "small" }, "On that machine, add this line to ", h("code", {}, "~USER/.ssh/authorized_keys"), " (on a Synology: enable SSH and \"user home\", then the same):"), key),
    s3: h("div", { class: "stack" },
      field("endpoint", "Address", { placeholder: "https://s3.eu-central-003.backblazeb2.com" }),
      h("p", { class: "muted small" }, "Backblaze B2: https://s3.<region>.backblazeb2.com · Wasabi: https://s3.<region>.wasabisys.com · Cloudflare R2: https://<account>.r2.cloudflarestorage.com · AWS: https://s3.<region>.amazonaws.com"),
      h("div", { class: "row" }, field("bucket", "Bucket"), field("region", "Region (if it asks)")),
      h("div", { class: "row" }, field("accessKey", "Access key (key ID)"), field("secretKey", "Secret key", { type: "password", autocomplete: "off" }))),
    homeport: h("div", { class: "stack" },
      h("p", { class: "small" }, "Your friend installs ", h("b", {}, "Backup space"), " from their Homeport's catalog and makes a user for you (its Notes say how), then gives you its address, the user name and the password."),
      field("url", "Address", { placeholder: "https://backup.friend.duckdns.org" }),
      h("div", { class: "row" }, field("username", "User name"), field("secret", "Password", { type: "password", autocomplete: "off" }))),
    local: h("div", { class: "stack" },
      b.drives.length
        ? h("label", {}, "Drive", (f.drive = h("select", {}, b.drives.map(d => h("option", { value: d.path }, d.path + (d.label ? " (" + d.label + ")" : "") + (d.free ? " · " + d.free + " free" : ""))))))
        : h("p", { class: "muted small" }, "No drive found: plug one in and mount it (under /media or /mnt), or type its folder below."),
      field("folder", "Folder on it", { value: "homeport-backup" })),
  };
  const box = h("div", {});
  const show = () => {
    box.replaceChildren(parts[kind.value]);
    if (kind.value === "sftp") api("GET", "/backup/public-key").then(r => { key.textContent = r.key; }).catch(err => { key.textContent = err.message; });
  };
  kind.addEventListener("change", show);
  show();
  const submit = h("button", { class: "primary", type: "submit" }, "Check and add");
  openDialog(h("form", { method: "dialog", class: "wide", onsubmit: async e => {
    e.preventDefault();
    const v = k => (f[k] ? f[k].value.trim() : "");
    const d = { kind: kind.value, name: name.value.trim() };
    if (d.kind === "sftp") Object.assign(d, { host: v("host"), port: +v("port") || 22, user: v("user"), path: v("path") });
    if (d.kind === "s3") Object.assign(d, { endpoint: v("endpoint"), bucket: v("bucket"), region: v("region"), accessKey: v("accessKey"), secretKey: v("secretKey") });
    if (d.kind === "homeport") Object.assign(d, { url: v("url"), username: v("username"), secret: v("secret") });
    if (d.kind === "local") d.path = ((f.drive && f.drive.value) || "").replace(/\/$/, "") + "/" + v("folder").replace(/^\//, "");
    submit.disabled = true; submit.textContent = "Checking…";
    try { const r = await api("POST", "/backup/destinations", d); dialog.close(); toast(r.message); route(); }
    catch (err) { toast(err.message); submit.disabled = false; submit.textContent = "Check and add"; }
  } },
    h("h2", {}, "Add a place for backups"),
    h("label", {}, "Kind", kind), h("label", {}, "Name", name), box,
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), submit)));
}

// snapshotsDialog lists the backups at a place, and restores an app from one.
async function snapshotsDialog(d, appsList) {
  const body = h("div", { class: "stack" }, h("p", { class: "muted" }, "Asking " + d.name + "…"));
  openDialog(h("div", { class: "body wide-body" }, h("h2", {}, "Backups at " + d.name), body,
    h("div", { class: "buttons" }, h("button", { onclick: () => dialog.close() }, "Close"))));
  let snaps;
  try { snaps = await api("GET", "/backup/destinations/" + d.id + "/snapshots"); } catch (err) { body.replaceChildren(h("p", { class: "error" }, err.message)); return; }
  const names = Object.fromEntries(appsList.map(a => [a.id, a.name]));
  const restore = s => {
    if (!confirm("Restore " + (names[s.what] || s.what) + " as it was on " + new Date(s.time).toLocaleString() + "? It stops for a moment; its current folder is kept beside it, so this can be undone.")) return;
    dialog.close();
    runJob("Restoring " + (names[s.what] || s.what), () => api("POST", "/backup/restore", { destination: d.id, snapshot: s.id, app: s.what }));
  };
  body.replaceChildren(snaps.length
    ? h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["When", "What", "Size", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, snaps.map(s => h("tr", {},
          h("td", {}, new Date(s.time).toLocaleString()),
          h("td", {}, s.what === "homeport" ? "Homeport's settings" : (names[s.what] || s.what + " (not installed)")),
          h("td", { class: "small" }, s.size ? sizeText(s.size) : ""),
          h("td", { class: "actions" }, s.what !== "homeport" && names[s.what] ? h("button", { onclick: () => restore(s) }, "Restore") : null))))))
    : h("p", { class: "muted" }, "No backups there yet."));
}

function usePasswordDialog() {
  const pw = h("input", { type: "password", required: true, minlength: "8", autocomplete: "off", "aria-label": "Password" });
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    try { await api("PUT", "/backup/password", { password: pw.value }); dialog.close(); toast("Saved: backups now use that password"); route(); } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Use the password of earlier backups"),
    h("p", {}, "For restoring onto a new server: the password the backups were made with. New backups use it too. Backups at places already added with another password can't be read with it."),
    h("label", {}, "Password", pw),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Use it"))));
}

function removeDialog(a) {
  const purge = h("input", { type: "checkbox" });
  openDialog(h("form", { method: "dialog", onsubmit: e => {
    e.preventDefault(); dialog.close();
    runJob("Removing " + a.name, () => api("DELETE", "/apps/" + a.id + (purge.checked ? "?purge=1" : "")));
  } },
    h("h2", {}, "Remove " + a.name + "?"),
    h("p", {}, "It stops and comes off " + a.domain + "."),
    h("label", { class: "check" }, purge, "Also delete its data (files, database). This can't be undone."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "danger", type: "submit" }, "Remove"))));
}

function userDialog(u) {
  const editing = !!u;
  const name = h("input", { value: u ? u.name : "", required: !editing, disabled: editing, autocapitalize: "none", spellcheck: "false" });
  const display = h("input", { value: u ? u.displayName : "" });
  const email = h("input", { type: "email", value: u ? u.email || "" : "" });
  const password = h("input", { type: "password", autocomplete: "new-password", required: !editing, minlength: "10" });
  const groups = h("input", { value: u ? u.groups.join(", ") : "" , placeholder: "family, work" });
  const admin = h("input", { type: "checkbox", checked: u ? u.admin : false });
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const body = { displayName: display.value.trim(), email: email.value.trim(), groups: splitGroups(groups.value), admin: admin.checked };
    if (password.value) body.password = password.value;
    try {
      if (editing) await api("PATCH", "/users/" + u.name, body);
      else await api("POST", "/users", { name: name.value.trim(), ...body, password: password.value });
      dialog.close(); toast(editing ? "Saved" : "Added " + name.value.trim()); route();
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, editing ? "Edit " + u.name : "Add a user"),
    h("label", {}, "User name", name),
    h("label", {}, "Full name", display),
    h("label", {}, "Email", email),
    h("label", {}, editing ? "New password (leave empty to keep it)" : "Password (at least 10 characters)", password),
    h("label", {}, "Groups (comma-separated)", groups),
    h("label", { class: "check" }, admin, "Admin (can use this panel)"),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, editing ? "Save" : "Add"))));
}

function confirmDialog(title, text, action, run) {
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    try { await run(); dialog.close(); route(); } catch (err) { toast(err.message); }
  } },
    h("h2", {}, title), h("p", {}, text),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "danger", type: "submit" }, action))));
}

// shareDialog shares a new folder (sh null) or changes who can use one.
function shareDialog(sh, x, accounts) {
  const name = h("input", { required: true, value: sh ? sh.name : "", disabled: !!sh, placeholder: "Family", autocapitalize: "none", spellcheck: "false", pattern: "[A-Za-z0-9][A-Za-z0-9_-]*" });
  const place = h("select", {}, x.places.map(p => h("option", { value: p.base }, p.label + " · " + sizeText(p.free) + " free")), h("option", { value: "" }, "A folder that's already there…"));
  const path = h("input", { placeholder: "/mnt/backup/Movies", autocapitalize: "none", spellcheck: "false" });
  const where = h("p", { class: "muted small" });
  const pathRow = h("label", {}, "Folder", path);
  const show = () => {
    pathRow.hidden = !!place.value;
    where.textContent = place.value ? "A new folder: " + place.value + "/" + (name.value.trim() || "…") : "It keeps its files and their owner; Homeport doesn't change them.";
  };
  name.addEventListener("input", show); place.addEventListener("change", show);
  const guests = h("input", { type: "checkbox", checked: sh ? !!sh.guests : false });
  const comment = h("input", { value: sh ? sh.comment || "" : "", placeholder: "Photos for everyone at home" });
  const picks = accounts.map(a => {
    const sel = h("select", {}, h("option", { value: "" }, "No access"), h("option", { value: "read" }, "Read"), h("option", { value: "write" }, "Read and write"));
    sel.value = (sh && sh.access && sh.access[a.name]) || "";
    return { name: a.name, sel };
  });
  const submit = h("button", { class: "primary", type: "submit" }, sh ? "Save" : "Share it");
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const access = {};
    for (const p of picks) if (p.sel.value) access[p.name] = p.sel.value;
    const body = { name: name.value.trim(), guests: guests.checked, access, comment: comment.value.trim(), off: sh ? !!sh.off : false };
    if (!body.guests && !Object.keys(access).length &&
      !confirm("Nobody can open this share yet: no account has access, and guests are off. Save it anyway?")) return;
    submit.disabled = true;
    try {
      if (sh) await api("PUT", "/shares/" + encodeURIComponent(sh.name), body);
      else await api("POST", "/shares", { ...body, path: place.value ? place.value + "/" + body.name : path.value.trim() });
      dialog.close(); route();
    } catch (err) { toast(err.message); submit.disabled = false; }
  } },
    h("h2", {}, sh ? "Who can use " + sh.name : "Share a folder"),
    sh ? h("p", { class: "muted small" }, sh.path) : [h("label", {}, "Name on the network", name), h("label", {}, "Where", place), pathRow, where],
    h("label", {}, "Description (optional)", comment),
    h("label", { class: "check" }, guests, "Anyone on the home network can open it without signing in (read only)"),
    h("h3", {}, "Accounts"),
    picks.length ? h("div", { class: "stack" }, picks.map(p => h("label", { class: "row" }, h("span", { class: "grow" }, p.name), p.sel)))
      : h("p", { class: "muted small" }, "No accounts yet: add one under Accounts, then come back here."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), submit)));
  if (!sh) show();
}

// shareAccountDialog adds an account for shared folders, or sets a new password for one (name given).
function shareAccountDialog(existing) {
  const name = h("input", { required: true, value: existing || "", disabled: !!existing, autocapitalize: "none", spellcheck: "false", pattern: "[a-z_][a-z0-9_-]*", placeholder: "alex" });
  const pw = h("input", { type: "password", required: true, minlength: 8, autocomplete: "new-password" });
  const pw2 = h("input", { type: "password", required: true, minlength: 8, autocomplete: "new-password" });
  const submit = h("button", { class: "primary", type: "submit" }, existing ? "Set password" : "Add");
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    if (pw.value !== pw2.value) { toast("The passwords don't match"); return; }
    submit.disabled = true;
    try {
      if (existing) await api("POST", "/shares/accounts/" + encodeURIComponent(existing) + "/password", { password: pw.value });
      else await api("POST", "/shares/accounts", { name: name.value.trim(), password: pw.value });
      dialog.close(); toast(existing ? "Password changed" : "Added: now give it a shared folder"); route();
    } catch (err) { toast(err.message); submit.disabled = false; }
  } },
    h("h2", {}, existing ? "New password for " + existing : "Add an account"),
    h("label", {}, "Name", name),
    existing ? null : h("p", { class: "muted small" }, "Lower-case letters and digits. If it's already someone's login to this server, it uses that (with this password for shares)."),
    h("label", {}, "Password (at least 8 characters)", pw), h("label", {}, "Password again", pw2),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), submit)));
}

// filesDialog: an app's files (Kiwix's ZIM files, say): download links, magnet and torrent links on the server, with
// each download's progress, and the files already there.
function filesDialog(a) {
  const base = "/apps/" + encodeURIComponent(a.id);
  const status = h("p", { class: "muted small" });
  const downloads = h("div", { class: "stack" });
  const files = h("div", {});
  const link = h("input", { placeholder: "Download link, magnet link or .torrent link", autocapitalize: "none", spellcheck: "false", required: true });
  const add = h("button", { class: "primary", type: "submit" }, "Download");
  const find = h("a", { target: "_blank", rel: "noopener" });
  let timer, info = { what: "files" };
  const rate = n => n > 0 ? sizeText(n) + "/s" : "";
  const left = d => d.speed > 0 && d.total > d.done ? (s => s >= 3600 ? Math.floor(s / 3600) + " h " + Math.round(s % 3600 / 60) + " min" : s >= 60 ? Math.round(s / 60) + " min" : s + " s")(Math.round((d.total - d.done) / d.speed)) + " left" : "";
  const act = async (d, what) => { try { await api("POST", base + "/downloads/" + d.gid + "/" + what); refresh(); } catch (err) { toast(err.message); } };
  async function refresh() {
    let x;
    try { x = await api("GET", base + "/files"); } catch (err) { status.textContent = err.message; return; }
    info = x;
    find.href = x.find || "#"; find.textContent = x.find ? "Find " + (x.what || "files") + " at " + x.find.replace(/^https?:\/\//, "") : "";
    status.textContent = sizeText(x.free) + " free on this drive";
    downloads.replaceChildren(...x.downloads.map(d => {
      const pct = d.total ? Math.floor(d.done * 100 / d.total) : 0;
      const state = d.metadata ? "getting the torrent's details…" : d.status === "active" ? (pct + "% · " + sizeText(d.done) + " of " + sizeText(d.total) + (d.speed ? " · " + rate(d.speed) : "") + " · " + (d.torrent ? d.peers + " peers" + (d.seeders ? ", " + d.seeders + " seeding" : "") : d.peers + " connections") + (left(d) ? " · " + left(d) : ""))
        : d.status === "paused" ? "paused at " + pct + "%" : d.status === "waiting" ? "waiting its turn" : d.status === "complete" ? "done" : d.status === "error" ? "stopped: " + (d.error || "error") : d.status;
      return h("div", { class: "dl" },
        h("div", { class: "dl-top" }, h("b", {}, d.name || "…"), d.torrent ? h("span", { class: "pill" }, "torrent") : null),
        h("div", { class: "meter-bar dl-bar", "aria-hidden": "true" }, h("span", { class: "meter-fill w" + Math.round(pct / 5) * 5 })),
        h("div", { class: "row dl-row" }, h("span", { class: "muted small grow" }, state),
          d.status === "active" || d.status === "waiting" ? h("button", { type: "button", onclick: () => act(d, "pause") }, "Pause") : null,
          d.status === "paused" ? h("button", { type: "button", onclick: () => act(d, "resume") }, "Resume") : null,
          h("button", { type: "button", class: d.status === "complete" ? "" : "danger", onclick: () => {
            if (d.status !== "complete" && !confirm("Stop downloading " + (d.name || "this") + "?")) return;
            act(d, "cancel");
          } }, d.status === "complete" ? "Clear" : "Cancel")));
    }));
    files.replaceChildren(x.files.length ? h("div", { class: "list" }, h("table", {}, h("tbody", {}, x.files.map(f => h("tr", {},
      h("td", {}, h("b", {}, f.name), h("div", { class: "muted small" }, sizeText(f.size) + " · " + new Date(f.modified).toLocaleDateString())),
      h("td", { class: "actions" }, h("button", { class: "danger", onclick: async () => {
        if (!confirm("Delete " + f.name + " (" + sizeText(f.size) + ")?")) return;
        try { await api("DELETE", base + "/files/" + encodeURIComponent(f.name)); refresh(); } catch (err) { toast(err.message); }
      } }, "Delete")))))))
      : h("p", { class: "muted" }, "No " + (x.what || "files") + " yet."));
    clearTimeout(timer);
    if (dialog.open) timer = setTimeout(refresh, x.downloads.some(d => d.status !== "complete" && d.status !== "error") ? 2000 : 8000);
  }
  openDialog(h("div", { class: "stack" },
    h("h2", {}, a.name + ": files"),
    h("form", { class: "row", onsubmit: async e => {
      e.preventDefault();
      add.disabled = true; add.textContent = "Starting…";
      try {
        const j = await api("POST", base + "/files", { url: link.value.trim() });
        for (let i = 0; i < 180; i++) {   // the first download may install the downloader (a minute or so)
          const s = await api("GET", "/jobs/" + j.id);
          if (s.status !== "running") { if (s.status === "failed") throw new Error(s.error || s.log); break; }
          add.textContent = s.log.includes("Installing") ? "Installing the downloader…" : "Starting…";
          await new Promise(r => setTimeout(r, 1000));
        }
        link.value = ""; refresh();
      } catch (err) { toast(err.message); }
      add.disabled = false; add.textContent = "Download";
    } }, link, add),
    h("p", { class: "small" }, find, " · links, magnet links and .torrent links all work"),
    downloads, h("h3", {}, "On the server"), files, status,
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => { clearTimeout(timer); dialog.close(); } }, "Close"))));
  dialog.addEventListener("close", () => clearTimeout(timer), { once: true });
  refresh();
}

// Starts a background job and shows its log until it finishes.
async function runJob(title, start) {
  const status = h("span", { class: "pill" }, "starting");
  const log = h("pre", { class: "log" }, "");
  const close = h("button", { onclick: () => { dialog.close(); route(); } }, "Close");
  openDialog(h("div", { class: "body" }, h("div", { class: "head" }, h("h2", {}, title), status), log, h("div", { class: "buttons" }, close)));
  let job;
  try { job = await start(); } catch (err) { status.textContent = "failed"; status.className = "pill failed"; log.textContent = err.message; return null; }
  let j = job;
  while (dialog.open) {
    j = await api("GET", "/jobs/" + job.id).catch(err => ({ status: "failed", log: err.message }));
    log.textContent = j.log + (j.error ? "\n" + j.error : "");
    log.scrollTop = log.scrollHeight;
    status.textContent = j.status; status.className = "pill " + j.status;
    if (j.status !== "running") break;
    await new Promise(r => setTimeout(r, 1000));
  }
  return j;
}

// Installing an app from outside the catalog: read the repository, then review the manifest before installing.
function fromGitHubDialog() {
  const url = h("input", { required: true, placeholder: "https://github.com/owner/app", autocapitalize: "none", spellcheck: "false" });
  const use = h("select", {}, h("option", { value: "" }, "Claude if there's an API key, otherwise the local model"),
    h("option", { value: "claude" }, "Claude"), h("option", { value: "local" }, "The local model"));
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const j = await runJob("Reading the app", () => api("POST", "/apps/draft", { url: url.value.trim(), use: use.value }));
    if (j && j.status === "done" && j.result) reviewDialog(j.result);
  } },
    h("h2", {}, "Install from GitHub"),
    h("label", {}, "Repository", url),
    h("label", {}, "If it has no homeport.yaml, the assistant writes one with", use),
    h("p", { class: "muted small" }, "Homeport reads the repository's compose file and README; nothing from it runs yet. You see what it would install, and Homeport refuses anything that reaches outside the app's own folder and port."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Read it"))));
}

function reviewDialog(res) {
  const main = me.settings.mainDomain;
  const idOf = text => (text.match(/^id:\s*["']?([a-z0-9-]+)/m) || [])[1] || "app";
  const manifest = h("textarea", { rows: "18", spellcheck: "false", class: "code" });
  manifest.value = res.manifest;
  const domain = h("input", { required: true, value: idOf(res.manifest) + "." + main, autocapitalize: "none", spellcheck: "false" });
  const pub = h("input", { type: "checkbox" });
  const groups = h("input", { value: "all" });
  const findings = h("div", { class: "stack" });
  const install = h("button", { class: "primary", type: "submit" }, "Install");
  let edited = false;
  const show = r => {
    findings.replaceChildren(
      r.problems && r.problems.length ? h("div", { class: "card problems" }, h("b", {}, "Homeport won't install this:"), h("ul", {}, r.problems.map(p => h("li", {}, p)))) : h("p", {}, h("span", { class: "pill done" }, "Passed Homeport's checks")),
      r.warnings && r.warnings.length ? h("div", { class: "card warnings" }, h("b", {}, "Good to know:"), h("ul", {}, r.warnings.map(p => h("li", {}, p)))) : null);
    install.disabled = !!(r.problems && r.problems.length);
  };
  show(res.review);
  manifest.addEventListener("input", () => { edited = true; install.disabled = true; });
  const recheck = h("button", { type: "button", onclick: async () => { try { show(await api("POST", "/apps/check", { manifest: manifest.value })); } catch (err) { toast(err.message); } } }, "Check again");
  const drive = driveField();
  const by = res.source.by === "repository" ? "the repository's own homeport.yaml" : "written by " + res.source.by + " from " + (res.compose || "the repository");
  openDialog(h("form", { method: "dialog", class: "wide", onsubmit: async e => {
    e.preventDefault();
    const access = pub.checked ? { public: true } : { groups: splitGroups(groups.value) };
    const src = { ...res.source, by: edited ? "admin" : res.source.by };
    const j = await runJob("Installing from " + res.source.url, () => api("POST", "/apps", { manifest: manifest.value, source: src, domain: domain.value.trim(), access, drive: drive.value() }));
    if (j && j.status === "done") toast("Installed");
  } },
    h("h2", {}, "Review before installing"),
    h("p", { class: "warn-box" }, "This app isn't from Homeport's catalog. Homeport checked what it can reach, but nobody reviewed it: only install apps you trust."),
    h("p", { class: "small" }, h("a", { href: res.source.url, target: "_blank", rel: "noopener" }, res.source.url), " at commit ", h("code", {}, res.source.commit.slice(0, 12)), ". Manifest ", by, "."),
    h("label", {}, "Manifest (you can edit it, then Check again)", manifest),
    h("div", { class: "row" }, recheck),
    findings,
    h("label", {}, "Address", domain),
    h("label", { class: "check" }, pub, "Anyone can open it (no Homeport sign-in in front)"),
    h("label", {}, "Or only these groups (comma-separated; \"all\" is every user)", groups),
    drive.el,
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), install)));
}

function splitGroups(s) { return s.split(",").map(x => x.trim().toLowerCase()).filter(Boolean); }

// MARK: Routing

function terminalForm(st) {
  const on = h("input", { type: "checkbox", checked: !!st.terminal });
  return h("form", { class: "card stack", onsubmit: async e => {
    e.preventDefault();
    try {
      toast(on.checked ? "Turning the terminal on (installing ttyd the first time)…" : "Turning the terminal off…");
      me = await api("PUT", "/settings", { terminal: on.checked });
      terminalLink(); toast(on.checked ? "The terminal is on" : "The terminal is off");
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Terminal"),
    h("p", { class: "muted small" }, "A terminal on this server in your browser, at ", h("code", {}, "/terminal/"),
      ". Only admins can open it, and it then asks for a Linux account's name and password, like the console does. Handy for running Claude Code or other tools by hand."),
    h("label", { class: "check" }, on, "Turn on the web terminal"),
    h("div", {}, h("button", { class: "primary", type: "submit" }, "Save")));
}

// terminalLink shows Terminal in the menu while it's on.
function terminalLink() {
  const nav = document.querySelector(".side nav");
  let a = nav.querySelector('a[href="/terminal/"]');
  if (me.settings.terminal && !a) nav.insertBefore(h("a", { href: "/terminal/", target: "_blank", rel: "noopener" }, "Terminal ↗"), nav.querySelector('a[href="/"]'));
  if (!me.settings.terminal && a) a.remove();
}

function chatSettingsForm(cfg) {
  const st = cfg.settings;
  const opt = (value, label, current) => h("option", { value, selected: value === current }, label);
  const mode = h("select", {}, opt("auto", "Automatic: the local model for easy questions, Claude for hard ones", st.mode || "auto"),
    opt("claude", "Claude only", st.mode), opt("local", "Local model only (nothing leaves your network)", st.mode));
  const key = h("input", { type: "password", autocomplete: "off", placeholder: cfg.hasKey ? "Saved (" + (cfg.keyHint || "set") + "). Type a new one to replace it" : "sk-ant-…" });
  const model = h("select", {}, cfg.claudeModels.map(m => opt(m, m, st.claudeModel || cfg.defaults.claudeModel)));
  const effort = h("select", {}, [["low", "Low (fastest, cheapest)"], ["medium", "Medium (default)"], ["high", "High"], ["xhigh", "Extra high"], ["max", "Max (slowest)"]]
    .map(([v, l]) => opt(v, l, st.effort || cfg.defaults.effort)));
  const ask = h("input", { type: "checkbox", checked: !!st.askBeforeCloud });
  const cap = h("input", { type: "number", min: "0", step: "1", value: st.monthlyCapUSD || "", placeholder: "No limit" });
  const url = h("input", { value: st.localURL || "", placeholder: "http://192.168.1.20:1234 (LM Studio) or :11434 (Ollama)" });
  const localModel = h("select", {}, h("option", { value: "" }, "The first model it has loaded"),
    st.localModel ? h("option", { value: st.localModel, selected: true }, st.localModel) : null);
  const found = h("p", { class: "muted small" });
  async function findModels() {
    found.textContent = "Asking " + (url.value.trim() || "…") + "…";
    try {
      const r = await api("POST", "/chat-settings/local-models", { localURL: url.value });
      const keep = localModel.value;
      localModel.replaceChildren(h("option", { value: "" }, "The first model it has loaded"), ...r.models.map(m => opt(m, m, keep)));
      found.textContent = r.models.length ? "Found " + r.models.length + " model" + (r.models.length > 1 ? "s" : "") + " at " + r.localURL + "." : "It answered, but has no chat model loaded.";
    } catch (err) { found.textContent = err.message; }
  }
  return h("form", { class: "card stack", onsubmit: async e => {
    e.preventDefault();
    try {
      await api("PUT", "/chat-settings", { mode: mode.value, claudeModel: model.value, effort: effort.value, askBeforeCloud: ask.checked,
        monthlyCapUSD: +cap.value || 0, localURL: url.value.trim(), localModel: localModel.value, apiKey: key.value.trim() });
      toast("Saved"); route();
    } catch (err) { toast(err.message); }
  } },
    h("h2", {}, "Assistant chat"),
    h("p", { class: "muted small" }, "The chat uses Claude, a local model, or both. Homeport doesn't run a model itself: if this server has no strong graphics card, run LM Studio or Ollama on a computer that has one (a Mac with Apple silicon works well), turn on its network server, and put its address here."),
    h("label", {}, "Which model answers", mode),
    h("h3", {}, "Claude"),
    h("label", {}, "Anthropic API key (kept where only root can read it)", key),
    cfg.hasKey ? h("div", {}, h("button", { type: "button", class: "danger", onclick: async () => {
      try { await api("PUT", "/chat-settings", { ...st, removeKey: true }); toast("Key removed"); route(); } catch (err) { toast(err.message); } } }, "Remove the key")) : null,
    h("div", { class: "row" }, h("label", {}, "Model", model), h("label", {}, "Effort", effort)),
    h("label", { class: "check" }, ask, "Ask me before sending anything to Claude"),
    h("label", {}, "Monthly limit for Claude, in US dollars (an estimate; this month so far: about $" + cfg.spent.usd.toFixed(2) + ")", cap),
    h("p", { class: "muted small" }, "If a safety check declines a request, Anthropic re-runs it on its recommended fallback model (Opus models only)."),
    h("h3", {}, "Local model"),
    h("div", { class: "row" }, h("label", {}, "Address of an OpenAI-compatible server", url), h("button", { type: "button", onclick: findModels }, "Find models")),
    found,
    h("label", {}, "Model", localModel),
    h("p", { class: "muted small" }, "To find new models, see how well they'd run here and download them: ", h("a", { href: "#models" }, "AI models"), "."),
    h("div", {}, h("button", { class: "primary", type: "submit" }, "Save")));
}

// MARK: Assistant chat

const chat = { id: null, timer: null, renderedTurns: -1 };

// rich shows a reply's text: ``` blocks as code, `code` and **bold** inline, paragraphs and line breaks kept.
function chatView(list, cfg) {
  clearTimeout(chat.timer);
  chat.renderedTurns = -1;
  if (chat.id && !list.some(c => c.id === chat.id)) chat.id = null;
  const ready = cfg.hasKey || cfg.settings.localURL;
  const thread = h("div", { class: "thread" });
  const live = h("div", { class: "live", "aria-live": "polite" });
  const approvals = h("div", { class: "stack" });
  const input = h("textarea", { rows: "3", placeholder: "Ask about this server, or ask for a change… (drop a screenshot or file here too)", "aria-label": "Message" });
  const att = attacher(input, input);
  chatPicture = (i, n) => "/api/chat/" + chat.id + "/images/" + i + "/" + n;
  try { const ask = sessionStorage.getItem(ASK); if (ask) { input.value = ask; sessionStorage.removeItem(ASK); setTimeout(() => input.focus(), 0); } } catch { /* private window */ }
  const use = h("select", { "aria-label": "Which model answers" },
    h("option", { value: "" }, "Model: as in settings"),
    cfg.settings.localURL ? h("option", { value: "local" }, "Local model") : null,
    cfg.hasKey ? h("option", { value: "claude" }, "Claude") : null);
  const send = h("button", { class: "primary", type: "submit" }, "Send");
  const stop = h("button", { type: "button", hidden: true, onclick: async () => { try { await api("POST", "/chat/" + chat.id + "/stop"); } catch (err) { toast(err.message); } } }, "Stop");
  const convs = h("nav", { class: "convs", "aria-label": "Conversations" });

  function drawList(items) {
    convs.replaceChildren(
      h("button", { type: "button", class: chat.id ? "" : "current", onclick: () => { chat.id = null; route(); } }, "+ New conversation"),
      ...items.map(c => h("button", { type: "button", class: c.id === chat.id ? "current" : "", title: c.title,
        onclick: () => { chat.id = c.id; route(); } }, c.title)));
  }

  function intro() {
    if (!ready) return [h("div", { class: "card stack" }, h("h2", {}, "Set up a model first"),
      h("p", {}, "The chat needs Claude (an Anthropic API key) or a local model running in LM Studio or Ollama on a computer with a good graphics card. ",
        h("a", { href: "#settings" }, "Open Settings → Assistant chat"), "."))];
    const ask = text => () => { input.value = text; input.focus(); };
    return [h("div", { class: "card stack" },
      h("h2", {}, "Ask me to look after this server"),
      h("p", { class: "muted" }, "I can see the apps, users, domains, logs and health. Changes wait for your OK: an Approve button shows up here."),
      h("div", { class: "row" }, ["What's installed, and is everything healthy?", "Why can't I open an app?", "Install Mealie and let everyone use it"].map(t =>
        h("button", { type: "button", onclick: ask(t) }, t))))];
  }

  function draw(conv) {
    const turns = conv ? conv.turns : [];
    const follow = nearBottom() || turns.length !== chat.renderedTurns;
    if (turns.length !== chat.renderedTurns) {
      thread.replaceChildren(...(turns.length ? turns.map(turnView) : intro()));
      chat.renderedTurns = turns.length;
    }
    const bits = [];
    if (conv && conv.partial) bits.push(h("div", { class: "msg bot" }, rich(conv.partial)));
    if (conv && conv.busy) bits.push(h("p", { class: "muted small status" }, statusLine(conv)));
    if (conv && conv.consent) bits.push(h("div", { class: "card stack consent" },
      h("p", {}, "This would go to Claude, on Anthropic's servers", conv.consentWhy ? " (" + conv.consentWhy + ")" : "", ". Send it?"),
      h("div", { class: "row" },
        h("button", { class: "primary", type: "button", onclick: () => cont({ allowCloud: true }) }, "Send to Claude"),
        conv.canLocal ? h("button", { type: "button", onclick: () => cont({ use: "local" }) }, "Use the local model") : null)));
    live.replaceChildren(...bits);
    const busy = !!(conv && conv.busy);
    send.disabled = busy || !ready; stop.hidden = !busy;
    if (follow && turns.length) toBottom();
  }

  async function cont(body) {
    try { draw(await api("POST", "/chat/" + chat.id + "/continue", body)); poll(); } catch (err) { toast(err.message); }
  }

  async function drawApprovals() {
    const pending = await api("GET", "/approvals").catch(() => []);
    approvals.replaceChildren(...pending.map(p => h("div", { class: "card approval" },
      h("div", {}, h("b", {}, p.summary), h("div", { class: "muted small" }, p.client + " · " + p.tool)),
      h("div", { class: "row" },
        h("button", { class: "primary", type: "button", onclick: () => decide(p, "approve") }, "Approve"),
        h("button", { class: "danger", type: "button", onclick: () => decide(p, "deny") }, "Decline")))));
  }
  async function decide(p, d) {
    try { await api("POST", "/approvals/" + p.id + "/" + d); toast(d === "approve" ? "Approved" : "Declined"); } catch (err) { toast(err.message); }
    drawApprovals();
  }

  async function poll() {
    clearTimeout(chat.timer);
    if (location.hash !== "#assistant") return;
    let busy = false;
    try {
      if (chat.id) { const conv = await api("GET", "/chat/" + chat.id); draw(conv); busy = conv.busy; }
      await drawApprovals();
      if (!busy && chat.id) drawList(await api("GET", "/chat"));
    } catch (err) { if (/no such conversation/.test(err.message)) { chat.id = null; route(); return; } }
    chat.timer = setTimeout(poll, busy ? 900 : 4000);
  }

  const form = h("form", { class: "composer", onsubmit: async e => {
    e.preventDefault();
    const text = input.value.trim();
    if (!text && !att.count()) return;
    send.disabled = true;
    try {
      const conv = await api("POST", "/chat", { conversation: chat.id || "", text, use: use.value, files: att.list() });
      const isNew = chat.id !== conv.id;
      chat.id = conv.id; input.value = ""; att.clear();
      if (isNew) drawList(await api("GET", "/chat"));
      draw(conv); poll();
    } catch (err) { toast(err.message); send.disabled = false; }
  } }, att.strip, input, h("div", { class: "row" }, att.button, use, stop, send), att.picker);
  att.button.classList.add("plain");
  input.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); form.requestSubmit(); } });

  drawList(list);
  draw(null);
  setTimeout(poll, 0);
  const spent = cfg.hasKey ? "Claude this month: about $" + cfg.spent.usd.toFixed(2) + (cfg.settings.monthlyCapUSD ? " of $" + cfg.settings.monthlyCapUSD : "") : "";
  return [
    h("div", { class: "head" }, h("h1", {}, "Assistant"),
      h("div", { class: "row" }, spent ? h("span", { class: "muted small" }, spent) : null, h("a", { class: "button", href: "#activity" }, "Approvals and activity"))),
    h("div", { class: "chat" }, convs, h("div", { class: "stack" }, approvals, thread, live, form)),
  ];
}

async function route() {
  const name = (location.hash || "#overview").slice(1);
  const fn = views[name] || views.overview;
  for (const a of document.querySelectorAll(".side nav a")) a.toggleAttribute("aria-current", a.getAttribute("href") === "#" + name);
  document.querySelector('.side nav a[href="#' + name + '"]')?.setAttribute("aria-current", "page");
  try { view.replaceChildren(...(await fn()).flat(2).filter(x => x != null && x !== false)); }
  catch (err) { view.replaceChildren(h("p", { class: "error" }, err.message)); }
}

(async () => {
  try {
    me = await api("GET", "/overview");
    document.getElementById("domain").textContent = me.settings.mainDomain;
    document.getElementById("version").textContent = "Homeport " + me.version;
    const ai = document.getElementById("ai-link");
    ai.href = "https://" + (me.aiAddress || "ai." + me.settings.mainDomain) + "/"; ai.hidden = false;
    terminalLink();
  } catch (err) { toast(err.message); }
  document.getElementById("look-spot")?.replaceChildren(lookPicker());
  window.addEventListener("hashchange", route);
  route();
})();
