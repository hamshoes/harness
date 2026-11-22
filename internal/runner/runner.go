// Package runner runs one Claude Code turn as one process and turns its stream-json output into events.
//
// Built against Claude Code 2.1.323 (`claude --version` on the box). Its `--help` lists:
//   - `-n, --name <name>`: display name of the session. Passed on every invocation, including with
//     `--resume` (the help doesn't say whether a resumed session keeps the name, so it is repeated).
//   - `-p`, `--output-format stream-json` (needs `--verbose` with `-p`), `--permission-mode`, `-r, --resume`.
//
// stream-json lines handled here (the headless docs' message shapes):
//
//	{"type":"system","subtype":"init","session_id":…,"model":…,"cwd":…,…}
//	{"type":"assistant","message":{"content":[{"type":"text"|"thinking"|"tool_use",…}]},"parent_tool_use_id":…}
//	{"type":"user","message":{"content":[{"type":"tool_result",…}]},"parent_tool_use_id":…}
//	{"type":"result","subtype":…,"is_error":…,"duration_ms":…,"num_turns":…,"total_cost_usd":…,"result":…}
//
// Everything else (other system subtypes, unknown content blocks, unparseable lines) becomes a raw event.
package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxLine     = 16 << 20 // longest stdout line accepted
	stderrCap   = 200      // stderr lines kept per turn
	drainGrace  = 3 * time.Second
	reapGrace   = 5 * time.Second
	scanBufInit = 64 << 10
)

// EmitFunc stores one event. The runner calls it from more than one goroutine; it must be safe for that.
type EmitFunc func(typ string, data map[string]any)

// Turn is what one turn needs.
type Turn struct {
	Name           string // session name
	Dir            string // working directory
	Prompt         string // fed on stdin
	PermissionMode string
	ResumeID       string // Claude session id to resume; empty on the first turn
	Emit           EmitFunc
}

// Result is how a turn ended.
type Result struct {
	ExitCode  int    // 128+signal when killed by a signal, -1 if it couldn't be waited on
	SessionID string // session_id from the claude_init event, if one was seen
}

// Proc is a running turn.
type Proc struct {
	cmd      *exec.Cmd
	done     chan struct{}
	res      Result
	signaled atomic.Bool // the turn is being torn down: kill what it leaves behind
}

// Start spawns the turn:
//
//	agentic run --name <name> -- claude -p --output-format stream-json --verbose \
//	    --name <name> --permission-mode <mode> [--resume <id>]
//
// in its own process group, with the prompt on stdin.
func Start(t Turn) (*Proc, error) {
	args := []string{"run", "--name", t.Name, "--", "claude",
		"-p", "--output-format", "stream-json", "--verbose",
		"--name", t.Name, "--permission-mode", t.PermissionMode}
	if t.ResumeID != "" {
		args = append(args, "--resume", t.ResumeID)
	}
	cmd := exec.Command("agentic", args...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), "HARNESS_SESSION="+t.Name)
	cmd.Stdin = bytes.NewReader([]byte(t.Prompt))
	cmd.SysProcAttr = sysProcAttr()

	// Our own pipes rather than StdoutPipe: Wait would close those, and a grandchild that inherited
	// the write end could otherwise keep the readers (and the turn) open forever.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}

	p := &Proc{cmd: cmd, done: make(chan struct{})}
	var sessionID string
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		sessionID = readStdout(outR, t.Emit)
	}()
	go func() {
		defer readers.Done()
		readStderr(errR, t.Emit)
	}()
	go func() {
		code := exitCode(cmd.Wait())
		if p.signaled.Load() {
			reapGroup(cmd.Process.Pid)
		}
		drained := make(chan struct{})
		go func() { readers.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainGrace):
			outR.Close() // unblocks the readers
			errR.Close()
			<-drained
		}
		outR.Close()
		errR.Close()
		p.res = Result{ExitCode: code, SessionID: sessionID}
		close(p.done)
	}()
	return p, nil
}

// Signal sends sig to the turn's whole process group. Once the turn's process exits, anything
// left in the group is killed.
func (p *Proc) Signal(sig syscall.Signal) error {
	p.signaled.Store(true)
	return syscall.Kill(-p.cmd.Process.Pid, sig)
}

