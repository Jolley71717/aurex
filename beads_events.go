package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// beadEvent is the wire shape emitted on the SSE stream as `event: bead`.
// Fields match what the UI needs to invalidate or refresh its list view.
type beadEvent struct {
	Type   string `json:"type"`   // "created" | "updated" | "closed"
	BeadID string `json:"beadId"` // e.g. "k3s-XXXX"
	At     string `json:"at"`     // ISO-8601 timestamp from the record
}

// beadSnapshot is the per-issue state we track between scans so we can diff
// the current file against the previous file and emit only real deltas.
type beadSnapshot struct {
	Status    string
	UpdatedAt string
}

// beadRecord is the minimal subset of an issues.jsonl line we need for diffing.
// bd's records have many more fields; json.Unmarshal ignores the rest.
type beadRecord struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// beadsEvents owns the in-memory snapshot of the bd issue set and the
// per-client SSE fan-out channels.
//
// Source of truth is `bd list --json --all`, polled on an interval and also
// re-triggered by an fsnotify watch on .beads/issues.jsonl. The watcher gives
// low-latency reaction when bd is configured to rewrite the JSONL file on
// mutation (no-db mode); the ticker covers the case where bd writes to Dolt
// and the JSONL export lags or never updates (e.g. `bd close` run in a shell).
// Either trigger calls scheduleRescan(), which is debounced so a burst (poll
// races a write) collapses to a single rescan.
type beadsEvents struct {
	path     string
	repoRoot string

	// source returns the current snapshot. Defaults to readSnapshotFromBd in
	// production; tests override to read a fixture JSONL file directly.
	source func() (map[string]beadSnapshot, error)

	// pollInterval is the cadence at which we re-snapshot via bd. Shell-driven
	// mutations (no JSONL write) are seen within one tick.
	pollInterval time.Duration

	startOnce sync.Once

	mu       sync.Mutex
	clients  map[chan beadEvent]struct{}
	snapshot map[string]beadSnapshot
	seq      uint64

	debounceMu sync.Mutex
	debounce   *time.Timer
}

func newBeadsEvents(path, repoRoot string) *beadsEvents {
	e := &beadsEvents{
		path:         path,
		repoRoot:     repoRoot,
		clients:      make(map[chan beadEvent]struct{}),
		snapshot:     make(map[string]beadSnapshot),
		pollInterval: 2 * time.Second,
	}
	e.source = e.readSnapshotFromBd
	return e
}

// start lazily kicks off the fsnotify watcher. Safe to call repeatedly; only
// the first call does any work. Started lazily (on first SSE connect) so a
// boot with zero SSE clients doesn't hold a kqueue/inotify watch open.
func (e *beadsEvents) start() {
	e.startOnce.Do(func() {
		// Prime the snapshot so the first real change emits a real diff
		// instead of N "created" events for every existing bead.
		if snap, err := e.source(); err == nil {
			e.mu.Lock()
			e.snapshot = snap
			e.mu.Unlock()
		}

		// Periodic poll — catches Dolt-only mutations (`bd close` in a shell)
		// that don't touch issues.jsonl and therefore wouldn't fire fsnotify.
		if e.pollInterval > 0 {
			go func() {
				ticker := time.NewTicker(e.pollInterval)
				defer ticker.Stop()
				for range ticker.C {
					e.scheduleRescan()
				}
			}()
		}

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Printf("aurex: beads SSE watcher init: %v", err)
			return
		}
		// Watch the parent directory rather than the file itself: bd may
		// rotate or atomically replace issues.jsonl (rename-into-place),
		// which destroys a watch on the file but is still visible at the
		// directory level. Filtering by basename keeps us focused.
		dir := filepath.Dir(e.path)
		if err := watcher.Add(dir); err != nil {
			log.Printf("aurex: beads SSE watcher add %s: %v", dir, err)
			_ = watcher.Close()
			return
		}
		fname := filepath.Base(e.path)

		go func() {
			defer watcher.Close()
			for {
				select {
				case ev, ok := <-watcher.Events:
					if !ok {
						return
					}
					if filepath.Base(ev.Name) != fname {
						continue
					}
					e.scheduleRescan()
				case err, ok := <-watcher.Errors:
					if !ok {
						return
					}
					log.Printf("aurex: beads watcher error: %v", err)
				}
			}
		}()
	})
}

// scheduleRescan coalesces write bursts into a single rescan. bd may emit
// multiple fsnotify events for one logical update (write + rename + chmod);
// the 50ms debounce makes us read the file once after the dust settles.
func (e *beadsEvents) scheduleRescan() {
	e.debounceMu.Lock()
	defer e.debounceMu.Unlock()
	if e.debounce != nil {
		e.debounce.Stop()
	}
	e.debounce = time.AfterFunc(50*time.Millisecond, e.rescan)
}

