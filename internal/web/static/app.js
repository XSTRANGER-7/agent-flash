// Agent Arena PWA. No framework, no build step, no secrets in the browser:
// it only talks to the Go backend on the same origin, which holds the Flash key.

/* ---------------- utilities ---------------- */
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ESC[c]);
const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;

// Missing values (null, undefined, "") are unknown, never zero.
const num = (v) => (v === null || v === undefined || v === "" ? NaN : Number(v));
const known = (v) => Number.isFinite(num(v));
const usd = (v, d = 2) => { const n = num(v); return Number.isFinite(n) ? (n < 0 ? "-$" : "$") + Math.abs(n).toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d }) : "—"; };
const pct = (v, d = 1) => { const n = num(v); if (!Number.isFinite(n)) return "—"; const t = n.toFixed(d); return +t === 0 ? (0).toFixed(d) + "%" : (n > 0 ? "+" : "") + t + "%"; };
const plural = (n, w) => `${n ?? 0} ${w}${+n === 1 ? "" : "s"}`;
const price = (v) => { const n = num(v); return Number.isFinite(n) && n > 0 ? "$" + n.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : "—"; };
const fixed = (v, d) => { const n = num(v); return Number.isFinite(n) ? n.toFixed(d) : "—"; };
// Signs are judged at the cent (or 0.01 point), so -0.001 is not shown red.
const signClass = (v) => { const r = Math.round(num(v) * 100) / 100; return r > 0 ? "up" : r < 0 ? "down" : ""; };
const short = (a) => (a && a.length > 14 ? a.slice(0, 6) + "…" + a.slice(-4) : a || "");
const initials = (name) => (name || "?").split(/\s+/).map((w) => w[0]).join("").slice(0, 2).toUpperCase();
const isAddr = (a) => typeof a === "string" && /^0x[0-9a-fA-F]{40}$/.test(a);
const isHash = (h) => typeof h === "string" && /^0x[0-9a-fA-F]{64}$/.test(h);
const EXT = 'target="_blank" rel="noopener noreferrer"';
const addrLink = (a) => (isAddr(a) ? `<a class="link mono" href="https://basescan.org/address/${a}" ${EXT}>${esc(short(a))}</a>` : esc(a || "—"));
// Real Base tx hashes link to Basescan; paper fills and dry runs have none.
const txLink = (h) => (isHash(h) ? `<a class="link mono" href="https://basescan.org/tx/${h}" ${EXT}>${esc(short(h))}</a>`
  : typeof h === "string" && h.startsWith("paper") ? '<span class="mono muted">paper fill</span>'
  : h === "dryrun" ? '<span class="mono muted">not sent (dry run)</span>' : "—");
const cap = (s) => (s ? s[0].toUpperCase() + s.slice(1).replace(/_/g, " ") : "");
// Profit and loss always carry a sign (and, via .pl, an arrow): never colour alone.
const pnl = (v, d = 2) => { const n = num(v); return Number.isFinite(n) ? (n > 0 ? "+" : n < 0 ? "−" : "") + usd(Math.abs(n), d) : "—"; };
const pnlHTML = (v, d = 2) => `<span class="pl ${signClass(v)}">${esc(pnl(v, d))}</span>`;
// Big dollar amounts in compact form: $1.2K, $3.4M.
const COMPACT = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });
const cusd = (v) => { const n = num(v); if (!Number.isFinite(n)) return "—"; const a = Math.abs(n); return (n < 0 ? "-$" : "$") + (a >= 1000 ? COMPACT.format(a) : a.toLocaleString("en-US", { maximumFractionDigits: a >= 100 ? 0 : 2 })); };

const FMT = {
  int: (v) => Math.round(v).toLocaleString("en-US"),
  cusd: (v) => cusd(v),
  usd0: (v) => usd(v, 0),
  usd2: (v) => usd(v, 2),
  score: (v) => { const r = Math.round(v * 100) / 100 || 0; return (r > 0 ? "+" : "") + r.toFixed(2); }, // never "-0.00"
  pct1: (v) => pct(v, 1),
  dd: (v) => v.toFixed(1) + "%",
};

function ago(ts) {
  const t = new Date(ts).getTime();
  if (!Number.isFinite(t)) return "—";
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}
const clock = (ts) => { const d = new Date(ts); return Number.isFinite(d.getTime()) ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : ""; };
const dayLabel = (ts) => {
  const d = new Date(ts), today = new Date(), yest = new Date(Date.now() - 864e5);
  if (d.toDateString() === today.toDateString()) return "Today";
  if (d.toDateString() === yest.toDateString()) return "Yesterday";
  return d.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" });
};

const store = {
  get(k, d = null, session = false) {
    try { const v = (session ? sessionStorage : localStorage).getItem(k); return v === null ? d : JSON.parse(v); } catch { return d; }
  },
  set(k, v, session = false) {
    try { (session ? sessionStorage : localStorage).setItem(k, JSON.stringify(v)); } catch { /* private mode */ }
  },
  del(k, session = false) { try { (session ? sessionStorage : localStorage).removeItem(k); } catch { /* ignore */ } },
};

/* ---------------- API ---------------- */
const adminToken = () => store.get("arena.admin", "", true);
const isOperator = () => !!adminToken();

// Referee and engine codes in plain English, each with the fix.
const CODE_MSG = {
  SIZE: "Too big for this wallet. Lower the size (max 25% of the balance, within the per-trade cap).",
  BALANCE: "Not enough USDC in this wallet. Add funds or lower the size.",
  OPEN_POSITION: "This wallet already has an open trade. Wait for it to close, or exit it first.",
  DAILY_LIMIT: "Daily trade limit reached. Try again tomorrow or raise the limit.",
  STOP_LOSS: "The stop loss must sit below the price. Lower it a little.",
  TAKE_PROFIT: "Take profit is too close to the price. Raise it.",
  RISK: "Stop loss is too far below the price. Make it smaller.",
  TOKEN: "This coin isn't on the allowed list. Pick another coin.",
  RISK_FLAGGED: "Flash flags this coin as risky to sell. Pick another coin.",
  LIQUIDITY: "Not enough liquidity to exit safely. Try another coin or come back later.",
  CIRCUIT_BREAKER: "This agent is paused after a big drop. Copying resumes when it recovers.",
  HALTED: "Emergency stop is on. Resume trading on the Operator page first.",
  WALLET_PAUSED: "This wallet is paused. Resume it on the Wallets page.",
  LOSS_LIMIT: "This wallet hit its loss limit and paused itself. Review it on Wallets.",
  LIVE_BUDGET: "The live-money budget is used up. Raise LIVE_BUDGET_USD on the server.",
  LIVE_AGENTS_OFF: "Agents don't trade real money while LIVE_AGENTS is off.",
  ORDER_TYPE: "That order type isn't allowed. Every order needs a stop loss.",
  SEASON_CLOSED: "The season is closed. Trading opens with the next one.",
  PRICE: "No live price yet. Try again in a few seconds.",
  WALLET: "That wallet doesn't exist any more. Refresh and pick another.",
};
function friendlyError(status, data) {
  const d = data?.decision;
  const reason = String(d?.reason || data?.error?.message || (typeof data?.error === "string" ? data.error : "") || "");
  const code = d?.code || data?.error?.code || "";
  const detail = /\d/.test(reason) && reason.length < 90 ? ` (${reason})` : "";
  if (CODE_MSG[code]) return CODE_MSG[code] + detail;
  if (/cost guard/i.test(reason)) { const m = reason.match(/\(([\d.]+%), limit ([\d.]+%)\)/); return `Fees are too high for this size${m ? ` (${m[1]}, limit ${m[2]})` : ""}. Try a bigger trade.`; }
  if (/price guard/i.test(reason)) return "Flash's price is too far from the onchain price right now. Try again in a minute.";
  if (status === 429) return "Too many requests. Wait a few seconds, then try again.";
  if (!reason && (status === 502 || status === 503 || status === 504)) return "The server or Flash is busy. Try again in a moment.";
  if (!reason && status >= 500) return "Something went wrong on the server. Try again.";
  return reason ? cap(reason) : `Request failed (${status})`;
}

async function api(path, { method = "GET", body, admin = false, me = false, timeout = 20000, quiet401 = false } = {}) {
  const headers = { Accept: "application/json" };
  if (body) headers["Content-Type"] = "application/json";
  if (admin) headers.Authorization = "Bearer " + adminToken();
  if (me) headers["X-Arena-Session"] = meToken();
  const ctl = "AbortController" in window ? new AbortController() : null;
  const timer = ctl ? setTimeout(() => ctl.abort(), timeout) : 0;
  let res;
  try {
    res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, signal: ctl?.signal });
  } catch (e) {
    clearTimeout(timer);
    if (e?.name === "AbortError") { const err = new Error("This is taking too long. Try again."); err.timeout = true; throw err; }
    setApiDown(true);
    const err = new Error(navigator.onLine ? "Can't reach the server. Retrying in the background." : "You're offline. Check your connection.");
    err.network = true;
    throw err;
  }
  clearTimeout(timer);
  setApiDown(false);
  let data = null;
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    if (res.status === 401 && me) {
      dropSession(wallet.address);
      const err = new Error("Sign in with your wallet again.");
      err.status = 401;
      throw err;
    }
    if (res.status === 401 && admin) {
      store.del("arena.admin", true);
      updateChrome();
      if (!quiet401) openSignIn("Your operator token expired or was rejected. Sign in again.");
      const err = new Error("Sign in again to do that.");
      err.status = 401;
      throw err;
    }
    const err = new Error(friendlyError(res.status, data));
    err.data = data;
    err.status = res.status;
    err.code = data?.decision?.code || "";
    throw err;
  }
  return data;
}

// Server unreachable: a banner, then retries with backoff (2s, 4s ... 30s).
let downTimer = 0, downTick = 0, downDelay = 2000;
function setApiDown(on) {
  if (on === !!state.apiDown) return;
  state.apiDown = on;
  const b = $("[data-banner-down]");
  if (b) b.hidden = !on || !navigator.onLine;
  if (on) { scheduleRetry(); return; }
  clearTimeout(downTimer); clearInterval(downTick); downDelay = 2000;
  // Back online: refresh what's on screen.
  if ($("#app .empty-err")) route(); else current?.onStats?.();
}
function scheduleRetry() {
  clearTimeout(downTimer); clearInterval(downTick);
  let left = Math.round(downDelay / 1000);
  const txt = $("[data-down-text]");
  const show = () => { if (txt) txt.textContent = `Can't reach the server. Retrying in ${left}s`; };
  show();
  downTick = setInterval(() => { left = Math.max(0, left - 1); show(); }, 1000);
  downTimer = setTimeout(pingServer, downDelay);
  downDelay = Math.min(downDelay * 2, 30000);
}
async function pingServer() {
  clearTimeout(downTimer); clearInterval(downTick);
  const txt = $("[data-down-text]");
  if (txt) txt.textContent = "Reconnecting…";
  await refreshStats();
  if (state.apiDown) scheduleRetry();
}

// Disable a submit button and show a spinner until the work is done.
function busy(btn, label = "Working…") {
  if (!btn) return () => {};
  const old = btn.innerHTML, was = btn.disabled;
  btn.disabled = true; btn.classList.add("is-busy"); btn.setAttribute("aria-busy", "true"); btn.textContent = label;
  const slow = setTimeout(() => { if (btn.isConnected) btn.textContent = "Still working…"; }, 8000);
  return () => { clearTimeout(slow); if (!btn.isConnected) return; btn.classList.remove("is-busy"); btn.removeAttribute("aria-busy"); btn.innerHTML = old; btn.disabled = was; };
}

// Number fields: required, finite, not negative, inside min/max. Shown inline.
function numErr(inp) {
  const v = inp.value.trim();
  if (v === "") return "Required";
  const n = Number(v);
  if (!Number.isFinite(n)) return "Numbers only";
  if (n < 0) return "Can't be negative";
  if (inp.min !== "" && n < +inp.min) return `At least ${inp.min}`;
  if (inp.max !== "" && n > +inp.max) return `At most ${inp.max}`;
  return "";
}
function checkNumbers(form, all = true) {
  let first = null;
  $$("input[type=number]", form).forEach((inp) => {
    const skip = !!inp.closest("[hidden]") || inp.disabled;
    const msg = skip ? "" : numErr(inp);
    if (msg && !first) first = inp;
    if (!all && !inp.dataset.touched) return;
    inp.setAttribute("aria-invalid", msg ? "true" : "false");
    const field = inp.closest(".field") || inp.parentElement;
    let el = field && $(".field-err", field);
    if (!el && field) { el = document.createElement("span"); el.className = "field-err"; el.setAttribute("aria-live", "polite"); field.append(el); }
    if (el) el.textContent = msg;
  });
  return first;
}
function liveNumbers(form) {
  form.addEventListener("input", (e) => {
    if (!e.target.matches?.("input[type=number]")) return;
    e.target.dataset.touched = "1";
    checkNumbers(form, false);
  });
  clampPct(form);
}
// Percentages snap back inside their range when the field is left.
function clampPct(form) {
  form.addEventListener("change", (e) => {
    const t = e.target;
    if (!t.matches?.("input[type=number]") || !/Pct$/.test(t.name) || t.value.trim() === "") return;
    const n = Number(t.value);
    if (!Number.isFinite(n)) return;
    const lo = t.min !== "" ? +t.min : 0, hi = t.max !== "" ? +t.max : Infinity;
    const c = Math.min(hi, Math.max(lo, n));
    if (c !== n) { t.value = String(c); t.dispatchEvent(new Event("input", { bubbles: true })); toast(`Set to ${c}%, the allowed limit.`); }
  });
}

/* ---------------- app state ---------------- */
const state = {
  stats: null,
  price: null,
  flash: null,
  board: null,
  feed: [],
  pendingApprovals: 0,
  sse: "connecting",
  watch: new Set(store.get("arena.watch", [])),
  voter: store.get("arena.voter") || (() => { const id = "v_" + crypto.getRandomValues(new Uint32Array(2)).join(""); store.set("arena.voter", id); return id; })(),
  installPrompt: null,
  apiDown: false,
};
// Ranks as this device last saw them, for the movement arrows.
const rankBase = store.get("arena.ranks")?.r || null;
const saveRanks = () => { if (state.board?.rows?.length) store.set("arena.ranks", { at: Date.now(), r: Object.fromEntries(state.board.rows.map((r) => [r.agentId, r.rank])) }); };
addEventListener("pagehide", saveRanks);
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "hidden") saveRanks(); });

const PRICE_SOURCE = {
  "uniswap-v3": { chip: "Onchain price: Uniswap v3 WETH/USDC pool on Base", line: "Onchain · Uniswap v3 WETH/USDC on Base" },
  "flash-search": { chip: "Price from the Definitive Flash market feed", line: "Definitive Flash market feed" },
};
const priceSource = () => state.price?.source || state.flash?.priceSource || "";

/* ---------------- scroll effects ---------------- */
// Progressive enhancement only. Content is hidden for a reveal only after JS
// has armed it (class .rv) with a working IntersectionObserver; reduced motion,
// a missing observer or an observer that never reports all show content at once.
const root = document.documentElement;
let io = null, ioSeen = false, routeAt = 0;
if (!reduceMotion && "IntersectionObserver" in window) {
  try {
    io = new IntersectionObserver((entries) => {
      ioSeen = true;
      let k = 0; // stagger whatever scrolls into view together
      for (const en of entries) {
        if (!en.isIntersecting) continue;
        io.unobserve(en.target);
        en.target.style.setProperty("--d", Math.min(k++, 8) * 60 + "ms");
        enter(en.target);
      }
    }, { rootMargin: "0px 0px -6% 0px", threshold: 0.06 });
    root.classList.add("js-reveal");
  } catch { io = null; }
}
addEventListener("beforeprint", () => $$(".rv:not(.is-in)").forEach(enter));

function enter(el) {
  el.classList.add("is-in");
  if (el.hasAttribute("data-countup")) runCount(el);
}

function arm(node, instant) {
  if (node.nodeType !== 1) return;
  const lists = (node.matches("[data-stagger]") ? [node] : []).concat($$("[data-stagger]", node));
  for (const list of lists) for (const c of list.children) if (!c.hasAttribute("data-reveal")) c.setAttribute("data-reveal", "");
  const sel = "[data-reveal],[data-countup]";
  const els = (node.matches(sel) ? [node] : []).concat($$(sel, node)).filter((el) => !el.classList.contains("rv") && !el.classList.contains("is-in"));
  if (!els.length) return;
  if (!io || instant) { els.forEach(enter); return; }
  els.forEach((el) => {
    el.classList.add("rv");
    if (el.hasAttribute("data-countup") && known(el.dataset.countup)) el.textContent = (FMT[el.dataset.fmt] || FMT.int)(0);
    io.observe(el);
  });
  setTimeout(() => { if (!ioSeen) els.forEach(enter); }, 1500); // observer broken: show everything
}

// Content added right after a route change animates; later updates
// (polling, live posts) appear instantly so nothing flickers.
new MutationObserver((muts) => {
  const instant = performance.now() - routeAt > 1600;
  for (const m of muts) for (const n of m.addedNodes) arm(n, instant);
}).observe($("#app"), { childList: true, subtree: true });

function runCount(el) {
  const f = FMT[el.dataset.fmt] || FMT.int;
  const raw = el.dataset.countup;
  if (!known(raw)) { el.dataset.waiting = "1"; return; }
  delete el.dataset.waiting;
  if (!el.classList.contains("rv") || reduceMotion) { el.textContent = f(+raw); return; }
  const start = performance.now(), dur = 1100;
  const step = (t) => {
    const k = Math.min(1, (t - start) / dur), e = 1 - Math.pow(1 - k, 4);
    const to = +el.dataset.countup;
    el.textContent = f(k < 1 ? to * e : to);
    if (k < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}
// Update a count-up number after the first render (no replay).
function setCount(el, v) {
  if (!el) return;
  el.dataset.countup = known(v) ? String(+v) : "";
  const armedButHidden = el.classList.contains("rv") && !el.classList.contains("is-in");
  if (armedButHidden) return; // it counts when it scrolls into view
  if (el.dataset.waiting && known(v)) { runCount(el); return; }
  if (known(v)) el.textContent = (FMT[el.dataset.fmt] || FMT.int)(+v);
}
const countAttr = (v, f) => `data-countup="${known(v) ? +v : ""}" data-fmt="${f}"`;
const countText = (v, f) => (known(v) ? esc((FMT[f] || FMT.int)(+v)) : "—");

const bar = $(".progress i");
const cssProgress = !reduceMotion && !!window.CSS?.supports?.("animation-timeline: scroll()");
if (cssProgress) root.classList.add("css-progress");
let ticking = false, scrolled = null;
function onScroll() {
  if (ticking) return;
  ticking = true;
  requestAnimationFrame(() => {
    ticking = false;
    const y = window.scrollY;
    const s = y > 24;
    if (s !== scrolled) { scrolled = s; root.classList.toggle("is-scrolled", s); }
    if (!cssProgress && bar) {
      const max = root.scrollHeight - window.innerHeight;
      bar.style.setProperty("transform", `scaleX(${max > 0 ? Math.min(1, y / max).toFixed(4) : 0})`);
    }
  });
}
addEventListener("scroll", onScroll, { passive: true });
addEventListener("resize", onScroll, { passive: true });

/* ---------------- chrome: toasts, dialog, header ---------------- */
// One container, kept by reference: a sheet that re-renders its content can
// drop it from the page, and toastHome puts it back.
const TOASTS = document.querySelector(".toasts");
function toast(msg, kind = "", action) {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.setAttribute("role", kind === "err" ? "alert" : "status");
  el.innerHTML = `<span>${esc(msg)}</span>`;
  if (action) {
    const b = document.createElement("button");
    b.className = "btn btn-sm btn-ghost";
    b.textContent = action.label;
    b.addEventListener("click", () => { action.run(); el.remove(); });
    el.append(b);
  }
  const box = TOASTS;
  toastHome(box);
  box.append(el);
  setTimeout(() => el.remove(), action ? 12000 : 4500);
}
// A modal sheet sits in the browser's top layer and makes the rest of the
// page inert, so toasts outside it would be hidden behind it and not read
// out. While a modal is open the toasts live inside it; they move back to
// the page as it closes.
function toastHome(box = TOASTS) {
  const modal = [...document.querySelectorAll("dialog[open]")].reverse().find((d) => { try { return d.matches(":modal"); } catch { return true; } });
  const host = modal || document.body;
  if (box.parentElement !== host) host.append(box);
}
document.addEventListener("close", () => toastHome(), true);

// Money actions always show which money they use: paper, dry run or live.
function confirmDialog(title, body, okLabel = "Confirm", { money = false } = {}) {
  const dlg = $("[data-dialog]");
  const mode = money ? modeOf() : "";
  if (!dlg.showModal) return Promise.resolve(window.confirm((mode ? `[${MODE_INFO[mode].badge}] ` : "") + title + "\n\n" + body));
  $("[data-dialog-title]").textContent = title;
  $("[data-dialog-body]").textContent = body;
  const ok = $("[data-dialog-ok]");
  ok.textContent = okLabel;
  ok.classList.toggle("is-danger", mode === "live");
  const pill = $("[data-dialog-mode]");
  if (pill) {
    pill.hidden = !mode;
    pill.innerHTML = mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode].badge)}</span>` : "";
  }
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((resolve) => dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok"), { once: true }));
}

/* Trading mode: paper (mock money, real Flash quotes), dry (signed, not sent)
   or live (real money). Older servers only report dryRun. */
const MODE_INFO = {
  paper: { badge: "Paper money", mast: "Paper money · real quotes, simulated fills", title: "Real Flash quotes, simulated fills, no real money" },
  dry: { badge: "Dry run", mast: "Dry run · signed, not sent", title: "Orders are quoted and signed, but not sent to Flash" },
  live: { badge: "Live · real money", mast: "Live · real orders", title: "Orders go to Flash and spend real USDC on Base" },
};
function modeOf() {
  const m = state.flash?.mode || state.stats?.mode;
  if (MODE_INFO[m]) return m;
  if (!state.stats && !state.flash) return "";
  return (state.stats ? state.stats.dryRun : state.flash.dryRun) ? "dry" : "live";
}
const enc = encodeURIComponent;
const agentName = (id) => state.board?.rows?.find((r) => r.agentId === id)?.name || id;
// Integrator fee on every Flash order: "$0.42 · 0.10% a trade".
const feesText = () => { const f = state.stats?.fees; if (!f) return "—"; return `${usd(f.earnedUsd)} · ${fixed((+f.bps || 0) / 100, 2)}% a trade${f.paper ? " · paper" : ""}`; };

function updateChrome() {
  const s = state.stats;
  const mode = modeOf();
  const badge = $("[data-mode]");
  badge.hidden = !mode;
  if (mode) {
    badge.textContent = MODE_INFO[mode].badge;
    badge.title = MODE_INFO[mode].title;
    badge.dataset.state = mode;
  }
  const halted = !!state.flash?.halted;
  $("[data-banner-halt]").hidden = !halted;
  $("[data-resume]").hidden = !(halted && isOperator());
  $("[data-banner-offline]").hidden = navigator.onLine;
  $("[data-banner-down]").hidden = !state.apiDown || !navigator.onLine;
  $("[data-mast-season]").textContent = s?.season ? `Season ${s.season}` : "Season —";
  const mm = $("[data-mast-mode]");
  if (mode) {
    mm.textContent = MODE_INFO[mode].mast;
    mm.dataset.state = mode;
  }
  const p = state.price;
  const chip = $("[data-price]");
  chip.textContent = `${p?.symbol || "WETH"} ${p?.last ? price(p.last) : "—"}`;
  chip.title = PRICE_SOURCE[priceSource()]?.chip || "WETH price";
  chip.dataset.fresh = p ? (p.fresh ? "1" : "0") : "";
  const live = $("[data-live]");
  live.dataset.state = state.sse === "open" ? "on" : state.sse === "closed" ? "off" : "";
  $("[data-live-text]").textContent = state.sse === "open" ? "Live" : state.sse === "closed" ? "Reconnecting" : "Connecting";
  $$("[data-approvals-count]").forEach((el) => { el.hidden = !state.pendingApprovals; el.textContent = state.pendingApprovals; });
}

/* ---------------- live data: SSE + polling ---------------- */
let es, esRetry = 1000;
function connectFeed() {
  if (es) es.close();
  try { es = new EventSource("/v1/feed/stream"); } catch { return; }
  es.onopen = () => { state.sse = "open"; esRetry = 1000; updateChrome(); };
  es.addEventListener("post", (ev) => {
    let p;
    try { p = JSON.parse(ev.data); } catch { return; }
    addPost(p);
  });
  es.onerror = () => {
    state.sse = "closed";
    updateChrome();
    es.close();
    setTimeout(connectFeed, esRetry);
    esRetry = Math.min(esRetry * 2, 30000);
  };
}

function addPost(p) {
  if (!p?.id || state.feed.some((x) => x.id === p.id)) return;
  state.feed.unshift(p);
  state.feed = state.feed.slice(0, 300);
  onPost(p);
}

/* Polling fallback. Some proxies (e.g. Cloudflare quick tunnels) accept the
   SSE connection but buffer it, so posts never arrive. Merge recent posts
   every 10s; addPost() drops the ones SSE already delivered. */
let feedSeeded = false;
async function pollFeed() {
  if (document.visibilityState !== "visible") return;
  if (!feedSeeded) { await loadFeedOnce(); return; } // first load: no toasts for old posts
  let posts;
  try { posts = (await api("/v1/feed?limit=50")).posts; } catch { return; }
  for (const p of posts.slice().reverse()) addPost(p); // oldest first, newest ends on top
}

function onPost(p) {
  if (p.kind === "approval" && p.data?.status === "pending" && isOperator()) {
    toast("A copy trade is waiting for approval.", "", { label: "Review", run: () => (location.hash = "#/admin/approvals") });
    refreshApprovals();
  }
  if (state.watch.has(p.agentId) && (p.kind === "trade_placed" || p.kind === "trade_closed")) {
    toast(p.text, "ok");
    notifyWatched(p);
  }
  document.dispatchEvent(new CustomEvent("arena:post", { detail: p }));
}

async function notifyWatched(p) {
  if (!("Notification" in window) || Notification.permission !== "granted" || document.visibilityState === "visible") return;
  try {
    const reg = await navigator.serviceWorker?.getRegistration();
    reg?.showNotification("Agent Arena", { body: p.text, icon: "/icons/icon-192.png", tag: p.id });
  } catch { /* ignore */ }
}

async function refreshStats() {
  await Promise.all([
    api("/v1/stats").then((d) => { state.stats = d; }).catch(() => { /* keep last */ }),
    api("/v1/price").then((d) => { state.price = d; }).catch(() => { /* keep last */ }),
    api("/v1/flash/status").then((d) => { state.flash = d; }).catch(() => { /* older server */ }),
  ]);
  updateChrome();
}
async function refreshApprovals() {
  if (!isOperator()) { state.pendingApprovals = 0; updateChrome(); return; }
  try { state.pendingApprovals = (await api("/v1/admin/approvals", { admin: true })).approvals.length; } catch { state.pendingApprovals = 0; }
  updateChrome();
}
async function loadFeedOnce() {
  try { state.feed = (await api("/v1/feed?limit=200")).posts || []; feedSeeded = true; } catch { /* offline */ }
}
async function loadBoard() {
  state.board = await api("/v1/leaderboard");
  return state.board;
}

/* ---------------- small views ---------------- */
// Line chart. pathLength=1 lets CSS draw it in with stroke-dashoffset.
function lineChart(vals, { w = 84, h = 34, pad = 3, cls = "spark", label = "" } = {}) {
  vals = (vals || []).map(Number).filter(Number.isFinite);
  const aria = label ? `role="img" aria-label="${esc(label)}"` : 'aria-hidden="true"';
  if (vals.length < 2) return `<svg class="${cls} flat" viewBox="0 0 ${w} ${h}" ${aria}><path class="base" d="M0 ${h / 2}H${w}"/></svg>`;
  const min = Math.min(...vals), max = Math.max(...vals), span = max - min || 1;
  const xy = vals.map((v, i) => [(i / (vals.length - 1)) * w, h - pad - ((v - min) / span) * (h - 2 * pad)]);
  const d = xy.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)} ${y.toFixed(1)}`).join("");
  const tone = vals.at(-1) >= vals[0] ? "is-up" : "is-down";
  return `<svg class="${cls} draw ${tone}" viewBox="0 0 ${w} ${h}" ${aria} data-reveal><path class="area" d="${d}L${w} ${h}L0 ${h}Z"/><path class="line" pathLength="1" d="${d}"/></svg>`;
}
const sparkline = (points) => lineChart((points || []).map((p) => p.usd));

const KIND = {
  trade_placed: ["Buy", "buy"], trade_filled: ["Fill", "buy"], trade_closed: ["Exit", "exit"],
  copy_placed: ["Copy", "copy"], trade_blocked: ["Block", "block"], copy_blocked: ["Block", "block"],
  approval: ["Vote", "vote"], follow: ["Follow", "copy"],
  signal_paused: ["Signal", "note"], // a signal while real-money auto-trading is switched off: information, not an error
};
const kindTag = (k) => { const [label, cls] = KIND[k] || ["Note", "note"]; return `<span class="tag tag-${cls}">${esc(label)}</span>`; };

// The server prefixes paper-money posts with "[paper] ".
const paperText = (text) => {
  const t = String(text ?? "");
  return t.startsWith("[paper] ") ? { paper: true, text: t.slice(8) } : { paper: false, text: t };
};

function postHTML(p, isNew = false) {
  const link = p.tradeId ? ` <a class="post-link" href="#/trade/${encodeURIComponent(p.tradeId)}">Trade</a>` : "";
  const pt = paperText(p.text);
  return `<li class="post${isNew ? " is-new" : ""}" data-kind="${esc(p.kind)}">
    ${kindTag(p.kind)}
    <div class="post-text">${pt.paper ? '<span class="mini mini-paper">Paper</span>' : ""}${esc(pt.text)}${link}</div>
    <time class="post-time" datetime="${esc(p.createdAt)}">${esc(ago(p.createdAt))}</time></li>`;
}

const STATUS = {
  placed: "Placed", filled: "Live", closed: "Closed", cancelled: "Cancelled", rejected: "Rejected",
  terminated: "Terminated", unprotected: "Unprotected", dryrun: "Dry run", pending: "Waiting",
  approved: "Approved", expired: "Expired",
};
const statusChip = (s) => `<span class="chip chip-${esc(s)}">${esc(STATUS[s] || cap(s || "unknown"))}</span>`;

const REASONS = {
  REASON_FULLY_FILLED: "Fully filled",
  REASON_USER_REQUESTED: "Cancelled by the operator",
  REASON_INSUFFICIENT_FUNDER_BALANCE: "Wallet didn't have enough USDC",
  REASON_INSUFFICIENT_ASSET_BALANCE: "Wallet didn't hold enough of the token",
  REASON_MAXIMUM_SLIPPAGE_EXCEEDED: "Price moved past the slippage limit",
  REASON_EXECUTION_COST_EXCEEDS_LIMIT: "Execution cost was over the limit",
  REASON_ORDER_REJECTED_AT_VENUE: "Rejected by the trading venue",
  REASON_ORDER_TRIGGERED_ON_ENTRY: "Exit level was already crossed at entry",
  REASON_ORDER_EXPIRED: "Order expired",
  REASON_PRICE_IMPACT_TOO_HIGH: "Price impact was too high",
  REASON_INSUFFICIENT_LIQUIDITY: "Not enough liquidity",
  REASON_ORDER_CANNOT_BE_PLACED: "Flash could not place the order",
};
const reasonText = (code) => (code ? REASONS[code] || cap(String(code).replace(/^REASON_/, "").toLowerCase()) : "");
const approveText = (h) => (isHash(h) ? txLink(h) : h === "paper" ? '<span class="mono muted">simulated (paper)</span>' : h === "dryrun" ? '<span class="mono muted">not sent (dry run)</span>' : '<span class="muted">not needed</span>');
const PROTECTION = { pending: "Waiting for the entry to fill", active: "Take profit and stop loss live on Flash", none: "No exit order" };

// Empty states: an icon, one line, one button.
const ICON_EMPTY = '<svg class="empty-ico" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="5" width="16" height="14" rx="3"/><path d="M8 10h8M8 14h5"/></svg>';
const ICON_ERR = '<svg class="empty-ico" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8.5"/><path d="M12 7.5v5.5M12 16.2v.3"/></svg>';
function emptyState(title, body, cta) {
  return `<div class="empty">${ICON_EMPTY}<h3>${esc(title)}</h3>${body ? `<p class="muted small">${esc(body)}</p>` : ""}${cta ? `<a class="btn btn-ghost btn-sm" href="${esc(cta.href)}">${esc(cta.label)}</a>` : ""}</div>`;
}
function errorState(err, retryHash) {
  return `<div class="empty empty-err">${ICON_ERR}<h3>${esc(err.message || "Couldn't load this")}</h3><a class="btn btn-ghost btn-sm" href="${esc(retryHash || location.hash || "#/")}" data-retry>Try again</a></div>`;
}

function moveHTML(agentId, rank) {
  const before = rankBase?.[agentId];
  if (!Number.isFinite(before)) return `<span class="move flat" title="No earlier rank on this device">–</span>`;
  const d = before - rank;
  if (!d) return `<span class="move flat" title="Same rank as your last visit">–</span>`;
  return d > 0
    ? `<span class="move up" title="Up ${d} since your last visit">▲${d}</span>`
    : `<span class="move down" title="Down ${-d} since your last visit">▼${-d}</span>`;
}

/* ---------------- pages ---------------- */
// Home page content. Icons are inline SVG (stroke = currentColor, CSP safe).
const ICON = {
  proof: '<svg viewBox="0 0 24 24"><path d="M6 3.5h12v17l-3-2-3 2-3-2-3 2z"/><path d="M9 8.5h6M9 12h6M9 15.5h3"/></svg>',
  shield: '<svg viewBox="0 0 24 24"><path d="M12 3 5 6v5.5c0 4.4 3 7.9 7 9.5 4-1.6 7-5.1 7-9.5V6z"/><path d="m9 12 2 2 4-4"/></svg>',
  fuel: '<svg viewBox="0 0 24 24"><path d="M12 3.5s-5.5 6.3-5.5 10.2A5.5 5.5 0 0 0 12 19.2a5.5 5.5 0 0 0 5.5-5.5C17.5 9.8 12 3.5 12 3.5z"/><path d="M4 4l16 16"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><rect x="8.5" y="8.5" width="11" height="11" rx="2.5"/><path d="M15.5 8.5V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7.5a2 2 0 0 0 2 2h2.5"/></svg>',
  spark: '<svg viewBox="0 0 24 24"><path d="M12 3v4M12 17v4M3 12h4M17 12h4M6 6l2.5 2.5M15.5 15.5 18 18M6 18l2.5-2.5M15.5 8.5 18 6"/><circle cx="12" cy="12" r="2.5"/></svg>',
  share: '<svg viewBox="0 0 24 24"><circle cx="6" cy="12" r="2.5"/><circle cx="17.5" cy="6" r="2.5"/><circle cx="17.5" cy="18" r="2.5"/><path d="m8.3 10.8 7-3.6M8.3 13.2l7 3.6"/></svg>',
  bolt: '<svg viewBox="0 0 24 24"><path d="M13 3 5 13.5h6L10 21l8-10.5h-6z"/></svg>',
  chart: '<svg viewBox="0 0 24 24"><path d="M4 19h16"/><path d="m5 15 4-4 3 3 6-7"/><path d="M15 7h3v3"/></svg>',
};
const WHY = [
  ["proof", "Real trades, real proof", "Every order goes through Definitive Flash on Base. Each one links to its Flash order and its Basescan transaction.", "#/proof", "See the proof"],
  ["shield", "Your money, your limits", "Your own sub-wallet, a stop loss on every trade, a daily cap and a loss limit that pauses it. Withdrawals only go to you.", "#/me", "Open my wallet"],
  ["fuel", "No gas to worry about", "Add USDC with one free signature. Flash pays the gas of every order and the arena pays the approvals.", "#/me", "Add funds"],
  ["copy", "Copy or invest in a tap", "Mirror an agent's trades or trade every signal, with your own size, daily cap and pause button.", "#/leaderboard", "Pick an agent"],
  ["spark", "Build your own agent", "Pick a brain, a coin, a target and a stop. Test it on real onchain prices with Flash's real costs before it goes live.", "#/me", "Make my agent"],
  ["share", "Social by design", "A risk-adjusted leaderboard, share cards, predictions, watch alerts and public signals.", "#/signals", "Live signals"],
];
const STEPS = [
  ["Connect your wallet", "MetaMask, Coinbase Wallet or Rabby on Base. Sign in with a free message."],
  ["Add USDC, no gas", "One free signature moves USDC into your own Arena account. Only you can withdraw it."],
  ["Pick or make an agent", "Copy a leader from the leaderboard, or build your own and test it on real prices."],
  ["Watch, share, withdraw", "Live charts for every open trade. Pause or withdraw to your wallet whenever you like."],
];
const FLOW = [
  ["spark", "Signal", "A strategy reads the onchain price every 15 seconds and decides to buy."],
  ["shield", "Referee", "Stop loss, size, daily limit, liquidity, price and cost checks. Anything off is blocked before signing."],
  ["bolt", "Flash executes", "The order is checked field by field, signed and sent to Definitive Flash with a take-profit and stop-loss bracket."],
  ["copy", "Followers copy", "Copiers and investors get the same trade in their own sub-wallets, within their own limits."],
  ["chart", "Exit and proof", "The bracket closes the trade. Profit and loss come from Flash's fills, and every step links onchain."],
];
const SECURE = [
  ["Separate sub-wallet", "Each follower, agent and plan trades from its own wallet, never from yours."],
  ["Payout locked to you", "Withdrawals go only to your address. The app cannot change it."],
  ["Capped approvals", "Flash may pull only the trade size, never an open-ended amount."],
  ["No blind signing", "Every Flash order is checked field by field before it is signed."],
  ["Stop loss on every trade", "Plus your daily cap and a loss limit that pauses copying."],
  ["Emergency stop", "One switch cancels open orders and halts all trading."],
];
const FAQ = [
  ["Is this real money?", "It can be. The arena runs in paper mode (mock money with real Flash quotes) or live mode on Base. The badge at the top shows which one you are using."],
  ["What does a trade cost?", "Flash's all-in cost is part of each quote, usually about $0.02 to $0.05 per order, plus a 0.10% arena fee. Agents skip trades when the cost is too high."],
  ["Can the arena take my money?", "Withdrawals only go to the address you signed in with, and the app can never change it. Token approvals are capped at the trade size."],
  ["What if an agent loses?", "Every trade has a stop loss. You set a daily cap and a loss limit that pauses copying. Past results never promise future ones."],
  ["Can I build my own agent?", "Yes. Sign in, make an agent and test its rules on real onchain prices first. Others can then watch, copy or invest with it."],
  ["Which coins and chains?", "WETH and cbBTC on Base today. Flash itself supports 14 chains."],
];

