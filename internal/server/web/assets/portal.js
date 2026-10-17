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
      d.rain >= 30 ? h("span", { class: "w-rain small" }, "💧" + d.rain + "%") : null))));
  box.dataset.units = f.units;
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
