"use strict";

const RATINGS = ["blocker", "high", "medium", "low", "none"];
const RANK = { none: 0, low: 1, medium: 2, high: 3, blocker: 4 };
const LABEL = { blocker: "Blocker", high: "High", medium: "Medium", low: "Normal", none: "None" };
const BASIS = { legal: "Legal", profile: "Profile", usable: "Usable", security: "Security", hygiene: "Hygiene" };
const MEANS = {
  blocker: "Without it, Keycloak can't run lawfully or practically in production. Workarounds that don't scale don't count.",
  high: "Required by law or HAIP, or needed by most deployments.",
  medium: "Better EUDI flows, or needed by some deployments.",
  low: "Needed work that EUDI flows don't wait for, such as admin UI, docs and tests. Or it looks already done.",
};
const STATE_LABEL = { open: "Open", merged: "Merged", done: "Done" };
const REVIEW_LABEL = { draft: "Draft", none: "No review yet", reviewed: "Reviewed", changes: "Changes requested", approved: "Approved, ready to merge" };
// Sorting by review puts the PRs that most need a reviewer first.
const REVIEW_RANK = { approved: 0, changes: 1, reviewed: 2, draft: 3, none: 4 };
// Within a relevance level, the next PR to act on is one ready to merge, then one nobody has reviewed.
const NEXT_RANK = { approved: 2, none: 1, reviewed: 0 };
const AI_RANK = { major: 2, minor: 1, clean: 0 };
const DEFAULTS = { q: "", state: "open", rating: "", basis: "", topic: "", review: "", fix: "", ai: "", mine: "", parent: "", sort: "rating", dir: "desc" };
// One vendored PR changes 1.5 million lines, so the scale tops out here.
const SCALE_LINES = 5000;

let all = [];
let rows = [];
let specs = {};
let byNumber = new Map();
let state = { ...DEFAULTS };
// The GitHub login of the signed-in visitor, or null.
let me = null;

const $ = (id) => document.getElementById(id);
const el = (tag, attrs = {}, ...kids) => {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    // The CSP blocks style attributes but allows setting styles through the DOM.
    else if (k === "style") Object.assign(e.style, v);
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  e.append(...kids.filter((k) => k !== null && k !== undefined && k !== false && k !== ""));
  return e;
};
// Anything that narrows the list is a button with a funnel hint on hover.
const filterButton = (label, patch, cls, what) =>
  el("button", { type: "button", class: `f ${cls || ""}`, title: `Show only ${what}`, onclick: () => filterBy(patch) }, label);
const ext = (href, text, title) => el("a", { href, target: "_blank", rel: "noopener", ...(title ? { title } : {}) }, text);
const isOpen = (i) => i.state === "open";
const lines = (p) => p.add + p.del_;
const sizeClass = (n) => (n < 10 ? "XS" : n < 100 ? "S" : n < 500 ? "M" : n < 1000 ? "L" : "XL");

// The PRs a row's review and size describe: open ones for open work, all of them for history.
const activePRs = (r) => (isOpen(r) ? r.prs.filter(isOpen) : r.prs);

function reviewState(p) {
  if (p.draft) return "draft";
  const states = (p.reviews || []).map((x) => x[1]);
  if (states.includes("changes_requested")) return "changes";
  if (states.includes("approved")) return "approved";
  if (states.includes("commented")) return "reviewed";
  return "none";
}

const nextPRs = (r) => r.prs.filter((p) => isOpen(p) && reviewState(p) in NEXT_RANK);

