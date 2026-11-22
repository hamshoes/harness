"use strict";
// The session page. Plain DOM building: event text is model and tool output, so it only ever goes
// into the page as text nodes, never as HTML.

// ---------- helpers ----------

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "text") el.textContent = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid)); // strings become text nodes
  }
  return el;
}

function svg(attrs, paths) {
  const NS = "http://www.w3.org/2000/svg";
  const el = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  for (const d of paths) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    el.append(p);
  }
  return el;
}

const $ = (id) => document.getElementById(id);
const enc = encodeURIComponent;
const main = () => $("main");
const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    setConnected(false);
    throw new ApiError(0, "server unreachable");
  }
  setConnected(true);
  let data = null;
  try {
    data = await res.json();
  } catch (e) {
    data = null;
  }
  if (!res.ok) {
    throw new ApiError(res.status, (data && data.error) || res.statusText || "HTTP " + res.status);
  }
  return data;
}

function shortDir(d) {
  return String(d || "").replace(/^\/(home|Users)\/[^/]+(?=\/|$)/, "~");
}

function ago(ts) {
  const t = Date.parse(ts);
  if (isNaN(t)) return "";
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 45) return "now";
  if (s < 3600) return Math.round(s / 60) + "m";
  if (s < 86400) return Math.round(s / 3600) + "h";
  if (s < 2 * 86400) return "yesterday";
  return Math.floor(s / 86400) + "d";
}

function fmtDuration(ms) {
  if (typeof ms !== "number") return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  return Math.floor(s / 60) + "m " + String(s % 60).padStart(2, "0") + "s";
}

function fmtCost(c) {
  return typeof c === "number" ? "$" + c.toFixed(2) : "—";
}

function fmtTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return "—";
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function oneLine(s) {
  return String(s).replace(/\s+/g, " ").trim();
}