// rescan asks the configured source for the current snapshot, diffs against
// the previous snapshot, and fans out events. Called from the debounce timer
// goroutine.
func (e *beadsEvents) rescan() {
	next, err := e.source()
	if err != nil {
		// Source might be momentarily unavailable (file rename, bd subprocess
		// failure). Wait for the next trigger.
		return
	}

	e.mu.Lock()
	prev := e.snapshot
	var events []beadEvent
	for id, ns := range next {
		ps, ok := prev[id]
		if !ok {
			events = append(events, beadEvent{Type: "created", BeadID: id, At: ns.UpdatedAt})
			continue
		}
		if ns.Status == "closed" && ps.Status != "closed" {
			events = append(events, beadEvent{Type: "closed", BeadID: id, At: ns.UpdatedAt})
			continue
		}
		if ns.UpdatedAt != ps.UpdatedAt {
			events = append(events, beadEvent{Type: "updated", BeadID: id, At: ns.UpdatedAt})
		}
	}
	e.snapshot = next
	if len(events) > 0 {
		e.seq++
	}
	clients := make([]chan beadEvent, 0, len(e.clients))
	for c := range e.clients {
		clients = append(clients, c)
	}
	e.mu.Unlock()

	for _, ev := range events {
		for _, c := range clients {
			select {
			case c <- ev:
			default:
				// Slow client — drop rather than block the watcher. The
				// client should re-fetch the list on reconnect anyway.
			}
		}
	}
}

// readSnapshotFromBd shells `bd list --json --all --limit 0` against the
// configured repo and returns the issue map. This is the production source —
// it works in both no-db (JSONL) and Dolt-backed bd repos, because bd reads
// from whichever store is configured. `--all` is required so we see closed
// beads (needed to detect close transitions); `--limit 0` removes the default
// 50-row cap.
func (e *beadsEvents) readSnapshotFromBd() (map[string]beadSnapshot, error) {
	if e.repoRoot == "" {
		return nil, fmt.Errorf("beadsEvents: repoRoot not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", "list", "--json", "--all", "--limit", "0")
	cmd.Dir = e.repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var records []beadRecord
	if err := json.Unmarshal(out, &records); err != nil {
		return nil, err
	}
	snap := make(map[string]beadSnapshot, len(records))
	for _, r := range records {
		if r.ID == "" {
			continue
		}
		snap[r.ID] = beadSnapshot{Status: r.Status, UpdatedAt: r.UpdatedAt}
	}
	return snap, nil
}

// readSnapshot reads the JSONL file directly. Kept as an alternate source for
// tests (and as a hot-path fallback if someone wires it back in); production
// uses readSnapshotFromBd via e.source.
func (e *beadsEvents) readSnapshot() (map[string]beadSnapshot, error) {
	f, err := os.Open(e.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	snap := make(map[string]beadSnapshot)
	scanner := bufio.NewScanner(f)
	// bd descriptions can be long — give the scanner a generous line cap.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r beadRecord
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.ID == "" {
			continue
		}
		snap[r.ID] = beadSnapshot{Status: r.Status, UpdatedAt: r.UpdatedAt}
	}
	return snap, scanner.Err()
}

func (e *beadsEvents) addClient(c chan beadEvent) {
	e.mu.Lock()
	e.clients[c] = struct{}{}
	e.mu.Unlock()
}

func (e *beadsEvents) removeClient(c chan beadEvent) {
	e.mu.Lock()
	delete(e.clients, c)
	e.mu.Unlock()
}

// bumpSeq is called by mutation handlers (close/reopen/update/...) so the
// list cache and the SSE stream share a monotonically increasing version
// number. The fsnotify watcher will emit the corresponding bead event
// shortly after; bumping here is the synchronous half of "cache + SSE stay
// coherent" — list responses can be tagged with seq to detect drift.
func (e *beadsEvents) bumpSeq() {
	e.mu.Lock()
	e.seq++
	e.mu.Unlock()
}

func (e *beadsEvents) currentSeq() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

// handleEvents serves the SSE stream. Per-client channel; the watcher
// goroutine fans events out, and a 25s ticker writes keep-alive comments
// to defeat proxy idle timeouts.
func (m *BeadsManager) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if m.events == nil {
		// Defense-in-depth: NewBeadsManager initializes events, but if a
		// caller constructed BeadsManager directly we still cope.
		m.events = newBeadsEvents(m.issuesJSONLPath(), m.RepoRoot)
	}
	m.events.start()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx and friends sometimes buffer text/event-stream responses; this
	// header tells them not to.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Initial comment so the client's onopen fires immediately and the proxy
	// commits to streaming mode.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch := make(chan beadEvent, 64)
	m.events.addClient(ch)
	defer m.events.removeClient(ch)

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: bead\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

