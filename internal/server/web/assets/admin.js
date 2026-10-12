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
      h("div", { class: "head" }, h("h1", {}, "Overview"), h("a", { class: "button", href: "https://" + o.settings.mainDomain + "/" }, "Open the portal")),
      h("div", { class: "stats" },
        stat(o.apps, "apps installed", "#apps"), stat(o.users, "users", "#users"), stat(o.domains, "domains", "#domains")),
      h("div", { class: "card stack" },
        h("h2", {}, "This server"),
        h("p", {}, "Portal and admin panel at ", h("b", {}, o.settings.mainDomain), "."),
        h("p", {}, "Certificates: ", o.settings.tls === "internal"
          ? "Caddy's own test certificates (browsers will warn; fine for a private network)."
          : "Let's Encrypt" + (o.settings.acmeEmail ? ", notices to " + o.settings.acmeEmail : "") + ".")),
    ];
    function stat(n, label, href) { return h("a", { class: "stat", href, style: null }, h("b", {}, n), h("span", { class: "muted" }, label)); }
  },

  async apps() {
    const [installed, catalog] = await Promise.all([api("GET", "/apps"), api("GET", "/catalog")]);
    const rows = installed.map(a => h("tr", {},
      h("td", {}, h("b", {}, a.name), h("div", { class: "muted small" }, a.id, a.source && [" · from ", h("a", { href: a.source.url, target: "_blank", rel: "noopener" }, a.source.url.replace("https://github.com/", ""))])),
      h("td", {}, h("a", { href: "https://" + a.domain + "/", target: "_blank", rel: "noopener" }, a.domain)),
      h("td", {}, accessText(a.access)),
      h("td", {}, h("span", { class: "pill " + a.status.split(" ")[0] }, a.status)),
      h("td", { class: "actions" },
        h("button", { onclick: () => accessDialog(a) }, "Access"),
        a.hasSecrets && h("button", { onclick: () => secretsDialog(a) }, "Secrets"),
        h("button", { onclick: () => runJob("Updating " + a.name, () => api("POST", "/apps/" + a.id + "/update")) }, "Update"),
        h("button", { class: "danger", onclick: () => removeDialog(a) }, "Remove"))));
    return [
      h("div", { class: "head" }, h("h1", {}, "Apps"), h("button", { onclick: fromGitHubDialog }, "Install from GitHub")),
      installed.length
        ? h("div", { class: "list" }, h("table", {}, h("thead", {}, h("tr", {}, ["App", "Address", "Who can open it", "Status", ""].map(t => h("th", {}, t)))), h("tbody", {}, rows)))
        : h("p", { class: "muted" }, "Nothing installed yet. Pick an app below."),
      h("h2", {}, "Catalog"),
      h("div", { class: "catalog" }, catalog.map(c => h("div", { class: "entry" },
        h("div", { class: "top" }, h("span", { class: "glyph", "aria-hidden": "true" }, c.name[0]), h("div", {}, h("b", {}, c.name), h("div", { class: "muted small" }, c.category))),
        h("p", {}, c.description),
        h("p", { class: "small" }, signInText(c.signIn)),
        h("div", {}, h("button", { class: "primary", onclick: () => installDialog(c) }, "Install"))))),
    ];
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
      h("div", { class: "head" }, h("h1", {}, "Domains"), h("button", { class: "primary", onclick: freeDomainDialog }, "Get a free domain")),
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
          h("td", {}, h("b", {}, e.tool), h("div", { class: "muted small mono" }, (e.args || "").slice(0, 140))),
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
        try { await api("PUT", "/settings", { assistantShell: shell.checked, approvalMinutes: +minutes.value }); toast("Saved"); } catch (err) { toast(err.message); }
      } },
        h("h2", {}, "AI helpers"),
        h("label", { class: "check" }, shell, "Let helpers ask to run shell commands as root (each one still needs your approval)"),
        h("label", {}, "Minutes a change waits for approval before it's dropped", minutes),
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

