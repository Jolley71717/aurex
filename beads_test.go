package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBeadsCacheInvalidation verifies that mutations clear the list cache so
// the next GET re-runs bd list rather than returning stale data.
func TestBeadsCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	m := NewBeadsManager(dir)

	// Seed a warm cache entry.
	issuesFile := filepath.Join(dir, ".beads", "issues.jsonl")
	if err := os.MkdirAll(filepath.Dir(issuesFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(issuesFile, []byte(`{"id":"k3s-0001"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(issuesFile)
	if err != nil {
		t.Fatal(err)
	}
	warmData := json.RawMessage(`[{"id":"k3s-0001"}]`)
	m.cache = beadsCache{
		data:    warmData,
		modTime: info.ModTime(),
		size:    info.Size(),
		at:      time.Now(),
	}

	// Sanity: cache is populated.
	if m.cache.data == nil {
		t.Fatal("expected cache to be warm before invalidation")
	}

	// invalidateCache clears it.
	m.invalidateCache()

	if m.cache.data != nil {
		t.Fatal("expected cache to be nil after invalidation")
	}
	if !m.cache.at.IsZero() {
		t.Fatal("expected cache timestamp to be zero after invalidation")
	}
}
