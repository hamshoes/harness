"use strict";
// Uploads: drop files anywhere on the page (or pick them with an Upload button), check the target
// in a small panel, then POST /files. Loaded before app.js and uses its helpers (h, svg, S, …) only
// from event handlers and initUpload, which app.js calls once it has run.

function fmtBytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  if (n < 1024 * 1024 * 1024) return (n / 1048576).toFixed(1) + " MB";
  return (n / 1073741824).toFixed(1) + " GB";
}

// uploadTarget is where a drop lands: the open session's dir, the new-session form's dir, else ~/uploads.
function uploadTarget() {
  const v = S.view;
  if (v && v.kind === "session" && v.session) return { dir: v.session.dir, session: v.name, view: v };
  if (v && v.kind === "new") return { dir: v.el.dir.value.trim() || "~/uploads", view: v };
  return { dir: "~/uploads" };
}

function clipIcon() {
  return svg(
    { width: "16", height: "16", viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" },
    ["M13.5 7.5 8.2 12.8a3.2 3.2 0 0 1-4.5-4.5l5.6-5.6a2.1 2.1 0 0 1 3 3L6.7 11.3a1.1 1.1 0 0 1-1.5-1.5L10.3 4.7"],
  );
}

let picker = null; // the one hidden file input every Upload button opens

function uploadButton(extra) {
  return h("button", {
    type: "button",
    class: "btn-upload" + (extra ? " " + extra : ""),
    "aria-label": "Upload files",
    title: "Upload files",
    onclick: () => picker && picker.click(),
  }, clipIcon());
}

// ---------- drag and drop ----------

let dragDepth = 0;
let overlay = null;

function hasFiles(e) {
  return !!(e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files"));
}

function showOverlay() {
  overlay.querySelector(".drop-dir").textContent = uploadTarget().dir;
  overlay.hidden = false;
}

function hideOverlay() {
  dragDepth = 0;
  overlay.hidden = true;
}

// droppedFiles returns the dropped files, leaving out folders (which can't be read as one file).
function droppedFiles(dt) {
  const files = [];
  let folders = 0;
  const items = Array.from(dt.items || []);
  if (items.length && typeof items[0].webkitGetAsEntry === "function") {
    for (const it of items) {
      if (it.kind !== "file") continue;
      const entry = it.webkitGetAsEntry();
      if (entry && entry.isDirectory) {
        folders++;
        continue;
      }
      const f = it.getAsFile();
      if (f) files.push(f);
    }
    return { files, folders };
  }
  return { files: Array.from(dt.files || []), folders };
}

// ---------- the panel ----------

let panel = null;

function closePanel() {
  if (!panel) return;
  clearTimeout(panel.timer);
  if (panel.busy && panel.xhr) panel.xhr.abort();
  panel.el.remove();
  panel = null;
}

function openUploadPanel(files, folders) {
  if (panel && panel.busy) return; // one upload at a time
  closePanel();
  if (!files.length && !folders) return;
  const target = uploadTarget();
  const P = (panel = { target, files, busy: false, done: false, hovered: false });
  const total = files.reduce((n, f) => n + f.size, 0);
  const hasZip = files.some((f) => /\.zip$/i.test(f.name));

  P.dir = h("input", { class: "field up-dir", value: target.dir, spellcheck: "false", autocomplete: "off", "aria-label": "Directory" });
  P.extract = hasZip ? h("input", { type: "checkbox", checked: true }) : null;
  P.overwrite = h("input", { type: "checkbox" });
  P.fill = h("div", { class: "up-fill" });
  P.bar = h("div", { class: "up-bar", hidden: true }, P.fill);
  P.status = h("div", { class: "up-status", role: "status" });
  P.upload = h("button", { type: "button", class: "btn-primary", onclick: () => startUpload(P) }, "Upload");
  P.cancel = h("button", { type: "button", class: "btn-cancel", onclick: closePanel }, "Cancel");
  if (!files.length) P.upload.disabled = true;

  P.body = h("div", { class: "up-body" },
    h("ul", { class: "up-files" }, files.map((f) =>
      h("li", null, h("span", { class: "up-name", text: f.name, title: f.name }), h("span", { class: "up-size", text: fmtBytes(f.size) })))),
    folders ? h("div", { class: "up-note", text: (folders === 1 ? "1 folder" : folders + " folders") + " left out: zip folders to upload them." }) : null,
    h("label", { class: "lbl" }, "Directory", P.dir),
    P.extract ? h("label", { class: "up-check" }, P.extract, "Extract zips here") : null,
    h("label", { class: "up-check" }, P.overwrite, "Overwrite existing"),
    P.bar,
    P.status,
    h("div", { class: "up-actions" }, P.upload, P.cancel));

  P.el = h("section", {
    class: "up-panel",
    "aria-label": "Upload files",
    onkeydown: (e) => {
      // Only fires with focus inside the panel.
      if (e.key === "Escape") {
        e.preventDefault();
        closePanel();
      } else if (e.key === "Enter" && e.target.tagName === "INPUT" && !P.done) {
        e.preventDefault();
        startUpload(P);
      }
    },
    onmouseenter: () => { P.hovered = true; },
    onmouseleave: () => {
      P.hovered = false;
      if (P.closeOnLeave && panel === P) closePanel();
    },
  },
  h("div", { class: "up-head" },
    h("span", { text: files.length === 1 ? "Upload 1 file" : "Upload " + files.length + " files" }),
    h("span", { class: "up-total", text: fmtBytes(total) })),
  P.body);
  document.body.append(P.el);
  P.upload.focus();
}

function startUpload(P) {
  if (P !== panel || P.busy || P.done || !P.files.length) return;
  const dir = P.dir.value.trim();
  if (!dir) {
    setStatus(P, "Directory is required.", true);
    return;
  }
  const q = new URLSearchParams({ dir });
  if (P.extract && P.extract.checked) q.set("extract", "1");
  if (P.overwrite.checked) q.set("overwrite", "1");
  if (P.target.session) q.set("session", P.target.session);
  const fd = new FormData();
  for (const f of P.files) fd.append("file", f, f.name);

  // XMLHttpRequest rather than fetch: fetch gives no upload progress.
  const xhr = new XMLHttpRequest();
  P.xhr = xhr;
  P.busy = true;
  setInputs(P, false);
  P.bar.hidden = false;
  P.fill.style.width = "0%";
  setStatus(P, "Uploading…", false);
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    P.fill.style.width = ((100 * e.loaded) / e.total).toFixed(1) + "%";
    setStatus(P, fmtBytes(e.loaded) + " of " + fmtBytes(e.total), false);
  };
  xhr.onload = () => {
    P.busy = false;
    let data = null;
    try {
      data = JSON.parse(xhr.responseText);
    } catch (e) {
      data = null;
    }
    if (xhr.status === 200 && data) showResult(P, data);
    else failed(P, (data && data.error) || "Upload failed (HTTP " + xhr.status + ").");
  };
  xhr.onerror = () => {
    P.busy = false;
    failed(P, "Upload failed: server unreachable.");
  };
  xhr.onabort = () => { P.busy = false; };
  xhr.open("POST", "/files?" + q.toString());
  xhr.send(fd);
}

function setInputs(P, on) {
  for (const el of [P.dir, P.extract, P.overwrite, P.upload]) if (el) el.disabled = !on;
}

function setStatus(P, text, bad) {
  P.status.textContent = text;
  P.status.classList.toggle("bad", bad);
}

function failed(P, msg) {
  if (P !== panel) return;
  P.bar.hidden = true;
  setStatus(P, msg, true);
  setInputs(P, true);
}

function showResult(P, data) {
  if (P !== panel) return;
  P.done = true;
  const saved = data.saved || [];
  const skipped = data.skipped || [];
  const rel = (p) => (p.startsWith(data.dir + "/") ? p.slice(data.dir.length + 1) : p);
  P.body.replaceChildren(
    h("div", { class: "up-result-head", text: (saved.length === 1 ? "Saved 1 file to " : "Saved " + saved.length + " files to ") + data.dir }),
    saved.length ? h("ul", { class: "up-saved" }, saved.map((s) =>
      h("li", null, h("span", { class: "up-path", text: s.path, title: s.path }), copyButton(s.path)))) : null,
    skipped.length ? h("ul", { class: "up-skipped" }, skipped.map((s) =>
      h("li", null, h("span", { class: "up-name", text: s.name, title: s.name }), h("span", { class: "up-reason", text: s.reason })))) : null,
    h("div", { class: "up-actions" }, h("button", { type: "button", class: "btn-cancel", onclick: closePanel }, "Close")));
  P.timer = setTimeout(() => {
    if (P.hovered) P.closeOnLeave = true;
    else if (panel === P) closePanel();
  }, 6000);

  const v = P.target.view;
  if (v && v === S.view && v.kind === "session" && saved.length) v.noteUploaded(data.dir, saved.map((s) => rel(s.path)));
}

function copyButton(text) {
  return h("button", {
    type: "button",
    class: "btn-copy",
    "aria-label": "Copy path",
    title: "Copy path",
    onclick: (e) => copyText(text, e.currentTarget),
  }, "Copy");
}

// copyText uses the clipboard API where the page is a secure context (localhost), else the old
// select-and-copy way (e.g. on a plain-http host name).
async function copyText(text, btn) {
  let ok = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      ok = true;
    }
  } catch (e) {
    ok = false;
  }
  if (!ok) {
    const ta = h("textarea", { class: "sr-only", readonly: true });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    ta.remove();
  }
  btn.textContent = ok ? "Copied" : "Failed";
  setTimeout(() => { btn.textContent = "Copy"; }, 1500);
}

// ---------- setup (called from app.js) ----------

function initUpload() {
  picker = h("input", {
    type: "file",
    multiple: true,
    hidden: true,
    onchange: () => {
      const files = Array.from(picker.files || []);
      picker.value = "";
      if (files.length) openUploadPanel(files, 0);
    },
  });
  overlay = h("div", { class: "drop-overlay", hidden: true },
    h("div", { class: "drop-box" }, h("span", { text: "Drop to upload to " }), h("span", { class: "drop-dir" })));
  document.body.append(picker, overlay);
  $("new-btn").before(uploadButton("side"));

  window.addEventListener("dragenter", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    showOverlay();
  });
  window.addEventListener("dragover", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  });
  window.addEventListener("dragleave", (e) => {
    if (!hasFiles(e)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) hideOverlay();
  });
  window.addEventListener("drop", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    hideOverlay();
    const { files, folders } = droppedFiles(e.dataTransfer);
    openUploadPanel(files, folders);
  });
}
