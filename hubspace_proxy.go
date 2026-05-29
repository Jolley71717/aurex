package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Hubspace watering surface: proxy the WateringPage's calls through to the
// local Python sidecar daemon (which wraps the extended aioafero fork). The
// sidecar origin is $HUBSPACE_SIDECAR_URL (default http://127.0.0.1:8523) and
// its bearer token is read from the file at $HUBSPACE_SIDECAR_TOKEN_FILE
// (default /tmp/hubspace-sidecar-token), mirroring the paperclip pattern.
//
// Routes mirror the sidecar's API so the proxy stays a thin pass-through:
//   GET    /api/hubspace/devices
//   GET    /api/hubspace/devices/{id}
//   POST   /api/hubspace/devices/{id}/spigot/{instance}/run
//   POST   /api/hubspace/devices/{id}/spigot/{instance}/stop
//   GET    /api/hubspace/schedules
//   POST   /api/hubspace/schedules
//   DELETE /api/hubspace/schedules/{ruleId}

const (
	defaultHubspaceSidecarURL  = "http://127.0.0.1:8523"
	defaultHubspaceTokenFile   = "/tmp/hubspace-sidecar-token"
)

func hubspaceSidecarURL() string {
	if v := os.Getenv("HUBSPACE_SIDECAR_URL"); v != "" {
		return v
	}
	return defaultHubspaceSidecarURL
}

func hubspaceToken() string {
	path := os.Getenv("HUBSPACE_SIDECAR_TOKEN_FILE")
	if path == "" {
		path = defaultHubspaceTokenFile
	}
	return readTokenFile(path) // shared helper in connectors.go (enforces 0600)
}

// RegisterHubspaceRoutes wires the watering endpoints into the /api sub-router.
func RegisterHubspaceRoutes(r chi.Router) {
	r.Get("/hubspace/devices", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "GET", "/v1/devices")
	})
	r.Get("/hubspace/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "GET", "/v1/devices/"+chi.URLParam(r, "id"))
	})
	r.Post("/hubspace/devices/{id}/spigot/{instance}/run", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "POST",
			"/v1/devices/"+chi.URLParam(r, "id")+"/spigot/"+chi.URLParam(r, "instance")+"/run")
	})
	r.Post("/hubspace/devices/{id}/spigot/{instance}/stop", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "POST",
			"/v1/devices/"+chi.URLParam(r, "id")+"/spigot/"+chi.URLParam(r, "instance")+"/stop")
	})
	r.Get("/hubspace/schedules", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "GET", "/v1/schedules")
	})
	r.Post("/hubspace/schedules", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "POST", "/v1/schedules")
	})
	r.Delete("/hubspace/schedules/{ruleId}", func(w http.ResponseWriter, r *http.Request) {
		proxyToSidecar(w, r, "DELETE", "/v1/schedules/"+chi.URLParam(r, "ruleId"))
	})
}

// proxyToSidecar forwards the request to the sidecar with the bearer token and
// passes the upstream status + body straight back. The request body (for POST)
// is forwarded as-is.
func proxyToSidecar(w http.ResponseWriter, r *http.Request, method, upstreamPath string) {
	token := hubspaceToken()
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no Hubspace sidecar token — is the sidecar running? expected " + defaultHubspaceTokenFile,
		})
		return
	}

	var body io.Reader
	if r.Body != nil && (method == "POST" || method == "PUT") {
		body = r.Body
	}
	url := strings.TrimRight(hubspaceSidecarURL(), "/") + upstreamPath
	req, err := http.NewRequestWithContext(r.Context(), method, url, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