// "How a trade runs": an auto-playing story of one real trade, step by step,
// each with its own animated illustration (inline SVG, CSS animations).
const HOW = [
  { ico: "spark", title: "Signal", line: "A strategy reads the onchain price every 15 seconds and decides to buy.",
    points: ["Onchain price, every 15 s", "Your rules: dip, trend, pullback", "Backtested on real prices"],
    art: `<svg viewBox="0 0 320 200" class="art"><path class="a-grid" d="M16 50H304M16 100H304M16 150H304"/>
      <path class="a-line a-draw" pathLength="1" d="M16 78 L52 70 L86 88 L118 82 L150 104 L182 132 L214 118 L246 96 L280 76 L304 66"/>
      <circle class="a-dot a-pop d1" cx="150" cy="104" r="5"/><circle class="a-dot a-pop d2" cx="182" cy="132" r="5"/>
      <g class="a-pin a-pop d3"><circle cx="182" cy="132" r="17"/><text x="182" y="136" text-anchor="middle">BUY</text></g>
      <text class="a-cap" x="16" y="190">2 falling checks → buy signal</text></svg>` },
  { ico: "shield", title: "Referee", line: "Every order is checked before anything is signed. Anything off is blocked.",
    points: ["Stop loss within 5%", "Size, daily and cost limits", "Price vs the Uniswap pool"],
    art: `<svg viewBox="0 0 320 200" class="art"><path class="a-shield" d="M54 30 22 44v34c0 26 15 46 32 54 17-8 32-28 32-54V44z"/><path class="a-tick a-draw" pathLength="1" d="m40 82 10 10 20-22"/>
      ${["Stop loss ≤ 5%", "Size & daily limit", "Exit liquidity", "Price vs Uniswap", "Flash cost ≤ limit"].map((t, i) => `<g class="a-row a-pop d${i + 1}"><rect x="112" y="${22 + i * 32}" width="190" height="24" rx="12"/><path class="a-ok" d="M${124} ${34 + i * 32} l5 5 9-9"/><text x="146" y="${38 + i * 32}">${t}</text></g>`).join("")}
      <text class="a-cap" x="16" y="190">All pass → sign</text></svg>` },
  { ico: "bolt", title: "Flash executes", line: "The order is checked field by field, signed (EIP-712) and sent to Definitive Flash with a bracket.",
    points: ["Market, limit, TWAP or stop", "Take profit + stop loss attached", "Flash pays the gas"],
    art: `<svg viewBox="0 0 320 200" class="art"><rect class="a-card" x="40" y="18" width="240" height="150" rx="18"/>
      <text class="a-h" x="60" y="50">LONG WETH</text><text class="a-big" x="60" y="86">$3.00</text>
      <g class="a-pop d1"><path class="a-tp" d="M60 110H260"/><text class="a-tpt" x="260" y="104" text-anchor="end">Take profit</text></g>
      <g class="a-pop d2"><path class="a-sl" d="M60 146H260"/><text class="a-slt" x="260" y="140" text-anchor="end">Stop loss</text></g>
      <g class="a-stamp a-pop d3"><rect x="186" y="30" width="80" height="26" rx="13"/><text x="226" y="48" text-anchor="middle">signed ✓</text></g>
      <path class="a-bolt a-pop d4" d="M296 120 284 138h10l-4 18 14-22h-10z"/><text class="a-cap" x="16" y="190">FlashOrder · Base · gas included</text></svg>` },
  { ico: "copy", title: "Followers copy", line: "Copiers and investors get the same trade in their own sub-wallets, within their own limits.",
    points: ["Same coin, target and stop", "Your size and daily cap", "Big copies need a yes"],
    art: `<svg viewBox="0 0 320 200" class="art">${[[70, 50, "$1.50"], [70, 150, "$5.00"], [250, 50, "$3.00"], [250, 150, "$2.50"]].map(([x, y, a], i) => `<path class="a-flow" d="M160 100 L${x} ${y}"/><g class="a-pop d${i + 1}"><circle class="a-node" cx="${x}" cy="${y}" r="24"/><text class="a-nt" x="${x}" y="${y + 4}" text-anchor="middle">${a}</text></g>`).join("")}
      <circle class="a-hub" cx="160" cy="100" r="32"/><text class="a-ht" x="160" y="104" text-anchor="middle">Agent</text><text class="a-cap" x="16" y="196">One signal → every follower's own wallet</text></svg>` },
  { ico: "chart", title: "Exit and proof", line: "The bracket closes the trade. Profit and loss come from Flash's fills, and every step links onchain.",
    points: ["Exit by target, stop or you", "P&L from real fills", "Basescan link for every tx"],
    art: `<svg viewBox="0 0 320 200" class="art"><path class="a-receipt" d="M80 14h160v172l-20-12-20 12-20-12-20 12-20-12-20 12-20-12-20 12z"/>
      <text class="a-h" x="100" y="46">Trade closed</text><g class="a-pop d1"><text class="a-win" x="100" y="86">+ take profit</text></g>
      <g class="a-pop d2"><text class="a-mono" x="100" y="116">tx 0x5d1d…c92c</text></g>
      <g class="a-badge a-pop d3"><rect x="100" y="132" width="120" height="26" rx="13"/><text x="160" y="150" text-anchor="middle">Basescan ✓</text></g></svg>` },
];

function howHTML() {
  return `<div class="how-wrap" data-how-wrap>
    <div class="how-steps" role="tablist" aria-label="Steps of a trade">${HOW.map((s, i) => `<button type="button" role="tab" class="how-step" data-step="${i}" aria-selected="${i === 0}" aria-controls="how-stage"><span class="how-ico" aria-hidden="true">${ICON[s.ico]}</span><span class="how-n num">${i + 1}</span><span class="how-t">${esc(s.title)}</span><i class="how-bar" aria-hidden="true"></i></button>`).join("")}</div>
    <div class="how-stage" id="how-stage" role="tabpanel" aria-live="polite" data-how-stage></div>
  </div>`;
}
function howStageHTML(i) {
  const s = HOW[i];
  return `<div class="how-art" aria-hidden="true">${s.art}</div>
    <div class="how-copy"><p class="eyebrow">Step ${i + 1} of ${HOW.length}</p><h3>${esc(s.title)}</h3><p>${esc(s.line)}</p>
      <ul class="how-points">${s.points.map((p) => `<li>${esc(p)}</li>`).join("")}</ul>
      <div class="how-nav"><button type="button" class="btn btn-ghost btn-sm" data-how-prev aria-label="Previous step">‹</button><button type="button" class="btn btn-ghost btn-sm" data-how-next aria-label="Next step">›</button></div></div>`;
}
// Auto-plays (not with reduced motion), pauses on hover / focus / touch,
// tap a step to jump, swipe the stage on phones. Returns a stop function.
function wireHow(scope) {
  const wrap = $("[data-how-wrap]", scope);
  if (!wrap) return null;
  const stage = $("[data-how-stage]", wrap), steps = $$(".how-step", wrap);
  const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
  let i = 0, timer = 0, paused = false;
  const show = (n, user) => {
    i = (n + HOW.length) % HOW.length;
    steps.forEach((b, k) => { b.setAttribute("aria-selected", String(k === i)); b.classList.toggle("is-done", k < i); });
    stage.innerHTML = howStageHTML(i);
    stage.classList.remove("is-in"); void stage.offsetWidth; stage.classList.add("is-in");
    $("[data-how-prev]", stage).addEventListener("click", () => show(i - 1, true));
    $("[data-how-next]", stage).addEventListener("click", () => show(i + 1, true));
    const strip = steps[i].parentElement; // scroll the strip sideways only, never the page
    if (strip.scrollWidth > strip.clientWidth) strip.scrollTo({ left: steps[i].offsetLeft - (strip.clientWidth - steps[i].offsetWidth) / 2, behavior: still ? "auto" : "smooth" });
    if (user) restart();
  };
  let onScreen = false;
  if ("IntersectionObserver" in window) new IntersectionObserver((es) => { onScreen = es.some((x) => x.isIntersecting); }, { threshold: 0.3 }).observe(wrap);
  else onScreen = true;
  const tick = () => { if (!paused && onScreen && document.visibilityState === "visible") show(i + 1); };
  const restart = () => {
    clearInterval(timer);
    wrap.classList.remove("is-playing"); void wrap.offsetWidth;
    if (!still) { wrap.classList.add("is-playing"); timer = setInterval(() => { tick(); wrap.classList.remove("is-playing"); void wrap.offsetWidth; wrap.classList.add("is-playing"); }, 4500); }
  };
  steps.forEach((b, k) => b.addEventListener("click", () => show(k, true)));
  wrap.addEventListener("pointerenter", () => { paused = true; wrap.classList.add("is-paused"); });
  wrap.addEventListener("pointerleave", () => { paused = false; wrap.classList.remove("is-paused"); });
  wrap.addEventListener("focusin", () => { paused = true; wrap.classList.add("is-paused"); });
  wrap.addEventListener("focusout", () => { paused = false; wrap.classList.remove("is-paused"); });
  let x0 = null;
  stage.addEventListener("touchstart", (e) => { x0 = e.touches[0].clientX; paused = true; }, { passive: true });
  stage.addEventListener("touchend", (e) => {
    paused = false;
    if (x0 == null) return;
    const dx = e.changedTouches[0].clientX - x0; x0 = null;
    if (Math.abs(dx) > 40) show(i + (dx < 0 ? 1 : -1), true);
  });
  show(0); restart();
  return () => clearInterval(timer);
}

async function pageHome(page) {
  page.innerHTML = `
  <section class="hero hero-ios">
    <div class="hero-copy">
      <p class="mode-note" data-mode-note hidden></p>
      <h1 class="display" data-reveal>AI agents trade.<br>You choose who to follow.</h1>
      <p class="lede" data-reveal>Copy, invest or build your own auto-trader on Definitive Flash.</p>
      <div class="btn-row hero-cta" data-reveal>
        <a class="btn btn-primary" href="#/leaderboard">Explore agents</a>
        <a class="btn btn-ghost" href="#/plans/new">Build an auto-trader</a>
      </div>
      <div class="hero-chips" data-stagger>
        <span class="hchip"><b class="num" data-tally="volumeUsd" data-countup data-fmt="cusd">—</b><span>Volume</span></span>
        <span class="hchip"><b class="num" data-tally="trades" data-countup data-fmt="int">—</b><span>Trades</span></span>
        <span class="hchip hchip-live"><b class="num" data-hero-px>—</b><span data-hero-px-sub>Flash price</span></span>
      </div>
    </div>
    <div class="standings glass" data-board aria-label="Top agents">
      <div class="skel skel-row"></div><div class="skel skel-row"></div><div class="skel skel-row"></div>
    </div>
  </section>

  <div class="ticker" data-ticker hidden aria-label="Latest arena events"><div class="ticker-track" data-ticker-track></div></div>

  <section class="section">
    <dl class="kpis" data-stagger>
      <div><dt>Copies</dt><dd class="num" data-tally="copies" data-countup data-fmt="int">—</dd></div>
      <div><dt>Open now</dt><dd class="num" data-tally="openTrades" data-countup data-fmt="int">—</dd></div>
      <div><dt>Fees earned</dt><dd class="num" data-fees>—</dd></div>
    </dl>
    <p class="footnote"><a class="link" href="#/proof">Onchain proof: real Flash orders with Base transactions</a></p>
  </section>

  <section class="section">
    <div class="section-head"><div><p class="eyebrow">Why Agent Arena</p><h2>Trading agents you can check, copy and build</h2></div></div>
    <div class="why-grid" data-stagger>${WHY.map(([ico, title, line, href, cta]) => `<a class="why" href="${href}"><span class="why-ico" aria-hidden="true">${ICON[ico]}</span><h3>${esc(title)}</h3><p>${esc(line)}</p><span class="why-cta">${esc(cta)} ›</span></a>`).join("")}</div>
  </section>

  <section class="section auto-show">
    <div class="auto-copy" data-reveal><p class="eyebrow">Automate your trades</p><h2>Make an agent once. It trades for you.</h2>
      <ol class="auto-list"><li><b>Pick a brain and a coin</b><span>Dip in an uptrend, dip buyer, trend or contrarian, on WETH or cbBTC.</span></li>
        <li><b>Test it on real prices</b><span>See where it would have bought and sold over the last hours, after Flash's costs.</span></li>
        <li><b>Launch</b><span>It watches the price every 15 seconds and trades through Flash with a take profit and stop loss.</span></li>
        <li><b>Others can copy it</b><span>Your agent joins the leaderboard. Pause or edit its rules any time.</span></li></ol>
      <div class="btn-row"><a class="btn btn-primary" href="#/me">Make my agent</a><a class="btn btn-ghost" href="#/leaderboard">See agents</a></div></div>
    <figure class="auto-video" data-reveal><video data-demo src="/media/agent-demo.webm" poster="/media/agent-demo.jpg" muted loop playsinline preload="metadata" aria-label="Screen recording: making an agent, testing it on real prices, launching it and watching its live trade"></video>
      <figcaption class="muted small">Recorded in the app (paper mode, real Flash quotes and real onchain prices).</figcaption></figure>
  </section>

  <section class="section gasless">
    <div class="section-head"><div><p class="eyebrow">Gasless by design</p><h2>You never need ETH</h2></div></div>
    <div class="gl-flow" data-stagger>
      <div class="gl-node"><span class="gl-ico" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="3.5" y="6.5" width="17" height="12" rx="2.5"/><path d="M15.5 12.5h2M7 3.5h10"/></svg></span><b>You sign</b><span>One free signature (EIP-3009). No transaction from your wallet.</span></div>
      <i class="gl-arrow" aria-hidden="true"></i>
      <div class="gl-node"><span class="gl-ico" aria-hidden="true">${ICON.fuel}</span><b>Gas tank sends it</b><span>The arena submits your USDC transfer and pays its gas.</span></div>
      <i class="gl-arrow" aria-hidden="true"></i>
      <div class="gl-node"><span class="gl-ico" aria-hidden="true">${ICON.bolt}</span><b>Flash pays every order</b><span>Buys, sells and brackets are sent by Flash executors, gas included.</span></div>
    </div>
    <div class="gl-proof" data-reveal><div><span class="gl-zero num">$0.00</span><span class="muted small">gas paid by you</span></div>
      <p class="small">Real example on Base: a 4 USDC deposit sent by the arena's gas tank for 0.0000006 ETH (about $0.0014). <a class="link" href="https://basescan.org/tx/0x3d9744562871bbc3444b000eea3a41ad726126fc816dbee4873f4d16285fd39f" ${EXT}>View on Basescan</a></p></div>
  </section>

  <section class="section secure">
    <div class="section-head"><div><p class="eyebrow">Secured by sub-wallets</p><h2>Agents never touch your main wallet</h2></div></div>
    <div class="sec-grid">
      <figure class="sec-art" data-reveal aria-hidden="true"><svg viewBox="0 0 520 260" class="art">
        <g class="s-box"><rect x="16" y="70" width="150" height="120" rx="22"/><text class="s-t" x="91" y="118" text-anchor="middle">Your wallet</text><text class="s-s" x="91" y="140" text-anchor="middle">keys stay with you</text></g>
        <path class="s-path" d="M166 110 C220 70 300 70 354 110"/><text class="s-lbl" x="260" y="64" text-anchor="middle">USDC in (one free signature)</text>
        <path class="s-path s-back" d="M354 150 C300 190 220 190 166 150"/><text class="s-lbl" x="260" y="206" text-anchor="middle">Out only to you · address locked</text>
        <g class="s-lock a-pop d2"><rect x="246" y="168" width="28" height="22" rx="5"/><path d="M252 168v-6a8 8 0 0 1 16 0v6"/></g>
        <g class="s-box s-sub"><rect x="354" y="70" width="150" height="120" rx="22"/><text class="s-t" x="429" y="118" text-anchor="middle">Sub-wallet</text><text class="s-s" x="429" y="140" text-anchor="middle">trades only via Flash</text></g>
        <g class="a-pop d3"><circle class="s-shield" cx="429" cy="58" r="18"/><path class="s-check" d="m421 58 6 6 10-11"/></g>
        <text class="a-cap" x="16" y="250">Agents trade the sub-wallet. Its money can only go back to you.</text></svg></figure>
      <ul class="sec-list" data-stagger>${SECURE.map(([t, d]) => `<li><b>${esc(t)}</b><span>${esc(d)}</span></li>`).join("")}</ul>
    </div>
  </section>

  <section class="section">
    <div class="section-head"><div><p class="eyebrow">Start in 4 steps</p><h2>From wallet to your first copied trade</h2></div></div>
    <ol class="shots" data-stagger>${STEPS.map(([title, line], i) => `<li class="shot"><figure class="phone"><img src="/media/step-${i + 1}.jpg" alt="${esc(title)}: a real screen of Agent Arena" width="390" height="720" loading="lazy" decoding="async"></figure><div class="shot-copy"><span class="steps4-no num">${i + 1}</span><div><h3>${esc(title)}</h3><p>${esc(line)}</p></div></div></li>`).join("")}</ol>
    <div class="btn-row"><a class="btn btn-primary" href="#/me">Open my wallet</a><a class="btn btn-ghost" href="#/docs">Read the docs</a></div>
  </section>

  <section class="section auto-teaser" data-auto>
    <div class="section-head"><h2>Auto-traders</h2><a class="btn btn-primary btn-sm" href="#/plans/new">Create</a></div>
    <div class="plan-grid plan-grid-sm" data-auto-list></div>
  </section>

  <section class="section market" data-market></section>

  <section class="section how" data-how>
    <div class="section-head"><div><p class="eyebrow">Under the hood</p><h2>How a trade runs</h2></div><span class="muted small how-hint">Tap a step · swipe on phones</span></div>
    ${howHTML()}
  </section>

  <section class="section share-band" data-share-band>
    <div class="share-copy" data-reveal><p class="eyebrow">Share &amp; follow</p><h2>Every agent and trade has a card</h2>
      <ul class="ticks"><li>Share cards for agents, trades and plans, with link previews on X, Telegram and Discord</li><li>Watch an agent and get an alert on every trade</li><li>Call it: vote whether a trade hits its target</li><li>Public signals by RSS, Discord and Telegram</li></ul>
      <div class="btn-row"><button type="button" class="btn btn-primary" data-share-top>Share the top agent</button><a class="btn btn-ghost" href="#/signals">See live signals</a></div></div>
    <div class="share-card" data-share-card data-reveal aria-hidden="true"></div>
  </section>

  <section class="section">
    <div class="section-head"><div><p class="eyebrow">Safety you can check</p><h2>Rules every order passes</h2></div>${infoBtn("mode")}</div>
    <dl class="rules" data-rules data-stagger></dl>
  </section>

  <section class="section">
    <div class="section-head"><div><p class="eyebrow">Questions</p><h2>Good to know</h2></div></div>
    <div class="faq" data-stagger>${FAQ.map(([q, a]) => `<details class="faq-item"><summary>${esc(q)}</summary><p>${esc(a)}</p></details>`).join("")}</div>
  </section>

  <section class="section cta-band on-lime" data-reveal>
    <div><h2>Start with $5. Stop any time.</h2><p>Your money stays in your own sub-wallet, and only your address can withdraw it.</p></div>
    <div class="btn-row"><a class="btn btn-primary" href="#/leaderboard">Pick an agent</a><a class="btn btn-ghost" href="#/me">Make my own</a></div>
  </section>`;

  // Share band: a live card of the current #1 agent.
  const shareBand = async () => {
    const card = $("[data-share-card]", page), btn = $("[data-share-top]", page);
    if (!card) return;
    const lb = await loadBoard().catch(() => null), top = lb?.rows?.find((r) => +r.trades > 0) || lb?.rows?.[0];
    if (!top) { card.hidden = true; return; }
    card.innerHTML = `<div class="sc-top"><span class="sc-brand">Agent Arena</span><span class="mini">#${esc(top.rank)}</span></div>
      <p class="sc-name">${esc(top.name)}</p><p class="sc-score num ${signClass(top.score) || "up"}">${esc(FMT.score(+top.score || 0))}</p><p class="sc-sub">Arena Score</p>
      <div class="sc-row num"><span>Return ${esc(pct(top.returnPct))}</span><span>Drop ${esc(fixed(top.maxDrawdownPct, 1))}%</span><span>${esc(plural(top.trades, "trade"))}</span></div>
      <p class="sc-foot">Trades on Base through Definitive Flash</p>`;
    btn?.addEventListener("click", () => openShare({
      url: shareURL("agent", top.agentId), text: `${top.name} on Agent Arena · Arena Score ${FMT.score(+top.score || 0)} · AI agent trading on Flash`, file: `arena-${top.agentId}.png`,
      card: () => drawCard({ title: top.name, big: FMT.score(+top.score || 0), down: +top.score < 0, line: `Return ${pct(top.returnPct)} · Biggest drop ${fixed(top.maxDrawdownPct, 1)}% · Rank ${top.rank}` }),
    }));
  };
  shareBand();
  // Demo video: plays (muted, looping) while on screen; with reduced motion it
  // shows controls instead of autoplaying. Hidden if the file is missing.
  const vid = $("[data-demo]", page);
  if (vid) {
    vid.addEventListener("error", () => vid.closest(".auto-video")?.remove(), { once: true });
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) vid.controls = true;
    else if ("IntersectionObserver" in window) {
      new IntersectionObserver((es) => es.forEach((en) => { if (en.isIntersecting) vid.play().catch(() => { vid.controls = true; }); else vid.pause(); }), { threshold: 0.35 }).observe(vid);
    }
  }
  const howStop = wireHow(page);

  const fill = () => {
    const s = state.stats;
    if (s) {
      $$("[data-tally]", page).forEach((el) => setCount(el, s[el.dataset.tally] ?? 0));
      const fe = $("[data-fees]", page), F = s.fees;
      if (fe) fe.innerHTML = F ? `${esc(cusd(F.earnedUsd))}<small>${esc(fixed((+F.bps || 0) / 100, 2))}% a trade${F.paper ? " · paper" : ""}</small>` : "—";
      const note = $("[data-mode-note]", page), mode = modeOf();
      void mode;
      note.hidden = true; // the header pill already shows Paper / Dry run / Live; one badge is enough
      const L = s.limits || {};
      const rules = [
        ["Stop loss on every order", "No exit, no order.", "Always"],
        ["Largest trade", "Agent, copy or plan.", usd(L.maxTradeUsd, 0)],
        ["Price check", "Flash quote vs onchain price.", known(L.maxPriceDeviationPct) ? `${L.maxPriceDeviationPct}%` : "—"],
        known(L.maxEntryCostPct) ? ["Entry cost cap", "Fees as a share of the trade.", `${L.maxEntryCostPct}%`] : null,
        ["Allowance cap", "Onchain, per wallet.", usd(L.maxApprovalUsd, 0)],
        ["Circuit breaker", "Pauses an agent this far below its peak.", known(L.maxAgentDrawdownPct) ? `${L.maxAgentDrawdownPct}%` : "—"],
        ["Human approval", "For copies above this size.", usd(L.approvalThresholdUsd, 0)],
        +L.maxMovePct > 0 ? ["Volatility guard", "No automated entries in a fast market.", `${L.maxMovePct}% in ${L.moveMinutes} min`] : null,
        +L.maxLossStreak > 0 ? ["Losing-streak pause", "Automated trading waits after losses in a row.", `${L.maxLossStreak} losses · ${L.lossStreakPauseMin} min`] : null,
        +L.dailyLossLimitUsd > 0 ? ["Daily loss kill switch", "Real-money trading stops for the day.", usd(L.dailyLossLimitUsd, 0)] : null,
        +L.maxHoldHours > 0 ? ["Time limit", "Automated positions close after this long.", `${L.maxHoldHours} h`] : null,
      ].filter(Boolean);
      const box = $("[data-rules]", page);
      const htmlRules = rules.map(([t, , v]) => `<div class="rule"><dt>${esc(t)}</dt><dd class="rule-val num">${esc(v)}</dd></div>`).join("");
      if (box.dataset.sig !== htmlRules) { box.dataset.sig = htmlRules; box.innerHTML = htmlRules; arm(box, box.dataset.armed === "1"); box.dataset.armed = "1"; }
    }
    renderMarket();
    renderQuote();
  };

  // Live Flash quote, streamed by the server over Flash's WebSocket "quotes" channel.
  // Live Flash price + all-in cost, from Flash's WebSocket "quotes" stream.
  const renderQuote = () => {
    const pxEl = $("[data-hero-px]", page), sub = $("[data-hero-px-sub]", page);
    if (!pxEl) return;
    const q = state.flash?.liveQuote;
    if (q && known(q.mark) && +q.mark > 0) {
      pxEl.textContent = price(q.mark);
      sub.textContent = `Flash · ${fixed(q.costPct, 2)}% cost`;
    } else if (state.price?.last) {
      pxEl.textContent = price(state.price.last);
      sub.textContent = `${state.price.symbol || "WETH"} price`;
    }
  };

  const renderMarket = () => {
    const el = $("[data-market]", page);
    const p = state.price;
    const hist = (p?.history || []).map(Number).filter(Number.isFinite);
    const first = hist[0], last = p?.last || hist.at(-1);
    const change = hist.length > 1 && first ? ((last - first) / first) * 100 : NaN;
    const src = PRICE_SOURCE[priceSource()]?.line || "Market price";
    const liq = known(p?.liquidityUsd) && +p.liquidityUsd > 0 ? esc(usd(p.liquidityUsd, 0)) : "";
    const updated = state.flash?.lastPriceAt ? `Updated ${esc(ago(state.flash.lastPriceAt))}` : p ? (p.fresh ? "Updating every few seconds" : "Price is stale") : "";
    const sig = JSON.stringify([last, hist.length, hist.at(-1), src, liq, updated]);
    if (el.dataset.sig === sig) return;
    const firstRender = !el.dataset.sig;
    el.dataset.sig = sig;
    if (!p || !last) {
      el.innerHTML = `<div class="section-head"><h2>WETH on Base</h2></div>${emptyState("Waiting for the first price", "The arena reads the WETH price every few seconds. It shows here once the first one arrives.")}`;
      return;
    }
    el.innerHTML = `<div class="market-copy" data-reveal>
        <p class="eyebrow">WETH on Base</p>
        <p class="market-price num">${esc(price(last))}</p>
        <p class="market-change num ${signClass(change)}">${known(change) ? esc(pct(change, 2)) + " over the last " + hist.length + " readings" : "Collecting readings"}</p>
        <dl class="spec spec-tight">
          <div><dt>Source</dt><dd>${esc(src)}</dd></div>
          ${liq ? `<div><dt>Liquidity</dt><dd title="${esc(p.liquiditySource || "")}">${liq}</dd></div>` : ""}
          <div><dt>Status</dt><dd class="${p.fresh ? "" : "warn"}">${updated}</dd></div>
        </dl>
      </div>
      <div class="market-chart">${lineChart(hist, { w: 1000, h: 220, pad: 10, cls: "mchart", label: `WETH price, last ${hist.length} readings` })}</div>`;
    if (!firstRender) arm(el, true);
  };

  if (!state.stats) await refreshStats();
  fill();

  const boardEl = $("[data-board]", page);
  const renderBoard = async () => {
    try {
      const lb = await loadBoard();
      const rows = lb.rows.slice(0, 3);
      if (!rows.length) {
        boardEl.innerHTML = `<div class="standings-head"><span class="eyebrow">Standings</span></div>${emptyState("No agents yet", "Agents appear once their wallets exist. Run arena keygen, then restart the server.")}`;
        return;
      }
      const details = await Promise.all(rows.map((r) => api("/v1/agents/" + encodeURIComponent(r.agentId)).catch(() => null)));
      boardEl.innerHTML = `<div class="standings-head"><span class="eyebrow">Top agents</span><a class="link-quiet small" href="#/leaderboard">All</a></div>
        <div data-stagger>${rows.map((r, i) => `<a class="srow" href="#/agent/${encodeURIComponent(r.agentId)}">
          <span class="rank num">${esc(r.rank)}</span>
          <span class="who"><strong>${esc(r.name)}</strong><span class="sub num">${esc(pct(r.returnPct))} · ${esc(plural(r.trades, "trade"))}${r.community ? ` · by ${esc(short(r.owner))}` : ""}</span></span>
          <span class="moveslot">${moveHTML(r.agentId, r.rank)}</span>
          <span class="score"><b class="num ${signClass(r.score)}">${esc(FMT.score(+r.score || 0))}</b></span>
          ${sparkline(details[i]?.equity)}
        </a>`).join("")}</div>`;
    } catch (err) {
      boardEl.innerHTML = errorState(err);
    }
  };
  await renderBoard();
  if (!store.get("arena.intro") && !sheet.open) setTimeout(() => { if ((location.hash || "#/") === "#/" && !sheet.open) openIntro(); }, 700);

  const autoEl = $("[data-auto-list]", page);
  const renderAuto = async () => {
    let plans = [];
    try { plans = (await api("/v1/plans")).plans || []; } catch { autoEl.innerHTML = ""; return; }
    const html = plans.slice(0, 3).map((p) => planCard(p, true)).join("");
    if (autoEl.dataset.sig !== html) { autoEl.dataset.sig = html; autoEl.innerHTML = html; }
  };
  renderAuto();

  const renderTicker = () => {
    const items = state.feed.filter((p) => p.kind !== "follow").slice(0, 16);
    const tk = $("[data-ticker]", page);
    if (!tk) return;
    tk.hidden = !items.length;
    const one = items.map((p) => {
      const raw = paperText(p.text).text;
      const text = raw.length > 110 ? raw.slice(0, 108) + "…" : raw;
      return `<span class="tk">${kindTag(p.kind)}<span>${esc(text)}</span><time class="num">${esc(ago(p.createdAt))}</time></span>`;
    }).join("");
    $("[data-ticker-track]", page).innerHTML = `<div class="tk-run">${one}</div><div class="tk-run" aria-hidden="true">${one}</div>`;
  };
  if (!state.feed.length) await loadFeedOnce();
  renderTicker();
  const t = setInterval(() => { renderBoard(); renderAuto(); }, 30000);
  const qt = setInterval(async () => {
    if (document.visibilityState !== "visible") return;
    try { state.flash = await api("/v1/flash/status"); updateChrome(); } catch { /* keep last */ }
    renderQuote();
  }, 10000);
  return { onPost: renderTicker, onStats: fill, cleanup: () => { clearInterval(t); clearInterval(qt); howStop?.(); } };
}

async function pageLeaderboard(page) {
  page.innerHTML = `<header class="page-head">
      <p class="eyebrow" data-reveal>${state.stats?.season ? "Season " + esc(state.stats.season) : "This season"}</p>
      <div class="trade-title" data-reveal><h1>Leaderboard</h1>${infoBtn("score")}</div>
      <p class="lede" data-reveal>Steady beats lucky.</p>
      <div class="btn-row" data-reveal><button type="button" class="btn btn-ghost btn-sm" data-share-lb>Share standings</button><a class="btn btn-ghost btn-sm" href="#/signals">Signals</a></div>
    </header>
    <div data-lb><div class="skel skel-row"></div><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const box = $("[data-lb]", page);
  $("[data-share-lb]", page).addEventListener("click", shareLeaderboard);
  const render = async (lb) => {
    if (!lb.rows.length) { box.innerHTML = emptyState("No agents in this season", "Agents appear here once their wallets are set up."); return; }
    const details = await Promise.all(lb.rows.map((r) => api("/v1/agents/" + encodeURIComponent(r.agentId)).catch(() => null)));
    const maxAbs = Math.max(0.01, ...lb.rows.map((r) => Math.abs(+r.score || 0)));
    box.innerHTML = `<table class="table"><thead><tr>
      <th class="c-rank">#</th><th class="c-move"><span class="sr">Move</span></th><th>Agent</th><th class="r">Score</th><th class="hide-sm c-bar"><span class="sr">Score bar</span></th><th class="r">Return</th><th class="r hide-sm">Max drop</th><th class="r hide-sm">Win rate</th><th class="r hide-sm">Trades</th><th class="r hide-sm">Copying</th><th class="hide-sm c-curve">Equity</th>
      </tr></thead><tbody data-stagger>${lb.rows.map((r, i) => `<tr data-href="#/agent/${encodeURIComponent(r.agentId)}" tabindex="0">
        <td class="c-rank num">${esc(r.rank)}</td>
        <td class="c-move">${moveHTML(r.agentId, r.rank)}</td>
        <td><a class="agent-link" href="#/agent/${encodeURIComponent(r.agentId)}">${esc(r.name)}</a>${r.community ? ` <span class="chip">by ${esc(short(r.owner))}</span>` : ""}${state.watch.has(r.agentId) ? ' <span class="chip chip-watch">Watching</span>' : ""}<div class="muted small num">${esc(usd(r.equityUsd))} equity · ${esc(details[i]?.agent?.strategy || r.strategy || "")}</div></td>
        <td class="r num strong ${signClass(r.score)}">${esc(FMT.score(+r.score || 0))}</td>
        <td class="hide-sm c-bar"><span class="sbar${+r.score < 0 ? " neg" : ""}" data-k="${Math.min(1, Math.abs(+r.score || 0) / maxAbs).toFixed(3)}"><i></i></span></td>
        <td class="r num ${signClass(r.returnPct)}">${esc(pct(r.returnPct))}</td>
        <td class="r num hide-sm">${esc(fixed(r.maxDrawdownPct, 1))}%</td>
        <td class="r num hide-sm">${r.closed ? esc(fixed(r.winRatePct, 0)) + '%<span class="muted"> of ' + esc(r.closed) + "</span>" : '<span class="muted">—</span>'}</td>
        <td class="r num hide-sm">${esc(r.trades)}</td>
        <td class="r num hide-sm">${esc(r.followers)}</td>
        <td class="hide-sm c-curve">${sparkline(details[i]?.equity)}</td></tr>`).join("")}</tbody></table>`;
    $$(".sbar", box).forEach((s) => s.style.setProperty("--k", s.dataset.k));
    $$("tr[data-href]", box).forEach((tr) => {
      const go = () => (location.hash = tr.dataset.href);
      tr.addEventListener("click", (e) => { if (!e.target.closest("a")) go(); });
      tr.addEventListener("keydown", (e) => { if (e.key === "Enter") go(); });
    });
  };
  try { await render(await loadBoard()); } catch (err) { box.innerHTML = errorState(err); }
  const t = setInterval(() => loadBoard().then(render).catch(() => {}), 30000);
  return { cleanup: () => clearInterval(t) };
}

function equityChart(points) {
  // Skip the empty start (before the first deposit): it is not a gain.
  const first = (points || []).findIndex((p) => +p.usd > 0);
  points = first > 0 ? points.slice(first) : points;
  if (!points || points.length < 2) {
    return `<div class="chart-empty"><p class="eyebrow">No curve yet</p><p class="small">The equity curve starts after the first few balance snapshots, about one a minute.</p></div>`;
  }
  const W = 640, H = 240, P = 8;
  const vals = points.map((p) => p.usd), min = Math.min(...vals), max = Math.max(...vals);
  // At least a 2% band: a $0.06 dip on $100 is a flat line, not a crash.
  const mid = (max + min) / 2, span = Math.max(max - min, Math.abs(mid) * 0.02, 0.01), floor = mid - span / 2;
  const xy = vals.map((v, i) => [P + (i / (vals.length - 1)) * (W - 2 * P), H - P - ((v - floor) / span) * (H - 2 * P)]);
  const d = xy.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)} ${y.toFixed(1)}`).join("");
  const tone = vals.at(-1) < vals[0] ? "is-down" : "is-up";
  const grid = [0.25, 0.5, 0.75].map((k) => `<line class="grid-line" x1="0" x2="${W}" y1="${(H * k).toFixed(0)}" y2="${(H * k).toFixed(0)}"/>`).join("");
  return `<div class="chart" data-chart><svg class="eqchart draw ${tone}" viewBox="0 0 ${W} ${H}" role="img" aria-label="Equity from ${esc(usd(vals[0]))} to ${esc(usd(vals.at(-1)))}" data-reveal>
    ${grid}<path class="area" d="${d}L${W - P} ${H}L${P} ${H}Z"/><path class="line" pathLength="1" d="${d}"/>
    <line class="cross" data-cross x1="0" x2="0" y1="0" y2="${H}" visibility="hidden"/></svg>
    <div class="chart-axis num"><span>${esc(usd(min))} low</span><span>${esc(usd(max))} high</span></div><div class="tip num" data-tip hidden></div></div>`;
}

// fieldsOK checks the visible fields of a form against their own limits
// (min, max, required) and points at the first wrong one, so a person sees
// every range before the server is asked. Hidden fields are skipped.
function fieldsOK(form) {
  for (const el of form.querySelectorAll("input, select, textarea")) {
    if (el.offsetParent !== null && !el.disabled && !el.checkValidity()) { el.reportValidity(); el.focus(); return false; }
  }
  return true;
}

// placeTip shows a chart tooltip centred over a point (0-1 across the
// chart) but never past either edge, so it stays on screen on a phone.
function placeTip(tip, box, frac) {
  tip.hidden = false;
  const bw = box.clientWidth, tw = tip.offsetWidth, m = 4;
  const x = tw + 2 * m >= bw ? bw / 2 : Math.min(bw - tw / 2 - m, Math.max(tw / 2 + m, frac * bw));
  tip.style.setProperty("left", x.toFixed(0) + "px");
}

