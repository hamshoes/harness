"use strict";
// Crews: the Crew mode of the new-session form, and the crew view (members and the message timeline).
// Loaded before app.js and uses its helpers (h, api, S, …) only once constructed.

const CREW_RE = /^[a-z0-9][a-z0-9-]{0,23}$/;
const CREW_HINT = "a–z, 0–9, dashes, at most 24. Members are named <crew>.<member>.";
const COLLAPSE_LINES = 6;

// ---------- the Crew mode of the new-session form ----------

class CrewForm {
  constructor(view) {
    this.view = view;
    this.closed = false;
    this.busy = false;
    this.dirTouched = false;
    this.agents = null; // null while loading
    this.agentsErr = "";
    this.picks = new Map(); // agent name → {on, count}
    this.lead = null; // agent name
    this.loadSeq = 0;
    const E = (this.el = {});
    const submitKey = (e) => {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        this.submit();
      }
    };
    E.name = h("input", { id: "crew-name", class: "field", autocomplete: "off", spellcheck: "false", oninput: () => this.checkName(), onkeydown: submitKey });
    E.nameMsg = h("span", { class: "field-hint", text: CREW_HINT });
    E.dir = h("input", {
      id: "crew-dir", class: "field", autocomplete: "off", spellcheck: "false",
      oninput: () => { this.dirTouched = true; this.tree.dirChanged(); this.loadAgentsSoon(); },
    });
    this.tree = new DirTree({
      getDir: () => E.dir.value,
      setDir: (p) => { E.dir.value = p; this.dirTouched = true; this.loadAgentsSoon(); },
    });
    E.goal = h("textarea", { id: "crew-goal", class: "field field-text", rows: 6, onkeydown: submitKey });
    E.agents = h("div", { class: "agents", role: "group", "aria-label": "Agents" });
    E.maxTurns = h("input", { id: "crew-max", class: "field field-num", type: "number", min: "1", max: "1000", value: "60", onkeydown: submitKey });
    E.deploy = h("button", { type: "submit", class: "btn-start" }, "Deploy crew");
    E.err = h("div", { class: "form-error", role: "alert" });
    this.form = h("form", { class: "new-form", onsubmit: (e) => { e.preventDefault(); this.submit(); } },
      h("div", { class: "new-head" },
        h("h1", { text: "New crew" }),
        h("p", { text: "Agents deployed together in one directory, messaging each other. The goal goes to the lead." })),
      h("div", { class: "new-field" }, h("label", { for: "crew-name", class: "lbl" }, "Crew name"), E.name, E.nameMsg),
      h("div", { class: "new-field" }, h("label", { for: "crew-dir", class: "lbl" }, "Directory"), E.dir,
        h("span", { class: "field-hint", text: "Created if it doesn't exist. Every member works here." }),
        this.tree.el),
      h("div", { class: "new-field" }, h("label", { for: "crew-goal", class: "lbl" }, "Goal"), E.goal),
      h("div", { class: "new-field" }, h("span", { class: "lbl" }, "Agents"), E.agents),
      h("details", { class: "more" }, h("summary", { text: "More" }),
        h("div", { class: "new-field" }, h("label", { for: "crew-max", class: "lbl" }, "Max turns"), E.maxTurns,
          h("span", { class: "field-hint", text: "Across all members. The crew stops itself when they're used." }))),
      h("div", { class: "new-actions" },
        E.deploy,
        h("button", { type: "button", class: "btn-cancel", onclick: () => view.cancel() }, "Cancel"),
        h("span", { class: "kbd new-kbd", text: (isMac ? "⌘↵" : "Ctrl+↵") + " to deploy" })),
      E.err);
    this.updateDirs();
    this.loadAgents();
  }

  close() {
    this.closed = true;
    clearTimeout(this.agentsTimer);
  }

  focus() {
    this.el.name.focus();
  }

  updateDirs() {
    const dirs = recentDirs();
    if (!this.dirTouched && !this.el.dir.value && dirs.length) {
      this.el.dir.value = dirs[0];
      this.tree.dirChanged();
      this.loadAgentsSoon();
    }
  }

  checkName() {
    const v = this.el.name.value.trim();
    const bad = v !== "" && !CREW_RE.test(v);
    this.el.nameMsg.textContent = bad ? "Lowercase a–z, 0–9 and dashes, starting with a letter or digit, at most 24." : CREW_HINT;
    this.el.nameMsg.classList.toggle("bad", bad);
    return !bad && v !== "";
  }

  loadAgentsSoon() {
    clearTimeout(this.agentsTimer);
    this.agentsTimer = setTimeout(() => this.loadAgents(), 300);
  }

  async loadAgents() {
    const seq = ++this.loadSeq;
    const dir = this.el.dir.value.trim();
    let list;
    try {
      list = await api("GET", "/agents" + (dir ? "?dir=" + enc(dir) : ""));
      this.agentsErr = "";
    } catch (e) {
      list = [];
      this.agentsErr = e.message;
    }
    if (this.closed || seq !== this.loadSeq) return;
    this.agents = list;
    if (this.lead && !this.isOn(this.lead)) this.lead = null;
    this.renderAgents();
  }

  isOn(name) {
    const p = this.picks.get(name);
    return !!(p && p.on && (this.agents || []).some((a) => a.name === name));
  }

  pick(name) {
    let p = this.picks.get(name);
    if (!p) {
      p = { on: false, count: 1 };
      this.picks.set(name, p);
    }
    return p;
  }

  // checked lists the ticked agents in list order; the lead defaults to the first.
  checked() {
    return (this.agents || []).filter((a) => this.isOn(a.name));
  }

  renderAgents() {
    const E = this.el;
    if (this.agents === null) {
      E.agents.replaceChildren(h("div", { class: "agents-empty", text: "Loading agents…" }));
      return;
    }
    if (!this.agents.length) {
      E.agents.replaceChildren(h("div", { class: "agents-empty" },
        this.agentsErr ? "Couldn't load agents: " + this.agentsErr + ". " : "No agents yet. ",
        "Put Claude Code subagent files (markdown with name, description, model, tools frontmatter) in ",
        h("code", { text: "~/.config/harness/agents/" }), " or ", h("code", { text: "<dir>/.claude/agents/" }), "."));
      return;
    }
    const on = this.checked();
    if (!this.lead && on.length) this.lead = on[0].name;
    E.agents.replaceChildren(...this.agents.map((a) => {
      const p = this.pick(a.name);
      const id = "agent-" + a.name;
      const box = h("input", {
        type: "checkbox", id, checked: p.on,
        onchange: () => {
          p.on = box.checked;
          if (!p.on && this.lead === a.name) this.lead = null;
          this.renderAgents();
        },
      });
      const lead = h("input", {
        type: "radio", name: "crew-lead", checked: p.on && this.lead === a.name, disabled: !p.on,
        "aria-label": "Lead: " + a.name,
        onchange: () => { this.lead = a.name; },
      });
      const count = h("input", {
        type: "number", class: "agent-count", min: "1", max: "3", value: String(p.count), disabled: !p.on,
        "aria-label": "How many " + a.name,
        oninput: () => {
          const n = parseInt(count.value, 10);
          if (n >= 1 && n <= 3) p.count = n;
        },
      });
      return h("div", { class: "agent-row" + (p.on ? " on" : "") },
        box,
        h("label", { for: id, class: "agent-main" },
          h("span", { class: "agent-top" },
            h("span", { class: "agent-name", text: a.name }),
            a.model ? h("span", { class: "agent-model", text: a.model }) : null,
            h("span", { class: "agent-src " + a.source, text: a.source })),
          a.description ? h("span", { class: "agent-desc", text: a.description }) : null),
        h("label", { class: "agent-lead", title: "The goal goes to the lead" }, lead, "Lead"),
        count);
    }));
  }

  async submit() {
    if (this.busy) return;
    const E = this.el;
    const name = E.name.value.trim();
    const dir = E.dir.value.trim();
    const goal = E.goal.value;
    E.err.textContent = "";
    if (!this.checkName()) {
      E.err.textContent = name ? "Fix the crew name first." : "Crew name is required.";
      E.name.focus();
      return;
    }
    if (!dir) {
      E.err.textContent = "Directory is required.";
      E.dir.focus();
      return;
    }
    if (!goal.trim()) {
      E.err.textContent = "Write the goal.";
      E.goal.focus();
      return;
    }
    const on = this.checked();
    if (!on.length) {
      E.err.textContent = "Tick at least one agent.";
      return;
    }
    const lead = this.lead && this.isOn(this.lead) ? this.lead : on[0].name;
    const members = [];
    for (const a of on) {
      const n = Math.min(3, Math.max(1, this.pick(a.name).count || 1));
      for (let i = 0; i < n; i++) members.push({ agent: a.name });
    }
    const maxTurns = parseInt(E.maxTurns.value, 10);
    if (!(maxTurns >= 1 && maxTurns <= 1000)) {
      E.err.textContent = "Max turns must be 1 to 1000.";
      return;
    }
    this.busy = true;
    E.deploy.disabled = true;
    try {
      // The first member of an agent is named after it, so the lead is the agent's name.
      const c = await api("POST", "/crews", { name, dir, goal, lead, members, max_turns: maxTurns });
      if (this.closed) return;
      pollList();
      location.hash = "#/c/" + enc(c.name);
    } catch (e) {
      if (!this.closed) E.err.textContent = e.message;
    } finally {
      this.busy = false;
      if (!this.closed) E.deploy.disabled = false;
    }
  }
}

