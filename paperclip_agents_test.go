package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Helpers ---------------------------------------------------------------------

// setupPaperclipEnv points the helpers at a mock Paperclip server and a token
// file inside the test's temp dir. Restores the previous env on cleanup.
func setupPaperclipEnv(t *testing.T, baseURL string, token string) {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "pcp-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv("PAPERCLIP_BASE_URL", baseURL)
	t.Setenv("PAPERCLIP_TOKEN_FILE", tokenPath)
	t.Setenv("PAPERCLIP_COMPANY_ID", "test-company-uuid")
}

// router builds the chi router with the paperclip routes wired in, mirroring
// what RegisterPaperclipAgentRoutes does inside server.go's /api block.
func router() *chi.Mux {
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		RegisterPaperclipAgentRoutes(r)
	})
	return r
}

// doReq issues a request against the test router and returns the response.
func doReq(t *testing.T, r http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestPaperclipList_HappyPath verifies the list handler proxies the upstream
// /api/companies/{cid}/agents response, trims the payload to the public shape,
// and extracts heartbeat config out of runtimeConfig correctly.
func TestPaperclipList_HappyPath(t *testing.T) {
	var calledAuthHeader string
	var calledPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledAuthHeader = r.Header.Get("Authorization")
		calledPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// Array form (not envelope) — both should work.
		_, _ = w.Write([]byte(`[
			{
				"id":"ceo-uuid","name":"CEO","role":"ceo","title":"Chief Exec",
				"status":"idle",
				"runtimeConfig":{"heartbeat":{"enabled":true,"intervalSec":14400,"lastWakeAt":"2026-05-24T12:00:00Z"}}
			},
			{
				"id":"eng-uuid","name":"Engineer","role":"engineer",
				"status":"running",
				"runtimeConfig":{"heartbeat":{"enabled":true,"intervalSec":7200}}
			}
		]`))
	}))
	defer upstream.Close()

	setupPaperclipEnv(t, upstream.URL, "test-token")

	rec := doReq(t, router(), "GET", "/api/paperclip-agents", nil)
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if calledAuthHeader != "Bearer test-token" {
		t.Errorf("upstream Authorization = %q, want Bearer test-token", calledAuthHeader)
	}
	if calledPath != "/api/companies/test-company-uuid/agents" {
		t.Errorf("upstream path = %q, want /api/companies/test-company-uuid/agents", calledPath)
	}

	var body struct {
		Agents []PaperclipAgent `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(body.Agents))
	}
	ceo := body.Agents[0]
	if ceo.Name != "CEO" || ceo.Status != "idle" || !ceo.HeartbeatOn || ceo.HeartbeatSec != 14400 {
		t.Errorf("CEO row malformed: %+v", ceo)
	}
	if ceo.LastHeartbeat != "2026-05-24T12:00:00Z" {
		t.Errorf("CEO lastHeartbeat = %q, want 2026-05-24T12:00:00Z", ceo.LastHeartbeat)
	}
	eng := body.Agents[1]
	if eng.HeartbeatSec != 7200 || eng.Status != "running" {
		t.Errorf("Engineer row malformed: %+v", eng)
	}
}

// TestPaperclipList_EnvelopeShape verifies the handler also accepts the
// envelope-form upstream response ({"agents":[...]}), not just an array.
// This matters because Paperclip's API may switch shapes across releases.
func TestPaperclipList_EnvelopeShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"id":"a","name":"Janitor","status":"idle"}]}`))
	}))
	defer upstream.Close()
	setupPaperclipEnv(t, upstream.URL, "tok")

	rec := doReq(t, router(), "GET", "/api/paperclip-agents", nil)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct {
		Agents []PaperclipAgent `json:"agents"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Agents) != 1 || body.Agents[0].Name != "Janitor" {
		t.Fatalf("envelope shape not unwrapped correctly: %+v", body.Agents)
	}
}

// TestPaperclipList_NoTokenFile returns 503 when /tmp/pcp-token (or the env-
// overridden equivalent) is missing — the handler must surface a clear error
// instead of silently calling Paperclip without a bearer.
func TestPaperclipList_NoTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAPERCLIP_TOKEN_FILE", filepath.Join(dir, "does-not-exist"))
	t.Setenv("PAPERCLIP_BASE_URL", "http://unreachable.example")

	rec := doReq(t, router(), "GET", "/api/paperclip-agents", nil)
	if rec.Code != 503 {
		t.Fatalf("got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no Paperclip board token") {
		t.Errorf("body missing token-missing hint: %s", rec.Body.String())
	}
}

// TestPaperclipList_UpstreamError surfaces a 502 with the upstream code and a
// truncated body excerpt when Paperclip returns a non-2xx.
func TestPaperclipList_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("paperclip exploded"))
	}))
	defer upstream.Close()
	setupPaperclipEnv(t, upstream.URL, "tok")

	rec := doReq(t, router(), "GET", "/api/paperclip-agents", nil)
	if rec.Code != 502 {
		t.Fatalf("got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["upstream_code"] != "500" {
		t.Errorf("upstream_code = %q, want 500", body["upstream_code"])
	}
	if !strings.Contains(body["upstream_body"], "paperclip exploded") {
		t.Errorf("upstream_body should echo the upstream text: %q", body["upstream_body"])
	}
}

// TestPaperclipList_UnparseableUpstream returns 502 when Paperclip returns a
// 2xx with garbage that's neither an array nor an envelope.
func TestPaperclipList_UnparseableUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"not-an-array-or-envelope"`))
	}))
	defer upstream.Close()
	setupPaperclipEnv(t, upstream.URL, "tok")

	rec := doReq(t, router(), "GET", "/api/paperclip-agents", nil)
	if rec.Code != 502 {
		t.Fatalf("got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unparseable") {
		t.Errorf("body should mention unparseable: %s", rec.Body.String())
	}
}

