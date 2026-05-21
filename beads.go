package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// BeadsManager exposes bd CLI operations over HTTP. It shells bd against
// RepoRoot (~/git/k3s-pi by default). The list endpoint caches for 10s
// keyed on the .beads/issues.jsonl mtime+size so repeated polls are cheap.
type BeadsManager struct {
	RepoRoot string
	mu       sync.Mutex
	cache    beadsCache
	events   *beadsEvents
}

type beadsCache struct {
	data    json.RawMessage
	modTime time.Time
	size    int64
	at      time.Time
}

func NewBeadsManager(repoRoot string) *BeadsManager {
	m := &BeadsManager{RepoRoot: repoRoot}
	m.events = newBeadsEvents(m.issuesJSONLPath(), repoRoot)
	return m
}

// RegisterRoutes wires /api/beads/* onto an authenticated chi sub-router.
func (m *BeadsManager) RegisterRoutes(r chi.Router) {
	r.Get("/beads", m.handleList)
	r.Get("/beads/events", m.handleEvents)
	r.Post("/beads", m.handleCreate)
	r.Get("/beads/{id}", m.handleShow)
	r.Patch("/beads/{id}", m.handleUpdate)
	r.Post("/beads/{id}/close", m.handleClose)
	r.Post("/beads/{id}/reopen", m.handleReopen)
	r.Post("/beads/{id}/state", m.handleSetState)
	r.Get("/beads/{id}/comments", m.handleListComments)
	r.Post("/beads/{id}/comments", m.handleAddComment)
}

// --- helpers ---

func (m *BeadsManager) runBd(ctx context.Context, stdin io.Reader, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "bd", args...)
	cmd.Dir = m.RepoRoot
	if stdin != nil {
		cmd.Stdin = stdin
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, ee.ExitCode(), err
		}
		return nil, -1, err
	}
	return out, 0, nil
}

func bdCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// invalidateCache clears the list cache. Called by every mutation.
//
// It also bumps the SSE sequence so any SSE subscriber that re-fetches the
// list after seeing an event won't get a 10s-stale cached body. The fsnotify
// watcher will catch the file change a few ms later and emit the bead event;
// the bump here is the synchronous half of "list-cache and SSE stay coherent."
func (m *BeadsManager) invalidateCache() {
	m.cache = beadsCache{}
	if m.events != nil {
		m.events.bumpSeq()
	}
}

// issuesJSONLPath returns the path to the bd issues file, used for cache key.
func (m *BeadsManager) issuesJSONLPath() string {
	return filepath.Join(m.RepoRoot, ".beads", "issues.jsonl")
}

// cachedList returns the cached list if the underlying file hasn't changed
// within the last 10s; otherwise re-runs bd list --json and updates the cache.
func (m *BeadsManager) cachedList() (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, err := os.Stat(m.issuesJSONLPath())
	if err == nil {
		now := time.Now()
		c := m.cache
		if c.data != nil &&
			now.Sub(c.at) < 10*time.Second &&
			info.ModTime().Equal(c.modTime) &&
			info.Size() == c.size {
			return c.data, nil
		}
	}

	ctx, cancel := bdCtx()
	defer cancel()
	out, _, err := m.runBd(ctx, nil, "list", "--json")
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(bytes.TrimSpace(out))
	if info != nil {
		m.cache = beadsCache{
			data:    raw,
			modTime: info.ModTime(),
			size:    info.Size(),
			at:      time.Now(),
		}
	}
	return raw, nil
}

// --- handlers ---

func (m *BeadsManager) handleList(w http.ResponseWriter, r *http.Request) {
	data, err := m.cachedList()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd list failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (m *BeadsManager) handleShow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := bdCtx()
	defer cancel()
	out, code, err := m.runBd(ctx, nil, "show", id, "--json")
	if err != nil {
		status := http.StatusInternalServerError
		if code == 1 {
			status = http.StatusNotFound
		}
		jsonErr(w, status, "bd show failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

type patchBeadRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Priority    *string `json:"priority"`
}

// handleUpdate applies title/description/priority changes via bd update.
// Description is piped via stdin (bd update --stdin); bd update does not
// accept --description - — stdin is the supported way to pass multiline bodies.
func (m *BeadsManager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req patchBeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Title == nil && req.Description == nil && req.Priority == nil {
		jsonErr(w, http.StatusBadRequest, "nothing to update")
		return
	}

	args := []string{"update", id}
	var stdin io.Reader
	if req.Title != nil {
		args = append(args, "--title", *req.Title)
	}
	if req.Priority != nil {
		args = append(args, "--priority", *req.Priority)
	}
	if req.Description != nil {
		// bd update accepts description via --stdin (alias for --body-file -)
		// rather than --description, which only takes short inline strings.
		args = append(args, "--stdin")
		stdin = bytes.NewBufferString(*req.Description)
	}

	ctx, cancel := bdCtx()
	defer cancel()
	if _, _, err := m.runBd(ctx, stdin, args...); err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd update failed")
		return
	}
	m.mu.Lock()
	m.invalidateCache()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (m *BeadsManager) handleClose(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := bdCtx()
	defer cancel()
	if _, _, err := m.runBd(ctx, nil, "close", id); err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd close failed")
		return
	}
	m.mu.Lock()
	m.invalidateCache()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (m *BeadsManager) handleReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := bdCtx()
	defer cancel()
	if _, _, err := m.runBd(ctx, nil, "reopen", id); err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd reopen failed")
		return
	}
	m.mu.Lock()
	m.invalidateCache()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type setStateRequest struct {
	State string `json:"state"`
}

func (m *BeadsManager) handleSetState(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req setStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.State == "" {
		jsonErr(w, http.StatusBadRequest, "missing state")
		return
	}
	ctx, cancel := bdCtx()
	defer cancel()
	// bd set-state takes a dimension=value label (e.g. patrol=active), not an
	// issue status string. Use `bd update --status` for status transitions.
	if _, _, err := m.runBd(ctx, nil, "update", id, "--status", req.State); err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd update --status failed")
		return
	}
	m.mu.Lock()
	m.invalidateCache()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type createBeadRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Type        string `json:"type"`
}

func (m *BeadsManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createBeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Title == "" {
		jsonErr(w, http.StatusBadRequest, "title required")
		return
	}
	args := []string{"create", req.Title, "--json", "--silent"}
	if req.Description != "" {
		args = append(args, "-d", req.Description)
	}
	if req.Priority != "" {
		args = append(args, "--priority", req.Priority)
	}
	if req.Type != "" {
		args = append(args, "--type", req.Type)
	}
	ctx, cancel := bdCtx()
	defer cancel()
	out, _, err := m.runBd(ctx, nil, args...)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd create failed")
		return
	}
	m.mu.Lock()
	m.invalidateCache()
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(bytes.TrimSpace(out))
}

func (m *BeadsManager) handleListComments(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := bdCtx()
	defer cancel()
	out, code, err := m.runBd(ctx, nil, "comments", id, "--json")
	if err != nil {
		status := http.StatusInternalServerError
		if code == 1 {
			status = http.StatusNotFound
		}
		jsonErr(w, status, "bd comments failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

type addCommentRequest struct {
	Body string `json:"body"`
}

func (m *BeadsManager) handleAddComment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req addCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Body == "" {
		jsonErr(w, http.StatusBadRequest, "body required")
		return
	}
	ctx, cancel := bdCtx()
	defer cancel()
	if _, _, err := m.runBd(ctx, nil, "comment", id, req.Body); err != nil {
		jsonErr(w, http.StatusInternalServerError, "bd comment failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
