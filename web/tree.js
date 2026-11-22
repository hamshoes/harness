"use strict";
// The folder tree under the new-session Directory field: Recent, then each root, folders loaded on
// expand from /fs/ls. Uses h() and api() from app.js (called only after it has loaded).

const NEW_FOLDER_RE = /^[^/\0]+$/;

function chevronIcon() {
  return svg({ width: "12", height: "12", viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": "2", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" }, ["M6 3.5 10.5 8 6 12.5"]);
}

class DirTree {
  // opts.getDir() returns the Directory field's value; opts.setDir(path) fills it.
  constructor(opts) {
    this.opts = opts;
    this.nodes = []; // top level: section nodes
    this.cursor = null; // keyboard position
    this.selected = null; // the row matching the Directory field
    this.adding = null; // node the new-folder field is under
    this.filter = "";
    this.byPath = new Map();
    this.filterEl = h("input", {
      class: "field tree-filter", placeholder: "Filter folders", autocomplete: "off", spellcheck: "false", "aria-label": "Filter folders",
      oninput: () => { this.filter = this.filterEl.value.trim().toLowerCase(); this.render(); },
      onkeydown: (e) => { if (e.key === "ArrowDown") { e.preventDefault(); this.treeEl.focus(); } },
    });
    this.treeEl = h("div", { class: "tree", role: "tree", tabindex: "0", "aria-label": "Folders", onkeydown: (e) => this.onKey(e) });
    this.el = h("div", { class: "tree-wrap" }, this.filterEl, this.treeEl);
    this.treeEl.append(h("div", { class: "tree-empty", text: "Loading…" }));
    this.load();
  }

  async load() {
    let data;
    try {
      data = await api("GET", "/fs/roots");
    } catch (e) {
      this.treeEl.replaceChildren(h("div", { class: "tree-empty", text: "Couldn't load folders: " + e.message }));
      return;
    }
    const mk = (path, extra) => Object.assign({ path, name: path, kids: null, expanded: false, children: true, depth: 0, parent: null }, extra);
    const recent = { section: true, name: "Recent", kids: [], expanded: true, depth: 0 };
    recent.kids = (data.recent || []).map((p) => {
      const n = mk(p, { name: shortDir(p), children: false, depth: 1, parent: recent, recent: true });
      return n;
    });
    const roots = (data.roots || []).map((p) => mk(p, { name: shortDir(p) }));
    this.nodes = (recent.kids.length ? [recent] : []).concat(roots);
    for (const r of roots) this.byPath.set(r.path, r);
    this.syncSelected();
    this.render();
    for (const r of roots.slice(0, 1)) this.expand(r);
  }

  // ----- data -----

  async expand(n) {
    if (!n || n.section || !n.children) return;
    n.expanded = true;
    if (n.kids === null && !n.loading) {
      n.loading = true;
      this.render();
      try {
        const res = await api("GET", "/fs/ls?dir=" + enc(n.path));
        n.kids = res.entries.map((e) => ({ ...e, kids: null, expanded: false, depth: n.depth + 1, parent: n }));
        if (res.truncated) n.kids.push({ note: "more folders not shown", depth: n.depth + 1, parent: n, kids: [] });
        for (const k of n.kids) if (k.path) this.byPath.set(k.path, k);
        if (!n.kids.length) n.children = false;
      } catch (e) {
        n.kids = [{ note: e.message, depth: n.depth + 1, parent: n, kids: [] }];
      }
      n.loading = false;
      this.syncSelected();
    }
    this.render();
  }

  collapse(n) {
    if (!n || n.section) return;
    n.expanded = false;
    this.render();
  }

  // syncSelected marks the loaded row whose path is the Directory field's value.
  syncSelected() {
    const v = this.opts.getDir().replace(/\/+$/, "");
    let n = this.byPath.get(v) || null;
    if (!n && v.startsWith("~/")) {
      for (const [p, x] of this.byPath) if (shortDir(p) === v) n = x;
    }
    this.selected = n;
  }

  // dirChanged is called when the Directory field is typed in.
  dirChanged() {
    this.syncSelected();
    this.render();
  }

  // ----- rendering -----

  matches(n) {
    return !this.filter || (n.name || "").toLowerCase().includes(this.filter);
  }

  // visible flattens the tree into the rows on screen. With a filter, loaded nodes that match show with
  // their ancestors, expanded or not.
  visible() {
    const out = [];
    const walk = (n) => {
      if (this.filter) {
        if (!this.keep(n)) return;
      }
      out.push(n);
      const open = n.section || n.expanded || (this.filter && n.kids);
      if (open && n.kids) for (const k of n.kids) walk(k);
    };
    for (const n of this.nodes) walk(n);
    return out;
  }

  keep(n) {
    if (n.note) return false;
    if (n.section) return n.kids.some((k) => this.keep(k));
    if (this.matches(n)) return true;
    return !!n.kids && n.kids.some((k) => this.keep(k));
  }

