// Homeport's AI page (ai.<main domain>): a chat with the local model for everyone signed in; admins get the full
// assistant (tools, approvals, Claude). The model's name and what it can do sit above the message box, the model picker
// below it.
"use strict";

const $ = id => document.getElementById(id);
const els = {
  hero: $("hero"), status: $("status"), name: $("name"), desc: $("desc"), smallName: $("small-name"),
  thread: $("thread"), live: $("live"), approvals: $("approvals"), form: $("form"), input: $("input"),
  send: $("send"), stop: $("stop"), model: $("model"), chips: $("chips"), convs: $("convs"), menu: $("menu"),
};
const state = { me: null, info: null, conv: null, renderedTurns: -1, timer: null, loading: false };
const PICK = "homeport-ai-model"; // the model last chosen, per browser

async function api(method, path, body) {
  const res = await fetch("/api/ai" + path, {
    method,
    headers: { "X-Homeport": "1", ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  if (res.status === 401) { location.reload(); throw new Error("Signed out"); }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function toast(msg) {
  const t = $("toast");
  t.textContent = msg; t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, 4000);
}

function remembered() { try { return localStorage.getItem(PICK) || ""; } catch { return ""; } }
function remember(v) { try { localStorage.setItem(PICK, v); } catch { /* private window */ } }

// MARK: The model: name and description above the box, the picker below it

async function loadModel() {
  const pick = els.model.value || remembered();
  const local = pick && pick !== "claude" ? pick : "";
  const info = await api("GET", "/model" + (local ? "?model=" + encodeURIComponent(local) : ""));
  state.info = info;
  drawPicker(info, pick);
  drawHero();
  if (info.loaded === false) warm();
}

function drawPicker(info, pick) {
  const opts = [];
  if (state.me.admin) opts.push(h("option", { value: "" }, "Automatic (as in Settings)"));
  for (const c of info.choices || []) opts.push(h("option", { value: c.id }, c.name));
  if (info.claude) opts.push(h("option", { value: "claude" }, "Claude (Anthropic's servers)"));
  if (state.me.admin) opts.push(h("option", { value: "find" }, "Find more models…"));
  els.model.replaceChildren(...opts);
  const values = opts.map(o => o.value).filter(v => v !== "find");
  els.model.value = values.includes(pick) ? pick : (state.me.admin ? "" : info.default || values[0] || "");
  els.model.disabled = opts.length < 2;
}

function drawHero() {
  const info = state.info, pick = els.model.value;
  let name, desc, status = null;
  if (pick === "claude") {
    name = "Claude";
    desc = "Anthropic's model, answering on Anthropic's servers: for harder jobs and troubleshooting. It uses this server's API key and can look after the server with Homeport's tools.";
  } else if (!info.configured || !info.model) {
    name = "No local AI model yet";
    desc = state.me.admin
      ? "Set one up under Settings → Assistant chat in the admin panel, or run the installer again." + (info.claude ? " Claude can answer meanwhile: choose it below." : "")
      : "An admin can set one up on this server. Ask them!";
  } else {
    name = info.name;
    desc = info.description;
    if (state.me.admin && pick === "") desc += " Automatic: harder questions go to Claude when there's an API key.";
    if (state.loading || info.loaded === false) status = ["loading", "Loading the model…"];
    else if (info.loaded === true) status = ["ready", "Ready"];
  }
  els.name.textContent = name;
  els.desc.textContent = desc;
  els.smallName.textContent = name;
  els.status.hidden = !status;
  if (status) els.status.replaceChildren(h("span", { class: "dot " + status[0], "aria-hidden": "true" }), status[1]);
}

// warm has the model loaded now, so the first answer doesn't wait for it.
async function warm() {
  if (state.loading) return;
  state.loading = true; drawHero();
  const model = els.model.value && els.model.value !== "claude" ? els.model.value : "";
  try { await api("POST", "/model/load" + (model ? "?model=" + encodeURIComponent(model) : "")); }
  catch (err) { toast("Couldn't load the model: " + err.message); }
  state.loading = false;
  try { await loadModel(); } catch { drawHero(); }
}

// MARK: Conversations

async function drawConvs() {
  const list = await api("GET", "/chat").catch(() => []);
  els.convs.replaceChildren(
    h("button", { type: "button", class: "ai-new", onclick: () => open(null) }, "+ New chat"),
    ...(list.length ? [h("p", { class: "muted small" }, "Recent")] : []),
    ...list.map(c => h("button", { type: "button", class: state.conv && c.id === state.conv.id ? "current" : "", title: c.title,
      onclick: () => open(c.id) }, c.title)));
}

async function open(id) {
  clearTimeout(state.timer);
  state.conv = null; state.renderedTurns = -1;
  document.body.classList.remove("menu-open"); els.menu.setAttribute("aria-expanded", "false");
  if (id) {
    try { state.conv = await api("GET", "/chat/" + id); } catch (err) { toast(err.message); }
  }
  draw();
  drawConvs();
  if (state.conv && state.conv.busy) poll();
  els.input.focus();
}

function suggestions() {
  const ask = text => () => { els.input.value = text; grow(); els.input.focus(); };
  const list = state.me.admin
    ? ["What's installed on this server, and is everything healthy?", "Explain how DNS works, simply", "Help me write a short, friendly email"]
    : ["Explain how DNS works, simply", "Help me write a short, friendly email", "Give me a quick dinner idea"];
  return list.map(t => h("button", { type: "button", onclick: ask(t) }, t));
}

function draw() {
  const conv = state.conv;
  const turns = conv ? conv.turns : [];
  document.body.classList.toggle("talking", turns.length > 0);
  els.chips.replaceChildren(...(turns.length ? [] : suggestions()));
  const follow = nearBottom() || turns.length !== state.renderedTurns;
  if (turns.length !== state.renderedTurns) {
    els.thread.replaceChildren(...turns.map(turnView));
    state.renderedTurns = turns.length;
  }
  const bits = [];
  if (conv && conv.partial) bits.push(h("div", { class: "msg bot" }, rich(conv.partial)));
  if (conv && conv.busy) bits.push(h("p", { class: "muted small status" }, statusLine(conv)));
  if (conv && conv.consent) bits.push(h("div", { class: "card stack consent" },
    h("p", {}, "This would go to Claude, on Anthropic's servers", conv.consentWhy ? " (" + conv.consentWhy + ")" : "", ". Send it?"),
    h("div", { class: "row" },
      h("button", { class: "primary", type: "button", onclick: () => cont({ allowCloud: true }) }, "Send to Claude"),
      conv.canLocal ? h("button", { type: "button", onclick: () => cont({ use: "local" }) }, "Use the local model") : null)));
  els.live.replaceChildren(...bits);
  const busy = !!(conv && conv.busy);
  els.send.disabled = busy; els.stop.hidden = !busy;
  if (follow && turns.length) toBottom();
}

async function cont(body) {
  try { state.conv = await api("POST", "/chat/" + state.conv.id + "/continue", body); draw(); poll(); } catch (err) { toast(err.message); }
}

async function drawApprovals() {
  if (!state.me.admin) return;
  const pending = await api("GET", "/approvals").catch(() => []);
  els.approvals.replaceChildren(...pending.map(p => h("div", { class: "card approval" },
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
  clearTimeout(state.timer);
  if (!state.conv) return;
  try {
    state.conv = await api("GET", "/chat/" + state.conv.id);
    draw();
    await drawApprovals();
  } catch (err) { toast(err.message); return; }
  if (state.conv.busy) state.timer = setTimeout(poll, 900);
  else { drawConvs(); if (state.info && state.info.loaded === false) loadModel().catch(() => {}); }
}

// MARK: Sending

function grow() {
  els.input.style.height = "auto";
  els.input.style.height = Math.min(els.input.scrollHeight, 240) + "px";
}

els.form.addEventListener("submit", async e => {
  e.preventDefault();
  const text = els.input.value.trim();
  if (!text) return;
  els.send.disabled = true;
  try {
    const conv = await api("POST", "/chat", { conversation: state.conv ? state.conv.id : "", text, model: els.model.value });
    const isNew = !state.conv || state.conv.id !== conv.id;
    state.conv = conv; els.input.value = ""; grow();
    draw();
    if (isNew) drawConvs();
    poll();
  } catch (err) { toast(err.message); els.send.disabled = false; }
});
els.input.addEventListener("keydown", e => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); els.form.requestSubmit(); }
});
els.input.addEventListener("input", grow);
els.stop.addEventListener("click", async () => {
  try { await api("POST", "/chat/" + state.conv.id + "/stop"); } catch (err) { toast(err.message); }
});
els.model.addEventListener("change", () => {
  if (els.model.value === "find") {
    els.model.value = remembered();
    window.open("https://" + (state.me.mainAddress || state.me.mainDomain) + "/admin#models", "_blank", "noopener");
    return;
  }
  remember(els.model.value);
  loadModel().catch(err => toast(err.message));
});
els.menu.addEventListener("click", () => {
  const openNow = document.body.classList.toggle("menu-open");
  els.menu.setAttribute("aria-expanded", String(openNow));
});

// MARK: The live meter: processor, graphics and memory, about once a second while the page is in view.

function meter(label, pct, title) {
  const p = Math.max(0, Math.min(100, pct | 0));
  return h("span", { class: "meter", title },
    h("span", { class: "meter-label" }, label),
    h("span", { class: "meter-bar", "aria-hidden": "true" }, h("span", { class: "meter-fill" + (p > 85 ? " hot" : "") + " w" + Math.round(p / 5) * 5 })),
    h("span", { class: "meter-num" }, p + "%"));
}

async function busy() {
  if (document.visibilityState === "visible") {
    try {
      const l = await api("GET", "/busy");
      const gb = n => (n / 1073741824).toFixed(1);
      const bits = [meter("CPU", l.cpu, l.cores + " cores")];
      if (l.gpu) bits.push(meter("GPU", l.gpu.percent, l.gpu.name + (l.gpu.memTotal ? ": " + gb(l.gpu.memUsed) + " of " + gb(l.gpu.memTotal) + " GB" : "")));
      if (l.memTotal) bits.push(meter("RAM", l.memUsed * 100 / l.memTotal, gb(l.memUsed) + " of " + gb(l.memTotal) + " GB in use"));
      $("busy").replaceChildren(...bits);
      $("busy").hidden = false;
    } catch { /* signed out, or the server's busy: try again next time */ }
  }
  setTimeout(busy, state.conv && state.conv.busy ? 1000 : 2000);
}

(async () => {
  busy();
  try {
    state.me = await api("GET", "/me");
    $("domain").textContent = state.me.mainDomain;
    $("home").href = "https://" + (state.me.mainAddress || state.me.mainDomain) + "/";
    $("me").textContent = state.me.displayName || state.me.name;
    document.title = "AI · " + state.me.mainDomain;
    if (state.me.admin) { const a = $("admin"); a.href = "https://" + (state.me.mainAddress || state.me.mainDomain) + "/admin"; a.hidden = false; }
    await loadModel();
  } catch (err) { toast(err.message); }
  draw();
  drawConvs();
  els.input.focus();
})();
