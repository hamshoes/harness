> **Prompt for the agent:** implement docs/harness-upload-v1.md, milestones in order, committing
> each. Read .claude/CLAUDE.md first. Stop after the last milestone with the hand-over summary.

# harness upload v1: drag and drop files onto the page

## Context

The web UI (`harness-ui-v1`) is done. Getting files from the Mac onto the box is painful: the
box's ssh lock refuses `scp` and piped commands by design. The harness page is already open on
the Mac, so it becomes the way in: **drop files on the page, they land in a directory on the
box.** Typical use: drop a spec zip into a repo, or a screenshot or log for a session to read.

**Out of scope:** downloads from the box (that's `bb send`), dropping whole folders (zip them),
resumable or chunked uploads, auth (unchanged: the port is only reachable through Sam's
forward).

## Rules

Everything in `.claude/CLAUDE.md`, plus:
- Standard library only (`mime/multipart`, `archive/zip`).
- **Never write outside the target directory.** Clean every name; reject absolute paths, `..`
  segments and symlinks in zips ("zip slip"); never follow a symlink already in the target.
- Never log file contents.

## F1: the API

```
POST /files?dir=<dir>[&extract=1][&overwrite=1][&session=<name>]
Content-Type: multipart/form-data   (one or more parts named "file")
```

- **`dir`**: expand `~`; must be absolute; created (`MkdirAll`, `0755`) if missing; `400` if
  it exists and isn't a directory.
- **Each file:** saved as `<dir>/<base name>` (base name only, cleaned; empty or `.`/`..` →
  skipped). Write to a temp file in `dir`, `fsync`, then rename. Mode `0644`.
- **Existing file:** skipped with reason `exists` unless `overwrite=1`.
- **`extract=1`:** a part whose name ends `.zip` is extracted into `dir` instead of saved
  (directories created, entries as above, the same `exists`/`overwrite` rule per entry,
  symlink entries skipped with reason `symlink`, any entry escaping `dir` → the whole zip is
  rejected with reason `unsafe path` and nothing from it is written). The zip itself isn't kept.
  Non-zip parts are saved normally.
- **Limits:** 200 MB per request (`http.MaxBytesReader`), 5,000 entries and 1 GB uncompressed
  per zip. Over a limit → `413` (request) or the zip skipped with reason `too large`.
- **`session=<name>`:** if the session exists, append a `files_uploaded`
  `{dir, paths: [...]}` event to its log (paths relative to `dir`, max 200 listed, plus
  `more: N`).
- **Response `200`:** `{dir, saved: [{path, bytes}], skipped: [{name, reason}]}`; `path` is
  absolute.

Works on both the socket and `--http`. Quick check with curl:
`curl --unix-socket $S -F file=@notes.md 'localhost/files?dir=~/tmp/drop'`.

**Commit:** `feat(harness): file upload endpoint`.

## F2: drag and drop on the page

**Drop anywhere in the window.** While dragging files over the page, show a full-window overlay
(dark scrim, dashed `#6CA8FF` border, 16 px inset): **Drop to upload to `<dir>`**.

**Target `dir`:**
- in a session view: that session's `dir` (and `session=<name>`);
- on `#/new`: the Directory field's value;
- elsewhere (no session open): `~/uploads`.

**On drop**, open a small upload panel (bottom-right, 380 px, over the page, not a browser
dialog) listing the files with sizes, and:
- **Directory**: a mono text field prefilled with the target; editable;
- **Extract zips here**: checkbox, shown only if a `.zip` is in the drop, **on** by default;
- **Overwrite existing**: checkbox, off;
- **Upload** (Enter) and **Cancel** (Esc, focused field only).

Upload with `fetch` and `FormData`, showing a progress bar (use `XMLHttpRequest` upload
progress if `fetch` can't give it). Then the panel shows the result: each saved path
(mono, with a copy button) and each skip with its reason, and closes after 6 s unless hovered.

**Also clickable:** an **Upload** button (paperclip icon, `aria-label="Upload files"`) next to
**Send** in the reply box and in the sidebar header, opening a file picker that leads to the
same panel.

**After an upload in a session view:** if the reply box is enabled, append
`Uploaded to <dir>: <path>, <path>…` (relative paths, max 10, then `+N more`) to its text so
Sam can tell Claude in the next message. In the log, `files_uploaded` shows as a muted line
`uploaded N files to <dir>`.

**On `#/new`:** after an upload, keep the form as it is (the directory now exists).

**Commit:** `feat(harness): drag and drop uploads in the web ui`.

## Agent checks

`go vet ./...` and the cross-build. Push `main`. Don't run it against the box.

## Try it (Sam)

Rebuild, restart `harness serve --http :8085`, open the page, and drop a spec zip onto a
session: it should extract into the session's directory, and the reply box should list the new
files.

## Hand-over summary

Commits; anything you had to guess.