// ---------- the crew view ----------

class CrewView {
  constructor(name) {
    this.kind = "crew";
    this.name = name;
    this.crew = null;
    this.closed = false;
    this.seen = ""; // members' last_seq, to refetch the timeline only when something happened
    this.expanded = new Set(); // message keys shown in full
    this.follow = true;
    const E = (this.el = {});
    E.pill = h("span", { class: "pill" });
    E.meta = h("span", { class: "head-meta" });
    E.stop = h("button", { class: "btn-stop", type: "button", onclick: () => this.stopClick() }, "Stop crew");
    const head = h("header", { class: "head" }, h("h1", { text: this.name }), E.pill, E.meta, h("div", { class: "head-actions" }, E.stop));
    E.chips = h("div", { class: "member-chips", role: "list", "aria-label": "Members" });
    E.inner = h("div", { class: "log-inner" });
    E.log = h("section", { class: "log", "aria-label": "Crew messages", onscroll: () => this.onScroll() }, E.inner);
    E.err = h("div", { class: "form-error crew-err", role: "alert" });
    main().replaceChildren(head, h("div", { class: "crew-bar" }, E.chips), h("div", { class: "body" }, h("div", { class: "log-wrap" }, E.log)), E.err);
    E.inner.append(h("div", { class: "ev-note", text: "Loading…" }));
    this.load();
  }