// One row per issue with the PRs that fix it. An open PR on a finished issue, or a PR naming no issue, gets its own row.
// Epics leave the table. Their sub-issues carry the work, and the epic list shows the progress.
function group(list) {
  const attached = new Set();
  const out = [];
  for (const i of list.filter((x) => !x.pr)) {
    const prs = i.links.map((n) => byNumber.get(n)).filter((p) => p && p.pr && !(isOpen(p) && !isOpen(i)));
    prs.forEach((p) => attached.add(p.n));
    out.push(row(i, prs));
  }
  for (const p of list.filter((x) => x.pr && !attached.has(x.n))) out.push(row(p, [p]));
  const children = new Map();
  for (const r of out) if (r.parent) children.set(r.parent, [...(children.get(r.parent) || []), r]);
  const leaves = (r) => (children.get(r.n) || []).flatMap((k) => (children.has(k.n) ? leaves(k) : [k]));
  const byN = new Map(out.map((r) => [r.n, r]));
  for (const r of out) {
    if (children.has(r.n)) {
      r.leaves = leaves(r);
      r.kids = [r.leaves.filter((k) => !isOpen(k)).length, r.leaves.length];
    }
    r.epics = [];
    for (let p = byN.get(r.parent); p && !r.epics.includes(p.n); p = byN.get(p.parent)) r.epics.push(p.n);
  }
  return out;
}

function row(lead, prs) {
  const open = prs.filter(isOpen);
  // A reviewed rating stands. Otherwise the most relevant of the item and its open PRs counts.
  const rating = lead.basis ? lead.rating : [lead, ...open].reduce((r, m) => (RANK[m.rating] > RANK[r] ? m.rating : r), lead.rating);
  return {
    ...lead,
    prs,
    rating,
    topics: [...new Set([lead, ...prs].flatMap((m) => m.topics))].filter((t, _, a) => t !== "General" || a.length === 1),
    updated: [lead, ...prs].reduce((u, m) => (m.updated > u ? m.updated : u), lead.updated),
  };
}

const isEpic = (r) => Boolean(r.kids);

// Links can carry filters and sort columns from older versions of the page.
function valid(k, v) {
  if (k === "q") return true;
  if (k === "dir") return v === "asc" || v === "desc";
  if (k === "parent") return rows.some((r) => isEpic(r) && String(r.n) === v);
  if (k === "sort") return document.querySelector(`th[data-sort="${CSS.escape(v)}"]`) !== null || [...$("f-sort").options].some((o) => o.value === v);
  const select = $(`f-${k}`);
  return select !== null && [...select.options].some((o) => o.value === v);
}

function readHash() {
  const p = new URLSearchParams(location.hash.slice(1));
  state = { ...DEFAULTS };
  for (const k of Object.keys(DEFAULTS)) if (p.has(k) && valid(k, p.get(k))) state[k] = p.get(k);
  if (!me) state.mine = "";
}

function writeHash() {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(state)) if (v !== DEFAULTS[k]) p.set(k, v);
  const h = p.toString();
  history.replaceState(null, "", h ? "#" + h : location.pathname);
}

