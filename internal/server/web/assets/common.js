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

function turnView(t) {
  switch (t.role) {
    case "user": return h("div", { class: "msg user" }, rich(t.text));
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
