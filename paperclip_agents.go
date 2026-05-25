package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Paperclip persona surface: list the 10 named agents and trigger an
// on-demand wake-up. Distinct from agents.go which talks to Claude Code's
// local sub-agent daemon; this talks to Paperclip's HTTP API at
// $PAPERCLIP_BASE_URL with the bearer cached at $PAPERCLIP_TOKEN_FILE.
//
// Both surfaces are intentionally separate so each can fail independently —
// Claude CLI being absent shouldn't break the Paperclip kickstart UI, and
// vice versa.

// PaperclipAgent is the trimmed wire shape rendered by PaperclipAgentsPanel.jsx.
// We pass through only what the UI needs so the upstream Paperclip API can
// reshape its response without breaking us.
type PaperclipAgent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Role          string `json:"role,omitempty"`
	Title         string `json:"title,omitempty"`
	Status        string `json:"status,omitempty"`
	HeartbeatSec  int    `json:"heartbeat_sec,omitempty"`
	HeartbeatOn   bool   `json:"heartbeat_enabled"`
	LastHeartbeat string `json:"last_heartbeat_at,omitempty"`
}

// RegisterPaperclipAgentRoutes wires the two endpoints into the chi router.
// Caller passes the same `/api` sub-router used by everything else.
func RegisterPaperclipAgentRoutes(r chi.Router) {
	r.Get("/paperclip-agents", handlePaperclipList)
	r.Post("/paperclip-agents/{id}/wake", handlePaperclipWake)
}

func handlePaperclipList(w http.ResponseWriter, r *http.Request) {
	token := paperclipToken()
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no Paperclip board token — populate " + paperclipEnv("PAPERCLIP_TOKEN_FILE", defaultPaperclipTokenFile),
		})
		return
	}
	baseURL := paperclipEnv("PAPERCLIP_BASE_URL", defaultPaperclipBaseURL)
	companyID := paperclipEnv("PAPERCLIP_COMPANY_ID", defaultPaperclipCompanyID)

	url := strings.TrimRight(baseURL, "/") + "/api/companies/" + companyID + "/agents"
	req, err := http.NewRequestWithContext(r.Context(), "GET", url, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":          "paperclip returned non-2xx",
			"upstream_code":  fmt.Sprintf("%d", resp.StatusCode),
			"upstream_body":  truncate(string(body), 300),
		})
		return
	}

	// Paperclip returns either a JSON array or an envelope. Tolerate both.
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		var env struct {
			Agents []map[string]any `json:"agents"`
			Data   []map[string]any `json:"data"`
		}
		if err2 := json.Unmarshal(body, &env); err2 != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unparseable paperclip response: " + err.Error()})
			return
		}
		if env.Agents != nil {
			raw = env.Agents
		} else {
			raw = env.Data
		}
	}

	out := make([]PaperclipAgent, 0, len(raw))
	for _, a := range raw {
		pa := PaperclipAgent{
			ID:     asString(a["id"]),
			Name:   asString(a["name"]),
			Role:   asString(a["role"]),
			Title:  asString(a["title"]),
			Status: asString(a["status"]),
		}
		if rc, ok := a["runtimeConfig"].(map[string]any); ok {
			if hb, ok := rc["heartbeat"].(map[string]any); ok {
				if en, ok := hb["enabled"].(bool); ok {
					pa.HeartbeatOn = en
				}
				switch v := hb["intervalSec"].(type) {
				case float64:
					pa.HeartbeatSec = int(v)
				}
				pa.LastHeartbeat = asString(hb["lastWakeAt"])
			}
		}
		out = append(out, pa)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func handlePaperclipWake(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent id"})
		return
	}
	token := paperclipToken()
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no Paperclip board token",
		})
		return
	}
	baseURL := paperclipEnv("PAPERCLIP_BASE_URL", defaultPaperclipBaseURL)
	url := strings.TrimRight(baseURL, "/") + "/api/agents/" + id + "/wakeup"

	req, err := http.NewRequestWithContext(r.Context(), "POST", url, strings.NewReader("{}"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Pass the upstream status through so the UI can show "queued" vs error.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// --- small helpers -----------------------------------------------------------
// writeJSON lives in server.go and is shared.

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