function matches(r) {
  if (isEpic(r)) return false;
  if (state.state === "open" && !isOpen(r)) return false;
  if (state.state === "finished" && isOpen(r)) return false;
  if (state.parent && !r.epics.includes(Number(state.parent))) return false;
  if (state.rating) {
    if (state.rating.endsWith("+")) {
      if (RANK[r.rating] < RANK[state.rating.slice(0, -1)]) return false;
    } else if (r.rating !== state.rating) return false;
  }
  if (state.basis === "unrated" ? r.basis || !isOpen(r) : state.basis && r.basis !== state.basis) return false;
  if (state.topic && !r.topics.includes(state.topic)) return false;
  if (state.review === "next" ? !nextPRs(r).length : state.review && !r.prs.some((p) => isOpen(p) && reviewState(p) === state.review)) return false;
  if (state.fix === "open" && !r.prs.some(isOpen)) return false;
  if (state.ai) {
    const open = r.prs.filter(isOpen);
    if (state.ai === "none" ? !open.some((p) => !p.ai) : !open.some((p) => p.ai && (state.ai === "any" || p.ai.level === state.ai || (state.ai === "fix" && p.ai.level !== "clean")))) return false;
  }
  if (state.fix === "none" && r.prs.length) return false;
  if (state.mine === "mine" && ![r, ...r.prs].some((x) => x.author.toLowerCase() === me || x.assignees.some((a) => a.toLowerCase() === me))) return false;
  if (state.mine === "review" && !r.prs.some((p) => (p.reviews || []).some(([who]) => who.toLowerCase() === me))) return false;
  if (state.q) {
    const prs = r.prs.map((p) => `${p.n} ${p.title} ${p.author}`).join(" ");
    const hay = `${r.n} ${r.title} ${r.author} ${r.labels.join(" ")} ${r.topics.join(" ")} ${r.note} ${r.milestone} ${(r.same || []).join(" ")} ${prs}`.toLowerCase();
    if (!state.q.toLowerCase().replace(/#/g, "").split(/\s+/).every((w) => hay.includes(w))) return false;
  }
  return true;
}

function sortKey(r) {
  switch (state.sort) {
    case "n": return r.n;
    case "title": return r.title.toLowerCase();
    case "state": return { open: 0, merged: 1, done: 2 }[r.state];
    case "rating": return RANK[r.rating];
    case "review": {
      const open = r.prs.filter(isOpen);
      return open.length ? Math.max(...open.map((p) => REVIEW_RANK[reviewState(p)])) : -1;
    }
    case "ai": {
      const ai = activePRs(r).filter((p) => p.ai).map((p) => AI_RANK[p.ai.level]);
      return ai.length ? Math.max(...ai) : -1;
    }
    case "size": {
      const prs = activePRs(r);
      return prs.length ? prs.reduce((n, p) => n + lines(p), 0) : -1;
    }
    default: return r.updated;
  }
}

function compare(a, b) {
  const x = sortKey(a), y = sortKey(b);
  const c = x < y ? -1 : x > y ? 1 : 0;
  if (c) return state.dir === "asc" ? c : -c;
  const next = (r) => Math.max(-1, ...nextPRs(r).map((p) => NEXT_RANK[reviewState(p)]));
  if (state.review === "next" && next(a) !== next(b)) return next(b) - next(a);
  // Within a level, work with an open PR comes first.
  const openPR = (r) => (r.prs.some(isOpen) ? 1 : 0);
  return openPR(b) - openPR(a) || b.updated.localeCompare(a.updated);
}

function filterBy(patch) {
  state = { ...state, ...patch };
  update();
}

const fresh = (patch) => filterBy({ ...DEFAULTS, ...patch });

function specLink(ids) {
  const s = specs[(ids || [])[0]];
  return s ? el("div", { class: "specs" }, ext(s.url, s.rule, `${s.doc} ${s.ref}: “${s.text}”`)) : null;
}

// The reason for a rating opens on hover, focus or tap.
function why(r) {
  const s = specs[(r.specs || [])[0]];
  return el("span", { class: "why" },
    el("button", { type: "button", class: "why-btn", "aria-label": `Why ${LABEL[r.rating]}` }, "i"),
    el("span", { class: "why-pop", role: "tooltip" },
      el("b", {}, `Why ${LABEL[r.rating]}`),
      el("span", {}, r.why || r.note),
      s && el("span", { class: "why-spec" }, ext(s.url, s.rule), ` “${s.text.length > 220 ? s.text.slice(0, 218) + "…" : s.text}”`),
      el("span", { class: "muted" }, `${LABEL[r.rating]}: ${MEANS[r.rating]}`)));
}

function relCell(r) {
  return el("td", { class: "c-rel" },
    el("div", { class: "rel-line" }, filterButton(LABEL[r.rating], { rating: r.rating }, `rel rel-${r.rating}`, `${LABEL[r.rating]} items`),
      r.basis && filterButton(BASIS[r.basis], { basis: r.basis }, "basis", `items with basis ${BASIS[r.basis]}`),
      r.basis && why(r)),
    isOpen(r) && !r.basis && el("div", { class: "muted" }, "Not rated yet"),
    r.note && el("div", { class: "note" }, r.note));
}

function sizeCell(r) {
  const prs = activePRs(r);
  if (!prs.length) return el("td", { class: "c-size" });
  const add = prs.reduce((n, p) => n + p.add, 0), del = prs.reduce((n, p) => n + p.del_, 0);
  const files = prs.reduce((n, p) => n + p.files, 0), total = add + del;
  const width = Math.min(100, (Math.log10(total + 1) / Math.log10(SCALE_LINES + 1)) * 100);
  const addW = total ? (width * add) / total : 0;
  return el("td", { class: "c-size" },
    el("div", { class: "size-row" }, el("span", { class: "size-tag" }, sizeClass(total)),
      el("div", { class: "bar", title: `${add} added, ${del} deleted in ${files} files` },
        el("span", { class: "a", style: { width: `${addW}%` } }), el("span", { class: "d", style: { width: `${width - addW}%` } }))),
    el("div", { class: "size-num" }, el("span", { class: "pa" }, `+${add.toLocaleString()}`), " ",
      el("span", { class: "pd" }, `−${del.toLocaleString()}`), ` · ${files} ${files === 1 ? "file" : "files"}`));
}

// The PRs behind a row, with the review state of the open ones.
function prCell(r) {
  const td = el("td", { class: "c-review" });
  if (!r.prs.length) {
    if (isOpen(r) && !isEpic(r)) td.append(filterButton("No PR yet", { fix: "none" }, "muted", "items without a PR"));
    return td;
  }
  for (const p of r.prs) {
    const st = reviewState(p);
    const people = (p.reviews || []).filter((x) => x[1] !== "requested" && x[1] !== "dismissed").map((x) => x[0]);
    const asked = (p.reviews || []).filter((x) => x[1] === "requested").map((x) => x[0]);
    td.append(el("div", { class: "rv" },
      !r.pr && ext(p.url, `#${p.n}`, p.title), !r.pr && " ",
      isOpen(p) ? (st === "draft" ? el("span", { class: "rv-state rv-draft" }, REVIEW_LABEL[st]) : filterButton(REVIEW_LABEL[st], { review: st }, `rv-state rv-${st}`, `PRs with the state ${REVIEW_LABEL[st]}`))
        : el("span", { class: "muted" }, STATE_LABEL[p.state]),
      isOpen(p) && people.length > 0 && el("div", { class: "muted" }, people.join(", ")),
      isOpen(p) && asked.length > 0 && el("div", { class: "muted" }, `Requested: ${asked.join(", ")}`),
      p.demo && el("a", { class: "try", href: p.demo, target: "_blank", rel: "noopener", title: "This PR runs on the Keycloak wallet demo" },
        el("img", { src: "demo-logo.svg", alt: "", width: "14", height: "18" }), "Try it on the demo")));
  }
  return td;
}

// Claude's review of each open PR behind a row.
function aiCell(r) {
  const td = el("td", { class: "c-ai" });
  if (r.prs.some(isOpen)) td.append(el("span", { class: "narrow ai-label" }, "AI review"));
  for (const p of r.prs.filter(isOpen)) {
    if (!p.ai) {
      td.append(el("div", { class: "ai muted" }, r.prs.length > 1 ? `#${p.n} not reviewed yet` : "Not reviewed yet"));
      continue;
    }
    const parts = [p.ai.major && `${p.ai.major} must fix`, p.ai.minor && `${p.ai.minor} should fix`,
      p.ai.followup && `${p.ai.followup} ${p.ai.followup === 1 ? "follow-up" : "follow-ups"}`].filter(Boolean);
    const main = p.ai.major || p.ai.minor ? "Fix first" : "Merge as is", more = parts.length ? ` · ${parts.join(", ")}` : "";
    td.append(el("div", { class: "ai" },
      el("div", { class: `ai-verdict v-${p.ai.level}` }, r.prs.length > 1 ? `#${p.n} ` : "",
        filterButton(main, { ai: p.ai.level === "clean" ? "clean" : "fix" }, "ai-v", p.ai.level === "clean" ? "PRs Claude would merge as they are" : "PRs Claude would fix before merging"), more && el("span", { class: "muted" }, more)),
      el("div", { class: "ai-head" }, ...p.ai.headline.split("`").map((t, k) => (k % 2 ? el("code", {}, t) : t))),
      el("a", { class: "ai-link", href: `reviews/${p.n}.html` }, p.ai.stale ? "Full review of an older commit →" : "Full review →")));
  }
  return td;
}

function titleCell(r) {
  const meta = el("div", { class: "meta" });
  for (const t of r.topics) meta.append(filterButton(t, { topic: t }, "chip", `the topic ${t}`));
  meta.append(" by ", filterButton(r.author, { q: r.author }, "who", `items by ${r.author}`));
  if (r.assignees && r.assignees.length) meta.append(` · assigned to ${r.assignees.join(", ")}`);
  meta.append(` · updated ${r.updated}`);
  if (r.comments) meta.append(` · ${r.comments} ${r.comments === 1 ? "comment" : "comments"}`);
  if (r.same) meta.append(" · also ", ...r.same.flatMap((n, k) => [k ? ", " : "", ext(`https://github.com/keycloak/keycloak/issues/${n}`, `#${n}`)]));

  const epicOf = (n) => () => fresh({ parent: String(n), state: "", sort: state.sort, dir: state.dir });
  return el("td", { class: "c-title" },
    r.pr && el("span", { class: "kind" }, "PR"),
    ext(r.url, r.title),
    meta,
    r.parent && state.parent !== String(r.parent) && el("div", { class: "meta" }, "Part of ",
      el("button", { type: "button", class: "linkish", title: "Show all sub-issues of this epic", onclick: epicOf(r.parent) },
        `#${r.parent} ${r.parentTitle.length > 60 ? r.parentTitle.slice(0, 57) + "…" : r.parentTitle}`)));
}

function rowEl(r) {
  return el("tr", {},
    el("td", { class: "c-num" }, ext(r.url, `#${r.n}`)),
    titleCell(r),
    el("td", { class: "c-state" }, el("span", { class: `badge s-${r.state}` }, STATE_LABEL[r.state])),
    relCell(r),
    prCell(r),
    sizeCell(r),
    aiCell(r));
}

function render() {
  const shown = rows.filter(matches).sort(compare);
  $("rows").replaceChildren(...shown.slice(0, 500).map(rowEl));
  $("count").textContent = shown.length > 500 ? `${shown.length} items, showing the first 500` : `${shown.length} ${shown.length === 1 ? "item" : "items"}`;
  $("empty").hidden = shown.length > 0;
  $("table").classList.toggle("only-open", state.state === "open");
  $("active").replaceChildren(...activeFilters().map(([label, patch]) =>
    el("button", { type: "button", class: "chip active", title: "Remove this filter", onclick: () => filterBy(patch) }, label, el("span", { "aria-hidden": "true" }, " ✕"))));
}

// The filters in use, each with the patch that removes it.
function activeFilters() {
  const out = [];
  for (const k of ["state", "rating", "basis", "topic", "review", "fix", "ai", "mine"]) {
    if (state[k] === DEFAULTS[k]) continue;
    const name = $(`f-${k}`).closest("label").firstChild.textContent.trim(), value = $(`f-${k}`).selectedOptions[0].textContent;
    out.push([value.startsWith(name) ? value : `${name}: ${value}`, { [k]: DEFAULTS[k] }]);
  }
  if (state.parent) out.push([`Epic: ${rows.find((r) => String(r.n) === state.parent).title}`, { parent: "" }]);
  if (state.q) out.push([`Search: ${state.q}`, { q: "" }]);
  return out;
}

function syncControls() {
  $("q").value = state.q;
  for (const k of ["state", "rating", "basis", "topic", "review", "fix", "ai", "mine", "sort"]) $(`f-${k}`).value = state[k];
  for (const th of document.querySelectorAll("th[data-sort]")) {
    th.setAttribute("aria-sort", th.dataset.sort === state.sort ? (state.dir === "asc" ? "ascending" : "descending") : "none");
  }
}

function update() {
  writeHash();
  syncControls();
  render();
}

function summary() {
  const work = rows.filter((r) => isOpen(r) && !isEpic(r));
  const tiles = [
    [work.filter((r) => nextPRs(r).length).length, "PRs to review or merge next", { review: "next" }],
    [work.filter((r) => r.rating === "blocker").length, "blocker", { rating: "blocker" }],
    [work.filter((r) => RANK[r.rating] >= RANK.high && !r.prs.length).length, "blockers or high items without a PR", { rating: "high+", fix: "none" }],
  ];
  $("stats").replaceChildren(...tiles.map(([n, label, f]) => el("button", { type: "button", class: "stat", title: `Show the ${label}`, onclick: () => fresh(f) }, el("b", {}, String(n)), el("span", {}, n === 1 || label !== "blocker" ? label : "blockers"), el("span", { class: "go", "aria-hidden": "true" }, "Show →"))));

  $("open-total").textContent = `${work.length} open`;
  for (const r of RATINGS) {
    const n = work.filter((x) => x.rating === r).length;
    if (!n) continue;
    $("stack").append(el("button", { type: "button", style: { flex: String(n), background: `var(--${r})` }, title: `${n} ${LABEL[r]}`, "aria-label": `${n} ${LABEL[r]}`, onclick: () => fresh({ rating: r }) }));
    $("legend").append(el("button", { type: "button", class: "f", title: `Show only ${LABEL[r]} items`, onclick: () => fresh({ rating: r }) }, el("i", { style: { background: `var(--${r})` } }), `${LABEL[r]} ${n}`));
  }
}

// Open epics as a tree, each with its progress and the relevance of its open sub-issues.
function epics() {
  const open = rows.filter((r) => isEpic(r) && isOpen(r));
  const item = (e) => {
    const counts = RATINGS.map((x) => [x, e.leaves.filter((k) => isOpen(k) && k.rating === x).length]).filter(([, n]) => n);
    const subs = open.filter((x) => x.parent === e.n);
    return el("li", {},
      el("button", { type: "button", class: "epic", title: "Show the sub-issues", onclick: () => fresh({ parent: String(e.n) }) },
        el("span", { class: "subs-bar" }, el("span", { style: { width: `${(100 * e.kids[0]) / e.kids[1]}%` } })),
        el("span", { class: "epic-title" }, e.title.replace(/^\[[^\]]*\]\s*/, "")),
        el("span", { class: "muted" }, `${e.kids[0]} of ${e.kids[1]} done`),
        ...counts.map(([x, n]) => el("span", { class: `rel rel-${x}` }, `${n} ${LABEL[x]}`))),
      subs.length > 0 && el("ul", {}, ...subs.map(item)));
  };
  const top = open.filter((e) => !open.some((x) => x.n === e.parent));
  $("epics").replaceChildren(...top.sort((a, b) => b.leaves.filter(isOpen).length - a.leaves.filter(isOpen).length).map(item));
}

// A short status for the meeting notes of the Keycloak OAuth SIG. It lists what the table shows, in the same order.
function statusMarkdown() {
  const work = rows.filter((r) => isOpen(r) && !isEpic(r));
  const count = (x) => work.filter((r) => r.rating === x).length;
  const filters = activeFilters().map(([label]) => label);
  const short = (t) => (t.length > 90 ? t.slice(0, 88) + "…" : t);
  const ai = (p) => {
    if (!isOpen(p) || !p.ai) return "";
    const counts = [p.ai.major && `${p.ai.major} must fix`, p.ai.minor && `${p.ai.minor} should fix`].filter(Boolean);
    const verdict = counts.length ? `fix first: ${counts.join(", ")}` : "merge as is";
    return `, AI review [${verdict}](${new URL(`reviews/${p.n}.html`, location.href).href})${p.ai.stale ? " (older commit)" : ""}`;
  };
  const pr = (r) => (r.prs.length ? r.prs.map((p) => `PR #${p.n} ${(isOpen(p) ? REVIEW_LABEL[reviewState(p)] : STATE_LABEL[p.state]).toLowerCase()}${ai(p)}`) : isOpen(r) ? ["no PR"] : []);
  const list = rows.filter(matches).sort(compare).map((r) => `- [#${r.n}](${r.url}) ${short(r.title)} (${[LABEL[r.rating], ...pr(r)].join(", ")})`);
  const link = location.href.split("#")[1] ? location.href : `${location.origin}/`;
  return [
    `### OpenID4VC status ${new Date().toISOString().slice(0, 10)}`,
    "",
    `${work.length} open items, ${count("blocker")} of them blockers and ${count("high")} high. [Tracker](${link})`,
    "",
    `**${filters.length ? filters.join(", ") : "All open items"}** (${list.length})`,
    "",
    ...(list.length ? list : ["- none"]),
  ].join("\n");
}

async function copyStatus() {
  const text = statusMarkdown();
  const button = $("copy-status");
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = "Copied";
  } catch {
    $("status-text").hidden = false;
    $("status-text").value = text;
    $("status-text").select();
    button.textContent = "Copy the text below";
  }
  setTimeout(() => { button.textContent = "Copy as Markdown"; }, 2500);
}