  close() {
    this.closed = true;
    clearTimeout(this.stopTimer);
  }

  async load() {
    try {
      const c = await api("GET", "/crews/" + enc(this.name));
      if (!this.closed) this.setCrew(c);
    } catch (e) {
      if (!this.closed && e.status === 404) emptyState("No crew named " + this.name + ".");
    }
  }

  setCrew(c) {
    this.crew = c;
    const E = this.el;
    const pill = pillEl(crewStatus(c));
    E.pill.className = pill.className;
    E.pill.textContent = pill.textContent;
    E.meta.textContent = shortDir(c.dir) + " · " + c.turns + " / " + c.max_turns + " turns";
    E.meta.title = c.dir;
    if (!E.stop.classList.contains("confirm")) E.stop.disabled = c.status === "stopped";
    E.chips.replaceChildren(...c.members.map((m) => h("a", {
      class: "member-chip", role: "listitem", href: "#/s/" + enc(m.name), title: m.agent + (m.description ? ": " + m.description : ""),
    }, h("span", { class: "member-name", text: m.member + (m.member === c.lead ? " ★" : "") }), pillEl(m))));
    const seen = c.members.map((m) => m.last_seq).join(",");
    if (seen !== this.seen) {
      this.seen = seen;
      this.loadMessages();
    }
  }

  async loadMessages() {
    let ms;
    try {
      ms = await api("GET", "/crews/" + enc(this.name) + "/messages");
    } catch (e) {
      return;
    }
    if (this.closed) return;
    this.renderMessages(ms);
  }

  renderMessages(ms) {
    const E = this.el;
    if (!ms.length) {
      E.inner.replaceChildren(h("div", { class: "ev-note", text: "No messages yet." }));
      return;
    }
    E.inner.replaceChildren(...ms.map((m) => {
      const key = m.session + ":" + m.seq;
      const text = h("div", { class: "msg-text" }, val(m.text));
      const msg = h("article", { class: "msg" + (m.from === "sam" ? " from-sam" : "") },
        h("div", { class: "msg-head" },
          h("span", { class: "msg-route", text: m.from + " → " + m.to }),
          h("time", { class: "msg-time", datetime: m.at, title: m.at, text: fmtTime(m.at) })),
        text);
      if (typeof m.text === "string" && m.text.split("\n").length > COLLAPSE_LINES || typeof m.text === "string" && m.text.length > 900) {
        const full = this.expanded.has(key);
        text.classList.toggle("clamped", !full);
        msg.append(h("button", {
          type: "button", class: "msg-more",
          onclick: () => {
            if (this.expanded.has(key)) this.expanded.delete(key);
            else this.expanded.add(key);
            this.renderMessages(ms);
          },
        }, full ? "Show less" : "Show more"));
      }
      return msg;
    }));
    if (this.follow) requestAnimationFrame(() => { if (!this.closed) E.log.scrollTop = E.log.scrollHeight; });
  }

  onScroll() {
    const L = this.el.log;
    this.follow = L.scrollHeight - L.scrollTop - L.clientHeight < 40;
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
    b.textContent = "Stopping…";
    api("POST", "/crews/" + enc(this.name) + "/stop")
      .then((c) => { if (!this.closed) { this.resetStop(); this.setCrew(c); pollList(); } })
      .catch((e) => {
        if (this.closed) return;
        this.el.err.textContent = e.message;
        this.resetStop();
      });
  }

  resetStop() {
    clearTimeout(this.stopTimer);
    this.el.stop.classList.remove("confirm");
    this.el.stop.textContent = "Stop crew";
    if (this.crew) this.el.stop.disabled = this.crew.status === "stopped";
  }
}