function installDialog(c) {
  const main = me.settings.mainDomain;
  const domain = h("input", { name: "domain", value: c.id + "." + main, required: true, autocapitalize: "none", spellcheck: "false" });
  const pub = h("input", { type: "checkbox", name: "public", checked: c.signIn === "own" });
  const groups = h("input", { name: "groups", value: "all", placeholder: "all" });
  openDialog(h("form", { method: "dialog", onsubmit: async e => {
    e.preventDefault();
    const access = pub.checked ? { public: true } : { groups: splitGroups(groups.value) };
    dialog.close();
    runJob("Installing " + c.name, () => api("POST", "/apps", { catalog: c.id, domain: domain.value.trim(), access }));
  } },
    h("h2", {}, "Install " + c.name),
    h("label", {}, "Address", domain),
    h("label", { class: "check" }, pub, "Anyone can open it (no Homeport sign-in in front)"),
    h("label", {}, "Or only these groups (comma-separated; \"all\" is every user)", groups),
    h("p", { class: "muted small" }, signInText(c.signIn), " Its DNS must point at this server."),
    h("div", { class: "buttons" }, h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Install"))));
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

const certNames = { letsencrypt: "Let's Encrypt", internal: "Caddy's own", custom: "Uploaded" };

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

async function secretsDialog(a) {
  try {
    const s = await api("GET", "/apps/" + a.id + "/secrets");
    openDialog(h("div", { class: "body" },
      h("h2", {}, a.name + " secrets"),
      h("p", { class: "muted small" }, "Made when it was installed. Keep them private."),
      h("div", { class: "list" }, h("table", {}, h("tbody", {}, Object.entries(s).map(([k, v]) => h("tr", {}, h("td", {}, k), h("td", { class: "mono" }, v)))))),
      h("div", { class: "buttons" }, h("button", { onclick: () => dialog.close() }, "Close"))));
  } catch (err) { toast(err.message); }
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
  const by = res.source.by === "repository" ? "the repository's own homeport.yaml" : "written by " + res.source.by + " from " + (res.compose || "the repository");
  openDialog(h("form", { method: "dialog", class: "wide", onsubmit: async e => {
    e.preventDefault();
    const access = pub.checked ? { public: true } : { groups: splitGroups(groups.value) };
    const src = { ...res.source, by: edited ? "admin" : res.source.by };
    const j = await runJob("Installing from " + res.source.url, () => api("POST", "/apps", { manifest: manifest.value, source: src, domain: domain.value.trim(), access }));
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
  const input = h("textarea", { rows: "3", placeholder: "Ask about this server, or ask for a change…", "aria-label": "Message" });
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
    if (turns.length !== chat.renderedTurns) {
      thread.replaceChildren(...(turns.length ? turns.map(turnView) : intro()));
      chat.renderedTurns = turns.length;
      thread.lastElementChild?.scrollIntoView({ block: "end" });
    }
    const bits = [];
    if (conv && conv.partial) bits.push(h("div", { class: "msg bot" }, rich(conv.partial)));
    if (conv && conv.busy) bits.push(h("p", { class: "muted small status" }, conv.status || "Working…"));
    if (conv && conv.consent) bits.push(h("div", { class: "card stack consent" },
      h("p", {}, "This would go to Claude, on Anthropic's servers", conv.consentWhy ? " (" + conv.consentWhy + ")" : "", ". Send it?"),
      h("div", { class: "row" },
        h("button", { class: "primary", type: "button", onclick: () => cont({ allowCloud: true }) }, "Send to Claude"),
        conv.canLocal ? h("button", { type: "button", onclick: () => cont({ use: "local" }) }, "Use the local model") : null)));
    live.replaceChildren(...bits);
    const busy = !!(conv && conv.busy);
    send.disabled = busy || !ready; stop.hidden = !busy;
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
    if (!text) return;
    send.disabled = true;
    try {
      const conv = await api("POST", "/chat", { conversation: chat.id || "", text, use: use.value });
      const isNew = chat.id !== conv.id;
      chat.id = conv.id; input.value = "";
      if (isNew) drawList(await api("GET", "/chat"));
      draw(conv); poll();
    } catch (err) { toast(err.message); send.disabled = false; }
  } }, input, h("div", { class: "row" }, use, stop, send));
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
  try { view.replaceChildren(...(await fn())); }
  catch (err) { view.replaceChildren(h("p", { class: "error" }, err.message)); }
}

(async () => {
  try {
    me = await api("GET", "/overview");
    document.getElementById("domain").textContent = me.settings.mainDomain;
    document.getElementById("version").textContent = "Homeport " + me.version;
    const ai = document.getElementById("ai-link");
    ai.href = "https://ai." + me.settings.mainDomain + "/"; ai.hidden = false;
    terminalLink();
  } catch (err) { toast(err.message); }
  window.addEventListener("hashchange", route);
  route();
})();