function gaps(list) {
  $("gap-count").textContent = `(${list.length})`;
  $("gaps").replaceChildren(...list.sort((a, b) => RANK[b.rating] - RANK[a.rating]).map((g) => el("li", {},
    el("div", {}, el("span", { class: `rel rel-${g.rating}` }, LABEL[g.rating]), el("span", { class: "basis" }, BASIS[g.basis]), " ", el("b", {}, g.title)),
    el("div", { class: "note" }, g.note),
    specLink(g.specs))));
}

// Hosts without the post service answer 404, and the account stays hidden there.
async function account() {
  let res;
  try {
    res = await fetch("api/me", { credentials: "same-origin" });
  } catch {
    return;
  }
  if (res.ok) {
    me = (await res.json()).login.toLowerCase();
    $("me-login").textContent = me;
    $("signed-in").hidden = false;
    $("f-mine-label").hidden = false;
  } else if (res.status === 401) {
    // GitHub sends people back to the bare page, so the filters wait in sessionStorage.
    $("signin").hidden = false;
    $("signin").addEventListener("click", () => {
      try { sessionStorage.setItem("return-hash", location.hash); } catch { /* the filters are lost, nothing else */ }
    });
  } else {
    return;
  }
  $("account").hidden = false;
  $("signout").addEventListener("click", async () => {
    await fetch("auth/logout", { method: "POST", credentials: "same-origin" }).catch(() => {});
    location.reload();
  });
}

