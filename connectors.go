package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// Connectors are aurex's typed registry of external systems it integrates with
// (Paperclip, Beads, Gas City, Ollama, ...). Before this surface existed each
// integration was hard-coded as its own tab and source-toggle, which didn't
// scale — adding a new system meant editing the nav, the routing layer, and
// each consumer surface. The Connectors page is the single source of truth:
// register a Connector with {type, name, url, tokenRef}, and downstream
// surfaces (Agents, Fleet, Inbox) discover capabilities through the registry.
//
// Storage: a flat JSON file at $AUREX_DIR/aurex.connectors.json (mode 0600).
// Defaults are seeded on first boot from the existing config so an upgrade
// is invisible to users — Paperclip / Beads / Gas City connectors appear
// pre-filled if their corresponding config keys were populated.
//
// Tokens are stored by REFERENCE — either an absolute file path that the Go
// process reads at probe time (mode-0600 enforced) or a 1Password identifier
// the user resolves out of band. We never put a token value in the JSON.

// ConnectorType is the discriminator that picks the driver. New types extend
// the switch in (ConnectorsManager).probe.
type ConnectorType string

const (
	ConnectorTypePaperclip ConnectorType = "paperclip"
	ConnectorTypeBeads     ConnectorType = "beads"
	ConnectorTypeGasCity   ConnectorType = "gascity"
	ConnectorTypeOllama    ConnectorType = "ollama"
	ConnectorTypeHubspace  ConnectorType = "hubspace"
)

// Capability is what a Connector advertises it can do. Surfaces filter the
// registry by capability — e.g. the Agents page lists connectors with
// `agents`, the LLM picker lists `llm`.
type Capability string

const (
	CapAgents     Capability = "agents"
	CapIssues     Capability = "issues"
	CapIdeas      Capability = "ideas"
	CapLLM        Capability = "llm"
	CapDashboard  Capability = "dashboard"
	CapSupervisor Capability = "supervisor"
	CapControl    Capability = "control"  // actuate a device (e.g. open a spigot)
	CapSensor     Capability = "sensor"   // read device readings (battery, state)
	CapSchedule   Capability = "schedule" // create/read recurring schedules
)

// Health is the cached probe result. UpdatedUnix is unset (0) until the first
// probe completes — surfaces should render "checking..." in that window.
type Health struct {
	OK          bool   `json:"ok"`
	Status      string `json:"status"`           // e.g. "healthy", "down", "unauthorized"
	Detail      string `json:"detail,omitempty"` // short human-readable, max 200 chars
	UpdatedUnix int64  `json:"updated_unix"`
}

// Connector is the wire shape stored in aurex.connectors.json AND rendered
// by ConnectorsSettings.jsx. Tokens never appear here in cleartext — only
// the reference.
type Connector struct {
	ID           string        `json:"id"`
	Type         ConnectorType `json:"type"`
	Name         string        `json:"name"`
	URL          string        `json:"url,omitempty"`
	TokenRef     string        `json:"token_ref,omitempty"` // e.g. "/tmp/pcp-token"
	Enabled      bool          `json:"enabled"`
	Capabilities []Capability  `json:"capabilities,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"` // type-specific (e.g. repoRoot for beads)
	Health       Health        `json:"health"`
}

// ConnectorsManager owns the in-memory list, file persistence, and the
// background health-refresh loop. Construct one in main.go and pass to
// Server; the routes and the refresh goroutine share it.
type ConnectorsManager struct {
	Path   string // absolute path to aurex.connectors.json

	mu         sync.RWMutex
	connectors []*Connector

	httpClient *http.Client
}

