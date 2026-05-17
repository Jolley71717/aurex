package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// gcProxy holds the configured reverse proxies for the Gas City dashboard
// and supervisor. Either field may be nil when the corresponding upstream
// was unreachable at boot (auto-disabled).
type gcProxy struct {
	dashboardURL   *url.URL // e.g. http://localhost:8088
	supervisorURL  *url.URL // e.g. http://127.0.0.1:8372
	dashboardProxy *httputil.ReverseProxy
	// supervisorProxy is only used for non-SSE requests. SSE traffic
	// (/v0/events/stream) gets a hand-rolled streaming handler so we can
	// flush every chunk immediately and avoid any buffered reverse-proxy
	// behaviour that would stall the event feed on a mobile browser.
	supervisorProxy *httputil.ReverseProxy
}

// newGcProxy probes both upstreams. Each side is enabled independently:
// supervisor unreachable -> /gc-api disabled. dashboard unreachable -> /gc
// disabled. Both reachable -> both enabled. The probe timeout is 1s, so a
// cold boot pays at most ~2s before deciding.
func newGcProxy(dashboardURL, supervisorURL string) *gcProxy {
	p := &gcProxy{}

	if supervisorURL != "" {
		u, err := url.Parse(supervisorURL)
		if err != nil {
			log.Printf("aurex: gc supervisor URL invalid (%v) — /gc-api disabled", err)
		} else if !probeGcHealth(u) {
			log.Printf("aurex: gc supervisor probe failed at %s/health — /gc-api disabled", u)
		} else {
			p.supervisorURL = u
			p.supervisorProxy = httputil.NewSingleHostReverseProxy(u)
			p.supervisorProxy.ErrorLog = log.New(io.Discard, "", 0)
			log.Printf("aurex: gc supervisor proxy enabled (%s -> /gc-api)", u)
		}
	}

	if dashboardURL != "" {
		u, err := url.Parse(dashboardURL)
		if err != nil {
			log.Printf("aurex: gc dashboard URL invalid (%v) — /gc disabled", err)
		} else if p.supervisorURL == nil {
			// Without the supervisor the dashboard is useless (it'd just be
			// a static page that can't reach its API). Disable both as a
			// pair so the user sees the same error consistently.
			log.Printf("aurex: gc dashboard proxy disabled because supervisor is unreachable")
		} else {
			proxy := httputil.NewSingleHostReverseProxy(u)
			proxy.ModifyResponse = rewriteDashboardHTML
			proxy.ErrorLog = log.New(io.Discard, "", 0)
			p.dashboardURL = u
			p.dashboardProxy = proxy
			log.Printf("aurex: gc dashboard proxy enabled (%s -> /gc)", u)
		}
	}

	return p
}

