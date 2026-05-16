package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// AgentsManager exposes Claude Code's local `claude agents` daemon state to
// the aurex frontend. Claude doesn't ship an HTTP/WS surface — the only
// supply is on-disk JSON under $HOME/.claude/{daemon,jobs}. We poll the
// files on demand and shell out to the `claude` CLI for the two
// imperative actions (`claude logs`, `claude stop`).
//
// The schema files we read are research-preview and have already churned
// since v2.1.139. Every unmarshal here is best-effort: when a field is
// missing or a state file is half-written we still include the agent in
// the response so the user can see "the daemon has it, but aurex can't
// parse it" instead of silently dropping rows. The frontend treats
// status="unparseable" specially.
type AgentsManager struct {
	// DataRoot is typically $HOME/.claude. We derive everything else from it.
	DataRoot string

	// ClaudePath is the absolute path to the `claude` CLI binary we found via
	// exec.LookPath at startup. Empty when not on PATH from this process's
	// launch context (notably a problem under macOS launchd, where PATH is
	// minimal). When empty, /api/agents still works for read but /log and
	// /stop return 503 with a helpful body.
	ClaudePath string

	mu sync.Mutex
}

// AgentInfo is the wire shape rendered by AgentsPanel.jsx.
type AgentInfo struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Summary         string `json:"summary,omitempty"`
	Label           string `json:"label,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	LastUpdateUnix  int64  `json:"last_update_unix"`
	AgeSeconds      int64  `json:"age_seconds"`
}

// NewAgentsManager builds a manager rooted at dataRoot. We do an exec.LookPath
// for `claude` here so we only pay the PATH-walk cost once. Missing CLI is
// not fatal — the read surface still works and the missing-CLI state is
// surfaced to the client via claudeCliAvailable in /api/agents.
func NewAgentsManager(dataRoot string) *AgentsManager {
	m := &AgentsManager{DataRoot: dataRoot}
	if p, err := exec.LookPath("claude"); err == nil {
		m.ClaudePath = p
	}
	return m
}

// RegisterRoutes wires the agents endpoints onto an authenticated chi sub-
// router. Same pattern as IdeasManager — the caller applies auth middleware
// before calling this.
func (m *AgentsManager) RegisterRoutes(r chi.Router) {
	r.Get("/agents", m.handleList)
	r.Get("/agents/{id}/log", m.handleLog)
	r.Post("/agents/{id}/stop", m.handleStop)
}

// --- read path ---

// rosterEntry mirrors the relevant top-level fields in roster.json. We use a
// permissive shape: any field we don't recognise is ignored by encoding/json
// by default (DisallowUnknownFields is off). The Claude team has churned
// this schema once already; do not tighten this up.
type rosterEntry struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Label           string `json:"label"`
	ParentSessionID string `json:"parent_session_id"`
	// Multiple casings have appeared across versions; we try a few.
	UpdatedAt int64 `json:"updated_at"`
	UpdatedTs int64 `json:"updated_ts"`
	LastUpdate int64 `json:"last_update_unix"`
}

// rosterFile is wrapped because roster.json has switched between an array of
// entries and an object with an "agents" key in past versions. We try both.
type rosterFile struct {
	Agents []rosterEntry `json:"agents"`
}

// stateEntry mirrors the relevant fields in jobs/<id>/state.json.
type stateEntry struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	// Some versions nest the haiku summary under "haiku" / "haiku_summary".
	Haiku        string `json:"haiku"`
	HaikuSummary string `json:"haiku_summary"`
	UpdatedAt    int64  `json:"updated_at"`
	UpdatedTs    int64  `json:"updated_ts"`
}

func (m *AgentsManager) rosterPath() string {
	return filepath.Join(m.DataRoot, "daemon", "roster.json")
}

func (m *AgentsManager) jobStatePath(id string) string {
	return filepath.Join(m.DataRoot, "jobs", id, "state.json")
}

// loadRoster reads roster.json with tolerance for both shapes (array or
// object). Returns an empty slice and nil error when the file does not
// exist — first-run UX matters more than strict typing here.
func (m *AgentsManager) loadRoster() ([]rosterEntry, error) {
	data, err := os.ReadFile(m.rosterPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Try object-with-"agents"-array form first.
	var obj rosterFile
	if err := json.Unmarshal(data, &obj); err == nil && len(obj.Agents) > 0 {
		return obj.Agents, nil
	}
	// Fall back to a bare array.
	var arr []rosterEntry
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	// Last-ditch: try to extract IDs from a map keyed by ID. Some early
	// versions wrote {"<id>": {...}, "<id2>": {...}} at the top level.
	var asMap map[string]rosterEntry
	if err := json.Unmarshal(data, &asMap); err == nil {
		out := make([]rosterEntry, 0, len(asMap))
		for k, v := range asMap {
			if v.ID == "" {
				v.ID = k
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("roster.json: unrecognised shape")
}

// loadState returns the parsed state.json for an agent, or an error if the
// file is missing/unreadable/unparseable. Callers fall back gracefully.
func (m *AgentsManager) loadState(id string) (stateEntry, error) {
	data, err := os.ReadFile(m.jobStatePath(id))
	if err != nil {
		return stateEntry{}, err
	}
	var s stateEntry
	if err := json.Unmarshal(data, &s); err != nil {
		return stateEntry{}, err
	}
	return s, nil
}

// statusPriority orders agents so the most actionable rows float to the top
// of the list. Anything not in the map sorts last (alphabetical tie-break
// by id is applied separately).
func statusPriority(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "running", "active":
		return 0
	case "needs-input", "needs_input", "input-required", "input_required", "waiting-input":
		return 1
	case "done", "completed", "success", "finished":
		return 2
	case "failed", "error", "errored", "crashed":
		return 3
	case "unparseable":
		return 4
	default:
		return 5
	}
}

// build assembles the AgentInfo wire shape for one roster entry, layering in
// state.json + Haiku summary when available.
func (m *AgentsManager) build(now time.Time, e rosterEntry) AgentInfo {
	info := AgentInfo{
		ID:              e.ID,
		Status:          strings.ToLower(strings.TrimSpace(e.Status)),
		Label:           e.Label,
		ParentSessionID: e.ParentSessionID,
	}
	// Pick the freshest of the timestamp fields the roster might have used.
	for _, t := range []int64{e.UpdatedAt, e.UpdatedTs, e.LastUpdate} {
		if t > info.LastUpdateUnix {
			info.LastUpdateUnix = t
		}
	}

	if state, err := m.loadState(e.ID); err == nil {
		// state.json status wins over roster when present — it's updated
		// per-tick, roster only when the supervisor flushes.
		if s := strings.ToLower(strings.TrimSpace(state.Status)); s != "" {
			info.Status = s
		}
		summary := firstNonEmpty(state.Summary, state.HaikuSummary, state.Haiku)
		info.Summary = strings.TrimSpace(summary)
		for _, t := range []int64{state.UpdatedAt, state.UpdatedTs} {
			if t > info.LastUpdateUnix {
				info.LastUpdateUnix = t
			}
		}
	} else if !os.IsNotExist(err) {
		// File exists but failed to parse — flag it so the user sees it.
		info.Status = "unparseable"
	}

	if info.Status == "" {
		info.Status = "unknown"
	}

	if info.LastUpdateUnix > 0 {
		info.AgeSeconds = now.Unix() - info.LastUpdateUnix
		if info.AgeSeconds < 0 {
			info.AgeSeconds = 0
		}
	}
	return info
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// handleList serves GET /api/agents. Always returns 200 with an envelope —
// an empty list when the daemon files aren't present yet is the correct
// first-run shape (the panel renders an empty state explaining what to do).
func (m *AgentsManager) handleList(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	envelope := map[string]any{
		"agents":             []AgentInfo{},
		"claudeCliAvailable": m.ClaudePath != "",
		"dataRoot":           m.DataRoot,
	}

	roster, err := m.loadRoster()
	if err != nil {
		envelope["error"] = err.Error()
		writeJSON(w, http.StatusOK, envelope)
		return
	}

	now := time.Now()
	out := make([]AgentInfo, 0, len(roster))
	for _, e := range roster {
		if e.ID == "" {
			continue
		}
		out = append(out, m.build(now, e))
	}

	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := statusPriority(out[i].Status), statusPriority(out[j].Status)
		if pi != pj {
			return pi < pj
		}
		if out[i].LastUpdateUnix != out[j].LastUpdateUnix {
			return out[i].LastUpdateUnix > out[j].LastUpdateUnix
		}
		return out[i].ID < out[j].ID
	})

	envelope["agents"] = out
	writeJSON(w, http.StatusOK, envelope)
}

// handleLog serves GET /api/agents/{id}/log. Shells out to `claude logs <id>`
// with a short timeout. Returns text/plain.
func (m *AgentsManager) handleLog(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if !validAgentID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if m.ClaudePath == "" {
		http.Error(w, "claude CLI not found on aurex's PATH — run `which claude` from a terminal and confirm it resolves to a binary the aurex process can see", http.StatusServiceUnavailable)
		return
	}
	lines := 200
	if q := r.URL.Query().Get("lines"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}

	// Tight timeout — the panel renders this inline and we don't want to
	// pin a tab waiting on a stuck CLI invocation. The spec says 200ms; in
	// practice `claude logs` reads a file so that's plenty.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.ClaudePath, "logs", id, fmt.Sprintf("--tail=%d", lines))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Treat exit code != 0 as a 502 with the captured output as the body
		// so the user can see what went wrong (typical: "unknown agent id").
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "claude logs %s failed: %v\n\n%s", id, err, string(out))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(out)
}

// handleStop serves POST /api/agents/{id}/stop. Shells out to `claude stop
// <id>`. Returns 204 on success, 502 on shell error.
func (m *AgentsManager) handleStop(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if !validAgentID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if m.ClaudePath == "" {
		http.Error(w, "claude CLI not found on aurex's PATH", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.ClaudePath, "stop", id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "claude stop %s failed: %v\n\n%s", id, err, string(out))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validAgentID restricts the IDs we'll pass to `exec.Command` to a
// conservative character class. Even though we use exec.CommandContext
// (no shell interpretation), keeping this strict is cheap defense in depth
// against a future refactor that adds a shell hop.
func validAgentID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
