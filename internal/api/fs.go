package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hamshoes/harness/internal/sessions"
)

const (
	maxListed   = 1000
	maxRecent   = 8
	dirsPerRead = 64
)

// skippedDirs are never listed, besides hidden ones.
var skippedDirs = map[string]bool{"node_modules": true, "vendor": true}

// Root is a directory the folder tree may browse: Path as configured (after ~ expansion), Real after
// EvalSymlinks.
type Root struct {
	Path string
	Real string
}

// ParseRoots turns the --roots flag (comma-separated) into roots. Each must exist, be a directory and
// lie under home, also after resolving symlinks.
func ParseRoots(flag string) ([]Root, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	var out []Root
	for p := range strings.SplitSeq(flag, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		dir, exists, err := sessions.ExpandDir(p)
		if err != nil {
			return nil, fmt.Errorf("--roots %s: %w", p, err)
		}
		if !exists {
			return nil, fmt.Errorf("--roots %s: no such directory", p)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, fmt.Errorf("--roots %s: %w", p, err)
		}
		if !within(realHome, real) || !within(home, dir) {
			return nil, fmt.Errorf("--roots %s: not under %s", p, home)
		}
		out = append(out, Root{Path: dir, Real: real})
	}
	if len(out) == 0 {
		return nil, errors.New("--roots: no directories given")
	}
	return out, nil
}

// within reports whether path is base or under it. Both must be clean and absolute.
func within(base, path string) bool {
	return path == base || strings.HasPrefix(path, strings.TrimSuffix(base, "/")+"/")
}

// allowed reports whether a resolved path lies within one of the roots.
func (s *Server) allowed(real string) bool {
	for _, r := range s.Roots {
		if within(r.Real, real) {
			return true
		}
	}
	return false
}

func (s *Server) fsRoots(w http.ResponseWriter, r *http.Request) {
	roots := make([]string, 0, len(s.Roots))
	for _, x := range s.Roots {
		roots = append(roots, x.Path)
	}
	recent := []string{}
	if xs, err := s.Sessions.List(); err == nil {
		home, _ := os.UserHomeDir()
		for _, x := range xs { // newest first
			if len(recent) == maxRecent {
				break
			}
			if slices.Contains(recent, x.Dir) || !within(home, x.Dir) {
				continue
			}
			if fi, err := os.Stat(x.Dir); err == nil && fi.IsDir() {
				recent = append(recent, x.Dir)
			}
		}
	} else {
		s.Log.Error("listing sessions for recent dirs", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots, "recent": recent})
}

// FSEntry is one directory in a listing.
type FSEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Git      bool   `json:"git"`
	Branch   string `json:"branch"`
	Children bool   `json:"children"`
}

// fsLs lists the subdirectories of ?dir=, which must be a root or under one after resolving symlinks.
func (s *Server) fsLs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("dir")
	if q == "" {
		writeError(w, http.StatusBadRequest, "dir is required")
		return
	}
	dir := q
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			s.fail(w, err)
			return
		}
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		writeError(w, http.StatusBadRequest, "dir must be absolute or start with ~/")
		return
	}
	dir = filepath.Clean(dir)
	// Nothing about a path outside the roots is told, not even whether it exists.
	forbidden := func() { writeError(w, http.StatusForbidden, "dir is outside the browsable roots") }
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		lexical := false
		for _, r := range s.Roots {
			lexical = lexical || within(r.Path, dir)
		}
		if lexical && errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "no such directory")
		} else {
			forbidden()
		}
		return
	}
	if !s.allowed(real) {
		forbidden()
		return
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		writeError(w, http.StatusBadRequest, "not a directory")
		return
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		writeError(w, http.StatusForbidden, "reading dir: "+err.Error())
		return
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name())); c != 0 {
			return c
		}
		return strings.Compare(a.Name(), b.Name())
	})
	out := []FSEntry{}
	truncated := false
	for _, e := range entries {
		sub, ok := s.listable(real, e)
		if !ok {
			continue
		}
		if len(out) == maxListed {
			truncated = true
			break
		}
		x := FSEntry{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Children: s.hasChildren(sub)}
		x.Git, x.Branch = gitInfo(sub)
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "entries": out, "truncated": truncated})
}

// listable reports whether entry e of the resolved dir parent shows in the tree, and its resolved path:
// a directory, not hidden or skipped; a symlink only if it points at a directory inside the roots.
func (s *Server) listable(parent string, e os.DirEntry) (string, bool) {
	name := e.Name()
	if strings.HasPrefix(name, ".") || skippedDirs[name] {
		return "", false
	}
	p := filepath.Join(parent, name)
	if e.IsDir() {
		return p, true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !s.allowed(real) {
		return "", false
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return real, true
}

// hasChildren reports whether a resolved dir has at least one listable subdirectory, reading no further
// than the first.
func (s *Server) hasChildren(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		es, err := f.ReadDir(dirsPerRead)
		for _, e := range es {
			if _, ok := s.listable(dir, e); ok {
				return true
			}
		}
		if err != nil { // io.EOF at the end
			return false
		}
	}
}

// gitInfo reports whether dir is a git checkout (a .git dir or file) and its branch, from HEAD: the
// branch name, a short sha when detached, "" when unreadable. It never runs git.
func gitInfo(dir string) (bool, string) {
	dotGit := filepath.Join(dir, ".git")
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return false, ""
	}
	gitDir := dotGit
	if !fi.IsDir() { // a worktree or submodule: "gitdir: <path>"
		line, err := firstLine(dotGit)
		if err != nil || !strings.HasPrefix(line, "gitdir:") {
			return true, ""
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
	}
	head, err := firstLine(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return true, ""
	}
	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		return true, strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	}
	if len(head) >= 7 {
		return true, head[:7]
	}
	return true, ""
}

func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