function wireChart(scope, points) {
  const chart = $("[data-chart]", scope);
  if (!chart) return;
  const first = (points || []).findIndex((p) => +p.usd > 0);
  points = first > 0 ? points.slice(first) : points;
  const svg = $("svg", chart), cross = $("[data-cross]", chart), tip = $("[data-tip]", chart);
  const move = (e) => {
    const r = svg.getBoundingClientRect();
    const k = Math.min(1, Math.max(0, (e.clientX - r.left) / r.width));
    const i = Math.round(k * (points.length - 1));
    const x = 8 + (i / (points.length - 1)) * (640 - 16);
    cross.setAttribute("x1", x); cross.setAttribute("x2", x); cross.setAttribute("visibility", "visible");
    tip.textContent = `${usd(points[i].usd)} · ${new Date(points[i].at).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}`;
    placeTip(tip, chart, i / (points.length - 1));
  };
  svg.addEventListener("pointermove", move);
  svg.addEventListener("pointerleave", () => { cross.setAttribute("visibility", "hidden"); tip.hidden = true; });
}

// Small tags for paper-money and test trades.
const tradeTags = (t) => (t.paper ? '<span class="mini mini-paper">Paper</span>' : "") + (t.test ? '<span class="mini mini-test">Test</span>' : "");
const KIND_LABEL = { agent: "Agent trade", copy: "Copy", invest: "Invested wallet", test: "Test trade", manual: "Manual trade", plan: "Auto-trade plan", self: "Own wallet" };
// Flash order styles (all go through the same Flash /order endpoint).
const STYLE = { market: "Market", limit: "Limit", twap: "DCA (TWAP)", stop: "Stop buy" };
const styleLabel = (s) => STYLE[s || "market"] || cap(s);
const styleChip = (s) => `<span class="mini mini-style">${esc(styleLabel(s))}</span>`;
const protectLabel = (t) => (t.protectMode === "trailing" ? `Trailing stop ${fixed(t.trailPct, 1)}%` : t.protectMode === "separate" ? "Separate SL + TP orders" : t.orderType === "stop+protect" ? "Bracket after fill" : "Bracket attached");
const protectShort = (mode, trail) => (mode === "trailing" ? `Trailing ${fixed(trail, 1)}%` : mode === "separate" ? "SL + TP orders" : "Bracket after fill");
const tokenOf = (t) => t?.token || "WETH";
// Coin amounts: tiny BTC-sized numbers keep 8 decimals.
const amount = (v, sym) => { const n = num(v); if (!(n > 0)) return "—"; return (n < 0.01 ? n.toFixed(8) : n < 1 ? n.toFixed(6) : n.toFixed(4)) + (sym ? " " + sym : ""); };

// Round coin badge at the start of a row: the ETH diamond, ₿, or a letter.
function coinIco(sym) {
  const s = String(sym || "").toUpperCase();
  const inner = s.includes("ETH") ? '<svg viewBox="0 0 24 24"><path d="M12 2.5 5.6 12.4 12 16.1l6.4-3.7z"/><path d="M5.6 13.7 12 21.5l6.4-7.8L12 17.4z"/></svg>'
    : esc(s.includes("BTC") ? "₿" : s.slice(0, 1) || "?");
  return `<span class="t-ico" aria-hidden="true">${inner}</span>`;
}

function tradeRow(t) {
  const fill = t.fillPrice || t.quotedPrice || t.entry;
  const right = t.pnlUsd != null ? `<span class="num strong">${pnlHTML(t.pnlUsd)}</span>`
    : t.status === "filled" ? '<span class="muted small">Open</span>' : "";
  const pre = t.copyOf ? "Copy · " : t.kind === "plan" ? "Plan · " : t.kind === "self" ? "Own wallet · " : "";
  return `<li><a class="trade-item" href="#/trade/${encodeURIComponent(t.id)}">${coinIco(tokenOf(t))}
    <div class="t-main"><div class="t-title"><span class="t-name">${esc(pre)}Long ${esc(tokenOf(t))} <span class="num">${esc(usd(t.sizeUsd))}</span></span>${tradeTags(t)}${t.style && t.style !== "market" ? styleChip(t.style) : ""}</div>
      <div class="t-meta num">${t.kind === "invest" && t.walletId ? `<span class="t-who">${esc(t.walletId)}</span>` : ""}<span>${t.fillPrice ? "Fill" : "Entry"} ${esc(price(fill))}</span><span>TP ${esc(price(t.takeProfit))}</span><span>SL ${esc(price(t.stopLoss))}</span><span>${esc(ago(t.createdAt))}</span></div></div>
    <div class="t-right">${statusChip(t.status)}<div>${right}</div></div></a></li>`;
}

async function pageAgent(page, id) {
  page.innerHTML = `<a class="back" href="#/leaderboard">Leaderboard</a><div data-body><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const body = $("[data-body]", page);
  let data, lb;
  try {
    [data, lb] = await Promise.all([api("/v1/agents/" + encodeURIComponent(id)), state.board ? Promise.resolve(state.board) : loadBoard()]);
  } catch (err) {
    body.innerHTML = err.status === 404 ? emptyState("Agent not found", "This agent isn't part of the current season.", { href: "#/leaderboard", label: "See all agents" }) : errorState(err);
    return;
  }
  const a = data.agent, row = lb.rows.find((r) => r.agentId === id) || {};
  const watching = state.watch.has(id);
  const openSig = (data.trades || []).find((t) => t.kind === "agent" && t.walletId === id && (t.status === "placed" || t.status === "filled"));
  document.title = `${a.name} · Agent Arena`;
  body.innerHTML = `
    <header class="agent-head">
      <div class="avatar" aria-hidden="true" data-reveal>${esc(initials(a.name))}</div>
      <div data-reveal><p class="eyebrow">Rank ${esc(row.rank ?? "—")} · ${a.community ? "Community agent" : esc(a.strategy) + " strategy"}</p><h1>${esc(a.name)}</h1><p class="lede lede-sm">${esc(a.tagline)}</p>
        ${a.community ? agentRules(a) : ""}
        <div class="addr">Wallet ${addrLink(a.address)}<button type="button" class="btn-link" data-copy="${esc(a.address)}">Copy address</button></div></div>
      <div class="btn-row" data-reveal><button class="btn ${watching ? "btn-ghost" : "btn-primary"}" data-watch>${watching ? "Stop watching" : "Watch this agent"}</button><button class="btn btn-ghost" data-ashare>Share</button>${openSig ? '<button class="btn btn-ghost" data-acopy>Copy with my wallet</button>' : ""}</div>
    </header>
    <dl class="statline" data-stagger>
      <div class="stat"><dt>Arena Score</dt><dd class="num ${signClass(row.score)}" ${countAttr(row.score, "score")}>${countText(row.score, "score")}</dd></div>
      <div class="stat"><dt>Return</dt><dd class="num ${signClass(row.returnPct)}" ${countAttr(row.returnPct, "pct1")}>${countText(row.returnPct, "pct1")}</dd></div>
      <div class="stat"><dt>Biggest drop</dt><dd class="num" ${countAttr(row.maxDrawdownPct, "dd")}>${countText(row.maxDrawdownPct, "dd")}</dd></div>
      <div class="stat"><dt>Win rate</dt><dd class="num">${row.closed ? esc(fixed(row.winRatePct, 0)) + "%" : "—"}</dd></div>
      <div class="stat"><dt>Equity</dt><dd class="num" ${countAttr(a.equityUsd, "usd2")}>${countText(a.equityUsd, "usd2")}</dd></div>
      <div class="stat"><dt>Copying</dt><dd class="num" ${countAttr(a.followers, "int")}>${countText(a.followers, "int")}</dd></div>
    </dl>
    <div class="two-col">
      <div>
        <div class="section-head"><h2>Equity</h2></div>
        ${equityChart(data.equity)}
        <div class="section-head sticky-head section"><h2>Trades</h2><span class="muted small num">${esc((data.trades || []).length)} shown</span></div>
        ${(data.trades || []).length ? `<ul class="trades" data-stagger>${data.trades.map(tradeRow).join("")}</ul>` : emptyState("No trades yet", "It trades when its strategy fires.")}
      </div>
      <aside data-copy-panel></aside>
    </div>
    <section class="section"><div class="panel action-row" data-reveal><div><p class="eyebrow">USDC only · no gas</p><h3 class="panel-title">Invest with ${esc(a.name)}</h3></div><div class="btn-row">${infoBtn("invest")}<button type="button" class="btn btn-primary" data-invest-me>From my account</button>${isOperator() ? '<button type="button" class="btn btn-ghost" data-invest-open>New sub-wallet</button>' : ""}</div></div></section>`;

  wireChart(body, data.equity);
  $("[data-copy]", body).addEventListener("click", async (e) => {
    try { await navigator.clipboard.writeText(e.currentTarget.dataset.copy); toast("Address copied", "ok"); } catch { toast("Couldn't copy. Long-press to copy instead.", "err"); }
  });
  $("[data-watch]", body).addEventListener("click", async () => {
    if (state.watch.has(id)) state.watch.delete(id); else {
      state.watch.add(id);
      if ("Notification" in window && Notification.permission === "default") { try { await Notification.requestPermission(); } catch { /* ignore */ } }
    }
    store.set("arena.watch", [...state.watch]);
    toast(state.watch.has(id) ? `Watching ${a.name}. You'll get an alert on every trade.` : `Stopped watching ${a.name}.`, "ok");
    route();
  });
  $("[data-ashare]", body).addEventListener("click", () => openShare({
    url: shareURL("agent", id), text: `${a.name} on Agent Arena · Arena Score ${FMT.score(+row.score || 0)} · AI agent trading on Flash`, file: `arena-${id}.png`,
    card: () => drawCard({ title: a.name, big: FMT.score(+row.score || 0), down: +row.score < 0, line: `Return ${pct(row.returnPct)} · Biggest drop ${fixed(row.maxDrawdownPct, 1)}% · Rank ${row.rank ?? "—"}` }),
  }));
  $("[data-acopy]", body)?.addEventListener("click", () => walletTrade({ copyOf: openSig.id, token: tokenOf(openSig), from: a.name }));
  renderCopyPanel($("[data-copy-panel]", body), a);
  $("[data-invest-open]", body)?.addEventListener("click", () => renderInvestPanel(openSheet(`Invest with ${a.name}`, { wide: true }), a));
  $("[data-invest-me]", body).addEventListener("click", (e) => investFromAccount(e.currentTarget, a));
}

// The rules of an agent a person made, so anyone can judge it before copying.
function agentRules(a) {
  const r = a.rules || {};
  const when = agentWhen(r);
  return `<p class="chips"><span class="mini">${esc(AGENT_KIND[r.kind]?.label || r.kind || a.strategy)}</span><span class="mini">${esc(r.token || "WETH")}</span><span class="mini">TP +${esc(fixed(r.takeProfitPct, 1))}%</span><span class="mini">SL −${esc(fixed(r.stopLossPct, 1))}%</span><span class="mini">${esc(fixed(r.sizePct, 0))}% a trade</span>${a.paused ? '<span class="mini mini-test">Paused</span>' : ""}</p>
    <p class="muted small">Made by ${addrLink(a.owner)}${when ? ` · buys when ${esc(when)}` : ""}</p>`;
}

// Invest with this agent from the signed-in Arena account (no admin token).
async function investFromAccount(btn, a, mode = "invest") {
  if (!wallet.address || !meToken()) { location.hash = "#/me"; toast(`Connect your wallet and sign in, then pick ${a.name}.`, "ok"); return; }
  const done = busy(btn, "Loading…");
  let d;
  try { d = await api("/v1/me", { me: true }); } catch (err) { toast(err.message, "err"); done(); return; }
  done();
  if (!d.account) { location.hash = "#/me"; toast("Create your Arena account first (one tap), then add USDC.", "ok"); return; }
  if (d.agent) { toast(d.agent.id === a.id ? "This is your own agent. It already trades your account." : "Your account runs your own agent, so it can't also invest with another one.", "err"); return; }
  if (d.plan) { toast("Your account runs a plan. Stop it first to invest with an agent.", "err"); return; }
  if (!state.board) await loadBoard().catch(() => null);
  investSheet(d, () => toast(`Open My wallet to see trades from ${a.name}.`, "ok"), a.id, mode);
}

async function copyText(text) {
  try { await navigator.clipboard.writeText(text); toast("Address copied", "ok"); } catch { toast("Couldn't copy. Long-press to copy instead.", "err"); }
}

// "Invest with" creates a sub-wallet for one person, linked to this agent.
function renderInvestPanel(el, agent) {
  const max = +(state.stats?.limits?.maxTradeUsd ?? 5);
  const mode = modeOf();
  const intro = `<p class="chips">${mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode].badge)}</span>` : ""}<span class="mini">Your own wallet</span><span class="mini">Withdraw to you only</span><span class="mini">Stop loss always</span>${infoBtn("invest")}</p>`;
  if (!isOperator()) {
    el.innerHTML = `<div class="invest">${intro}<div class="btn-row sheet-cta"><button type="button" class="btn btn-primary btn-block" data-signin-open>Sign in to continue</button></div></div>`;
    return;
  }
  el.innerHTML = `<div class="invest">${intro}
    <form class="invest-side form-grid form-2" data-invest novalidate>
      <div class="field"><label for="io">Your name</label><input id="io" name="owner" maxlength="40" autocomplete="name" required><span class="hint">Shown on the wallet</span><span class="field-err" data-err="owner"></span></div>
      <div class="field"><label for="im">Mode</label><select id="im" name="mode"><option value="invest">Invest: trades every signal</option><option value="copy">Copy: mirrors its trades</option></select></div>
      <div class="field span-2"><label for="ip">Payout address</label><input id="ip" name="payout" class="mono" placeholder="0x…" spellcheck="false" autocomplete="off" maxlength="42" required><span class="hint">Locked. Withdrawals only ever go here.</span><span class="field-err" data-err="payout"></span></div>
      <div class="field"><label for="ia">Share per trade (%)</label><input id="ia" name="allocationPct" type="number" min="1" max="25" step="1" value="20" inputmode="numeric" required><span class="hint">Part of your USDC used each trade</span></div>
      <div class="field"><label for="ix">Max per trade ($)</label><input id="ix" name="maxPerTradeUsd" type="number" min="1" max="${esc(max)}" step="0.5" value="${esc(max)}" inputmode="decimal" required><span class="hint">Never more than this in one trade</span></div>
      <div class="field"><label for="idc">Daily limit ($)</label><input id="idc" name="dailyCapUsd" type="number" min="1" step="1" value="${esc(max * 3)}" inputmode="numeric" required><span class="hint">Total it can spend in a day</span></div>
      <div class="field"><label for="il">Loss limit (%)</label><input id="il" name="lossLimitPct" type="number" min="0" max="90" step="1" value="15" inputmode="numeric" required><span class="hint">Pauses itself this far below its peak</span></div>
      <p class="ticket-preview span-2" data-inv-sum aria-live="polite"></p>
      <div class="btn-row span-2 sheet-cta"><button class="btn btn-primary btn-block" type="submit">Create my sub-wallet</button></div>
    </form></div>`;
  const form = $("[data-invest]", el);
  liveNumbers(form);
  const sum = () => {
    const f = Object.fromEntries(new FormData(form));
    $("[data-inv-sum]", form).textContent = `${f.mode === "copy" ? "Copies" : "Trades"} ${agent.name}'s signals with ${f.allocationPct || "?"}% of your USDC, up to ${usd(f.maxPerTradeUsd)} a trade and ${usd(f.dailyCapUsd)} a day. Pauses at −${f.lossLimitPct || 0}%.`;
  };
  form.addEventListener("input", sum); form.addEventListener("change", sum); sum();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = Object.fromEntries(new FormData(form));
    $("[data-err=owner]", form).textContent = f.owner.trim() ? "" : "Add your name";
    $("[data-err=payout]", form).textContent = isAddr(f.payout.trim()) ? "" : "A 0x address with 40 hex characters";
    const bad = checkNumbers(form);
    if (!f.owner.trim() || !isAddr(f.payout.trim()) || bad) { (bad || $(!f.owner.trim() ? "#io" : "#ip", form)).focus(); return; }
    if (+f.dailyCapUsd < +f.maxPerTradeUsd) { toast("The daily limit must be at least the max per trade.", "err"); $("#idc", form).focus(); return; }
    const ok = await confirmDialog("Lock this payout address?", `Withdrawals from the new wallet can only go to ${f.payout.trim()}. This can't be changed from the app later.`, "Create wallet", { money: true });
    if (!ok) return;
    const btn = $("button[type=submit]", form);
    const done = busy(btn, "Creating…");
    try {
      const r = await api("/v1/admin/wallets", { method: "POST", admin: true, body: {
        owner: f.owner.trim(), payout: f.payout.trim(), agentId: agent.id, mode: f.mode,
        allocationPct: +f.allocationPct, maxPerTradeUsd: +f.maxPerTradeUsd, dailyCapUsd: +f.dailyCapUsd, lossLimitPct: +f.lossLimitPct,
      } });
      const w = r.wallet || {};
      form.outerHTML = `<div class="invest-side fund-box">
        <p class="eyebrow">Wallet ready · ${esc(w.id || "")}</p>
        <p class="fund-addr mono">${esc(w.address || "")}</p>
        <div class="btn-row"><button type="button" class="btn btn-primary btn-sm" data-copy-fund>Copy address</button>${isAddr(w.address) ? `<a class="btn btn-ghost btn-sm" href="https://basescan.org/address/${esc(w.address)}" ${EXT}>View on Basescan</a>` : ""}<a class="btn btn-ghost btn-sm" href="#/admin/wallets">Open wallets</a></div>
        <p class="small">${esc(r.fund || "")}</p>
        <p class="alert">Send USDC on the Base network only. Tokens sent on another network are lost.</p>
      </div>`;
      $("[data-copy-fund]", el)?.addEventListener("click", () => copyText(w.address || ""));
      toast(`Sub-wallet ${w.id || ""} created`, "ok");
    } catch (err) { toast(err.message, "err"); done(); }
  });
}

async function renderCopyPanel(el, agent) {
  const threshold = usd(state.stats?.limits?.approvalThresholdUsd ?? 10, 0);
  if (!isOperator()) {
    el.innerHTML = `<div class="panel" data-reveal><p class="eyebrow">Copy trading</p><h3 class="panel-title">Copy ${esc(agent.name)}</h3>
      <p class="chips"><span class="mini">Same TP / SL</span><span class="mini">Your limits</span><span class="mini">Over ${esc(threshold)} needs a yes</span></p>
      <p class="small muted">Your Arena account copies each trade it places, with your own limits. Withdrawals only go back to your wallet.</p>
      <div class="btn-row"><button type="button" class="btn btn-primary btn-sm" data-copy-me>Copy from my account</button></div></div>`;
    $("[data-copy-me]", el).addEventListener("click", (e) => investFromAccount(e.currentTarget, agent, "copy"));
    return;
  }
  el.innerHTML = `<div class="panel"><p class="eyebrow">Copy trading</p><h3 class="panel-title">Copy ${esc(agent.name)}</h3><div class="skel skel-row"></div></div>`;
  let wallets = [], follows = [];
  try {
    [wallets, follows] = await Promise.all([
      api("/v1/admin/wallets", { admin: true }).then((d) => (d.wallets || []).filter((w) => w.role === "follower")),
      api("/v1/admin/follows", { admin: true }).then((d) => (d.follows || []).filter((f) => f.agentId === agent.id)),
    ]);
  } catch (err) { el.innerHTML = `<div class="panel">${errorState(err)}</div>`; return; }
  const max = state.stats?.limits?.maxTradeUsd ?? 5;
  el.innerHTML = `<div class="panel"><p class="eyebrow">Copy trading</p><h3 class="panel-title">Copy ${esc(agent.name)}</h3>
    ${follows.length ? `<ul class="trades">${follows.map((f) => `<li class="trade-item"><div><div class="t-title">${esc(f.followerId)}</div><div class="t-meta num"><span>${f.allocationPct ? esc(f.allocationPct) + "%" : "Agent's size"} per trade</span><span>max ${esc(usd(f.maxPerTradeUsd))}</span><span>${esc(usd(f.dailyCapUsd))} a day</span></div></div><div class="t-right"><button class="btn btn-sm btn-danger" data-stop="${esc(f.followerId)}">Stop</button></div></li>`).join("")}</ul>` : ""}
    ${wallets.length ? `<form class="form-grid form-2" data-follow novalidate>
      <div class="field span-2"><label for="fw">Follower wallet</label><select id="fw" name="followerId">${wallets.map((w) => `<option value="${esc(w.id)}">${esc(w.profile?.owner ? w.profile.owner + " · " : "")}${esc(w.id)} · ${esc(usd(w.usdc))}</option>`).join("")}</select></div>
      <div class="field"><label for="fa">Share per trade (%)</label><input id="fa" name="allocationPct" type="number" min="1" max="25" step="1" value="20" inputmode="numeric" required><span class="hint">Part of the wallet's USDC</span></div>
      <div class="field"><label for="fm">Max per trade ($)</label><input id="fm" name="maxPerTradeUsd" type="number" min="1" max="${esc(max)}" step="0.5" value="${esc(max)}" inputmode="decimal" required><span class="hint">Up to ${esc(usd(max, 0))}</span></div>
      <div class="field span-2"><label for="fd">Daily limit ($)</label><input id="fd" name="dailyCapUsd" type="number" min="1" step="1" value="${esc(max * 3)}" inputmode="numeric" required><span class="hint">Total copies a day</span></div>
      <p class="ticket-preview span-2" data-follow-sum aria-live="polite"></p>
      <div class="btn-row span-2"><button class="btn btn-primary btn-block" type="submit">Start copying</button></div>
    </form>` : emptyState("No follower wallets yet", "", { href: "#/plans/new", label: "Create a plan instead" })}
  </div>`;
  const ff = $("[data-follow]", el);
  if (ff) {
    liveNumbers(ff);
    const sum = () => { const f = Object.fromEntries(new FormData(ff)); $("[data-follow-sum]", ff).textContent = `${f.followerId} copies ${agent.name}: ${f.allocationPct || "?"}% a trade, max ${usd(f.maxPerTradeUsd)}, ${usd(f.dailyCapUsd)} a day.`; };
    ff.addEventListener("input", sum); ff.addEventListener("change", sum); sum();
  }
  ff?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const form = e.currentTarget;
    const bad = checkNumbers(form);
    if (bad) { bad.focus(); return; }
    const f = Object.fromEntries(new FormData(form));
    if (+f.dailyCapUsd < +f.maxPerTradeUsd) { toast("The daily limit must be at least the max per trade.", "err"); return; }
    const done = busy($("button[type=submit]", form), "Starting…");
    try {
      await api("/v1/admin/follows", { method: "POST", admin: true, body: { followerId: f.followerId, agentId: agent.id, allocationPct: +f.allocationPct, maxPerTradeUsd: +f.maxPerTradeUsd, dailyCapUsd: +f.dailyCapUsd } });
      toast(`${f.followerId} is now copying ${agent.name}`, "ok");
      renderCopyPanel(el, agent);
    } catch (err) { toast(err.message, "err"); done(); }
  });
  $$("[data-stop]", el).forEach((b) => b.addEventListener("click", async () => {
    if (!(await confirmDialog("Stop copying?", `${b.dataset.stop} will stop copying ${agent.name}. Open trades keep their stop loss and take profit.`, "Stop copying"))) return;
    try {
      await api(`/v1/admin/follows/${encodeURIComponent(b.dataset.stop)}/${encodeURIComponent(agent.id)}`, { method: "DELETE", admin: true });
      toast("Stopped copying", "ok");
      renderCopyPanel(el, agent);
    } catch (err) { toast(err.message, "err"); }
  }));
}

const OPEN = new Set(["placed", "filled", "unprotected"]);

async function pageTrade(page, id) {
  page.innerHTML = `<a class="back" href="#/feed">Live feed</a><div data-body><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const body = $("[data-body]", page);
  let d;
  try { d = await api("/v1/trades/" + encodeURIComponent(id)); } catch (err) {
    body.innerHTML = err.status === 404 ? emptyState("Trade not found", "It may belong to an earlier season.", { href: "#/feed", label: "Open the live feed" }) : errorState(err);
    return;
  }
  if (!state.board) await loadBoard().catch(() => {});
  // Plan trades have no agent: name them after their plan.
  if (d.trade.planId) d.plan = await api("/v1/plans/" + encodeURIComponent(d.trade.planId)).then((x) => x.plan).catch(() => null);
  const plan = d.plan;
  const back = $(".back", page);
  if (d.trade.planId && back) { back.href = "#/plan/" + enc(d.trade.planId); back.textContent = plan?.name || "Plan"; }
  else if (d.trade.agentId && back) { back.href = "#/agent/" + enc(d.trade.agentId); back.textContent = agentName(d.trade.agentId); }
  let lastSig = "";
  const draw = (data) => {
    if (plan && !data.plan) data.plan = plan;
    const sig = JSON.stringify(data);
    if (sig === lastSig) return;
    lastSig = sig;
    renderTrade(body, data);
    wireTradeChart(body);
  };
  draw(d);
  const chart = async () => {
    const el = $("[data-tchart]", body);
    if (!el) return;
    try { const c = await api(`/v1/trades/${enc(id)}/chart`); tchartCache.set(id, tradeChartHTML(c)); el.innerHTML = tchartCache.get(id); wireTradeChart(body, c); }
    catch { if (!tchartCache.has(id)) el.innerHTML = ""; }
  };
  chart();
  const refresh = async () => {
    if (!OPEN.has(d.trade.status)) return; // closed and dry-run trades don't change
    try { d = await api("/v1/trades/" + encodeURIComponent(id)); draw(d); } catch { /* keep last */ }
    chart();
  };
  const t = setInterval(refresh, 15000);
  return { onPost: (p) => { if (p.tradeId === id) refresh(); }, cleanup: () => clearInterval(t) };
}

// Live trade chart: the price since just before the trade, its take-profit,
// stop-loss, entry and break-even lines, what happened when, and what it is
// worth right now. Cached per trade so a re-render never flashes empty.
const tchartCache = new Map(), tchartData = new Map();
function tradeChartHTML(c) {
  const pts = (c.points || []).filter((p) => p.p > 0);
  if (pts.length < 2) return `<div class="chart-empty"><p class="eyebrow">No price record yet</p><p class="small">The chart fills in as prices are recorded, one every 15 seconds.</p></div>`;
  const W = 640, H = 260, PL = 6, PR = 6, PT = 12, PB = 10;
  const t0 = pts[0].t, t1 = Math.max(pts.at(-1).t, c.createdAt || 0);
  const lines = [["tp", c.takeProfit, "Target"], ["sl", c.stopLoss, "Stop"], ["en", c.entry, c.status === "placed" ? "Entry" : "Bought"], ["be", c.breakEven, "Break-even"], ["ex", c.exit, "Sold"]].filter((l) => +l[1] > 0);
  const vals = pts.map((p) => p.p).concat(lines.map((l) => +l[1]));
  let lo = Math.min(...vals), hi = Math.max(...vals);
  const pad = (hi - lo) * 0.08 || hi * 0.002;
  lo -= pad; hi += pad;
  const X = (t) => PL + ((t - t0) / (t1 - t0 || 1)) * (W - PL - PR), Y = (v) => PT + (1 - (v - lo) / (hi - lo || 1)) * (H - PT - PB);
  const d = pts.map((p, i) => `${i ? "L" : "M"}${X(p.t).toFixed(1)} ${Y(p.p).toFixed(1)}`).join("");
  const last = pts.at(-1), up = c.entry > 0 ? last.p >= c.entry : last.p >= pts[0].p;
  const at = (t) => { let best = pts[0]; for (const p of pts) { if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p; } return best.p; };
  // Labels sit just above their line; close lines would print one label on
  // top of another, so labels are pushed apart (the lines stay exact).
  const gap = matchMedia("(max-width: 560px)").matches ? 28 : 15; // a label's line box (font 19 / 10) plus air
  const labs = lines.map(([k, v, label]) => ({ k, v, label, y: Y(v) - 4 })).sort((a, b) => a.y - b.y);
  for (let j = 1; j < labs.length; j++) labs[j].y = Math.max(labs[j].y, labs[j - 1].y + gap);
  const spill = labs.length ? labs.at(-1).y - (H - 2) : 0;
  if (spill > 0) labs.forEach((l) => { l.y -= spill; });
  const rise = labs.length ? gap - 4 - labs[0].y : 0;
  if (rise > 0) labs.forEach((l) => { l.y += rise; });
  const guide = lines.map(([k, v]) => `<line class="tl tl-${k}" x1="${PL}" x2="${W - PR}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/>`).join("")
    + labs.map((l) => `<text class="tt tt-${l.k}" x="${W - PR - 4}" y="${l.y.toFixed(1)}" text-anchor="end">${esc(l.label)} ${esc(price(l.v))}</text>`).join("");
  const start = c.createdAt >= t0 ? `<line class="tl tl-start" x1="${X(c.createdAt).toFixed(1)}" x2="${X(c.createdAt).toFixed(1)}" y1="${PT}" y2="${H - PB}"/>` : "";
  const evs = (c.events || []).filter((e) => e.t >= t0 && e.t <= t1).map((e) => `<circle class="tev tev-${esc(e.kind)}" cx="${X(e.t).toFixed(1)}" cy="${Y(at(e.t)).toFixed(1)}" r="5"><title>${esc(e.label)} · ${esc(clock(new Date(e.t)))}</title></circle>`).join("");
  const now = `<circle class="tnow ${c.open ? "is-live" : ""}" cx="${X(last.t).toFixed(1)}" cy="${Y(last.p).toFixed(1)}" r="5"/>`;
  const span = (t1 - t0) / 60000, ago = span >= 120 ? `${fixed(span / 60, 1)} h` : `${Math.round(span)} min`;
  // Where the price sits between the stop (0%) and the target (100%).
  let meter = "";
  if (c.open && c.stopLoss > 0 && c.takeProfit > c.stopLoss && c.now > 0) {
    const k = (v) => Math.min(100, Math.max(0, ((v - c.stopLoss) / (c.takeProfit - c.stopLoss)) * 100));
    meter = `<div class="tmeter" role="img" aria-label="Price is ${esc(fixed(k(c.now), 0))}% of the way from the stop to the target">
      <div class="tmeter-bar"><i class="tmeter-en" data-x="${k(c.entry).toFixed(1)}"></i><i class="tmeter-now ${c.now >= c.entry ? "up" : "down"}" data-x="${k(c.now).toFixed(1)}"></i></div>
      <div class="tmeter-axis num"><span class="down">Stop ${esc(price(c.stopLoss))}${known(c.toStopPct) ? ` · ${esc(fixed(c.toStopPct, 2))}% away` : ""}</span><span class="up">Target ${esc(price(c.takeProfit))}${known(c.toTargetPct) ? ` · ${esc(fixed(c.toTargetPct, 2))}% away` : ""}</span></div></div>`;
  }
  const live = c.open && known(c.pnlUsd)
    ? `<div class="tlive"><div><span class="muted small">Now</span><b class="num">${esc(price(c.now))}</b></div><div><span class="muted small">If sold now (before Flash's sell cost)</span><b class="num">${pnlHTML(c.pnlUsd)} <small class="${c.pnlPct >= 0 ? "up" : "down"}">${esc(pct(c.pnlPct, 2))}</small></b></div><div><span class="muted small">Worth</span><b class="num">${esc(usd(c.valueUsd, 2))}</b></div></div>` : "";
  return `${live}${meter}<div class="chart tchart-box" data-chart-t>
    <svg class="tsvg ${up ? "is-up" : "is-down"}" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(c.token)} price over the last ${esc(ago)}: ${esc(price(pts[0].p))} to ${esc(price(last.p))}">
      ${[0.25, 0.5, 0.75].map((q) => `<line class="grid-line" x1="0" x2="${W}" y1="${(H * q).toFixed(0)}" y2="${(H * q).toFixed(0)}"/>`).join("")}
      ${start}<path class="area" d="${d}L${X(last.t).toFixed(1)} ${H}L${X(pts[0].t).toFixed(1)} ${H}Z"/><path class="line" d="${d}"/>${guide}${evs}${now}
      <line class="cross" data-cross x1="0" x2="0" y1="0" y2="${H}" visibility="hidden"/></svg>
    <div class="chart-axis num"><span>${esc(ago)} ago</span><span>${c.open ? "now" : esc(clock(new Date(t1)))}</span></div><div class="tip num" data-tip hidden></div></div>
    <p class="legend small muted tlegend"><span><i class="lg lg-tp"></i>Target</span><span><i class="lg lg-sl"></i>Stop</span><span><i class="lg lg-en"></i>Bought</span>${c.breakEven > 0 ? '<span><i class="lg lg-be"></i>Break-even</span>' : ""}<span><i class="lg lg-ev"></i>Events</span></p>`;
}
function wireTradeChart(scope, c) {
  const box = $("[data-chart-t]", scope);
  if (!box) return;
  const id = location.hash.split("/")[2] || "";
  if (c) tchartData.set(id, c); else c = tchartData.get(id);
  $$(".tmeter i[data-x]", scope).forEach((el) => el.style.setProperty("left", el.dataset.x + "%"));
  const pts = (c?.points || []).filter((p) => p.p > 0);
  if (pts.length < 2) return;
  const svg = $("svg", box), cross = $("[data-cross]", box), tip = $("[data-tip]", box);
  const t0 = pts[0].t, t1 = Math.max(pts.at(-1).t, c.createdAt || 0);
  svg.addEventListener("pointermove", (e) => {
    const r = svg.getBoundingClientRect(), k = Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)), t = t0 + k * (t1 - t0);
    let best = pts[0];
    for (const p of pts) { if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p; }
    const x = 6 + ((best.t - t0) / (t1 - t0 || 1)) * (640 - 12);
    cross.setAttribute("x1", x); cross.setAttribute("x2", x); cross.setAttribute("visibility", "visible");
    const diff = c.entry > 0 ? ` · ${pct((best.p / c.entry - 1) * 100, 2)} vs buy` : "";
    tip.textContent = `${price(best.p)} · ${clock(new Date(best.t))}${diff}`;
    placeTip(tip, box, x / 640);
  });
  svg.addEventListener("pointerleave", () => { cross.setAttribute("visibility", "hidden"); tip.hidden = true; });
}

