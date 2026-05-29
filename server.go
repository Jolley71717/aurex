package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

type Server struct {
	cfg        *Config
	store      *SessionStore
	push       *PushManager
	ideas      *IdeasManager
	beads      *BeadsManager
	agents     *AgentsManager
	gc         *gcProxy
	connectors *ConnectorsManager
	frontend   fs.FS // embedded React build (may be nil if not embedded yet)
	upgrader   websocket.Upgrader
}

func NewServer(cfg *Config, store *SessionStore, push *PushManager, ideas *IdeasManager, beads *BeadsManager, agents *AgentsManager, gc *gcProxy, connectors *ConnectorsManager, frontend fs.FS) *Server {
	s := &Server{
		cfg:        cfg,
		store:      store,
		push:       push,
		ideas:      ideas,
		beads:      beads,
		agents:     agents,
		gc:         gc,
		connectors: connectors,
		frontend:   frontend,
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.checkWSOrigin,
	}
	return s
}

// checkWSOrigin enforces same-host or allowlisted-host on WebSocket upgrades.
// Default policy: the request's Origin header host must equal its Host header
// host (the browser served the page from the same place it's now WS-ing to).
// This blocks DNS-rebinding and cross-origin WS hijack from a malicious tab
// on a tailnet device, which the Tailscale network boundary alone does NOT
// prevent.
//
// If Origin is missing, we reject. Non-browser clients (curl, wscat) can
// supply an Origin header explicitly — that's a deliberate choice to require
// callers to opt into the trust boundary instead of getting it for free.
//
// cfg.AllowedOrigins, when non-empty, replaces the same-host rule with an
// exact-match allowlist of `scheme://host[:port]` URLs.
func (s *Server) checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if len(s.cfg.AllowedOrigins) > 0 {
		for _, allowed := range s.cfg.AllowedOrigins {
			if strings.EqualFold(strings.TrimRight(allowed, "/"), strings.TrimRight(origin, "/")) {
				return true
			}
		}
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/ws/{sessionID}", s.authWS(s.handleWS))

	r.Route("/api", func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/sessions", s.handleListSessions)
		r.Post("/sessions", s.handleCreateSession)
		r.Patch("/sessions/{sessionID}", s.handleRenameSession)
		r.Delete("/sessions/{sessionID}", s.handleDeleteSession)
		r.Get("/sessions/{sessionID}/transcript", s.handleTranscript)
		r.Get("/push/vapid-public-key", s.handleVapidPublicKey)
		r.Post("/push/subscribe", s.handleSubscribe)
		r.Post("/push/test", s.handlePushTest)
		r.Post("/paste/image", s.pasteHandler)
		if s.ideas != nil {
			s.ideas.RegisterRoutes(r)
		}
		if s.beads != nil {
			s.beads.RegisterRoutes(r)
		}
		if s.agents != nil {
			s.agents.RegisterRoutes(r)
		}
		if s.connectors != nil {
			s.connectors.RegisterRoutes(r)
		}
		// Paperclip persona surface — list the 10 named agents and wake any
		// one of them on demand. No prerequisite manager; relies on the
		// /tmp/pcp-token file + env-configurable Paperclip URL. Always
		// registered; the handlers themselves emit 503 if the token is
		// missing.
		RegisterPaperclipAgentRoutes(r)
		// Hubspace watering surface — proxies to the local Python sidecar
		// daemon. Always registered; handlers emit 503 if the sidecar token
		// is missing (i.e. the sidecar isn't running).
		RegisterHubspaceRoutes(r)
	})

	// Hook endpoint is intentionally NOT behind auth — it gates on localhost instead.
	r.Post("/api/hook/aura", hookAuraHandler(s.store, s.push))

	// Gas City reverse proxy: /gc/* serves the dashboard SPA (with HTML
	// rewritten to use /gc-api as the API base), /gc-api/* forwards to the
	// supervisor. Both routes are gated by the aurex session middleware so
	// the dashboard is only reachable to authenticated tailnet clients.
	if s.gc != nil {
		r.Route("/gc", func(r chi.Router) {
			r.Use(s.authMiddleware)
			r.HandleFunc("/", s.proxyGcDashboard)
			r.HandleFunc("/*", s.proxyGcDashboard)
		})
		r.Route("/gc-api", func(r chi.Router) {
			r.Use(s.authMiddleware)
			r.HandleFunc("/", s.proxyGcSupervisor)
			r.HandleFunc("/*", s.proxyGcSupervisor)
		})
	}

	// Embedded frontend last so /api/* and /ws/* take precedence.
	r.Handle("/*", s.frontendHandler())

	return r
}

// sessionCookieName is the cookie set after a successful basic-auth so
// subsequent requests skip the WWW-Authenticate prompt. iOS Safari drops
// basic-auth cache aggressively (every app switch); this cookie outlives
// that and keeps the PWA usable.
const sessionCookieName = "aurex_session"