function clip(s, n) {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

// statusOf is the pill's status: running with a pending approval shows as waiting.
function statusOf(s) {
  if (s.status === "running" && s.waiting_on && s.waiting_on.length) return "waiting";
  return s.status;
}

function pillEl(s) {
  const st = statusOf(s);
  return h("span", { class: "pill " + st, text: st.charAt(0).toUpperCase() + st.slice(1) });
}

function isBlob(v) {
  return v && typeof v === "object" && typeof v.blob === "string" && /^sha256:[0-9a-f]{64}$/.test(v.blob);
}

function blobLink(v) {
  return h("a", { class: "blob-link", href: "/blobs/" + v.blob.slice(7), target: "_blank", rel: "noopener", text: "large output" });
}

// val turns an event value into page content: text, or a link for a blob.
function val(v) {
  if (isBlob(v)) return blobLink(v);
  if (v == null) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

function toolSummary(d) {
  const inp = d.input;
  if (isBlob(inp)) return blobLink(inp);
  if (inp && typeof inp === "object") {
    const pick = d.name === "Bash" ? inp.command : inp.file_path != null ? inp.file_path : inp.notebook_path;
    if (pick != null) return isBlob(pick) ? blobLink(pick) : clip(oneLine(pick), 300);
  }
  let s;
  try {
    s = JSON.stringify(inp);
  } catch (e) {
    s = String(inp);
  }
  return clip(oneLine(s == null ? "" : s), 300);
}

function warnIcon() {
  return svg(
    { width: "20", height: "20", viewBox: "0 0 20 20", fill: "none", stroke: "#F2B544", "stroke-width": "1.8", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" },
    ["M10 2.5 18 17H2z", "M10 8v4M10 14.5v.01"],
  );
}

const DECISIONS = {
  allow: ["allowed", "res-ok"],
  deny: ["denied", "res-error"],
  cancelled: ["cancelled", "res-muted"],
  withdrawn: ["withdrawn", "res-muted"],
};

function setRes(el, text, cls) {
  el.textContent = text;
  el.className = "tool-res " + cls;
}

function note(text) {
  return h("div", { class: "ev-note", text });
}

// Every event type the server writes. EventSource only delivers named events to listeners for that
// name, so each one is registered.
const EVENT_TYPES = [
  "user_message", "turn_started", "claude_init", "assistant_text", "thinking", "tool_use", "tool_result",
  "result", "raw", "stderr", "stderr_truncated", "turn_ended", "turn_interrupted", "session_stopped",
  "dir_created", "approval_pending", "approval_resolved",
];
const REFRESH_ON = new Set(["turn_started", "turn_ended", "approval_pending", "approval_resolved", "session_stopped"]);

// ---------- app state ----------

const S = {
  sessions: null, // null until the first load
  filter: "all",
  route: { view: "auto" },
  prevHash: "",
  connected: true,
  health: null,
  view: null, // the open SessionView or NewView
};

function setConnected(ok) {
  if (S.connected === ok) return;
  S.connected = ok;
  renderHealth();
}

// ---------- sidebar ----------

const FILTERS = [["all", "All"], ["active", "Active"], ["idle", "Idle"], ["ended", "Ended"]];

function inFilter(s, f) {
  switch (f) {
    case "active": return s.status === "running";
    case "idle": return s.status === "idle";
    case "ended": return s.status === "failed" || s.status === "stopped";
    default: return true;
  }
}

function renderFilters() {
  $("filters").replaceChildren(...FILTERS.map(([id, label]) =>
    h("button", {
      type: "button",
      class: "chip" + (S.filter === id ? " on" : ""),
      onclick: () => { S.filter = id; renderFilters(); renderList(); },
    }, label)));
}

function renderList() {
  const list = $("list");
  if (S.sessions === null) {
    list.replaceChildren(h("div", { class: "list-empty", text: "Loading…" }));
    return;
  }
  const cur = S.route.view === "session" ? S.route.name : null;
  const shown = S.sessions.filter((s) => inFilter(s, S.filter));
  if (!shown.length) {
    list.replaceChildren(h("div", { class: "list-empty", text: S.sessions.length ? "Nothing here." : "No sessions yet." }));
    return;
  }
  list.replaceChildren(...shown.map((s) =>
    h("button", {
      type: "button",
      class: "row" + (s.name === cur ? " on" : ""),
      onclick: () => { location.hash = "#/s/" + enc(s.name); },
    },
    h("span", { class: "row-top" }, h("span", { class: "row-name", text: s.name }), pillEl(s)),
    s.last_text ? h("span", { class: "row-last", text: s.last_text }) : null,
    h("span", { class: "row-meta", text: shortDir(s.dir) + " · " + ago(s.updated_at) }))));
}

function renderHealth() {
  $("dot").classList.toggle("down", !S.connected);
  if (!S.connected) {
    $("health").textContent = "disconnected";
    return;
  }
  const hz = S.health;
  if (!hz) return;
  const running = (hz.sessions && hz.sessions.running) || 0;
  $("health").textContent = running + " running · " + (hz.pending_approvals || 0) + " waiting";
}

async function pollHealth() {
  try {
    S.health = await api("GET", "/healthz");
  } catch (e) {
    // shown as disconnected
  }
  renderHealth();
}

async function pollList() {
  try {
    S.sessions = await api("GET", "/sessions");
  } catch (e) {
    return;
  }
  renderList();
  const v = S.view;
  if (v && v.kind === "session") {
    const s = S.sessions.find((x) => x.name === v.name);
    if (s) v.setSession(s);
  }
  if (v && v.kind === "new") v.updateDirs();
  if (S.route.view === "auto") autoPick();
}

// recentDirs are the distinct session dirs, newest first, at most 5.
function recentDirs() {
  const out = [];
  for (const s of S.sessions || []) {
    if (!out.includes(s.dir)) out.push(s.dir);
    if (out.length === 5) break;
  }
  return out;
}

// ---------- routing ----------

function parseRoute() {
  const hash = location.hash;
  if (hash === "#/new") return { view: "new" };
  const m = hash.match(/^#\/s\/(.+)$/);
  if (m) {
    try {
      return { view: "session", name: decodeURIComponent(m[1]) };
    } catch (e) {
      return { view: "auto" };
    }
  }
  return { view: "auto" };
}

let lastHash = location.hash;
window.addEventListener("hashchange", () => {
  S.prevHash = lastHash;
  lastHash = location.hash;
  route();
});

function route() {
  S.route = parseRoute();
  $("new-btn").classList.toggle("on", S.route.view === "new");
  if (S.route.view === "session") showSession(S.route.name);
  else if (S.route.view === "new") showNew();
  else autoPick();
  renderList();
}

function closeView() {
  if (S.view) S.view.close();
  S.view = null;
}

function emptyState(text) {
  closeView();
  main().replaceChildren(h("div", { class: "empty" }, h("span", { text }), h("a", { href: "#/new", text: "Start a session" })));
}

// autoPick opens the newest running session, else the newest; with none, an empty state.
function autoPick() {
  if (S.sessions === null) {
    if (!S.view) main().replaceChildren(h("div", { class: "empty", text: "Loading…" }));
    return;
  }
  if (!S.sessions.length) {
    emptyState("No sessions yet.");
    return;
  }
  const pick = S.sessions.find((s) => s.status === "running") || S.sessions[0];
  location.replace("#/s/" + enc(pick.name));
}

function showSession(name) {
  if (S.view && S.view.kind === "session" && S.view.name === name) return;
  closeView();
  S.view = new SessionView(name);
}

function showNew() {
  if (S.view && S.view.kind === "new") return;
  closeView();
  S.view = new NewView();
}

// ---------- session view ----------

class SessionView {
  constructor(name) {
    this.kind = "session";
    this.name = name;
    this.session = null;
    this.events = [];
    this.lastSeq = 0;
    this.raw = false;
    this.follow = true;
    this.sending = false;
    this.closed = false;
    this.tools = new Map(); // tool_use id → {res, done}
    this.asks = new Map(); // ask_id → {card, title, tool}
    this.build();
    this.load();
    this.connect();
  }

  build() {
    const E = (this.el = {});
    E.pill = h("span", { class: "pill" });
    E.meta = h("span", { class: "head-meta" });
    E.rawBtn = h("button", { class: "btn-raw", type: "button", title: "Show every event", onclick: () => this.toggleRaw() }, "Raw");
    E.stop = h("button", { class: "btn-stop", type: "button", onclick: () => this.stopClick() }, "Stop session");
    const head = h("header", { class: "head" },
      h("h1", { text: this.name }), E.pill, E.meta, h("div", { class: "head-actions" }, E.rawBtn, E.stop));

    E.inner = h("div", { class: "log-inner" });
    E.log = h("section", { class: "log", "aria-label": "Event log", onscroll: () => this.onScroll() }, E.inner);
    E.jump = h("button", { class: "jump", type: "button", hidden: true, onclick: () => this.jumpToLatest() }, "Jump to latest");
    E.details = h("aside", { class: "details", "aria-label": "Session details" });
    const body = h("div", { class: "body" }, h("div", { class: "log-wrap" }, E.log, E.jump), E.details);

    E.text = h("textarea", {
      id: "reply",
      rows: 2,
      onkeydown: (e) => {
        if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
          e.preventDefault();
          this.send();
        }
      },
    });
    E.send = h("button", { class: "btn-primary", type: "button", onclick: () => this.send() }, "Send");
    E.err = h("div", { class: "form-error", role: "alert" });
    const form = h("form", { class: "reply", onsubmit: (e) => { e.preventDefault(); this.send(); } },
      h("div", { class: "reply-inner" },
        h("label", { for: "reply", class: "sr-only" }, "Reply"),
        E.text,
        h("div", { class: "reply-side" }, E.send, h("span", { class: "kbd", text: isMac ? "⌘↵" : "Ctrl+↵" }))),
      E.err);
    main().replaceChildren(head, body, form);
    this.updateReply();
  }

  close() {
    this.closed = true;
    if (this.es) this.es.close();
    clearTimeout(this.stopTimer);
    clearTimeout(this.refreshTimer);
  }

  async load() {
    try {
      const s = await api("GET", "/sessions/" + enc(this.name));
      if (!this.closed) this.setSession(s);
    } catch (e) {
      if (!this.closed && e.status === 404) emptyState("No session named " + this.name + ".");
    }
  }

  refreshSoon() {
    clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(() => this.load(), 150);
  }

  setSession(s) {
    this.session = s;
    const E = this.el;
    const pill = pillEl(s);
    E.pill.className = pill.className;
    E.pill.textContent = pill.textContent;
    E.meta.textContent = shortDir(s.dir) + " · turn " + s.turns;
    E.meta.title = s.dir;
    if (!E.stop.classList.contains("confirm")) E.stop.disabled = s.status === "stopped";
    this.renderDetails();
    this.updateReply();
  }

  renderDetails() {
    const s = this.session;
    const kv = (k, v, title) => h("div", { class: "kv" }, h("span", { text: k }), h("span", { text: v, title: title || v }));
    const waiting = s.waiting_on || [];
    const lr = s.last_result;
    this.el.details.replaceChildren(
      h("section", null, h("h2", { text: "Session" }),
        kv("dir", shortDir(s.dir), s.dir),
        kv("turns", String(s.turns)),
        kv("seq", String(s.last_seq)),
        kv("claude id", s.claude_session_id || "—"),
        kv("created", fmtTime(s.created_at))),
      h("section", null, h("h2", { text: "Waiting on" }),
        waiting.length
          ? waiting.map((w) => h("div", { class: "wait-item", text: w.title + " · " + ago(w.since) }))
          : h("span", { class: "wait-none", text: "nothing" })),
      h("section", null, h("h2", { text: "Last turn" }),
        kv("duration", lr ? fmtDuration(lr.duration_ms) : "—"),
        kv("cost", lr ? fmtCost(lr.total_cost_usd) : "—")));
  }

  updateReply() {
    const st = this.session ? this.session.status : null;
    const can = st === "idle" || st === "failed";
    const E = this.el;
    E.text.disabled = !can;
    E.text.placeholder = st === "stopped" ? "Session stopped"
      : can ? "Reply to " + this.name
      : st === null ? ""
      : "Turn running — reply when it's idle";
    E.send.disabled = !can || this.sending;
  }

  async send() {
    const E = this.el;
    const text = E.text.value;
    if (!text.trim() || E.send.disabled) return;
    this.sending = true;
    this.updateReply();
    E.err.textContent = "";
    try {
      const s = await api("POST", "/sessions/" + enc(this.name) + "/messages", { text });
      if (this.closed) return;
      E.text.value = "";
      this.setSession(s);
      this.follow = true;
      this.scrollToEnd();
    } catch (e) {
      if (!this.closed) E.err.textContent = e.message;
    } finally {
      this.sending = false;
      if (!this.closed) this.updateReply();
    }
  }

  stopClick() {
    const b = this.el.stop;
    if (!b.classList.contains("confirm")) {
      b.classList.add("confirm");
      b.textContent = "Confirm stop";
      this.stopTimer = setTimeout(() => this.resetStop(), 4000);
      return;
    }
    this.resetStop();
    b.disabled = true;
    api("POST", "/sessions/" + enc(this.name) + "/stop")
      .then((s) => { if (!this.closed) this.setSession(s); })
      .catch((e) => {
        if (this.closed) return;
        this.el.err.textContent = e.message;
        b.disabled = false;
      });
  }

  resetStop() {
    clearTimeout(this.stopTimer);
    this.el.stop.classList.remove("confirm");
    this.el.stop.textContent = "Stop session";
    if (this.session) this.el.stop.disabled = this.session.status === "stopped";
  }

  // ----- the event stream -----

  connect() {
    // From 0 every time; EventSource sends Last-Event-ID itself when it reconnects.
    const es = new EventSource("/sessions/" + enc(this.name) + "/events?after=0");
    this.es = es;
    const on = (e) => this.onEvent(e);
    for (const t of EVENT_TYPES) es.addEventListener(t, on);
    es.onmessage = on;
  }

  onEvent(e) {
    if (this.closed) return;
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch (err) {
      return;
    }
    if (typeof ev.seq !== "number" || ev.seq <= this.lastSeq) return;
    this.lastSeq = ev.seq;
    this.events.push(ev);
    this.renderEvent(ev);
    if (REFRESH_ON.has(ev.type)) this.refreshSoon();
    if (ev.type === "session_stopped") this.es.close();
  }

  append(el) {
    this.el.inner.append(el);
    if (this.follow) this.scrollToEnd();
    else this.el.jump.hidden = false;
  }

  // openTool is the newest tool row still waiting for its result, preferring Bash (what agentic holds).
  openTool() {
    let any = null;
    let bash = null;
    for (const t of this.tools.values()) {
      if (t.done) continue;
      any = t;
      if (t.name === "Bash") bash = t;
    }
    return bash || any;
  }

  renderEvent(ev) {
    if (this.raw) {
      this.append(h("div", { class: "raw-line" },
        h("span", { class: "raw-seq", text: String(ev.seq) }), " ",
        h("span", { class: "raw-type", text: ev.type }), " ",
        JSON.stringify(ev.data)));
      return;
    }
    const d = ev.data || {};
    switch (ev.type) {
      case "user_message":
        this.append(h("div", { class: "ev-user" }, val(d.text)));
        return;
      case "assistant_text":
        this.append(h("p", { class: "ev-text" }, val(d.text)));
        return;
      case "tool_use": {
        const res = h("span", { class: "tool-res res-muted", text: "…" });
        this.tools.set(d.id, { res, done: false, name: d.name });
        this.append(h("div", { class: "tool" },
          h("span", { class: "tool-name", text: String(d.name == null ? "tool" : d.name) }),
          h("span", { class: "tool-sum" }, toolSummary(d)),
          res));
        return;
      }
      case "tool_result": {
        const t = this.tools.get(d.tool_use_id);
        if (!t) return;
        t.done = true;
        if (d.is_error) setRes(t.res, "error", "res-error");
        else setRes(t.res, "ok", "res-ok");
        return;
      }
      case "approval_pending": {
        const title = String(d.title == null ? "approval" : d.title);
        const card = h("section", { class: "ask", "aria-label": "Approval" }, warnIcon(),
          h("div", { class: "ask-body" },
            h("span", { class: "ask-title", text: title }),
            d.detail ? h("span", { class: "ask-detail" }, val(d.detail)) : null,
            h("span", { class: "ask-hint", text: "Answer in the Mac dialog." })));
        const tool = this.openTool();
        if (tool) setRes(tool.res, "held", "res-held");
        this.asks.set(d.ask_id, { card, title, tool });
        this.append(card);
        return;
      }
      case "approval_resolved": {
        const a = this.asks.get(d.ask_id);
        const [label, cls] = DECISIONS[d.decision] || [String(d.decision), "res-muted"];
        const row = h("div", { class: "tool" },
          h("span", { class: "tool-name", text: "ask" }),
          h("span", { class: "tool-sum", text: a ? a.title : String(d.ask_id) }),
          h("span", { class: "tool-res " + cls, text: label }));
        if (!a) {
          this.append(row);
          return;
        }
        a.card.replaceWith(row);
        if (a.tool && !a.tool.done) setRes(a.tool.res, "…", "res-muted");
        this.asks.delete(d.ask_id);
        return;
      }
      case "turn_ended":
        if (d.status === "failed") this.append(h("div", { class: "tool ev-failed", text: "turn failed (exit " + d.exit_code + ")" }));
        return;
      case "turn_interrupted":
        this.append(note("turn " + d.turn + " interrupted" + (d.reason ? " (" + d.reason + ")" : "")));
        return;
      case "dir_created":
        this.append(note("created " + shortDir(d.dir)));
        return;
      case "session_stopped":
        this.append(note("session stopped"));
        return;
      default:
        // claude_init, thinking, result, turn_started, stderr, raw, …: hidden unless Raw
    }
  }

  toggleRaw() {
    this.raw = !this.raw;
    this.el.rawBtn.classList.toggle("on", this.raw);
    this.tools.clear();
    this.asks.clear();
    this.el.inner.replaceChildren();
    for (const ev of this.events) this.renderEvent(ev);
    this.follow = true;
    this.scrollToEnd();
  }

  // ----- scrolling: follow the bottom until the user scrolls up -----

  onScroll() {
    const L = this.el.log;
    const atBottom = L.scrollHeight - L.scrollTop - L.clientHeight < 40;
    this.follow = atBottom;
    this.el.jump.hidden = atBottom;
  }

  scrollToEnd() {
    if (this.scrollPending) return;
    this.scrollPending = true;
    requestAnimationFrame(() => {
      this.scrollPending = false;
      if (this.closed) return;
      const L = this.el.log;
      L.scrollTop = L.scrollHeight;
      this.el.jump.hidden = true;
    });
  }

  jumpToLatest() {
    this.follow = true;
    this.scrollToEnd();
  }
}

// ---------- new session view ----------

class NewView {
  constructor() {
    this.kind = "new";
    main().replaceChildren(h("div", { class: "empty", text: "New session: coming next." }));
  }

  updateDirs() {}

  close() {}
}

// ---------- start ----------

renderFilters();
renderList();
route();
pollList();
pollHealth();
setInterval(pollList, 3000);
setInterval(pollHealth, 5000);