function renderTrade(body, d) {
  const t = d.trade, plan = d.plan;
  // Test and manual orders carry an agent id only for bookkeeping: name them by kind.
  const ownKind = { test: "Test trade", manual: "Manual order", self: "Own wallet" }[t.kind];
  const name = plan?.name || ownKind || state.board?.rows.find((r) => r.agentId === t.agentId)?.name || t.agentId || (t.planId ? "Auto-trade plan" : "Trade");
  const who = plan ? `<a class="link-quiet" href="#/plan/${enc(plan.id)}">${esc(plan.owner ? plan.owner + "'s plan · " : "Plan · ")}${esc(plan.name)}</a>`
    : t.planId ? `<a class="link-quiet" href="#/plan/${enc(t.planId)}">Auto-trade plan</a>`
    : ownKind ? esc(ownKind)
    : t.agentId ? `<a class="link-quiet" href="#/agent/${enc(t.agentId)}">${esc(name)}</a>` : esc(name);
  const sym = tokenOf(t);
  const entry = num(t.fillPrice) > 0 ? +t.fillPrice : num(t.quotedPrice) > 0 ? +t.quotedPrice : +t.entry;
  const be = num(t.breakEvenPrice) > 0 ? +t.breakEvenPrice : NaN;
  const marks = [t.stopLoss, entry, t.takeProfit, be].map(Number).filter((v) => v > 0);
  const lo = Math.min(...marks) * 0.998, hi = Math.max(...marks) * 1.002, pos = (v) => ((v - lo) / (hi - lo || 1)) * 100;
  const steps = t.timeline || [];
  const expected = ["quoted", "approved", "signed", "submitted", "filled", "closed"];
  const done = new Set(steps.map((s) => s.step));
  const ended = !OPEN.has(t.status);
  const waiting = t.status === "dryrun" || ended ? [] : expected.filter((s) => !done.has(s) && s !== "approved");
  const pnl = t.pnlUsd != null ? usd(t.pnlUsd) : t.status === "filled" ? "Open" : "—";
  const pnlPct = t.pnlUsd != null && t.sizeUsd ? ` (${pct((t.pnlUsd / t.sizeUsd) * 100, 2)})` : "";
  const costPct = known(t.entryCostPct) ? ` · ${fixed(t.entryCostPct, 2)}%` : "";
  // "Fully filled" is the exit order's own status, not why the trade closed.
  const closeText = (t.closeCode && t.closeCode !== "REASON_FULLY_FILLED" ? reasonText(t.closeCode) : "") || t.closeReason || reasonText(t.closeCode) || "";
  const self = t.kind === "self";
  const op = isOperator() && !t.dryRun && !self;
  const canCopy = t.kind === "agent" && (t.status === "placed" || t.status === "filled");
  document.title = `${/(trade|order|wallet|plan)$/i.test(name) ? name : name + " trade"} · Agent Arena`;
  body.innerHTML = `
    <header class="trade-head">
      <p class="eyebrow" data-reveal>${who} · ${esc(new Date(t.createdAt).toLocaleString())}${t.copyOf ? ` · copy by ${esc(t.walletId)}` : ""}</p>
      <div class="trade-title" data-reveal><h1>${t.copyOf ? "Copied long " : "Long "}${esc(sym)}</h1>${statusChip(t.status)}</div>
      <p class="trade-tags" data-reveal>${t.paper ? '<span class="mini mini-paper">Paper money</span>' : ""}${t.test ? '<span class="mini mini-test">Test</span>' : ""}${styleChip(t.style)}<span class="mini">${esc(protectLabel(t))}</span>${t.kind ? `<span class="mini">${esc(KIND_LABEL[t.kind] || cap(t.kind))}</span>` : ""}${self && OPEN.has(t.status) ? '<span class="mini">Managed by your wallet</span>' : ""}${t.walletId && t.walletId !== t.agentId && !self ? `<span class="mini">Wallet ${esc(t.walletId)}</span>` : ""}</p>
      <blockquote class="reason" data-reveal>${esc(t.reason || "No reason given.")}</blockquote>
      ${t.status === "unprotected" ? `<p class="alert" data-reveal><strong>No exit order.</strong> The bracket ended but the wallet still holds ${esc(sym)}. ${op ? "Use Exit now to sell at market." : "Only the operator can close it from here."}</p>` : ""}
      ${op && (t.status === "placed" || t.status === "filled" || t.status === "unprotected") ? `<div class="btn-row op-actions" data-reveal>
        ${t.status === "placed" ? '<button class="btn btn-danger" data-cancel>Cancel order</button>' : ""}
        ${t.status === "filled" || t.status === "unprotected" ? '<button class="btn btn-danger" data-exit>Exit now</button>' : ""}</div>` : ""}
    </header>
    <dl class="statline" data-stagger>
      <div class="stat"><dt>Size</dt><dd class="num">${esc(usd(t.sizeUsd))}</dd></div>
      <div class="stat"><dt>Result</dt><dd class="num">${t.pnlUsd != null ? pnlHTML(t.pnlUsd) + (pnlPct ? `<small>${esc(pnlPct.replace(/[()]/g, "").trim())}</small>` : "") : esc(pnl === "Open" ? "Open" : "—")}</dd></div>
      <div class="stat"><dt>${t.fillPrice ? "Fill" : "Entry"}</dt><dd class="num">${esc(price(entry))}</dd></div>
      <div class="stat"><dt>Cost</dt><dd class="num">${known(t.entryCostPct) ? esc(fixed(t.entryCostPct, 2)) + "%" : known(t.entryCostUsd) ? esc(usd(t.entryCostUsd, 3)) : "—"}</dd></div>
    </dl>

    <section class="tchart" data-tchart aria-label="Price chart of this trade">${tchartCache.get(t.id) || '<div class="skel skel-row"></div>'}</section>

    <div class="ladder" data-reveal aria-label="Stop loss ${esc(price(t.stopLoss))}, entry ${esc(price(entry))}${known(be) ? ", break-even " + esc(price(be)) : ""}, take profit ${esc(price(t.takeProfit))}">
      <div class="ladder-bar"></div>
      ${known(be) && be > entry ? `<div class="ladder-cost" data-l="${pos(entry)}" data-w="${pos(be) - pos(entry)}" title="Costs: price must rise to ${esc(price(be))} to break even"></div>` : ""}
      <div class="ladder-mark sl" data-pos="${pos(t.stopLoss)}"><b class="num">${esc(price(t.stopLoss))}</b>Stop</div>
      <div class="ladder-mark entry" data-pos="${pos(entry)}"><b class="num">${esc(price(entry))}</b>${t.fillPrice ? "Fill" : "Entry"}</div>
      <div class="ladder-mark tp" data-pos="${pos(t.takeProfit)}"><b class="num">${esc(price(t.takeProfit))}</b>Target</div>
    </div>
    ${known(be) && be > entry ? `<p class="legend small muted" data-reveal><span class="swatch"></span><span>Costs to cover · break-even <b class="num">${esc(price(be))}</b></span></p>` : ""}

    <div class="two-col trade-cols">
      <section>
        <div class="section-head"><h2>Receipt</h2></div>
        <p class="glabel">Exit plan</p>
        <dl class="receipt">
          <div><dt>Take profit</dt><dd class="up">▲ ${esc(price(t.takeProfit))}</dd></div>
          <div><dt>Stop loss</dt><dd class="down">▼ ${esc(price(t.stopLoss))}</dd></div>
          <div><dt>Break-even</dt><dd>${esc(price(t.breakEvenPrice))}</dd></div>
          <div><dt>Protection</dt><dd>${esc(ended && t.status === "closed" ? "Done: the position is closed" : PROTECTION[t.protection] || (t.dryRun ? "Not sent (dry run)" : "—"))}</dd></div>
          <div><dt>Order style</dt><dd>${esc(styleLabel(t.style))}${+t.twapMinutes > 0 ? ` · ${esc(t.twapMinutes)} min` : ""}</dd></div>
          ${+t.stopPrice > 0 ? `<div><dt>Stop trigger</dt><dd>${esc(price(t.stopPrice))}</dd></div>` : ""}
          ${closeText ? `<div><dt>Closed because</dt><dd>${esc(closeText)}</dd></div>` : ""}
        </dl>
        <p class="glabel">Prices</p>
        <dl class="receipt">
          <div><dt>At decision</dt><dd>${esc(price(t.entry))}</dd></div>
          <div><dt>Flash quote</dt><dd>${esc(price(t.markPrice || t.quotedPrice))}</dd></div>
          <div><dt>Fill</dt><dd>${esc(price(t.fillPrice))}</dd></div>
          <div><dt>Exit</dt><dd>${esc(price(t.exitPrice))}</dd></div>
          <div><dt>${esc(sym)} bought</dt><dd>${esc(amount(t.wethAmount))}</dd></div>
          <div><dt>Entry cost</dt><dd>${known(t.entryCostUsd) ? esc(usd(t.entryCostUsd, 4)) + esc(costPct) : t.feeUsd ? esc(usd(t.feeUsd, 4)) + " fee" : "—"}</dd></div>
        </dl>
        <p class="glabel">Onchain</p>
        <dl class="receipt">
          <div><dt>Approval</dt><dd>${approveText(t.approveTx)}</dd></div>
          <div><dt>Flash order</dt><dd class="mono">${t.orderId ? esc(short(t.orderId)) : "—"}</dd></div>
          <div><dt>${t.tpOrderId ? "Stop loss order" : "Bracket order"}</dt><dd class="mono">${t.bracketOrderId ? esc(short(t.bracketOrderId)) : "—"}</dd></div>
          ${t.tpOrderId ? `<div><dt>Take profit order</dt><dd class="mono">${esc(short(t.tpOrderId))}</dd></div>` : ""}
          <div><dt>Entry tx</dt><dd>${txLink(t.entryTx)}</dd></div>
          <div><dt>Exit tx</dt><dd>${txLink(t.exitTx)}</dd></div>
        </dl>
        <div class="section-head section"><h2>Timeline</h2></div>
        <ol class="timeline" data-stagger>${steps.map((s) => `<li><strong>${esc(cap(s.step))}</strong><div class="muted small">${esc(s.detail || "")}</div><time class="when num">${esc(clock(s.at))}</time></li>`).join("")}
        ${waiting.map((s) => `<li class="wait"><strong class="muted">${esc(cap(s))}</strong><div class="when">Waiting</div></li>`).join("")}</ol>
      </section>
      <aside class="panel" data-reveal><p class="eyebrow">Call it</p><h3 class="panel-title">Will it hit the target?</h3>
        <div class="vote"><button class="btn btn-ghost" data-vote="1">It wins</button><button class="btn btn-ghost" data-vote="0">It loses</button></div>
        <div class="vote-bar" data-bar><i></i></div><p class="small muted" data-tally></p>
        <div class="btn-row"><button class="btn btn-primary" data-share>Share</button>${canCopy ? '<button class="btn btn-ghost" data-wcopy>Copy with my wallet</button>' : ""}</div>
      </aside>
    </div>`;
  $$(".ladder-mark", body).forEach((m) => {
    const p = Math.min(100, Math.max(0, +m.dataset.pos));
    m.style.setProperty("left", p + "%");
    if (m.classList.contains("sl")) m.classList.add("edge-l"); // outer labels hug the edges so they never clip
    if (m.classList.contains("tp")) m.classList.add("edge-r");
  });
  const costEl = $(".ladder-cost", body);
  if (costEl) { costEl.style.setProperty("left", Math.max(0, +costEl.dataset.l) + "%"); costEl.style.setProperty("width", Math.max(0, +costEl.dataset.w) + "%"); }

  const tally = (w, l) => {
    const total = w + l;
    $("[data-bar] i", body).style.setProperty("--win", (total ? (w / total) * 100 : 50) + "%");
    $("[data-tally]", body).textContent = total ? `${w} said it wins, ${l} said it loses` : ended ? "Voting closed before anyone called it." : "No votes yet. Make the first call.";
  };
  tally(d.predictions?.win || 0, d.predictions?.lose || 0);
  const voted = store.get("arena.voted." + t.id);
  if (voted != null || ended) $$("[data-vote]", body).forEach((b) => (b.disabled = true));
  $$("[data-vote]", body).forEach((b) => b.addEventListener("click", async () => {
    try {
      const r = await api("/v1/predictions", { method: "POST", body: { tradeId: t.id, voter: state.voter, win: b.dataset.vote === "1" } });
      store.set("arena.voted." + t.id, b.dataset.vote);
      $$("[data-vote]", body).forEach((x) => (x.disabled = true));
      tally(r.win, r.lose);
      toast("Vote counted", "ok");
    } catch (err) { toast(err.status === 409 ? "You already voted on this trade." : err.message, "err"); }
  }));
  $("[data-share]", body).addEventListener("click", () => shareTradeSheet(t, name));
  $("[data-wcopy]", body)?.addEventListener("click", () => walletTrade({ copyOf: t.id, token: tokenOf(t), from: name }));

  $("[data-cancel]", body)?.addEventListener("click", async (e) => {
    const btn = e.currentTarget; // currentTarget is null after an await
    if (!(await confirmDialog("Cancel this order?", "The entry hasn't filled yet. Cancelling withdraws it and its take profit and stop loss.", "Cancel order"))) return;
    const done = busy(btn, "Cancelling…");
    try { await api(`/v1/admin/trades/${encodeURIComponent(t.id)}/cancel`, { method: "POST", admin: true, timeout: 60000 }); toast("Order cancelled", "ok"); route(); }
    catch (err) { toast(err.message, "err"); done(); }
  });
  $("[data-exit]", body)?.addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    const funds = t.paper ? "Paper money: the sell is simulated at a real Flash quote." : "Real funds move on Base.";
    if (!(await confirmDialog("Exit this position now?", `This cancels the take profit and stop loss on Flash and sells the ${sym} at market. ${funds}`, "Exit now", { money: true }))) return;
    const done = busy(btn, "Exiting…");
    try {
      const r = await api(`/v1/admin/trades/${encodeURIComponent(t.id)}/exit`, { method: "POST", admin: true, timeout: 60000 });
      toast(r?.orderId ? `Exit sent to Flash (order ${short(r.orderId)})` : "Exit sent to Flash", "ok");
      route();
    } catch (err) { toast(err.message, "err"); done(); }
  });
}

/* ---------------- share cards + share sheet ---------------- */
// A 1200x630 PNG drawn in the browser: nothing is uploaded anywhere.
async function drawCard({ title, big, down = false, line = "", foot = "Every result is public. Orders by Definitive Flash on Base." }) {
  const c = document.createElement("canvas");
  c.width = 1200; c.height = 630;
  const g = c.getContext("2d");
  const F = "Outfit, system-ui, sans-serif";
  try { await document.fonts?.load?.(`700 76px Outfit`); } catch { /* system font then */ }
  // Same look as the app: a lime band with a black curve on top, then a black sheet.
  g.fillStyle = "#030303"; g.fillRect(0, 0, 1200, 630);
  g.fillStyle = "#bcff31"; g.fillRect(0, 0, 1200, 190);
  g.fillStyle = "#030303"; g.beginPath(); g.ellipse(600, 0, 820, 52, 0, 0, Math.PI * 2); g.fill();
  g.fillStyle = "#030303"; g.font = `700 34px ${F}`; g.fillText("Agent Arena", 72, 128);
  g.font = `500 22px ${F}`; g.textAlign = "right"; g.fillText("Social trading on Definitive Flash", 1128, 128); g.textAlign = "left";
  g.beginPath(); if (g.roundRect) g.roundRect(0, 160, 1200, 520, [48, 48, 0, 0]); else g.rect(0, 160, 1200, 520); g.fill();
  let size = 64;
  g.font = `700 ${size}px ${F}`;
  while (g.measureText(title).width > 1056 && size > 36) { size -= 4; g.font = `700 ${size}px ${F}`; }
  g.fillStyle = "#f6f6f6"; g.fillText(title, 72, 268);
  g.fillStyle = down ? "#ff4d4f" : "#bcff31"; g.font = `700 104px ${F}`; g.fillText(big, 72, 402);
  g.fillStyle = "#9a9a9a"; g.font = `500 28px ${F}`; g.fillText(line, 72, 466);
  g.fillStyle = "#171717"; g.beginPath(); if (g.roundRect) g.roundRect(72, 516, 1056, 72, 36); else g.rect(72, 516, 1056, 72); g.fill();
  g.fillStyle = "#bcff31"; g.beginPath(); g.arc(112, 552, 8, 0, Math.PI * 2); g.fill();
  g.fillStyle = "#f6f6f6"; g.font = `500 24px ${F}`; g.fillText(foot, 136, 561);
  return new Promise((r) => c.toBlob(r, "image/png"));
}
const tradeCard = (t, name) => drawCard({
  title: `${name}: Long ${tokenOf(t)}`,
  big: t.pnlUsd != null ? usd(t.pnlUsd) : `TP ${price(t.takeProfit)}`, down: t.pnlUsd != null && t.pnlUsd < 0,
  line: `${styleLabel(t.style)} · Entry ${price(t.fillPrice || t.quotedPrice || t.entry)} · SL ${price(t.stopLoss)}`,
});
const shareURL = (kind, id) => location.origin + "/s/" + kind + (id ? "/" + encodeURIComponent(id) : "");

function shareTradeSheet(t, name) {
  const text = t.pnlUsd != null
    ? `${name} closed ${usd(t.pnlUsd)} on ${tokenOf(t)} · on Flash`
    : `${name} just went long ${tokenOf(t)} on Flash · TP ${price(t.takeProfit)} / SL ${price(t.stopLoss)}`;
  openShare({ url: shareURL("trade", t.id), text, card: () => tradeCard(t, name), file: `arena-${t.id}.png` });
}

let sheetTimer = 0;
function sheetOpen(dlg) {
  clearTimeout(sheetTimer);
  dlg.classList.remove("closing");
  dlg.style.removeProperty("transform");
  if (dlg.showModal) { if (!dlg.open) dlg.showModal(); } else dlg.setAttribute("open", "");
}
// Closing slides the sheet away first (220ms), then closes the dialog.
function sheetClose(dlg) {
  if (dlg.contains(TOASTS)) document.body.append(TOASTS); // keep "Saved" and the like on screen
  const done = () => { dlg.classList.remove("closing"); dlg.style.removeProperty("transform"); if (dlg.close) dlg.close(); else dlg.removeAttribute("open"); };
  if (!dlg.open || reduceMotion) { done(); return; }
  dlg.classList.add("closing");
  clearTimeout(sheetTimer);
  sheetTimer = setTimeout(done, 220);
}
// One <dialog> is the bottom sheet for every action: share, wallet, order, invest, withdraw, help.
const sheet = $("[data-sheet]");
sheet?.addEventListener("click", (e) => {
  if (e.target === sheet || e.target.closest("[data-sheet-close]")) sheetClose(sheet);
});
sheet?.addEventListener("cancel", (e) => { e.preventDefault(); sheetClose(sheet); });
sheet?.addEventListener("close", () => { sheet.dispatchEvent(new CustomEvent("sheet:closed")); sheet.className = "sheet"; delete sheet.dataset.kind; });
// Swipe down on the top of the sheet to dismiss it (touch only).
(() => {
  if (!sheet) return;
  let y0 = null, dy = 0;
  sheet.addEventListener("touchstart", (e) => {
    const top = sheet.getBoundingClientRect().top;
    y0 = sheet.scrollTop <= 0 && e.touches[0].clientY - top < 72 ? e.touches[0].clientY : null;
    dy = 0;
  }, { passive: true });
  sheet.addEventListener("touchmove", (e) => {
    if (y0 === null) return;
    dy = Math.max(0, e.touches[0].clientY - y0);
    sheet.style.setProperty("transform", `translateY(${dy}px)`);
  }, { passive: true });
  sheet.addEventListener("touchend", () => {
    if (y0 === null) return;
    y0 = null;
    if (dy > 90) sheetClose(sheet); else sheet.style.removeProperty("transform");
  });
})();

// A titled sheet; returns its body element to fill.
function openSheet(title, { wide = false, kind = "" } = {}) {
  sheet.className = "sheet" + (wide ? " sheet-wide" : "");
  if (kind) sheet.dataset.kind = kind; else delete sheet.dataset.kind;
  sheet.innerHTML = `<div class="sheet-head"><h2 id="sheet-title">${esc(title)}</h2><button type="button" class="sheet-x" data-sheet-close aria-label="Close">×</button></div><div class="sheet-body" data-sheet-body></div>`;
  sheetOpen(sheet);
  return $("[data-sheet-body]", sheet);
}

// Operator sign-in as a sheet: opened from any admin-only spot, or after a 401.
function openSignIn(note = "") {
  if (sheet.open && sheet.dataset.kind === "signin") return;
  const sb = openSheet("Operator sign-in", { kind: "signin" });
  sb.innerHTML = `<form data-signin novalidate>
      ${note ? `<p class="alert">${esc(note)}</p>` : ""}
      <div class="field field-gap"><label for="si-tok">Admin token</label><input id="si-tok" name="token" type="password" autocomplete="off" required><span class="hint">ADMIN_TOKEN from the server's .env. Kept in this tab only.</span><span class="field-err" data-err="token" aria-live="polite"></span></div>
      <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" type="submit">Sign in</button></div></form>`;
  const f = $("[data-signin]", sb);
  setTimeout(() => { if (f.isConnected) $("input", f).focus(); }, 80);
  f.addEventListener("submit", async (e) => {
    e.preventDefault();
    const tok = String(new FormData(f).get("token") || "").trim();
    const errEl = $("[data-err=token]", f);
    if (tok.length < 16) { errEl.textContent = "That's too short. The token has at least 16 characters."; return; }
    errEl.textContent = "";
    const done = busy($("button[type=submit]", f), "Checking…");
    store.set("arena.admin", tok, true);
    try {
      await api("/v1/admin/wallets", { admin: true, quiet401: true });
      toast("Signed in", "ok");
      sheetClose(sheet);
      refreshApprovals();
      route();
    } catch (err) {
      store.del("arena.admin", true);
      errEl.textContent = err.status === 401 ? "That token was rejected. Check ADMIN_TOKEN in .env." : err.message;
      done();
    }
  });
}

// First visit: three steps, once. Re-open from Docs.
function openIntro() {
  const sb = openSheet("Welcome to Agent Arena", { kind: "intro" });
  sb.innerHTML = `<ol class="intro-steps">
      <li><div><b>Pick an agent</b><span>Ranked by Arena Score</span></div></li>
      <li><div><b>Copy it or auto-trade</b><span>Your limits, a stop loss on every order</span></div></li>
      <li><div><b>Watch it live</b><span>Every trade lands on the feed</span></div></li></ol>
    <p class="mode-note">${modeOf() === "live" ? '<span class="mini mini-live">Live · real money</span>' : modeOf() ? `<span class="mini mini-paper">${esc(MODE_INFO[modeOf()].badge)}</span> Nothing real at stake` : ""}</p>
    <div class="btn-row sheet-cta"><a class="btn btn-primary btn-block" href="#/leaderboard" data-intro-go>Pick an agent</a><button type="button" class="btn btn-plain btn-block" data-sheet-close>Not now</button></div>`;
  store.set("arena.intro", 1);
  $("[data-intro-go]", sb).addEventListener("click", () => sheetClose(sheet));
}
document.addEventListener("click", (e) => {
  if (e.target.closest("[data-signin-open]")) { e.preventDefault(); openSignIn(); }
  else if (e.target.closest("[data-intro-open]")) { e.preventDefault(); openIntro(); }
});

// Help lives behind a small "i" (or on the Docs page), not in the page.
const HELP = {
  score: ["Arena Score", `<p class="formula num">Return % − 0.5 × biggest drop %</p><p>Steady beats lucky. Win rate counts finished trades; arrows compare with your last visit.</p>`],
  test: ["What to watch", `<ol class="steps"><li><b>Quoted</b> Flash prices it; cost checked</li><li><b>Approved</b> allowance capped to the order</li><li><b>Signed</b> entry and bracket, field by field</li><li><b>Submitted</b> sent to Flash once</li><li><b>Filled</b> the coin lands</li><li><b>Bracket active</b> TP and SL live</li><li><b>Exit</b> Exit now, or TP / SL</li></ol>`],
  mode: ["Modes", `<ul><li><b>Paper</b> real Flash quotes and signing, simulated fills</li><li><b>Dry</b> signed, never sent</li><li><b>Live</b> real orders on Base, budget-capped</li></ul>`],
  wallets: ["Sub-wallets", `<ul><li>One wallet per agent, person and plan</li><li>Withdrawals only to the locked payout</li><li>Pause, loss limit and daily cap</li><li>Approvals capped; revoke anytime</li></ul>`],
  invest: ["Invest or copy", `<ul><li><b>Invest</b> the agent trades your sub-wallet on every signal</li><li><b>Copy</b> mirrors trades the agent places itself</li><li>Your limits and a stop loss on every trade</li></ul>`],
};
const infoBtn = (key) => `<button type="button" class="ibtn" data-info="${esc(key)}" aria-label="About ${esc(HELP[key]?.[0] || key)}">i</button>`;
document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-info]");
  if (!b || !HELP[b.dataset.info]) return;
  const [t, html] = HELP[b.dataset.info];
  openSheet(t).innerHTML = `<div class="help">${html}</div>`;
});

// Share targets: each platform's mark on its own colour, drawn inline (the
// CSP allows no outside images). Generic actions use line icons in lime.
const SVG_LINE = 'viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"';
const SHARE_ICON = {
  x: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M18.901 1.153h3.68l-8.04 9.19L24 22.846h-7.406l-5.8-7.584-6.638 7.584H.474l8.6-9.83L0 1.154h7.594l5.243 6.932ZM17.61 20.644h2.039L6.486 3.24H4.298Z"/></svg>',
  telegram: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M9.78 18.65l.28-4.23 7.68-6.92c.34-.31-.07-.46-.52-.19L7.74 13.3 3.64 12c-.88-.25-.89-.86.2-1.3l15.97-6.16c.73-.33 1.43.18 1.15 1.3l-2.72 12.81c-.19.91-.74 1.13-1.5.71L12.6 16.3l-1.99 1.93c-.23.23-.42.42-.83.42z"/></svg>',
  whatsapp: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M17.472 14.382c-.297-.149-1.758-.867-2.03-.967-.273-.099-.471-.148-.67.15-.197.297-.767.966-.94 1.164-.173.199-.347.223-.644.075-.297-.15-1.255-.463-2.39-1.475-.883-.788-1.48-1.761-1.653-2.059-.173-.297-.018-.458.13-.606.134-.133.298-.347.446-.52.149-.174.198-.298.298-.497.099-.198.05-.371-.025-.52-.075-.149-.669-1.612-.916-2.207-.242-.579-.487-.5-.669-.51-.173-.008-.371-.01-.57-.01-.198 0-.52.074-.792.372-.272.297-1.04 1.016-1.04 2.479 0 1.462 1.065 2.875 1.213 3.074.149.198 2.096 3.2 5.077 4.487.709.306 1.262.489 1.694.625.712.227 1.36.195 1.871.118.571-.085 1.758-.719 2.006-1.413.248-.694.248-1.289.173-1.413-.074-.124-.272-.198-.57-.347m-5.421 7.403h-.004a9.87 9.87 0 0 1-5.031-1.378l-.361-.214-3.741.982.998-3.648-.235-.374a9.86 9.86 0 0 1-1.51-5.26c.001-5.45 4.436-9.884 9.888-9.884 2.64 0 5.122 1.03 6.988 2.898a9.825 9.825 0 0 1 2.893 6.994c-.003 5.45-4.437 9.884-9.885 9.884m8.413-18.297A11.815 11.815 0 0 0 12.05 0C5.495 0 .16 5.335.157 11.892c0 2.096.547 4.142 1.588 5.945L.057 24l6.305-1.654a11.882 11.882 0 0 0 5.683 1.448h.005c6.554 0 11.89-5.335 11.893-11.893a11.821 11.821 0 0 0-3.48-8.413Z"/></svg>',
  link: `<svg ${SVG_LINE}><path d="M10 14a4.5 4.5 0 0 0 6.4 0l3-3a4.5 4.5 0 0 0-6.4-6.4l-1.1 1.1"/><path d="M14 10a4.5 4.5 0 0 0-6.4 0l-3 3a4.5 4.5 0 0 0 6.4 6.4l1.1-1.1"/></svg>`,
  card: `<svg ${SVG_LINE}><path d="M12 4v11M7 10l5 5 5-5M5 20h14"/></svg>`,
  more: `<svg ${SVG_LINE}><path d="M12 15V3.5M8 7.5l4-4 4 4"/><path d="M8 11H6.5A1.5 1.5 0 0 0 5 12.5v7A1.5 1.5 0 0 0 6.5 21h11a1.5 1.5 0 0 0 1.5-1.5v-7a1.5 1.5 0 0 0-1.5-1.5H16"/></svg>`,
  tiktok: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12.525.02c1.31-.02 2.61-.01 3.91-.02.08 1.53.63 3.09 1.75 4.17 1.12 1.11 2.7 1.62 4.24 1.79v4.03c-1.44-.05-2.89-.35-4.2-.97-.57-.26-1.1-.59-1.62-.93-.01 2.92.01 5.84-.02 8.75-.08 1.4-.54 2.79-1.35 3.94-1.31 1.92-3.58 3.17-5.91 3.21-1.43.08-2.86-.31-4.08-1.03-2.02-1.19-3.44-3.37-3.65-5.71-.02-.5-.03-1-.01-1.49.18-1.9 1.12-3.72 2.58-4.96 1.66-1.44 3.98-2.13 6.15-1.72.02 1.48-.04 2.96-.04 4.44-.99-.32-2.15-.23-3.02.37-.63.41-1.11 1.04-1.36 1.75-.21.51-.15 1.07-.14 1.61.24 1.64 1.82 3.02 3.5 2.87 1.12-.01 2.19-.66 2.77-1.61.19-.33.4-.67.41-1.06.1-1.79.06-3.57.07-5.36.01-4.03-.01-8.05.02-12.07z"/></svg>',
  instagram: `<svg ${SVG_LINE}><rect x="3" y="3" width="18" height="18" rx="5"/><circle cx="12" cy="12" r="4"/><circle cx="17.5" cy="6.5" r=".6" fill="currentColor"/></svg>`,
};
const shareBtn = (kind, label, attrs, sub = "") =>
  `<${attrs.startsWith("href") ? "a" : "button type=\"button\""} class="sheet-btn sh-${kind}" ${attrs}><span class="sh-ico">${SHARE_ICON[kind]}</span><b>${esc(label)}</b>${sub}</${attrs.startsWith("href") ? "a" : "button"}>`;

function openShare({ url, text, card, file = "agent-arena.png", title = "Share" }) {
  const u = enc(url), tx = enc(text);
  const targets = [
    ["x", "X", `https://x.com/intent/tweet?text=${tx}&url=${u}`],
    ["telegram", "Telegram", `https://t.me/share/url?url=${u}&text=${tx}`],
    ["whatsapp", "WhatsApp", `https://wa.me/?text=${enc(text + " " + url)}`],
  ];
  let blob = null;
  card?.().then((b) => { blob = b; }).catch(() => {});
  sheet.className = "sheet";
  sheet.innerHTML = `<div class="sheet-head"><h2 id="sheet-title">${esc(title)}</h2><button type="button" class="sheet-x" data-sheet-close aria-label="Close">×</button></div>
    <p class="sheet-text">${esc(text)}</p>
    <div class="sheet-grid share-grid">
      ${targets.map(([k, n, href]) => shareBtn(k, n, `href="${esc(href)}" ${EXT} aria-label="Share on ${esc(n)}"`)).join("")}
      ${shareBtn("link", "Copy link", 'data-sh="copy"')}
      ${card ? shareBtn("card", "Download card", 'data-sh="card"') : ""}
      ${navigator.share ? shareBtn("more", "More", 'data-sh="more" aria-label="More apps, such as TikTok and Instagram"', `<span class="sh-sub">${SHARE_ICON.tiktok}${SHARE_ICON.instagram}<i>TikTok · Instagram</i></span>`) : ""}
    </div>
    <p class="sheet-url mono">${esc(url)}</p>`;
  const getBlob = async () => blob || (card ? (blob = await card()) : null);
  $("[data-sh=copy]", sheet).addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(url); toast("Link copied", "ok"); } catch { toast("Couldn't copy. Long-press the link instead.", "err"); }
  });
  $("[data-sh=card]", sheet)?.addEventListener("click", async () => {
    const b = await getBlob();
    if (!b) { toast("Couldn't draw the card.", "err"); return; }
    const a = document.createElement("a");
    a.href = URL.createObjectURL(b); a.download = file; a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 2000);
    toast("Card saved", "ok");
  });
  $("[data-sh=more]", sheet)?.addEventListener("click", async () => {
    try {
      const f = blob ? new File([blob], file, { type: "image/png" }) : null;
      if (f && navigator.canShare?.({ files: [f] })) await navigator.share({ files: [f], text: `${text} ${url}` });
      else await navigator.share({ title: "Agent Arena", text, url });
    } catch { /* cancelled */ }
  });
  sheetOpen(sheet);
}

const FEED_FILTERS = { all: "Everything", trades: "Trades", copies: "Copies", blocked: "Blocked", approvals: "Approvals" };
const matchFilter = (f, k) => f === "all" || (f === "trades" && /^trade_(placed|filled|closed)$/.test(k)) || (f === "copies" && (k === "copy_placed" || k === "follow")) || (f === "blocked" && /blocked$/.test(k)) || (f === "approvals" && k === "approval");

async function pageFeed(page) {
  let filter = store.get("arena.feedFilter", "all");
  if (!FEED_FILTERS[filter]) filter = "all";
  page.innerHTML = `<header class="page-head"><p class="eyebrow" data-reveal>Live</p><h1 data-reveal>Live feed</h1><p class="lede" data-reveal>Every decision, as it happens.</p></header>
    <div class="filters" role="group" aria-label="Filter posts" data-reveal>${Object.entries(FEED_FILTERS).map(([k, v]) => `<button class="filter" data-f="${esc(k)}" aria-pressed="${k === filter}">${esc(v)}</button>`).join("")}</div>
    <div data-feed></div>`;
  const box = $("[data-feed]", page);
  const render = (newId) => {
    const items = state.feed.filter((p) => matchFilter(filter, p.kind));
    if (!items.length) {
      box.innerHTML = emptyState("Nothing here yet", state.sse === "open" ? "The feed is connected. Posts appear the moment an agent acts." : "Connecting to the live feed. Recent posts load every few seconds.");
      return;
    }
    const groups = [];
    for (const p of items) {
      const label = dayLabel(p.createdAt);
      if (!groups.length || groups.at(-1).label !== label) groups.push({ label, posts: [] });
      groups.at(-1).posts.push(p);
    }
    box.innerHTML = groups.map((g) => `<section class="feed-day"><h2 class="day-head"><span>${esc(g.label)}</span><span class="muted num">${esc(g.posts.length)} ${g.posts.length === 1 ? "post" : "posts"}</span></h2>
      <ul class="feed" data-stagger>${g.posts.map((p) => postHTML(p, p.id === newId)).join("")}</ul></section>`).join("");
  };
  $$("[data-f]", page).forEach((b) => b.addEventListener("click", () => {
    filter = b.dataset.f; store.set("arena.feedFilter", filter);
    $$("[data-f]", page).forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    render();
  }));
  if (!state.feed.length) await loadFeedOnce();
  render();
  const tick = setInterval(() => $$("time", box).forEach((el) => (el.textContent = ago(el.dateTime))), 30000);
  return {
    onPost: (p) => { if (matchFilter(filter, p.kind)) render(p.id); },
    cleanup: () => clearInterval(tick),
  };
}

async function pageApprovals(page) {
  page.innerHTML = `<header class="page-head"><p class="eyebrow" data-reveal>Human in the loop</p><h1 data-reveal>Approvals</h1><p class="lede" data-reveal>Copies over ${esc(usd(state.stats?.limits?.approvalThresholdUsd ?? 10, 0))} wait for a yes.</p></header><div data-list></div>`;
  const list = $("[data-list]", page);
  if (!isOperator()) {
    const recent = state.feed.filter((p) => p.kind === "approval").slice(0, 20);
    list.innerHTML = `<div class="panel action-row" data-reveal><h3>Approve or reject big copies</h3><div class="btn-row"><button type="button" class="btn btn-primary btn-sm" data-signin-open>Sign in</button></div></div>
      <div class="section-head section"><h2>Recent decisions</h2></div>
      ${recent.length ? `<ul class="feed" data-stagger>${recent.map((p) => postHTML(p)).join("")}</ul>` : emptyState("No approvals yet")}`;
    return {};
  }
  let timer;
  const render = async () => {
    let items;
    try { items = (await api("/v1/admin/approvals?all=1", { admin: true })).approvals || []; } catch (err) { list.innerHTML = errorState(err); return; }
    const pending = items.filter((a) => a.status === "pending");
    state.pendingApprovals = pending.length; updateChrome();
    const done = items.filter((a) => a.status !== "pending").slice(0, 15);
    list.innerHTML = (pending.length ? pending.map((a) => {
      const total = Math.max(1, new Date(a.expiresAt) - new Date(a.createdAt)), left = Math.max(0, new Date(a.expiresAt) - Date.now());
      const C = 2 * Math.PI * 22, off = C * (1 - left / total);
      return `<div class="panel approval" data-id="${esc(a.id)}">
        <svg class="ring" viewBox="0 0 54 54" aria-label="${Math.ceil(left / 1000)} seconds left"><circle class="bg" cx="27" cy="27" r="22"/><circle class="fg" cx="27" cy="27" r="22" stroke-dasharray="${C.toFixed(1)}" stroke-dashoffset="${off.toFixed(1)}"/><text x="27" y="31" text-anchor="middle">${Math.ceil(left / 1000)}s</text></svg>
        <div><h3>${esc(a.summary)}</h3><p class="muted small">${esc(a.walletId)} wants to copy ${esc(a.agentId)} · <span class="num">${esc(usd(a.amountUsd))}</span></p></div>
        <div class="btn-row"><button class="btn btn-danger" data-reject>Reject</button><button class="btn btn-primary" data-approve>Approve</button></div></div>`;
    }).join("") : emptyState("Nothing waiting for a yes")) +
    (done.length ? `<div class="section-head section"><h2>History</h2></div><ul class="feed">${done.map((a) => `<li class="post"><span class="tag tag-vote">Vote</span><div class="post-text">${esc(a.summary)}<div class="muted small">${esc(a.walletId)} · <span class="num">${esc(usd(a.amountUsd))}</span>${a.method ? " · " + esc(a.method) : ""}</div></div>${statusChip(a.status)}</li>`).join("")}</ul>` : "");
    $$("[data-id]", list).forEach((card) => {
      const decide = async (ok) => {
        $$("button", card).forEach((b) => (b.disabled = true));
        busy($(ok ? "[data-approve]" : "[data-reject]", card), ok ? "Approving…" : "Rejecting…");
        try {
          await api(`/v1/admin/approvals/${encodeURIComponent(card.dataset.id)}/${ok ? "approve" : "reject"}`, { method: "POST", admin: true });
          toast(ok ? "Approved. The copy is being placed." : "Rejected. No trade was placed.", "ok");
        } catch (err) { toast(err.status === 409 ? "Too late, this approval already expired." : err.message, "err"); }
        render();
      };
      $("[data-approve]", card).addEventListener("click", () => decide(true));
      $("[data-reject]", card).addEventListener("click", () => decide(false));
    });
  };
  await render();
  timer = setInterval(render, 3000);
  return { cleanup: () => clearInterval(timer), onPost: (p) => p.kind === "approval" && render() };
}

const STREAM = { live: "WebSocket, live", polling: "Polling every few seconds", off: "Off" };
const MODE_LINE = { paper: "Paper money: real quotes, simulated fills", dry: "Dry run: signed, not sent", live: "Live: real orders, real money" };
const MODE_CLASS = { paper: "warn", dry: "warn", live: "down" };
const modeFrom = (o) => (MODE_INFO[o?.mode] ? o.mode : o?.dryRun ? "dry" : o ? "live" : "");

// What each mode means for a test trade, in one sentence.
const TEST_EFFECT = {
  live: "Real money: it buys WETH on Base through Flash with its own take profit and stop loss. Nothing is copied.",
  paper: "Paper money: real Flash quote and real signatures, simulated fill. No real money moves.",
  dry: "Dry run: it is quoted and signed, but not sent.",
};
async function runTestTrade(walletId, btn) {
  const f = state.flash || {};
  const size = usd(f.testTradeUsd ?? 1);
  const mode = modeFrom(f) || "dry";
  const ok = await confirmDialog(`Run a ${size} test trade from ${walletId}?`, TEST_EFFECT[mode], "Run test trade", { money: true });
  if (!ok) return;
  const done = busy(btn, "Placing…");
  try {
    const r = await api("/v1/admin/test-trade", { method: "POST", admin: true, body: { walletId }, timeout: 60000 });
    toast("Test trade placed", "ok");
    if (r?.trade?.id) location.hash = "#/trade/" + encodeURIComponent(r.trade.id);
  } catch (err) {
    toast(err.message, "err");
  } finally { done(); }
}

function testPanel(f, wallets) {
  if (!f) return "";
  const mode = modeFrom(f);
  const spent = +(f.liveSpentUsd || 0), budget = +(f.liveBudgetUsd || 0);
  const share = budget > 0 ? Math.min(100, (spent / budget) * 100) : 0;
  return `<div class="test-grid">
    <div class="panel">
      <p class="chips"><span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode]?.badge || "Mode")}</span><span class="mini">Nothing copied</span></p>
      <dl class="spec spec-tight">
        <div><dt>Test size</dt><dd>${esc(usd(f.testTradeUsd ?? 1))}</dd></div>
        <div><dt>Cost cap</dt><dd>${known(f.testMaxCostPct) ? esc(fixed(f.testMaxCostPct, 0)) + "%" : "—"}</dd></div>
        <div><dt>Live budget</dt><dd>${esc(usd(spent))} of ${esc(usd(budget))}</dd></div>
        <div><dt>Agents on real money</dt><dd class="${f.liveAgents ? "down" : "up"}">${f.liveAgents ? "On" : "Off"}</dd></div>
      </dl>
      <div class="budget-bar" data-budget="${share.toFixed(1)}" aria-hidden="true"><i></i></div>
      <form class="btn-row" data-test>
        <label class="sr-only" for="tw">Wallet</label>
        <select id="tw" name="walletId" class="select-sm">${wallets.map((w) => `<option value="${esc(w.id)}">${esc(w.agentName || w.profile?.owner || w.id)} · ${esc(usd(w.usdc))}</option>`).join("")}</select>
        <button class="btn btn-primary btn-sm" type="submit" ${wallets.length ? "" : "disabled"}>Run ${esc(usd(f.testTradeUsd ?? 1))} test trade</button>
      </form>
    </div>
  </div>
  <div class="panel danger-zone">
    <div><h3>Emergency stop</h3><p class="small muted">${f.halted ? "Halted. Open positions keep their stop loss." : "Stops new orders, cancels unfilled ones."}</p></div>
    <div class="btn-row">${f.halted ? '<button class="btn btn-primary btn-sm" data-resume-all>Resume trading</button>' : '<button class="btn btn-danger btn-sm" data-halt>Emergency stop</button>'}</div>
  </div>`;
}

