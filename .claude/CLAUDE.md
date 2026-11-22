# Working in this repo

`harness` owns Claude Code sessions on the box: `harness serve` exposes a JSON API on a Unix socket,
runs Claude turns headless, stores every event in SQLite, and reports `agentic`'s approval holds
(read from the notify spool) in each session's log.

## How to work

- Specs live in `docs/` (Sam's; never edit them). Implement them milestone by milestone, commit each,
  and stop and ask on gaps or on anything a spec doesn't cover.
- Work on `main` and push it. Nothing releases from this repo yet.
- No tests, no README (Sam writes it), no CI unless a spec asks. Never run docker, gcloud or ssh against the VM.
- Pre-alpha, one user: no backward compatibility, no migrations framework (create tables if missing).
- Generic language in code: the remote side is **the box**, the Mac binary is **the client**. Never write
  the product name.
- `CGO_ENABLED=0`; check with `go vet ./...` and `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`.
- Dependencies: standard library (`net/http` with Go 1.22+ routing) and `modernc.org/sqlite`. Nothing else
  without asking.
- Logs: never log tokens. Prompt and message bodies only at debug level.
- Claude Code flags and stream-json vary by version. Read `claude --help` and the headless docs before
  changing the runner; the version it was built against is noted in `internal/runner/runner.go`.
  `--name` is mandatory on every `claude` invocation. If the installed version disagrees with a spec,
  stop and ask.
- Spool: check formats in the devbox-tools source before relying on them (how `agentic` writes ask files,
  how the notify pair writes answers, the spool's environment variable); the commit read is noted in
  `internal/approvals/approvals.go`. If something disagrees with the source, stop and ask. Never write,
  rename, delete or `chmod` anything in the spool. Never log an ask's `detail`. Poll; no file-watching library.

## Layout

```
main.go              # `harness serve`
internal/store       # SQLite: sessions, events; blob files
internal/runner      # one turn = one process; stream-json → events
internal/sessions    # session state machine, per-session lock, live fan-out
internal/approvals   # spool watcher: approval_pending / approval_resolved events
internal/upload      # POST /files: save uploads (and extract zips) inside one directory, never outside it
internal/api         # HTTP handlers (Unix socket, and TCP with --http)
web/                 # the page: index.html, app.css, app.js, embedded; plain JS, no build step
docs/                # specs
```

`agentic` (wraps agent runs: logs Bash calls, gates `git push`) and `claude` are runtime binaries on the
box's `PATH`, called with `os/exec`. They are not imports; there is no code dependency on their repos.
