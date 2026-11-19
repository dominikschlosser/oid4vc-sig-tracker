"use strict";

// The post panel stays hidden on hosts without the post service, such as a self-hosted static copy.
// account.js runs first and finds out.
(async () => {
  const panel = document.getElementById("post");
  if (!panel) return;
  const $ = (id) => document.getElementById(id);
  const picks = [...document.querySelectorAll(".pick input[data-id]")];
  const answerPicks = [...document.querySelectorAll(".pick input[data-answer]")];
  const allAnswers = $("post-answers");
  const sevBoxes = [...panel.querySelectorAll("input[data-sev]")];
  const msg = $("post-msg");
  const acct = await window.account;
  if (!acct) return;
  const login = acct.login;
  if (login) {
    $("post-preview").hidden = false;
    // While the bot is down, people on its list still see it as their default, so they don't post in their
    // own name without meaning to.
    if (acct.bot || acct.botDown) {
      $("post-as-me").textContent = login;
      if (acct.bot) $("post-as-bot").textContent = acct.bot;
      else panel.querySelector(".post-as-bot").textContent = "The tracker's bot account isn't available.";
      $("post-as").hidden = false;
      $("post-own").hidden = true;
    }
  } else {
    const signIn = $("post-login");
    signIn.href = `../auth/login?return=${encodeURIComponent(location.pathname)}`;
    signIn.hidden = false;
  }
  panel.hidden = false;
  document.querySelectorAll(".pick").forEach((p) => (p.hidden = false));

  // Each account reviews a commit once. reviewed holds the accounts ("bot", or "" for the person's own name)
  // whose review of this commit is on GitHub. For them only answers can still go out, without a review.
  const reviewed = new Set();
  // People who may use the bot post through it unless they pick their own name.
  const as = () => (acct.bot && !$("post-as-own").checked ? "bot" : "");
  const ids = () => (reviewed.has(as()) ? [] : picks.filter((p) => p.checked).map((p) => Number(p.dataset.id)));
  const answers = () => answerPicks.filter((p) => p.checked && !p.disabled).map((p) => p.dataset.answer);
  // An answer that is on GitHub, or that a newer reply overtook, can't be picked again. Its pick becomes a link.
  const mark = (id, text, url) => {
    const pick = answerPicks.find((p) => p.dataset.answer === id);
    if (!pick || pick.disabled) return;
    pick.checked = false;
    pick.disabled = true;
    pick.parentElement.replaceChildren(pick, Object.assign(document.createElement("a"), { href: url, textContent: text }));
    syncAllAnswers();
  };
  // The box for all answers covers the answers that can still go out, and turns off when none can.
  const syncAllAnswers = () => {
    if (!allAnswers) return;
    const open = answerPicks.filter((p) => !p.disabled);
    allAnswers.disabled = !open.length;
    allAnswers.checked = open.length > 0 && open.every((p) => p.checked);
    allAnswers.indeterminate = !allAnswers.checked && open.some((p) => p.checked);
  };
  const markSkipped = (skipped) =>
    (skipped || []).forEach((s) => mark(s.id, s.kind === "posted" ? "On GitHub" : "Newer reply on GitHub", s.url));
  const say = (text, cls = "") => {
    msg.textContent = text;
    msg.className = `post-msg ${cls}`;
  };
  const earlierLink = (href) => Object.assign(document.createElement("a"), { href, textContent: "The earlier review on GitHub →" });
  // The finding picks are off while the chosen account has reviewed this commit.
  // saved holds the picks from before they turned off, so switching to the other account brings them back.
  let saved = null;
  // account is the one the request went out as, which the checkbox may no longer show.
  const lockFindings = (account) => {
    reviewed.add(account);
    showFindings();
  };
  const showFindings = () => {
    const boxes = [...picks, ...sevBoxes];
    if (reviewed.has(as())) {
      saved ??= boxes.map((p) => [p.checked, p.indeterminate]);
      boxes.forEach((p) => {
        p.checked = false;
        p.indeterminate = false;
        p.disabled = true;
      });
    } else {
      if (saved) boxes.forEach((p, i) => ([p.checked, p.indeterminate] = saved[i]));
      saved = null;
      boxes.forEach((p) => (p.disabled = false));
    }
  };
  // After an error the picks may have changed, so the panel starts over and the next post needs a new preview.
  const sayError = (e, account) => {
    if (e.earlier) lockFindings(account);
    markSkipped(e.skipped);
    update();
    say(`${e.message} `, "err");
    if (e.earlier) msg.append(earlierLink(e.earlier));
  };
  const closePreview = () => {
    $("preview").hidden = true;
    $("post-preview").hidden = !login;
  };
  const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
  let downShown = false; // whether the message says that the bot is down
  // Post sends the picks of the preview, also when the page changed while the preview loaded.
  // round counts the changes of the panel, so a preview that comes back after one is dropped.
  // loading counts the previews asked for, so only the newest one clears the loading message.
  let previewed = null;
  let round = 0;
  let loading = 0;
  const update = () => {
    round++;
    const n = ids().length;
    const a = answers().length;
    const what = n ? `with ${plural(n, "comment", "comments")}` : "with the summary only";
    const button = $("post-preview");
    if (acct.botDown && !$("post-as-own").checked) {
      button.textContent = "The bot account isn't available";
      button.disabled = true;
      say(acct.botDown, "err");
      downShown = true;
      closePreview();
      return;
    }
    if (downShown) say("");
    downShown = false;
    // A post with answers but no findings sends only the replies, as does every post after the review.
    const posted = reviewed.has(as());
    if (posted && !a) button.textContent = answerPicks.some((p) => !p.disabled) ? "Pick an answer to post it as a reply" : "This account already posted its review of this commit";
    else if (posted) button.textContent = `Preview ${plural(a, "reply", "replies")}`;
    else if (!n && a) button.textContent = `Preview ${plural(a, "reply", "replies")} without a review`;
    else button.textContent = `Preview the review ${what}${a ? ` and ${plural(a, "reply", "replies")}` : ""}`;
    button.disabled = posted && !a;
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
  answerPicks.forEach((p) =>
    p.addEventListener("change", () => {
      syncAllAnswers();
      update();
    }));
  allAnswers?.addEventListener("change", () => {
    answerPicks.filter((p) => !p.disabled).forEach((p) => (p.checked = allAnswers.checked));
    update();
  });
  $("post-as-own").addEventListener("change", () => {
    showFindings();
    update();
  });
  update();

  const picked = () => ({ version: panel.dataset.version, ids: ids(), answers: answers(), as: as() });
  const call = async (path, sent) => {
    const preview = path === "/preview";
    const res = await fetch(`../api/reviews/${panel.dataset.pr}${path}`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(sent),
    });
    const body = await res.json().catch(() => ({}));
    // A 502 or 504 without an error of the service's own comes from the web server in front of it, which may
    // have given up waiting while the service still posts.
    const gaveUp = !body.error && (res.status === 502 || res.status === 504);
    const late = preview ? "The preview took too long. Try again." : "The post service didn't answer in time. Your post may have gone out. Post again to see what did.";
    const reason = body.error || (gaveUp ? late : `The post service answered ${res.status}.`);
    if (!res.ok) throw Object.assign(new Error(reason), { earlier: body.earlier, skipped: body.skipped });
    return body;
  };

  // GitHub renders and sanitizes the preview HTML, and the page CSP blocks scripts in it.
  // where is a list of text and code parts that say where the part goes, empty for the review body.
  const part = (where, html) => {
    const div = document.createElement("div");
    div.className = "preview-part";
    if (where.length) {
      const p = document.createElement("p");
      p.className = "preview-where";
      p.append(...where);
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
    const body = picked();
    const mine = round;
    const ticket = ++loading;
    const stillLoading = () => ticket === loading && msg.textContent === "Loading the preview…";
    try {
      const out = await call("/preview", body);
      if (mine !== round) {
        if (stillLoading()) say("");
        return;
      }
      previewed = body;
      const code = (text) => Object.assign(document.createElement("code"), { textContent: text });
      const comments = out.comments || [];
      const replies = out.replies || [];
      const skipped = out.skipped || [];
      markSkipped(skipped);
      // update() closes the preview, so it runs before the preview opens below.
      if (out.earlier) {
        lockFindings(body.as);
        update();
      }
      const notes = [];
      if (out.note) {
        notes.push(Object.assign(document.createElement("p"), { className: "preview-where", textContent: `${out.note} ` }));
        if (out.earlier) notes[0].append(earlierLink(out.earlier));
      }
      for (const s of skipped) {
        const p = Object.assign(document.createElement("p"), { className: "preview-where", textContent: `Left out: the answer to ${s.label}. ${s.reason} ` });
        p.append(Object.assign(document.createElement("a"), { href: s.url, textContent: "View it →" }));
        notes.push(p);
      }
      $("preview-parts").replaceChildren(
        ...notes,
        ...(out.body ? [part([], out.body)] : []),
        ...comments.map((c) => part(["Inline comment on ", code(`${c.path}:${c.line}`)], c.html)),
        ...replies.map((r) => part([r.thread ? "Reply in the thread of " : "New comment that answers ", r.label], r.html)));
      const n = comments.length;
      const a = replies.length;
      const what = out.body ? ["the review"] : [];
      if (n) what.push(plural(n, "inline comment", "inline comments"));
      if (a) what.push(plural(a, "reply", "replies"));
      $("post-send").textContent = `Post as ${as() === "bot" ? acct.bot : login}`;
      $("post-send").disabled = !what.length;
      $("preview").querySelector(".preview-head").textContent = what.length ?
        `Preview of what GitHub will show: ${what.join(", ")}` : "Nothing left to post.";
      $("preview").hidden = false;
      button.hidden = true;
      say("");
    } catch (e) {
      if (mine === round) sayError(e, body.as);
      else if (stillLoading()) say("");
    }
  });

  $("post-cancel").addEventListener("click", update);

  $("post-send").addEventListener("click", async () => {
    const send = $("post-send");
    send.disabled = true;
    say("Posting…");
    // A new preview may replace previewed while this post is on its way.
    const sent = previewed;
    try {
      const out = await call("", sent);
      // earlier means the account had already reviewed this commit, from another tab or an earlier visit.
      if (out.reviewed || out.earlier) lockFindings(sent.as);
      const failed = out.failed || [];
      const skipped = out.skipped || [];
      Object.entries(out.posted || {}).forEach(([id, url]) => mark(id, "Posted", url));
      markSkipped(skipped);
      const replies = Object.keys(out.posted || {}).length;
      const what = [out.reviewed ? "the review" : "", replies ? plural(replies, "reply", "replies") : ""].filter(Boolean).join(" and ");
      const parts = [`Posted ${what}.`];
      if (skipped.length) parts.push(`Left out the answers to ${skipped.map((s) => s.label).join("; ")}. See the links next to them.`);
      // After a refusal, the service may stop before sending the other replies, so not each of them was refused.
      if (failed.length) parts.push(`The replies to ${failed.join("; ")} failed. ${out.refused.replace(/\.?$/, ".")} Post them again, or reply to them on GitHub.`);
      update();
      say(`${parts.join(" ")} `, failed.length ? "err" : "ok");
      // url is the new review. Without one the post sent only replies, and the link goes to the first.
      const reply = Object.values(out.posted || {})[0];
      if (out.url || reply) {
        msg.append(Object.assign(document.createElement("a"), { href: out.url || reply, textContent: out.url ? "View the review on GitHub →" : "View the reply on GitHub →" }));
      }
    } catch (e) {
      sayError(e, sent.as);
    }
  });
})();