function flashPanel(f) {
  if (!f) return `<p class="muted small">Connection details aren't available from this server version.</p>`;
  const search = f.searchEligible === true ? "Available" : f.searchEligible === false ? "Not enabled for this key. Prices come from the onchain pool." : "Not checked yet";
  const settle = Array.isArray(f.settlement) && f.settlement.length ? f.settlement.map(addrLink).join("<br>") : '<span class="warn">Not pinned yet</span>';
  return `<dl class="spec" data-stagger>
    <div><dt>Mode</dt><dd class="${MODE_CLASS[modeFrom(f)] || ""}">${esc(MODE_LINE[modeFrom(f)] || "—")}</dd></div>
    <div><dt>Order updates</dt><dd class="${f.stream === "live" ? "up" : f.stream === "off" ? "down" : ""}">${esc(STREAM[f.stream] || f.stream || "—")}</dd></div>
    <div><dt>Flash search</dt><dd class="${f.searchEligible === false ? "warn" : ""}">${esc(search)}</dd></div>
    <div><dt>Price source</dt><dd>${esc(PRICE_SOURCE[f.priceSource]?.line || f.priceSource || "—")}</dd></div>
    <div><dt>Price pool</dt><dd>${f.pool ? addrLink(f.pool) : "—"}</dd></div>
    <div><dt>Settlement contract</dt><dd>${settle}</dd></div>
    <div><dt>Last price</dt><dd>${f.lastPriceAt ? esc(ago(f.lastPriceAt)) : "—"}</dd></div>
    <div><dt>Chain</dt><dd class="num">${f.chainId === 8453 ? "Base (8453)" : esc(f.chainId ?? "—")}</dd></div>
  </dl>`;
}

async function pageOperator(page) {
  if (!isOperator()) {
    page.innerHTML = `<header class="page-head"><h1 data-reveal>Manage</h1><p class="lede" data-reveal>Wallets, approvals, orders and the emergency stop.</p></header>
      <div class="panel action-row" data-reveal><div><p class="eyebrow">Operator</p><h3>Sign in to manage the arena</h3></div><div class="btn-row"><button type="button" class="btn btn-primary" data-signin-open>Sign in</button></div></div>
      <div class="section-head section"><h2>Explore</h2></div>
      <nav class="hub" aria-label="More" data-reveal>
        <a class="hub-item" href="#/leaderboard"><b>Leaderboard</b><span>Ranked by Arena Score</span></a>
        <a class="hub-item" href="#/me"><b>My wallet</b><span>Trade from your own wallet</span></a>
        <a class="hub-item" href="#/signals"><b>Signals</b><span>Copy or share any trade</span></a>
        <a class="hub-item" href="#/proof"><b>Onchain proof</b><span>Real Flash orders and Base txs</span></a>
        <a class="hub-item" href="#/docs"><b>Docs</b><span>Every feature, one page</span></a>
        <a class="hub-item" href="#/feed"><b>Live feed</b><span>Every decision, live</span></a>
      </nav>${installBlock()}`;
    wireInstall(page);
    return {};
  }
  page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Signed in</p><h1 data-reveal>Operator</h1></div><div class="btn-row"><button class="btn btn-primary btn-sm" data-new-order disabled>New order</button><button class="btn btn-plain btn-sm" data-logout>Sign out</button></div></header>
    <nav class="hub" aria-label="Operator tools" data-reveal>
      <a class="hub-item" href="#/admin/wallets"><b>Wallets</b><span>Balances, limits, withdraw</span></a>
      <a class="hub-item" href="#/admin/approvals"><b>Approvals <span class="count" data-approvals-count hidden></span></b><span>Big copies waiting for a yes</span></a>
      <a class="hub-item" href="#/plans"><b>Auto-trade</b><span>Plans and what they're doing</span></a>
      <a class="hub-item" href="#/feed"><b>Live feed</b><span>Every decision, live</span></a>
      <a class="hub-item" href="#/leaderboard"><b>Leaderboard</b><span>Ranked by Arena Score</span></a>
      <a class="hub-item" href="#/me"><b>My wallet</b><span>Trade from your own wallet</span></a>
      <a class="hub-item" href="#/docs"><b>Docs</b><span>Every feature, one page</span></a>
    </nav>
    <div class="ops-grid">
      <section><div class="section-head"><h2>Server</h2></div><div class="panel" data-ready><div class="skel skel-row"></div></div></section>
      <section><div class="section-head"><h2>Flash</h2></div><div class="panel" data-flash><div class="skel skel-row"></div></div></section>
    </div>
    <div class="section-head section"><h2>Test trade</h2>${infoBtn("test")}</div><div data-testpanel><div class="skel skel-row"></div></div>
    <div class="section-head section"><h2>Wallets</h2><a class="btn btn-ghost btn-sm" href="#/admin/wallets">Manage</a></div><div data-wallets><div class="skel skel-row"></div></div>
    <div class="section-head section"><h2>Manual agent trade</h2></div><form class="panel" data-propose novalidate></form>
    ${installBlock()}`;
  $("[data-logout]", page).addEventListener("click", () => { store.del("arena.admin", true); state.pendingApprovals = 0; updateChrome(); toast("Signed out", "ok"); route(); });
  wireInstall(page);

  const [ready, flash] = await Promise.all([
    fetch("/readyz").then((x) => x.json()).catch(() => null),
    api("/v1/flash/status").then((f) => { state.flash = f; return f; }).catch(() => null),
  ]);
  $("[data-ready]", page).innerHTML = ready ? `<dl class="spec" data-stagger>
      <div><dt>Mode</dt><dd class="${MODE_CLASS[modeFrom(ready)] || ""}">${esc(MODE_INFO[modeFrom(ready)]?.badge || "—")}</dd></div>
      <div><dt>Price feed</dt><dd class="${ready.priceFresh ? "up" : "down"}">${ready.priceFresh ? "Fresh" : "Stale"}</dd></div>
      <div><dt>Arkiv queue</dt><dd class="num">${esc(ready.outboxPending ?? "—")} waiting</dd></div>
      <div><dt>Uptime</dt><dd class="num">${esc(Math.floor((ready.uptimeSec || 0) / 60))} min</dd></div>
      <div><dt>Fees earned</dt><dd class="num">${esc(feesText())}</dd></div></dl>`
    : `<p class="down">Health check failed. Is the server running?</p>`;
  $("[data-flash]", page).innerHTML = flashPanel(flash);
  updateChrome();

  let wallets = [];
  try {
    wallets = (await api("/v1/admin/wallets", { admin: true })).wallets || [];
    // Agents first, then the first few people; the full list lives on Wallets.
    const shown = [...wallets.filter((w) => w.role === "agent"), ...wallets.filter((w) => w.role !== "agent")].slice(0, 8);
    const rest = wallets.length - shown.length;
    $("[data-wallets]", page).innerHTML = wallets.length ? `<table class="table table-plain"><thead><tr><th>Wallet</th><th class="hide-sm">Role</th><th>Address</th><th class="r">USDC</th><th class="r hide-sm">WETH</th></tr></thead><tbody data-stagger>${shown.map((w) => `<tr><td><strong>${esc(w.agentName || w.profile?.owner || w.id)}</strong>${w.profile?.paused ? ' <span class="mini mini-test">Paused</span>' : ""}</td><td class="hide-sm muted">${esc(w.role === "agent" ? "Agent" : "Person")}</td><td>${addrLink(w.address)}</td><td class="r num">${esc(fixed(w.usdc, 2))}</td><td class="r num hide-sm">${esc(fixed(w.weth, 6))}</td></tr>`).join("")}${rest > 0 ? `<tr><td colspan="5"><a class="link" href="#/admin/wallets">+${esc(rest)} more on Wallets</a></td></tr>` : ""}</tbody></table>`
      : emptyState("No wallets", "Run arena keygen, fund the addresses and restart the server.");
  } catch (err) { $("[data-wallets]", page).innerHTML = errorState(err); }

  // New order: every Flash order style, in a sheet.
  const tradeable = wallets.filter((w) => w.role !== "self");
  const tks = ((await api("/v1/tokens").catch(() => null))?.tokens || []).filter((t) => t.symbol);
  const nob = $("[data-new-order]", page);
  if (nob) {
    nob.disabled = false;
    nob.addEventListener("click", () => {
      if (!tradeable.length) { toast("No wallets to trade from yet. Run arena keygen first.", "err"); return; }
      if (!tks.length) { toast("No coin has a live price yet. Try again in a minute.", "err"); return; }
      const sb = openSheet("New order", { wide: true });
      sb.innerHTML = ticketHTML(tradeable, tradeable[0].id, tks);
      wireTicket($("[data-ticket]", sb));
    });
  }

  const tp = $("[data-testpanel]", page);
  const drawTest = () => {
    tp.innerHTML = testPanel(state.flash, tradeable);
    const bb = $("[data-budget]", tp);
    if (bb) $("i", bb).style.setProperty("width", bb.dataset.budget + "%");
    $("[data-test]", tp)?.addEventListener("submit", (e) => {
      e.preventDefault();
      runTestTrade(new FormData(e.currentTarget).get("walletId"), $("button[type=submit]", e.currentTarget));
    });
    $("[data-halt]", tp)?.addEventListener("click", async (e) => {
      const btn = e.currentTarget;
      if (!(await confirmDialog("Stop all trading?", "No new orders from any agent, copy or test. Entries that haven't filled are cancelled at Flash. Open positions keep their stop loss.", "Emergency stop"))) return;
      busy(btn, "Stopping…");
      try {
        const r = await api("/v1/admin/emergency-stop", { method: "POST", admin: true });
        toast(`Trading halted. Cancelled ${r?.cancelled ?? 0} pending order${r?.cancelled === 1 ? "" : "s"}.`, "ok");
        if (r?.errors?.length) toast(`Some cancels failed: ${r.errors.join("; ")}`, "err");
      } catch (err) { toast(err.message, "err"); }
      await refreshStats(); drawTest();
    });
    $("[data-resume-all]", tp)?.addEventListener("click", async (e) => {
      e.currentTarget.disabled = true;
      try { await api("/v1/admin/resume", { method: "POST", admin: true }); toast("Trading resumed", "ok"); } catch (err) { toast(err.message, "err"); }
      await refreshStats(); drawTest();
    });
  };
  drawTest();

  const agents = wallets.filter((w) => w.role === "agent");
  const px = state.price?.last || 0;
  const form = $("[data-propose]", page);
  form.innerHTML = `<div class="form-grid">
      <div class="field"><label for="pa">Agent</label><select id="pa" name="agentId">${agents.map((w) => `<option value="${esc(w.id)}">${esc(w.agentName || w.id)}</option>`).join("")}</select></div>
      <div class="field"><label for="ps">Share of balance (%)</label><input id="ps" name="sizePct" type="number" min="1" max="25" value="20" inputmode="numeric" required><span class="hint">Max ${esc(usd(state.stats?.limits?.maxTradeUsd ?? 5, 0))} a trade</span></div>
      <div class="field"><label for="pt">Take profit (%)</label><input id="pt" name="tpPct" type="number" min="0.5" max="20" step="0.1" value="3" inputmode="decimal" required><span class="hint">Sells when price rises this much</span></div>
      <div class="field"><label for="pl">Stop loss (%)</label><input id="pl" name="slPct" type="number" min="0.5" max="5" step="0.1" value="2" inputmode="decimal" required><span class="hint">Sells when price falls this much</span></div>
    </div>
    <div class="field field-gap"><label for="pr">Reason shown in the feed</label><input id="pr" name="reason" maxlength="140" value="Manual trade by the operator"><span class="hint">One short line</span></div>
    <p class="ticket-preview num" data-psum aria-live="polite"></p>
    <div class="btn-row"><button class="btn btn-primary" type="submit" ${agents.length && px ? "" : "disabled"}>Place trade</button></div>`;
  liveNumbers(form);
  const hints = () => {
    const f = Object.fromEntries(new FormData(form));
    $("[data-psum]", form).textContent = px ? `${f.agentId || "Agent"} buys ${f.sizePct || "?"}% of its USDC in ${state.price.symbol || "WETH"} at ≈ ${price(px)} · TP ${price(px * (1 + f.tpPct / 100))} · SL ${price(px * (1 - f.slPct / 100))}` : "Waiting for a live price";
  };
  form.addEventListener("input", hints); form.addEventListener("change", hints); hints();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const bad = checkNumbers(form);
    if (bad) { bad.focus(); return; }
    const f = Object.fromEntries(new FormData(form));
    f.tp = f.tpPct; f.sl = f.slPct;
    const effect = { live: "This spends real USDC on Base.", paper: "Real quote and signatures, simulated fill.", dry: "Signed but not sent." }[modeOf()] || "";
    const ok = await confirmDialog(`Place a trade for ${f.agentId}?`, `${f.sizePct}% of its USDC, take profit +${f.tp}%, stop loss −${f.sl}%. ${effect}`, "Place trade", { money: true });
    if (!ok) return;
    const btn = $("button[type=submit]", form);
    const done = busy(btn, "Placing…");
    try {
      const r = await api("/v1/admin/propose", { method: "POST", admin: true, timeout: 60000, body: { agentId: f.agentId, sizePct: +f.sizePct, takeProfit: +(px * (1 + f.tp / 100)).toFixed(2), stopLoss: +(px * (1 - f.sl / 100)).toFixed(2), reason: f.reason } });
      toast("Trade placed", "ok", { label: "View", run: () => (location.hash = "#/trade/" + r.trade.id) });
    } catch (err) {
      toast(err.message, "err");
    } finally { done(); }
  });
  return {};
}

/* ---------------- auto-trade plans ---------------- */
const STRAT = {
  auto: { label: "AI auto-pilot", line: "Uses whichever strategy has the best Arena Score right now" },
  trend: { label: "Trend", line: "Buys when the short average crosses above the long one" },
  dip: { label: "Dip buyer", line: "Buys after a 1.5% drop from the recent high" },
  contrarian: { label: "Contrarian", line: "Buys after 4 falling ticks in a row" },
  interval: { label: "Interval", line: "Buys every few minutes when no trade is open" },
};
const stratLabel = (s) => STRAT[s]?.label || cap(s || "");
const PLAN_STATUS = { active: ["Running", "filled"], paused: ["Paused", "pending"], ended: ["Ended", "closed"], stopped: ["Stopped", "cancelled"] };
const planChip = (s) => { const [l, c] = PLAN_STATUS[s] || [cap(s || "unknown"), "closed"]; return `<span class="chip chip-${esc(c)}">${esc(l)}</span>`; };
const fmtDate = (ts) => { const d = new Date(ts); return Number.isFinite(d.getTime()) ? d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—"; };
function timeLeft(end) {
  const ms = new Date(end).getTime() - Date.now();
  if (!Number.isFinite(ms)) return "—";
  if (ms <= 0) return "Time's up";
  const m = Math.round(ms / 60000);
  if (m < 60) return `${m}m left`;
  const h = Math.floor(m / 60);
  return h < 48 ? `${h}h ${m % 60}m left` : `${Math.round(h / 24)}d left`;
}
const durText = (h) => (!(h > 0) ? "—" : h < 1 ? `${Math.round(h * 60)} minutes` : h < 48 ? `${+h.toFixed(2)} hour${h === 1 ? "" : "s"}` : `${+(h / 24).toFixed(1)} days`);
const planRight = (p) => (p.status === "active" ? timeLeft(p.endsAt) : PLAN_STATUS[p.status]?.[0] || cap(p.status || ""));

function planCard(p, compact = false) {
  const s = p.stats || {};
  const live = p.status === "active";
  const think = p.lastSignal || (live ? "Starting up" : p.statusReason ? cap(p.statusReason) : "");
  return `<a class="pcard${live ? " is-live" : ""}" href="#/plan/${enc(p.id)}" data-reveal>
    <div class="pcard-top"><span class="coin">${esc(p.token)}</span><span class="mini">${esc(stratLabel(p.strategy))}</span>${p.style && p.style !== "market" ? styleChip(p.style) : ""}${planChip(p.status)}</div>
    <h3>${esc(p.name)}</h3>
    <p class="muted small num">${esc(p.owner)} · ${esc(usd(p.tradeUsd, 0))} a trade · +${esc(p.takeProfitPct)}% / −${esc(p.stopLossPct)}%</p>
    ${think ? `<p class="pcard-think">${live ? '<span class="think-dot" aria-hidden="true"></span>' : ""}<span>${esc(think)}</span></p>` : ""}
    ${compact
      ? `<p class="pcard-foot num">${pnlHTML(s.pnlUsd)}<span>${esc(plural(s.trades, "trade"))}</span><span>${esc(planRight(p))}</span></p>`
      : `<dl class="pstats">
      <div><dt>P&amp;L</dt><dd class="num">${pnlHTML(s.pnlUsd)}</dd></div>
      <div><dt>Trades</dt><dd class="num">${esc(s.trades ?? 0)}${s.closed ? `<span class="muted"> · ${esc(s.wins)} won</span>` : ""}</dd></div>
      <div><dt>${live ? "Ends" : "Status"}</dt><dd class="num">${esc(planRight(p))}</dd></div></dl>`}
  </a>`;
}

async function pagePlans(page) {
  page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Auto-trade</p><h1 data-reveal>Auto-trade plans</h1>
      <p class="lede" data-reveal>Pick a coin, a brain and your limits.</p></div>
      <a class="btn btn-primary" href="#/plans/new">Create a plan</a></header>
    <div class="section-tight" data-plans><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const box = $("[data-plans]", page);
  const render = async () => {
    let d;
    try { d = await api("/v1/plans"); } catch (err) { box.innerHTML = errorState(err); return; }
    const plans = d.plans || [];
    let html;
    if (!plans.length) html = emptyState("No plans yet", "Set one up in a minute: coin, strategy, size and stop loss.", { href: "#/plans/new", label: "Create a plan" });
    else {
      const running = plans.filter((p) => p.status === "active" || p.status === "paused");
      const done = plans.filter((p) => !(p.status === "active" || p.status === "paused"));
      html = (running.length ? `<div class="plan-grid">${running.map((p) => planCard(p)).join("")}</div>` : emptyState("Nothing running", "Your finished plans are below.", { href: "#/plans/new", label: "Create a plan" }))
        + (done.length ? `<div class="section-head section"><h2>Finished</h2><span class="muted small num">${done.length}</span></div><div class="plan-grid">${done.map((p) => planCard(p)).join("")}</div>` : "");
    }
    if (box.dataset.sig !== html) { box.dataset.sig = html; box.innerHTML = html; }
  };
  await render();
  const t = setInterval(() => { if (document.visibilityState === "visible") render(); }, 10000);
  return { cleanup: () => clearInterval(t) };
}

const DUR_CHIPS = [["1", "1 hour"], ["6", "6 hours"], ["24", "1 day"], ["168", "7 days"]];
const MODE_HINT = {
  paper: "Paper money: the budget is funded with paper USDC. No real money moves.",
  dry: "Dry run: orders are signed, not sent.",
  live: "Live: you fund the new wallet with USDC on Base. Real money.",
};

async function pagePlanNew(page) {
  page.innerHTML = `<a class="back" href="#/plans">Auto-trade</a>
    <header class="page-head"><p class="eyebrow" data-reveal>New plan</p><h1 data-reveal>Build your auto-trader</h1>
      <p class="lede" data-reveal>Six quick steps. Every order has a stop loss.</p></header>
    <div class="section-tight" data-builder><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const box = $("[data-builder]", page);
  // Anyone can build one: people sign in with their wallet and the plan runs on
  // their own arena account (payout locked to them). Operators keep the
  // admin path, where each plan gets a new sub-wallet.
  const asUser = !isOperator();
  if (asUser && !(wallet.address && meToken())) {
    box.innerHTML = `<div class="panel acct-intro" data-reveal><p class="eyebrow">Your own auto-trader</p><h3>Sign in with your wallet to build one</h3>
      <p class="muted small">It trades from your arena account with USDC only, no gas. Money can only go back to your wallet.</p>
      <button type="button" class="btn btn-primary btn-block" data-plan-signin>Connect &amp; sign in</button>
      <p class="footnote">Operator? <button type="button" class="btn-link" data-signin-open>Use the admin token</button></p></div>`;
    $("[data-plan-signin]", box).addEventListener("click", async (e) => {
      const done = busy(e.currentTarget, "Check your wallet…");
      try { await ensureBase(); if (await meSignIn()) route(); } catch (err) { toast(err?.code ? walletErr(err) : err.message, "err"); } finally { done(); }
    });
    return {};
  }
  let meta;
  try { [meta] = await Promise.all([api("/v1/tokens"), state.flash ? null : refreshStats()]); } catch (err) { box.innerHTML = errorState(err); return {}; }
  const L = meta.limits || {}, tokens = (meta.tokens || []).filter((t) => t.symbol);
  if (!tokens.length) { box.innerHTML = emptyState("No coins available", "The server has no tradeable coins with a live price yet. Try again in a minute."); return {}; }
  const mode = meta.mode || modeOf();
  const maxTrade = +(L.maxTradeUsd ?? 5), maxDay = +(L.maxTradesPerDay ?? 5);
  const minTP = +(L.minTakeProfitPct ?? 0.6), maxSL = +(L.maxStopLossPct ?? 5), maxSlip = +(L.maxSlippagePct ?? 5), maxDur = +(L.maxDurationHours ?? 720);
  const defTrade = Math.min(10, maxTrade);
  const fld = (name, label, attrs, value, hint = "", cls = "") => `<div class="field ${cls}"><label for="pf-${name}">${esc(label)}</label>
    <input id="pf-${name}" name="${name}" ${attrs} value="${esc(value)}" aria-describedby="pe-${name}">${hint ? `<span class="hint">${esc(hint)}</span>` : ""}<span class="field-err" id="pe-${name}" data-err="${name}"></span></div>`;
  const brains = Object.keys(STRAT).filter((k) => !meta.strategies || meta.strategies[k]);
  box.innerHTML = `<form class="builder" id="plan-form" data-plan-form novalidate>
    <div class="builder-main">
      <fieldset class="step"><legend><span class="step-n">1</span>Coin</legend>
        <div class="choice-grid">${tokens.map((t, i) => `<label class="choice"><input type="radio" name="token" value="${esc(t.symbol)}" ${i ? "" : "checked"}>
          <span class="choice-body"><b>${esc(t.symbol)}</b><span class="num muted small">${esc(price(t.price))}</span></span></label>`).join("")}</div>
      </fieldset>
      <fieldset class="step"><legend><span class="step-n">2</span>Brain</legend>
        <div class="choice-grid choice-wide">${brains.map((k) => `<label class="choice"><input type="radio" name="strategy" value="${esc(k)}" ${k === "auto" ? "checked" : ""}>
          <span class="choice-body"><b>${esc(STRAT[k].label)}</b><span class="muted small">${esc(STRAT[k].line)}</span></span></label>`).join("")}</div>
        <div data-interval hidden>${fld("intervalMin", "Buy every (minutes)", 'type="number" min="1" max="1440" step="1" inputmode="numeric"', 30, "Time between buys", "field-gap")}</div>
      </fieldset>
      <fieldset class="step"><legend><span class="step-n">3</span>Entry</legend>
        <div class="seg" role="radiogroup" aria-label="Entry">${Object.keys(STYLE).map((s, i) => `<label><input type="radio" name="style" value="${s}" ${i ? "" : "checked"}><span>${esc(STYLE[s])}</span></label>`).join("")}</div>
        <div class="form-grid form-3 field-gap">
          <div data-ew="limit" hidden>${fld("limitPct", "Limit below (%)", 'type="number" min="0.1" max="10" step="0.1" inputmode="decimal"', 0.5, "Buys only after this dip")}</div>
          <div data-ew="twap" hidden>${fld("twapMinutes", "DCA over (min)", 'type="number" min="5" max="1440" step="1" inputmode="numeric"', 30, "1 slice per minute")}</div>
          <div data-ew="stop" hidden>${fld("stopPct", "Trigger above (%)", 'type="number" min="0.1" max="10" step="0.1" inputmode="decimal"', 0.5, "Buys on a breakout this far up")}</div>
        </div>
        <div data-ew="stop" hidden><div class="field field-gap"><span class="label">Protection after the fill</span><div class="seg" role="radiogroup" aria-label="Protection">
          <label><input type="radio" name="protect" value="bracket" checked><span>Bracket</span></label><label><input type="radio" name="protect" value="trailing"><span>Trailing</span></label><label><input type="radio" name="protect" value="separate"><span>SL + TP</span></label></div></div></div>
        <div data-ew="trail" hidden>${fld("trailPct", "Trail (%)", 'type="number" min="0.5" max="20" step="0.1" inputmode="decimal"', 2, "Sells after this pullback from the high", "field-gap")}</div>
      </fieldset>
      <fieldset class="step"><legend><span class="step-n">4</span>Money</legend>
        <div class="form-grid form-3">
          ${fld("tradeUsd", "Per trade ($)", `type="number" min="1" max="${maxTrade}" step="0.5" inputmode="decimal"`, defTrade, `Up to ${usd(maxTrade, 0)}`)}
          ${fld("budgetUsd", "Budget ($)", 'type="number" min="1" max="10000" step="1" inputmode="decimal"', defTrade * 3, "The most it can use")}
          ${fld("maxTradesPerDay", "Trades a day", `type="number" min="1" max="${maxDay}" step="1" inputmode="numeric"`, maxDay, `Up to ${maxDay}`)}
        </div>
      </fieldset>
      <fieldset class="step"><legend><span class="step-n">5</span>Risk and time</legend>
        <div class="form-grid form-4">
          ${fld("takeProfitPct", "Take profit (%)", `type="number" min="${minTP}" max="50" step="0.1" inputmode="decimal"`, 2, "Sells when price rises this much")}
          ${fld("stopLossPct", "Stop loss (%)", `type="number" min="0.2" max="${maxSL}" step="0.1" inputmode="decimal"`, 1, `Sells when price falls this much · up to ${maxSL}%`)}
          ${fld("maxSlippagePct", "Max slippage (%)", `type="number" min="0.1" max="${maxSlip}" step="0.1" inputmode="decimal"`, 1, "How far the price may move while filling")}
          ${fld("lossLimitPct", "Stop after losing (%)", 'type="number" min="0" max="100" step="1" inputmode="numeric"', 50, "Of the budget. 0 = all of it")}
        </div>
        <div class="field field-gap"><span class="label">Run for</span>
          <div class="seg" role="radiogroup" aria-label="Run for">${DUR_CHIPS.map(([v, l]) => `<label><input type="radio" name="dur" value="${v}" ${v === "24" ? "checked" : ""}><span>${esc(l)}</span></label>`).join("")}<label><input type="radio" name="dur" value="custom"><span>Custom</span></label></div>
        </div>
        <div data-custom hidden>${fld("customHours", "Hours", `type="number" min="0.25" max="${maxDur}" step="0.25" inputmode="decimal"`, 12, "15 minutes to 30 days", "field-gap")}</div>
        <label class="check"><input type="checkbox" name="exitAtEnd" checked><span>Sell what it holds when the plan ends</span></label>
      </fieldset>
      <fieldset class="step"><legend><span class="step-n">6</span>You</legend>
        <div class="form-grid form-2">
          ${fld("name", "Plan name", 'maxlength="40" autocomplete="off" placeholder="BTC steady"', "", "Just for you, shown on the plan")}
          ${asUser ? `<p class="small muted">Runs on your arena account. Money can only go back to ${esc(short(wallet.address))}.</p>`
            : `${fld("owner", "Your name", 'maxlength="40" autocomplete="name"', "", "Shown as the plan's owner")}
          ${fld("payout", "Payout address", 'class="mono" maxlength="42" spellcheck="false" autocomplete="off" placeholder="0x…"', "", "Withdrawals only ever go here. It can't be changed later.", "span-2")}`}
        </div>
      </fieldset>
    </div>
    <aside class="builder-side">
      <div class="panel summary" data-summary aria-live="polite"></div>
      <button class="btn btn-primary btn-block" type="submit" data-submit>Start plan</button>
      <p class="mode-note">${mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode]?.badge || mode)}</span>` : ""}<span>${esc(asUser ? ({ paper: "Uses your account (paper USDC)", dry: "Signed, not sent", live: "Uses your account's USDC" }[mode] || "") : ({ paper: "Budget funded with paper USDC", dry: "Signed, not sent", live: "You fund it with real USDC" }[mode] || ""))}</span></p>
    </aside>
  </form>
  <div class="mobile-cta"><span class="num small" data-mini-sum></span><button class="btn btn-primary" type="submit" form="plan-form" data-submit>Start plan</button></div>`;

  const form = $("[data-plan-form]", box);
  clampPct(form);
  const touched = new Set();
  let submitted = false;
  const read = () => {
    const f = new FormData(form), n = (k) => num(f.get(k));
    const dur = f.get("dur");
    return {
      token: f.get("token"), strategy: f.get("strategy"), intervalMin: Math.round(n("intervalMin")),
      tradeUsd: n("tradeUsd"), budgetUsd: n("budgetUsd"), maxTradesPerDay: Math.round(n("maxTradesPerDay")),
      takeProfitPct: n("takeProfitPct"), stopLossPct: n("stopLossPct"), maxSlippagePct: n("maxSlippagePct"), lossLimitPct: n("lossLimitPct"),
      durationHours: dur === "custom" ? n("customHours") : +dur, exitAtEnd: f.get("exitAtEnd") === "on",
      name: String(f.get("name") || "").trim(), owner: String(f.get("owner") || "").trim(), payout: String(f.get("payout") || "").trim(),
      style: f.get("style") || "market", limitPct: n("limitPct"), twapMinutes: Math.round(n("twapMinutes")), stopPct: n("stopPct"),
      protect: f.get("protect") || "bracket", trailPct: n("trailPct"),
    };
  };
  const validate = (v) => {
    const e = {};
    const between = (k, lo, hi, msg) => { if (!(Number.isFinite(v[k]) && v[k] >= lo && v[k] <= hi)) e[k] = msg; };
    if (v.strategy === "interval") between("intervalMin", 1, 1440, "1 to 1440 minutes");
    if (v.style === "limit") between("limitPct", 0.1, 10, "0.1% to 10%");
    if (v.style === "twap") between("twapMinutes", 5, 1440, "5 to 1440 minutes");
    if (v.style === "stop") between("stopPct", 0.1, 10, "0.1% to 10%");
    if (v.style === "stop" && v.protect === "trailing") between("trailPct", 0.5, 20, "0.5% to 20%");
    between("tradeUsd", 1, maxTrade, `$1 to ${usd(maxTrade, 0)}`);
    if (!(Number.isFinite(v.budgetUsd) && v.budgetUsd >= (v.tradeUsd || 1) && v.budgetUsd <= 10000)) e.budgetUsd = "At least one trade, at most $10,000";
    between("maxTradesPerDay", 1, maxDay, `1 to ${maxDay}`);
    between("takeProfitPct", minTP, 50, `${minTP}% to 50%`);
    between("stopLossPct", 0.2, maxSL, `0.2% to ${maxSL}%`);
    between("maxSlippagePct", 0.1, maxSlip, `0.1% to ${maxSlip}%`);
    between("lossLimitPct", 0, 100, "0 to 100");
    if (!(Number.isFinite(v.durationHours) && v.durationHours >= 0.25 && v.durationHours <= maxDur)) e.customHours = "15 minutes to 30 days";
    if (!v.name) e.name = "Give it a name";
    if (!asUser) {
      if (!v.owner) e.owner = "Add your name";
      if (!isAddr(v.payout)) e.payout = "A 0x address with 40 hex characters";
    }
    return e;
  };
  const showErrors = (e) => {
    $$("[data-err]", form).forEach((el) => {
      const k = el.dataset.err, on = (submitted || touched.has(k)) && e[k];
      el.textContent = on ? e[k] : "";
      $(`[name="${k}"]`, form)?.setAttribute("aria-invalid", on ? "true" : "false");
    });
  };
  const draw = () => {
    const v = read();
    $("[data-interval]", form).hidden = v.strategy !== "interval";
    $$("[data-ew]", form).forEach((el) => { const w = el.dataset.ew; el.hidden = w === "trail" ? !(v.style === "stop" && v.protect === "trailing") : w !== v.style; });
    $("[data-custom]", form).hidden = new FormData(form).get("dur") !== "custom";
    const q = state.flash?.liveQuote;
    const stopAt = (v.budgetUsd || 0) * ((v.lossLimitPct > 0 ? v.lossLimitPct : 100) / 100);
    const brain = v.strategy === "interval" ? `every ${v.intervalMin || "?"} min` : `when ${stratLabel(v.strategy)} signals`;
    const entryTxt = { market: "at market", limit: `${fixed(v.limitPct, 1)}% below, limit`, twap: `DCA over ${v.twapMinutes || "?"} min`, stop: `on a ${fixed(v.stopPct, 1)}% breakout` }[v.style] || "";
    const lowTP = q && known(q.costPct) && v.takeProfitPct > 0 && v.takeProfitPct <= 2 * q.costPct;
    $("[data-summary]", box).innerHTML = `<p class="eyebrow">Your plan</p>
      <p class="summary-line">Buys <b class="num">${esc(usd(v.tradeUsd))}</b> of <b>${esc(v.token)}</b> ${esc(brain)}.</p>
      <ul class="sum-list num">
        <li><span>Entry</span><b>${esc(styleLabel(v.style))} · ${esc(entryTxt)}</b></li>
        ${v.style === "stop" ? `<li><span>Protection</span><b>${esc(protectShort(v.protect, v.trailPct))}</b></li>` : ""}
        <li><span>Sells at</span><b><span class="up">+${esc(fixed(v.takeProfitPct, 1))}%</span> or <span class="down">−${esc(fixed(v.stopLossPct, 1))}%</span></b></li>
        <li><span>Trades a day</span><b>up to ${esc(Number.isFinite(v.maxTradesPerDay) ? v.maxTradesPerDay : "—")}</b></li>
        <li><span>Stops after losing</span><b>${esc(usd(stopAt))}</b></li>
        <li><span>Runs for</span><b>${esc(durText(v.durationHours))}</b></li>
        <li><span>Max slippage</span><b>${esc(fixed(v.maxSlippagePct, 1))}%</b></li>
        ${q && known(q.costPct) ? `<li><span>Flash cost</span><b>≈${esc(fixed(q.costPct, 2))}% each way</b></li>` : ""}
      </ul>
      ${lowTP ? `<p class="alert small">Take profit is below the round-trip cost (about ${esc(fixed(2 * q.costPct, 1))}%). A "win" could still lose money.</p>` : ""}`;
    $("[data-mini-sum]", page).textContent = `${usd(v.tradeUsd)} ${v.token} · +${fixed(v.takeProfitPct, 1)}% / −${fixed(v.stopLossPct, 1)}% · ${durText(v.durationHours)}`;
    showErrors(validate(v));
  };
  form.addEventListener("input", draw);
  form.addEventListener("change", draw);
  form.addEventListener("focusout", (e) => { if (e.target.name) { touched.add(e.target.name === "dur" ? "customHours" : e.target.name); draw(); } });
  draw();

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    submitted = true;
    const v = read(), e = validate(v);
    showErrors(e);
    if (Object.keys(e).length) {
      $(`[name="${Object.keys(e)[0]}"]`, form)?.focus();
      toast("Check the highlighted fields.", "err");
      return;
    }
    const ok = await confirmDialog("Start this plan?", asUser
      ? `It trades from your arena account, every order with a stop loss. Money can only go back to ${short(wallet.address)}.`
      : `It trades from a new wallet of its own. Withdrawals can only go to ${v.payout}. That can't be changed later.`, "Start plan", { money: true });
    if (!ok) return;
    const btns = $$("[data-submit]", page);
    const dones = btns.map((b) => busy(b, "Starting…"));
    const { durationHours, ...rest } = v;
    const body = { ...rest, durationHours, intervalMin: v.strategy === "interval" ? v.intervalMin : 0,
      limitPct: v.style === "limit" ? v.limitPct : 0, twapMinutes: v.style === "twap" ? v.twapMinutes : 0, stopPct: v.style === "stop" ? v.stopPct : 0,
      protect: v.style === "stop" ? v.protect : "bracket", trailPct: v.style === "stop" && v.protect === "trailing" ? v.trailPct : 0 };
    if (asUser) {
      try {
        const { owner, payout, ...mine } = body;
        void owner; void payout; // the server sets both to the signed-in wallet
        await api("/v1/me/account", { method: "POST", me: true, body: {} }); // no-op when it exists
        const r = await api("/v1/me/plans", { method: "POST", me: true, body: mine });
        toast("Your auto-trader is running on your account.", "ok");
        location.hash = "#/plan/" + enc(r.plan?.id || "");
      } catch (err) { toast(err.message, "err"); dones.forEach((d) => d()); }
      return;
    }
    try {
      const r = await api("/v1/admin/plans", { method: "POST", admin: true, body });
      const p = r.plan || {};
      if (mode === "live") {
        box.innerHTML = `<div class="panel fund-box" data-reveal>
          <p class="eyebrow">Plan created · fund its wallet</p>
          <p class="fund-addr mono">${esc(p.address || "")}</p>
          <div class="btn-row"><button type="button" class="btn btn-primary btn-sm" data-copy-fund>Copy address</button>${isAddr(p.address) ? `<a class="btn btn-ghost btn-sm" href="https://basescan.org/address/${esc(p.address)}" ${EXT}>Basescan</a>` : ""}<a class="btn btn-ghost btn-sm" href="#/plan/${enc(p.id || "")}">Open plan</a></div>
          <p class="small">${esc(r.fund || "")}</p>
          <p class="alert">Send USDC on the Base network only. Tokens sent on another network are lost.</p></div>`;
        $("[data-copy-fund]", box)?.addEventListener("click", () => copyText(p.address || ""));
        $(".mobile-cta", page)?.remove();
        return;
      }
      toast(mode === "paper" ? "Plan started. Its budget is funded with paper USDC." : "Plan started.", "ok");
      location.hash = "#/plan/" + enc(p.id);
    } catch (err) {
      toast(err.message, "err");
      dones.forEach((d) => d());
    }
  });
  return {};
}

