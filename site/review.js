"use strict";

// The post panel stays hidden on hosts without the post service, such as a self-hosted static copy.
(async () => {
  const panel = document.getElementById("post");
  if (!panel) return;
  const $ = (id) => document.getElementById(id);
  const picks = [...document.querySelectorAll(".pick input")];
  const sevBoxes = [...panel.querySelectorAll("input[data-sev]")];
  const msg = $("post-msg");
  let login;

  let me;
  try {
    me = await fetch("../api/me", { credentials: "same-origin" });
  } catch {
    return;
  }
  if (me.status === 401) {
    const signIn = $("post-login");
    signIn.href = `../auth/login?return=${encodeURIComponent(location.pathname)}`;
    signIn.hidden = false;
  } else if (me.ok) {
    login = (await me.json()).login;
    $("post-user").textContent = `Signed in as ${login}`;
    $("post-send").hidden = false;
    $("post-logout").hidden = false;
  } else {
    return;
  }
  panel.hidden = false;
  document.querySelectorAll(".pick").forEach((p) => (p.hidden = false));

  const update = () => {
    const n = picks.filter((p) => p.checked).length;
    $("post-send").textContent = n ? `Post ${n} ${n === 1 ? "comment" : "comments"} as ${login}` : `Post the summary as ${login}`;
  };
  sevBoxes.forEach((box) =>
    box.addEventListener("change", () => {
      picks.filter((p) => p.dataset.sev === box.dataset.sev).forEach((p) => (p.checked = box.checked));
      update();
    }));
  picks.forEach((p) =>
    p.addEventListener("change", () => {
      const same = picks.filter((q) => q.dataset.sev === p.dataset.sev);
      const box = sevBoxes.find((b) => b.dataset.sev === p.dataset.sev);
      box.checked = same.every((q) => q.checked);
      box.indeterminate = !box.checked && same.some((q) => q.checked);
      update();
    }));
  update();

  $("post-send").addEventListener("click", async () => {
    const send = $("post-send");
    send.disabled = true;
    msg.textContent = "Posting…";
    msg.className = "post-msg";
    try {
      const res = await fetch(`../api/reviews/${panel.dataset.pr}`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ids: picks.filter((p) => p.checked).map((p) => Number(p.dataset.id)) }),
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || `The post service answered ${res.status}.`);
      msg.textContent = "Posted. ";
      msg.append(Object.assign(document.createElement("a"), { href: body.url, textContent: "View the review on GitHub →" }));
      msg.className = "post-msg ok";
    } catch (e) {
      msg.textContent = e.message;
      msg.className = "post-msg err";
      send.disabled = false;
    }
  });

  $("post-logout").addEventListener("click", async () => {
    await fetch("../auth/logout", { method: "POST", credentials: "same-origin" }).catch(() => {});
    location.reload();
  });
})();
