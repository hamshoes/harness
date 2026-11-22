> **Prompt for the agent** (save as `docs/harness-crews-v1.md`, start a fresh agent in the repo
> root, paste):
>
> ```
> Implement all of docs/harness-crews-v1.md in one go: every milestone, in order, committing
> each, without stopping between them. Read .claude/CLAUDE.md and the earlier specs in docs/
> first. Only stop early for something the spec says to stop and ask about. Finish with the
> hand-over summary.
> ```

# harness: folder tree and crews (v1)

## Context

The harness runs Claude Code sessions on the box: one `claude -p` process per turn (through
`agentic run`), every event in SQLite, a socket API, a web UI on `--http`, approvals reported
from the notify spool, drag-and-drop uploads. Read the existing specs in `docs/` for details.

**Two features:**

1. **Folder tree** for choosing a session's directory, in place of typing a path.
2. **Crews:** customisable agents deployed together into one directory, connected so they can
   message each other. Typical: a planner plans, sends the coder the work, the coder builds and
   reports back. No fixed shape: any agents deployed in one go can message any other.

**How crew messaging works with one process per turn:** a message to a member **becomes that
member's next turn**. If the member is idle, its turn starts now; if it's mid-turn, the message
is queued and delivered when the turn ends. Nobody blocks waiting. The harness is always
running, so members stay reachable even with no process alive.

**Out of scope:** keeping Claude processes alive between turns, members on the Mac, worktrees
per member, Claude Code's native agent teams or cross-session messaging, crews spanning
directories.

## Rules

Everything in `.claude/CLAUDE.md`, plus:
- **Check Claude Code's flags before relying on them** (`claude --help` on the box, ask Sam to
  paste it if needed): `--append-system-prompt`, `--model`, `--allowedTools` /
  `--disallowedTools`, `--permission-mode`, `--mcp-config`, and the subagent file format.
  Stop and ask on any mismatch. Record the version in `runner.go`'s comment.
- No new Go dependencies. The MCP server is hand-written JSON-RPC over stdio.
- `/fs` routes never leave the box user's home.

---

## C1: folder tree

**Flag:** `harness serve … [--roots ~/projects,~/scratch]` (default `~/projects`; each must be
under home and exist).

**API:**
- `GET /fs/roots` → `{roots: [...], recent: [...]}`; `recent` = distinct session dirs,
  newest first, max 8, that still exist.
- `GET /fs/ls?dir=<path>` → directories only, sorted case-insensitively:
  `[{name, path, git: bool, branch: "main"|"" , children: bool}]`.
  - `dir` must be one of the roots or under one (after `EvalSymlinks`), else `403`.
  - Hidden dirs (`.`-prefixed) and `node_modules`, `vendor` are skipped.
  - `git` = has a `.git` dir or file; `branch` from reading `.git/HEAD` (no `git` exec;
    detached → short sha).
  - `children` = has at least one listable subdirectory (stop at the first).
  - Max 1,000 entries, then `truncated: true`.

**UI** (new-session form): the Directory text field stays (mono, editable) and gets a tree panel
under it, 320 px tall, scrolling:
- **Recent** (pinned at the top), then each root as an expandable node; folders load on expand
  (`/fs/ls`), with a chevron only when `children` is true.
- Git repos show a small branch label (`main`) in muted mono.
- Clicking a folder fills the Directory field. Double-click expands.
- **Filter** box above the tree filters the loaded nodes by name, keeping their ancestors.
- **New folder:** a "+" on the selected row inserts an inline name field; Enter appends the
  name to the path in the Directory field (the session creates it, as today).
- Keyboard: arrows move, → expands, ← collapses, Enter selects (focused tree only).

**Commit:** `feat(harness): folder tree for choosing a session dir`.

## C2: agent definitions

**Format: Claude Code's subagent files** (markdown + YAML frontmatter), so the same file works
as a normal subagent too:

```markdown
---
name: planner
description: Breaks the goal into steps and hands work to the coder.
model: opus
tools: Read, Grep, Glob, Bash(git log:*)
permissionMode: default
---
You are the planner. Write the plan to PLAN.md, then send the coder one step at a time…
```

- **Where:** `~/.config/harness/agents/*.md` (yours), then `<session dir>/.claude/agents/*.md`
  (project; overrides by name). **No built-in agents:** Sam writes his own (a planner comes
  next, once this is built).
- **Fields used:** `name` (required, `^[a-z0-9][a-z0-9-]{0,23}$`), `description`, `model`,
  `tools` (→ `--allowedTools`), `permissionMode` (→ `--permission-mode`, overriding the serve
  default), body (→ `--append-system-prompt`). Unknown fields are ignored.
- **Crew tools are always allowed:** add the harness MCP tools (C3) to `--allowedTools` for
  every crew member, whatever `tools` says.
- **API:** `GET /agents?dir=<path>` → `[{name, description, model, source: user|project}]`. Empty list
  → the UI says where to put agent files.

**Commit:** `feat(harness): agent definitions`.

## C3: crews

**Names:** a crew is `^[a-z0-9][a-z0-9-]{0,23}$`. Each member is a normal session named
**`<crew>.<member>`** (e.g. `auth-fix.planner`). Widen the session-name rule to allow that one
dot. A member name defaults to its agent name; deploying two of the same agent gives `coder`,
`coder-2`.

