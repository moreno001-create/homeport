// Homeport's colors and text size on this device: applied before the page draws (this file loads first, not deferred).
// They're kept in a cookie for the whole home name, so the portal, the admin panel and the AI page agree.
"use strict";
(function () {
  const get = name => (document.cookie.match(new RegExp("(?:^|; )" + name + "=([a-z-]+)")) || [])[1] || "";
  const root = document.documentElement;
  const theme = get("hp_theme"), text = get("hp_text");
  if (theme) root.dataset.theme = theme;
  if (text) root.dataset.text = text;
  // setLook saves a choice ("" for automatic / normal) and applies it at once.
  window.setLook = (name, value) => {
    const host = location.hostname, ip = /^[\d.]+$|:/.test(host);
    const domain = ip ? "" : "; domain=" + host.replace(/^ai\./, "");
    document.cookie = name + "=" + value + domain + "; path=/; max-age=" + (value ? 31536000 : 0) + "; samesite=lax; secure";
    const key = name === "hp_theme" ? "theme" : "text";
    if (value) root.dataset[key] = value; else delete root.dataset[key];
  };
  window.currentLook = () => ({ theme: get("hp_theme"), text: get("hp_text") });
})();