func probeGcHealth(u *url.URL) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(u.String() + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// proxyGcDashboard forwards /gc/* to the upstream dashboard origin, stripping
// the /gc prefix. HTML responses are rewritten so the embedded supervisor
// URL points at /gc-api (the cookie-gated supervisor proxy) and the absolute
// asset paths (/dashboard.css, /dashboard.js, /assets/...) are prefixed with
// /gc so the browser fetches them back through aurex.
func (s *Server) proxyGcDashboard(w http.ResponseWriter, r *http.Request) {
	if s.gc == nil || s.gc.dashboardProxy == nil {
		gcUnavailable(w, "dashboard")
		return
	}
	// chi mounts this under r.Route("/gc"), so r.URL.Path on entry is the
	// remainder after /gc. We need to forward "/" when the user hits /gc
	// (no trailing slash) and "/foo" when they hit /gc/foo.
	r2 := r.Clone(r.Context())
	r2.URL.Path = stripPrefix(r.URL.Path, "/gc")
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	r2.Host = s.gc.dashboardURL.Host
	s.gc.dashboardProxy.ServeHTTP(w, r2)
}

// proxyGcSupervisor forwards /gc-api/* to the supervisor origin. The SSE
// endpoint gets a dedicated streaming handler so we can flush chunks as
// they arrive — net/http/httputil.ReverseProxy will stream by default, but
// only when the upstream sends a Content-Type of text/event-stream AND the
// downstream supports http.Flusher. Doing it by hand keeps the behaviour
// obvious and lets us set sane headers even when the upstream doesn't.
func (s *Server) proxyGcSupervisor(w http.ResponseWriter, r *http.Request) {
	if s.gc == nil || s.gc.supervisorProxy == nil {
		gcUnavailable(w, "supervisor")
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = stripPrefix(r.URL.Path, "/gc-api")
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	if strings.HasSuffix(r2.URL.Path, "/events/stream") {
		s.streamGcSSE(w, r, r2.URL.Path)
		return
	}
	r2.Host = s.gc.supervisorURL.Host
	s.gc.supervisorProxy.ServeHTTP(w, r2)
}

// streamGcSSE proxies an SSE connection from the supervisor to the client
// without buffering. The supervisor closes the stream when the city stops
// or the process dies; we propagate that by ending the response. Client
// disconnect is propagated upstream via r.Context() cancellation.
func (s *Server) streamGcSSE(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	target := *s.gc.supervisorURL
	target.Path = upstreamPath
	target.RawQuery = r.URL.RawQuery

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Preserve a couple of standard SSE headers from the client; the rest
	// are noise (auth cookies/basic auth are aurex-side, not supervisor-side).
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		upstreamReq.Header.Set("Last-Event-ID", v)
	}
	upstreamReq.Header.Set("Accept", "text/event-stream")
	upstreamReq.Header.Set("Cache-Control", "no-cache")

	// No timeout on the response read — SSE is long-lived. Dial timeout
	// matters; once we're connected the only way out is the request
	// context cancelling (client disconnect) or the upstream closing.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			ResponseHeaderTimeout: 5 * time.Second,
		},
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		if r.Context().Err() != nil {
			return // client gave up first
		}
		http.Error(w, "upstream sse: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Mirror the upstream status + selected headers, then start flushing.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Should never happen with net/http's default ResponseWriter, but if
		// it does we degrade gracefully — events will batch but won't be lost.
		_, _ = io.Copy(w, resp.Body)
		return
	}
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// rewriteDashboardHTML rewrites the dashboard's index HTML so that:
//   - The embedded supervisor URL meta tag points at /gc-api (the aurex
//     reverse-proxy mount of the supervisor API), which a phone can
//     actually reach over the tailnet.
//   - Absolute references to /dashboard.css, /dashboard.js, /assets/... get
//     prefixed with /gc so the browser pulls those through aurex too.
//
// Non-HTML responses are passed through untouched.
func rewriteDashboardHTML(resp *http.Response) error {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "text/html") {
		return nil
	}

	original, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read dashboard body: %w", err)
	}
	_ = resp.Body.Close()

	out := original
	out = bytes.ReplaceAll(out,
		[]byte(`content="http://127.0.0.1:8372"`),
		[]byte(`content="/gc-api"`))
	out = bytes.ReplaceAll(out,
		[]byte(`content="http://localhost:8372"`),
		[]byte(`content="/gc-api"`))
	// Asset path rewrites. The dashboard ships with absolute paths from its
	// own root; we need them to come back through aurex.
	out = bytes.ReplaceAll(out, []byte(`href="/dashboard.css"`), []byte(`href="/gc/dashboard.css"`))
	out = bytes.ReplaceAll(out, []byte(`src="/dashboard.js"`), []byte(`src="/gc/dashboard.js"`))
	out = bytes.ReplaceAll(out, []byte(`href="/assets/`), []byte(`href="/gc/assets/`))
	out = bytes.ReplaceAll(out, []byte(`src="/assets/`), []byte(`src="/gc/assets/`))

	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	return nil
}

// stripPrefix is the small helper chi's r.URL.Path doesn't actually do —
// when you mount under r.Route("/gc"), chi rewrites RoutePath but leaves
// URL.Path containing the full original "/gc/..." path. We strip it
// ourselves so the upstream sees the request it would have received if
// it were the origin server.
func stripPrefix(p, prefix string) string {
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" {
		return "/"
	}
	if !strings.HasPrefix(rest, "/") {
		// Defensive: an exact-prefix-no-slash path (e.g. "/gcfoo") shouldn't
		// be routed here, but if it is we don't want to munge it.
		return p
	}
	return rest
}

// gcUnavailable writes a 503 JSON body explaining what the user needs to
// do to bring Gas City back online. This is what gets returned when the
// startup probe failed — the user can fix it and restart aurex.
func gcUnavailable(w http.ResponseWriter, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	body := fmt.Sprintf(
		`{"error":"gc_%s_unavailable","message":"Gas City %s is not reachable from aurex. SSH to the Mac and run: gc resume ~/gascity, then restart aurex via launchctl kickstart -k gui/$(id -u)/com.aurex.server."}`,
		kind, kind,
	)
	_, _ = io.WriteString(w, body)
}