  render() {
    const rows = this.visible();
    this.rows = rows;
    if (!rows.length) {
      this.treeEl.replaceChildren(h("div", { class: "tree-empty", text: this.filter ? "No loaded folder matches." : "No folders." }));
      return;
    }
    if (this.cursor && !rows.includes(this.cursor)) this.cursor = null;
    const out = [];
    for (const n of rows) {
      out.push(this.rowEl(n));
      if (n === this.adding) out.push(this.addEl(n));
    }
    this.treeEl.replaceChildren(...out);
    const cur = this.treeEl.querySelector(".tree-row.cursor");
    if (cur) cur.scrollIntoView({ block: "nearest" });
  }

  rowEl(n) {
    const pad = 8 + n.depth * 16;
    if (n.section) return h("div", { class: "tree-section", style: "padding-left:" + pad + "px", text: n.name });
    if (n.note) return h("div", { class: "tree-note", style: "padding-left:" + (pad + 18) + "px", text: n.note });
    const chev = n.children
      ? h("span", {
        class: "tree-chev" + (n.expanded || (this.filter && n.kids) ? " open" : ""),
        onclick: (e) => { e.stopPropagation(); n.expanded ? this.collapse(n) : this.expand(n); },
      }, chevronIcon())
      : h("span", { class: "tree-chev none" });
    const sel = n === this.selected;
    const row = h("div", {
      class: "tree-row" + (sel ? " on" : "") + (n === this.cursor ? " cursor" : ""),
      role: "treeitem",
      "aria-selected": sel ? "true" : "false",
      "aria-expanded": n.children ? String(!!n.expanded) : null,
      style: "padding-left:" + pad + "px",
      title: n.path,
      onclick: () => this.pick(n),
      ondblclick: () => { n.expanded ? this.collapse(n) : this.expand(n); },
    },
    chev,
    h("span", { class: "tree-name", text: n.name }),
    n.loading ? h("span", { class: "tree-branch", text: "…" }) : null,
    n.branch ? h("span", { class: "tree-branch", text: n.branch }) : null);
    if (sel) {
      row.append(h("button", {
        type: "button", class: "tree-add", title: "New folder here", "aria-label": "New folder in " + n.name,
        onclick: (e) => { e.stopPropagation(); this.startAdd(n); },
      }, "+"));
    }
    return row;
  }

  addEl(n) {
    const input = h("input", {
      class: "tree-new", placeholder: "new folder name", autocomplete: "off", spellcheck: "false", "aria-label": "New folder name",
      onkeydown: (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          e.stopPropagation();
          const name = input.value.trim();
          if (!NEW_FOLDER_RE.test(name) || name === "." || name === "..") {
            input.classList.add("bad");
            return;
          }
          const base = this.opts.getDir().trim().replace(/\/+$/, "") || n.path;
          this.adding = null;
          this.opts.setDir(base + "/" + name);
          this.syncSelected();
          this.render();
          this.treeEl.focus();
        } else if (e.key === "Escape") {
          e.preventDefault();
          e.stopPropagation();
          this.adding = null;
          this.render();
          this.treeEl.focus();
        }
      },
    });
    const wrap = h("div", { class: "tree-add-row", style: "padding-left:" + (8 + (n.depth + 1) * 16 + 18) + "px" }, input);
    requestAnimationFrame(() => input.focus());
    return wrap;
  }

  startAdd(n) {
    this.adding = n;
    this.render();
  }

  pick(n) {
    if (!n || !n.path) return;
    this.cursor = n;
    this.selected = n;
    this.adding = null;
    this.opts.setDir(n.path);
    this.render();
  }

  // ----- keyboard (only while the tree has focus) -----

  onKey(e) {
    if (e.target !== this.treeEl) return; // the new-folder field handles its own keys
    const rows = (this.rows || []).filter((n) => n.path);
    if (!rows.length) return;
    let i = rows.indexOf(this.cursor);
    const n = this.cursor;
    switch (e.key) {
      case "ArrowDown":
        i = Math.min(rows.length - 1, i + 1);
        break;
      case "ArrowUp":
        i = Math.max(0, i - 1);
        break;
      case "ArrowRight":
        if (n && n.children && !n.expanded) this.expand(n);
        else if (n && n.expanded && n.kids && n.kids.length && n.kids[0].path) i = rows.indexOf(n.kids[0]);
        break;
      case "ArrowLeft":
        if (n && n.expanded) this.collapse(n);
        else if (n && n.parent && n.parent.path) i = rows.indexOf(n.parent);
        break;
      case "Enter":
        if (n) this.pick(n);
        break;
      default:
        return;
    }
    e.preventDefault();
    if (e.key !== "Enter") {
      this.cursor = rows[Math.max(0, i)];
      this.render();
    }
  }
}