**Store:**

```sql
crews(name TEXT PRIMARY KEY, dir TEXT NOT NULL, goal TEXT NOT NULL, lead TEXT NOT NULL,
      status TEXT NOT NULL,            -- active | stopped
      turns INTEGER NOT NULL DEFAULT 0, max_turns INTEGER NOT NULL,
      created_at TEXT NOT NULL)
crew_queue(id INTEGER PRIMARY KEY, crew TEXT, to_member TEXT, from_member TEXT,
           text TEXT, at TEXT)
```

Sessions gain `crew`, `member` and `agent` (nullable for plain sessions).

**API:**
- `POST /crews {name, dir, goal, lead, members: [{agent, name?}], max_turns?}`: validate
  every agent exists for `dir` and `lead` is a member; create the crew and one idle session per
  member (no turn yet); then send `goal` to the lead as its first message (from `sam`).
  `max_turns` default 60. `201` → the crew.
- `GET /crews`, `GET /crews/{name}` → crew + members (each with the session JSON fields).
- `POST /crews/{name}/stop` → stops every member (as session stop) and marks the crew
  `stopped`. Idempotent.
- `GET /crews/{name}/messages` → every crew message, oldest first (from the members' events).

**Turns for members:** as plain sessions, plus `--model`, `--allowedTools`,
`--permission-mode` and `--append-system-prompt` from the agent, and
`--mcp-config` pointing the `harness` MCP server at `harness mcp --session <name>` (write the
config file under the data dir). Also append a short crew note to the system prompt: the
crew's name, goal, members with their descriptions, and how messaging works.

**`harness mcp --session <name>`** (stdio MCP server, talks to the harness socket). Tools:
- `send(to, text)`: to another member of the same crew (by member name). Text ≤ 8,000 chars.
  Returns `delivered` (started its turn), `queued` (it's mid-turn) or an error.
- `members()`: each member's name, agent, description and status (running/idle/waiting/stopped).

**Delivery rules** (in the sessions package, under the per-session lock):
- Writes `crew_sent {to, text}` to the sender's log and `crew_message {from, text}` to the
  receiver's.
- Receiver **idle or failed** → start its turn now with the message as the prompt.
- Receiver **running** → insert into `crew_queue`. When any member's turn ends, if it has
  queued messages, start its next turn with **all** of them in one prompt, oldest first.
- The prompt for a delivered message is
  `Message from <from> (crew <crew>):\n\n<text>`; several queued ones are joined with a
  blank line between.
- A message from **Sam** to a member (the existing `POST /sessions/{name}/messages`) follows
  the same rules for crew members: queued if running instead of `409`.

**Guards:**
- Only members of the same, active crew; not to self.
- Crew `turns` counts every member turn; at `max_turns` the crew stops itself with a
  `crew_limit` event in every member's log.
- Per receiver: more than 30 messages in 10 minutes → `send` returns an error
  (`too many messages to <to>; batch them`).
- Stopping one member doesn't stop the crew; messages to a stopped member return an error.

**Commit:** `feat(harness): crews with connected members`.

## C4: crews in the UI

**New session form** gets a **Single | Crew** switch at the top.

**Crew mode:**
- **Crew name** (validated as you type), **Directory** (the C1 tree), **Goal** (textarea).
- **Agents:** the list from `GET /agents?dir=` (refreshes when the directory changes): each row
  a checkbox, name, description, model and a source tag; a **Lead** radio on checked rows
  (defaults to the first checked). A small count field (1–3) per row for
  several of the same agent.
- **Max turns** (default 60) under a "More" disclosure.
- **Deploy crew** (⌘↵) → `POST /crews`, then open the crew.

**Sidebar:** crews show as one row (crew name, a status pill, member count) that expands to its
members, each like a session row with the member name. Plain sessions stay as they are.
Filters apply to crews by their members' statuses.

**Crew view** (clicking the crew row): header with crew name, dir, `turns / max`, **Stop
crew**. Body: a **timeline of crew messages** (`GET /crews/{name}/messages`, refreshed on
member events): each as `from → to`, time, and the text (collapsed after 6 lines). Member chips
above it show each member's status; clicking one opens that member's normal session view.

**Member session view:** as today, plus `crew_message` (bubble labelled with the sender,
distinct colour `#2A3442`) and `crew_sent` (muted line `→ coder: <first line>`); `crew_limit`
as a red line. The reply box sends as Sam (queued if running).

**Commit:** `feat(harness): crews in the web ui`.

---

## Agent checks

`go vet ./...` and the cross-build. Push `main`. Don't run it against the box.

## Try it (Sam)

Rebuild and restart. Put two small agent files in `~/.config/harness/agents/` (any two, e.g. a
read-only `lead` that hands work out and a `worker` that does it). In the UI: New → Crew, pick a
repo from the tree, tick both, deploy with a small goal. Watch the lead message the worker, the
worker report back, and the crew timeline fill in. Then Stop crew.

## Hand-over summary

Commits; the Claude Code flags you confirmed; anything you had to guess.