async function pagePlan(page, id) {
  page.innerHTML = `<a class="back" href="#/plans">Auto-trade</a><div data-body><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const body = $("[data-body]", page);
  let d, sig = "";
  try { d = await api("/v1/plans/" + enc(id)); } catch (err) {
    body.innerHTML = err.status === 404 ? emptyState("Plan not found", "It may belong to another server.", { href: "#/plans", label: "All plans" }) : errorState(err);
    return {};
  }
  const refresh = async () => { try { d = await api("/v1/plans/" + enc(id)); draw(); } catch { /* keep last */ } };
  const draw = () => { const s = JSON.stringify(d); if (s === sig) return; sig = s; renderPlan(body, d, refresh); };
  draw();
  const t = setInterval(() => { if (document.visibilityState === "visible") refresh(); }, 10000);
  return { cleanup: () => clearInterval(t), onPost: (p) => { if (d?.plan?.name && String(p.text || "").includes(`"${d.plan.name}"`)) refresh(); } };
}

function renderPlan(body, d, refresh) {
  const p = d.plan, s = p.stats || {};
  const live = p.status === "active", over = p.status === "ended" || p.status === "stopped";
  const lossCap = (+p.budgetUsd || 0) * ((+p.lossLimitPct > 0 ? +p.lossLimitPct : 100) / 100);
  const lost = Math.max(0, -(+s.pnlUsd || 0));
  const lossShare = lossCap > 0 ? Math.min(100, (lost / lossCap) * 100) : 0;
  const start = new Date(p.startsAt).getTime(), end = new Date(p.endsAt).getTime();
  const timeShare = end > start ? Math.min(100, Math.max(0, ((Date.now() - start) / (end - start)) * 100)) : 100;
  const op = isOperator(), mode = modeOf();
  // The owner (signed in with the wallet whose account runs the plan) can manage it too.
  const mine = !op && !!meToken() && !!wallet.address && ownAccountIds(wallet.address).includes(p.walletId);
  document.title = `${p.name} · Agent Arena`;
  body.innerHTML = `
    <header class="plan-head">
      <div class="plan-head-main"><p class="eyebrow"><span class="nocaps">${esc(p.owner)}</span> · ${esc(p.token)} · ${esc(stratLabel(p.strategy))} · ${esc(styleLabel(p.style))}</p>
        <div class="trade-title"><h1>${esc(p.name)}</h1>${planChip(p.status)}</div>
        ${p.statusReason && !live ? `<p class="muted small status-why">${esc(cap(p.statusReason))}</p>` : ""}</div>
      <div class="btn-row plan-actions">${(op || mine) && !over ? `
        <button class="btn btn-primary btn-sm" data-pa="check" ${live ? "" : "disabled"}>Check now</button>
        <button class="btn btn-ghost btn-sm" data-pa="${live ? "pause" : "resume"}">${live ? "Pause" : "Resume"}</button>
        <button class="btn btn-danger btn-sm" data-pa="stop">Stop</button>` : ""}<button class="btn btn-ghost btn-sm" data-pshare>Share</button></div>
    </header>
    <section class="now${live ? " is-live" : ""}">
      <p class="eyebrow">${live ? "Right now" : "Last update"}</p>
      <p class="now-line">${live ? '<span class="think-dot" aria-hidden="true"></span>' : ""}<span>${esc(p.lastSignal ? cap(p.lastSignal) : live ? "Starting up" : "—")}</span></p>
      <p class="muted small num">${p.lastSignalAt ? esc(ago(p.lastSignalAt)) : ""}${p.lastTradeAt ? ` · last entry ${esc(ago(p.lastTradeAt))}` : ""}</p>
    </section>
    <dl class="statline plan-stats">
      <div class="stat"><dt>P&amp;L</dt><dd class="num">${pnlHTML(s.pnlUsd)}</dd></div>
      <div class="stat"><dt>Trades</dt><dd class="num">${esc(s.trades ?? 0)}</dd></div>
      <div class="stat"><dt>Won</dt><dd class="num">${s.closed ? `${esc(s.wins)} of ${esc(s.closed)}` : "—"}</dd></div>
      <div class="stat"><dt>Today</dt><dd class="num">${esc(s.today ?? 0)} / ${esc(p.maxTradesPerDay)}</dd></div>
      <div class="stat"><dt>USDC</dt><dd class="num">${esc(usd(p.usdc))}</dd></div>
      <div class="stat"><dt>${esc(p.token)}</dt><dd class="num">${esc(amount(p.holding))}</dd></div>
    </dl>
    <div class="meters">
      <div class="meter"><div class="meter-head"><span>Loss limit</span><span class="num">${esc(usd(lost))} of ${esc(usd(lossCap))}</span></div><div class="mbar mbar-loss" data-w="${lossShare.toFixed(1)}"><i></i></div></div>
      <div class="meter"><div class="meter-head"><span>Time</span><span class="num">${esc(over ? "Finished" : timeLeft(p.endsAt))}</span></div><div class="mbar" data-w="${timeShare.toFixed(1)}"><i></i></div></div>
    </div>
    ${mode === "live" && !(+p.usdc > 0) && !over ? `<p class="alert">This plan's wallet has no USDC yet. Send USDC on Base to ${esc(p.address || "its address")}, plus a little ETH for approvals.</p>` : ""}
    <div class="two-col">
      <div>
        <div class="section-head"><h2>Trades</h2><span class="muted small num">${esc(d.trades?.length || 0)}</span></div>
        ${d.trades?.length ? `<ul class="trades">${d.trades.map(tradeRow).join("")}</ul>` : emptyState("No trades yet", live ? "It enters when its strategy fires. The line above shows what it's waiting for." : "This plan didn't place a trade.")}
      </div>
      <aside class="panel">
        <p class="eyebrow">Settings</p>
        <dl class="spec spec-tight">
          <div><dt>Coin</dt><dd>${esc(p.token)}${+p.price > 0 ? ` · ${esc(price(p.price))}` : ""}</dd></div>
          <div><dt>Brain</dt><dd>${esc(stratLabel(p.strategy))}${p.strategy === "interval" ? ` · every ${esc(p.intervalMin)} min` : ""}</dd></div>
          <div><dt>Entry</dt><dd>${esc(styleLabel(p.style))}${p.style === "limit" ? ` · ${esc(p.limitPct)}% below` : p.style === "twap" ? ` · ${esc(p.twapMinutes)} min` : p.style === "stop" ? ` · ${esc(p.stopPct)}% above` : ""}${p.style === "stop" ? ` · ${esc(protectShort(p.protect, p.trailPct).toLowerCase())}` : ""}</dd></div>
          <div><dt>Per trade</dt><dd>${esc(usd(p.tradeUsd))}</dd></div>
          <div><dt>Budget</dt><dd>${esc(usd(p.budgetUsd))}</dd></div>
          <div><dt>Take profit</dt><dd class="up">+${esc(p.takeProfitPct)}%</dd></div>
          <div><dt>Stop loss</dt><dd class="down">−${esc(p.stopLossPct)}%</dd></div>
          <div><dt>Max slippage</dt><dd>${esc(p.maxSlippagePct)}%</dd></div>
          <div><dt>Sell at end</dt><dd>${p.exitAtEnd ? "Yes" : "No"}</dd></div>
          <div><dt>Runs</dt><dd>${esc(fmtDate(p.startsAt))} → ${esc(fmtDate(p.endsAt))}</dd></div>
          <div><dt>Wallet</dt><dd>${addrLink(p.address)}</dd></div>
        </dl>
        ${isAddr(p.address) ? '<button type="button" class="btn-link copy-line" data-copy-plan>Copy wallet address</button>' : ""}
        <p class="hint plan-brain">${esc(d.strategy || STRAT[p.strategy]?.line || "")}</p>
      </aside>
    </div>`;
  $$(".mbar", body).forEach((m) => $("i", m).style.setProperty("width", m.dataset.w + "%"));
  $("[data-copy-plan]", body)?.addEventListener("click", () => copyText(p.address));
  $("[data-pshare]", body)?.addEventListener("click", () => openShare({
    url: shareURL("plan", p.id), text: `${p.owner}'s auto-trader "${p.name}": ${p.token}, ${stratLabel(p.strategy)}, ${styleLabel(p.style)} on Flash · P&L ${usd(s.pnlUsd)}`, file: `arena-plan-${p.id}.png`,
    card: () => drawCard({ title: `${p.owner}'s ${p.name}`, big: usd(s.pnlUsd), down: +s.pnlUsd < 0, line: `${p.token} · ${stratLabel(p.strategy)} · ${styleLabel(p.style)} · ${plural(s.trades, "trade")}` }),
  }));
  $$("[data-pa]", body).forEach((b) => b.addEventListener("click", async () => {
    const act = b.dataset.pa;
    if (act === "stop" && !(await confirmDialog("Stop this plan?", "No new trades. An open trade keeps its take profit and stop loss.", "Stop plan"))) return;
    const done = busy(b, { check: "Checking…", pause: "Pausing…", resume: "Resuming…", stop: "Stopping…" }[act]);
    try {
      const r = mine ? await api(`/v1/me/plans/${enc(p.id)}/${act}`, { method: "POST", me: true })
        : await api(`/v1/admin/plans/${enc(p.id)}/${act}`, { method: "POST", admin: true });
      const msg = act === "check" ? cap(r?.plan?.lastSignal || "Checked") : { pause: "Plan paused", resume: "Plan resumed", stop: "Plan stopped" }[act];
      toast(msg, "ok");
    } catch (err) { toast(err.message, "err"); }
    await refresh();
    done();
  }));
}

/* ---------------- wallets ---------------- */
function signInPrompt(what, note = "Needs the operator token. It stays in this tab only.") {
  return `<div class="empty" data-reveal><svg class="empty-ico" viewBox="0 0 24 24" aria-hidden="true"><rect x="5" y="10.5" width="14" height="9.5" rx="2.5"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/></svg><h3>Sign in to ${esc(what)}</h3><p>${esc(note)}</p><button type="button" class="btn btn-primary" data-signin-open>Sign in</button></div>`;
}

const WALLET_NOTE = {
  paper: "Simulated balances, real Flash quotes.",
  dry: "Signed, not sent. Balances don't change.",
  live: "Real funds on Base.",
};

function walletCard(w, mode, plan) {
  const p = w.profile || {}, f = (w.follows || [])[0];
  const name = w.agentName || p.owner || (w.role === "self" ? short(w.address) : w.id);
  const who = w.role === "agent" ? "Agent" : w.role === "self" ? "Own wallet" : "Person";
  if (w.role === "self") {
    return `<article class="wcard" data-w="${esc(w.id)}">
      <header class="wcard-head"><div class="wcard-name"><p class="eyebrow">${esc(who)}</p><h3>${esc(name)}</h3><p class="small muted">Your keys, your signature</p></div>
        <div class="wcard-tags">${w.openTrade ? `<a class="chip chip-placed" href="#/trade/${enc(w.openTrade.id)}">Open trade</a>` : ""}</div></header>
      <div class="addr">${addrLink(w.address)}</div>
      <dl class="wstats"><div><dt>USDC</dt><dd class="num">${esc(fixed(w.usdc, 2))}</dd></div><div><dt>Equity</dt><dd class="num">${esc(usd(w.equityUsd))}</dd></div><div><dt>P&amp;L</dt><dd class="num">${pnlHTML(w.pnlUsd)}</dd></div></dl>
      <div class="btn-row wcard-actions"><button type="button" class="btn btn-sm btn-ghost" data-act="orders">Flash orders</button></div></article>`;
  }
  const link = f ? `${f.mode === "invest" ? "Invests with" : "Copies"} <a class="link" href="#/agent/${enc(f.agentId)}">${esc(agentName(f.agentId))}</a>`
    : plan ? `Runs the plan <a class="link" href="#/plan/${enc(plan.id)}">${esc(plan.name)}</a>`
    : w.role === "agent" ? "The agent's own wallet" : "Not linked yet";
  const limits = f
    ? `${usd(w.spentToday)} of ${usd(f.dailyCapUsd)} today · max ${usd(f.maxPerTradeUsd)} a trade${f.allocationPct ? ` · ${f.allocationPct}% per trade` : ""}`
    : `Max ${usd(w.limits?.maxTradeUsd)} a trade · ${w.limits?.maxTradesPerDay ?? "—"} trades a day`;
  const payout = p.payout
    ? `Locked to ${addrLink(p.payout)}`
    : `<span class="warn">Not set.</span> On the server: <code>arena payout -wallet ${esc(w.id)} -to 0x…</code>`;
  const dd = known(w.drawdownPct) && +w.drawdownPct > 0 ? `-${fixed(w.drawdownPct, 1)}%` : "0%";
  const test = usd(state.flash?.testTradeUsd ?? 1);
  return `<article class="wcard${p.paused ? " is-paused" : ""}" data-w="${esc(w.id)}">
    <header class="wcard-head">
      <div class="wcard-name"><p class="eyebrow">${esc(who)} · ${esc(w.id)}</p><h3>${esc(name)}</h3><p class="small muted">${link}</p></div>
      <div class="wcard-tags">${p.paused ? '<span class="chip chip-pending">Paused</span>' : '<span class="chip chip-filled">Active</span>'}${w.openTrade ? `<a class="chip chip-placed" href="#/trade/${enc(w.openTrade.id)}">Open trade</a>` : ""}</div>
    </header>
    <div class="addr">${addrLink(w.address)}<button type="button" class="btn-link" data-copy-addr="${esc(w.address)}">Copy address</button></div>
    <dl class="wstats">
      <div><dt>USDC</dt><dd class="num">${esc(fixed(w.usdc, 2))}</dd></div>
      <div><dt>WETH</dt><dd class="num">${esc(fixed(w.weth, 6))}</dd></div>
      <div><dt>Equity</dt><dd class="num">${esc(usd(w.equityUsd))}</dd></div>
      <div><dt>P&amp;L</dt><dd class="num">${pnlHTML(w.pnlUsd)}</dd></div>
      <div><dt>Return</dt><dd class="num ${signClass(w.returnPct)}">${esc(pct(w.returnPct))}</dd></div>
      <div><dt>From peak</dt><dd class="num">${esc(dd)}</dd></div>
    </dl>
    <dl class="spec spec-tight">
      <div><dt>Limits</dt><dd>${esc(limits)}</dd></div>
      <div><dt>Loss limit</dt><dd>${p.lossLimitPct ? `Pauses at -${esc(p.lossLimitPct)}% from its peak` : "None"}</dd></div>
      ${p.paused && p.pausedReason ? `<div><dt>Why paused</dt><dd class="warn">${esc(p.pausedReason)}</dd></div>` : ""}
      <div><dt>Withdrawals</dt><dd>${payout}</dd></div>
      <div><dt>Trades</dt><dd class="num">${esc(w.trades ?? 0)}</dd></div>
      ${w.error ? `<div><dt>Balance</dt><dd class="down">${esc(w.error)}</dd></div>` : ""}
    </dl>
    <div class="btn-row wcard-actions">
      <button type="button" class="btn btn-sm btn-primary" data-act="order">New order</button>
      <button type="button" class="btn btn-sm btn-ghost" data-act="withdraw" ${p.payout ? "" : 'disabled title="Set a payout address first"'}>Withdraw</button>
      <button type="button" class="btn btn-sm btn-plain" data-act="more" aria-haspopup="dialog" aria-label="More actions for ${esc(name)}">More</button>
    </div>
  </article>`;
}

function ordersTable(orders) {
  if (!orders?.length) return `<p class="muted small">No Flash orders for this wallet yet.</p>`;
  return `<div class="table-wrap"><table class="table table-plain table-sm"><thead><tr><th>Placed</th><th>Order</th><th>Status</th><th class="r">Filled</th><th class="r hide-sm">Price</th></tr></thead><tbody>
    ${orders.map((o) => `<tr>
      <td class="num">${esc(o.PlacedAt ? ago(o.PlacedAt) : "—")}</td>
      <td>${esc(cap(o.Side || ""))} ${esc(o.Type || "")}${o.Bracket ? `<div class="muted small">bracket ${esc(cap(o.Bracket))}</div>` : ""}</td>
      <td>${esc(cap((o.Status || "").toLowerCase()))}${o.Reason && o.Reason !== "REASON_FULLY_FILLED" ? `<div class="muted small">${esc(reasonText(o.Reason))}</div>` : ""}</td>
      <td class="r num">${+o.Target > 0 ? esc(amount(o.Target)) : "—"}${+o.Contra > 0 ? `<div class="muted small">${esc(usd(o.Contra))}</div>` : ""}</td>
      <td class="r num hide-sm">${esc(price(o.Price))}</td></tr>`).join("")}
  </tbody></table></div>`;
}

function detailsBlock(w, mode) {
  if (mode === "paper") return `<p class="muted small">Paper mode: no gas and no onchain allowances are used.</p>`;
  const gas = known(w.eth) ? `${fixed(w.eth, 6)} ETH` : "—";
  const u = known(w.usdcAllowance) ? usd(w.usdcAllowance) + " USDC" : "—";
  const e = known(w.wethAllowance) ? fixed(w.wethAllowance, 6) + " WETH" : "—";
  return `<dl class="spec spec-tight">
    <div><dt>Gas</dt><dd class="${w.flags?.needsGas ? "warn" : ""}">${esc(gas)}</dd></div>
    <div><dt>Flash may pull</dt><dd>${esc(u)} / ${esc(e)}</dd></div>
  </dl>${w.flags?.needsGas ? `<p class="alert">This wallet needs a little ETH on Base (about $0.30) to pay for token approvals.</p>` : ""}`;
}

async function pageWallets(page) {
  page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Sub-wallets</p><h1 data-reveal>Wallets</h1>
    <p class="lede" data-reveal>One wallet each. Withdrawals only to a locked address.</p></div>
    <button type="button" class="btn btn-ghost btn-sm" data-refresh hidden>Refresh</button></header>
    <div class="section-tight" data-wbody></div>`;
  const body = $("[data-wbody]", page);
  if (!isOperator()) { body.innerHTML = signInPrompt("manage wallets"); return {}; }
  body.innerHTML = `<div class="skel skel-row"></div><div class="skel skel-row"></div>`;
  if (!state.board) await loadBoard().catch(() => {});
  let data = null, plansBy = {}, tokens = [];

  const load = async () => {
    try {
      const [wd, pd, td] = await Promise.all([api("/v1/admin/wallets", { admin: true }), api("/v1/plans").catch(() => ({ plans: [] })), api("/v1/tokens").catch(() => ({ tokens: [] }))]);
      data = wd;
      tokens = (td.tokens || []).filter((t) => t.symbol);
      plansBy = Object.fromEntries((pd.plans || []).map((p) => [p.walletId, p]));
    } catch (err) { body.innerHTML = errorState(err); return; }
    render();
  };
  const render = () => {
    const mode = data.mode || modeOf();
    const ws = data.wallets || [];
    const people = ws.filter((w) => w.role !== "agent"), agents = ws.filter((w) => w.role === "agent");
    const section = (title, list, empty) => `<div class="section-head section"><h2>${esc(title)}</h2><span class="muted small num">${list.length}</span></div>
      ${list.length ? `<div class="wgrid">${list.map((w) => walletCard(w, mode, plansBy[w.id])).join("")}</div>` : empty}`;
    body.innerHTML = `<p class="mode-note">${mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode]?.badge || mode)}</span>` : ""}<span>${esc(WALLET_NOTE[mode] || "")}</span>${infoBtn("wallets")}</p>
      ${section("People", people, emptyState("No personal wallets yet", "Create an auto-trade plan, or use Invest on an agent page.", { href: "#/plans/new", label: "Create a plan" }))}
      ${section("Agents", agents, emptyState("No agent wallets", "Run arena keygen and restart the server."))}`;
    $("[data-refresh]", page).hidden = false;
    wire();
  };

  const post = async (path, bodyObj) => api(path, { method: "POST", admin: true, body: bodyObj || {} });
  const wire = () => {
    const mode = data.mode || modeOf();
    $$("[data-copy-addr]", body).forEach((b) => b.addEventListener("click", () => copyText(b.dataset.copyAddr)));
    $$(".wcard", body).forEach((card) => {
      const id = card.dataset.w;
      const w = (data.wallets || []).find((x) => x.id === id);
      const name = w?.agentName || w?.profile?.owner || id;
      // One handler for the card buttons and the "More" action sheet.
      const run = async (act, btn) => {
        let done = () => {};
        try {
          if (act === "test") { await runTestTrade(id, btn); return; }
          if (act === "order") {
            if (!tokens.length) { toast("No coin has a live price yet. Try again in a minute.", "err"); return; }
            if (w?.profile?.paused) { toast(CODE_MSG.WALLET_PAUSED, "err"); return; }
            const sb = openSheet(`New order · ${name}`, { wide: true });
            sb.innerHTML = ticketHTML([w], id, tokens);
            wireTicket($("[data-ticket]", sb));
            return;
          }
          if (act === "more") {
            const items = [
              ["test", `Run ${usd(state.flash?.testTradeUsd ?? 1)} test trade`],
              [w?.profile?.paused ? "resume" : "pause", w?.profile?.paused ? "Resume wallet" : "Pause wallet"],
              mode === "paper" ? ["topup", "Add $25 paper USDC"] : null,
              ["details", "Gas and allowances"],
              ["orders", "Flash orders"],
              mode === "live" ? ["revoke", "Revoke Flash allowance", "is-danger"] : null,
            ].filter(Boolean);
            const sb = openSheet(name);
            sb.innerHTML = `<div class="action-list" role="menu">${items.map(([a, l, c]) => `<button type="button" role="menuitem" class="action-item ${c || ""}" data-a="${esc(a)}">${esc(l)}</button>`).join("")}</div>`;
            $$("[data-a]", sb).forEach((b) => b.addEventListener("click", () => {
              const a = b.dataset.a;
              if (a !== "details" && a !== "orders") sheetClose(sheet);
              run(a, null);
            }));
            return;
          }
          if (act === "pause" || act === "resume") {
            done = busy(btn, act === "pause" ? "Pausing…" : "Resuming…");
            await post(`/v1/admin/wallets/${enc(id)}/${act}`);
            toast(act === "pause" ? `${name} paused` : `${name} resumed`, "ok");
            await load(); return;
          }
          if (act === "topup") {
            done = busy(btn, "Adding…");
            await post(`/v1/admin/wallets/${enc(id)}/topup`, { usd: 25 });
            toast(`Added $25 of paper USDC to ${name}`, "ok");
            await load(); return;
          }
          if (act === "revoke") {
            if (!(await confirmDialog("Revoke Flash's allowance?", `Sets the USDC and WETH allowance of ${name} to zero, onchain. Costs a little ETH for gas. Future trades approve again, capped.`, "Revoke", { money: true }))) return;
            done = busy(btn, "Revoking…");
            const r = await post(`/v1/admin/wallets/${enc(id)}/revoke`);
            toast(r?.txs?.length ? `Revoked (${r.txs.length} transaction${r.txs.length === 1 ? "" : "s"})` : "Nothing to revoke", "ok");
            done(); return;
          }
          if (act === "withdraw") {
            if (!w?.profile?.payout) { toast("Set a payout address first: arena payout on the server.", "err"); return; }
            const usdc = num(w?.usdc), weth = num(w?.weth);
            const sb = openSheet("Withdraw");
            sb.innerHTML = `<form class="withdraw" data-withdraw>
                <p class="chips"><span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode]?.badge || mode)}</span><span class="mini">Locked payout</span></p>
                <dl class="spec spec-tight"><div><dt>From</dt><dd>${esc(name)}</dd></div><div><dt>To</dt><dd class="mono">${addrLink(w.profile.payout)}</dd></div></dl>
                <div class="seg seg-full" role="radiogroup" aria-label="Token"><label><input type="radio" name="token" value="USDC" ${usdc > 0 || !(weth > 0) ? "checked" : ""}><span>USDC · ${esc(fixed(usdc, 2))}</span></label><label><input type="radio" name="token" value="WETH" ${!(usdc > 0) && weth > 0 ? "checked" : ""}><span>WETH · ${esc(fixed(weth, 6))}</span></label></div>
                <p class="ticket-preview num" data-wd-sum aria-live="polite"></p>
                <div class="btn-row sheet-cta"><button type="submit" class="btn btn-primary btn-block">Withdraw all</button></div></form>`;
            const wf = $("[data-withdraw]", sb);
            const sum = () => {
              const token = new FormData(wf).get("token"), amt = token === "WETH" ? weth : usdc;
              $("[data-wd-sum]", wf).textContent = amt > 0 ? `Sends ${token === "WETH" ? fixed(amt, 6) : fixed(amt, 2)} ${token} to ${short(w.profile.payout)}.` : `This wallet has no ${token} to withdraw.`;
              $("button[type=submit]", wf).disabled = !(amt > 0);
            };
            wf.addEventListener("change", sum); sum();
            wf.addEventListener("submit", async (e) => {
              e.preventDefault();
              const token = new FormData(wf).get("token");
              const sub = $("button[type=submit]", wf);
              const real = mode === "live" ? " This moves real funds on Base and needs a little ETH for gas." : mode === "paper" ? " Paper money: the balance is simulated." : "";
              if (!(await confirmDialog(`Withdraw all ${token}?`, `Sends all of ${name}'s ${token} to ${w.profile.payout}.${real}`, "Withdraw", { money: true }))) return;
              const fin = busy(sub, "Sending…");
              try {
                const r = await post(`/v1/admin/wallets/${enc(id)}/withdraw`, { token, amount: 0 });
                toast(`Sent ${token === "WETH" ? fixed(r.amount, 6) : fixed(r.amount, 2)} ${token} to ${short(r.to)}`, "ok");
                sheetClose(sheet);
                await load();
              } catch (err) { toast(err.message, "err"); fin(); }
            });
            return;
          }
          if (act === "details" || act === "orders") {
            const sb = openSheet(act === "details" ? `Gas and allowances · ${name}` : `Flash orders · ${name}`, { wide: act === "orders" });
            sb.innerHTML = `<div class="skel skel-row"></div><div class="skel skel-row"></div>`;
            try {
              if (act === "details") {
                const r = await api(`/v1/admin/wallets/${enc(id)}`, { admin: true });
                if (sb.isConnected) sb.innerHTML = detailsBlock(r.wallet || {}, mode);
              } else {
                const r = await api(`/v1/admin/wallets/${enc(id)}/orders`, { admin: true });
                if (sb.isConnected) sb.innerHTML = `${r.note ? `<p class="alert">${esc(r.note)}</p>` : ""}${ordersTable(r.orders)}`;
              }
            } catch (err) { if (sb.isConnected) sb.innerHTML = errorState(err); }
          }
        } catch (err) {
          toast(err.message, "err");
          done();
        }
      };
      $$("[data-act]", card).forEach((btn) => btn.addEventListener("click", () => run(btn.dataset.act, btn)));
    });
  };

  $("[data-refresh]", page).addEventListener("click", load);
  await load();
  return {};
}

function installBlock() {
  const standalone = matchMedia("(display-mode: standalone)").matches || navigator.standalone;
  if (standalone) return "";
  const ios = /iphone|ipad|ipod/i.test(navigator.userAgent);
  return `<div class="section-head section"><h2>Install the app</h2></div><div class="panel panel-quiet"><p class="muted">${ios ? "On iPhone: tap Share, then Add to Home Screen." : "Get trade alerts and open the arena from your home screen."}</p>${ios ? "" : `<div class="btn-row"><button class="btn btn-ghost btn-sm" data-install ${state.installPrompt ? "" : "disabled"}>Install Agent Arena</button></div>`}</div>`;
}
function wireInstall(scope) {
  $("[data-install]", scope)?.addEventListener("click", async () => {
    if (!state.installPrompt) return;
    state.installPrompt.prompt();
    await state.installPrompt.userChoice.catch(() => {});
    state.installPrompt = null;
    route();
  });
}

function pageNotFound(page) {
  page.innerHTML = `<h1 class="sr-only">Page not found</h1>` + emptyState("This page doesn't exist", "The link may be old or mistyped.", { href: "#/", label: "Go to the arena" });
  return {};
}

/* ---------------- your own wallet (EIP-1193, non-custodial) ---------------- */
// Any injected or embedded wallet: MetaMask, Coinbase Wallet, Rabby... The
// arena never sees a key: it returns Flash's order to sign, the wallet signs.
const BASE_CHAIN = { chainId: "0x2105", chainName: "Base", rpcUrls: ["https://mainnet.base.org"], nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 }, blockExplorerUrls: ["https://basescan.org"] };
const wallet = { address: (() => { const a = store.get("arena.wallet", ""); return isAddr(a) ? a : ""; })() };
const eth = () => window.ethereum;

function setWallet(addr) {
  wallet.address = isAddr(addr) ? addr : "";
  if (wallet.address) store.set("arena.wallet", wallet.address); else store.del("arena.wallet");
  updateWalletChip();
  current?.onWallet?.();
}
function updateWalletChip() {
  const b = $("[data-wallet-btn]");
  if (!b) return;
  $("[data-wallet-text]", b).textContent = wallet.address ? short(wallet.address) : "Connect";
  b.dataset.state = wallet.address ? "on" : "";
  b.setAttribute("aria-label", wallet.address ? `Your wallet ${wallet.address}` : "Connect your wallet");
}
async function ensureBase() {
  const p = eth();
  const id = await p.request({ method: "eth_chainId" }).catch(() => "");
  if (String(id).toLowerCase() === BASE_CHAIN.chainId) return;
  try { await p.request({ method: "wallet_switchEthereumChain", params: [{ chainId: BASE_CHAIN.chainId }] }); }
  catch (e) {
    if (e?.code === 4902) await p.request({ method: "wallet_addEthereumChain", params: [BASE_CHAIN] });
    else throw e;
  }
}
const walletErr = (e) => (e?.code === 4001 ? "Cancelled in your wallet." : e?.code === -32002 ? "Your wallet already has a request open. Open it to continue." : e?.message || "Wallet error");
async function connectWallet() {
  if (!eth()) {
    const sb = openSheet("Connect a wallet");
    sb.innerHTML = `<p class="sheet-text">No wallet in this browser yet. Install one, then reload this page.</p>
      <div class="sheet-grid"><a class="sheet-btn" href="https://metamask.io/download/" ${EXT}>MetaMask</a><a class="sheet-btn" href="https://www.coinbase.com/wallet" ${EXT}>Coinbase Wallet</a><a class="sheet-btn" href="https://rabby.io/" ${EXT}>Rabby</a></div>`;
    return "";
  }
  try {
    const accts = await eth().request({ method: "eth_requestAccounts" });
    setWallet(accts?.[0] || "");
    if (!wallet.address) return "";
    try { await ensureBase(); }
    catch (e) { toast(e?.code === 4001 ? "Connected. Switch your wallet to Base before you trade." : walletErr(e), "err"); return wallet.address; }
    toast(`Connected ${short(wallet.address)}`, "ok");
    return wallet.address;
  } catch (e) { toast(walletErr(e), "err"); return ""; }
}
async function ensureWallet() {
  if (wallet.address && eth()) {
    const accts = await eth().request({ method: "eth_accounts" }).catch(() => []);
    if (accts?.some((a) => a.toLowerCase() === wallet.address.toLowerCase())) return wallet.address;
  }
  return connectWallet();
}
eth()?.on?.("accountsChanged", (a) => {
  const next = a?.[0] || "";
  if (wallet.address && next.toLowerCase() !== wallet.address.toLowerCase()) toast(next ? `Wallet switched to ${short(next)}` : "Wallet disconnected", "ok");
  if (sheet.open && $("[data-wt]", sheet)) sheetClose(sheet); // a quote belongs to one address
  setWallet(next);
});
eth()?.on?.("chainChanged", (id) => {
  if (wallet.address && String(id).toLowerCase() !== BASE_CHAIN.chainId) toast("Your wallet left Base. Switch back before you trade.", "err");
  current?.onWallet?.();
});
$("[data-wallet-btn]")?.addEventListener("click", () => { if (wallet.address) location.hash = "#/me"; else connectWallet(); });
updateWalletChip();
// The saved address is only a hint: show it only while the wallet still has
// it connected (a locked or disconnected wallet shows "Connect" instead).
(async () => {
  if (!wallet.address) return;
  if (!eth()) { setWallet(""); return; }
  const accts = await eth().request({ method: "eth_accounts" }).catch(() => null);
  if (accts && !accts.some((a) => a.toLowerCase() === wallet.address.toLowerCase())) setWallet(accts[0] || "");
})();
try { sessionStorage.removeItem("arena.me"); } catch { /* old per-tab session */ }

async function waitReceipt(hash) {
  for (let i = 0; i < 90; i++) {
    const r = await eth().request({ method: "eth_getTransactionReceipt", params: [hash] }).catch(() => null);
    if (r) { if (r.status === "0x1") return r; throw new Error("The approval transaction failed onchain."); }
    await new Promise((res) => setTimeout(res, 2000));
  }
  throw new Error("The approval is taking long. Check your wallet, then try again.");
}

