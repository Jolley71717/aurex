package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBeadsEventsDiff covers the snapshot-diff logic: the rescan must emit
// one event per real change (created / updated / closed), drop no-op
// rescans, and never emit duplicates when the file is rewritten with the
// same content.
func TestBeadsEventsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newBeadsEvents(path, dir)
	// Diff test uses the file-based source so it doesn't shell bd.
	e.source = e.readSnapshot
	e.pollInterval = 0

	// Seed: one open issue. We prime the snapshot manually so the first
	// rescan after a real edit doesn't fire a "created" for the seed.
	writeJSONL(t, path, []string{
		`{"id":"k3s-001","status":"open","updated_at":"2026-05-20T10:00:00Z"}`,
	})
	snap, err := e.readSnapshot()
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	e.snapshot = snap

	ch := make(chan beadEvent, 8)
	e.addClient(ch)
	defer e.removeClient(ch)

	// Step 1: add a new issue. Expect a single "created" event.
	writeJSONL(t, path, []string{
		`{"id":"k3s-001","status":"open","updated_at":"2026-05-20T10:00:00Z"}`,
		`{"id":"k3s-002","status":"open","updated_at":"2026-05-20T10:05:00Z"}`,
	})
	e.rescan()
	got := drain(ch, 100*time.Millisecond)
	if len(got) != 1 || got[0].Type != "created" || got[0].BeadID != "k3s-002" {
		t.Fatalf("step1: want one created for k3s-002, got %#v", got)
	}

	// Step 2: rewrite identical content. Expect no events.
	writeJSONL(t, path, []string{
		`{"id":"k3s-001","status":"open","updated_at":"2026-05-20T10:00:00Z"}`,
		`{"id":"k3s-002","status":"open","updated_at":"2026-05-20T10:05:00Z"}`,
	})
	e.rescan()
	got = drain(ch, 50*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("step2: want no events on no-op rescan, got %#v", got)
	}

	// Step 3: close k3s-001 (status flips open → closed). Expect "closed".
	writeJSONL(t, path, []string{
		`{"id":"k3s-001","status":"closed","updated_at":"2026-05-20T10:10:00Z"}`,
		`{"id":"k3s-002","status":"open","updated_at":"2026-05-20T10:05:00Z"}`,
	})
	e.rescan()
	got = drain(ch, 100*time.Millisecond)
	if len(got) != 1 || got[0].Type != "closed" || got[0].BeadID != "k3s-001" {
		t.Fatalf("step3: want one closed for k3s-001, got %#v", got)
	}

	// Step 4: edit k3s-002 (bump updated_at). Expect "updated".
	writeJSONL(t, path, []string{
		`{"id":"k3s-001","status":"closed","updated_at":"2026-05-20T10:10:00Z"}`,
		`{"id":"k3s-002","status":"open","updated_at":"2026-05-20T10:20:00Z"}`,
	})
	e.rescan()
	got = drain(ch, 100*time.Millisecond)
	if len(got) != 1 || got[0].Type != "updated" || got[0].BeadID != "k3s-002" {
		t.Fatalf("step4: want one updated for k3s-002, got %#v", got)
	}
}

// TestBeadsEventsWatcherEndToEnd writes to the jsonl, lets fsnotify fire,
// and verifies the SSE handler emits a `event: bead` frame within the
// debounce window. Catches regressions in the watcher/debounce wiring
// that a pure-diff test would miss.
func TestBeadsEventsWatcherEndToEnd(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "issues.jsonl")
	writeJSONL(t, path, []string{
		`{"id":"k3s-100","status":"open","updated_at":"2026-05-20T10:00:00Z"}`,
	})

	m := NewBeadsManager(dir)
	// Sanity: NewBeadsManager wired events to the right path.
	if m.events == nil || m.events.path != path {
		t.Fatalf("events not wired: %#v", m.events)
	}
	// End-to-end watcher test reads from the fixture jsonl rather than
	// shelling bd, and disables the poll so we exercise the fsnotify path.
	m.events.source = m.events.readSnapshot
	m.events.pollInterval = 0

	srv := httptest.NewServer(http.HandlerFunc(m.handleEvents))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)
	// Read the initial ": connected" comment so we know the watcher is up.
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read connected: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("first line = %q, want comment", line)
	}
	// Consume the blank separator after the comment.
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read separator: %v", err)
	}

	// Give the watcher goroutine a beat to subscribe to fsnotify before
	// we modify the file. Without this, the write can race the watcher's
	// initial Add() and the event is lost.
	time.Sleep(100 * time.Millisecond)

	// Mutate the file: add a new issue.
	writeJSONL(t, path, []string{
		`{"id":"k3s-100","status":"open","updated_at":"2026-05-20T10:00:00Z"}`,
		`{"id":"k3s-101","status":"open","updated_at":"2026-05-20T10:30:00Z"}`,
	})

	// Read until we see an `event: bead` frame or the context expires.
	deadline := time.Now().Add(2 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: bead") {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("did not see event: bead frame after file mutation")
	}
}

// TestBeadsEventsPollFiresWithoutFsnotify simulates the QA failure: a
// mutation that never touches issues.jsonl (e.g. `bd close` writing to
// Dolt only). The fsnotify watcher is silent; only the periodic poll can
// surface the change. Verifies a `closed` event still arrives within the
// poll interval.
func TestBeadsEventsPollFiresWithoutFsnotify(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "issues.jsonl")

	// In-memory source the test mutates directly. Starts with one open bead.
	state := map[string]beadSnapshot{
		"k3s-200": {Status: "open", UpdatedAt: "2026-05-20T10:00:00Z"},
	}
	var stateMu sync.Mutex

	m := NewBeadsManager(dir)
	m.events.source = func() (map[string]beadSnapshot, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		out := make(map[string]beadSnapshot, len(state))
		for k, v := range state {
			out[k] = v
		}
		return out, nil
	}
	m.events.pollInterval = 100 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(m.handleEvents))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	// Drain the initial connect comment + blank separator.
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read connected: %v", err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read separator: %v", err)
	}

	// Let start() prime the snapshot.
	time.Sleep(150 * time.Millisecond)

	// Mutate the source state ONLY — no file write, so fsnotify is silent.
	// The bd close from a shell looks like this from aurex's perspective:
	// bd state changes, the JSONL is untouched.
	stateMu.Lock()
	state["k3s-200"] = beadSnapshot{Status: "closed", UpdatedAt: "2026-05-20T10:05:00Z"}
	stateMu.Unlock()

	// Stat the jsonl path is not necessary; just verify no file exists,
	// proving the fsnotify watcher cannot have fired.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("issues.jsonl unexpectedly present: %v", err)
	}

	// Wait for the poll ticker to pick it up. Acceptance is "within 5s";
	// we give ourselves 2s.
	deadline := time.Now().Add(2 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: bead") {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("did not see event: bead from poll-only mutation")
	}
}

func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func drain(ch chan beadEvent, wait time.Duration) []beadEvent {
	var out []beadEvent
	timeout := time.After(wait)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-timeout:
			return out
		}
	}
}
