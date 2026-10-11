// Homeport admin panel. Plain DOM building (no innerHTML with data), talking to /api with the X-Homeport header.
"use strict";

const view = document.getElementById("view");
const dialog = document.getElementById("dialog");
let me = { settings: {} };

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid instanceof Node ? kid : String(kid));
  return el;
}

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
      h("td", {}, h("b", {}, a.name), h("div", { class: "muted small" }, a.id)),
      h("td", {}, h("a", { href: "https://" + a.domain + "/", target: "_blank", rel: "noopener" }, a.domain)),
      h("td", {}, accessText(a.access)),
      h("td", {}, h("span", { class: "pill " + a.status.split(" ")[0] }, a.status)),
      h("td", { class: "actions" },
        h("button", { onclick: () => accessDialog(a) }, "Access"),
        a.hasSecrets && h("button", { onclick: () => secretsDialog(a) }, "Secrets"),
        h("button", { onclick: () => runJob("Updating " + a.name, () => api("POST", "/apps/" + a.id + "/update")) }, "Update"),
        h("button", { class: "danger", onclick: () => removeDialog(a) }, "Remove"))));
    return [
      h("div", { class: "head" }, h("h1", {}, "Apps")),
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
    return [
      h("div", { class: "head" }, h("h1", {}, "Domains")),
      h("form", { class: "card row", onsubmit: async e => { e.preventDefault(); try { await api("POST", "/domains", { name: input.value.trim() }); toast("Added " + input.value.trim()); route(); } catch (err) { toast(err.message); } } },
        h("label", {}, "Add a domain", input), h("button", { class: "primary", type: "submit" }, "Add")),
      h("div", { class: "list" }, h("table", {},
        h("thead", {}, h("tr", {}, ["Domain", "Used for", "Added", ""].map(t => h("th", {}, t)))),
        h("tbody", {}, domains.map(d => h("tr", {},
          h("td", {}, h("b", {}, d.name)),
          h("td", {}, d.main ? "Portal and admin panel" : d.app || h("span", { class: "muted" }, "nothing yet")),
          h("td", {}, when(d.added)),
          h("td", { class: "actions" }, !d.main && !d.app && h("button", { class: "danger", onclick: async () => { try { await api("DELETE", "/domains/" + d.name); toast("Removed " + d.name); route(); } catch (err) { toast(err.message); } } }, "Remove"))))))),
      h("p", { class: "muted small" }, "Point each domain's DNS (an A and AAAA record) at this server. Subdomains of a domain listed here are added for you when you install an app on one."),
    ];
  },

  async assistant() {
    const [pending, audit, mem] = await Promise.all([api("GET", "/approvals"), api("GET", "/audit?limit=60"), api("GET", "/memory")]);
    const list = pending.length
      ? h("div", { class: "list" }, h("table", {}, h("tbody", {}, pending.map(p => h("tr", {},
          h("td", {}, h("b", {}, p.summary), h("div", { class: "muted small" }, p.client + " · " + p.tool + " · waits until " + new Date(p.expires).toLocaleTimeString())),
          h("td", { class: "actions" },
            h("button", { class: "primary", onclick: () => decide(p, "approve") }, "Approve"),
            h("button", { class: "danger", onclick: () => decide(p, "deny") }, "Decline")))))))
      : h("p", { class: "muted" }, "Nothing is waiting for approval.");
    clearTimeout(window.approvalTimer);
    window.approvalTimer = setTimeout(() => { if (location.hash === "#assistant" && !dialog.open) route(); }, 4000);
    return [
      h("div", { class: "head" }, h("h1", {}, "Assistant")),
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
    const o = await api("GET", "/overview");
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
  try { job = await start(); } catch (err) { status.textContent = "failed"; status.className = "pill failed"; log.textContent = err.message; return; }
  while (dialog.open) {
    const j = await api("GET", "/jobs/" + job.id).catch(err => ({ status: "failed", log: err.message }));
    log.textContent = j.log + (j.error ? "\n" + j.error : "");
    log.scrollTop = log.scrollHeight;
    status.textContent = j.status; status.className = "pill " + j.status;
    if (j.status !== "running") break;
    await new Promise(r => setTimeout(r, 1000));
  }
}

function splitGroups(s) { return s.split(",").map(x => x.trim().toLowerCase()).filter(Boolean); }

// MARK: Routing

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
  } catch (err) { toast(err.message); }
  window.addEventListener("hashchange", route);
  route();
})();
