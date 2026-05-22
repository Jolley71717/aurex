package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is overwritten at build time via -ldflags=-X main.version=…
// (see .github/workflows/release.yml).
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("aurex", version)
		return
	}
	log.Printf("aurex %s starting", version)
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("aurex: load config: %v", err)
	}

	if err := InitSessionSecret(cfg.SessionSecretFile); err != nil {
		log.Fatalf("aurex: init session secret: %v", err)
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		log.Fatalf("aurex: tmux not found in PATH — install tmux to use aurex")
	}

	push := NewPushManager(cfg.VapidPublicKey, cfg.VapidPrivateKey, cfg.PushSubscriptionsFile)
	store := NewSessionStore(cfg.TmuxPrefix, cfg.DefaultShell, push, cfg.Port, cfg.SilenceSeconds)
	if err := store.AdoptExisting(); err != nil {
		log.Printf("aurex: adopt existing tmux sessions: %v", err)
	}

	var ideas *IdeasManager
	if cfg.IdeasDataRoot != "" {
		ideas = NewIdeasManager(cfg.IdeasDataRoot, cfg.IdeasRepoRoot)
		log.Printf("aurex: ideas surface enabled at %s (repo: %s)", cfg.IdeasDataRoot, cfg.IdeasRepoRoot)
		// Background poller: every 10 s, scan _paperclip-dispatches.json for
		// outstanding Revise/Generate work and apply any JSON comments the
		// persona agents have posted. Self-cancels when the process exits;
		// context.Background is fine here — aurex doesn't have graceful
		// shutdown semantics for its sub-loops.
		ideas.StartPaperclipPoller(context.Background(), 10*time.Second)
		log.Printf("aurex: paperclip dispatch poller running every 10s")
	}
	var beads *BeadsManager
	if cfg.BeadsRepoRoot != "" {
		if info, err := os.Stat(cfg.BeadsRepoRoot); err == nil && info.IsDir() {
			beads = NewBeadsManager(cfg.BeadsRepoRoot)
			log.Printf("aurex: beads surface enabled (repo: %s)", cfg.BeadsRepoRoot)
		} else {
			log.Printf("aurex: beads surface disabled (%s not found)", cfg.BeadsRepoRoot)
		}
	}
	// Agents surface — only enable if the configured Claude data root actually
	// exists on disk at boot. Saves a "directory not found" error on every
	// /api/agents poll for users who don't run `claude agents`.
	var agents *AgentsManager
	if cfg.ClaudeAgentsDataRoot != "" {
		if info, err := os.Stat(cfg.ClaudeAgentsDataRoot); err == nil && info.IsDir() {
			agents = NewAgentsManager(cfg.ClaudeAgentsDataRoot)
			if agents.ClaudePath == "" {
				log.Printf("aurex: agents surface enabled at %s (warning: `claude` CLI not on PATH — read works, stop/log won't)", cfg.ClaudeAgentsDataRoot)
			} else {
				log.Printf("aurex: agents surface enabled at %s (claude=%s)", cfg.ClaudeAgentsDataRoot, agents.ClaudePath)
			}
		} else {
			log.Printf("aurex: agents surface disabled (%s not found)", cfg.ClaudeAgentsDataRoot)
		}
	}
	// Gas City reverse proxy. Probes both upstreams once at boot; if either
	// is unreachable the corresponding mount stays disabled until restart.
	gc := newGcProxy(cfg.GcDashboardURL, cfg.GcSupervisorURL)

	server := NewServer(cfg, store, push, ideas, beads, agents, gc, Frontend())
	store.SetOnUpdate(server.BroadcastSessionUpdate)

	stop := make(chan struct{})
	go store.PollMetadata(3*time.Second, stop)
	go store.PollIdle(stop)
	go startPasteJanitor(cfg.PasteDir, time.Duration(cfg.PasteMaxAgeHours)*time.Hour, stop)

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	httpServer := &http.Server{
		Addr:     addr,
		Handler:  server.Routes(),
		ErrorLog: log.New(quietTLSWriter{}, "", log.LstdFlags),
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("aurex: shutting down (tmux sessions remain alive)")
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	// TLS is Tailscale-only — aurex never generates self-signed certs.
	// Without Tailscale we just serve HTTP and push notifications won't work,
	// which is the right trade for not making users wrestle with cert install
	// on their phone.
	useTLS, certFile, keyFile, publicURL := resolveTailscaleCert(cfg, stop)

	bindLabel := cfg.BindAddr
	if bindLabel == "" {
		bindLabel = "0.0.0.0"
	}
	switch {
	case useTLS:
		log.Printf("aurex: bound to %s — open %s on your phone — real cert, no warnings", bindLabel, publicURL)
	case cfg.Tailscale == "on":
		log.Fatalf("aurex: tailscale required (cfg.tailscale=on) but unavailable")
	default:
		log.Printf("aurex: listening on http://%s:%d — install Tailscale and set HTTPS in the admin console for push notifications", bindLabel, cfg.Port)
	}

	if useTLS && cfg.HTTPRedirectPort > 0 {
		go runHTTPRedirect(cfg.BindAddr, cfg.HTTPRedirectPort, cfg.Port)
	}

	var serveErr error
	if useTLS {
		serveErr = httpServer.ListenAndServeTLS(certFile, keyFile)
	} else {
		serveErr = httpServer.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatalf("aurex: serve: %v", serveErr)
	}
}

