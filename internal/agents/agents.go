// Package agents reads agent definitions: Claude Code subagent files (markdown with YAML frontmatter),
// from the user's directory and then a project's .claude/agents, the project's overriding by name.
//
// Format checked against Claude Code 2.1.323's agent-file loader: name, description (Claude Code
// itself refuses a file without one; here it is optional), model ("inherit" = the default), tools (a
// comma-separated string or a YAML list), permissionMode, and the body as the system prompt. Other
// fields are ignored.
package agents

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Sources.
const (
	SourceUser    = "user"
	SourceProject = "project"
)

// NameRE is what an agent name must match.
var NameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)

// PermissionModes are the values Claude Code 2.1.323's --permission-mode accepts ("default" is
// accepted too, though its help lists "manual").
var PermissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "manual", "plan"}

const maxFile = 256 << 10

// Agent is one definition.
type Agent struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Model          string   `json:"model"`
	Source         string   `json:"source"`
	Tools          []string `json:"-"`
	PermissionMode string   `json:"-"`
	Prompt         string   `json:"-"`
	Path           string   `json:"-"`
}

// UserDir is where the user's agent files live: ~/.config/harness/agents.
func UserDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "harness", "agents"), nil
}

// ProjectDir is where a project's agent files live.
func ProjectDir(dir string) string {
	return filepath.Join(dir, ".claude", "agents")
}

// Load returns the agents for a session dir (empty: the user's only), sorted by name. Files that don't
// parse are logged and skipped.
func Load(dir string, log *slog.Logger) ([]Agent, error) {
	user, err := UserDir()
	if err != nil {
		return nil, err
	}
	byName := map[string]Agent{}
	readDir(user, SourceUser, byName, log)
	if dir != "" {
		readDir(ProjectDir(dir), SourceProject, byName, log)
	}
	out := make([]Agent, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Agent) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Find returns one agent for a session dir.
func Find(dir, name string, log *slog.Logger) (Agent, bool, error) {
	all, err := Load(dir, log)
	if err != nil {
		return Agent{}, false, err
	}
	for _, a := range all {
		if a.Name == name {
			return a, true, nil
		}
	}
	return Agent{}, false, nil
}

func readDir(dir, source string, into map[string]Agent, log *slog.Logger) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return
	}
	slices.Sort(paths)
	for _, p := range paths {
		a, err := ReadFile(p)
		if err != nil {
			log.Warn("skipping agent file", "path", p, "err", err)
			continue
		}
		a.Source = source
		into[a.Name] = a
	}
}

// ReadFile parses one agent file.
func ReadFile(path string) (Agent, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Agent{}, err
	}
	if !fi.Mode().IsRegular() {
		return Agent{}, errors.New("not a regular file")
	}
	if fi.Size() > maxFile {
		return Agent{}, errors.New("file is too large")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Agent{}, err
	}
	front, body, err := splitFrontmatter(string(b))
	if err != nil {
		return Agent{}, err
	}
	fields, err := parseFrontmatter(front)
	if err != nil {
		return Agent{}, err
	}
	a := Agent{Path: path, Prompt: strings.TrimSpace(body)}
	a.Name, _ = fields["name"].(string)
	if !NameRE.MatchString(a.Name) {
		return Agent{}, fmt.Errorf("name %q must match %s", a.Name, NameRE)
	}
	a.Description, _ = fields["description"].(string)
	a.Description = strings.ReplaceAll(a.Description, `\n`, "\n")
	if m, _ := fields["model"].(string); m != "" && !strings.EqualFold(m, "inherit") {
		a.Model = m
	}
	switch t := fields["tools"].(type) {
	case string:
		a.Tools = splitTools(t)
	case []string:
		for _, x := range t {
			a.Tools = append(a.Tools, splitTools(x)...)
		}
	}
	if pm, _ := fields["permissionMode"].(string); pm != "" {
		if !slices.Contains(PermissionModes, pm) {
			return Agent{}, fmt.Errorf("permissionMode %q is not one of %s", pm, strings.Join(PermissionModes, ", "))
		}
		a.PermissionMode = pm
	}
	return a, nil
}

func splitFrontmatter(s string) (front, body string, err error) {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", "", errors.New("no frontmatter (the file must start with ---)")
	}
	rest := s[4:]
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		return "", strings.TrimPrefix(rest, "---"), nil
	}
	i := strings.Index(rest, "\n---\n")
	if i < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return rest[:len(rest)-4], "", nil
		}
		return "", "", errors.New("frontmatter is not closed with ---")
	}
	return rest[:i], rest[i+5:], nil
}

// parseFrontmatter reads the small YAML subset agent files use: top-level `key: value` pairs, values
// plain or quoted, `|` and `>` block scalars, and block lists (`- item`). Values are strings, or []string
// for lists.
func parseFrontmatter(src string) (map[string]any, error) {
	out := map[string]any{}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 64<<10), maxFile)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	indented := func(l string) bool { return l != "" && (l[0] == ' ' || l[0] == '\t') }
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if indented(line) {
			continue // part of a structure this reader doesn't use
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("frontmatter line %d: expected key: value", i+1)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// gather the indented (or list) lines that follow
		var block []string
		for i+1 < len(lines) && (indented(lines[i+1]) || strings.TrimSpace(lines[i+1]) == "" || strings.HasPrefix(lines[i+1], "- ")) {
			i++
			block = append(block, lines[i])
		}
		switch {
		case val == "" && hasListItems(block):
			var items []string
			for _, b := range block {
				if it, ok := strings.CutPrefix(strings.TrimSpace(b), "- "); ok {
					items = append(items, unquote(strings.TrimSpace(it)))
				}
			}
			out[key] = items
		case strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">"):
			out[key] = blockScalar(block, val[0] == '>')
		case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
			var items []string
			for _, it := range strings.Split(val[1:len(val)-1], ",") {
				if it = unquote(strings.TrimSpace(it)); it != "" {
					items = append(items, it)
				}
			}
			out[key] = items
		default:
			v := unquote(stripComment(val))
			for _, b := range block { // a plain scalar folded over several lines
				if s := strings.TrimSpace(b); s != "" {
					v += " " + s
				}
			}
			out[key] = v
		}
	}
	return out, nil
}

func hasListItems(block []string) bool {
	for _, b := range block {
		if strings.HasPrefix(strings.TrimSpace(b), "- ") {
			return true
		}
	}
	return false
}

func blockScalar(block []string, folded bool) string {
	indent := -1
	for _, b := range block {
		if strings.TrimSpace(b) == "" {
			continue
		}
		n := len(b) - len(strings.TrimLeft(b, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	var parts []string
	for _, b := range block {
		if len(b) >= indent && indent >= 0 {
			b = b[indent:]
		} else {
			b = strings.TrimSpace(b)
		}
		parts = append(parts, b)
	}
	sep := "\n"
	if folded {
		sep = " "
	}
	return strings.TrimSpace(strings.Join(parts, sep))
}

func stripComment(v string) string {
	if v == "" || v[0] == '"' || v[0] == '\'' {
		return v
	}
	if i := strings.Index(v, " #"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return v
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		inner := v[1 : len(v)-1]
		if v[0] == '\'' {
			return strings.ReplaceAll(inner, "''", "'")
		}
		r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t")
		return r.Replace(inner)
	}
	return v
}

// splitTools splits a tools string on commas outside parentheses: "Read, Bash(git log:*)".
func splitTools(s string) []string {
	var out []string
	depth, start := 0, 0
	add := func(t string) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				add(s[start:i])
				start = i + 1
			}
		}
	}
	add(s[start:])
	return out
}
