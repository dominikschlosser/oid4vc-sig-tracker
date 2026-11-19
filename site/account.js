"use strict";

// Shows the GitHub account in the header and resolves to { login, bot, botDown, mine } as soon as the login is known.
// login is null for visitors who are signed out.
// bot is the login of the tracker's bot account when this person may post through it, else null.
// botDown says why a person on the bot's list can't use it right now.
// mine resolves later to the sets { involved, requested }, because it waits for GitHub search.
// Hosts without the post service answer 404. Then the account stays hidden and this resolves to null.
window.account = (async () => {
  const box = document.getElementById("account");
  if (!box) return null;
  const $ = (id) => document.getElementById(id);
  const root = box.dataset.root;
  let res;
  try {
    res = await fetch(`${root}api/me`, { credentials: "same-origin" });
  } catch {
    return null;
  }
  const out = { login: null, bot: null, botDown: null, mine: Promise.resolve({ involved: new Set(), requested: new Set() }) };
  if (res.ok) {
    const me = await res.json();
    out.login = me.login;
    out.bot = me.bot || null;
    out.botDown = me.botDown || null;
    $("me-login").textContent = out.login;
    $("me-avatar").src = `https://avatars.githubusercontent.com/${encodeURIComponent(out.login)}?s=48`;
    $("me-menu").hidden = false;
    for (const a of $("me-menu").querySelectorAll("a")) a.addEventListener("click", () => ($("me-menu").open = false));
    $("signout").addEventListener("click", async () => {
      await fetch(`${root}auth/logout`, { method: "POST", credentials: "same-origin" }).catch(() => {});
      location.reload();
    });
    out.mine = fetch(`${root}api/mine`, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : { involved: [], requested: [] }))
      .then((m) => ({ involved: new Set(m.involved), requested: new Set(m.requested) }))
      // The GitHub search failed, so only "Opened by me" finds items.
      .catch(() => ({ involved: new Set(), requested: new Set() }));
  } else if (res.status === 401) {
    const signIn = $("signin");
    signIn.href = `${root}auth/login?return=${encodeURIComponent(location.pathname)}`;
    signIn.hidden = false;
    $("signin-why").hidden = false;
    // GitHub sends people back without the hash, so the list filters wait in sessionStorage.
    signIn.addEventListener("click", () => {
      try { sessionStorage.setItem("return-hash", location.hash); } catch { /* the filters are lost, nothing else */ }
    });
  } else {
    return null;
  }
  box.hidden = false;
  return out;
})();