// resolveTailscaleCert tries to obtain a Tailscale-issued Let's Encrypt cert
// for this node's MagicDNS FQDN. Returns useTLS=false on any failure unless
// cfg.Tailscale == "on" (in which case the caller fatals). On success kicks
// off the daily renewal goroutine.
//
// macOS launchd note: on macOS, the brew tailscale CLI cannot reach the
// IPNExtension daemon when invoked from a launchd context (the Mach port
// access just isn't there). Both `tailscale status` and `tailscale cert`
// fail. If pre-fetched cert + key files exist at the configured paths
// (populated by a user-context process — initial setup or a renewal
// LaunchAgent), fall back to using them instead of dropping to HTTP. This
// keeps push notifications working when aurex itself runs under launchd.
func resolveTailscaleCert(cfg *Config, stop <-chan struct{}) (useTLS bool, certFile, keyFile, publicURL string) {
	if cfg.Tailscale == "off" {
		return false, "", "", ""
	}

	fallbackFQDN := cfg.TailscaleStaticFQDN
	fallbackOK := fallbackFQDN != "" && certFilesReady(cfg.TailscaleCertFile, cfg.TailscaleKeyFile)
	staticURL := func() string { return fmt.Sprintf("https://%s:%d", fallbackFQDN, cfg.Port) }

	fqdn, err := TailscaleFQDN()
	if err != nil {
		if fallbackOK {
			log.Printf("aurex: tailscale daemon unreachable (%v) — falling back to static cert for %s", err, fallbackFQDN)
			return true, cfg.TailscaleCertFile, cfg.TailscaleKeyFile, staticURL()
		}
		if cfg.Tailscale == "on" {
			log.Fatalf("aurex: tailscale required but unavailable: %v", err)
		}
		// Quiet about the common case (tailscale not installed).
		if !strings.Contains(err.Error(), "not in PATH") {
			log.Printf("aurex: tailscale not usable (%v) — serving HTTP", err)
		}
		return false, "", "", ""
	}
	if err := TailscaleEnsureCert(fqdn, cfg.TailscaleCertFile, cfg.TailscaleKeyFile); err != nil {
		if fallbackOK {
			log.Printf("aurex: tailscale cert refresh failed (%v) — using existing cert files for %s", err, fallbackFQDN)
			return true, cfg.TailscaleCertFile, cfg.TailscaleKeyFile, staticURL()
		}
		if cfg.Tailscale == "on" {
			log.Fatalf("aurex: tailscale cert required but failed: %v", err)
		}
		log.Printf("aurex: tailscale cert unavailable (%v) — serving HTTP", err)
		return false, "", "", ""
	}
	log.Printf("aurex: using Tailscale cert for %s (auto-renew on restart)", fqdn)
	go renewTailscaleCert(fqdn, cfg.TailscaleCertFile, cfg.TailscaleKeyFile, stop)
	return true, cfg.TailscaleCertFile, cfg.TailscaleKeyFile, fmt.Sprintf("https://%s:%d", fqdn, cfg.Port)
}

// certFilesReady returns true when both cert and key files exist and are
// non-empty. Used by the launchd-context fallback path.
func certFilesReady(certPath, keyPath string) bool {
	if certPath == "" || keyPath == "" {
		return false
	}
	c, err := os.Stat(certPath)
	if err != nil || c.Size() == 0 {
		return false
	}
	k, err := os.Stat(keyPath)
	if err != nil || k.Size() == 0 {
		return false
	}
	return true
}

// quietTLSWriter drops TLS handshake noise (clients with stale/wrong certs,
// scanners, etc.) but lets real http.Server errors through to stderr.
type quietTLSWriter struct{}

func (quietTLSWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.Contains(s, "TLS handshake error") ||
		strings.Contains(s, "tls: unknown certificate") ||
		strings.Contains(s, "tls: client didn't provide a certificate") {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// runHTTPRedirect serves an HTTP-only listener that 301-redirects everything
// to the same host on the HTTPS port. Honors bindAddr so the redirect
// listener doesn't leak onto interfaces aurex was kept off of.
func runHTTPRedirect(bindAddr string, httpPort, httpsPort int) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || host == "" {
			host = r.Host
		}
		target := fmt.Sprintf("https://%s:%d%s", host, httpsPort, r.RequestURI)
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	addr := fmt.Sprintf("%s:%d", bindAddr, httpPort)
	log.Printf("aurex: HTTP→HTTPS redirect listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Printf("aurex: http redirect server: %v (continuing without redirect)", err)
	}
}

// renewTailscaleCert re-runs `tailscale cert` daily so long-running aurex
// instances pick up Let's Encrypt renewals (90-day lifetime). The daemon
// no-ops when the cert isn't near expiry, so this is cheap.
func renewTailscaleCert(fqdn, certFile, keyFile string, stop <-chan struct{}) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := TailscaleEnsureCert(fqdn, certFile, keyFile); err != nil {
				log.Printf("aurex: tailscale cert renew: %v", err)
			}
		}
	}
}