// NewConnectorsManager loads (or seeds) the registry. `defaults` is a list
// of Connectors to write on first boot — typically derived from the existing
// aurex.config.json so users see Paperclip / Beads pre-populated.
func NewConnectorsManager(path string, defaults []Connector) (*ConnectorsManager, error) {
	m := &ConnectorsManager{
		Path: path,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	if err := m.load(); err != nil {
		return nil, fmt.Errorf("connectors: load: %w", err)
	}
	// Seed defaults if the file was missing or empty.
	if len(m.connectors) == 0 && len(defaults) > 0 {
		for i := range defaults {
			c := defaults[i]
			if c.ID == "" {
				c.ID = c.string("id")
			}
			m.connectors = append(m.connectors, &c)
		}
		if err := m.persist(); err != nil {
			return nil, fmt.Errorf("connectors: seed defaults: %w", err)
		}
	}
	return m, nil
}

// string is a small helper for deterministic ID derivation. Not exported.
func (c Connector) string(field string) string {
	if field == "id" {
		return fmt.Sprintf("%s-%s", c.Type, strings.ToLower(strings.ReplaceAll(c.Name, " ", "-")))
	}
	return ""
}

// load reads aurex.connectors.json. Missing file is not an error — the
// caller's defaults handler kicks in.
func (m *ConnectorsManager) load() error {
	b, err := os.ReadFile(m.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var arr []*Connector
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("parse %s: %w", m.Path, err)
	}
	m.mu.Lock()
	m.connectors = arr
	m.mu.Unlock()
	return nil
}

// persist writes the registry atomically (tmp + rename) at mode 0600. The
// caller holds m.mu.
func (m *ConnectorsManager) persist() error {
	dir := filepath.Dir(m.Path)
	tmp, err := os.CreateTemp(dir, ".connectors-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Always try to clean up on error path. If rename succeeded, the
		// remove fails harmlessly.
		_ = os.Remove(tmpName)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m.connectors); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, m.Path)
}