// Done is closed once the process has exited and its output is read.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Wait blocks until the turn is over.
func (p *Proc) Wait() Result {
	<-p.done
	return p.res
}

// reapGroup gives what is left of a torn-down turn's process group reapGrace to exit on its own (agentic's
// hook uses it to withdraw a pending approval), then kills the rest.
func reapGroup(pgid int) {
	deadline := time.Now().Add(reapGrace)
	for syscall.Kill(-pgid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

func readStderr(r io.Reader, emit EmitFunc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, scanBufInit), maxLine)
	n := 0
	for sc.Scan() {
		n++
		switch {
		case n <= stderrCap:
			emit("stderr", map[string]any{"line": sc.Text()})
		case n == stderrCap+1:
			emit("stderr_truncated", map[string]any{"after_lines": stderrCap})
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		emit("stderr", map[string]any{"line": "harness: reading stderr: " + err.Error()})
	}
}

// readStdout maps every stream-json line to events and returns the init event's session_id.
func readStdout(r io.Reader, emit EmitFunc) string {
	br := bufio.NewReaderSize(r, scanBufInit)
	var sessionID string
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if id := mapLine(line, emit); id != "" {
				sessionID = id
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				emit("raw", map[string]any{"line": "harness: reading stdout: " + err.Error()})
			}
			return sessionID
		}
	}
}

// readLine returns the next line without its newline. A line over maxLine is cut there; the rest is
// discarded so the stream stays in sync.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) <= maxLine {
			buf = append(buf, chunk...)
		} else if len(buf) < maxLine {
			buf = append(buf, chunk[:maxLine-len(buf)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// streamLine is the part of a stream-json line used for routing.
type streamLine struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// mapLine emits the events for one stdout line and returns the session id if it was an init line.
func mapLine(line []byte, emit EmitFunc) string {
	raw := func() { emit("raw", map[string]any{"line": string(line)}) }
	var head streamLine
	var all map[string]any
	if decode(line, &head) != nil || decode(line, &all) != nil {
		raw()
		return ""
	}
	switch {
	case head.Type == "system" && head.Subtype == "init":
		id, _ := all["session_id"].(string)
		emit("claude_init", map[string]any{"session_id": id, "model": all["model"], "cwd": all["cwd"]})
		return id

	case head.Type == "result":
		emit("result", map[string]any{
			"subtype":        all["subtype"],
			"is_error":       all["is_error"],
			"duration_ms":    all["duration_ms"],
			"num_turns":      all["num_turns"],
			"total_cost_usd": all["total_cost_usd"],
			"result":         all["result"],
		})

	case head.Type == "assistant" || head.Type == "user":
		var msg struct {
			Content []map[string]any `json:"content"`
		}
		if decode(head.Message, &msg) != nil || len(msg.Content) == 0 {
			raw() // includes user messages whose content is a plain string
			return ""
		}
		unknown := false
		for _, b := range msg.Content {
			typ, data := mapBlock(head.Type, b)
			if typ == "" {
				unknown = true
				continue
			}
			if head.ParentToolUseID != nil && *head.ParentToolUseID != "" {
				data["parent_tool_use_id"] = *head.ParentToolUseID
			}
			emit(typ, data)
		}
		if unknown {
			raw() // nothing is dropped: the whole line, for the blocks that had no mapping
		}

	default:
		raw()
	}
	return ""
}

// mapBlock maps one content block of an assistant or user message. typ is "" for unmapped blocks.
func mapBlock(role string, b map[string]any) (typ string, data map[string]any) {
	bt, _ := b["type"].(string)
	switch {
	case role == "assistant" && bt == "text":
		return "assistant_text", map[string]any{"text": b["text"]}
	case role == "assistant" && bt == "thinking":
		return "thinking", map[string]any{"text": b["thinking"]}
	case role == "assistant" && bt == "tool_use":
		return "tool_use", map[string]any{"id": b["id"], "name": b["name"], "input": b["input"]}
	case role == "user" && bt == "tool_result":
		isErr, _ := b["is_error"].(bool)
		return "tool_result", map[string]any{"tool_use_id": b["tool_use_id"], "is_error": isErr, "content": b["content"]}
	}
	return "", nil
}
