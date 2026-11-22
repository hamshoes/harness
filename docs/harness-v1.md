> **Prompt for the agent** (create the empty repo `github.com/hamshoes/harness` and clone it,
> save this file as `docs/harness-v1.md`, then start a fresh agent in the repo root and paste):
>
> ```
> Implement docs/harness-v1.md, milestones in order, committing each. The repo is new:
> H1.0 sets it up. Sam will give you the path of a devbox-tools checkout to read (read
> only). Stop after the last milestone with the hand-over summary.
> ```

# harness v1: sessions over a socket API, with approvals in the log

## Context

Sam develops inside a Linux container (**the box**) on a GCP VM. Claude Code is installed there,
and `agentic` (a tool from Sam's devbox-tools repo, already on the box's `PATH`) wraps agent runs: `agentic run --name N -- claude …`
adds Claude Code hooks that log every Bash command and hold `git push` for Sam's approval on
the Mac.

**`harness`** is a new tool, in **its own repo**, that owns Claude Code sessions on the box, so Sam can hand work to
agents and stay in control without babysitting terminals. Later work adds a web UI, running
as a service, releases and phone access. **This doc is only this:** a process Sam starts by
hand (`harness serve`, in tmux) that exposes a small JSON API on a **Unix socket**, runs Claude
turns headless, stores every event in SQLite, and reports `agentic`'s approval holds in each
session's log.

**How a session works:**
- A session has a **name** (required) and a **directory**.
- Each turn is **one process**: `agentic run --name <name> -- claude -p --output-format stream-json …`.
  The first turn captures Claude's session id; later turns add `--resume <id>`.
- Between turns no process runs. The session sits at `idle` in SQLite.
- Every stream-json line becomes an **event** with a per-session `seq`. Clients read the log
  from any `seq` and can follow it live.

**It's a user tool, not platform.** No image change, no controld service, no CI or releases. Sam installs it on the box with `go install`. It has **no code dependency** on
devbox-tools; it only shells out to the `agentic` and `claude` binaries.

**Out of scope:** web UI, TCP port, auth, running as a service, releases, answering approvals
from the harness, jobs, a CLI client (curl is the client), config files, interactive take-over.

**Approvals.** How they already work on the box (built in devbox-tools; this repo only reads it):
- `agentic`'s hook writes an **ask file** to the notify spool, `<spool>/asks/<ask_id>.json`,
  then waits.
- The notify pair shows it on the Mac. The decision lands in `<spool>/answers/<ask_id>.json`
  as `{decision, note, answered_at}`, where `decision` is `allow`, `deny` or `cancelled`.
- If the agent is interrupted, `agentic` removes its ask file and withdraws it.
- The ask file carries `run_name`, which `agentic run --name` sets. **The harness passes the
  session name there, so `run_name` == session name.** That's the join.
- The harness **only reports**: it never writes to the spool. Answering comes later.

## Rules

- **Branch:** work on `main` and push it. Nothing releases from this repo yet.
- **Module:** `github.com/hamshoes/harness`, Go version from `go version` on the Mac (1.22+).
- No tests, no README (Sam writes it), no CI. Never run docker, gcloud or ssh against the VM.
- **Pre-alpha, one user:** no backward compatibility, no migrations framework (create tables
  if missing).
- Generic language in code: the remote side is **the box**, the Mac binary is **the client**.
  Never write the product name.
- `CGO_ENABLED=0`, `go vet ./...`.
- **Dependencies:** standard library (`net/http` with Go 1.22+ routing), `modernc.org/sqlite`.
  Nothing else without asking.