// List returns a snapshot of the registry. Health is included as last cached.
func (m *ConnectorsManager) List() []*Connector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Connector, len(m.connectors))
	for i, c := range m.connectors {
		// Shallow copy so the caller can serialize without holding the lock.
		cp := *c
		out[i] = &cp
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type == out[j].Type {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// Get returns the connector with the given id, or nil.
func (m *ConnectorsManager) Get(id string) *Connector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.connectors {
		if c.ID == id {
			cp := *c
			return &cp
		}
	}
	return nil
}

// Upsert adds a new connector or replaces an existing one matching by ID.
// Returns the persisted (potentially ID-assigned) connector.
func (m *ConnectorsManager) Upsert(in Connector) (*Connector, error) {
	if in.Type == "" {
		return nil, fmt.Errorf("connectors: type is required")
	}
	if in.Name == "" {
		return nil, fmt.Errorf("connectors: name is required")
	}
	if in.ID == "" {
		in.ID = in.string("id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.connectors {
		if c.ID == in.ID {
			// Preserve last-known health on update so the UI doesn't blink to
			// "unknown" while waiting for the next probe.
			in.Health = c.Health
			m.connectors[i] = &in
			if err := m.persist(); err != nil {
				return nil, err
			}
			cp := in
			return &cp, nil
		}
	}
	m.connectors = append(m.connectors, &in)
	if err := m.persist(); err != nil {
		return nil, err
	}
	cp := in
	return &cp, nil
}

// Delete removes the connector with the given id. Returns true if it existed.
func (m *ConnectorsManager) Delete(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.connectors {
		if c.ID == id {
			m.connectors = append(m.connectors[:i], m.connectors[i+1:]...)
			if err := m.persist(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// Probe runs the type-specific health check for a single connector and
// updates its cached Health. Safe to call from multiple goroutines.
func (m *ConnectorsManager) Probe(ctx context.Context, id string) Health {
	c := m.Get(id)
	if c == nil {
		return Health{OK: false, Status: "not_found", Detail: "no connector with id " + id, UpdatedUnix: time.Now().Unix()}
	}
	if !c.Enabled {
		h := Health{OK: false, Status: "disabled", UpdatedUnix: time.Now().Unix()}
		m.setHealth(id, h)
		return h
	}
	h := m.probeByType(ctx, c)
	h.UpdatedUnix = time.Now().Unix()
	m.setHealth(id, h)
	return h
}

func (m *ConnectorsManager) setHealth(id string, h Health) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.connectors {
		if c.ID == id {
			c.Health = h
			break
		}
	}
	// Don't bother persisting on every health tick — file would churn. The
	// registry survives a restart with "unknown" health until the next probe.
}

// probeByType dispatches to the driver. Drivers must be fast (<5s) and
// non-side-effecting — they're called every 60s.
func (m *ConnectorsManager) probeByType(ctx context.Context, c *Connector) Health {
	switch c.Type {
	case ConnectorTypePaperclip:
		return m.probePaperclip(ctx, c)
	case ConnectorTypeBeads:
		return m.probeBeads(ctx, c)
	case ConnectorTypeGasCity:
		return m.probeGasCity(ctx, c)
	case ConnectorTypeOllama:
		return m.probeOllama(ctx, c)
	case ConnectorTypeHubspace:
		return m.probeHubspace(ctx, c)
	default:
		return Health{Status: "unsupported_type", Detail: "no driver for type " + string(c.Type)}
	}
}

// probeHubspace health-checks the local Python sidecar daemon (which wraps the
// aioafero fork). The sidecar's /health endpoint is unauthenticated, but we
// still send the bearer token when one is configured.
func (m *ConnectorsManager) probeHubspace(ctx context.Context, c *Connector) Health {
	if c.URL == "" {
		return Health{Status: "misconfigured", Detail: "url is empty"}
	}
	url := strings.TrimRight(c.URL, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Health{Status: "error", Detail: err.Error()}
	}
	if c.TokenRef != "" {
		if tok := readTokenFile(c.TokenRef); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Health{Status: "down", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Health{Status: "unauthorized", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 300 {
		return Health{Status: "down", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return Health{OK: true, Status: "healthy", Detail: truncateString(string(body), 120)}
}

func (m *ConnectorsManager) probePaperclip(ctx context.Context, c *Connector) Health {
	if c.URL == "" {
		return Health{Status: "misconfigured", Detail: "url is empty"}
	}
	url := strings.TrimRight(c.URL, "/") + "/api/health"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Health{Status: "error", Detail: err.Error()}
	}
	if c.TokenRef != "" {
		if tok := readTokenFile(c.TokenRef); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Health{Status: "down", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Health{Status: "unauthorized", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 300 {
		return Health{Status: "down", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return Health{OK: true, Status: "healthy", Detail: truncateString(string(body), 120)}
}

func (m *ConnectorsManager) probeBeads(ctx context.Context, c *Connector) Health {
	repoRoot := c.URL
	if repoRoot == "" {
		repoRoot = c.Metadata["repo_root"]
	}
	if repoRoot == "" {
		return Health{Status: "misconfigured", Detail: "repo path is empty"}
	}
	// `bd doctor --json` returns a small JSON payload with the issue counts.
	// Use --quiet to suppress the auto-import preamble that pollutes stdout.
	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "bd", "doctor", "--json")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Health{Status: "down", Detail: truncateString(string(out), 200)}
	}
	return Health{OK: true, Status: "healthy", Detail: "bd doctor returned 0"}
}

func (m *ConnectorsManager) probeGasCity(ctx context.Context, c *Connector) Health {
	url := c.URL
	if url == "" {
		url = c.Metadata["supervisor_url"]
	}
	if url == "" {
		return Health{Status: "misconfigured", Detail: "supervisor URL empty"}
	}
	url = strings.TrimRight(url, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Health{Status: "error", Detail: err.Error()}
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Health{Status: "down", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Health{Status: "down", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return Health{OK: true, Status: "healthy"}
}

func (m *ConnectorsManager) probeOllama(ctx context.Context, c *Connector) Health {
	if c.URL == "" {
		return Health{Status: "misconfigured", Detail: "url is empty"}
	}
	url := strings.TrimRight(c.URL, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Health{Status: "error", Detail: err.Error()}
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Health{Status: "down", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Health{Status: "down", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return Health{OK: true, Status: "healthy"}
}

// RefreshAll probes every enabled connector concurrently. Caller controls
// the cadence — typically a 60s ticker. Each probe respects ctx.
func (m *ConnectorsManager) RefreshAll(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.connectors))
	for _, c := range m.connectors {
		ids = append(ids, c.ID)
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m.Probe(ctx, id)
		}(id)
	}
	wg.Wait()
}

// StartRefreshLoop kicks off a goroutine that refreshes every `period`. The
// first refresh fires immediately so the UI doesn't show "unknown" for the
// full period after boot.
func (m *ConnectorsManager) StartRefreshLoop(ctx context.Context, period time.Duration) {
	go func() {
		m.RefreshAll(ctx)
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.RefreshAll(ctx)
			}
		}
	}()
}

// --- HTTP handlers ----------------------------------------------------------

// RegisterRoutes wires the Connectors CRUD + probe endpoints under /api.
// Called from server.go's /api block alongside the other RegisterRoutes
// methods so they share the auth middleware.
func (m *ConnectorsManager) RegisterRoutes(r chi.Router) {
	r.Get("/connectors", m.handleList)
	r.Post("/connectors", m.handleCreate)
	r.Get("/connectors/{id}", m.handleGetOne)
	r.Patch("/connectors/{id}", m.handleUpdate)
	r.Delete("/connectors/{id}", m.handleDelete)
	r.Get("/connectors/{id}/health", m.handleProbe)
	r.Get("/connectors/{id}/test", m.handleProbe) // alias for the UI's "Test connection" button
}

func (m *ConnectorsManager) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"connectors": m.List()})
}

func (m *ConnectorsManager) handleGetOne(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c := m.Get(id)
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (m *ConnectorsManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in Connector
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c, err := m.Upsert(in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Kick off a probe so the UI sees fresh health within seconds.
	go m.Probe(context.Background(), c.ID)
	writeJSON(w, http.StatusCreated, c)
}

func (m *ConnectorsManager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing := m.Get(id)
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	var patch Connector
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Patch semantics: every non-empty field overrides the existing value.
	// Enabled and Capabilities are always taken from the patch because the
	// client always sends them when it sends a PATCH (the form has the
	// current values pre-loaded). Token can be cleared by sending "".
	updated := *existing
	if patch.Name != "" {
		updated.Name = patch.Name
	}
	if patch.URL != "" {
		updated.URL = patch.URL
	}
	updated.TokenRef = patch.TokenRef
	updated.Enabled = patch.Enabled
	if patch.Capabilities != nil {
		updated.Capabilities = patch.Capabilities
	}
	if patch.Metadata != nil {
		updated.Metadata = patch.Metadata
	}
	updated.ID = id
	updated.Type = existing.Type // type is immutable
	c, err := m.Upsert(updated)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go m.Probe(context.Background(), c.ID)
	writeJSON(w, http.StatusOK, c)
}

func (m *ConnectorsManager) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ok, err := m.Delete(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *ConnectorsManager) handleProbe(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	h := m.Probe(ctx, id)
	writeJSON(w, http.StatusOK, h)
}

// --- helpers ----------------------------------------------------------------

// readTokenFile reads a token file, enforcing mode 0600 to match the rest of
// aurex's auth posture (see launch-helper.sh for the parallel check on the
// session password). Returns "" on any error.
func readTokenFile(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if info.Mode().Perm() != 0o600 {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// DefaultConnectorsFromConfig builds the seed list used on first boot. The
// caller passes the current Config so this can introspect what's already
// configured without re-parsing files itself.
func DefaultConnectorsFromConfig(cfg *Config) []Connector {
	var out []Connector

	// Paperclip — always seed; the URL + token path are well-known defaults
	// the user can disable if they don't run it.
	out = append(out, Connector{
		ID:           "paperclip-default",
		Type:         ConnectorTypePaperclip,
		Name:         "Paperclip",
		URL:          "http://localhost:3100",
		TokenRef:     "/tmp/pcp-token",
		Enabled:      true,
		Capabilities: []Capability{CapAgents, CapIssues, CapIdeas},
	})

	// Beads — seed only if a repo root is configured.
	if cfg != nil && cfg.BeadsRepoRoot != "" {
		out = append(out, Connector{
			ID:           "beads-default",
			Type:         ConnectorTypeBeads,
			Name:         "Beads",
			URL:          cfg.BeadsRepoRoot,
			Enabled:      true,
			Capabilities: []Capability{CapIssues},
			Metadata:     map[string]string{"repo_root": cfg.BeadsRepoRoot},
		})
	}

	// Gas City — seed only if a supervisor URL is configured.
	if cfg != nil && cfg.GcSupervisorURL != "" {
		out = append(out, Connector{
			ID:           "gascity-default",
			Type:         ConnectorTypeGasCity,
			Name:         "Gas City",
			URL:          cfg.GcSupervisorURL,
			Enabled:      true,
			Capabilities: []Capability{CapAgents, CapDashboard, CapSupervisor},
			Metadata: map[string]string{
				"supervisor_url": cfg.GcSupervisorURL,
				"dashboard_url":  cfg.GcDashboardURL,
			},
		})
	}

	return out
}
