"use strict";

// The post panel stays hidden on hosts without the post service, such as a self-hosted static copy.
// account.js runs first and finds out.
(async () => {
  const panel = document.getElementById("post");
  if (!panel) return;
  const $ = (id) => document.getElementById(id);
  const picks = [...document.querySelectorAll(".pick input")];
  const sevBoxes = [...panel.querySelectorAll("input[data-sev]")];
  const msg = $("post-msg");
  const acct = await window.account;
  if (!acct) return;
  const login = acct.login;
  if (login) {
    $("post-preview").hidden = false;
  } else {
    const signIn = $("post-login");
    signIn.href = `../auth/login?return=${encodeURIComponent(location.pathname)}`;
    signIn.hidden = false;
  }
  panel.hidden = false;
  document.querySelectorAll(".pick").forEach((p) => (p.hidden = false));

  const ids = () => picks.filter((p) => p.checked).map((p) => Number(p.dataset.id));
  const say = (text, cls = "") => {
    msg.textContent = text;
    msg.className = `post-msg ${cls}`;
  };
  const closePreview = () => {
    $("preview").hidden = true;
    $("post-preview").hidden = !login;
  };
  const update = () => {
    const n = ids().length;
    $("post-preview").textContent = n ? `Preview the review with ${n} ${n === 1 ? "comment" : "comments"}` : "Preview the review with the summary only";
    closePreview();
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

  const call = async (path) => {
    const res = await fetch(`../api/reviews/${panel.dataset.pr}${path}`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids: ids() }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `The post service answered ${res.status}.`);
    return body;
  };

  // GitHub renders and sanitizes the preview HTML, and the page CSP blocks scripts in it.
  const part = (where, html) => {
    const div = document.createElement("div");
    div.className = "preview-part";
    if (where) {
      const p = document.createElement("p");
      p.className = "preview-where";
      p.append("Inline comment on ", Object.assign(document.createElement("code"), { textContent: where }));
      div.append(p);
    }
    const md = document.createElement("div");
    md.className = "md";
    md.innerHTML = html;
    div.append(md);
    return div;
  };

  $("post-preview").addEventListener("click", async () => {
    const button = $("post-preview");
    button.disabled = true;
    say("Loading the preview…");
    try {
      const out = await call("/preview");
      $("preview-parts").replaceChildren(part("", out.body), ...out.comments.map((c) => part(`${c.path}:${c.line}`, c.html)));
      const n = out.comments.length;
      $("post-send").textContent = `Post as ${login}`;
      $("preview").querySelector(".preview-head").textContent =
        `Preview of what GitHub will show: the review${n ? ` and ${n} inline ${n === 1 ? "comment" : "comments"}` : ""}`;
      $("preview").hidden = false;
      button.hidden = true;
      say("");
    } catch (e) {
      say(e.message, "err");
    }
    button.disabled = false;
  });

  $("post-cancel").addEventListener("click", closePreview);

  $("post-send").addEventListener("click", async () => {
    const send = $("post-send");
    send.disabled = true;
    say("Posting…");
    try {
      const out = await call("");
      $("preview").hidden = true;
      say("Posted. ", "ok");
      msg.append(Object.assign(document.createElement("a"), { href: out.url, textContent: "View the review on GitHub →" }));
    } catch (e) {
      say(e.message, "err");
      send.disabled = false;
    }
  });
})();
