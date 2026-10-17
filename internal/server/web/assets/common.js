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
    if (i % 2) { out.push(h("pre", { class: "log" }, part.replace(/^[a-z0-9+-]*\n/i, "").replace(/\n$/, ""))); return; }
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
      h("div", { class: "by" }, t.by || ""));
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
