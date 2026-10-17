// Homeport portal: the weather where you are (your browser's location, or a ZIP code or town you type).
"use strict";

const box = document.getElementById("weather");

async function api(method, path, body) {
  const res = await fetch("/api/me" + path, {
    method,
    headers: { "X-Homeport": "1", ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// The browser's language guesses the units until you pick (°F in the US).
const guess = /-US$|^en$/i.test(navigator.language || "") ? "f" : "c";

// where asks the browser where it is (it asks you the first time); null when it can't or you said no.
function where() {
  return new Promise(ok => {
    if (!navigator.geolocation) return ok(null);
    navigator.geolocation.getCurrentPosition(p => ok(p.coords), () => ok(null), { timeout: 10000, maximumAge: 30 * 60 * 1000 });
  });
}

async function load(askBrowser) {
  let x;
  try {
    x = await api("GET", "/weather?units=" + guess);
    if (x.needPlace && x.auto && askBrowser) {
      const c = await where();
      if (c) x = await api("GET", "/weather?units=" + guess + "&lat=" + c.latitude.toFixed(2) + "&lon=" + c.longitude.toFixed(2));
    }
  } catch (err) {
    box.hidden = false;
    box.replaceChildren(h("p", { class: "muted small" }, "Weather: " + err.message), changeButton());
    return;
  }
  if (x.off) {
    box.hidden = false; box.className = "weather off";
    box.replaceChildren(h("button", { type: "button", class: "w-change", onclick: async () => {
      try { await api("PUT", "/weather", { off: false }); box.className = "weather"; await load(true); } catch (err) { box.textContent = err.message; }
    } }, "🌤️ Show the weather"));
    return;
  }
  box.hidden = false;
  if (x.needPlace) {
    box.replaceChildren(h("div", { class: "w-ask" },
      h("span", { "aria-hidden": "true" }, "🌤️"),
      h("span", {}, "Weather for where you are: allow your location, or type a ZIP code."), placeForm(true)));
    return;
  }
  draw(x);
}

function deg(n) { return Math.round(n) + "°"; }
function dayName(iso, i) {
  if (i === 0) return "Today";
  return new Date(iso + "T12:00:00").toLocaleDateString(undefined, { weekday: "short" });
}

function draw(x) {
  const f = x.forecast, n = f.now, today = f.days[0];
  box.replaceChildren(
    h("div", { class: "w-now" },
      h("span", { class: "w-icon", "aria-hidden": "true" }, n.icon),
      h("div", {},
        h("div", { class: "w-temp" }, deg(n.temp), h("span", { class: "w-unit" }, f.units === "f" ? "F" : "C")),
        h("div", { class: "w-text" }, n.text, today ? " · H " + deg(today.high) + " L " + deg(today.low) : "")),
      h("div", { class: "w-where" },
        h("div", {}, (x.auto ? "📍 " : "") + f.place.name),
        h("div", { class: "muted small" }, "Feels " + deg(n.feelsLike) + " · " + n.humidity + "% humidity · wind " + Math.round(n.wind) + (f.units === "f" ? " mph" : " km/h")),
        changeButton())),
    h("ol", { class: "w-days" }, f.days.slice(1, 6).map((d, i) => h("li", { title: d.text + (d.rain ? ", " + d.rain + "% chance of rain" : "") },
      h("span", { class: "muted small" }, dayName(d.date, i + 1)),
      h("span", { class: "w-dicon", "aria-hidden": "true" }, d.icon),
      h("span", { class: "small" }, h("b", {}, deg(d.high)), " ", h("span", { class: "muted" }, deg(d.low))),
      d.rain >= 30 ? h("span", { class: "w-rain small" }, "💧" + d.rain + "%") : null))),
    morePlaces(x));
  box.dataset.units = f.units;
}

// clockAt is the time now in a time zone (America/New_York), the way this browser writes times.
function clockAt(tz) {
  try { return new Date().toLocaleTimeString([], { timeZone: tz, hour: "numeric", minute: "2-digit" }); } catch { return ""; }
}

// morePlaces are up to two more places under the forecast: now, today's high and low, and their local time.
function morePlaces(x) {
  const list = x.more || [];
  const rows = list.map(o => {
    const f = o.forecast, today = f.days[0];
    return h("li", {},
      h("span", { class: "w-micon", "aria-hidden": "true" }, f.now.icon),
      h("span", { class: "w-mname" }, f.place.name),
      h("span", { class: "w-mtime muted small", "data-tz": f.timezone || "" }, f.timezone ? clockAt(f.timezone) : ""),
      h("span", { class: "w-mtemp" }, h("b", {}, deg(f.now.temp)), today ? h("span", { class: "muted small" }, " " + deg(today.high) + " / " + deg(today.low)) : null),
      h("button", { type: "button", class: "attach-x", "aria-label": "Remove " + f.place.name, onclick: async () => {
        try { await api("PUT", "/weather", { remove: o.index }); load(false); } catch (err) { alert(err.message); }
      } }, "×"));
  });
  const room = 1 + list.length < (x.max || 3);
  const addBtn = h("button", { type: "button", class: "w-change", onclick: () => {
    const input = h("input", { placeholder: "ZIP code or town", "aria-label": "Another place" });
    input.addEventListener("input", () => input.setCustomValidity(""));
    const form = h("form", { class: "w-form", onsubmit: async e => {
      e.preventDefault();
      if (!input.value.trim()) return;
      try { await api("PUT", "/weather", { add: input.value.trim() }); load(false); }
      catch (err) { input.setCustomValidity(err.message); input.reportValidity(); }
    } }, input, h("button", { class: "primary", type: "submit" }, "Add"), h("button", { type: "button", onclick: () => form.replaceWith(addBtn) }, "Cancel"));
    addBtn.replaceWith(form); input.focus();
  } }, "+ Add a place");
  if (!rows.length && !room) return null;
  return h("div", { class: "w-more" }, rows.length ? h("ul", {}, rows) : null, room ? addBtn : null);
}

function changeButton() {
  return h("button", { type: "button", class: "w-change", onclick: () => {
    box.replaceChildren(h("div", { class: "w-ask" }, placeForm(false)));
    box.querySelector("input").focus();
  } }, "Change");
}

function placeForm(first) {
  const input = h("input", { placeholder: "ZIP code or town", "aria-label": "ZIP code or town", autocomplete: "postal-code", inputmode: "text" });
  const units = h("select", { "aria-label": "Units" }, h("option", { value: "f" }, "°F"), h("option", { value: "c" }, "°C"));
  units.value = box.dataset.units || guess;
  const save = async body => {
    try { await api("PUT", "/weather", body); await load(true); } catch (err) { input.setCustomValidity(err.message); input.reportValidity(); }
  };
  input.addEventListener("input", () => input.setCustomValidity(""));
  return h("form", { class: "w-form", onsubmit: e => {
    e.preventDefault();
    const q = input.value.trim();
    save(q ? { query: q, units: units.value } : { units: units.value });
  } },
    input, units,
    h("button", { class: "primary", type: "submit" }, "Show"),
    h("button", { type: "button", onclick: () => save({ auto: true, units: units.value }) }, "📍 My location"),
    first ? null : h("button", { type: "button", onclick: () => load(false) }, "Cancel"),
    first ? null : h("button", { type: "button", class: "w-hide", onclick: () => save({ off: true }) }, "Hide weather"));
}

if (box) load(true);

// How to reach this server, at the bottom of the portal.
api("GET", "/reach").then(x => {
  const spot = document.getElementById("reach");
  if (spot) spot.replaceChildren(reachCard(x, false));
}).catch(() => { /* signed out, or an older server */ });

// MARK: Your links (right of the weather)

const linksBox = document.getElementById("links");
let links = [], editing = false;

function host(u) { try { return new URL(u).hostname.replace(/^www\./, ""); } catch { return ""; } }

function linkIcon(l) {
  const glyph = h("span", { class: "link-glyph", "aria-hidden": "true" }, (l.name || "?")[0].toUpperCase());
  const img = h("img", { src: "/api/me/icon?host=" + encodeURIComponent(host(l.url)), alt: "", loading: "lazy" });
  img.addEventListener("error", () => img.replaceWith(glyph));
  return img;
}

function drawLinks() {
  const add = h("form", { class: "link-add", onsubmit: async e => {
    e.preventDefault();
    const next = [...links, { name: name.value.trim(), url: url.value.trim() }];
    try { links = await api("PUT", "/links", next); editing = true; drawLinks(); linksBox.querySelector(".link-add input[name=url]").focus(); }
    catch (err) { url.setCustomValidity(err.message); url.reportValidity(); }
  } });
  const url = h("input", { name: "url", placeholder: "Address (netflix.com)", "aria-label": "Address", autocapitalize: "none", spellcheck: "false", inputmode: "url", required: true });
  const name = h("input", { name: "name", placeholder: "Name (optional)", "aria-label": "Name" });
  url.addEventListener("input", () => url.setCustomValidity(""));
  add.append(url, name, h("button", { class: "primary", type: "submit" }, "Add"));
  linksBox.replaceChildren(...[
    h("div", { class: "links-head" }, h("b", {}, "Your links"),
      links.length ? h("button", { type: "button", class: "w-change", onclick: () => { editing = !editing; drawLinks(); } }, editing ? "Done" : "Edit") : null),
    links.length ? h("div", { class: "links-list" }, links.map((l, i) => h("span", { class: "link-item" },
      h("a", { href: l.url, title: l.url }, linkIcon(l), h("span", {}, l.name)),
      editing ? h("button", { type: "button", class: "attach-x", "aria-label": "Remove " + l.name, onclick: async () => {
        try { links = await api("PUT", "/links", links.filter((_, j) => j !== i)); drawLinks(); } catch (err) { alert(err.message); }
      } }, "×") : null))) : h("p", { class: "muted small" }, "Keep the sites you use here: your bank, Netflix, a shop…"),
    editing || !links.length ? add : null].filter(Boolean));
}

if (linksBox) api("GET", "/links").then(x => { links = x; drawLinks(); }).catch(() => linksBox.remove());

// MARK: News (under the weather)

const newsBox = document.getElementById("news");

function ago(t) {
  const m = Math.round((Date.now() - new Date(t)) / 60000);
  if (!t || isNaN(m) || m < 0) return "";
  if (m < 60) return m + " min ago";
  if (m < 48 * 60) return Math.round(m / 60) + " h ago";
  return Math.round(m / 1440) + " days ago";
}

async function loadNews(source) {
  let x;
  try { x = await api("GET", "/news" + (source ? "?source=" + source : "")); } catch { newsBox.hidden = true; return; }
  newsBox.hidden = false;
  newsBox.replaceChildren(
    h("div", { class: "news-head" }, h("b", {}, "News"),
      h("div", { class: "news-tabs", role: "tablist" }, x.sources.map(src => h("button", { type: "button", role: "tab",
        "aria-selected": src.id === x.source ? "true" : "false", class: src.id === x.source ? "current" : "",
        onclick: () => loadNews(src.id) }, src.name)))),
    x.error ? h("p", { class: "muted small" }, x.error) : h("ol", { class: "news-list" }, x.items.map(it => h("li", {},
      h("a", { href: it.link, target: "_blank", rel: "noopener noreferrer" }, it.title),
      h("span", { class: "muted small" }, ago(it.time))))));
}

if (newsBox) loadNews("");

// MARK: World clock (under the weather)

const clocksBox = document.getElementById("clocks");
let zones = [], maxClocks = 6, editingClocks = false;

function cityOf(tz) { return tz.split("/").pop().replace(/_/g, " "); }

// offset says how far a zone is from here ("+3 h", "−8 h 30", "same time").
function offset(tz) {
  const now = new Date();
  const there = new Date(now.toLocaleString("en-US", { timeZone: tz }));
  const here = new Date(now.toLocaleString("en-US"));
  const mins = Math.round((there - here) / 60000);
  if (!mins) return "same time";
  const sign = mins > 0 ? "+" : "−", a = Math.abs(mins);
  return sign + Math.floor(a / 60) + " h" + (a % 60 ? " " + (a % 60) : "");
}
function dayThere(tz) {
  const fmt = d => d.toLocaleDateString("en-CA", { timeZone: tz });
  const here = new Date().toLocaleDateString("en-CA");
  const there = fmt(new Date());
  return there > here ? "Tomorrow" : there < here ? "Yesterday" : "Today";
}

function drawClocks() {
  const here = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const card = (tz, label, removable, i) => h("div", { class: "clock" + (label ? " here" : "") },
    h("span", { class: "clock-city" }, label || cityOf(tz)),
    h("span", { class: "clock-time", "data-tz": tz }, clockAt(tz)),
    h("span", { class: "muted small" }, label ? new Date().toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" }) : dayThere(tz) + " · " + offset(tz)),
    removable ? h("button", { type: "button", class: "attach-x", "aria-label": "Remove " + cityOf(tz), onclick: () => save(zones.filter((_, j) => j !== i)) }, "×") : null);
  const save = async next => {
    try { const x = await api("PUT", "/clocks", { zones: next }); zones = x.zones; drawClocks(); } catch (err) { alert(err.message); }
  };
  let all = [];
  try { all = Intl.supportedValuesOf("timeZone"); } catch { /* older browser: type it */ }
  const list = h("datalist", { id: "zone-list" }, all.map(z => h("option", { value: z }, cityOf(z))));
  const input = h("input", { list: "zone-list", placeholder: "City or time zone (Paris, Asia/Tokyo)", "aria-label": "City or time zone" });
  input.addEventListener("input", () => input.setCustomValidity(""));
  const add = h("form", { class: "w-form clock-add", onsubmit: e => {
    e.preventDefault();
    const v = input.value.trim().toLowerCase();
    const z = all.find(t => t.toLowerCase() === v) || all.find(t => cityOf(t).toLowerCase() === v) || all.find(t => t.toLowerCase().includes(v.replace(/ /g, "_")));
    if (!z && all.length) { input.setCustomValidity("Pick a city from the list"); input.reportValidity(); return; }
    save([...zones, z || input.value.trim()]);
  } }, input, list, h("button", { class: "primary", type: "submit" }, "Add"));
  clocksBox.hidden = false;
  clocksBox.replaceChildren(...[
    h("div", { class: "clocks-head" }, h("b", {}, "World clock"),
      h("button", { type: "button", class: "w-change", onclick: () => { editingClocks = !editingClocks; drawClocks(); } }, editingClocks ? "Done" : "Edit")),
    h("div", { class: "clock-row" }, card(here, "Here", false), zones.map((z, i) => card(z, "", editingClocks, i))),
    editingClocks && zones.length < maxClocks ? add : null].filter(Boolean));
}

// Every clock (and the other places' local times) ticks along.
setInterval(() => {
  for (const el of document.querySelectorAll("[data-tz]")) if (el.dataset.tz) el.textContent = clockAt(el.dataset.tz);
}, 15000);

if (clocksBox) api("GET", "/clocks").then(x => { zones = x.zones; maxClocks = x.max; drawClocks(); }).catch(() => clocksBox.remove());
