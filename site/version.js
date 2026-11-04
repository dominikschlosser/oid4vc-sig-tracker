"use strict";

// Asks the reader to reload when a deploy changed the page or its data while it was open.
(() => {
  const url = new URL("version.txt", document.currentScript.src);
  const get = () => fetch(url, { cache: "no-store" }).then((r) => (r.ok ? r.text() : null)).catch(() => null);
  let loaded = null;
  const check = async () => {
    const now = await get();
    if (!now || !loaded || now === loaded || document.getElementById("reload-bar")) return;
    const bar = document.createElement("div");
    bar.id = "reload-bar";
    bar.className = "reload-bar";
    const button = Object.assign(document.createElement("button"), { type: "button", className: "btn", textContent: "Reload" });
    button.addEventListener("click", () => location.reload());
    bar.append("The tracker has been updated since you opened this page.", button);
    document.body.append(bar);
  };
  get().then((v) => (loaded = v));
  setInterval(check, 5 * 60 * 1000);
  document.addEventListener("visibilitychange", () => document.visibilityState === "visible" && check());
})();