// sessionTTL is how long the session cookie persists. Long enough to feel
// "stay signed in" but short enough that a stolen device session expires
// on its own. Each successful request refreshes it.
const sessionTTL = 30 * 24 * time.Hour

// sessionSecret is the HMAC key for signed session cookies. Initialized
// by InitSessionSecret() from main — persisted to disk so process restarts
// don't invalidate every logged-in device.
//
// To force-kick every session (e.g. after a suspected compromise), delete
// the on-disk secret file and restart aurex. A fresh key will be generated
// and every old cookie will fail HMAC verification.
var sessionSecret []byte

var sessionMu sync.RWMutex

// InitSessionSecret loads the HMAC secret from path, generating + persisting
// a new 32-byte key if the file is missing or unreadable. Called from main()
// after config load. Safe to call only once at startup.
//
// Persistence is best-effort: if the write fails (read-only fs, permissions),
// aurex still boots with the in-memory key, but the secret won't survive
// the next restart. We log a warning in that case so it's visible.
func InitSessionSecret(path string) error {
	if path == "" {
		// Caller didn't configure a path — fall back to ephemeral (old
		// behavior). Still works, just kicks everyone on restart.
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("crypto/rand: %w", err)
		}
		sessionMu.Lock()
		sessionSecret = b
		sessionMu.Unlock()
		return nil
	}

	if data, err := os.ReadFile(path); err == nil && len(data) == 32 {
		sessionMu.Lock()
		sessionSecret = data
		sessionMu.Unlock()
		return nil
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("crypto/rand: %w", err)
	}
	sessionMu.Lock()
	sessionSecret = b
	sessionMu.Unlock()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("aurex: session-secret mkdir %s: %v (sessions won't survive restart)", dir, err)
			return nil
		}
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Printf("aurex: failed to persist session-secret to %s: %v (sessions won't survive restart)", path, err)
		return nil
	}
	log.Printf("aurex: session-secret persisted to %s", path)
	return nil
}

// signSession returns a base64url(payload "|" hmac(payload)) where payload
// is "<username>|<expiry-unix-seconds>". Constant-time on verify.
func signSession(username string) string {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%s|%d", username, exp)
	sessionMu.RLock()
	mac := hmac.New(sha256.New, sessionSecret)
	sessionMu.RUnlock()
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySession(token, expectedUser string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	sessionMu.RLock()
	mac := hmac.New(sha256.New, sessionSecret)
	sessionMu.RUnlock()
	mac.Write(payload)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return false
	}
	pieces := strings.SplitN(string(payload), "|", 2)
	if len(pieces) != 2 {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(pieces[0]), []byte(expectedUser)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(pieces[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return true
}

// setSessionCookie writes the HMAC'd session cookie. HttpOnly so JS can't
// see it; Secure so it only flows over TLS; SameSite=Lax so it survives
// normal same-site navigation.
func setSessionCookie(w http.ResponseWriter, username string, https bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSession(username),
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   https,
		SameSite: http.SameSiteLaxMode,
	})
}

// authPass is the shared check used by both the HTTP API middleware and
// the WS upgrade handler. Returns true if the request is authenticated
// via either a valid session cookie OR valid basic auth credentials.
// In the basic-auth-success case it also issues a session cookie so the
// next navigation doesn't re-prompt.
func (s *Server) authPass(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.Auth {
		return true
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && verifySession(c.Value, s.cfg.Username) {
		// Rolling refresh — extends the expiry on every authenticated hit.
		setSessionCookie(w, s.cfg.Username, r.TLS != nil)
		return true
	}
	u, p, ok := r.BasicAuth()
	if !ok ||
		subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="aurex"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	setSessionCookie(w, s.cfg.Username, r.TLS != nil)
	return true
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authPass(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authWS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authPass(w, r) {
			return
		}
		h(w, r)
	}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.List()})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	sess, err := s.store.Create(body.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sessionID")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.store.Rename(id, body.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sess := s.store.Get(id); sess != nil {
		writeJSON(w, http.StatusOK, sess)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sessionID")
	if err := s.store.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ANSI strippers. Returns whatever's in the per-session ring buffer with all
// terminal control sequences removed — gives back a plain-text approximation
// of everything that was sent to the PTY since the buffer was last cleared.
//
// We compile the regexes once package-wide rather than per request so a
// transcript fetch isn't paying setup cost.
var (
	// OSC: ESC ] ... BEL  (or ESC ] ... ESC \) — title sets, hyperlinks, etc.
	// Match longest sequences first so their ESC byte isn't picked off by a
	// shorter rule before the full sequence is recognized.
	stripOSC = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// DCS / SOS / PM / APC: ESC {P,X,^,_} ... ST (terminated by ESC\ or BEL).
	stripDCS = regexp.MustCompile("\x1b[PX^_][^\x1b\x07]*(?:\x07|\x1b\\\\)")
	// CSI: ESC [ ... letter — covers SGR colors, cursor movement, erase, etc.
	stripCSI = regexp.MustCompile("\x1b\\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]")
	// Character-set designation: ESC ( X / ESC ) X / ESC * X / ESC + X / ESC % X / ESC - X / ESC . X
	stripCharset = regexp.MustCompile("\x1b[()*+%\\-.][0-9A-Za-z@]")
	// Two-byte ESC sequences (ESC followed by a single final byte).
	stripESC = regexp.MustCompile("\x1b[@-Z\\\\-_=>78cDEHMNOPZ]")
	// Other unprintable bytes — keep \t \n \r, drop everything else below 0x20
	// plus DEL. Any stray ESC left over also goes via this catch-all.
	stripNonPrint = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")
)

func stripAnsi(s string) string {
	// Order matters: strip the longer, more specific sequences first so their
	// leading ESC byte isn't picked off by a shorter rule (or the non-print
	// catch-all) before the full sequence is recognized.
	s = stripOSC.ReplaceAllString(s, "")
	s = stripDCS.ReplaceAllString(s, "")
	s = stripCSI.ReplaceAllString(s, "")
	s = stripCharset.ReplaceAllString(s, "")
	s = stripESC.ReplaceAllString(s, "")
	s = stripNonPrint.ReplaceAllString(s, "")
	// Treat carriage returns as line breaks so TUI redraws that overwrite a
	// line via \r become separate lines in the transcript rather than
	// silently swallowed.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sessionID")
	sess := s.store.Get(id)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.buffer == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	raw, _ := sess.buffer.ReadFrom(sess.buffer.Start())
	text := stripAnsi(string(raw))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

func (s *Server) handleVapidPublicKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.push.PublicKey()})
}

