package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// IdeasManager owns the on-disk store for C-suite persona ideas. It is the
// backing for the /ideas surface in the SPA. v1 reads pre-seeded JSONL files
// under <root>/<YYYY-MM-DD>-<persona>.jsonl, keeps review state in a single
// sidecar JSON, and appends declined entries to _declined.jsonl.
//
// The root path doubles as the k3s-pi repo root + "ideas" subfolder. We
// shell out to git from the parent repo on persona-prompt saves, so the
// IdeasManager needs to know both:
//   - DataRoot:   the directory holding the JSONL idea files (always the
//                 "ideas/" folder inside the k3s-pi repo).
//   - RepoRoot:   the git repo root for commits (parent of DataRoot's
//                 unchained-0-projectPlan ancestor).
//
// All mutations take the manager's mu — the SPA and any future cron writer
// share the same process so serialized access through the single instance
// is sufficient. If the cron ever moves to a separate process, swap to
// file locks.
type IdeasManager struct {
	DataRoot string // absolute path to the "ideas/" folder
	RepoRoot string // absolute path to the git repo root
	mu       sync.Mutex
}

// Idea is the wire shape the inbox renders. revision_count + state come
// from the sidecar review-state file, never from the JSONL. accepted_bead_id
// is set when an accept action shells bd create successfully.
type Idea struct {
	ID                  string   `json:"id"`
	Persona             string   `json:"persona"`
	Date                string   `json:"date"`
	Title               string   `json:"title"`
	Body                string   `json:"body"`
	References          []string `json:"references"`
	Effort              string   `json:"effort"`
	RiskIfWeDont        string   `json:"risk_if_we_dont"`
	FirstStep           string   `json:"first_step"`
	Synthesis           string   `json:"synthesis"`
	State               string   `json:"state"` // pending-review|needs-revision|accepted|declined|archived|parked
	RevisionCount       int      `json:"revision_count"`
	Note                string   `json:"note,omitempty"`
	AcceptedBeadID      string   `json:"accepted_bead_id,omitempty"`
	AcceptedPaperclipID string   `json:"accepted_paperclip_id,omitempty"`
	PaperclipError      string   `json:"paperclip_error,omitempty"`
	LastTouchedTs       int64    `json:"last_touched_ts,omitempty"`
}

// reviewStateEntry is the per-idea sidecar.
type reviewStateEntry struct {
	State                string `json:"state"`
	Note                 string `json:"note,omitempty"`
	RevisionCount        int    `json:"revision_count"`
	AcceptedBeadID       string `json:"accepted_bead_id,omitempty"`
	AcceptedPaperclipID  string `json:"accepted_paperclip_id,omitempty"`
	PaperclipError       string `json:"paperclip_error,omitempty"`
	LastTouchedTs        int64  `json:"last_touched_ts"`
}

// fileIdea is what we serialize on disk in the per-day JSONL. State lives in
// the sidecar, not here, so re-running the generator can never overwrite
// review decisions.
//
// Revision is bumped by the needs-revision regen path: when the user sends an
// idea back for revision, we shell claude with the persona prompt + the user
// note and APPEND a new line to the same JSONL with Revision+1. loadAll
// dedupes by ID and picks the highest revision, so the rendered idea always
// reflects the latest regenerated body. Revision 0 (the original generator
// output) doesn't write the field; it's the implicit default.
type fileIdea struct {
	ID           string   `json:"id"`
	Persona      string   `json:"persona"`
	Date         string   `json:"date"`
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	References   []string `json:"references"`
	Effort       string   `json:"effort"`
	RiskIfWeDont string   `json:"risk_if_we_dont"`
	FirstStep    string   `json:"first_step"`
	Synthesis    string   `json:"synthesis"`
	Revision     int      `json:"revision,omitempty"`
}

func NewIdeasManager(dataRoot, repoRoot string) *IdeasManager {
	return &IdeasManager{DataRoot: dataRoot, RepoRoot: repoRoot}
}

// RegisterRoutes wires the /api/ideas + /api/persona-prompts + /api/idea-graph
// endpoints onto an authenticated chi sub-router. The caller is responsible
// for applying the auth middleware before calling this.
func (m *IdeasManager) RegisterRoutes(r chi.Router) {
	r.Get("/ideas", m.handleListIdeas)
	r.Post("/ideas/{id}/action", m.handleAction)
	r.Get("/persona-prompts", m.handleGetPrompts)
	r.Post("/persona-prompts", m.handleSavePrompts)
	r.Post("/ideas/generate", m.handleGenerateIdeas)
	r.Get("/idea-graph", m.handleGraph)
}

// --- read path ---

// loadAll walks the data root, parses every <date>-<persona>.jsonl file, and
// merges in the review-state sidecar. Skips files starting with _ (reserved
// for state + declined log). Sort order: most recent date first, then by
// persona within a date, then by JSONL order (i.e. generator order).
func (m *IdeasManager) loadAll() ([]Idea, error) {
	entries, err := os.ReadDir(m.DataRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	state, _ := m.loadState()
	declinedIDs := m.loadDeclinedIDs()

	// First pass: read every line into a map keyed by idea ID, keeping the
	// highest-Revision entry per ID. This handles the needs-revision regen
	// path that appends new lines for the same ID with Revision+1, while
	// preserving the original generator output (Revision 0) for never-revised
	// ideas.
	type fileWithCtx struct {
		fi      fileIdea
		date    string
		persona string
	}
	byID := map[string]fileWithCtx{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// Filename convention: YYYY-MM-DD-<persona>.jsonl
		parts := strings.SplitN(strings.TrimSuffix(name, ".jsonl"), "-", 4)
		if len(parts) != 4 {
			continue
		}
		date := strings.Join(parts[:3], "-")
		persona := parts[3]

		f, err := os.Open(filepath.Join(m.DataRoot, name))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var fi fileIdea
			if err := json.Unmarshal([]byte(line), &fi); err != nil {
				continue
			}
			if fi.ID == "" {
				continue
			}
			if existing, ok := byID[fi.ID]; ok && existing.fi.Revision >= fi.Revision {
				continue
			}
			byID[fi.ID] = fileWithCtx{fi: fi, date: date, persona: persona}
		}
		_ = f.Close()
	}

	var out []Idea
	for _, fwc := range byID {
		fi := fwc.fi
		if fi.Persona == "" {
			fi.Persona = fwc.persona
		}
		if fi.Date == "" {
			fi.Date = fwc.date
		}
		idea := Idea{
			ID:           fi.ID,
			Persona:      fi.Persona,
			Date:         fi.Date,
			Title:        fi.Title,
			Body:         fi.Body,
			References:   fi.References,
			Effort:       fi.Effort,
			RiskIfWeDont: fi.RiskIfWeDont,
			FirstStep:    fi.FirstStep,
			Synthesis:    fi.Synthesis,
			State:        "pending-review",
		}
		if s, ok := state[idea.ID]; ok {
			idea.State = s.State
			idea.Note = s.Note
			idea.RevisionCount = s.RevisionCount
			idea.AcceptedBeadID = s.AcceptedBeadID
			idea.AcceptedPaperclipID = s.AcceptedPaperclipID
			idea.PaperclipError = s.PaperclipError
			idea.LastTouchedTs = s.LastTouchedTs
		} else if _, declined := declinedIDs[idea.ID]; declined {
			idea.State = "declined"
		}
		out = append(out, idea)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		return out[i].Persona < out[j].Persona
	})
	return out, nil
}

