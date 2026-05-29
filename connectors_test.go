package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Helpers --------------------------------------------------------------------

func newMgr(t *testing.T, seeds ...Connector) (*ConnectorsManager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aurex.connectors.json")
	m, err := NewConnectorsManager(path, seeds)
	if err != nil {
		t.Fatalf("newMgr: %v", err)
	}
	return m, path
}

func connectorsRouter(m *ConnectorsManager) http.Handler {
	r := chi.NewRouter()
	r.Route("/api", m.RegisterRoutes)
	return r
}

// Tests ----------------------------------------------------------------------

// TestNewConnectorsManager_SeedsDefaultsOnFirstBoot writes the registry file
// when none exists, using the seed list. Subsequent loads must not re-seed.
func TestNewConnectorsManager_SeedsDefaultsOnFirstBoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aurex.connectors.json")

	seeds := []Connector{
		{Type: ConnectorTypePaperclip, Name: "Paperclip", URL: "http://localhost:3100", Enabled: true},
	}
	m, err := NewConnectorsManager(path, seeds)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if got := len(m.List()); got != 1 {
		t.Fatalf("seeded count = %d, want 1", got)
	}

	// File must exist and parse as JSON array.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	var arr []Connector
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatalf("parse seeded file: %v", err)
	}
	if len(arr) != 1 || arr[0].Name != "Paperclip" {
		t.Fatalf("seeded file shape wrong: %+v", arr)
	}

	// Second load must NOT add the seeds again (e.g., the user deleted the
	// default Paperclip; subsequent boot mustn't resurrect it).
	m2, err := NewConnectorsManager(path, seeds)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if got := len(m2.List()); got != 1 {
		t.Fatalf("second-load count = %d, want 1 (no re-seed)", got)
	}
}

// TestConnectorsManager_UpsertAndPersist confirms add + update flow and that
// the file is rewritten atomically with mode 0600.
func TestConnectorsManager_UpsertAndPersist(t *testing.T) {
	m, path := newMgr(t)

	// Insert
	in := Connector{Type: ConnectorTypePaperclip, Name: "Paperclip", URL: "http://localhost:3100", Enabled: true}
	c, err := m.Upsert(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if c.ID == "" {
		t.Errorf("Upsert must assign an ID; got empty")
	}

	// Mode 0600 enforced on disk
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}

	// Update — same ID, different URL
	upd := *c
	upd.URL = "http://localhost:3200"
	if _, err := m.Upsert(upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := m.Get(c.ID)
	if got == nil || got.URL != "http://localhost:3200" {
		t.Fatalf("update did not persist; got=%+v", got)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("Upsert should update, not duplicate; got %d entries", n)
	}
}

func TestConnectorsManager_Delete(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "x"})
	ok, err := m.Delete(c.ID)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if m.Get(c.ID) != nil {
		t.Errorf("Get after Delete should return nil")
	}
	// Idempotent — second delete returns false, not an error.
	ok2, err := m.Delete(c.ID)
	if err != nil || ok2 {
		t.Errorf("second delete: ok=%v err=%v, want (false, nil)", ok2, err)
	}
}

func TestConnectorsManager_UpsertRequiresTypeAndName(t *testing.T) {
	m, _ := newMgr(t)
	if _, err := m.Upsert(Connector{Name: "n"}); err == nil {
		t.Errorf("expected error for missing type")
	}
	if _, err := m.Upsert(Connector{Type: ConnectorTypePaperclip}); err == nil {
		t.Errorf("expected error for missing name")
	}
}

func TestConnectorsManager_UpdateLastKnownHealthOnUpsert(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "x"})
	// Plant cached health
	m.setHealth(c.ID, Health{OK: true, Status: "healthy", UpdatedUnix: 1234})
	// Upsert same ID with new URL — Upsert should preserve the cached health
	// so the UI doesn't blink to "unknown" between probes.
	updated := *c
	updated.URL = "y"
	out, _ := m.Upsert(updated)
	if out.Health.Status != "healthy" {
		t.Errorf("Upsert dropped cached health: %+v", out.Health)
	}
}

// HTTP-handler tests ---------------------------------------------------------

func TestHTTPList(t *testing.T) {
	m, _ := newMgr(t)
	m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "x"})

	r := connectorsRouter(m)
	req := httptest.NewRequest("GET", "/api/connectors", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var body struct {
		Connectors []Connector `json:"connectors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Connectors) != 1 {
		t.Fatalf("got %d, want 1", len(body.Connectors))
	}
}

func TestHTTPCreate(t *testing.T) {
	m, _ := newMgr(t)
	r := connectorsRouter(m)
	payload := `{"type":"paperclip","name":"Paperclip","url":"http://localhost:3100","enabled":true,"capabilities":["agents","issues"]}`
	req := httptest.NewRequest("POST", "/api/connectors", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var c Connector
	json.Unmarshal(rec.Body.Bytes(), &c)
	if c.ID == "" || c.Name != "Paperclip" {
		t.Errorf("response shape wrong: %+v", c)
	}
}

func TestHTTPUpdate_PatchSemantics(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "old", Enabled: true})
	r := connectorsRouter(m)

	// PATCH with only name + enabled — URL must not be wiped.
	payload := `{"name":"NewName","enabled":false}`
	req := httptest.NewRequest("PATCH", "/api/connectors/"+c.ID, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got := m.Get(c.ID)
	if got.Name != "NewName" {
		t.Errorf("Name not updated: %q", got.Name)
	}
	if got.URL != "old" {
		t.Errorf("URL should be preserved on omitted patch field; got %q", got.URL)
	}
	if got.Enabled {
		t.Errorf("Enabled should be false after patch; got true")
	}
}

func TestHTTPUpdate_TypeIsImmutable(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "x"})
	r := connectorsRouter(m)
	req := httptest.NewRequest("PATCH", "/api/connectors/"+c.ID,
		strings.NewReader(`{"type":"ollama","name":"P"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if m.Get(c.ID).Type != ConnectorTypePaperclip {
		t.Errorf("Type must be immutable on PATCH; got %s", m.Get(c.ID).Type)
	}
}

func TestHTTPDelete(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "x"})
	r := connectorsRouter(m)
	req := httptest.NewRequest("DELETE", "/api/connectors/"+c.ID, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("got %d, want 204", rec.Code)
	}
	if m.Get(c.ID) != nil {
		t.Errorf("connector still present after delete")
	}
}