func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	res := s.push.Notify(NotificationPayload{
		Title:     "aurex test",
		Body:      "Push pipeline is working.",
		SessionID: "",
		Tag:       "aurex-test",
	})
	log.Printf("push: test → total=%d sent=%d failed=%d pruned=%d lastErr=%q",
		res.Total, res.Sent, res.Failed, res.Pruned, res.LastErr)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	sub, err := SubscriptionFromJSON(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.push.AddSubscription(sub)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sessionID")
	sess := s.store.Get(id)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	var cursor int64
	if c := q.Get("cursor"); c != "" {
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n >= 0 {
			cursor = n
		}
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	// Resize the PTY to the client's reported size BEFORE attaching, so any
	// snapshot or redraw we capture is at the client's actual dimensions
	// instead of whatever the previous client left the PTY at.
	if cols > 0 && rows > 0 {
		ResizePTY(sess, cols, rows)
	}
	class := classifyClient(r.Header.Get("User-Agent"))
	AttachSubscriber(sess, s.store, conn, cursor, class)
}

// BroadcastSessionUpdate fans out session metadata changes (name, aura, cwd,
// branch) to every connected WebSocket subscriber across all sessions. Uses
// the per-subscriber send channel so it doesn't block on slow clients.
func (s *Server) BroadcastSessionUpdate(sess *Session) {
	sessJSON, _ := json.Marshal(sess)
	updateBytes, _ := json.Marshal(map[string]any{
		"type":    "session_update",
		"session": json.RawMessage(sessJSON),
	})
	listMsg := map[string]any{"type": "sessions_list", "sessions": s.store.List()}
	listBytes, _ := json.Marshal(listMsg)

	all := s.store.List()
	seen := make(map[*Subscriber]bool)
	for _, target := range all {
		target.subMu.Lock()
		subs := make([]*Subscriber, 0, len(target.activeSubs))
		for sub := range target.activeSubs {
			subs = append(subs, sub)
		}
		target.subMu.Unlock()
		for _, sub := range subs {
			if seen[sub] {
				continue
			}
			seen[sub] = true
			// We can't use Push here because Push is keyed to SubscriberMessage
			// JSON shape (output, cursor). Drop control messages directly into
			// the writer via a raw-payload variant.
			_ = pushRaw(sub, listBytes)
		}
		// Only this session's subscribers also get the per-session update.
		for _, sub := range subs {
			_ = pushRaw(sub, updateBytes)
		}
	}
}

// pushRaw sends an already-encoded JSON payload to a subscriber's writer
// goroutine via a non-blocking channel write.
func pushRaw(sub *Subscriber, payload []byte) bool {
	select {
	case sub.send <- SubscriberMessage{rawPayload: payload}:
		return true
	default:
		return false
	}
}

func (s *Server) frontendHandler() http.Handler {
	if s.frontend == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "frontend not embedded — build with `cd client && npm run build`", http.StatusNotFound)
		})
	}
	fileServer := http.FileServer(http.FS(s.frontend))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: if the request doesn't map to a file, serve index.html.
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(s.frontend, p); err != nil {
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
