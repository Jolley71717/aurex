package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// connectorProxyRouter mounts the generic connector UI proxy the same way
// server.go does, so path stripping + chi.URLParam("id") behave identically.
func connectorProxyRouter(m *ConnectorsManager) http.Handler {
	r := chi.NewRouter()
	r.Route("/connector/{id}", func(r chi.Router) {
		r.HandleFunc("/", m.proxyConnectorUI)
		r.HandleFunc("/*", m.proxyConnectorUI)
	})
	return r
}

func TestConnector_WebURL_And_LaunchPath(t *testing.T) {
	cases := []struct {
		name     string
		c        Connector
		wantWeb  string
		wantPath string
	}{
		{
			name:     "paperclip falls back to URL",
			c:        Connector{ID: "paperclip-default", Type: ConnectorTypePaperclip, URL: "http://localhost:3100"},
			wantWeb:  "http://localhost:3100",
			wantPath: "/connector/paperclip-default/",
		},
		{
			name:     "explicit web_url wins",
			c:        Connector{ID: "x", Type: ConnectorTypeHubspace, URL: "http://api", Metadata: map[string]string{"web_url": "http://ui"}},
			wantWeb:  "http://ui",
			wantPath: "/connector/x/",
		},
		{
			name:     "gascity reuses the /gc proxy",
			c:        Connector{ID: "gascity-default", Type: ConnectorTypeGasCity, URL: "http://sup", Metadata: map[string]string{"dashboard_url": "http://dash"}},
			wantWeb:  "", // gascity has no generic web_url; it launches via /gc
			wantPath: "/gc",
		},
		{
			name:     "beads has no web UI",
			c:        Connector{ID: "beads-default", Type: ConnectorTypeBeads, URL: "/repo"},
			wantWeb:  "",
			wantPath: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.WebURL(); got != tc.wantWeb {
				t.Errorf("WebURL() = %q, want %q", got, tc.wantWeb)
			}
			if got := tc.c.LaunchPath(); got != tc.wantPath {
				t.Errorf("LaunchPath() = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// TestHandleList_IncludesLaunchURL verifies the derived launch_url is present
// in the list response but never written to the persisted file.
func TestHandleList_IncludesLaunchURL(t *testing.T) {
	m, path := newMgr(t,
		Connector{ID: "paperclip-default", Type: ConnectorTypePaperclip, Name: "Paperclip", URL: "http://localhost:3100", Enabled: true},
	)
	srv := httptest.NewServer(connectorsRouter(m))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/connectors")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Connectors []map[string]any `json:"connectors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Connectors) != 1 {
		t.Fatalf("got %d connectors, want 1", len(body.Connectors))
	}
	if got := body.Connectors[0]["launch_url"]; got != "/connector/paperclip-default/" {
		t.Errorf("launch_url = %v, want /connector/paperclip-default/", got)
	}

	// launch_url must NOT be persisted to disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(raw), "launch_url") {
		t.Errorf("persisted file leaked launch_url:\n%s", raw)
	}
}

// TestProxyConnectorUI_StripsFramingAndRewritesHTML drives the proxy against a
// fake upstream that sets X-Frame-Options + CSP and serves a root-absolute
// asset path. The proxy must strip the framing headers, drop frame-ancestors,
// inject a <base>, and prefix the asset path.
func TestProxyConnectorUI_StripsFramingAndRewritesHTML(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>app</title></head><body><script src="/assets/app.js"></script><img src="//cdn/x.png"></body></html>`)
	}))
	defer upstream.Close()

	m, _ := newMgr(t, Connector{
		ID:       "app",
		Type:     ConnectorTypeHubspace,
		Name:     "App",
		Enabled:  true,
		Metadata: map[string]string{"web_url": upstream.URL},
	})
	srv := httptest.NewServer(connectorProxyRouter(m))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/connector/app/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Frame-Options") != "" {
		t.Errorf("X-Frame-Options not stripped: %q", resp.Header.Get("X-Frame-Options"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "frame-ancestors") {
		t.Errorf("frame-ancestors not stripped from CSP: %q", csp)
	}
	b, _ := io.ReadAll(resp.Body)
	html := string(b)
	if !strings.Contains(html, `<base href="/connector/app/">`) {
		t.Errorf("missing injected <base>:\n%s", html)
	}
	if !strings.Contains(html, `src="/connector/app/assets/app.js"`) {
		t.Errorf("root-absolute asset not prefixed:\n%s", html)
	}
	if !strings.Contains(html, `src="//cdn/x.png"`) {
		t.Errorf("protocol-relative URL should be left untouched:\n%s", html)
	}
}

func TestProxyConnectorUI_DisabledOrMissing(t *testing.T) {
	m, _ := newMgr(t,
		Connector{ID: "off", Type: ConnectorTypeHubspace, Name: "Off", Enabled: false, Metadata: map[string]string{"web_url": "http://x"}},
		Connector{ID: "noui", Type: ConnectorTypeBeads, Name: "Beads", Enabled: true, URL: "/repo"},
	)
	srv := httptest.NewServer(connectorProxyRouter(m))
	defer srv.Close()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/connector/off/", http.StatusServiceUnavailable},  // disabled
		{"/connector/noui/", http.StatusServiceUnavailable}, // no web UI
		{"/connector/ghost/", http.StatusNotFound},          // unknown id
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
}
