// Package mcp is the crew members' MCP server: `harness mcp --session <crew>.<member>`, started by
// Claude Code over stdio for every member turn. It speaks JSON-RPC 2.0, one message per line, and
// answers its tools by calling the harness API on the Unix socket.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// protocolVersions are the MCP revisions this server can speak, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const maxMessage = 4 << 20

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type server struct {
	crew, member string
	version      string
	client       *http.Client
}

// Serve runs the server until in ends.
func Serve(session, socket, version string, in io.Reader, out io.Writer) error {
	crew, member, ok := strings.Cut(session, ".")
	if !ok || crew == "" || member == "" {
		return fmt.Errorf("session %q is not a crew member (<crew>.<member>)", session)
	}
	s := &server{crew: crew, member: member, version: version, client: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
	br := bufio.NewReaderSize(in, 64<<10)
	w := bufio.NewWriter(out)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if len(line) > maxMessage {
				s.write(w, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "message too large"}})
			} else if resp, ok := s.handle(line); ok {
				s.write(w, resp)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *server) write(w *bufio.Writer, r response) {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: r.ID, Error: &rpcError{-32603, err.Error()}})
	}
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

// handle answers one message; ok is false for notifications.
func (s *server) handle(line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}, true
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return response{}, false // a notification (initialized, cancelled, …): nothing to answer
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := protocolVersions[0]
		for _, x := range protocolVersions {
			if x == p.ProtocolVersion {
				v = x
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "harness", "version": s.version},
			"instructions": fmt.Sprintf("You are %q in crew %q. send(to, text) messages another member; members() lists the crew.",
				s.member, s.crew),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		text, err := s.call(p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			resp.Error = &rpcError{-32602, err.Error()}
			break
		}
		if err != nil {
			resp.Result = toolResult(err.Error(), true)
		} else {
			resp.Result = toolResult(text, false)
		}
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp, true
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name": "send",
			"description": "Send a message to another member of your crew, by member name. It becomes their next turn: " +
				"it starts now if they are idle (returns \"delivered\") or when their current turn ends (\"queued\"). " +
				"Don't wait for a reply; replies arrive as your next turn.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":   map[string]any{"type": "string", "description": "the member's name, as members() lists it"},
					"text": map[string]any{"type": "string", "description": "the message, self-contained", "maxLength": 8000},
				},
				"required":             []string{"to", "text"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "members",
			"description": "List your crew's members: name, agent, description and status (running, idle, waiting, failed, stopped).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
	}
}

var errUnknownTool = errors.New("unknown tool")

func (s *server) call(name string, args json.RawMessage) (string, error) {
	switch name {
	case "send":
		var a struct{ To, Text string }
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", errors.New("arguments must be {to, text}")
			}
		}
		if a.To == "" || strings.TrimSpace(a.Text) == "" {
			return "", errors.New("to and text are required")
		}
		var out struct {
			Result string `json:"result"`
		}
		err := s.api(http.MethodPost, "/crews/"+url.PathEscape(s.crew)+"/send",
			map[string]string{"from": s.member, "to": a.To, "text": a.Text}, &out)
		return out.Result, err
	case "members":
		var c struct {
			Lead    string `json:"lead"`
			Members []struct {
				Member      string `json:"member"`
				Agent       string `json:"agent"`
				Description string `json:"description"`
				Status      string `json:"status"`
				WaitingOn   []any  `json:"waiting_on"`
			} `json:"members"`
		}
		if err := s.api(http.MethodGet, "/crews/"+url.PathEscape(s.crew), nil, &c); err != nil {
			return "", err
		}
		type row struct {
			Name        string `json:"name"`
			Agent       string `json:"agent"`
			Description string `json:"description"`
			Status      string `json:"status"`
			Lead        bool   `json:"lead,omitempty"`
			You         bool   `json:"you,omitempty"`
		}
		rows := make([]row, 0, len(c.Members))
		for _, m := range c.Members {
			st := m.Status
			if st == "running" && len(m.WaitingOn) > 0 {
				st = "waiting"
			}
			rows = append(rows, row{m.Member, m.Agent, m.Description, st, m.Member == c.Lead, m.Member == s.member})
		}
		b, err := json.MarshalIndent(rows, "", "  ")
		return string(b), err
	}
	return "", fmt.Errorf("%w: %s", errUnknownTool, name)
}

// api calls the harness API; a non-2xx answer becomes its error message.
func (s *server) api(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://harness"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("harness is unreachable: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxMessage))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("harness answered %s", res.Status)
	}
	return json.Unmarshal(b, out)
}
