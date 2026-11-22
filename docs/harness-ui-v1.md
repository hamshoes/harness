> **Prompt for the agent** (unzip into the harness repo so `docs/harness-ui-v1.md` and
> `docs/design/` land in `docs/`, then start a fresh agent in the repo root and paste):
>
> ```
> Implement docs/harness-ui-v1.md, milestones in order, committing each. Read
> .claude/CLAUDE.md first. The designs are in docs/design/. Stop after the last milestone
> with the hand-over summary.
> ```

# harness UI v1: a Mac browser page for sessions

## Context

`harness-v1` is done: `harness serve` runs Claude sessions on the box, logs every event in
SQLite, reports `agentic` approval holds, and serves a JSON API on a Unix socket. Sam drives it
with curl.

**This builds the web UI**, for a browser on Sam's Mac:
- `harness serve --http :8085` also listens on TCP and serves the same API plus one page;
- Sam runs `bb forward 8085` and opens `http://devbox:8085` (or `localhost:8085`).

**First go.** Get it working and looking like the designs; polish comes later.

**Out of scope:** answering approvals from the page (the card is read-only; Sam answers in the
Mac dialog), auth, phone layout, markdown rendering, running as a service.

## The designs (`docs/design/`)

`Main.dc.html` (session list, open session, details) and `New.dc.html` (new session form) are
exports from a design canvas. They're **reference, not code to ship**:
- the markup inside `<x-dc>` and the `<helmet><style>` block show layout, spacing, colours and
  type: copy those;
- `{{…}}`, `<sc-for>`, `<sc-if>`, `support.js` and the `class Component` block are the canvas's
  template runtime with mock data: ignore them, except as a guide to what each part shows;
- where a design and this spec disagree, **this spec wins** (e.g. the approval card has no
  Allow/Deny buttons in v1).

**Tokens** (from the designs):

| Use | Value |
| --- | --- |
| Page / sidebar / panel | `#0F1115` / `#0C0E12` / `#171A21` (hover), lines `#1E222B`, inputs `#141720` |
| Text / secondary / muted | `#E6E8EC` / `#C9CED6`, `#AEB5C0` / `#9AA3AF` |
| Running | `#6CA8FF` (filled pill, dark text) |
| Waiting / approvals | `#F2B544`; card `#2A2213`, border `#5C4718`, text `#F7D38A` / `#E9DDBE` |
| Idle | outline `#5B6472` |
| Failed | `#FF8A8A` outline and text |
| Stopped | `#232731` fill, `#9AA3AF` text |
| OK | `#5FD08A` |
| Your messages | `#26324A` bubble |
| Fonts | Inter (UI), JetBrains Mono (names, paths, tool rows, meta), from Google Fonts |

## Rules

Everything in `.claude/CLAUDE.md`, plus:
- **Plain HTML, CSS and JavaScript.** No framework, no bundler, no npm, no build step. Files
  under `web/`, embedded with `embed`. One `index.html`, one `app.css`, one `app.js`
  (split `app.js` only if it passes ~800 lines).
- **Never put event text into the page as HTML.** Use `textContent` (or DOM building)
  everywhere: event text is model output and tool output.
- No browser dialogs (`alert`, `confirm`, `prompt`).
- Keyboard shortcuts only on the focused field, never global handlers.

---

## U1: HTTP listener, SSE, extra fields

**Flag:** `harness serve … [--http ADDR]`. Empty (default) = no TCP listener. When set, the
**same handler** serves the API on TCP and the socket, plus:
- `GET /` → `index.html`; `GET /app.css`, `GET /app.js`;
- everything else exactly as on the socket (`/sessions…`, `/approvals`, `/blobs/…`,
  `/healthz`). The page uses hash routes (`#/s/<name>`, `#/new`), so there's no clash.

No auth: the port is only reachable through Sam's ssh forward. Log the address at start.

**SSE on the events route.** `GET /sessions/{name}/events` with `Accept: text/event-stream`
(and `follow` implied):
- each event is `id: <seq>`, `event: <type>`, `data: <the same JSON object as an NDJSON line>`;
- `Last-Event-ID` (sent by `EventSource` on reconnect) takes precedence over `?after=`;
- a `: ping` comment every 20 s;
- after `session_stopped`, send it and close.
NDJSON behaviour is unchanged for everything else.

**Session JSON gains** (list and single):
- `last_text`: the newest `assistant_text`, first 200 characters (`""` if none);
- `last_result`: `{duration_ms, total_cost_usd}` from the newest `result` event, or `null`.

Keep them in the `sessions` row, updated when those events are appended; don't scan the log.

**Commit:** `feat(harness): http listener, sse events, list fields`.

## U2: the page

**Layout** (as `Main.dc.html`): full-window, three columns.

