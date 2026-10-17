// Homeport's pages share these: building DOM without innerHTML, and drawing chat messages (a little Markdown: code
// blocks, headings, tables, **bold** and `code`).
"use strict";

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

function rich(text) {
  const out = [];
  String(text || "").split(/```/).forEach((part, i) => {
    if (i % 2) {
      const code = part.replace(/^[a-z0-9+-]*\n/i, "").replace(/\n$/, "");
      out.push(h("div", { class: "code" }, h("pre", { class: "log" }, code), copyButton(code, "Copy", "code-copy")));
      return;
    }
    for (const para of part.split(/\n{2,}/)) {
      if (!para.trim()) continue;
      const lines = para.trim().split("\n");
      const rows = lines.filter(l => l.trim().startsWith("|"));
      if (rows.length >= 2 && rows.length === lines.filter(l => l.trim()).length) { out.push(table(rows)); continue; }
      const head = /^#{1,6}\s+(.*)$/.exec(lines[0].trim());
      if (head) {
        out.push(h("p", { class: "heading" }, h("b", {}, head[1].replace(/\*\*/g, ""))));
        if (lines.length > 1) out.push(...rich(lines.slice(1).join("\n")));
        continue;
      }
      out.push(h("p", {}, para.trim().split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/).map(bit =>
        bit.startsWith("`") && bit.endsWith("`") && bit.length > 2 ? h("code", {}, bit.slice(1, -1))
        : bit.startsWith("**") && bit.endsWith("**") && bit.length > 4 ? h("b", {}, bit.slice(2, -2)) : bit)));
    }
  });
  return out;
}

// table shows a Markdown table (some models write them however they're asked).
function table(rows) {
  const cells = r => r.trim().replace(/^\||\|$/g, "").split("|").map(c => c.trim().replace(/\*\*/g, ""));
  const body = rows.filter(r => !/^\s*\|[\s:|-]+\|\s*$/.test(r)).map(cells);
  const [head, ...rest] = body;
  return h("div", { class: "list" }, h("table", {},
    h("thead", {}, h("tr", {}, head.map(c => h("th", {}, c)))),
    h("tbody", {}, rest.map(r => h("tr", {}, r.map(c => h("td", {}, c)))))));
}

function argsText(args) {
  const parts = Object.entries(args || {}).map(([k, v]) => k + ": " + (typeof v === "string" ? v : JSON.stringify(v)));
  const t = parts.join(", ");
  return t.length > 120 ? t.slice(0, 117) + "…" : t;
}

// copyButton copies text to the clipboard, and says so for a moment.
function copyButton(text, label, cls) {
  const b = h("button", { type: "button", class: "copy " + (cls || ""), title: "Copy to the clipboard" }, "⧉ " + label);
  b.addEventListener("click", async () => {
    let ok = false;
    try { await navigator.clipboard.writeText(text); ok = true; }
    catch {
      // Without clipboard access (an older browser, say): copy through a hidden text box.
      const t = h("textarea", { "aria-hidden": "true" });
      t.value = text; t.style.position = "fixed"; t.style.opacity = "0";
      document.body.append(t); t.select();
      try { ok = document.execCommand("copy"); } catch { /* not allowed */ }
      t.remove();
    }
    b.textContent = ok ? "✓ Copied" : "Couldn't copy";
    b.classList.toggle("done", ok);
    setTimeout(() => { b.textContent = "⧉ " + label; b.classList.remove("done"); }, 1600);
  });
  return b;
}

// chatPicture gives the address of picture n of turn i in the open conversation (each page sets it).
let chatPicture = null;

function turnView(t, i) {
  switch (t.role) {
    case "user": return h("div", { class: "msg user" },
      t.images && t.images.length && chatPicture ? h("div", { class: "msg-pics" }, t.images.map((im, n) =>
        h("a", { href: chatPicture(i, n), target: "_blank", rel: "noopener" }, h("img", { src: chatPicture(i, n), alt: im.name, loading: "lazy" })))) : null,
      t.files && t.files.length ? h("div", { class: "msg-files" }, t.files.map(f => h("span", { class: "file-chip" }, "📄 " + f))) : null,
      rich(t.text));
    case "note": return h("div", { class: "msg note" }, t.text);
    case "tools": return h("div", { class: "msg tools" }, t.results.map(r => h("details", {},
      h("summary", {}, h("span", { class: "pill " + (r.isError ? "failed" : "done") }, r.isError ? "didn't work" : "result"), " ", r.name),
      h("pre", { class: "log" }, r.text))));
    default: return h("div", { class: "msg bot" },
      t.text ? rich(t.text) : null,
      (t.calls || []).map(c => h("div", { class: "call small" }, "Uses ", h("b", {}, c.name), c.args && Object.keys(c.args).length ? h("span", { class: "muted" }, " (" + argsText(c.args) + ")") : null)),
      h("div", { class: "msg-foot" }, t.text ? copyButton(t.text, "Copy") : null, h("div", { class: "by" }, t.by || "")));
  }
}

// Chats follow new text while you're at the bottom of the page, and leave you be when you've scrolled up to read.
function nearBottom() {
  const el = document.scrollingElement;
  return el.scrollHeight - el.scrollTop - el.clientHeight < 120;
}
function toBottom() {
  const el = document.scrollingElement;
  el.scrollTop = el.scrollHeight;
}

// statusLine is what the assistant is doing, with how long it's been at it once that's more than a few seconds
// (a local model on a slow machine can read for minutes before it writes).
function statusLine(conv) {
  const s = conv.status || "Working…";
  const t = conv.statusFor || 0;
  if (t < 5) return s;
  return s + " " + (t < 60 ? t + " s" : Math.floor(t / 60) + ":" + String(t % 60).padStart(2, "0"));
}

function safeJSON(s) { try { return JSON.parse(s); } catch { return { "": s }; } }

// MARK: Pictures and files in a chat box

// attacher lets pictures and files go along with a message: drop them on zone, paste them into input, or pick them
// with the 📎 button. Big photos are shrunk first (a model doesn't need more than about 1600 pixels).
function attacher(zone, input) {
  const files = [];
  const strip = h("div", { class: "attach-strip", hidden: true });
  const picker = h("input", { type: "file", multiple: true, hidden: true,
    accept: "image/*,.pdf,.txt,.md,.csv,.tsv,.log,.json,.yaml,.yml,.xml,.conf,.cfg,.ini,.toml,.env,.sh,.py,.js,.ts,.go,.rs,.java,.c,.h,.cpp,.html,.css,.sql,text/*" });
  const button = h("button", { type: "button", class: "attach-btn", title: "Add pictures or files", "aria-label": "Add pictures or files", onclick: () => picker.click() }, "📎");
  picker.addEventListener("change", () => { add([...picker.files]); picker.value = ""; });
  let depth = 0;
  zone.addEventListener("dragenter", e => { if (hasFiles(e)) { depth++; zone.classList.add("dropping"); } });
  zone.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; zone.classList.remove("dropping"); } });
  zone.addEventListener("dragover", e => { if (hasFiles(e)) e.preventDefault(); });
  zone.addEventListener("drop", e => {
    if (!hasFiles(e)) return;
    e.preventDefault(); depth = 0; zone.classList.remove("dropping");
    add([...e.dataTransfer.files]);
  });
  input.addEventListener("paste", e => {
    const list = [...((e.clipboardData && e.clipboardData.files) || [])];
    if (list.length) { e.preventDefault(); add(list); }
  });
  function hasFiles(e) { return e.dataTransfer && [...e.dataTransfer.types].includes("Files"); }

  async function add(list) {
    for (const f of list) {
      if (files.length >= 8) { toast("Up to 8 pictures and files at a time"); break; }
      try { files.push(await read(f)); } catch (err) { toast(f.name + ": " + err.message); }
    }
    draw();
    input.focus();
  }
  async function read(f) {
    const picture = f.type.startsWith("image/");
    if (!picture && f.size > 15 * 1024 * 1024) throw new Error("too big (15 MB at most)");
    let blob = f, type = f.type;
    if (picture && f.type !== "image/gif") {
      const small = await shrink(f).catch(() => null);
      if (small) { blob = small; type = small.type; }
    }
    const data = await new Promise((ok, fail) => {
      const r = new FileReader();
      r.onload = () => ok(String(r.result).split(",")[1] || "");
      r.onerror = () => fail(new Error("couldn't read it"));
      r.readAsDataURL(blob);
    });
    return { name: f.name || (picture ? "picture" : "file"), type: type || "application/octet-stream", data,
      preview: picture ? URL.createObjectURL(blob) : null };
  }
  // shrink makes a photo at most 1600 pixels across as a JPEG; small pictures (screenshots) stay as they are.
  async function shrink(f) {
    const img = await createImageBitmap(f);
    const max = 1600, scale = Math.min(1, max / Math.max(img.width, img.height));
    if (scale === 1 && f.size < 1.5 * 1024 * 1024 && /png|jpeg|webp/.test(f.type)) return null;
    const c = document.createElement("canvas");
    c.width = Math.round(img.width * scale); c.height = Math.round(img.height * scale);
    c.getContext("2d").drawImage(img, 0, 0, c.width, c.height);
    return new Promise(ok => c.toBlob(b => ok(b), "image/jpeg", 0.85));
  }
  function draw() {
    strip.hidden = !files.length;
    strip.replaceChildren(...files.map((f, i) => h("span", { class: "attach-item" },
      f.preview ? h("img", { src: f.preview, alt: "" }) : h("span", { class: "attach-doc", "aria-hidden": "true" }, "📄"),
      h("span", { class: "attach-name" }, f.name),
      h("button", { type: "button", class: "attach-x", "aria-label": "Remove " + f.name, onclick: () => {
        if (files[i].preview) URL.revokeObjectURL(files[i].preview);
        files.splice(i, 1); draw();
      } }, "×"))));
  }
  return {
    button, strip, picker,
    count: () => files.length,
    list: () => files.map(({ name, type, data }) => ({ name, type, data })),
    clear() { for (const f of files) if (f.preview) URL.revokeObjectURL(f.preview); files.length = 0; draw(); },
  };
}

// MARK: How to reach this server

// reachCard shows the addresses to use (GET /api/me/reach): the portal, the AI page and apps, with the internet port,
// home-network fallbacks, file sharing and, for admins, SSH and the router's part.
function reachCard(x, open) {
  const row = (label, value, extra) => h("div", { class: "reach-row" },
    h("span", { class: "reach-label" }, label),
    h("span", { class: "reach-value" }, value, extra || null));
  const link = l => [h("a", { href: l.url, target: "_blank", rel: "noopener" }, l.url.replace(/^https:\/\//, "").replace(/\/$/, "")),
    " ", h("span", { class: "pill " + (l.internet ? "internet" : "home") }, l.internet ? "home + internet" : "home only"),
    copyButton(l.url, "Copy")];
  const code = t => [h("code", {}, t), copyButton(t, "Copy")];
  const a = x.admin;
  return h("details", { class: "card reach", open: !!open },
    h("summary", {}, h("b", {}, "How to reach this server"),
      x.port ? h("span", { class: "muted small" }, " · from the internet, addresses end in :" + x.port) : null),
    h("div", { class: "stack" },
      x.port ? h("p", { class: "small muted" }, "The router's port 443 goes to another machine, so this server is reached through port " + x.port +
        ": keep the :" + x.port + " in addresses (the links here have it).") : null,
      row("Portal", link(x.portal)),
      x.ai ? row("AI page", link(x.ai)) : null,
      x.apps.length ? h("details", { class: "reach-apps" }, h("summary", { class: "small" }, x.apps.length + " app" + (x.apps.length > 1 ? "s" : "")),
        x.apps.map(l => row(l.name, link(l)))) : null,
      x.local.length ? row("At home, by address", x.local.map(u => h("span", { class: "reach-item" }, code(u))),
        h("div", { class: "muted small" }, "When a name won't open at home. The browser warns about the certificate there: that's expected.")) : null,
      x.files && x.files.length ? row("Shared folders", x.files.map(u => h("span", { class: "reach-item" }, code(u))),
        h("div", { class: "muted small" }, "Mac: Finder → Go → Connect to Server. Windows: File Explorer's address bar.")) : null,
      a ? [
        h("h3", { class: "reach-admin" }, "For admins"),
        row("Admin panel", [h("a", { href: a.adminURL }, a.adminURL.replace(/^https:\/\//, "")), copyButton(a.adminURL, "Copy")]),
        row("SSH (terminal)", code(a.ssh), h("div", { class: "muted small" }, "Port " + a.sshPort + (a.sshPort === 22 ? " (the usual one)" : "") + ". Run it on your computer, then the server's commands at its prompt.")),
        a.publicIP ? row("Home's internet address", code(a.publicIP)) : null,
        a.router ? row("Router", h("span", {}, a.router), h("div", { class: "muted small" }, "The port forwarding that brings the internet here.")) : null,
      ] : null));
}