- **Logs:** never log tokens. Prompt and message bodies only at debug level.
- **Claude Code flags and stream-json vary by version.** Before writing the runner, read
  `claude --help` (ask Sam to paste it from the box if the Mac's version differs) and the
  headless docs. Record the version you built against in a comment in `runner.go`.
  - **`--name` is mandatory on every `claude` invocation.** Confirm the flag's spelling and
    whether it must repeat on `--resume`. If the installed `claude` has no such flag,
    **stop and ask**. Don't drop it.
  - If anything here disagrees with the installed version, stop and ask.
- **Spool:** check the formats in the devbox-tools source before relying on them (how `agentic`
  writes ask files, how the notify pair writes answers, the spool's environment variable).
  **If an ask file has no `run_name`, or anything here disagrees with the source, stop and
  ask.** Record the devbox-tools commit you read in a comment in `approvals.go`. Never write,
  rename, delete or `chmod` anything in the spool. Never log an ask's `detail`. Poll; no
  file-watching library.
- Stop and ask on anything this spec doesn't cover.

## Layout (repo root)

```
go.mod  go.sum  .gitignore
.claude/CLAUDE.md    # H1.0
docs/                # specs (Sam's; never edit them)
main.go              # `harness serve` (the only subcommand in stage 1)
internal/store       # SQLite: sessions, events; blob files
internal/runner      # one turn = one process; stream-json → events
internal/sessions    # session state machine, per-session lock, live fan-out
internal/approvals   # spool watcher (H2)
internal/api         # HTTP handlers on the Unix socket
```

## Paths and flags

```
harness serve [--socket PATH] [--data DIR] [--spool DIR] [--permission-mode MODE] [--debug]
```

| What | Default |
| --- | --- |
| Socket | `~/.local/state/harness/harness.sock` (create the dir `0700` if missing; socket `0600`; remove a stale socket on start) |
| Data | `~/.local/share/harness/`: `harness.db` and `blobs/` (created `0700` if missing) |
| Spool | the notify spool's environment variable (expected `BOX_NOTIFY_SPOOL`). **Never created by the harness:** the image owns it. |
| Permission mode | `bypassPermissions`, passed to every turn as `--permission-mode`. The box is the sandbox; `agentic`'s hooks still gate `git push`. |

**Fail fast on start** if `claude` or `agentic` isn't on `PATH`, the socket is in use by a
live process, or the spool isn't set or its `asks/` or `answers/` isn't readable. Capture `claude --version` once at start for `/healthz`.

---

## H1.0: repo setup

- `go mod init github.com/hamshoes/harness`; `.gitignore` for the built binary.
- `.claude/CLAUDE.md`, short, carrying the standing rules for future agents in this repo:
  - the Rules section above, minus anything specific to stage 1;
  - "specs live in `docs/`; implement them milestone by milestone, commit each, stop and ask on
    gaps";
  - the layout, and that `agentic` and `claude` are runtime binaries on the box, not imports.
- A stub `main.go` with `harness serve` that only parses flags and exits.

**Commit:** `chore: repo setup`.

## H1.1: store

**SQLite**, WAL mode, `busy_timeout`, one writer connection.

```sql
sessions(
  name TEXT PRIMARY KEY,           -- ^[a-z0-9][a-z0-9-]{0,39}$
  dir TEXT NOT NULL,               -- absolute, after ~ expansion
  status TEXT NOT NULL,            -- running | idle | stopped | failed
  claude_session_id TEXT,          -- from the first turn's init event
  turns INTEGER NOT NULL DEFAULT 0,
  last_seq INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL   -- RFC 3339 UTC
)
events(
  session TEXT NOT NULL, seq INTEGER NOT NULL,
  ts TEXT NOT NULL, type TEXT NOT NULL, data TEXT NOT NULL,   -- data is a JSON object
  PRIMARY KEY (session, seq)
)
```

- `seq` starts at 1 per session and has no gaps. Appending an event and bumping
  `sessions.last_seq` happen in one transaction.
- **Blobs:** any string value in an event's `data` larger than **32 KiB** is written to
  `blobs/<sha256 hex>` (write to a temp file, then rename; skip if it exists) and replaced by
  `{"blob":"sha256:<hex>","bytes":N}`.

**Commit:** `feat(harness): sqlite event store and blobs`.

## H1.2: runner

**One turn:**

```
cwd = session.dir
agentic run --name <name> -- claude -p --output-format stream-json --verbose \
    --name <name> --permission-mode <mode> [--resume <claude_session_id>]
stdin  = the prompt text (not argv)
env    = inherited + HARNESS_SESSION=<name>
```

- Run in its **own process group**, so stopping kills the whole tree.
- Read stdout line by line (allow long lines, e.g. 16 MiB buffer). Each line becomes events:

| stream-json | Event `type` | `data` |
| --- | --- | --- |
| `system` / `init` | `claude_init` | `session_id`, `model`, `cwd` (store `session_id` on the first turn) |
| assistant `text` block | `assistant_text` | `text` |
| assistant `thinking` block | `thinking` | `text` |
| assistant `tool_use` block | `tool_use` | `id`, `name`, `input` |
| user `tool_result` block | `tool_result` | `tool_use_id`, `is_error`, `content` |
| `result` | `result` | `is_error`, `duration_ms`, `num_turns`, `total_cost_usd`, `result` |
| anything else, or unparseable | `raw` | `line` |

  Nothing is dropped: if in doubt, it's `raw`. Adjust the mapping to what the installed
  version actually emits.
- **stderr:** each line becomes a `stderr` event (cap 200 lines per turn, then one
  `stderr_truncated` event).
- **Turn bookends** (written by the harness, not Claude):
  - `user_message` `{text, turn}` then `turn_started` `{turn}` before spawning;
  - `turn_ended` `{turn, exit_code, status}` after exit. Exit 0 → session `idle`; non-zero
    exit or no `claude_init` on the first turn → `failed`.

**Commit:** `feat(harness): headless claude runner`.

## H1.3: sessions, API and `serve`

**State machine** (one mutex per session):

```
create → running → idle ⇄ running
                 ↘ failed            stop (from any non-stopped state) → stopped
```

- A `failed` session accepts a new message (it may resume fine). A `stopped` session accepts
  nothing.
- **Stop:** send SIGINT to the turn's process group; SIGKILL after 10 s. Write
  `turn_ended` `{status:"stopped"}` if a turn was running, then `session_stopped`.
- **Live fan-out:** each session has an in-memory broadcaster. Followers subscribe **before**
  reading the backlog and drop duplicates by `seq`, so there's no gap.
- **Harness restart:** on start, any session left `running` becomes `idle` with a
  `turn_interrupted` event (its Claude session can still be resumed).
- **Shutdown** (SIGINT/SIGTERM): stop accepting, SIGINT running turns, wait up to 10 s, write
  `turn_interrupted`, exit.

**API** (Unix socket only, JSON in and out, errors as `{"error":"…"}`):

| Route | Does | Notes |
| --- | --- | --- |
| `POST /sessions` | Create and start turn 1 | Body `{name, dir, prompt}`, all required. `400` bad name/prompt, or `dir` is a file; `409` name exists (names are never reused). `201` → session. |
| `GET /sessions` | List | Newest first. |
| `GET /sessions/{name}` | One session | `404` if unknown. |
| `GET /sessions/{name}/events` | Read the log | `?after=N` (default 0), `?follow=1`. NDJSON, one event per line: `{seq, ts, type, data}`. Without `follow`, returns the backlog and closes. With `follow`, stays open and streams live (flush per line) until the client goes or the session is `stopped`. |
| `POST /sessions/{name}/messages` | Next turn | Body `{text}`. `409` if `running` or `stopped`. `202` → session. |
| `POST /sessions/{name}/stop` | End the session | Idempotent. `200` → session. |
| `GET /blobs/{hash}` | Raw blob bytes | `hash` as `sha256:<hex>` or bare hex. |
| `GET /healthz` | Liveness | `{ok, version, claude_version, sessions: {running, idle, …}}` |

**Session JSON:** `{name, dir, status, turns, last_seq, claude_session_id, created_at, updated_at}`.

**`dir`:** expand `~`; must be absolute. If it doesn't exist, create it (`MkdirAll`, `0755`) and write a `dir_created` `{dir}` event before turn 1. If it exists but isn't a directory → `400`.

**Commit:** `feat(harness): session API on a unix socket`.

---

## H2.1: spool watcher

`internal/approvals`, and a table:

```sql
approvals(
  ask_id TEXT PRIMARY KEY,
  session TEXT NOT NULL,
  kind TEXT NOT NULL, title TEXT NOT NULL,
  created_at TEXT NOT NULL,        -- from the ask file
  state TEXT NOT NULL,             -- pending | allow | deny | cancelled | withdrawn
  resolved_at TEXT
)
```

**Every second:**
1. **New asks.** For each `asks/*.json` not already in the table:
   - parse it (skip and log at debug if it's unreadable or still being written; try next tick);
   - match `run_name` to a session whose status is `running`. No match → ignore it (it's some
     other agent run on the box), and don't look at it again this process lifetime;
   - insert a `pending` row and append `approval_pending` to that session:
     `{ask_id, kind, title, detail, cwd, created_at}` (`detail` goes through the 32 KiB blob
     rule like any event).
2. **Resolutions.** For each `pending` row:
   - `answers/<ask_id>.json` exists → state = its `decision`;
   - else the ask file is gone → state = `withdrawn`;
   - else still pending.

   On a change, update the row and append `approval_resolved` to the session:
   `{ask_id, decision, note}` (`decision` is the new state).
3. **Restart:** pending rows from before a restart carry on being checked as above. Asks that
   appeared while the harness was down are matched normally if their session is `running`
   again; otherwise ignored.

The ask ids, answers and states come from the spool, so the harness is never the source of
truth for a decision: it only reports.

**Commit:** `feat(harness): report agentic approvals as session events`.

## H2.2: show what's waiting

- **Session JSON** gains `waiting_on`: the pending approvals for that session, oldest first,
  `[{ask_id, kind, title, since}]` (empty list when none).
- **New route** `GET /approvals?state=pending` (default `pending`; `all` lists everything,
  newest first, max 200): `[{ask_id, session, kind, title, state, created_at, resolved_at}]`.
- **`/healthz`** gains `pending_approvals: N` and `spool: "<dir>"`.
- **Stopping a session** with a pending approval needs nothing new: stop already sends SIGINT
  to the turn's process group, `agentic` withdraws its ask, and the watcher records
  `withdrawn`.

**Commit:** `feat(harness): waiting_on and pending approvals route`.

---

## Agent checks

`go vet ./...` and `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`. Push `main`. Don't
run it against the box.

## Try it (Sam, on the box)

```sh
git clone git@github.com:hamshoes/harness ~/src/harness && cd ~/src/harness && go install .
harness serve        # in a tmux window

S=~/.local/state/harness/harness.sock
curl --unix-socket $S localhost/healthz
curl --unix-socket $S -X POST localhost/sessions \
  -d '{"name":"hello","dir":"~/src/devbox-tools","prompt":"list the tools in this repo, briefly"}'
curl -N --unix-socket $S 'localhost/sessions/hello/events?follow=1'
curl --unix-socket $S -X POST localhost/sessions/hello/messages -d '{"text":"which is newest?"}'
curl --unix-socket $S -X POST localhost/sessions/hello/stop

# approvals: a session that tries to push
curl --unix-socket $S -X POST localhost/sessions -d '{"name":"push-test","dir":"~/src/scratch",
  "prompt":"git init, make an empty commit, add a remote you can push to, and git push"}'
curl -N --unix-socket $S 'localhost/sessions/push-test/events?follow=1'
# → approval_pending … (Mac dialog) … approval_resolved
curl --unix-socket $S localhost/approvals
```

**Done when** a session answers, remembers turn 1 in turn 2, a follower reconnecting with
`?after=<seq>` picks up exactly where it left off, and killing `harness serve` mid-turn leaves
the session `idle` and resumable, and the push session's log shows the ask and its outcome
(with `waiting_on` set while the dialog is up).

## Hand-over summary

Commits; the `claude` version built against and how `--name` behaved; any
stream-json types that landed as `raw`; the devbox-tools commit you read and the spool fields
and env var you found; anything you had to guess.
