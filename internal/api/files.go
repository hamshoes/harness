package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hamshoes/harness/internal/sessions"
	"github.com/hamshoes/harness/internal/store"
	"github.com/hamshoes/harness/internal/upload"
)

const maxListedPaths = 200

// files saves the "file" parts of a multipart upload into ?dir= (created if missing), extracting
// zips with ?extract=1 and replacing existing files with ?overwrite=1. With ?session= naming a
// session, its log gets a files_uploaded event.
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("dir") == "" {
		writeError(w, http.StatusBadRequest, "dir is required")
		return
	}
	dir, exists, err := sessions.ExpandDir(q.Get("dir"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !exists {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeError(w, http.StatusBadRequest, "creating dir: "+err.Error())
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, upload.MaxRequest)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart/form-data body with file parts")
		return
	}
	t, err := upload.Open(dir, upload.Options{Extract: flag(q.Get("extract")), Overwrite: flag(q.Get("overwrite"))})
	if err != nil {
		s.fail(w, err)
		return
	}
	defer t.Close()

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil && part.FormName() == "file" {
			err = t.AddFile(part.FileName(), part)
		}
		if err != nil {
			s.uploadFailed(w, dir, t, err)
			return
		}
	}
	s.Log.Info("upload", "dir", dir, "saved", len(t.Saved), "skipped", len(t.Skipped))

	if name := q.Get("session"); name != "" && len(t.Saved) > 0 {
		paths := make([]string, 0, min(len(t.Saved), maxListedPaths))
		for _, f := range t.Saved[:min(len(t.Saved), maxListedPaths)] {
			rel, err := filepath.Rel(dir, f.Path)
			if err != nil {
				rel = f.Path
			}
			paths = append(paths, rel)
		}
		data := map[string]any{"dir": dir, "paths": paths}
		if more := len(t.Saved) - len(paths); more > 0 {
			data["more"] = more
		}
		if err := s.Sessions.Append(name, "files_uploaded", data, nil); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("logging upload", "session", name, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, t.Result)
}

// uploadFailed answers a request whose body couldn't be read: 413 over the size limit, else 400.
// Files already saved stay; the error says how many.
func (s *Server) uploadFailed(w http.ResponseWriter, dir string, t *upload.Target, err error) {
	s.Log.Warn("upload failed", "dir", dir, "saved", len(t.Saved), "err", err)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "upload is over 200 MB", "saved": t.Saved})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "reading upload: " + err.Error(), "saved": t.Saved})
}

func flag(v string) bool {
	return v == "1" || v == "true"
}