func TestHTTPDelete_404OnMissing(t *testing.T) {
	m, _ := newMgr(t)
	r := connectorsRouter(m)
	req := httptest.NewRequest("DELETE", "/api/connectors/nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("got %d, want 404", rec.Code)
	}
}

// Probe tests ----------------------------------------------------------------

func TestProbePaperclip_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/health") {
			http.Error(w, "wrong path", 404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: upstream.URL, Enabled: true})

	h := m.Probe(context.Background(), c.ID)
	if !h.OK {
		t.Errorf("expected OK, got %+v", h)
	}
}

func TestProbePaperclip_DisabledIsSkipped(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "http://unreachable", Enabled: false})
	h := m.Probe(context.Background(), c.ID)
	if h.Status != "disabled" {
		t.Errorf("expected disabled, got %+v", h)
	}
}

func TestProbePaperclip_DownReturnsActionableError(t *testing.T) {
	m, _ := newMgr(t)
	// Bind to a port that's nothing — connect will refuse.
	c, _ := m.Upsert(Connector{Type: ConnectorTypePaperclip, Name: "P", URL: "http://127.0.0.1:1", Enabled: true})
	h := m.Probe(context.Background(), c.ID)
	if h.OK {
		t.Errorf("expected NOT OK; got %+v", h)
	}
	if h.Status != "down" {
		t.Errorf("expected status=down; got %q", h.Status)
	}
}

// Defaults derived from config ----------------------------------------------

func TestDefaultConnectorsFromConfig(t *testing.T) {
	cfg := &Config{
		BeadsRepoRoot:   "/tmp/some-repo",
		GcSupervisorURL: "http://localhost:8372",
		GcDashboardURL:  "http://localhost:8373",
	}
	defaults := DefaultConnectorsFromConfig(cfg)
	// Paperclip always seeded; Beads + Gas City conditionally.
	if len(defaults) != 3 {
		t.Fatalf("expected 3 defaults, got %d: %+v", len(defaults), defaults)
	}
	types := map[ConnectorType]bool{}
	for _, c := range defaults {
		types[c.Type] = true
	}
	for _, want := range []ConnectorType{ConnectorTypePaperclip, ConnectorTypeBeads, ConnectorTypeGasCity} {
		if !types[want] {
			t.Errorf("missing default for %s", want)
		}
	}
}

func TestDefaultConnectorsFromConfig_SkipsUnconfigured(t *testing.T) {
	cfg := &Config{} // no Beads or Gas City
	defaults := DefaultConnectorsFromConfig(cfg)
	if len(defaults) != 1 {
		t.Fatalf("expected 1 (Paperclip only), got %d", len(defaults))
	}
	if defaults[0].Type != ConnectorTypePaperclip {
		t.Errorf("expected Paperclip-only default, got %s", defaults[0].Type)
	}
}

// readTokenFile -------------------------------------------------------------

func TestReadTokenFile_EnforcesMode0600(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readTokenFile(loose); got != "" {
		t.Errorf("expected empty for mode 0644, got %q", got)
	}

	tight := filepath.Join(dir, "tight")
	if err := os.WriteFile(tight, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readTokenFile(tight); got != "secret" {
		t.Errorf("expected 'secret', got %q", got)
	}
}

func TestReadTokenFile_NonExistent(t *testing.T) {
	if got := readTokenFile("/tmp/nonexistent-file-xyz"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestProbeHubspace_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/health") {
			http.Error(w, "wrong path", 404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok","connected":true,"devices":9}`))
	}))
	defer upstream.Close()

	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{
		Type:         ConnectorTypeHubspace,
		Name:         "Hubspace",
		URL:          upstream.URL,
		Enabled:      true,
		Capabilities: []Capability{CapControl, CapSensor, CapSchedule},
	})

	h := m.Probe(context.Background(), c.ID)
	if !h.OK || h.Status != "healthy" {
		t.Errorf("expected healthy, got %+v", h)
	}
}

func TestProbeHubspace_Misconfigured(t *testing.T) {
	m, _ := newMgr(t)
	c, _ := m.Upsert(Connector{Type: ConnectorTypeHubspace, Name: "Hubspace", URL: "", Enabled: true})
	h := m.Probe(context.Background(), c.ID)
	if h.OK || h.Status != "misconfigured" {
		t.Errorf("expected misconfigured, got %+v", h)
	}
}