async function main() {
  const [data] = await Promise.all([fetch("items.json").then((r) => r.json()), account()]);
  try {
    const back = sessionStorage.getItem("return-hash");
    sessionStorage.removeItem("return-hash");
    if (back && !location.hash) history.replaceState(null, "", back);
  } catch { /* nothing to restore */ }
  all = data.items;
  specs = data.specs;
  byNumber = new Map(all.map((i) => [i.n, i]));
  rows = group(all);
  $("fetched").textContent = `Updated ${data.fetched.slice(0, 10)}.`;
  $("f-topic").append(...[...new Set(rows.flatMap((r) => r.topics))].sort().map((t) => el("option", { value: t }, t)));
  $("docs").replaceChildren(...data.docs.map((d) => el("li", {}, ext(d.url, d.name), ` ${d.version}`)));
  summary();
  epics();
  gaps(data.gaps);

  $("q").addEventListener("input", (e) => filterBy({ q: e.target.value }));
  for (const k of ["state", "rating", "basis", "topic", "review", "fix", "ai", "mine"]) $(`f-${k}`).addEventListener("change", (e) => filterBy({ [k]: e.target.value }));
  $("f-sort").addEventListener("change", (e) => filterBy({ sort: e.target.value, dir: "desc" }));
  $("reset").addEventListener("click", () => fresh({}));
  $("copy-status").addEventListener("click", copyStatus);
  for (const th of document.querySelectorAll("th[data-sort]")) {
    th.addEventListener("click", () => {
      const k = th.dataset.sort;
      filterBy(state.sort === k ? { dir: state.dir === "asc" ? "desc" : "asc" } : { sort: k, dir: k === "title" || k === "state" ? "asc" : "desc" });
    });
  }
  window.addEventListener("hashchange", () => { readHash(); syncControls(); render(); });
  readHash();
  update();
}

main();