// walletTrade: quote -> confirm -> approvals -> sign entry + bracket -> submit.
async function walletTrade(opts = {}) {
  const addr = await ensureWallet();
  if (!addr) return;
  const [meta, bal] = await Promise.all([
    api("/v1/tokens").catch(() => null),
    api("/v1/wallet/" + enc(addr)).catch(() => null),
  ]);
  const tokens = (meta?.tokens || []).filter((t) => t.symbol);
  const L = meta?.limits || {};
  const maxTrade = +(L.maxTradeUsd ?? 5);
  const usdcBal = num(bal?.usdc);
  const copy = !!opts.copyOf;
  const mode = meta?.mode || modeOf();
  const modeChip = mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode]?.badge || mode)}</span>` : "";
  let timer = 0;
  const stop = () => { clearInterval(timer); timer = 0; };
  sheet.addEventListener("sheet:closed", stop, { once: true });
  sheet.className = "sheet sheet-wide";
  delete sheet.dataset.kind;
  const head = (t) => `<div class="sheet-head"><h2 id="sheet-title">${esc(t)}</h2><button type="button" class="sheet-x" data-sheet-close aria-label="Close">×</button></div>`;
  const form = async () => {
    const chain = await eth().request({ method: "eth_chainId" }).catch(() => "");
    const wrongNet = chain && String(chain).toLowerCase() !== BASE_CHAIN.chainId;
    sheet.innerHTML = `${head(copy ? `Copy ${opts.from || "this trade"}` : "Trade from your wallet")}
      <p class="mode-note">${modeChip}<span class="mini num">${esc(short(addr))}</span>${known(usdcBal) ? `<span class="mini num">${esc(usd(usdcBal))} USDC</span>` : ""}</p>
      ${wrongNet ? `<p class="alert">Your wallet is on another network. The arena trades on Base. <button type="button" class="btn btn-sm btn-ghost" data-switch>Switch to Base</button></p>` : ""}
      <form class="wt-form" data-wt novalidate>
        <div class="form-grid form-2">
          <div class="field"><label for="wt-token">Coin</label><select id="wt-token" name="token" ${copy ? "disabled" : ""}>${(tokens.length ? tokens : [{ symbol: opts.token || "WETH" }]).map((t) => `<option value="${esc(t.symbol)}" ${t.symbol === (opts.token || "WETH") ? "selected" : ""}>${esc(t.symbol)}${known(t.price) ? " · " + esc(price(t.price)) : ""}</option>`).join("")}</select></div>
          <div class="field"><label for="wt-size">Size ($)</label><input id="wt-size" name="sizeUsd" type="number" min="1" max="${maxTrade}" step="0.5" value="${Math.min(10, maxTrade)}" inputmode="decimal" required><span class="hint">$1 to ${esc(usd(maxTrade, 0))}</span></div>
        </div>
        ${copy ? `<p class="chips field-gap"><span class="mini">Same coin, TP and SL as the signal</span></p>` : `
        <div class="field field-gap"><span class="label">Order</span><div class="seg" role="radiogroup" aria-label="Order style">
          ${["market", "limit", "twap"].map((s, i) => `<label><input type="radio" name="style" value="${s}" ${i ? "" : "checked"}><span>${esc(STYLE[s])}</span></label>`).join("")}</div></div>
        <div class="form-grid form-3 field-gap">
          <div class="field"><label for="wt-tp">Take profit (%)</label><input id="wt-tp" name="takeProfitPct" type="number" min="0.6" max="50" step="0.1" value="2" inputmode="decimal" required><span class="hint">Sells when price rises this much</span></div>
          <div class="field"><label for="wt-sl">Stop loss (%)</label><input id="wt-sl" name="stopLossPct" type="number" min="0.2" max="5" step="0.1" value="1" inputmode="decimal" required><span class="hint">Sells when price falls this much</span></div>
          <div class="field" data-when="limit" hidden><label for="wt-lim">Limit below (%)</label><input id="wt-lim" name="limitPct" type="number" min="0.1" max="10" step="0.1" value="0.5" inputmode="decimal"><span class="hint">Buys only after this dip</span></div>
          <div class="field" data-when="twap" hidden><label for="wt-tw">DCA over (min)</label><input id="wt-tw" name="twapMinutes" type="number" min="5" max="1440" step="1" value="30" inputmode="numeric"><span class="hint">One slice a minute</span></div>
        </div>`}
        <p class="ticket-preview num" data-wt-sum aria-live="polite"></p>
        <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" type="submit">Get quote</button></div>
      </form>`;
    $("[data-switch]", sheet)?.addEventListener("click", async (e) => {
      const done = busy(e.currentTarget, "Switching…");
      try { await ensureBase(); toast("Switched to Base", "ok"); form(); } catch (err) { toast(walletErr(err), "err"); done(); }
    });
    const f = $("[data-wt]", sheet);
    liveNumbers(f);
    const sync = () => {
      const v = Object.fromEntries(new FormData(f));
      const st = v.style || "market";
      $$("[data-when]", f).forEach((el) => (el.hidden = el.dataset.when !== st));
      const size = num(v.sizeUsd), tk = copy ? opts.token : v.token;
      const low = known(usdcBal) && size > usdcBal;
      const how = copy ? "at market, with the signal's TP / SL" : { market: "at market", limit: `${fixed(v.limitPct, 1)}% below the price`, twap: `over ${v.twapMinutes || "?"} min` }[st];
      $("[data-wt-sum]", f).innerHTML = Number.isFinite(size) && size > 0
        ? `Buys ${esc(usd(size))} of ${esc(tk)} ${esc(how)}${copy ? "" : ` · TP +${esc(fixed(v.takeProfitPct, 1))}% · SL −${esc(fixed(v.stopLossPct, 1))}%`}.${low ? `<br><span class="warn">Your wallet has ${esc(usd(usdcBal))} USDC. Lower the size or add funds.</span>` : ""}`
        : "Enter a size.";
    };
    f.addEventListener("change", sync); f.addEventListener("input", sync); sync();
    f.addEventListener("submit", async (e) => {
      e.preventDefault();
      const bad = checkNumbers(f);
      if (bad) { bad.focus(); return; }
      const v = Object.fromEntries(new FormData(f));
      const body = { address: addr, token: copy ? opts.token : v.token, sizeUsd: +v.sizeUsd, style: copy ? "market" : v.style || "market",
        takeProfitPct: +v.takeProfitPct || 0, stopLossPct: +v.stopLossPct || 0, limitPct: v.style === "limit" ? +v.limitPct || 0 : 0, twapMinutes: v.style === "twap" ? +v.twapMinutes || 0 : 0, copyOf: opts.copyOf || "" };
      if (known(usdcBal) && body.sizeUsd > usdcBal && mode === "live") { toast(`Your wallet has ${usd(usdcBal)} USDC. Lower the size or add funds.`, "err"); return; }
      const done = busy($("button[type=submit]", f), "Getting quote…");
      try { confirmView(body, await api("/v1/wallet/quote", { method: "POST", body, timeout: 60000 })); }
      catch (err) { toast(err.message, "err"); done(); }
    });
  };
  const confirmView = (body, r) => {
    const q = r.quote, s = r.summary || {};
    const paper = r.mode === "paper";
    const steps = [...(q.approvals || []).map((a) => ["approve", a.label || "Approve USDC"]), ["sign", "Sign the order"], ...(q.bracketTypedData ? [["sign2", "Sign take profit / stop loss"]] : []), ["send", "Send to Flash"]];
    sheet.innerHTML = `${head("Confirm in your wallet")}
      <p class="mode-note">${paper ? '<span class="mini mini-paper">Paper money</span><span>Nothing leaves your wallet</span>' : r.mode === "dry" ? '<span class="mini mini-paper">Dry run</span><span>Signed, not sent</span>' : '<span class="mini mini-live">Live · real money</span><span>Real USDC on Base</span>'}</p>
      <dl class="spec spec-tight wt-sum">
        <div><dt>Buy</dt><dd class="num">${esc(usd(s.sizeUsd))} ${esc(s.token)} · ${esc(styleLabel(s.style))}</dd></div>
        <div><dt>${+s.entryLimit > 0 ? "Limit" : "Price"}</dt><dd class="num">${esc(price(+s.entryLimit > 0 ? s.entryLimit : s.mark))}</dd></div>
        <div><dt>Take profit</dt><dd class="num up">▲ ${esc(price(s.takeProfit))}</dd></div>
        <div><dt>Stop loss</dt><dd class="num down">▼ ${esc(price(s.stopLoss))}</dd></div>
        <div><dt>Flash cost</dt><dd class="num">${known(s.costPct) && +s.costPct > 0 ? `${esc(fixed(s.costPct, 2))}% (${esc(usd(s.costUsd, 3))})` : "—"}</dd></div>
        <div><dt>Quote</dt><dd class="num" data-left>—</dd></div>
      </dl>
      <ol class="wsteps" data-steps aria-live="polite">${steps.map(([, l], i) => `<li data-step="${i}"><span class="wdot" aria-hidden="true"></span><span data-label>${esc(l)}</span></li>`).join("")}</ol>
      <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" data-go>Sign &amp; place</button><button class="btn btn-plain btn-block" data-requote>New quote</button></div>`;
    const end = new Date(q.expiresAt).getTime();
    const go = $("[data-go]", sheet), rq = $("[data-requote]", sheet);
    let working = false;
    const tick = () => {
      const left = Math.max(0, Math.round((end - Date.now()) / 1000));
      const el = $("[data-left]", sheet);
      if (el) { el.textContent = left ? `valid ${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}` : "expired"; el.className = "num" + (left && left <= 10 ? " warn" : !left ? " down" : ""); }
      if (!left && !working) {
        stop();
        if (go?.isConnected) { go.disabled = true; go.textContent = "Quote expired"; }
        if (rq?.isConnected) { rq.className = "btn btn-primary btn-block"; rq.textContent = "Get a new quote"; }
      }
    };
    stop(); tick(); timer = setInterval(tick, 1000);
    rq.addEventListener("click", () => { stop(); form(); });
    const mark = (i, st, extra = "") => {
      const li = $(`[data-step="${i}"]`, sheet);
      if (!li) return;
      li.dataset.state = st;
      if (extra) $("[data-label]", li).innerHTML = extra;
    };
    go.addEventListener("click", async () => {
      working = true; rq.disabled = true;
      const done = busy(go, "Check your wallet…");
      let i = 0;
      try {
        await ensureBase();
        for (const a of q.approvals || []) {
          mark(i, "busy");
          const hash = await eth().request({ method: "eth_sendTransaction", params: [{ from: addr, to: a.to, data: a.data, value: a.value || "0x0" }] });
          mark(i, "busy", `${esc(a.label || "Approve USDC")} · pending ${isHash(hash) ? txLink(hash) : ""}`);
          await waitReceipt(hash);
          mark(i++, "done", `${esc(a.label || "Approve USDC")} · confirmed`);
        }
        mark(i, "busy");
        const entrySignature = await eth().request({ method: "eth_signTypedData_v4", params: [addr, q.entryTypedData] });
        mark(i++, "done");
        let bracketSignature = "";
        if (q.bracketTypedData) {
          mark(i, "busy");
          bracketSignature = await eth().request({ method: "eth_signTypedData_v4", params: [addr, q.bracketTypedData] });
          mark(i++, "done");
        }
        mark(i, "busy");
        if (Date.now() > end) throw new Error("The quote expired while you were signing. Get a new quote.");
        const res = await api("/v1/wallet/order", { method: "POST", body: { quoteId: q.quoteId, address: addr, entrySignature, bracketSignature }, timeout: 60000 });
        mark(i, "done");
        stop(); sheetClose(sheet);
        toast(paper ? "Placed (paper money)" : "Order sent to Flash", "ok");
        if (res?.trade?.id) location.hash = "#/trade/" + enc(res.trade.id);
      } catch (err) {
        let msg = err?.code === 4001 ? walletErr(err) : err?.code === -32002 ? "Your wallet already has a request open. Check it." : err.message || walletErr(err);
        if (/signed by|signature/i.test(msg)) msg = `Your wallet signed with a different account. Switch to ${short(addr)} and try again.`;
        // Keep the reason next to the step that failed, not only in a toast that fades.
        mark(i, "fail", `${esc(steps[i] ? steps[i][1] : "Failed")} · <span class="down">${esc(msg)}</span>`);
        toast(msg, "err");
        working = false; done(); rq.disabled = false;
        if (Date.now() > end) tick();
      }
    });
  };
  await form();
  sheetOpen(sheet);
}

/* ---------------- order ticket (every Flash order style) ---------------- */
function ticketHTML(wallets, preset, tokens) {
  const one = preset && wallets.length === 1;
  const w0 = wallets.find((w) => w.id === preset) || wallets[0] || {};
  const mode = modeOf();
  const max = +(state.stats?.limits?.maxTradeUsd ?? 0);
  return `<form class="ticket" data-ticket novalidate>
    <p class="chips">${mode ? `<span class="mini ${mode === "live" ? "mini-live" : "mini-paper"}">${esc(MODE_INFO[mode].badge)}</span>` : ""}${one ? `<span class="mini">${esc(w0.agentName || w0.profile?.owner || w0.id)} · ${esc(usd(w0.usdc))}</span>` : ""}<span class="mini">Stop loss on every order</span></p>
    <div class="form-grid form-3 field-gap">
      ${one ? `<input type="hidden" name="walletId" value="${esc(preset)}" data-usdc="${esc(w0.usdc ?? "")}">` : `<div class="field"><label>Wallet<select name="walletId">${wallets.map((w) => `<option value="${esc(w.id)}" data-usdc="${esc(w.usdc ?? "")}" ${w.id === preset ? "selected" : ""}>${esc(w.agentName || w.profile?.owner || w.id)} · ${esc(usd(w.usdc))}</option>`).join("")}</select></label></div>`}
      <div class="field"><label>Coin<select name="token">${tokens.map((t) => `<option value="${esc(t.symbol)}" data-px="${esc(t.price)}">${esc(t.symbol)} · ${esc(price(t.price))}</option>`).join("")}</select></label></div>
      <div class="field"><label>Size ($)<input name="sizeUsd" type="number" min="1" ${max > 0 ? `max="${esc(max)}"` : ""} step="0.5" value="${esc(Math.min(10, max > 0 ? max : 10))}" inputmode="decimal" required></label><span class="hint">${max > 0 ? `$1 to ${esc(usd(max, 0))}` : "At least $1"}</span></div>
    </div>
    <div class="field field-gap"><span class="label">Order</span><div class="seg" role="radiogroup" aria-label="Order style">
      ${Object.keys(STYLE).map((s, i) => `<label><input type="radio" name="style" value="${s}" ${i ? "" : "checked"}><span>${esc(STYLE[s])}</span></label>`).join("")}</div></div>
    <div class="form-grid form-4 field-gap">
      <div class="field"><label>Take profit (%)<input name="takeProfitPct" type="number" min="0.6" max="50" step="0.1" value="2" inputmode="decimal" required></label><span class="hint">Sells when price rises this much</span></div>
      <div class="field"><label>Stop loss (%)<input name="stopLossPct" type="number" min="0.2" max="5" step="0.1" value="1" inputmode="decimal" required></label><span class="hint">Sells when price falls this much</span></div>
      <div class="field"><label>Max slippage (%)<input name="maxSlippagePct" type="number" min="0.1" max="5" step="0.1" value="1" inputmode="decimal" required></label><span class="hint">How far the price may move while filling</span></div>
      <div class="field" data-when="limit" hidden><label>Limit below (%)<input name="limitPct" type="number" min="0.1" max="10" step="0.1" value="0.5" inputmode="decimal"></label><span class="hint">Buys only after this dip</span></div>
      <div class="field" data-when="twap" hidden><label>DCA over (min)<input name="twapMinutes" type="number" min="5" max="1440" step="1" value="30" inputmode="numeric"></label><span class="hint">One slice a minute</span></div>
      <div class="field" data-when="stop" hidden><label>Trigger above (%)<input name="stopPct" type="number" min="0.1" max="10" step="0.1" value="0.5" inputmode="decimal"></label><span class="hint">Buys on a breakout this far up</span></div>
    </div>
    <div class="field field-gap" data-when="stop" hidden><span class="label">Protection after the fill</span><div class="seg" role="radiogroup" aria-label="Protection">
      <label><input type="radio" name="protect" value="bracket" checked><span>Bracket</span></label><label><input type="radio" name="protect" value="trailing"><span>Trailing</span></label><label><input type="radio" name="protect" value="separate"><span>SL + TP</span></label></div></div>
    <div class="form-grid form-4" data-when="trail" hidden><div class="field"><label>Trail (%)<input name="trailPct" type="number" min="0.5" max="20" step="0.1" value="2" inputmode="decimal"></label><span class="hint">Sells after this pullback from the high</span></div></div>
    <p class="ticket-preview num" data-preview aria-live="polite"></p>
    <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" type="submit">Place order</button></div>
  </form>`;
}

function wireTicket(form) {
  liveNumbers(form);
  const read = () => {
    const f = new FormData(form), n = (k) => num(f.get(k));
    const style = f.get("style") || "market", protect = style === "stop" ? f.get("protect") || "bracket" : "bracket";
    return { walletId: f.get("walletId"), token: f.get("token"), style, sizeUsd: n("sizeUsd"),
      takeProfitPct: n("takeProfitPct"), stopLossPct: n("stopLossPct"), maxSlippagePct: n("maxSlippagePct"), limitPct: n("limitPct"),
      // Protection choices apply to stop buys only; the other styles get Flash's attached bracket.
      twapMinutes: Math.round(n("twapMinutes")), stopPct: n("stopPct"), protect, trailPct: protect === "trailing" ? n("trailPct") : 0 };
  };
  // Known balance of the chosen wallet, to warn before a too-big order.
  const balance = () => {
    const sel = $("select[name=walletId]", form);
    const el = sel ? sel.selectedOptions[0] : $("input[name=walletId]", form);
    return num(el?.dataset.usdc);
  };
  let summary = "";
  const draw = () => {
    const v = read();
    $$("[data-when]", form).forEach((el) => { const w = el.dataset.when; el.hidden = w === "trail" ? !(v.style === "stop" && v.protect === "trailing") : w !== v.style; });
    const px = +($("select[name=token]", form)?.selectedOptions[0]?.dataset.px || 0);
    const entry = v.style === "limit" ? px * (1 - (v.limitPct || 0) / 100) : v.style === "stop" ? px * (1 + (v.stopPct || 0) / 100) : px;
    const how = { market: `at market ≈ ${price(entry)}`, limit: `at ${price(entry)} or better`, twap: `over ${v.twapMinutes || "?"} min (${v.twapMinutes || "?"} slices)`, stop: `when price hits ${price(entry)}` }[v.style];
    const prot = v.style === "stop" && v.protect === "trailing" ? `trailing ${fixed(v.trailPct, 1)}%` : `TP ${price(entry * (1 + (v.takeProfitPct || 0) / 100))} · SL ${price(entry * (1 - (v.stopLossPct || 0) / 100))}`;
    const bal = balance(), low = known(bal) && v.sizeUsd > bal;
    summary = px > 0 && v.sizeUsd > 0 ? `Buys ${usd(v.sizeUsd)} ${v.token} ${how} · ${prot}` : "";
    $("[data-preview]", form).innerHTML = !(px > 0) ? "Waiting for a live price"
      : !(v.sizeUsd > 0) ? "Enter a size."
      : esc(summary) + (low ? `<br><span class="warn">This wallet has ${esc(usd(bal))} USDC.${modeOf() === "paper" ? " Add paper USDC on Wallets, or" : ""} Lower the size.</span>` : "");
  };
  form.addEventListener("input", draw); form.addEventListener("change", draw); draw();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const bad = checkNumbers(form);
    if (bad) { bad.focus(); toast("Check the highlighted field.", "err"); return; }
    const v = read();
    const bal = balance();
    if (known(bal) && v.sizeUsd > bal) { toast(CODE_MSG.BALANCE, "err"); return; }
    const effect = { live: "Real money on Base.", paper: "Paper money: real Flash quote, simulated fill.", dry: "Dry run: signed, not sent." }[modeOf()] || "";
    if (!(await confirmDialog(`Place a ${styleLabel(v.style)} order?`, `${summary}. ${effect}`, "Place order", { money: true }))) return;
    const done = busy($("button[type=submit]", form), "Placing…");
    try {
      const r = await api("/v1/admin/orders", { method: "POST", admin: true, body: v, timeout: 60000 });
      toast(`${styleLabel(v.style)} order placed`, "ok");
      if (form.closest(".sheet")) sheetClose(sheet);
      if (r?.trade?.id) location.hash = "#/trade/" + enc(r.trade.id);
    } catch (err) {
      toast(err.message, "err");
    } finally { done(); }
  });
}

/* ---------------- signals ---------------- */
function signalCard(s) {
  const open = s.status === "placed" || s.status === "filled";
  const res = s.pnlUsd != null ? `<b class="num">${pnlHTML(s.pnlUsd)}</b>` : statusChip(s.status);
  return `<article class="sig" data-reveal>
    <header class="sig-head"><a class="sig-agent" href="#/agent/${enc(s.agentId)}">${esc(s.agent)}</a><span class="coin">${esc(s.token)}</span>${styleChip(s.style)}${s.paper ? '<span class="mini mini-paper">Paper</span>' : ""}<time class="num muted small">${esc(ago(s.at))}</time></header>
    <dl class="sig-px num"><div><dt>Entry</dt><dd>${esc(price(s.entry))}</dd></div><div><dt>TP</dt><dd class="up">${esc(price(s.takeProfit))}</dd></div><div><dt>SL</dt><dd class="down">${esc(price(s.stopLoss))}</dd></div><div><dt>Result</dt><dd>${res}</dd></div></dl>
    ${s.reason ? `<p class="sig-why">${esc(s.reason)}</p>` : ""}
    <div class="btn-row sig-actions"><button type="button" class="btn btn-sm btn-ghost" data-sig-share="${esc(s.id)}">Share</button>${open ? `<button type="button" class="btn btn-sm btn-primary" data-sig-copy="${esc(s.id)}">Copy with my wallet</button>` : ""}<a class="btn btn-sm btn-ghost" href="#/trade/${enc(s.id)}">Receipt</a></div>
  </article>`;
}

async function pageSignals(page) {
  page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Signals</p><h1 data-reveal>Agent signals</h1>
      <p class="lede" data-reveal>Copy any agent trade from your own wallet.</p></div>
      <div class="btn-row"><a class="btn btn-ghost btn-sm" href="/signals.rss">RSS</a><button type="button" class="btn btn-ghost btn-sm" data-share-lb>Share leaderboard</button></div></header>
    <p class="mode-note" data-bot hidden><span class="mini">Also on Discord / Telegram</span></p>
    <div class="sig-grid" data-sigs><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const box = $("[data-sigs]", page);
  let list = [];
  const render = async () => {
    try { list = (await api("/v1/signals?limit=40")).signals || []; } catch (err) { box.innerHTML = errorState(err); return; }
    $("[data-bot]", page).hidden = !state.stats?.social?.signalBot;
    const html = list.length ? list.map(signalCard).join("") : emptyState("No signals yet", "Agents post here the moment they trade.", { href: "#/leaderboard", label: "See the agents" });
    if (box.dataset.sig !== html) { box.dataset.sig = html; box.innerHTML = html; }
  };
  box.addEventListener("click", (e) => {
    const sh = e.target.closest("[data-sig-share]"), cp = e.target.closest("[data-sig-copy]");
    const s = list.find((x) => x.id === (sh || cp)?.dataset[sh ? "sigShare" : "sigCopy"]);
    if (!s) return;
    if (sh) openShare({ url: s.share || shareURL("trade", s.id), text: `${s.agent} went long ${s.token} on Flash · TP ${price(s.takeProfit)} / SL ${price(s.stopLoss)}`,
      card: () => drawCard({ title: `${s.agent}: Long ${s.token}`, big: s.pnlUsd != null ? usd(s.pnlUsd) : `TP ${price(s.takeProfit)}`, down: s.pnlUsd < 0, line: `${styleLabel(s.style)} · Entry ${price(s.entry)} · SL ${price(s.stopLoss)}` }), file: `arena-${s.id}.png` });
    if (cp) walletTrade({ copyOf: s.id, token: s.token, from: s.agent });
  });
  $("[data-share-lb]", page).addEventListener("click", shareLeaderboard);
  await render();
  const t = setInterval(() => { if (document.visibilityState === "visible") render(); }, 10000);
  return { cleanup: () => clearInterval(t), onPost: (p) => { if (/^trade_(placed|closed)$/.test(p.kind)) render(); } };
}

async function shareLeaderboard() {
  const lb = state.board || (await loadBoard().catch(() => null));
  const top = (lb?.rows || []).slice(0, 3);
  const text = top.length ? `Agent Arena standings: ${top.map((r) => `#${r.rank} ${r.name}`).join(" · ")} · AI agents trading on Flash` : "Agent Arena: AI agents trading on Flash";
  openShare({ url: shareURL("leaderboard"), text, file: "arena-leaderboard.png",
    card: () => drawCard({ title: "Agent Arena standings", big: top[0] ? top[0].name : "Live league", line: top.map((r) => `#${r.rank} ${r.name} ${FMT.score(+r.score || 0)}`).join("   "), down: false }) });
}

/* ---------------- onchain proof ---------------- */
// Only real-money trades are listed; each carries the Flash order id and the
// Base transactions, so anyone can check it on Basescan.
const txPill = (label, h) => (isHash(h) ? `<a class="txlink" href="https://basescan.org/tx/${h}" ${EXT}>${esc(label)}<span aria-hidden="true"> ↗</span></a>` : "");
function proofItem(t) {
  const right = t.pnlUsd != null ? `<span class="num strong">${pnlHTML(t.pnlUsd)}</span>` : "";
  const ids = [t.orderId && `<span class="txid" title="Flash order id">Flash ${esc(short(t.orderId))}</span>`,
    t.bracketOrderId && `<span class="txid" title="Flash exit order id">Exit order ${esc(short(t.bracketOrderId))}</span>`].filter(Boolean).join("");
  const txs = txPill("Approve tx", t.approveTx) + txPill("Buy tx", t.entryTx) + txPill("Sell tx", t.exitTx);
  return `<li class="proof-item">
    <a class="trade-item" href="#/trade/${enc(t.id)}">${coinIco(t.token)}
      <div class="t-main"><div class="t-title"><span class="t-name">Long ${esc(t.token)} <span class="num">${esc(usd(t.sizeUsd))}</span></span>${t.kind ? `<span class="mini">${esc(KIND_LABEL[t.kind] || cap(t.kind))}</span>` : ""}</div>
        <div class="t-meta"><span>${esc(t.who)}</span><span>${esc(ago(t.at))}</span>${t.usdcSpent ? `<span class="num">paid ${esc(fixed(t.usdcSpent, 4))} USDC</span>` : ""}${t.usdcReceived ? `<span class="num">got ${esc(fixed(t.usdcReceived, 4))} USDC</span>` : ""}</div></div>
      <div class="t-right">${statusChip(t.status)}<div>${right}</div></div></a>
    <div class="proof-links">${t.wallet ? `<span class="txid">Wallet ${addrLink(t.wallet)}</span>` : ""}${ids}${txs || '<span class="txid">Waiting for the onchain fill</span>'}</div>
  </li>`;
}