1. **Sidebar** (300 px): `harness` wordmark, **New** button (→ `#/new`), filter chips
   **All / Active / Idle / Ended** (Active = running, incl. waiting; Ended = failed or
   stopped), then session rows: name, status pill, `last_text` (two lines, clipped),
   `dir · ago`. Selected row highlighted. Footer: a green dot and `N running · N waiting`
   (from `/healthz`); the dot goes `#FF8A8A` and the text says `disconnected` when
   requests fail.
2. **Session** (fluid): header with name, status pill, `dir · turn N`, **Stop session**.
   Below, the event log (max width 760 px, centred), then the reply box.
3. **Details** (280 px): dir, turns, seq, Claude session id, created; **Waiting on** (each
   `waiting_on` item `title · age`, or `nothing`); **Last turn** duration and cost from
   `last_result`. Hidden below 1100 px window width.

**Status pill:** `Waiting` when status is `running` and `waiting_on` isn't empty; otherwise the
status itself (`Running`, `Idle`, `Failed`, `Stopped`).

**Data:**
- **List:** `GET /sessions` on load and every 3 s; `GET /healthz` every 5 s. Recent dirs (for
  the new-session chips) are the distinct `dir`s from the list, newest first, max 5.
- **Open session:** `GET /sessions/{name}`, and an `EventSource` on its events from `after=0`.
  Switching session closes the old stream. Refresh the session JSON on every `turn_started`,
  `turn_ended`, `approval_*` and `session_stopped` event.
- **No session selected** (empty route): open the newest session that's running or waiting,
  else the newest; none at all → an empty state with a **Start a session** button.

**Events → log rows:**

| Event | Row |
| --- | --- |
| `user_message` | right-aligned bubble |
| `assistant_text` | paragraph, `white-space: pre-wrap` |
| `tool_use` | tool row: tool name (muted, mono) and a one-line summary: `command` for Bash, `file_path` for file tools, else compact JSON of `input`, clipped. Right side: `…` until its result |
| `tool_result` | updates its `tool_use` row: `ok` (green) or `error` (red). Not a row of its own |
| `approval_pending` | the amber card: title, `detail` (mono, `pre-line`), "Answer in the Mac dialog." **No buttons in v1.** The matching tool row shows `held` (amber) |
| `approval_resolved` | the card collapses to a tool-style row: `ask · <title>` and `allowed` / `denied` / `cancelled` / `withdrawn` |
| `turn_ended` with status `failed` | red row: `turn failed (exit N)` |
| `turn_interrupted`, `dir_created`, `session_stopped` | small muted line |
| `claude_init`, `thinking`, `result`, `turn_started`, `stderr`, `raw` | hidden |

A value that came back as a blob (`{"blob": "sha256:…"}`) shows as `large output` linking to
`/blobs/<hash>` in a new tab.

**Raw toggle:** a small **Raw** button in the session header switches the log to every event
as one mono line `seq type json`, hidden types included. For debugging.

**Scrolling:** stick to the bottom while the user is at the bottom; once they scroll up, stop
following and show a **Jump to latest** pill.

**Reply box:** a two-line textarea and **Send**. Enabled only when the session is `idle` or
`failed`; otherwise disabled with the placeholder `Turn running — reply when it's idle` (or
`Session stopped`). ⌘↵ (Ctrl+↵ elsewhere) sends from the textarea. `POST …/messages`;
on `409` show the error text under the box. Clear the box on `202`.

**Stop session:** first click turns the button into **Confirm stop** for 4 s; a second click
`POST …/stop`. No dialog.

**Commit:** `feat(harness): web ui: sessions, log, reply, stop`.

## U3: new session

As `New.dc.html`, at `#/new`, sidebar kept:
- **Name** (mono; validate `^[a-z0-9][a-z0-9-]{0,39}$` as you type, message under the field),
  **Directory** (default: the newest recent dir), recent-dir chips that fill it, **First
  message** (10-row textarea).
- **Start session** (and ⌘↵ in the message box) → `POST /sessions`. On `201`, go to
  `#/s/<name>`. On `400`/`409`, show the API's error under the form; keep what was typed.
- **Cancel** → back to the previous route.

**Commit:** `feat(harness): web ui: new session`.

---

## Agent checks

`go vet ./...`, the cross-build (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`). Push `main`.
You may run `harness serve --http 127.0.0.1:8085` on the Mac **only if** `claude` and
`agentic` resolve there; otherwise don't run it.

## Try it (Sam)

```sh
# box
cd ~/src/harness && git pull && go install . && harness serve --http :8085   # in tmux
bb forward 8085
# Mac
open http://devbox:8085
```

**Done when** you can start a session from the page, watch it live, see an approval card appear
and resolve when you answer the Mac dialog, reply once it's idle, and stop it; and closing
and reopening the tab picks the log back up.

## Hand-over summary

Commits; anything in the designs you couldn't match; anything you had to guess.