func (m *IdeasManager) loadState() (map[string]reviewStateEntry, error) {
	path := filepath.Join(m.DataRoot, "_review-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]reviewStateEntry{}, nil
		}
		return nil, err
	}
	state := map[string]reviewStateEntry{}
	if err := json.Unmarshal(data, &state); err != nil {
		return map[string]reviewStateEntry{}, err
	}
	return state, nil
}

func (m *IdeasManager) saveState(state map[string]reviewStateEntry) error {
	path := filepath.Join(m.DataRoot, "_review-state.json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.DataRoot, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// loadDeclinedIDs returns the set of idea IDs that have ever been appended
// to _declined.jsonl, so a wiped review-state can't accidentally resurrect a
// rejected idea into the inbox.
func (m *IdeasManager) loadDeclinedIDs() map[string]struct{} {
	path := filepath.Join(m.DataRoot, "_declined.jsonl")
	out := map[string]struct{}{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err == nil && rec.ID != "" {
			out[rec.ID] = struct{}{}
		}
	}
	return out
}

// --- handlers ---

func (m *IdeasManager) handleListIdeas(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ideas, err := m.loadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts := map[string]int{}
	for _, i := range ideas {
		counts[i.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ideas":  ideas,
		"counts": counts,
	})
}

type actionRequest struct {
	Action string `json:"action"`
	Note   string `json:"note"`
}

func (m *IdeasManager) handleAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req actionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ideas, err := m.loadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var target *Idea
	for i := range ideas {
		if ideas[i].ID == id {
			target = &ideas[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "idea not found", http.StatusNotFound)
		return
	}

	state, _ := m.loadState()
	entry := state[id]
	entry.LastTouchedTs = time.Now().Unix()

	switch req.Action {
	case "accept":
		beadID, beadErr := m.runBdCreate(target)
		if beadErr != nil {
			http.Error(w, fmt.Sprintf("bd create failed: %v", beadErr), http.StatusInternalServerError)
			return
		}
		entry.State = "accepted"
		entry.AcceptedBeadID = beadID
		entry.Note = req.Note
		// Bridge into Paperclip: create a matching Paperclip issue so the agent
		// chain (Planner / Engineer / Lead) can pick the work up. If Paperclip
		// is unreachable we still keep the bead — record the error in the
		// sidecar so the UI can surface it, but don't fail the action.
		paperclipID, paperclipErr := m.createPaperclipIssue(target, beadID)
		if paperclipErr != nil {
			entry.PaperclipError = paperclipErr.Error()
		} else {
			entry.AcceptedPaperclipID = paperclipID
			entry.PaperclipError = ""
		}
	case "decline":
		entry.State = "declined"
		entry.Note = req.Note
		if err := m.appendDeclined(target, req.Note); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "needs-revision":
		entry.State = "needs-revision"
		entry.Note = req.Note
		entry.RevisionCount = entry.RevisionCount + 1
		// Hand the actual regen off to Paperclip — the persona agent over
		// there runs claude in its own auth context (OAuth keychain works,
		// API key works, model fallback works). Aurex returns immediately
		// with the Paperclip issue identifier; the background poller picks
		// up the result comment when the agent posts it.
		issueID, dispErr := m.dispatchRevise(target, req.Note, entry.RevisionCount)
		if dispErr != nil {
			http.Error(w, fmt.Sprintf("paperclip dispatch failed: %v", dispErr), http.StatusBadGateway)
			return
		}
		entry.PaperclipError = ""
		// Reuse AcceptedPaperclipID as the "in-flight Paperclip ref" so the
		// UI can show "in review at UNC-NNN". A real Accept later moves this
		// to its own slot.
		target.AcceptedPaperclipID = issueID
	case "archive":
		entry.State = "archived"
		entry.Note = req.Note
	case "park":
		entry.State = "parked"
		entry.Note = req.Note
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	state[id] = entry
	if err := m.saveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target.State = entry.State
	target.Note = entry.Note
	target.RevisionCount = entry.RevisionCount
	target.AcceptedBeadID = entry.AcceptedBeadID
	target.AcceptedPaperclipID = entry.AcceptedPaperclipID
	target.PaperclipError = entry.PaperclipError
	target.LastTouchedTs = entry.LastTouchedTs
	writeJSON(w, http.StatusOK, target)
}

// ----- Paperclip persona dispatch -------------------------------------------
//
// "Send back for revision" and "+ Generate" both hand off to Paperclip rather
// than running claude locally. Reasons:
//   - Paperclip's claude_local adapter runs claude in the user's interactive
//     session via the Paperclip daemon, so OAuth keychain auth works (aurex
//     under launchd can't reach the same keychain).
//   - The CTO / CEO / etc. agents already exist with the right persona prompts
//     and Opus 4.7 model selection. No reason to duplicate that wiring here.
//   - Result arrives as an issue comment in Paperclip's Postgres. Aurex polls
//     the issue for the JSON-fenced comment and applies the result locally.
//
// Dispatches are tracked in _paperclip-dispatches.json next to the existing
// _review-state.json sidecar so a daemon restart doesn't lose pending work.

const dispatchesFileName = "_paperclip-dispatches.json"

type paperclipDispatch struct {
	Kind             string    `json:"kind"` // "revise" | "generate"
	Persona          string    `json:"persona"`
	Date             string    `json:"date"`
	IdeaID           string    `json:"idea_id,omitempty"`   // revise only
	Revision         int       `json:"revision,omitempty"`  // revise only
	PaperclipIssueID string    `json:"paperclip_issue_id"`
	IssuedAt         time.Time `json:"issued_at"`
	LastCheckedAt    time.Time `json:"last_checked_at,omitempty"`
	Failures         int       `json:"failures,omitempty"`
}

func (m *IdeasManager) dispatchesPath() string {
	return filepath.Join(m.DataRoot, dispatchesFileName)
}

func (m *IdeasManager) loadDispatches() (map[string]*paperclipDispatch, error) {
	data, err := os.ReadFile(m.dispatchesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*paperclipDispatch{}, nil
		}
		return nil, err
	}
	d := map[string]*paperclipDispatch{}
	if err := json.Unmarshal(data, &d); err != nil {
		return map[string]*paperclipDispatch{}, err
	}
	return d, nil
}

func (m *IdeasManager) saveDispatches(d map[string]*paperclipDispatch) error {
	if err := os.MkdirAll(m.DataRoot, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.dispatchesPath(), b, 0o644)
}

// personaAgentID maps a persona ID (CEO / CTO / CMO / CFO / CPO) to the
// Paperclip agent UUID that should own its dispatched work. Falls back to
// the CEO agent for any persona we don't have a dedicated agent for yet.
// Override via $AUREX_PAPERCLIP_AGENT_<PERSONA>.
func personaAgentID(persona string) string {
	p := strings.ToUpper(strings.TrimSpace(persona))
	if v := strings.TrimSpace(os.Getenv("AUREX_PAPERCLIP_AGENT_" + p)); v != "" {
		return v
	}
	defaults := map[string]string{
		"CEO": "2b335bd6-eff4-42db-a598-15de5070829f",
		"CTO": "b851177c-67db-4009-96c7-eba62a4c7ab4",
		"CMO": "afb24047-f7bc-45c7-8d8d-658ee330a534",
		"CFO": "d73289dd-7fb6-4a3d-8f62-2910b24c30eb",
		"CPO": "87ccb7d6-8003-4bb5-b75a-a9592a411d57",
	}
	if v, ok := defaults[p]; ok {
		return v
	}
	return defaults["CTO"]
}

// dispatchToPaperclip POSTs a new Paperclip issue with the given title, body,
// and assignee. Returns Paperclip's friendly identifier (e.g. "UNC-128") so
// the UI and the dispatch sidecar can reference it.
func (m *IdeasManager) dispatchToPaperclip(title, description, assigneeAgentID string) (string, error) {
	baseURL := paperclipEnv("PAPERCLIP_BASE_URL", defaultPaperclipBaseURL)
	companyID := paperclipEnv("PAPERCLIP_COMPANY_ID", defaultPaperclipCompanyID)
	token := paperclipToken()
	if token == "" {
		return "", fmt.Errorf("no Paperclip board API key — populate %s (mode 0600)", paperclipEnv("PAPERCLIP_TOKEN_FILE", defaultPaperclipTokenFile))
	}
	payload, _ := json.Marshal(map[string]any{
		"title":           title,
		"description":     description,
		"status":          "todo",
		"priority":        "medium",
		"assigneeAgentId": assigneeAgentID,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(baseURL, "/")+"/api/companies/"+companyID+"/issues",
		strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paperclip POST failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		preview := string(body)
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		return "", fmt.Errorf("paperclip HTTP %d: %s", resp.StatusCode, preview)
	}
	var parsed struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse paperclip response: %w", err)
	}
	if parsed.Identifier == "" {
		parsed.Identifier = parsed.ID
	}
	return parsed.Identifier, nil
}

// paperclipFetchIssueComments fetches the comments thread on a Paperclip
// issue identified by its friendly identifier (e.g. UNC-128).
func paperclipFetchIssueComments(issueIdentifier string) ([]string, error) {
	baseURL := paperclipEnv("PAPERCLIP_BASE_URL", defaultPaperclipBaseURL)
	token := paperclipToken()
	if token == "" {
		return nil, fmt.Errorf("no Paperclip token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/api/issues/" + strings.ToLower(issueIdentifier) + "/comments"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("paperclip GET comments HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// API returns either an array of comments or {"comments": [...]} depending
	// on Paperclip version. Accept both.
	var asArray []struct {
		Body          string `json:"body"`
		AuthorAgentID string `json:"authorAgentId"`
	}
	if err := json.Unmarshal(body, &asArray); err == nil {
		out := make([]string, 0, len(asArray))
		for _, c := range asArray {
			if c.AuthorAgentID != "" {
				out = append(out, c.Body)
			}
		}
		return out, nil
	}
	var asObject struct {
		Comments []struct {
			Body          string `json:"body"`
			AuthorAgentID string `json:"authorAgentId"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(body, &asObject); err == nil {
		out := make([]string, 0, len(asObject.Comments))
		for _, c := range asObject.Comments {
			if c.AuthorAgentID != "" {
				out = append(out, c.Body)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("paperclip comments response had unexpected shape")
}

// extractJSONBlock pulls a fenced ```json block out of a markdown comment.
// Falls back to scanning for the first top-level JSON object if no fence is
// present. Returns the raw JSON string (no fences) or an error.
func extractJSONBlock(comment string) (string, error) {
	c := strings.TrimSpace(comment)
	if idx := strings.Index(c, "```json"); idx != -1 {
		rest := c[idx+len("```json"):]
		if end := strings.Index(rest, "```"); end != -1 {
			return strings.TrimSpace(rest[:end]), nil
		}
	}
	if idx := strings.Index(c, "```"); idx != -1 {
		rest := c[idx+len("```"):]
		if end := strings.Index(rest, "```"); end != -1 {
			candidate := strings.TrimSpace(rest[:end])
			if strings.HasPrefix(candidate, "{") || strings.HasPrefix(candidate, "[") {
				return candidate, nil
			}
		}
	}
	// Final fallback — find the first { ... } that parses as JSON.
	if start := strings.Index(c, "{"); start != -1 {
		// Naive brace-balanced extraction.
		depth := 0
		for i := start; i < len(c); i++ {
			switch c[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return c[start : i+1], nil
				}
			}
		}
	}
	return "", fmt.Errorf("no JSON block found in comment")
}

// pollPaperclipDispatches scans every open dispatch and, for each, fetches
// Paperclip's comments and tries to apply a parseable JSON result. Drops
// applied entries from the sidecar. Designed to run on a 10s ticker in a
// background goroutine started by main.
func (m *IdeasManager) pollPaperclipDispatches() {
	m.mu.Lock()
	defer m.mu.Unlock()
	dispatches, err := m.loadDispatches()
	if err != nil || len(dispatches) == 0 {
		return
	}
	changed := false
	for key, d := range dispatches {
		comments, err := paperclipFetchIssueComments(d.PaperclipIssueID)
		d.LastCheckedAt = time.Now().UTC()
		if err != nil {
			d.Failures++
			continue
		}
		applied := false
		for _, c := range comments {
			jsonStr, err := extractJSONBlock(c)
			if err != nil {
				continue
			}
			if d.Kind == "revise" {
				if err := m.applyRevisionResultLocked(d, jsonStr); err == nil {
					applied = true
					break
				}
			} else if d.Kind == "generate" {
				if err := m.applyGenerationResultLocked(d, jsonStr); err == nil {
					applied = true
					break
				}
			}
		}
		if applied {
			delete(dispatches, key)
			changed = true
		}
	}
	if changed {
		_ = m.saveDispatches(dispatches)
	} else {
		_ = m.saveDispatches(dispatches) // persist LastCheckedAt + Failures
	}
}

// StartPaperclipPoller runs pollPaperclipDispatches every interval until ctx
// is cancelled. Call from main once at startup.
func (m *IdeasManager) StartPaperclipPoller(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.pollPaperclipDispatches()
			}
		}
	}()
}

// applyRevisionResultLocked parses the JSON returned by the persona agent and
// appends a new JSONL line for the idea with the bumped revision. Caller
// must hold m.mu.
func (m *IdeasManager) applyRevisionResultLocked(d *paperclipDispatch, jsonStr string) error {
	var fi fileIdea
	if err := json.Unmarshal([]byte(jsonStr), &fi); err != nil {
		return fmt.Errorf("parse revision JSON: %w", err)
	}
	if fi.ID != "" && fi.ID != d.IdeaID {
		// Force original ID — agent must not invent a new one.
		fi.ID = d.IdeaID
	} else if fi.ID == "" {
		fi.ID = d.IdeaID
	}
	fi.Persona = strings.ToUpper(strings.TrimSpace(fi.Persona))
	if fi.Persona == "" {
		fi.Persona = d.Persona
	}
	if fi.Date == "" {
		fi.Date = d.Date
	}
	fi.Revision = d.Revision
	if fi.Body == "" {
		return fmt.Errorf("regenerated idea has empty body")
	}
	name := fmt.Sprintf("%s-%s.jsonl", fi.Date, strings.ToLower(fi.Persona))
	path := filepath.Join(m.DataRoot, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := json.Marshal(fi)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return err
	}
	return nil
}

// applyGenerationResultLocked parses the JSON returned by the persona agent
// (expected shape: {persona, date, ideas: [fileIdea...]}) and appends each
// idea to today's JSONL after renumbering IDs to avoid collisions. Caller
// must hold m.mu.
func (m *IdeasManager) applyGenerationResultLocked(d *paperclipDispatch, jsonStr string) error {
	var parsed struct {
		Persona string     `json:"persona"`
		Date    string     `json:"date"`
		Ideas   []fileIdea `json:"ideas"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return fmt.Errorf("parse generation JSON: %w", err)
	}
	if len(parsed.Ideas) == 0 {
		return fmt.Errorf("generation returned zero ideas")
	}
	persona := strings.ToUpper(strings.TrimSpace(parsed.Persona))
	if persona == "" {
		persona = d.Persona
	}
	date := parsed.Date
	if date == "" {
		date = d.Date
	}
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	maxCounter := m.maxIdeaCounter(persona, date)
	for i := range parsed.Ideas {
		fi := parsed.Ideas[i]
		if fi.Persona == "" {
			fi.Persona = persona
		}
		if fi.Date == "" {
			fi.Date = date
		}
		maxCounter++
		fi.ID = fmt.Sprintf("%s-%s-%02d", strings.ToLower(persona), fi.Date, maxCounter)
		if err := m.appendNewIdea(fi); err != nil {
			return err
		}
	}
	return nil
}

// dispatchRevise hands the regen off to Paperclip: posts a Revise issue
// assigned to the matching persona agent, records the dispatch in the
// sidecar so the background poller can apply the result later, and returns
// the Paperclip issue identifier (e.g. "UNC-128"). No LLM call happens in
// the aurex process — claude runs inside Paperclip's claude_local adapter
// where keychain auth works.
func (m *IdeasManager) dispatchRevise(orig *Idea, note string, newRevision int) (string, error) {
	promptsPath := m.personaPromptsPath()
	data, err := os.ReadFile(promptsPath)
	if err != nil {
		return "", fmt.Errorf("read persona prompts: %w", err)
	}
	sections := parsePromptSections(string(data))
	personaKey := strings.ToLower(strings.TrimSpace(orig.Persona))
	personaPrompt := sections[personaKey]
	if personaPrompt == "" {
		return "", fmt.Errorf("no persona section for %q", orig.Persona)
	}
	origJSON, _ := json.MarshalIndent(map[string]any{
		"id":              orig.ID,
		"title":           orig.Title,
		"body":            orig.Body,
		"references":      orig.References,
		"effort":          orig.Effort,
		"risk_if_we_dont": orig.RiskIfWeDont,
		"first_step":      orig.FirstStep,
		"synthesis":       orig.Synthesis,
	}, "", "  ")
	title := fmt.Sprintf("Revise idea: %s", strings.TrimSpace(orig.Title))
	if len(title) > 200 {
		title = title[:197] + "..."
	}
	desc := fmt.Sprintf(`You are acting as the %s persona for the aurex Ideas inbox. The user is sending this idea back for revision. Read the revision note and produce a single updated idea in the same JSON shape, then **post the result as a comment on this issue inside a fenced ` + "```json" + ` block**. Do not edit the issue description; do not open new beads. Just comment with the JSON.

## Persona context
%s

## Original idea
` + "```json" + `
%s
` + "```" + `

## Revision note (this is what the reviewer wants changed)
%s

## Output contract
- Reply with a comment on this issue.
- The comment must contain a fenced ` + "```json" + ` block holding a single object with these fields: id, title, body, references[], effort, risk_if_we_dont, first_step, synthesis.
- The "id" field MUST be exactly: %s
- The "body" must materially address the revision note (expand, focus, add numbers — whatever the note asks for).
- Body >= 300 chars. No em-dashes. No invented facts.
- Once you post the comment, mark the issue as done.
`,
		strings.ToUpper(orig.Persona),
		strings.TrimSpace(personaPrompt),
		string(origJSON),
		strings.TrimSpace(note),
		orig.ID,
	)
	agentID := personaAgentID(orig.Persona)
	issueID, err := m.dispatchToPaperclip(title, desc, agentID)
	if err != nil {
		return "", err
	}
	// Record the dispatch so the poller knows to apply the result.
	dispatches, _ := m.loadDispatches()
	dispatches[orig.ID] = &paperclipDispatch{
		Kind:             "revise",
		Persona:          strings.ToUpper(orig.Persona),
		Date:             orig.Date,
		IdeaID:           orig.ID,
		Revision:         newRevision,
		PaperclipIssueID: issueID,
		IssuedAt:         time.Now().UTC(),
	}
	if err := m.saveDispatches(dispatches); err != nil {
		return "", fmt.Errorf("save dispatch sidecar: %w", err)
	}
	return issueID, nil
}

// dispatchGenerate hands the "Generate N new ideas" request off to the
// persona agent in Paperclip. Returns the Paperclip issue identifier.
func (m *IdeasManager) dispatchGenerate(persona string, count int) (string, error) {
	promptsPath := m.personaPromptsPath()
	data, err := os.ReadFile(promptsPath)
	if err != nil {
		return "", fmt.Errorf("read persona prompts: %w", err)
	}
	sections := parsePromptSections(string(data))
	personaKey := strings.ToLower(strings.TrimSpace(persona))
	personaPrompt := sections[personaKey]
	if personaPrompt == "" {
		return "", fmt.Errorf("no persona section for %q", persona)
	}
	today := time.Now().UTC().Format("2006-01-02")
	title := fmt.Sprintf("Generate %d new %s ideas (%s)", count, strings.ToUpper(persona), today)
	desc := fmt.Sprintf(`You are acting as the %s persona for the aurex Ideas inbox. Generate %d new ideas for today (%s), grounded in current repo state. Read recent commits, bd ready, and the recently declined ideas log before generating. Do not repropose what was declined.

## Persona context
%s

## Output contract
- Reply with a comment on this issue.
- The comment must contain a fenced ` + "```json" + ` block holding ONE object: {"persona": "%s", "date": "%s", "ideas": [ { ... }, ... ]}.
- Each idea: {id?: string (optional, the aurex side will renumber), title, body (>= 300 chars), references[], effort (S|M|L|XL), risk_if_we_dont, first_step, synthesis (optional)}.
- No em-dashes. No invented facts. Each idea must have a concrete first_step.
- Once you post the comment, mark the issue as done.
`,
		strings.ToUpper(persona),
		count,
		today,
		strings.TrimSpace(personaPrompt),
		strings.ToUpper(persona),
		today,
	)
	agentID := personaAgentID(persona)
	issueID, err := m.dispatchToPaperclip(title, desc, agentID)
	if err != nil {
		return "", err
	}
	dispatches, _ := m.loadDispatches()
	dispatches["generate:"+strings.ToUpper(persona)+":"+today+":"+issueID] = &paperclipDispatch{
		Kind:             "generate",
		Persona:          strings.ToUpper(persona),
		Date:             today,
		PaperclipIssueID: issueID,
		IssuedAt:         time.Now().UTC(),
	}
	if err := m.saveDispatches(dispatches); err != nil {
		return "", fmt.Errorf("save dispatch sidecar: %w", err)
	}
	return issueID, nil
}

// runBdCreate shells out to `bd create` with the idea body. We run from the
// k3s-pi repo root because that's the repo bd is bound to. Captures stdout
// so we can pluck the new bead ID out of "Created bd-XXXX:" / similar.
func (m *IdeasManager) runBdCreate(idea *Idea) (string, error) {
	body := strings.TrimSpace(idea.Body)
	if idea.FirstStep != "" {
		body += "\n\n**First step:** " + strings.TrimSpace(idea.FirstStep)
	}
	if idea.RiskIfWeDont != "" {
		body += "\n\n**Risk if we don't:** " + strings.TrimSpace(idea.RiskIfWeDont)
	}
	if len(idea.References) > 0 {
		body += "\n\n**References:** " + strings.Join(idea.References, ", ")
	}
	body += fmt.Sprintf("\n\n_Origin: idea %s, persona %s, %s_", idea.ID, idea.Persona, idea.Date)

	args := []string{
		"create", idea.Title,
		"--type", "feature",
		"--priority", "2",
		"-d", body,
		"--silent",
	}
	cmd := exec.Command("bd", args...)
	cmd.Dir = m.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bd create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// --silent prints just the ID, e.g. "k3s-abcd".
	id := strings.TrimSpace(string(out))
	// Defensive: if --silent ever changes, look for a bd ID anywhere in output.
	re := regexp.MustCompile(`\b[a-z0-9-]+-[a-z0-9]+\b`)
	if id == "" || !re.MatchString(id) {
		if m := re.FindString(string(out)); m != "" {
			return m, nil
		}
	}
	return id, nil
}

// Paperclip company + agent identifiers. Hard-coded to the active "Unchained"
// company because aurex is single-tenant. Override via env if you ever pair
// aurex with a different Paperclip instance.
const (
	defaultPaperclipBaseURL    = "http://localhost:3100"
	defaultPaperclipCompanyID  = "9a142f30-af42-4941-9cc3-aa713acf9dbe"
	defaultPaperclipTokenFile  = "/tmp/pcp-token"
	defaultPaperclipBEEngineer = "84514739-2d39-4265-a5ff-d52c6a65e370"
	defaultPaperclipUIEngineer = "b237f9c4-8241-46f8-a185-de2668d35b41"
)

func paperclipEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func paperclipToken() string {
	path := paperclipEnv("PAPERCLIP_TOKEN_FILE", defaultPaperclipTokenFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// routeIdeaToAgent picks which Paperclip agent should own a new issue based on
// the idea's references — anything mentioning unchained-ui goes to the UI
// Engineer, everything else falls through to the Backend Engineer (who is
// fullstack and can route the work further if needed).
func routeIdeaToAgent(idea *Idea) string {
	beID := paperclipEnv("PAPERCLIP_BE_ENGINEER_ID", defaultPaperclipBEEngineer)
	uiID := paperclipEnv("PAPERCLIP_UI_ENGINEER_ID", defaultPaperclipUIEngineer)
	for _, ref := range idea.References {
		r := strings.ToLower(ref)
		if strings.Contains(r, "unchained-ui") || strings.Contains(r, "iosapp") || strings.Contains(r, "unchained-legal") {
			return uiID
		}
	}
	return beID
}

// createPaperclipIssue POSTs a new Paperclip issue mirroring the accepted
// idea, with the bead ID embedded in the title so the bd <-> Paperclip link is
// visible at a glance. The board API key is read from PAPERCLIP_TOKEN_FILE
// (defaults to /tmp/pcp-token, the file scripts/native-build-with-pause.sh
// also reads). Returns the Paperclip-side identifier (e.g. "UNC-104").
func (m *IdeasManager) createPaperclipIssue(idea *Idea, beadID string) (string, error) {
	baseURL := paperclipEnv("PAPERCLIP_BASE_URL", defaultPaperclipBaseURL)
	companyID := paperclipEnv("PAPERCLIP_COMPANY_ID", defaultPaperclipCompanyID)
	token := paperclipToken()
	if token == "" {
		return "", fmt.Errorf("no board API key — set PAPERCLIP_TOKEN_FILE or write /tmp/pcp-token")
	}

	desc := strings.TrimSpace(idea.Body)
	if idea.FirstStep != "" {
		desc += "\n\n**First step:** " + strings.TrimSpace(idea.FirstStep)
	}
	if idea.RiskIfWeDont != "" {
		desc += "\n\n**Risk if we don't:** " + strings.TrimSpace(idea.RiskIfWeDont)
	}
	if len(idea.References) > 0 {
		desc += "\n\n**References:** " + strings.Join(idea.References, ", ")
	}
	desc += fmt.Sprintf("\n\n_Origin: aurex Ideas inbox — persona %s, idea %s, bead %s_", idea.Persona, idea.ID, beadID)

	title := fmt.Sprintf("[%s] %s", beadID, strings.TrimSpace(idea.Title))
	if len(title) > 200 {
		title = title[:197] + "..."
	}
	assignee := routeIdeaToAgent(idea)

	payload := map[string]any{
		"title":            title,
		"description":      desc,
		"status":           "todo",
		"priority":         "medium",
		"assigneeAgentId":  assignee,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(baseURL, "/") + "/api/companies/" + companyID + "/issues"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paperclip POST failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		preview := string(respBody)
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		return "", fmt.Errorf("paperclip returned HTTP %d: %s", resp.StatusCode, preview)
	}

	var parsed struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse paperclip response: %w", err)
	}
	if parsed.Identifier == "" {
		parsed.Identifier = parsed.ID // fall back to the UUID if no friendly ident
	}
	return parsed.Identifier, nil
}

func (m *IdeasManager) appendDeclined(idea *Idea, note string) error {
	path := filepath.Join(m.DataRoot, "_declined.jsonl")
	if err := os.MkdirAll(m.DataRoot, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	rec := map[string]any{
		"id":      idea.ID,
		"persona": idea.Persona,
		"date":    idea.Date,
		"title":   idea.Title,
		"note":    note,
		"ts":      time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// --- generate more ideas (on-demand) ---

type generateIdeasRequest struct {
	Persona string `json:"persona"`           // CEO | CFO | CMO | CTO | CPO
	Count   int    `json:"count,omitempty"`   // hint: 3-5; default 3
}

// handleGenerateIdeas dispatches a "Generate N new <persona> ideas" issue to
// the persona's Paperclip agent and returns 202 with the Paperclip issue
// identifier. The background poller applies the result to today's JSONL
// when the agent posts its JSON comment.
func (m *IdeasManager) handleGenerateIdeas(w http.ResponseWriter, r *http.Request) {
	var req generateIdeasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	persona := strings.ToUpper(strings.TrimSpace(req.Persona))
	switch persona {
	case "CEO", "CFO", "CMO", "CTO", "CPO":
	default:
		http.Error(w, "persona must be one of CEO|CFO|CMO|CTO|CPO", http.StatusBadRequest)
		return
	}
	count := req.Count
	if count < 1 {
		count = 3
	}
	if count > 5 {
		count = 5
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	issueID, err := m.dispatchGenerate(persona, count)
	if err != nil {
		http.Error(w, fmt.Sprintf("paperclip dispatch failed: %v", err), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"persona":              persona,
		"paperclip_issue_id":   issueID,
		"status":               "dispatched",
		"message":              fmt.Sprintf("Generation queued at Paperclip %s — refresh the inbox in 30-60s to see the new ideas", issueID),
	})
}

func (m *IdeasManager) appendNewIdea(fi fileIdea) error {
	name := fmt.Sprintf("%s-%s.jsonl", fi.Date, strings.ToLower(fi.Persona))
	path := filepath.Join(m.DataRoot, name)
	if err := os.MkdirAll(m.DataRoot, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := json.Marshal(fi)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return err
	}
	return nil
}

// maxIdeaCounter scans today's <date>-<persona>.jsonl (case-insensitive) and
// returns the highest <NN> counter seen in idea IDs of the form
// <persona>-<date>-<NN>. Returns 0 if the file doesn't exist or no IDs match.
func (m *IdeasManager) maxIdeaCounter(persona, date string) int {
	name := fmt.Sprintf("%s-%s.jsonl", date, strings.ToLower(persona))
	path := filepath.Join(m.DataRoot, name)
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	prefix := strings.ToLower(persona) + "-" + date + "-"
	max := 0
	for scanner.Scan() {
		var fi fileIdea
		if err := json.Unmarshal(scanner.Bytes(), &fi); err != nil {
			continue
		}
		if !strings.HasPrefix(fi.ID, prefix) {
			continue
		}
		var n int
		_, err := fmt.Sscanf(strings.TrimPrefix(fi.ID, prefix), "%d", &n)
		if err == nil && n > max {
			max = n
		}
	}
	return max
}

// buildSharedContext returns a bounded string of recent commits, bd ready
// output, and a list of recent declined idea titles. Conservative on size so
// we don't blow up the claude context (Opus 4.7 handles it but no point
// burning tokens on stale repo state).
func (m *IdeasManager) buildSharedContext() string {
	var b strings.Builder
	// Recent commits
	if out, err := exec.Command("git", "log", "--oneline", "-15").CombinedOutput(); err == nil {
		fmt.Fprintf(&b, "## Recent commits (newest first)\n```\n%s```\n\n", out)
	}
	// bd ready (limit 20)
	bdReady := exec.Command("bd", "ready", "--limit", "20")
	bdReady.Dir = m.RepoRoot
	if out, err := bdReady.CombinedOutput(); err == nil {
		fmt.Fprintf(&b, "## bd ready (unblocked work)\n```\n%s```\n\n", out)
	}
	// Recent declined idea titles (last 20 lines of _declined.jsonl)
	if data, err := os.ReadFile(filepath.Join(m.DataRoot, "_declined.jsonl")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if n := len(lines); n > 20 {
			lines = lines[n-20:]
		}
		b.WriteString("## Recently declined ideas (do not repropose without explicit argument)\n")
		for _, l := range lines {
			var rec struct {
				Title string `json:"title"`
				Note  string `json:"note"`
			}
			if json.Unmarshal([]byte(l), &rec) == nil && rec.Title != "" {
				fmt.Fprintf(&b, "- %s — reason: %s\n", rec.Title, rec.Note)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// --- persona prompts ---

func (m *IdeasManager) personaPromptsPath() string {
	return filepath.Join(m.DataRoot, "PERSONA-PROMPTS.md")
}

func (m *IdeasManager) handleGetPrompts(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(m.personaPromptsPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sections := parsePromptSections(string(data))
	writeJSON(w, http.StatusOK, sections)
}

type savePromptsRequest struct {
	CEO              string `json:"ceo"`
	CFO              string `json:"cfo"`
	CMO              string `json:"cmo"`
	CTO              string `json:"cto"`
	CPO              string `json:"cpo"`
	SharedGuardrails string `json:"sharedGuardrails"`
	Preamble         string `json:"preamble"`
}

func (m *IdeasManager) handleSavePrompts(w http.ResponseWriter, r *http.Request) {
	var req savePromptsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	merged := renderPromptSections(req)
	if err := os.WriteFile(m.personaPromptsPath(), []byte(merged), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.gitCommitPush("vetted: k3s-gdxk persona prompts"); err != nil {
		// Don't fail the API — the file is saved. Just report the git issue
		// to the client so the user can resolve it manually.
		writeJSON(w, http.StatusOK, map[string]any{
			"saved":  true,
			"pushed": false,
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "pushed": true})
}

func (m *IdeasManager) gitCommitPush(message string) error {
	if m.RepoRoot == "" {
		return fmt.Errorf("no repo root configured")
	}
	rel, err := filepath.Rel(m.RepoRoot, m.personaPromptsPath())
	if err != nil {
		return err
	}
	// Stage only the prompts file. We never want this endpoint to sweep
	// up any other unrelated change in the working tree.
	add := exec.Command("git", "add", rel)
	add.Dir = m.RepoRoot
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Check whether there's actually a staged diff. If the user clicked
	// save with no edits, exit successfully without an empty commit.
	diff := exec.Command("git", "diff", "--cached", "--quiet", rel)
	diff.Dir = m.RepoRoot
	if diff.Run() == nil {
		return nil
	}
	commit := exec.Command("git", "commit", "-m", message)
	commit.Dir = m.RepoRoot
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	push := exec.Command("git", "push")
	push.Dir = m.RepoRoot
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parsePromptSections is the markdown -> struct half of the round trip. It
// splits on `## CEO`, `## CFO`, `## CMO`, `## CTO`, `## CPO` headers. Anything
// before the first persona header is the preamble. Anything after the last
// persona section (typically the "Backpressure rule" + "Cross-persona dedup"
// + "Tuning instructions") is the shared guardrails block.
func parsePromptSections(md string) map[string]string {
	out := map[string]string{
		"preamble":         "",
		"ceo":              "",
		"cfo":              "",
		"cmo":              "",
		"cto":              "",
		"cpo":              "",
		"sharedGuardrails": "",
	}
	type span struct {
		key   string
		start int
		end   int
	}
	headers := []struct {
		re  *regexp.Regexp
		key string
	}{
		{regexp.MustCompile(`(?m)^## CEO[^\n]*$`), "ceo"},
		{regexp.MustCompile(`(?m)^## CFO[^\n]*$`), "cfo"},
		{regexp.MustCompile(`(?m)^## CMO[^\n]*$`), "cmo"},
		{regexp.MustCompile(`(?m)^## CTO[^\n]*$`), "cto"},
		{regexp.MustCompile(`(?m)^## CPO[^\n]*$`), "cpo"},
	}
	var spans []span
	for _, h := range headers {
		loc := h.re.FindStringIndex(md)
		if loc == nil {
			continue
		}
		spans = append(spans, span{key: h.key, start: loc[0]})
	}
	if len(spans) == 0 {
		out["preamble"] = md
		return out
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	// Preamble is everything before the first persona header.
	out["preamble"] = strings.TrimSpace(md[:spans[0].start])

	// Each persona span ends at the next section start (next persona, or the
	// shared-guardrails delimiter `---\n## ` after the last persona).
	for i := range spans {
		end := len(md)
		if i+1 < len(spans) {
			end = spans[i+1].start
		}
		spans[i].end = end
	}

	for _, s := range spans {
		body := md[s.start:s.end]
		// Drop the `## CEO — ...` header line itself.
		nl := strings.Index(body, "\n")
		if nl >= 0 {
			body = body[nl+1:]
		}
		body = strings.TrimSpace(body)
		// Drop a trailing `---` divider if the next section started with one.
		body = strings.TrimSuffix(body, "---")
		body = strings.TrimSpace(body)
		out[s.key] = body
	}

	// CPO is the final persona header, but the file's "Backpressure rule" +
	// "Cross-persona deduplication" + "Tuning instructions" appear AFTER it
	// under their own `## ` headers. Move the CPO span end to the first
	// non-persona `## ` header encountered after its start, and assign the
	// remainder to sharedGuardrails. Without this, those trailing sections
	// get glued onto CPO and the shared block ends up empty.
	last := spans[len(spans)-1]
	nextHeader := regexp.MustCompile(`(?m)^## [^\n]+$`)
	body := md[last.start+1:] // skip the first char so we don't re-match the CPO header itself
	if loc := nextHeader.FindStringIndex(body); loc != nil {
		boundary := last.start + 1 + loc[0]
		// Re-extract CPO with the corrected end.
		cpoBody := md[last.start:boundary]
		nl := strings.Index(cpoBody, "\n")
		if nl >= 0 {
			cpoBody = cpoBody[nl+1:]
		}
		cpoBody = strings.TrimSpace(cpoBody)
		cpoBody = strings.TrimSuffix(cpoBody, "---")
		cpoBody = strings.TrimSpace(cpoBody)
		out["cpo"] = cpoBody
		out["sharedGuardrails"] = strings.TrimSpace(md[boundary:])
	} else {
		out["sharedGuardrails"] = strings.TrimSpace(md[last.end:])
	}

	return out
}

// renderPromptSections is the inverse of parsePromptSections. Order matters
// because git diffs are nicer when sections stay put.
func renderPromptSections(req savePromptsRequest) string {
	var b strings.Builder
	if strings.TrimSpace(req.Preamble) != "" {
		b.WriteString(strings.TrimSpace(req.Preamble))
		b.WriteString("\n\n---\n\n")
	}
	personas := []struct {
		header, body string
	}{
		{"## CEO — Chief Executive Officer", req.CEO},
		{"## CFO — Chief Financial Officer", req.CFO},
		{"## CMO — Chief Marketing Officer (Marketing Head)", req.CMO},
		{"## CTO — Chief Technology Officer", req.CTO},
		{"## CPO — Chief Product Officer", req.CPO},
	}
	for i, p := range personas {
		b.WriteString(p.header)
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(p.body))
		b.WriteString("\n\n")
		if i < len(personas)-1 {
			b.WriteString("---\n\n")
		}
	}
	if strings.TrimSpace(req.SharedGuardrails) != "" {
		b.WriteString("---\n\n")
		b.WriteString(strings.TrimSpace(req.SharedGuardrails))
		b.WriteString("\n")
	}
	return b.String()
}

// --- graph ---

// handleGraph returns nodes + edges for the connections view. v1 builds edges
// from any two ideas that share at least one entry in their references list,
// case-insensitive. v2 should layer in semantic similarity.
func (m *IdeasManager) handleGraph(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ideas, err := m.loadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type node struct {
		ID            string `json:"id"`
		Persona       string `json:"persona"`
		Title         string `json:"title"`
		State         string `json:"state"`
		RevisionCount int    `json:"revision_count"`
	}
	type edge struct {
		Source string   `json:"source"`
		Target string   `json:"target"`
		Shared []string `json:"shared"`
	}
	nodes := make([]node, 0, len(ideas))
	for _, i := range ideas {
		nodes = append(nodes, node{
			ID: i.ID, Persona: i.Persona, Title: i.Title, State: i.State, RevisionCount: i.RevisionCount,
		})
	}
	var edges []edge
	for i := 0; i < len(ideas); i++ {
		for j := i + 1; j < len(ideas); j++ {
			shared := intersectRefs(ideas[i].References, ideas[j].References)
			if len(shared) > 0 {
				edges = append(edges, edge{Source: ideas[i].ID, Target: ideas[j].ID, Shared: shared})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}

func intersectRefs(a, b []string) []string {
	set := map[string]struct{}{}
	for _, x := range a {
		set[strings.ToLower(strings.TrimSpace(x))] = struct{}{}
	}
	var out []string
	for _, y := range b {
		k := strings.ToLower(strings.TrimSpace(y))
		if k == "" {
			continue
		}
		if _, ok := set[k]; ok {
			out = append(out, y)
		}
	}
	return out
}

// --- util: read body once for debugging if needed ---

var _ = io.Discard // keep io import if we trim other uses later
