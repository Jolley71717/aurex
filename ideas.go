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
	ID             string   `json:"id"`
	Persona        string   `json:"persona"`
	Date           string   `json:"date"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	References     []string `json:"references"`
	Effort         string   `json:"effort"`
	RiskIfWeDont   string   `json:"risk_if_we_dont"`
	FirstStep      string   `json:"first_step"`
	Synthesis      string   `json:"synthesis"`
	State          string   `json:"state"`           // pending-review|needs-revision|accepted|declined|archived|parked
	RevisionCount  int      `json:"revision_count"`
	Note           string   `json:"note,omitempty"`
	AcceptedBeadID string   `json:"accepted_bead_id,omitempty"`
	LastTouchedTs  int64    `json:"last_touched_ts,omitempty"`
}

// reviewStateEntry is the per-idea sidecar.
type reviewStateEntry struct {
	State          string `json:"state"`
	Note           string `json:"note,omitempty"`
	RevisionCount  int    `json:"revision_count"`
	AcceptedBeadID string `json:"accepted_bead_id,omitempty"`
	LastTouchedTs  int64  `json:"last_touched_ts"`
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
		// Regenerate the idea body via the same persona that produced it,
		// running through `claude --print` with the persona section from
		// PERSONA-PROMPTS.md. On success we append a new fileIdea to the
		// JSONL with Revision=entry.RevisionCount; loadAll picks the highest
		// revision per ID. On failure we still record the note + bump the
		// counter, so the user sees what happened and can retry.
		regenerated, regenErr := m.regenerateIdea(target, req.Note, entry.RevisionCount)
		if regenErr != nil {
			http.Error(w, fmt.Sprintf("regenerate failed: %v", regenErr), http.StatusBadGateway)
			return
		}
		if appendErr := m.appendIdeaRevision(target, regenerated); appendErr != nil {
			http.Error(w, fmt.Sprintf("append revision failed: %v", appendErr), http.StatusInternalServerError)
			return
		}
		// Mirror the regenerated body onto the target so the response shape
		// already contains the new content (the client doesn't need a second
		// round-trip to /api/ideas).
		target.Title = regenerated.Title
		target.Body = regenerated.Body
		target.References = regenerated.References
		target.Effort = regenerated.Effort
		target.RiskIfWeDont = regenerated.RiskIfWeDont
		target.FirstStep = regenerated.FirstStep
		target.Synthesis = regenerated.Synthesis
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
	target.LastTouchedTs = entry.LastTouchedTs
	writeJSON(w, http.StatusOK, target)
}

// regenerateIdea shells out to `claude --print` with the persona's section
// from PERSONA-PROMPTS.md as the system prompt and the original idea + the
// user's revision note as input. Returns the parsed fileIdea ready to append
// to the day's JSONL.
//
// The claude binary path is configurable via $AUREX_CLAUDE_BIN; defaults to
// "claude" on PATH. Model defaults to claude-opus-4-7 — same Opus 4.7 the
// project's senior agents use — but is overridable via $AUREX_REGEN_MODEL.
// 5-minute hard timeout so a stuck regen can't hang the HTTP handler.
func (m *IdeasManager) regenerateIdea(orig *Idea, note string, newRevision int) (fileIdea, error) {
	var zero fileIdea
	promptsPath := m.personaPromptsPath()
	data, err := os.ReadFile(promptsPath)
	if err != nil {
		return zero, fmt.Errorf("read persona prompts: %w", err)
	}
	sections := parsePromptSections(string(data))
	personaKey := strings.ToLower(strings.TrimSpace(orig.Persona))
	personaPrompt := sections[personaKey]
	if personaPrompt == "" {
		return zero, fmt.Errorf("no persona section for %q in %s", orig.Persona, promptsPath)
	}

	// Build the system + user prompt. System is the persona prompt + shared
	// guardrails so the regen stays in voice. User is the original idea (as
	// the JSON shape the persona was trained against) plus the revision ask.
	systemPrompt := strings.TrimSpace(sections["preamble"]) + "\n\n" +
		strings.TrimSpace(personaPrompt) + "\n\n" +
		strings.TrimSpace(sections["sharedGuardrails"])

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

	userPrompt := fmt.Sprintf(`You previously produced this idea. The reviewer is sending it back for revision. Read the revision note carefully and produce a single updated idea in the EXACT same JSON shape. Keep the "id" field IDENTICAL. Update the title/body/references/effort/risk_if_we_dont/first_step/synthesis to address the note.

Output rules:
- Output ONE JSON object only. No markdown fences, no prose before or after.
- The "body" field must materially address the reviewer's note. If they asked for more detail, expand. If they asked you to focus, focus.
- Keep within the shared guardrails (no em-dashes, no hallucinations, body >= 300 chars, concrete first_step).

ORIGINAL IDEA:
%s

REVIEWER NOTE (this is the revision ask):
%s
`, string(origJSON), strings.TrimSpace(note))

	claudeBin := os.Getenv("AUREX_CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin = "claude"
	}
	model := os.Getenv("AUREX_REGEN_MODEL")
	if model == "" {
		model = "claude-opus-4-7"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBin,
		"--print",
		"--model", model,
		"--append-system-prompt", systemPrompt,
		userPrompt,
	)
	cmd.Dir = m.RepoRoot
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		return zero, fmt.Errorf("claude regen timed out after 5 min: %w", ctx.Err())
	}
	if runErr != nil {
		stderr := ""
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return zero, fmt.Errorf("claude exited non-zero: %w (stderr: %s)", runErr, stderr)
	}

	// Strip ```json fences if the model wraps despite instructions.
	resp := strings.TrimSpace(string(out))
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var parsed fileIdea
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		preview := resp
		if len(preview) > 400 {
			preview = preview[:400] + "..."
		}
		return zero, fmt.Errorf("parse claude response as fileIdea: %w (response head: %s)", err, preview)
	}
	if parsed.ID != orig.ID {
		// Force the original ID — the regen must not invent a new one or our
		// loadAll dedup-by-ID falls apart.
		parsed.ID = orig.ID
	}
	parsed.Persona = orig.Persona
	parsed.Date = orig.Date
	parsed.Revision = newRevision
	return parsed, nil
}

// appendIdeaRevision writes a new JSONL line to the same per-day file the
// original idea lives in. loadAll dedupes by ID so the most-recent revision
// becomes the canonical one.
func (m *IdeasManager) appendIdeaRevision(orig *Idea, fi fileIdea) error {
	name := fmt.Sprintf("%s-%s.jsonl", orig.Date, strings.ToLower(orig.Persona))
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
