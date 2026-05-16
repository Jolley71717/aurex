package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigChmodsLooseFile asserts that when the config file on disk
// has loose permissions (e.g. 0644), LoadConfig auto-corrects it to 0600
// rather than starting up with a world-readable VAPID private key.
func TestLoadConfigChmodsLooseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aurex.config.json")

	// Write a minimal but valid config. Auth is disabled so applyEnvAndValidate
	// doesn't require a password to be set.
	cfg := Config{
		Port:                  7681,
		Auth:                  false,
		Username:              "aurex",
		Password:              "",
		VapidPublicKey:        "test-public-key",
		VapidPrivateKey:       "test-private-key",
		DefaultShell:          "bash",
		TmuxPrefix:            "aurex",
		HTTPRedirectPort:      7680,
		Tailscale:             "off",
		TailscaleCertFile:     "aurex.ts.cert.pem",
		TailscaleKeyFile:      "aurex.ts.key.pem",
		PushSubscriptionsFile: "aurex.subscriptions.json",
		PasteDir:              "pastes",
		PasteMaxAgeHours:      24,
		SilenceSeconds:        5,
		SessionSecretFile:     "aurex.session-secret",
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write loose-perm config: %v", err)
	}

	// Confirm we really did land at 0644 (some umasks could narrow it, but
	// 0o644 should pass through cleanly on dev machines).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after seed write: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Logf("seed file landed at %#o, not 0644 — test still proceeds", mode)
	}

	// Point LoadConfig at the temp file.
	t.Setenv("AUREX_CONFIG", path)

	loaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadConfig returned nil config")
	}

	// Verify the VAPID keys round-tripped unchanged (no regeneration on boot —
	// that would invalidate every push subscription).
	if loaded.VapidPrivateKey != "test-private-key" {
		t.Errorf("VapidPrivateKey changed across LoadConfig: got %q want %q",
			loaded.VapidPrivateKey, "test-private-key")
	}
	if loaded.VapidPublicKey != "test-public-key" {
		t.Errorf("VapidPublicKey changed across LoadConfig: got %q want %q",
			loaded.VapidPublicKey, "test-public-key")
	}

	// The defense-in-depth check should have chmod'd the file to 0600.
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat after LoadConfig: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("LoadConfig did not chmod loose config to 0600, got %#o", mode)
	}
}

// TestSaveWritesStrictPerms asserts that Save() leaves the config file at
// 0600 on a clean write, matching the first-run boot path.
func TestSaveWritesStrictPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aurex.config.json")

	cfg := &Config{
		Port: 7681,
		path: path,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after Save: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("Save wrote file with mode %#o, want 0600", mode)
	}
}