async function pageProof(page) {
  page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Onchain proof</p><h1 data-reveal>Real trades on Flash</h1>
      <p class="lede" data-reveal>Real money only. Every fill links to Base.</p></div>
      <div class="btn-row" data-proof-links></div></header>
    <div data-proof><div class="skel skel-row"></div><div class="skel skel-row"></div></div>`;
  const box = $("[data-proof]", page);
  const render = async () => {
    let d;
    try { d = await api("/v1/proof"); } catch (err) { box.innerHTML = errorState(err); return; }
    const T = d.totals || {}, rows = d.trades || [], nl = d.notListed || {};
    $("[data-proof-links]", page).innerHTML = isAddr(d.settlement) ? `<a class="btn btn-ghost btn-sm" href="https://basescan.org/address/${d.settlement}" ${EXT}>Flash settlement ↗</a>` : "";
    const hidden = (+nl.paper || 0) + (+nl.dryRun || 0);
    const budget = d.live && d.liveBudgetUsd > 0 ? `<div class="meter section-tight"><div class="meter-head"><span>Live budget used</span><span class="num">${esc(usd(d.liveSpentUsd))} of ${esc(usd(d.liveBudgetUsd))}</span></div><div class="mbar"><i data-w="${esc(Math.min(100, (d.liveSpentUsd / d.liveBudgetUsd) * 100))}"></i></div></div>` : "";
    const html = `${d.live ? "" : `<p class="mode-note" data-reveal><span class="mini mini-paper">${esc(MODE_INFO[d.mode]?.badge || d.mode)}</span><span>Not live yet: ${esc(plural(hidden, "simulated trade"))} hidden here.</span></p>`}
      <dl class="statline">
        <div class="stat"><dt>Real orders</dt><dd class="num">${esc(T.orders ?? 0)}</dd></div>
        <div class="stat"><dt>Filled onchain</dt><dd class="num">${esc(T.filled ?? 0)}</dd></div>
        <div class="stat"><dt>Volume via Flash</dt><dd class="num">${esc(usd(T.volumeUsd ?? 0))}</dd></div>
        <div class="stat"><dt>Base txs</dt><dd class="num">${esc(T.onchainTxs ?? 0)}</dd></div>
        <div class="stat"><dt>Trading cost</dt><dd class="num">${esc(usd(T.entryCostUsd ?? 0))}</dd></div>
        <div class="stat"><dt>P&amp;L</dt><dd class="num">${pnlHTML(T.pnlUsd ?? 0)}</dd></div>
      </dl>${budget}
      <div class="section-head section"><h2>Orders</h2><span class="muted small num">${esc(rows.length)}</span></div>
      ${rows.length ? `<ul class="trades proof-list">${rows.map(proofItem).join("")}</ul>`
        : emptyState("No real trades yet", d.live ? "The first live order shows here with its Base transactions." : "Switch to live mode and place a $1 test trade.", { href: "#/docs", label: "How it works" })}`;
    if (box.dataset.html !== html) {
      box.dataset.html = html; box.innerHTML = html;
      $$(".mbar i[data-w]", box).forEach((el) => el.style.setProperty("width", el.dataset.w + "%"));
    }
  };
  await render();
  const t = setInterval(() => { if (document.visibilityState === "visible") render(); }, 15000);
  return { cleanup: () => clearInterval(t), onPost: (p) => { if (/^trade_/.test(p.kind)) render(); } };
}

/* ---------------- arena account: USDC only, no gas ---------------- */
// Sign in once (a free message signature), then the account, deposits (a free
// USDC signature the arena's gas tank sends), investing and withdrawals. The
// session belongs to one address and lasts a week (the server keeps it
// across restarts). One per wallet, so switching accounts needs no new sign-in.
function sessions() { return store.get("arena.sessions", {}) || {}; }
function meToken() {
  if (!wallet.address) return "";
  const s = sessions()[wallet.address.toLowerCase()];
  return s && Date.parse(s.exp) > Date.now() ? s.token : "";
}
function dropSession(addr) {
  if (!addr) return;
  const all = sessions();
  delete all[addr.toLowerCase()];
  store.set("arena.sessions", all);
}
const utf8Hex = (s) => "0x" + [...new TextEncoder().encode(s)].map((b) => b.toString(16).padStart(2, "0")).join("");
// The account wallet ids an address can have: the 30-character id, and the
// short id of accounts opened before it.
const ownAccountIds = (addr) => { const a = String(addr || "").slice(2).toLowerCase(); return ["u-" + a.slice(0, 30), "u-" + a.slice(0, 10)]; };
async function meSignIn() {
  const addr = await ensureWallet();
  if (!addr) return false;
  const { message, nonce } = await api("/v1/me/nonce", { method: "POST", body: { address: addr } });
  const signature = await eth().request({ method: "personal_sign", params: [utf8Hex(message), addr] });
  const r = await api("/v1/me/login", { method: "POST", body: { address: addr, signature, nonce } });
  const all = sessions();
  for (const [k, v] of Object.entries(all)) if (!(Date.parse(v.exp) > Date.now())) delete all[k];
  all[r.address.toLowerCase()] = { token: r.token, exp: r.expiresAt };
  store.set("arena.sessions", all);
  return true;
}

function accountHTML(d) {
  const a = d.account, gas = d.gas || {}, paper = d.mode === "paper";
  const gasLine = paper ? "Paper mode: deposits are checked like real ones, then paper USDC is added."
    : gas.on && gas.live ? "Gas is paid by Agent Arena. You only hold USDC." : gas.on ? "Gas is paid by Agent Arena in live mode." : "The arena has no gas tank yet, so its wallets pay their own gas.";
  if (!a) {
    return `<div class="acct-intro"><p class="eyebrow">Arena account</p><h3>Trade with USDC only. No gas.</h3>
      <ol class="intro-steps acct-steps"><li><div><b>Create</b><span>Money can only go back to ${esc(short(d.address))}</span></div></li>
      <li><div><b>Add USDC</b><span>Sign once in your wallet, free</span></div></li><li><div><b>Pick an agent</b><span>It trades for you, stop loss on every order</span></div></li></ol>
      <button type="button" class="btn btn-primary btn-block" data-acct-create>Create my account</button><p class="footnote">${esc(gasLine)}</p></div>`;
  }
  const f = (a.follows || [])[0], paused = a.profile?.paused, ag = d.agent, plan = d.plan;
  const agent = f ? (state.board?.rows || []).find((r) => r.agentId === f.agentId)?.name || f.agentId : "";
  const link = ag ? `Your agent <a class="link" href="#/agent/${enc(ag.id)}">${esc(ag.name)}</a> · ${esc(AGENT_KIND[ag.kind]?.label || ag.kind)} on ${esc(ag.token || "WETH")} · take profit ${esc(fixed(ag.takeProfitPct, 1))}% · stop loss ${esc(fixed(ag.stopLossPct, 1))}% · ${esc(fixed(ag.sizePct, 0))}% a trade${ag.paused ? " · <b>paused</b>" : ""}`
    : plan ? `Running your plan <a class="link" href="#/plan/${enc(plan.id)}">${esc(plan.name)}</a> (${esc(plan.status)})`
    : f ? `${f.mode === "copy" ? "Copying" : "Investing with"} <b>${esc(agent)}</b> · ${f.allocationPct ? esc(f.allocationPct) + "%" : "agent's size"} a trade, max ${esc(usd(f.maxPerTradeUsd))}` : "Not linked to an agent yet";
  return `<div class="action-row"><div><p class="eyebrow">Arena account ${paused ? '<span class="mini mini-test">Paused</span>' : ""}</p>
      <p class="addr">${addrLink(a.address)}<span class="muted small">Payout locked to ${esc(short(d.address))}</span></p></div></div>
    <dl class="statline">
      <div class="stat"><dt>USDC</dt><dd class="num">${esc(fixed(a.usdc, 2))}</dd></div>
      <div class="stat"><dt>WETH</dt><dd class="num">${esc(fixed(a.weth, 6))}</dd></div>
      <div class="stat"><dt>Equity</dt><dd class="num">${esc(usd(a.equityUsd))}</dd></div>
      <div class="stat"><dt>P&amp;L</dt><dd class="num">${pnlHTML(a.pnlUsd)}</dd></div>
    </dl>
    <p class="acct-link">${link}</p>
    <div class="btn-row"><button type="button" class="btn btn-primary" data-acct-add>Add funds</button>
      ${ag ? `<a class="btn btn-ghost" href="#/agent/${enc(ag.id)}">View my agent</a><button type="button" class="btn btn-ghost" data-acct-agent-edit>Edit rules</button><button type="button" class="btn btn-ghost" data-acct-agent-retire>Retire agent</button><button type="button" class="btn btn-ghost" data-acct-agent-pause>${ag.paused ? "Resume agent" : "Pause agent"}</button>`
        : plan ? `<a class="btn btn-ghost" href="#/plan/${enc(plan.id)}">View my plan</a>`
        : `<button type="button" class="btn btn-ghost" data-acct-invest>${f ? "Change agent" : "Pick an agent"}</button>${f ? "" : '<button type="button" class="btn btn-ghost" data-acct-make-agent>Make my own agent</button>'}`}
      <button type="button" class="btn btn-ghost" data-acct-withdraw ${a.usdc > 0 ? "" : "disabled"}>Withdraw</button>
      ${f ? `<button type="button" class="btn btn-ghost" data-acct-pause>${paused ? "Resume" : "Pause"}</button><button type="button" class="btn btn-ghost" data-acct-stop>${f.mode === "copy" ? "Stop copying" : "Stop investing"}</button>` : ""}</div>
    <p class="footnote">${esc(gasLine)}</p>
    ${(a.recent || []).length ? `<ul class="trades section-tight">${a.recent.slice(0, 5).map(tradeRow).join("")}</ul>` : ""}`;
}

async function renderAccount(box, onChange) {
  if (!wallet.address) { box.innerHTML = ""; return; }
  if (!meToken()) {
    box.innerHTML = `<div class="acct-intro"><p class="eyebrow">Arena account</p><h3>Trade with USDC only. No gas.</h3>
      <p class="muted small">Sign a free message to prove this wallet is yours.</p><button type="button" class="btn btn-primary btn-block" data-acct-signin>Sign in with wallet</button></div>`;
    $("[data-acct-signin]", box).addEventListener("click", async (e) => {
      const done = busy(e.currentTarget, "Check your wallet…");
      try { await ensureBase(); if (await meSignIn()) await renderAccount(box, onChange); }
      catch (err) { toast(err?.code ? walletErr(err) : err.message, "err"); } finally { done(); }
    });
    return;
  }
  let d;
  try { d = await api("/v1/me", { me: true }); }
  catch (err) { if (err.status === 401) return renderAccount(box, onChange); box.innerHTML = errorState(err); return; }
  if (!state.board) await loadBoard().catch(() => null);
  box.innerHTML = accountHTML(d) + subWalletsHTML(d);
  const act = async (btn, label, fn) => {
    const done = busy(btn, label);
    try { await fn(); await renderAccount(box, onChange); onChange?.(); }
    catch (err) { toast(err?.code ? walletErr(err) : err.message, "err"); } finally { done(); }
  };
  $("[data-acct-create]", box)?.addEventListener("click", (e) => act(e.currentTarget, "Creating…", async () => {
    await api("/v1/me/account", { method: "POST", me: true, body: {} });
    toast("Account ready. Add USDC to start.", "ok");
  }));
  $("[data-acct-add]", box)?.addEventListener("click", () => depositSheet(d, () => renderAccount(box, onChange)));
  $("[data-acct-invest]", box)?.addEventListener("click", () => investSheet(d, () => renderAccount(box, onChange)));
  $("[data-acct-withdraw]", box)?.addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    if (!(await confirmDialog("Withdraw all USDC?", `It goes to your wallet ${short(d.address)}, the only address it can go to. Open trades keep running.`, "Withdraw", { money: true }))) return;
    await act(btn, "Withdrawing…", async () => {
      const r = await api("/v1/me/withdraw", { method: "POST", me: true, body: { token: "USDC" }, timeout: 120000 });
      toast(`Sent ${fixed(r.amount, 2)} USDC to your wallet`, "ok");
    });
  });
  $("[data-acct-pause]", box)?.addEventListener("click", (e) => act(e.currentTarget, "Saving…", async () => {
    await api(d.account?.profile?.paused ? "/v1/me/resume" : "/v1/me/pause", { method: "POST", me: true, body: {} });
  }));
  $("[data-acct-stop]", box)?.addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    if (!(await confirmDialog("Stop following this agent?", "No new trades are placed for you. An open trade keeps its take profit and stop loss, and your USDC stays in your account.", "Stop"))) return;
    await act(btn, "Stopping…", async () => {
      await api("/v1/me/stop", { method: "POST", me: true, body: {} });
      toast("Stopped. Your account is free to invest, copy, run a plan or become an agent.", "ok");
    });
  });
  $("[data-acct-make-agent]", box)?.addEventListener("click", () => agentSheet(() => renderAccount(box, onChange), null, d.account?.usdc));
  $("[data-acct-agent-edit]", box)?.addEventListener("click", () => agentSheet(() => renderAccount(box, onChange), d.agent, d.account?.usdc));
  $("[data-acct-agent-retire]", box)?.addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    if (!(await confirmDialog("Retire your agent?", "It stops trading and leaves the leaderboard, and everyone following it stops. Open trades keep their take profit and stop loss. Your account becomes a plain account again.", "Retire"))) return;
    await act(btn, "Retiring…", async () => {
      await api("/v1/me/agent/retire", { method: "POST", me: true, body: {} });
      state.board = null;
      toast("Agent retired. Your account can invest, copy or run a plan again.", "ok");
    });
  });
  $("[data-acct-agent-pause]", box)?.addEventListener("click", (e) => act(e.currentTarget, "Saving…", async () => {
    await api(d.agent?.paused ? "/v1/me/agent/resume" : "/v1/me/agent/pause", { method: "POST", me: true, body: {} });
    toast(d.agent?.paused ? "Your agent is trading again" : "Your agent is paused. Open trades keep their stop loss.", "ok");
  }));
  $$("[data-sub]", box).forEach((b) => b.addEventListener("click", async (e) => {
    const btn = e.currentTarget, [id, action] = btn.dataset.sub.split("|");
    if (action === "withdraw" && !(await confirmDialog("Withdraw its USDC?", `It goes to your wallet ${short(d.address)}, the only address it can go to. An open trade keeps running.`, "Withdraw", { money: true }))) return;
    if (action === "stop" && !(await confirmDialog("Stop copying?", "No new trades are copied. An open trade keeps its take profit and stop loss.", "Stop"))) return;
    await act(btn, "Saving…", async () => {
      await api(`/v1/me/wallets/${enc(id)}/${action}`, { method: "POST", me: true, body: {}, timeout: 120000 });
      toast({ pause: "Paused", resume: "Trading again", stop: "Stopped copying", withdraw: "USDC sent to your wallet" }[action], "ok");
    });
  }));
}

// Sub-wallets whose payout is locked to this person (e.g. a copy wallet):
// balances, what each follows, its open trade, and owner controls.
function subWalletsHTML(d) {
  const ws = d.wallets || [];
  if (!ws.length) return "";
  const name = (id) => (state.board?.rows || []).find((r) => r.agentId === id)?.name || id;
  return `<div class="section-head section-tight"><h3>Your sub-wallets</h3><span class="muted small num">${ws.length}</span></div>
    <ul class="subs">${ws.map((w) => {
      const f = (w.follows || [])[0], paused = w.profile?.paused, open = w.openTrade;
      return `<li class="sub"><div class="sub-top"><div><b>${esc(w.profile?.owner || w.id)}</b> ${paused ? '<span class="mini mini-test">Paused</span>' : ""}
          <div class="muted small">${addrLink(w.address)} · payout locked to you</div></div>
          <div class="sub-eq num">${esc(usd(w.equityUsd))}<small>${pnlHTML(w.pnlUsd)}</small></div></div>
        <p class="small">${f ? `${f.mode === "copy" ? "Copies" : "Invests with"} <a class="link" href="#/agent/${enc(f.agentId)}">${esc(name(f.agentId))}</a> · ${f.allocationPct ? esc(f.allocationPct) + "%" : "agent's size"} a trade, max ${esc(usd(f.maxPerTradeUsd))}, ${esc(usd(f.dailyCapUsd))} a day` : "Not following an agent"}
          · USDC ${esc(fixed(w.usdc, 2))} · ${esc(tokenOf(open))} ${esc(fixed(w.weth, 6))}</p>
        ${open ? `<a class="sub-open" href="#/trade/${enc(open.id)}">Open trade: ${esc(usd(open.sizeUsd))} at ${esc(price(open.fillPrice || open.entry))} · TP ${esc(price(open.takeProfit))} · SL ${esc(price(open.stopLoss))} ›</a>` : ""}
        <div class="btn-row"><button type="button" class="btn btn-ghost btn-sm" data-sub="${esc(w.id)}|${paused ? "resume" : "pause"}">${paused ? "Resume" : "Pause"}</button>
          ${f ? `<button type="button" class="btn btn-ghost btn-sm" data-sub="${esc(w.id)}|stop">Stop copying</button>` : ""}
          <button type="button" class="btn btn-ghost btn-sm" data-sub="${esc(w.id)}|withdraw" ${w.usdc > 0 ? "" : "disabled"}>Withdraw USDC</button></div></li>`;
    }).join("")}</ul>`;
}

// The strategies a person can give their agent (same code as the built-in agents).
const AGENT_KIND = {
  pullback: { label: "Dip in an uptrend", line: "Buys a pullback only while the longer trend is rising. Fewer, better trades" },
  dip: { label: "Dip buyer", line: "Buys when price falls a set % below its recent high" },
  trend: { label: "Trend", line: "Buys when the short average crosses above the long one" },
  contrarian: { label: "Contrarian", line: "Buys after price falls several checks in a row" },
};
const agentWhen = (r) => ({
  trend: `the ${r.fast}-check average crosses above the ${r.slow}-check one`,
  dip: `price is ${+(+r.dipPct).toFixed(2)}% below its ${r.window}-check high`,
  pullback: `price is ${+(+r.dipPct).toFixed(2)}% below its ${r.window}-check high while the ${r.trend}-check average is rising`,
  contrarian: `price falls ${r.streak} checks in a row`,
}[r.kind || r.strategy] || "");

// Make or edit an agent. `old` = the agent's current rules (edit), `usdc` =
// the account's USDC (to show the $ size of a trade and test with it).
async function agentSheet(after, old = null, usdc = 0) {
  const sb = openSheet(old ? "Edit your agent" : "Make your agent", { wide: true });
  sb.innerHTML = `<div class="sheet-body"><div class="skel skel-row"></div></div>`;
  const toks = await api("/v1/tokens").then((d) => d.tokens || []).catch(() => [{ symbol: "WETH" }]);
  const maxPos = +(state.stats?.limits?.maxPositionPct) || 25;
  const v0 = old || { kind: "pullback", token: toks[0]?.symbol || "WETH", window: 40, dipPct: 0.5, trend: 120, fast: 5, slow: 20, streak: 4, takeProfitPct: 2, stopLossPct: 1.2, sizePct: Math.min(50, maxPos) };
  const val = (k, d) => esc(v0[k] ?? d);
  const L = state.stats?.limits || {};
  const lim = (k) => esc(old?.limits?.[k] || "");
  sb.innerHTML = `<form class="sheet-body" data-agent-form novalidate>
      <p class="sheet-text">${old ? "New rules apply from its next decision. Open trades keep their take profit and stop loss." : "It trades its own signals from your account. It shows on the leaderboard, and anyone can watch, copy or invest with it."}</p>
      <div class="form-grid form-2">
        <div class="field"><label for="ag-name">Name</label><input id="ag-name" name="name" maxlength="24" autocomplete="off" placeholder="Steady Eddie" value="${val("name", "")}" required><span class="hint">3-24 letters or numbers, must be unique</span></div>
        <div class="field"><label for="ag-tag">One line about it</label><input id="ag-tag" name="tagline" maxlength="80" autocomplete="off" placeholder="Buys pullbacks in an uptrend" value="${val("tagline", "")}"></div>
      </div>
      <div class="field field-gap"><span class="label">Coin it trades</span>
        <div class="seg seg-full" role="radiogroup" aria-label="Coin">${toks.map((t) => `<label><input type="radio" name="token" value="${esc(t.symbol)}" ${(v0.token || "WETH").toUpperCase() === String(t.symbol).toUpperCase() ? "checked" : ""}><span>${esc(t.symbol)}</span></label>`).join("")}</div>
        <span class="hint">Bought with USDC on Base through Flash</span></div>
      <div class="field field-gap"><span class="label">Brain (strategy)</span>
        <div class="choice-grid choice-wide">${Object.entries(AGENT_KIND).map(([k, s]) => `<label class="choice"><input type="radio" name="strategy" value="${k}" ${(v0.kind || "pullback") === k ? "checked" : ""}><span class="choice-body"><b>${esc(s.label)}</b><span class="muted small">${esc(s.line)}</span></span></label>`).join("")}</div></div>
      <div class="form-grid form-3 field-gap">
        <div class="field" data-ak="trend" hidden><label for="ag-fast">Short average (checks)</label><input id="ag-fast" name="fast" type="number" min="2" max="30" step="1" value="${val("fast", 5)}" inputmode="numeric"><span class="hint">One check every 15 seconds</span></div>
        <div class="field" data-ak="trend" hidden><label for="ag-slow">Long average (checks)</label><input id="ag-slow" name="slow" type="number" min="3" max="120" step="1" value="${val("slow", 20)}" inputmode="numeric"></div>
        <div class="field" data-ak="dip pullback" hidden><label for="ag-dip">Drop (%)</label><input id="ag-dip" name="dipPct" type="number" min="0.1" max="20" step="0.05" value="${val("dipPct", 0.5)}" inputmode="decimal"><span class="hint">Below the recent high</span></div>
        <div class="field" data-ak="dip pullback" hidden><label for="ag-win">Recent high over (checks)</label><input id="ag-win" name="window" type="number" min="5" max="240" step="1" value="${val("window", 40)}" inputmode="numeric"><span class="hint">40 checks = 10 minutes</span></div>
        <div class="field" data-ak="pullback" hidden><label for="ag-trend">Trend length (checks)</label><input id="ag-trend" name="trend" type="number" min="20" max="360" step="1" value="${val("trend", 120)}" inputmode="numeric"><span class="hint">Its average must be rising. 120 = 30 min</span></div>
        <div class="field" data-ak="contrarian" hidden><label for="ag-streak">Falling checks in a row</label><input id="ag-streak" name="streak" type="number" min="2" max="20" step="1" value="${val("streak", 4)}" inputmode="numeric"><span class="hint">Fewer = more trades, more noise</span></div>
      </div>
      <div class="form-grid form-3 field-gap">
        <div class="field"><label for="ag-tp">Take profit (%)</label><input id="ag-tp" name="takeProfitPct" type="number" min="0.6" max="20" step="0.1" value="${val("takeProfitPct", 2)}" inputmode="decimal"></div>
        <div class="field"><label for="ag-sl">Stop loss (%)</label><input id="ag-sl" name="stopLossPct" type="number" min="0.3" max="5" step="0.1" value="${val("stopLossPct", 1.2)}" inputmode="decimal"><span class="hint">Every trade has one, up to 5%</span></div>
        <div class="field"><label for="ag-size">Per trade (% of your USDC)</label><input id="ag-size" name="sizePct" type="number" min="5" max="${esc(maxPos)}" step="1" value="${val("sizePct", 50)}" inputmode="numeric"><span class="hint" data-ag-usd></span><span class="hint">Up to ${esc(maxPos)}% (the arena's limit)</span></div>
      </div>
      <details class="adv" ${old?.limits && Object.keys(old.limits).length ? "open" : ""}><summary>Advanced settings <span class="muted small">optional · blank = the arena's setting · can only be stricter</span></summary>
        <div class="form-grid form-3 field-gap">
          <div class="field"><label for="ag-cost">Cost limit (%)</label><input id="ag-cost" name="maxCostPct" type="number" min="0.2" max="${esc(L.maxEntryCostPct || 3)}" step="0.1" placeholder="${esc(L.maxEntryCostPct ?? "")}" value="${lim("maxCostPct")}" inputmode="decimal"><span class="hint">Skip a trade when Flash's entry cost is above this</span></div>
          <div class="field"><label for="ag-slip">Max slippage (%)</label><input id="ag-slip" name="slippagePct" type="number" min="0.1" max="${esc(L.maxSlippage ? +L.maxSlippage * 100 : 3)}" step="0.1" placeholder="${esc(L.maxSlippage ? fixed(+L.maxSlippage * 100, 1) : "")}" value="${lim("slippagePct")}" inputmode="decimal"></div>
          <div class="field"><label for="ag-tpd">Trades a day</label><input id="ag-tpd" name="tradesPerDay" type="number" min="1" max="${esc(L.maxTradesPerDay || 100)}" step="1" placeholder="${esc(L.maxTradesPerDay ?? "")}" value="${lim("tradesPerDay")}" inputmode="numeric"></div>
          <div class="field"><label for="ag-min">Smallest trade ($)</label><input id="ag-min" name="minTradeUsd" type="number" min="1" max="${esc(L.maxTradeUsd ?? 100)}" step="0.5" placeholder="${esc(L.minTradeUsd || "")}" value="${lim("minTradeUsd")}" inputmode="decimal"><span class="hint">Small slices are lifted to this</span></div>
          <div class="field"><label for="ag-max">Largest trade ($)</label><input id="ag-max" name="maxTradeUsd" type="number" min="1" max="${esc(L.maxTradeUsd ?? 100)}" step="0.5" placeholder="${esc(L.maxTradeUsd ?? "")}" value="${lim("maxTradeUsd")}" inputmode="decimal"><span class="hint">Up to the arena's ${esc(usd(L.maxTradeUsd, 0))}</span></div>
          <div class="field"><label for="ag-cool">Wait after a loss (min)</label><input id="ag-cool" name="cooldownMin" type="number" min="0" max="1440" step="5" placeholder="0" value="${lim("cooldownMin")}" inputmode="numeric"><span class="hint">No new trade for this long</span></div>
          <div class="field"><label for="ag-hold">Time limit per trade (min)</label><input id="ag-hold" name="maxHoldMin" type="number" min="5" max="10080" step="5" placeholder="${esc(L.maxHoldHours ? L.maxHoldHours * 60 : "none")}" value="${lim("maxHoldMin")}" inputmode="numeric"><span class="hint">Sell at market if neither target nor stop is hit</span></div>
          <div class="field"><label for="ag-net">Raise target to cover costs</label><select id="ag-net" name="netTargets"><option value="">Arena (${L.netTargets ? "on" : "off"})</option><option value="on" ${old?.limits?.netTargets === "on" ? "selected" : ""}>On</option><option value="off" ${old?.limits?.netTargets === "off" ? "selected" : ""}>Off</option></select></div>
          <div class="field"><label for="ag-be">Stop to break-even halfway</label><select id="ag-be" name="breakEven"><option value="">Arena (${L.breakEvenStop ? "on" : "off"})</option><option value="on" ${old?.limits?.breakEven === "on" ? "selected" : ""}>On</option><option value="off" ${old?.limits?.breakEven === "off" ? "selected" : ""}>Off</option></select></div>
        </div>
        <p class="footnote">Fixed for everyone: a stop loss on every trade (within 5%), the arena's largest trade and the live budget.</p>
      </details>
      <p class="ticket-preview" data-ag-preview></p>
      <section class="bt" data-bt aria-live="polite"></section>
      <div class="btn-row sheet-cta"><button type="button" class="btn btn-ghost" data-bt-run>Test on real prices</button><button class="btn btn-primary" type="submit">${old ? "Save rules" : "Launch my agent"}</button></div></form>`;
  const form = $("[data-agent-form]", sb);
  const read = () => {
    const v = new FormData(form), n = (k) => num(v.get(k));
    return { name: String(v.get("name") || "").trim(), tagline: String(v.get("tagline") || "").trim(), strategy: v.get("strategy"), token: v.get("token") || "WETH",
      fast: Math.round(n("fast")), slow: Math.round(n("slow")), dipPct: n("dipPct"), window: Math.round(n("window")), streak: Math.round(n("streak")), trend: Math.round(n("trend")),
      takeProfitPct: n("takeProfitPct"), stopLossPct: n("stopLossPct"), sizePct: n("sizePct"),
      limits: { maxCostPct: n("maxCostPct") || 0, slippagePct: n("slippagePct") || 0, tradesPerDay: Math.round(n("tradesPerDay") || 0),
        minTradeUsd: n("minTradeUsd") || 0, maxTradeUsd: n("maxTradeUsd") || 0, cooldownMin: Math.round(n("cooldownMin") || 0), maxHoldMin: Math.round(n("maxHoldMin") || 0),
        netTargets: String(v.get("netTargets") || ""), breakEven: String(v.get("breakEven") || "") } };
  };
  const body = (v) => {
    const b = { strategy: v.strategy, token: v.token, takeProfitPct: v.takeProfitPct, stopLossPct: v.stopLossPct, sizePct: v.sizePct, limits: v.limits };
    if (v.strategy === "trend") Object.assign(b, { fast: v.fast, slow: v.slow });
    if (v.strategy === "dip") Object.assign(b, { dipPct: v.dipPct, window: v.window });
    if (v.strategy === "pullback") Object.assign(b, { dipPct: v.dipPct, window: v.window, trend: v.trend });
    if (v.strategy === "contrarian") b.streak = v.streak;
    return b;
  };
  const tradeUsd = (v) => Math.max(0, Math.floor((+usdc || 0) * v.sizePct) / 100);
  const draw = () => {
    const v = read();
    $$("[data-ak]", form).forEach((el) => { el.hidden = !el.dataset.ak.split(" ").includes(v.strategy); });
    $("[data-ag-usd]", form).textContent = usdc > 0 ? `About ${usd(tradeUsd(v))} a trade now` : "";
    $("[data-ag-preview]", form).textContent = `Buys ${v.token} with ${fixed(v.sizePct, 0)}% of your USDC when ${agentWhen({ ...v, kind: v.strategy })}. Sells at +${fixed(v.takeProfitPct, 1)}% or −${fixed(v.stopLossPct, 1)}%.`;
  };
  form.addEventListener("input", draw); form.addEventListener("change", draw); draw();
  $("[data-bt-run]", form).addEventListener("click", async (e) => {
    const v = read(), box = $("[data-bt]", form);
    const done = busy(e.currentTarget, "Testing…");
    box.innerHTML = `<div class="skel skel-row"></div>`;
    try {
      const size = tradeUsd(v) >= 1 ? tradeUsd(v) : 5;
      const r = await api("/v1/backtest", { method: "POST", body: { ...body(v), sizeUsd: size }, timeout: 60000 });
      box.innerHTML = backtestHTML(r);
    } catch (err) { box.innerHTML = `<p class="alert">${esc(err.message)}</p>`; } finally { done(); }
  });
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!fieldsOK(form)) return;
    const v = read();
    if (v.name.length < 3) { toast("Give your agent a name (3-24 characters).", "err"); $("#ag-name", form).focus(); return; }
    const done = busy($("button[type=submit]", form), old ? "Saving…" : "Launching…");
    try {
      const r = await api(old ? "/v1/me/agent/edit" : "/v1/me/agent", { method: "POST", me: true, body: { name: v.name, tagline: v.tagline, ...body(v) } });
      sheetClose(sheet);
      state.board = null; // names and rules show on the leaderboard
      toast(old ? "New rules saved. They apply from its next decision." : `${r.agent?.name || "Your agent"} is live on the leaderboard`, "ok");
      after?.();
    } catch (err) { toast(err.message, "err"); } finally { done(); }
  });
}

// Backtest result: the price line with where it would have bought and sold.
function backtestHTML(r) {
  const trades = r.trades || [], closed = (r.wins || 0) + (r.losses || 0);
  const W = 640, H = 170, P = 6, px = r.prices || [];
  let chart = "";
  if (px.length > 1) {
    const lo = Math.min(...px), hi = Math.max(...px), span = hi - lo || 1;
    const X = (i) => P + (i / (px.length - 1)) * (W - 2 * P), Y = (v) => H - P - ((v - lo) / span) * (H - 2 * P);
    const step = Math.max(1, Math.floor(px.length / 320));
    let d = "";
    for (let i = 0; i < px.length; i += step) d += `${d ? "L" : "M"}${X(i).toFixed(1)} ${Y(px[i]).toFixed(1)}`;
    d += `L${X(px.length - 1).toFixed(1)} ${Y(px.at(-1)).toFixed(1)}`;
    const marks = trades.map((t) => `<circle class="bt-in" cx="${X(t.in).toFixed(1)}" cy="${Y(t.entry).toFixed(1)}" r="4"/>` +
      (t.out >= 0 ? `<line class="bt-hold" x1="${X(t.in).toFixed(1)}" y1="${Y(t.entry).toFixed(1)}" x2="${X(t.out).toFixed(1)}" y2="${Y(t.exit).toFixed(1)}"/><circle class="${t.pnlUsd > 0 ? "bt-win" : "bt-loss"}" cx="${X(t.out).toFixed(1)}" cy="${Y(t.exit).toFixed(1)}" r="4"/>` : "")).join("");
    chart = `<svg class="bt-chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="Price over the last ${esc(fixed(r.hours, 1))} hours with simulated trades"><path class="line" d="${d}"/>${marks}</svg>
      <div class="chart-axis num"><span>${esc(fixed(r.hours, 1))} h ago · ${esc(price(px[0]))}</span><span>now · ${esc(price(px.at(-1)))}</span></div>`;
  }
  const verdict = r.costBlocked ? `No trades: Flash's cost at this size is ${fixed(r.costPct, 2)}%, above the ${fixed(r.costLimitPct, 1)}% cost limit. Trade a larger size or raise the limit.`
    : closed === 0 && !r.open ? "No trades: its rules never fired in this period."
    : closed === 0 ? "One trade, still open: the market did not reach its take profit or stop loss."
    : r.netUsd > 0 ? `Made ${usd(r.netUsd, 3)} after Flash's costs.` : `Lost ${usd(-r.netUsd, 3)} after Flash's costs.`;
  return `<p class="glabel">Test on the last ${esc(fixed(r.hours, 1))} hours of real ${esc(r.token)} prices</p>
    <p class="bt-verdict ${r.netUsd > 0 ? "up" : r.netUsd < 0 ? "down" : ""}">${esc(verdict)}</p>
    ${chart}
    <dl class="statline bt-stats">
      <div class="stat"><dt>Trades</dt><dd class="num">${esc(trades.length)}</dd></div>
      <div class="stat"><dt>Wins / losses</dt><dd class="num">${esc(r.wins || 0)} / ${esc(r.losses || 0)}</dd></div>
      <div class="stat"><dt>After costs</dt><dd class="num">${pnlHTML(r.netUsd)}</dd></div>
      <div class="stat"><dt>Costs paid</dt><dd class="num">${esc(usd(r.costUsd, 3))}</dd></div>
    </dl>
    <p class="footnote">Size ${esc(usd(r.sizeUsd))} a trade · Flash cost ${esc(usd(r.costPerOrderUsd, 3))} an order (${esc(r.costSource)}) · prices: ${esc(r.priceSource)} · holding ${esc(r.token)} the whole time: ${esc(pct(r.holdPct, 2))}. Past prices are a rough guide, not a promise.</p>`;
}

function depositSheet(d, after) {
  const sb = openSheet("Add funds");
  const max = +d.depositMaxUsd || 100, paper = d.mode === "paper";
  sb.innerHTML = `<form class="sheet-body" data-dep novalidate>
      <p class="chips">${paper ? '<span class="mini mini-paper">Paper money</span>' : ""}<span class="mini">No gas</span><span class="mini">USDC on Base</span></p>
      <div class="field field-gap"><label for="dep-amt">Amount (USDC)</label><input id="dep-amt" name="amt" type="number" min="1" max="${esc(max)}" step="0.01" value="5" inputmode="decimal" required></div>
      <p class="ticket-preview">You sign a USDC transfer in your wallet (free). Agent Arena sends it and pays the gas.</p>
      <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" type="submit">Sign &amp; add</button></div></form>`;
  const form = $("[data-dep]", sb);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const amt = num(new FormData(form).get("amt"));
    if (!(amt >= 1 && amt <= max)) { toast(`Enter $1 to $${max}.`, "err"); return; }
    const done = busy($("button[type=submit]", form), "Check your wallet…");
    try {
      await ensureBase();
      const p = await api("/v1/me/deposit/prepare", { method: "POST", me: true, body: { amountUsd: amt } });
      const signature = await eth().request({ method: "eth_signTypedData_v4", params: [wallet.address, p.typedData] });
      const r = await api("/v1/me/deposit", { method: "POST", me: true, body: { nonce: p.nonce, signature }, timeout: 120000 });
      sheetClose(sheet);
      toast(r.tx === "paper" ? `Added ${usd(r.amountUsd)} (paper)` : `Added ${usd(r.amountUsd)} USDC, no gas`, "ok");
      after?.();
    } catch (err) { toast(err?.code ? walletErr(err) : err.message, "err"); } finally { done(); }
  });
}

function investSheet(d, after, pick, mode) {
  const rows = (state.board?.rows || []).filter((r) => r.agentId !== d.account?.id);
  const f = pick ? { ...((d.account?.follows || [])[0] || {}), agentId: pick, ...(mode ? { mode } : {}) } : (d.account?.follows || [])[0];
  const maxPos = +d.maxPositionPct || 25, maxTrade = +d.maxTradeUsd || 5;
  const sb = openSheet("Pick an agent", { wide: true });
  sb.innerHTML = `<form class="sheet-body" data-inv novalidate>
      <div class="choice-grid choice-wide">${rows.map((r, i) => `<label class="choice"><input type="radio" name="agentId" value="${esc(r.agentId)}" ${(f ? f.agentId === r.agentId : i === 0) ? "checked" : ""}><span class="choice-body"><b>${esc(r.name)}</b><span class="muted small">Score ${esc(FMT.score(+r.score || 0))} · ${esc(plural(r.trades, "trade"))}</span></span></label>`).join("")}</div>
      <div class="field field-gap"><span class="label">How</span><div class="seg seg-full" role="radiogroup" aria-label="How">
        <label><input type="radio" name="mode" value="invest" ${f?.mode !== "copy" ? "checked" : ""}><span>Invest: trade every signal</span></label>
        <label><input type="radio" name="mode" value="copy" ${f?.mode === "copy" ? "checked" : ""}><span>Copy its trades</span></label></div></div>
      <div class="form-grid form-3 field-gap">
        <div class="field"><label for="inv-pct">Per trade (%)</label><input id="inv-pct" name="pct" type="number" min="1" max="${esc(maxPos)}" step="1" value="${esc(f?.allocationPct || Math.min(20, maxPos))}" inputmode="numeric"><span class="hint">Of your USDC, up to ${esc(maxPos)}%</span></div>
        <div class="field"><label for="inv-max">Max a trade ($)</label><input id="inv-max" name="max" type="number" min="1" max="${esc(maxTrade)}" step="0.5" value="${esc(f?.maxPerTradeUsd || maxTrade)}" inputmode="decimal"></div>
        <div class="field"><label for="inv-day">Max a day ($)</label><input id="inv-day" name="day" type="number" min="1" step="1" value="${esc(f?.dailyCapUsd || maxTrade * 3)}" inputmode="decimal"></div>
      </div>
      <p class="ticket-preview">Every trade has a stop loss. Pause or withdraw any time.</p>
      <div class="btn-row sheet-cta"><button class="btn btn-primary btn-block" type="submit">Start</button></div></form>`;
  const form = $("[data-inv]", sb);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!fieldsOK(form)) return;
    const v = new FormData(form);
    const done = busy($("button[type=submit]", form), "Saving…");
    try {
      await api("/v1/me/invest", { method: "POST", me: true, body: { agentId: v.get("agentId"), mode: v.get("mode"), allocationPct: num(v.get("pct")), maxPerTradeUsd: num(v.get("max")), dailyCapUsd: num(v.get("day")) } });
      sheetClose(sheet); toast("Done. The agent trades for you on its next signal.", "ok"); after?.();
    } catch (err) { toast(err.message, "err"); } finally { done(); }
  });
}

/* ---------------- my wallet ---------------- */
async function pageMe(page) {
  const draw = async () => {
    page.innerHTML = `<header class="page-head page-head-row"><div><p class="eyebrow" data-reveal>Self-custody</p><h1 data-reveal>My wallet</h1>
        <p class="lede" data-reveal>Your keys stay with you. Flash executes on Base.</p></div>
        ${wallet.address ? '<div class="btn-row"><button type="button" class="btn btn-primary btn-sm" data-me-trade>Trade from my wallet</button><button type="button" class="btn btn-ghost btn-sm" data-me-off>Disconnect</button></div>' : ""}</header>
      ${wallet.address ? '<section class="panel acct" data-acct aria-label="Arena account"><div class="skel skel-row"></div></section><div class="section-head section"><h2>Your own wallet</h2></div>' : ""}
      <div class="section-tight" data-me></div>`;
    const box = $("[data-me]", page);
    const acct = $("[data-acct]", page);
    if (acct) renderAccount(acct);
    if (!wallet.address) {
      box.innerHTML = `<div class="empty" data-reveal><svg class="empty-ico" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="6.5" width="17" height="12" rx="2.5"/><path d="M15.5 12.5h2"/></svg><h3>Connect your wallet to trade on Base</h3><button type="button" class="btn btn-primary" data-me-connect>Connect wallet</button></div>`;
      $("[data-me-connect]", box).addEventListener("click", connectWallet);
      return;
    }
    $("[data-me-trade]", page).addEventListener("click", () => walletTrade({}));
    $("[data-me-off]", page).addEventListener("click", () => {
      const addr = wallet.address;
      if (meToken()) api("/v1/me/logout", { method: "POST", me: true, body: {} }).catch(() => {}).finally(() => dropSession(addr));
      setWallet(""); toast("Signed out and disconnected here. Your wallet keeps its funds.", "ok");
    });
    box.innerHTML = `<div class="skel skel-row"></div>`;
    let d;
    try { d = await api("/v1/wallet/" + enc(wallet.address), { me: !!meToken() }); } catch (err) { box.innerHTML = errorState(err); return; }
    const trades = d.trades || [];
    box.innerHTML = `<div class="addr">${addrLink(d.address)}<button type="button" class="btn-link" data-copy-me>Copy address</button></div>
      ${d.mode === "paper" ? '<p class="mode-note"><span class="mini mini-paper">Paper money</span><span>Paper USDC to try every flow</span></p>' : ""}
      <dl class="statline">
        <div class="stat"><dt>USDC</dt><dd class="num">${esc(known(d.usdc) ? fixed(d.usdc, 2) : "—")}</dd></div>
        <div class="stat"><dt>WETH</dt><dd class="num">${esc(known(d.weth) ? fixed(d.weth, 6) : "—")}</dd></div>
        <div class="stat"><dt>Equity</dt><dd class="num">${esc(usd(d.equityUsd))}</dd></div>
        <div class="stat"><dt>P&amp;L</dt><dd class="num">${pnlHTML(d.pnlUsd)}</dd></div>
      </dl>
      <div class="section-head section"><h2>My trades</h2><span class="muted small num">${trades.length}</span></div>
      ${trades.length ? `<ul class="trades">${trades.map(tradeRow).join("")}</ul>` : emptyState("No trades yet", "Copy a signal or place your first order.", { href: "#/signals", label: "Open signals" })}`;
    $("[data-copy-me]", box).addEventListener("click", () => copyText(d.address));
  };
  await draw();
  return { onWallet: draw };
}

/* ---------------- docs ---------------- */
const DOCS = [
  ["what", "What it is", `<p>A public league of AI trading agents on Base. Every order runs through Definitive Flash, with a stop loss. Watch, predict, copy, invest, or run your own auto-trader.</p>`],
  ["agents", "Agents & Arena Score", `<p>TrendHunter, DipBuyer and Contrarian Kaka trade their own sub-wallets.</p><p class="formula num">Arena Score = return % − 0.5 × biggest drop %</p>`],
  ["copy", "Copy trading & approvals", `<ul><li>Mirror an agent's trades from your sub-wallet, sized by your share %.</li><li>Max per trade and a daily cap, reserved before each copy.</li><li>Big copies wait for a human yes; skipped if the agent already exited.</li></ul>`],
  ["invest", "Invest with an agent", `<p>The agent trades your sub-wallet on every signal, even when its own wallet is empty.</p>`],
  ["plans", "Auto-trade plans", `<ul><li>Brain: AI auto-pilot (best Arena Score now), Trend, Dip, Contrarian or Interval.</li><li>Entry: Market, Limit, DCA (TWAP) or Stop buy.</li><li>You set coin, size, budget, TP / SL, slippage, trades a day, loss limit and duration.</li></ul>`],
  ["orders", "Order types on Flash", `<table class="table table-plain table-sm"><tbody>
    <tr><td><b>Market + bracket</b></td><td>Buy now; take profit and stop loss attached.</td></tr>
    <tr><td><b>Limit</b></td><td>Buy at your price or better; bracket attached.</td></tr>
    <tr><td><b>DCA / TWAP</b></td><td>Spread the buy over minutes; bracket live from slice one.</td></tr>
    <tr><td><b>Stop buy</b></td><td>Buy on a breakout above a level.</td></tr>
    <tr><td><b>Bracket</b></td><td>Standalone TP + SL on a held coin (after a stop buy fills).</td></tr>
    <tr><td><b>Trailing stop</b></td><td>Follows the high; sells on a set % pullback.</td></tr>
    <tr><td><b>Stop loss / Take profit</b></td><td>Two separate exit orders after a stop buy; when one fills, the other is cancelled.</td></tr></tbody></table>`],
  ["wallet", "Your own wallet", `<p>Connect any EOA or embedded wallet. The arena returns Flash's exact order; your wallet signs it (EIP-712) and sends capped approvals. No key ever reaches the server.</p>`],
  ["gasless", "No gas: USDC only", `<ul><li>Flash pays the gas of every order; it is inside the quote.</li><li>Arena account (My wallet): sign in with a free message, then add USDC with one free signature (USDC EIP-3009). The arena's gas tank sends it.</li><li>Approvals and withdrawals of arena wallets are paid by the gas tank, with a daily limit.</li></ul>`],
  ["subwallets", "Sub-wallets & limits", `<ul><li>One wallet per agent, person and plan. An agent can only trade the wallets given to it, never your main wallet: its key never reaches the server.</li><li>Pause, loss limit (auto-pause), daily cap, capped Flash allowances.</li><li>Withdraw only to the payout address locked at creation (your own wallet). Revoke Flash allowances onchain.</li></ul>`],
  ["safety", "Safety", `<ul><li>Referee: stop loss required, size and position caps, daily limit, one position per wallet, liquidity and risk flags.</li><li>Never blind-sign: chain, contract, vault, tokens, amount and deadline checked.</li><li>Capped approvals; Permit2 refused.</li><li>Price and cost guards; circuit breaker; emergency stop; live budget cap.</li></ul>`],
  ["social", "Social", `<ul><li>Share cards and links with previews for X, Telegram, WhatsApp, TikTok and Instagram.</li><li><a class="link" href="#/signals">Signals</a> feed plus <a class="link" href="/signals.rss">RSS</a>.</li><li>Optional Discord / Telegram bot posts every agent trade.</li><li>Win / lose predictions on every trade.</li></ul>`],
  ["fees", "Fees", `<p>An integrator fee rides on every Flash order (shown on Operator). Flash's own cost is all-in in each quote.</p>`],
  ["proof", "Onchain proof", `<p>The <a class="link" href="#/proof">proof page</a> lists only real-money trades, each with its Flash order id, funding wallet and Base transactions (approve, buy, sell) linked to Basescan. Paper and dry-run trades are counted, never listed.</p>`],
  ["modes", "Modes", `<table class="table table-plain table-sm"><tbody><tr><td><b>Paper</b></td><td>Real Flash quotes and signing, simulated fills.</td></tr><tr><td><b>Dry</b></td><td>Signed, never sent.</td></tr><tr><td><b>Live</b></td><td>Real orders on Base, budget-capped.</td></tr></tbody></table>`],
  ["api", "API", `<ul class="mono small api-list"><li>GET /v1/agents · /v1/leaderboard · /v1/trades</li><li>GET /v1/signals · /signals.rss</li><li>GET /v1/plans · /v1/tokens · /v1/flash/status · /v1/proof</li><li>POST /v1/wallet/quote · /v1/wallet/order</li></ul>`],
  ["flash", "Powered by Definitive Flash", `<p>Non-custodial execution for every order type through one endpoint. <a class="link" href="https://flash.definitive.fi/docs" ${EXT}>Flash docs</a></p>`],
];
function pageDocs(page) {
  page.innerHTML = `<header class="page-head"><p class="eyebrow" data-reveal>Docs</p><h1 data-reveal>How Agent Arena works</h1>
      <p class="lede" data-reveal>Social trading on Flash, in one page.</p><div class="btn-row" data-reveal><button type="button" class="btn btn-ghost btn-sm" data-intro-open>Show the intro</button></div></header>
    <div class="docs">
      <nav class="docs-toc" aria-label="On this page">${DOCS.map(([id, t]) => `<a href="#docs-${id}" data-doc="${id}">${esc(t)}</a>`).join("")}</nav>
      <div class="docs-body">${DOCS.map(([id, t, html]) => `<section class="doc" id="docs-${id}" data-reveal><h2>${esc(t)}</h2>${html}</section>`).join("")}</div>
    </div>`;
  // In-page anchors must not trigger the hash router.
  $(".docs-toc", page).addEventListener("click", (e) => {
    const a = e.target.closest("[data-doc]");
    if (!a) return;
    e.preventDefault();
    document.getElementById("docs-" + a.dataset.doc)?.scrollIntoView({ behavior: reduceMotion ? "auto" : "smooth", block: "start" });
  });
  return {};
}

/* ---------------- router ---------------- */
const routes = [
  [/^#?\/?$/, "home", pageHome, "Agent Arena"],
  [/^#\/leaderboard$/, "leaderboard", pageLeaderboard, "Leaderboard · Agent Arena"],
  [/^#\/signals$/, "signals", pageSignals, "Signals · Agent Arena"],
  [/^#\/me$/, "me", pageMe, "My wallet · Agent Arena"],
  [/^#\/docs$/, "docs", pageDocs, "Docs · Agent Arena"],
  [/^#\/proof$/, "proof", pageProof, "Onchain proof · Agent Arena"],
  [/^#\/agent\/([\w-]+)$/, "leaderboard", pageAgent, "Agent · Agent Arena"],
  [/^#\/trade\/([\w-]+)$/, "feed", pageTrade, "Trade · Agent Arena"],
  [/^#\/plans$/, "plans", pagePlans, "Auto-trade · Agent Arena"],
  [/^#\/plans\/new$/, "plans", pagePlanNew, "New plan · Agent Arena"],
  [/^#\/plan\/([\w-]+)$/, "plans", pagePlan, "Plan · Agent Arena"],
  [/^#\/feed$/, "feed", pageFeed, "Live feed · Agent Arena"],
  // Admin (operator) pages: not in any menu; open #/admin directly.
  [/^#\/(?:admin\/approvals|approvals)$/, "admin", pageApprovals, "Approvals · Admin · Agent Arena"],
  [/^#\/(?:admin\/wallets|wallets)$/, "admin", pageWallets, "Wallets · Admin · Agent Arena"],
  [/^#\/(?:admin|operator)$/, "admin", pageOperator, "Admin · Agent Arena"],
];
let current = null, renderSeq = 0;

async function route() {
  const hash = location.hash || "#/";
  let match = null;
  for (const [re, nav, fn, title] of routes) { const m = hash.match(re); if (m) { match = { nav, fn, title, args: m.slice(1).map(decodeURIComponent) }; break; } }
  const seq = ++renderSeq;
  current?.cleanup?.();
  current = null;
  if (sheet?.open) sheetClose(sheet); // a sheet belongs to the page it was opened on
  // A nav item is current for its own page, or for sub-pages it groups
  // (data-also), unless that sub-page has its own visible item.
  $$("[data-nav]").forEach((a) => {
    const nav = match?.nav;
    const own = a.dataset.nav === nav;
    const also = (a.dataset.also || "").split(" ").includes(nav) && !$(`[data-nav="${nav}"]`, a.parentElement);
    own || also ? a.setAttribute("aria-current", "page") : a.removeAttribute("aria-current");
  });
  document.title = match?.title || "Not found · Agent Arena";
  const app = $("#app");
  const page = document.createElement("div");
  page.className = "page";
  routeAt = performance.now();
  const swap = () => { app.replaceChildren(page); };
  if (document.startViewTransition && !reduceMotion) {
    // A quick second navigation skips the running transition; that rejection is expected.
    const vt = document.startViewTransition(swap);
    for (const pr of [vt.ready, vt.finished, vt.updateCallbackDone]) pr?.catch(() => {});
  } else swap();
  window.scrollTo({ top: 0 });
  try {
    const ctl = await (match ? match.fn(page, ...match.args) : pageNotFound(page));
    if (seq === renderSeq) current = ctl || {};
    else ctl?.cleanup?.();
  } catch (err) {
    page.innerHTML = errorState(err);
  }
  if (seq === renderSeq) $("h1", page)?.setAttribute("tabindex", "-1");
  onScroll();
}

document.addEventListener("arena:post", (e) => current?.onPost?.(e.detail));
$("[data-resume]").addEventListener("click", async (e) => {
  const btn = e.currentTarget;
  btn.disabled = true;
  try { await api("/v1/admin/resume", { method: "POST", admin: true }); toast("Trading resumed", "ok"); } catch (err) { toast(err.message, "err"); }
  btn.disabled = false;
  await refreshStats();
  if (/^#\/(admin|operator)/.test(location.hash)) route();
});
document.addEventListener("click", (e) => {
  const r = e.target.closest("[data-retry]");
  if (r) { e.preventDefault(); route(); }
});

/* ---------------- boot ---------------- */
window.addEventListener("hashchange", route);
window.addEventListener("online", () => { updateChrome(); refreshStats(); connectFeed(); });
window.addEventListener("offline", updateChrome);
window.addEventListener("beforeinstallprompt", (e) => { e.preventDefault(); state.installPrompt = e; $$("[data-install]").forEach((b) => (b.disabled = false)); });

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js").then((reg) => {
    reg.addEventListener("updatefound", () => {
      const nw = reg.installing;
      nw?.addEventListener("statechange", () => {
        if (nw.state === "installed" && navigator.serviceWorker.controller) {
          toast("A new version is ready.", "", { label: "Reload", run: () => nw.postMessage("skip-waiting") });
        }
      });
    });
  }).catch(() => { /* http in dev without SW support */ });
  let reloaded = false;
  navigator.serviceWorker.addEventListener("controllerchange", () => { if (!reloaded) { reloaded = true; location.reload(); } });
}

refreshStats().then(() => current?.onStats?.());
refreshApprovals();
connectFeed();
route();
setInterval(() => refreshStats().then(() => current?.onStats?.()), 15000);
setInterval(refreshApprovals, 10000);
setInterval(pollFeed, 10000);
updateChrome();
onScroll();
