package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type Config struct {
	Port            int    `json:"port"`
	Auth            bool   `json:"auth"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	VapidPublicKey  string `json:"vapidPublicKey"`
	VapidPrivateKey string `json:"vapidPrivateKey"`
	DefaultShell    string `json:"defaultShell"`
	TmuxPrefix      string `json:"tmuxPrefix"`

	// HTTPRedirectPort, when non-zero and TLS is on, starts a sibling HTTP
	// server that 301-redirects every request to the HTTPS origin. Default
	// 7680 (one below the main port, no privileges needed).
	HTTPRedirectPort int `json:"httpRedirectPort"`

	// Tailscale-only TLS. Aurex does not generate self-signed certs — the
	// install/trust dance on phones is too miserable to be worth it.
	//
	//   "auto" (default): try Tailscale; fall back to plain HTTP if unavailable.
	//   "on":             require Tailscale; refuse to start without it.
	//   "off":            run HTTP-only, no TLS attempt.
	//
	// When TLS is unavailable, push notifications won't work (browsers require
	// a secure context), but the terminal itself runs fine over plain HTTP on LAN.
	Tailscale         string `json:"tailscale"`
	TailscaleCertFile string `json:"tailscaleCertFile"`
	TailscaleKeyFile  string `json:"tailscaleKeyFile"`

	// TailscaleStaticFQDN is the MagicDNS hostname this server expects to
	// be reached as. When the `tailscale` CLI is unreachable from the
	// process context (notably: macOS LaunchAgents — the IPNExtension
	// daemon's Mach port isn't accessible from launchd-spawned processes),
	// aurex falls back to using the pre-fetched cert files at
	// TailscaleCertFile/TailscaleKeyFile and uses this FQDN to build the
	// public URL. Cert renewal in that mode is the caller's responsibility
	// (e.g. a sibling LaunchAgent that runs `tailscale cert` from the user
	// GUI session every N days).
	TailscaleStaticFQDN string `json:"tailscaleStaticFQDN"`

	PushSubscriptionsFile string `json:"pushSubscriptionsFile"`

	// PasteDir is where the image-paste upload endpoint writes incoming
	// screenshots. The terminal pane then types the absolute path so
	// downstream tools (Claude Code, vim, etc.) can open the file.
	// Empty defaults to "pastes" relative to the working dir.
	PasteDir string `json:"pasteDir"`

	// PasteMaxAgeHours: files in PasteDir with mtime older than this are
	// reaped by a janitor goroutine. 0 = disabled (no cleanup). Default 24.
	PasteMaxAgeHours int `json:"pasteMaxAgeHours"`

	// SilenceSeconds: how long a pane has to be quiet before aurex fires its
	// "agent waiting" aura. Backed by tmux's monitor-silence + alert-silence
	// hook so it requires no agent-side config. Tune up if long-running
	// builds or thinking phases are creating false positives.
	SilenceSeconds int `json:"silenceSeconds"`

	// BindAddr restricts the listener to a single interface address. Empty
	// means listen on all interfaces (the upstream default). Set to a
	// Tailscale interface IP (e.g. "100.x.y.z") to keep aurex off your LAN
	// entirely. The HTTP redirect listener honors the same address.
	BindAddr string `json:"bindAddr"`

	// AllowedOrigins is the WebSocket Origin allowlist. Empty means "the
	// request's own Host header must match the Origin host" (the safest
	// default — prevents cross-origin WS hijack). Add hostnames here only
	// if you reach aurex from a different vanity DNS name.
	AllowedOrigins []string `json:"allowedOrigins"`

	// SessionSecretFile is the path where the HMAC key for signed session
	// cookies is persisted, so process restarts don't invalidate every
	// logged-in device. Created mode 0600 on first boot. Empty falls back
	// to ephemeral (in-memory) keys — restart kicks everyone.
	SessionSecretFile string `json:"sessionSecretFile"`

	// IdeasDataRoot is the absolute path to the directory holding the
	// per-persona JSONL idea files (e.g. unchained-0-projectPlan/ideas).
	// IdeasRepoRoot is the absolute path to the git repo root that contains
	// it — used as the cwd for `git commit` + `git push` when persona
	// prompts are saved, and for `bd create` when an idea is accepted.
	// Empty values disable the /api/ideas surface entirely.
	IdeasDataRoot string `json:"ideasDataRoot"`
	IdeasRepoRoot string `json:"ideasRepoRoot"`

	// BeadsRepoRoot is the working directory for all `bd` invocations.
	// Defaults to ~/git/k3s-pi. Empty disables the /api/beads surface.
	BeadsRepoRoot string `json:"beadsRepoRoot"`

	// ClaudeAgentsDataRoot is the directory holding Claude Code's local
	// agent state — typically $HOME/.claude. The /api/agents surface reads
	// roster.json + per-agent state.json from inside this tree. Set to
	// empty (or point at a non-existent directory) to disable the agents
	// surface entirely.
	//
	// LoadConfig() auto-populates this with $HOME/.claude on first run if
	// that directory actually exists — that's the common case for a Mac
	// running `claude agents` in the same user account as aurex.
	ClaudeAgentsDataRoot string `json:"claudeAgentsDataRoot"`

	// GcDashboardURL is the local Gas City dashboard origin (default
	// http://localhost:8088). Empty disables the /gc proxy. Probed at startup
	// — if unreachable the proxy stays off until aurex restarts.
	GcDashboardURL string `json:"gcDashboardUrl"`

	// GcSupervisorURL is the local Gas City supervisor API origin (default
	// http://127.0.0.1:8372). Empty disables the /gc-api proxy. Probed at
	// startup against /health; failure also disables /gc since the dashboard
	// is useless without its API.
	GcSupervisorURL string `json:"gcSupervisorUrl"`

	path string
}

func defaultConfigPath() string {
	if p := os.Getenv("AUREX_CONFIG"); p != "" {
		return p
	}
	return "aurex.config.json"
}

// defaultClaudeAgentsRoot returns $HOME/.claude if that directory exists,
// or "" if it can't be resolved. The /api/agents surface activates only
// when this is non-empty AND the path exists at boot — keeps the surface
// silent on machines that don't run `claude agents`.
func defaultBeadsRepoRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "git", "k3s-pi")
}

func defaultClaudeAgentsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	candidate := filepath.Join(home, ".claude")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}

func LoadConfig() (*Config, error) {
	path := defaultConfigPath()
	cfg := &Config{
		Port:                  7681,
		BeadsRepoRoot:         defaultBeadsRepoRoot(),
		Auth:                  true,
		Username:              "aurex",
		Password:              "", // populated from AUREX_PASSWORD env at boot
		DefaultShell:          "bash",
		TmuxPrefix:            "aurex",
		HTTPRedirectPort:      7680,
		Tailscale:             "auto",
		TailscaleCertFile:     "aurex.ts.cert.pem",
		TailscaleKeyFile:      "aurex.ts.key.pem",
		PushSubscriptionsFile: "aurex.subscriptions.json",
		SilenceSeconds:        5,
		BindAddr:              "",
		AllowedOrigins:        nil,
		PasteDir:              "pastes",
		PasteMaxAgeHours:      24,
		SessionSecretFile:     "aurex.session-secret",
		ClaudeAgentsDataRoot:  defaultClaudeAgentsRoot(),
		GcDashboardURL:        "http://localhost:8088",
		GcSupervisorURL:       "http://127.0.0.1:8372",
		path:                  path,
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := cfg.ensureVapid(); err != nil {
			return nil, err
		}
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		fmt.Printf("aurex: wrote default config to %s\n", path)
		return applyEnvAndValidate(cfg, path)
	}
	if err != nil {
		return nil, err
	}

	// Defense-in-depth: the config file holds the VAPID private key (and
	// potentially the auth password). It must be owner-read-only. If we
	// find anything looser than 0600 (e.g. 0644 from a sloppy manual edit
	// or a previous version), loudly warn and auto-chmod. We don't refuse
	// to start — that bricks first-run boots — we just self-heal.
	enforceConfigPerms(path)

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.path = path

	dirty := false
	if cfg.VapidPublicKey == "" || cfg.VapidPrivateKey == "" {
		if err := cfg.ensureVapid(); err != nil {
			return nil, err
		}
		dirty = true
	}
	if cfg.DefaultShell == "" {
		cfg.DefaultShell = "bash"
		dirty = true
	}
	if cfg.TmuxPrefix == "" {
		cfg.TmuxPrefix = "aurex"
		dirty = true
	}
	if cfg.Port == 0 {
		cfg.Port = 7681
		dirty = true
	}
	if cfg.Tailscale == "" {
		cfg.Tailscale = "auto"
		dirty = true
	}
	if cfg.TailscaleCertFile == "" {
		cfg.TailscaleCertFile = "aurex.ts.cert.pem"
		dirty = true
	}
	if cfg.TailscaleKeyFile == "" {
		cfg.TailscaleKeyFile = "aurex.ts.key.pem"
		dirty = true
	}
	if cfg.PushSubscriptionsFile == "" {
		cfg.PushSubscriptionsFile = "aurex.subscriptions.json"
		dirty = true
	}
	if cfg.PasteDir == "" {
		cfg.PasteDir = "pastes"
		dirty = true
	}
	if cfg.PasteMaxAgeHours == 0 {
		cfg.PasteMaxAgeHours = 24
		dirty = true
	}
	if cfg.SessionSecretFile == "" {
		cfg.SessionSecretFile = "aurex.session-secret"
		dirty = true
	}
	if cfg.SilenceSeconds <= 0 {
		cfg.SilenceSeconds = 5
		dirty = true
	}
	// Backfill the GC proxy URLs for existing configs. We never overwrite
	// a non-empty value; only the truly-missing case gets a default. Set to
	// "-" (or any other invalid URL) to disable while keeping the field
	// present in the JSON.
	if cfg.GcDashboardURL == "" {
		cfg.GcDashboardURL = "http://localhost:8088"
		dirty = true
	}
	if cfg.GcSupervisorURL == "" {
		cfg.GcSupervisorURL = "http://127.0.0.1:8372"
		dirty = true
	}
	// Backfill ClaudeAgentsDataRoot for existing configs. We never overwrite
	// a non-empty value — if the user explicitly cleared it (or pointed it
	// somewhere else) we respect that. Only the truly-missing case gets
	// auto-populated from $HOME/.claude.
	if cfg.ClaudeAgentsDataRoot == "" {
		if def := defaultClaudeAgentsRoot(); def != "" {
			cfg.ClaudeAgentsDataRoot = def
			dirty = true
		}
	}
	if cfg.BeadsRepoRoot == "" {
		cfg.BeadsRepoRoot = defaultBeadsRepoRoot()
		dirty = true
	}
	if dirty {
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}
	return applyEnvAndValidate(cfg, path)
}

// applyEnvAndValidate layers AUREX_PASSWORD onto the loaded config and
// refuses to return a config that would start aurex in a known-broken
// auth state. Called from both the first-run and existing-config branches
// so the env-overlay + safety check happen exactly once.
func applyEnvAndValidate(cfg *Config, path string) (*Config, error) {
	// AUREX_PASSWORD env var wins over the config file value. Lets the
	// password come from 1Password / a secret manager without ever landing
	// on disk. Empty env => fall through to the file value.
	if envPw := os.Getenv("AUREX_PASSWORD"); envPw != "" {
		cfg.Password = envPw
	}

	if cfg.Auth {
		if cfg.Password == "" {
			return nil, fmt.Errorf("aurex: auth is enabled but password is empty — set AUREX_PASSWORD or write a password into %s", path)
		}
		if cfg.Password == "changeme" {
			return nil, fmt.Errorf("aurex: refusing to start with the default password \"changeme\" — set AUREX_PASSWORD or edit %s", path)
		}
	}

	return cfg, nil
}

func (c *Config) ensureVapid() error {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("generate vapid keys: %w", err)
	}
	c.VapidPrivateKey = priv
	c.VapidPublicKey = pub
	return nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return err
	}
	// Verify the write landed with 0600. Some filesystems (or a hostile
	// umask interfering with os.WriteFile semantics on edge platforms)
	// could leave looser perms; in that case force a chmod and warn so the
	// operator notices.
	if info, statErr := os.Stat(c.path); statErr == nil {
		mode := info.Mode().Perm()
		if mode != 0o600 && mode != 0o400 {
			fmt.Fprintf(os.Stderr, "aurex: WARNING %s landed with mode %#o after write, forcing 0600\n", c.path, mode)
			if chmodErr := os.Chmod(c.path, 0o600); chmodErr != nil {
				return fmt.Errorf("chmod config after save: %w", chmodErr)
			}
		}
	}
	return nil
}

// enforceConfigPerms stats the config file and chmods it to 0600 if it's
// looser than that. 0400 is also accepted as "stricter than 0600". Errors
// during stat/chmod are logged but not fatal — boot ergonomics win over
// strict enforcement for a single-user dev tool.
func enforceConfigPerms(path string) {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aurex: WARNING could not stat %s for permission check: %v\n", path, err)
		return
	}
	mode := info.Mode().Perm()
	if mode == 0o600 || mode == 0o400 {
		return
	}
	fmt.Fprintf(os.Stderr, "aurex: WARNING %s has insecure mode %#o (expected 0600); auto-correcting. This file contains the VAPID private key and must not be world-readable.\n", path, mode)
	if err := os.Chmod(path, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "aurex: WARNING failed to chmod %s to 0600: %v — please fix manually\n", path, err)
	}
}
