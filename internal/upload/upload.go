// Package upload saves uploaded files, and optionally extracts zips, into one target directory.
//
// Nothing is written outside the target: every operation goes through an os.Root opened on it,
// names are cleaned to a base name (files) or checked for absolute paths and ".." (zip entries),
// and an existing symlink anywhere on the way to a file is never followed. File contents are never
// logged.
package upload

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits.
const (
	MaxRequest    = 200 << 20 // bytes per request
	MaxZipEntries = 5000
	MaxZipBytes   = 1 << 30 // uncompressed bytes per zip
)

// Options are the request's switches.
type Options struct {
	Extract   bool // extract .zip parts instead of saving them
	Overwrite bool // replace existing files
}

// Saved is one file written; Path is absolute.
type Saved struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Skipped is one name not written, and why.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Result is the response body.
type Result struct {
	Dir     string    `json:"dir"`
	Saved   []Saved   `json:"saved"`
	Skipped []Skipped `json:"skipped"`
}

// Target is one upload request's destination.
type Target struct {
	root *os.Root
	opt  Options
	Result
}

// Open opens dir, which must exist, as the target.
func Open(dir string, opt Options) (*Target, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Target{root: root, opt: opt, Result: Result{Dir: dir, Saved: []Saved{}, Skipped: []Skipped{}}}, nil
}

// Close releases the target.
func (t *Target) Close() error { return t.root.Close() }

func (t *Target) skip(name, reason string) {
	t.Skipped = append(t.Skipped, Skipped{Name: name, Reason: reason})
}

// errSource marks a failure reading the request (as opposed to writing a file): the request as a
// whole failed.
type errSource struct{ err error }

func (e errSource) Error() string { return e.err.Error() }
func (e errSource) Unwrap() error { return e.err }

type sourceReader struct{ r io.Reader }

func (s sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		err = errSource{err}
	}
	return n, err
}

// AddFile saves (or, for a .zip with Extract, extracts) one uploaded file. An error means the
// request itself could not be read; problems with this file alone are recorded as skips.
func (t *Target) AddFile(name string, r io.Reader) error {
	base := filepath.Base(filepath.Clean(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || base == "." || base == ".." || base == "/" || base == "" {
		t.skip(name, "bad name")
		_, err := io.Copy(io.Discard, r)
		return err
	}
	if t.opt.Extract && strings.HasSuffix(strings.ToLower(base), ".zip") {
		return t.addZip(base, r)
	}
	err := t.save(base, sourceReader{r})
	var src errSource
	if errors.As(err, &src) {
		return src.err
	}
	if err != nil {
		t.skip(base, err.Error())
		_, err = io.Copy(io.Discard, r)
		return err
	}
	return nil
}

// errSkip is a reason to skip one file.
type errSkip string

func (e errSkip) Error() string { return string(e) }

// checkPath makes sure no existing component on the way to rel is a symlink or a non-directory,
// and that rel itself may be written.
func (t *Target) checkPath(rel string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i < len(parts); i++ {
		fi, err := t.root.Lstat(filepath.FromSlash(strings.Join(parts[:i], "/")))
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the rest will be created
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return errSkip("symlink")
		}
		if !fi.IsDir() {
			return errSkip("not a directory")
		}
	}
	fi, err := t.root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return errSkip("is a directory")
	case !t.opt.Overwrite:
		return errSkip("exists")
	}
	return nil // replaced by rename, which never follows a symlink
}

func tempName(dir string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return filepath.Join(dir, ".upload-"+hex.EncodeToString(b))
}

// save writes r to rel (relative to the target) through a temp file, fsync and rename.
func (t *Target) save(rel string, r io.Reader) error {
	if err := t.checkPath(rel); err != nil {
		return err
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := t.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := tempName(filepath.Dir(rel))
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = t.root.Rename(tmp, rel)
	}
	if err != nil {
		t.root.Remove(tmp)
		return err
	}
	t.Saved = append(t.Saved, Saved{Path: filepath.Join(t.Dir, rel), Bytes: n})
	return nil
}

// entry is one planned zip entry.
type entry struct {
	f    *zip.File
	rel  string // slash-separated, cleaned
	skip string // reason, if it won't be written
}

// addZip spools the zip into a temp file in the target (zip needs random access), checks every entry,
// and only then extracts. An entry that escapes the target rejects the whole zip.
func (t *Target) addZip(name string, r io.Reader) error {
	tmp := tempName(".")
	f, err := t.root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.skip(name, err.Error())
		_, err = io.Copy(io.Discard, r)
		return err
	}
	defer func() {
		f.Close()
		t.root.Remove(tmp)
	}()
	size, err := io.Copy(f, sourceReader{r})
	var src errSource
	if errors.As(err, &src) {
		return src.err
	}
	if err != nil {
		t.skip(name, err.Error())
		_, err = io.Copy(io.Discard, r)
		return err
	}
	zr, err := zip.NewReader(f, size)
	if err != nil {
		t.skip(name, "not a zip")
		return nil
	}
	plan, reason := t.planZip(zr)
	if reason != "" {
		t.skip(name, reason)
		return nil
	}
	for _, e := range plan {
		label := name + ": " + e.rel
		if e.skip != "" {
			t.skip(label, e.skip)
			continue
		}
		rel := filepath.FromSlash(e.rel)
		if e.f.FileInfo().IsDir() {
			if err := t.checkDir(rel); err != nil {
				t.skip(label, err.Error())
			}
			continue
		}
		rc, err := e.f.Open()
		if err != nil {
			t.skip(label, err.Error())
			continue
		}
		err = t.save(rel, rc)
		rc.Close()
		if err != nil {
			t.skip(label, err.Error())
		}
	}
	return nil
}

// planZip checks a zip's entries: limits, unsafe names (which reject the zip), symlinks.
func (t *Target) planZip(zr *zip.Reader) ([]entry, string) {
	if len(zr.File) > MaxZipEntries {
		return nil, "too large"
	}
	var total uint64
	plan := make([]entry, 0, len(zr.File))
	for _, f := range zr.File {
		n := strings.ReplaceAll(f.Name, "\\", "/")
		if n == "" || path.IsAbs(n) || strings.ContainsRune(n, 0) {
			return nil, "unsafe path"
		}
		for _, seg := range strings.Split(n, "/") {
			if seg == ".." {
				return nil, "unsafe path"
			}
		}
		rel := path.Clean(n)
		if rel == "." {
			continue
		}
		total += f.UncompressedSize64
		if total > MaxZipBytes {
			return nil, "too large"
		}
		e := entry{f: f, rel: rel}
		switch {
		case f.Mode()&fs.ModeSymlink != 0:
			e.skip = "symlink"
		case rel == "__MACOSX" || strings.HasPrefix(rel, "__MACOSX/"):
			e.skip = "macOS metadata"
		case !f.Mode().IsRegular() && !f.FileInfo().IsDir():
			e.skip = "not a regular file"
		}
		plan = append(plan, e)
	}
	return plan, ""
}

// checkDir creates a directory entry, refusing to go through symlinks.
func (t *Target) checkDir(rel string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i <= len(parts); i++ {
		fi, err := t.root.Lstat(filepath.FromSlash(strings.Join(parts[:i], "/")))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return errSkip("symlink")
		}
		if !fi.IsDir() {
			return errSkip("not a directory")
		}
	}
	if err := t.root.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	return nil
}
