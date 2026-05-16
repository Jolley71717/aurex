package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Image-paste pipeline (k3s adjacent — aurex extension): the browser uploads
// the image bytes via POST /api/paste/image; aurex saves them to PasteDir
// with a timestamped filename and replies with the absolute path. The client
// then types that path into the active terminal so whatever's running
// (Claude Code, vim+iTerm-image-render, etc.) can open the file.
//
// Files older than PasteMaxAge are reaped by a goroutine started in main.

// Accept the handful of formats browsers actually deliver from the clipboard.
// Mapping MIME -> extension; anything else is rejected so the directory can't
// fill with arbitrary uploads.
var pasteAllowedTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/jpg":  "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// 25 MiB cap. Large enough for a phone screenshot, small enough that a
// fat-finger upload can't fill the disk.
const pasteMaxBytes = 25 * 1024 * 1024

// pasteHandler accepts a single image body and returns the absolute path it
// was saved to. The caller (the React client) then sends that path as
// terminal input.
func (s *Server) pasteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ct := r.Header.Get("Content-Type")
	// Strip any parameters (e.g. "image/png; codecs=...").
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	ext, ok := pasteAllowedTypes[strings.ToLower(ct)]
	if !ok {
		http.Error(w, fmt.Sprintf("unsupported content type: %s", ct), http.StatusUnsupportedMediaType)
		return
	}

	// Read with a hard cap so a malicious or buggy client can't OOM us.
	body, err := io.ReadAll(io.LimitReader(r.Body, pasteMaxBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	if len(body) > pasteMaxBytes {
		http.Error(w, fmt.Sprintf("body exceeds %d bytes", pasteMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}

	dir := s.cfg.PasteDir
	if dir == "" {
		dir = "pastes"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, "create paste dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Timestamp-based name. Collisions extremely unlikely (1s resolution +
	// PID suffix). Hex PID keeps the suffix short and ASCII.
	name := fmt.Sprintf("%s-%x.%s", time.Now().Format("2006-01-02-150405"), os.Getpid()&0xffff, ext)
	abs, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		http.Error(w, "resolve path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(abs, body, 0o600); err != nil {
		http.Error(w, "write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("paste: saved %d byte %s image to %s", len(body), ct, abs)

	writeJSON(w, http.StatusOK, map[string]string{"path": abs})
}

// startPasteJanitor reaps paste-dir files older than maxAge once an hour.
// Returns immediately if dir or maxAge are unset.
func startPasteJanitor(dir string, maxAge time.Duration, stop <-chan struct{}) {
	if dir == "" || maxAge <= 0 {
		return
	}
	// Best-effort initial sweep at boot so a long downtime doesn't keep
	// stale files around until the first hour-tick.
	reapPasteDir(dir, maxAge)

	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			reapPasteDir(dir, maxAge)
		}
	}
}

func reapPasteDir(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// dir doesn't exist yet — no work to do.
		return
	}
	cutoff := time.Now().Add(-maxAge)
	reaped := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				reaped++
			}
		}
	}
	if reaped > 0 {
		log.Printf("paste: reaped %d file(s) older than %s from %s", reaped, maxAge, dir)
	}
}
