// Homeport sign-in: passkeys (Face ID, Touch ID, a security key), alone or as the second step after a password.
"use strict";

const b64 = s => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(s.length / 4) * 4, "=")), c => c.charCodeAt(0));
const unb64 = buf => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function passkeySignIn(button) {
  const say = msg => {
    let p = document.querySelector(".error");
    if (!p) { p = document.createElement("p"); p.className = "error"; p.setAttribute("role", "alert"); button.closest("form").querySelector("h1").after(p); }
    p.textContent = msg;
  };
  button.disabled = true;
  try {
    const ret = document.querySelector('input[name="return"]')?.value || "";
    const begin = await fetch("/login/passkey/begin", { method: "POST", credentials: "same-origin" });
    const opts = await begin.json();
    if (!begin.ok) throw new Error(opts.error || "Passkeys aren't available here.");
    const pk = opts.publicKey;
    pk.challenge = b64(pk.challenge);
    (pk.allowCredentials || []).forEach(c => { c.id = b64(c.id); });
    const cred = await navigator.credentials.get({ publicKey: pk });
    const body = {
      id: cred.id, rawId: unb64(cred.rawId), type: cred.type,
      response: {
        clientDataJSON: unb64(cred.response.clientDataJSON), authenticatorData: unb64(cred.response.authenticatorData),
        signature: unb64(cred.response.signature), userHandle: cred.response.userHandle ? unb64(cred.response.userHandle) : null,
      },
    };
    const fin = await fetch("/login/passkey/finish?return=" + encodeURIComponent(ret), { method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    const out = await fin.json();
    if (!fin.ok) throw new Error(out.error || "That passkey didn't work.");
    location.href = out.go || "/";
  } catch (err) {
    say(err.name === "NotAllowedError" ? "The passkey was cancelled, or timed out." : (err.message.charAt(0).toUpperCase() + err.message.slice(1)));
    button.disabled = false;
  }
}

const pkButton = document.getElementById("passkey");
if (pkButton && window.PublicKeyCredential && !/^\d+\.\d+\.\d+\.\d+$|^\[/.test(location.hostname)) {
  pkButton.hidden = false;
  pkButton.addEventListener("click", () => passkeySignIn(pkButton));
} else if (pkButton) {
  pkButton.hidden = true;
}