// TestPaperclipWake_HappyPath confirms the wake handler forwards POST with
// {} body + bearer to the upstream wakeup endpoint, and passes through both
// the upstream status code and body verbatim so the UI can show "queued".
func TestPaperclipWake_HappyPath(t *testing.T) {
	var calls atomic.Int32
	var receivedAuth string
	var receivedPath string
	var receivedBody string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"id":"run-uuid","status":"queued","invocationSource":"on_demand"}`))
	}))
	defer upstream.Close()
	setupPaperclipEnv(t, upstream.URL, "tok")

	rec := doReq(t, router(), "POST", "/api/paperclip-agents/agent-xyz/wake", strings.NewReader(`{}`))

	if rec.Code != 202 {
		t.Fatalf("got %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1", calls.Load())
	}
	if receivedAuth != "Bearer tok" {
		t.Errorf("upstream Authorization = %q, want Bearer tok", receivedAuth)
	}
	if receivedPath != "/api/agents/agent-xyz/wakeup" {
		t.Errorf("upstream path = %q, want /api/agents/agent-xyz/wakeup", receivedPath)
	}
	if receivedBody != `{}` {
		t.Errorf("upstream body = %q, want {}", receivedBody)
	}
	if !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Errorf("response body should pass through upstream JSON: %s", rec.Body.String())
	}
}

// TestPaperclipWake_NoTokenFile is the wake-path analogue of the list error
// — same 503 with the same hint message.
func TestPaperclipWake_NoTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAPERCLIP_TOKEN_FILE", filepath.Join(dir, "does-not-exist"))
	t.Setenv("PAPERCLIP_BASE_URL", "http://unreachable.example")

	rec := doReq(t, router(), "POST", "/api/paperclip-agents/some-id/wake", strings.NewReader(`{}`))
	if rec.Code != 503 {
		t.Fatalf("got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPaperclipWake_PassesThroughErrorStatus confirms a 5xx from Paperclip
// reaches the client with the upstream status preserved. This matters
// because the UI shows different feedback for "queued" vs "failed".
func TestPaperclipWake_PassesThroughErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"agent not found"}`))
	}))
	defer upstream.Close()
	setupPaperclipEnv(t, upstream.URL, "tok")

	rec := doReq(t, router(), "POST", "/api/paperclip-agents/ghost/wake", strings.NewReader(`{}`))
	if rec.Code != 403 {
		t.Fatalf("got %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "agent not found") {
		t.Errorf("response body should pass through upstream error: %s", rec.Body.String())
	}
}

// TestAsString and TestTruncate cover the small helpers separately so a
// regression there surfaces with a tight diagnostic instead of through a
// confused handler-level failure.
func TestAsString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hello", "hello"},
		{"", ""},
		{nil, ""},
		{42, ""},     // ints don't coerce — caller would have to handle separately
		{true, ""},   // booleans don't coerce either
	}
	for _, c := range cases {
		if got := asString(c.in); got != c.want {
			t.Errorf("asString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate long: got %q", got)
	}
	if got := truncate("hi", 5); got != "hi" {
		t.Errorf("truncate short: got %q", got)
	}
	if got := truncate("", 5); got != "" {
		t.Errorf("truncate empty: got %q", got)
	}
}
