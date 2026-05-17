import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import AgentsPanel from './AgentsPanel';

// PERSONA_TONE drives the badge color + accent ring per persona, matching
// the brief in the prompt. We use Tailwind utility classes so the build
// doesn't need a custom plugin pass — every color shows up in
// purgecss-safe strings literally here.
const PERSONA_TONE = {
  CEO: {
    label: 'CEO',
    badge: 'bg-indigo-500/20 text-indigo-300 border-indigo-400/40',
    dot: 'bg-indigo-400',
    hex: '#818cf8',
  },
  CFO: {
    label: 'CFO',
    badge: 'bg-emerald-500/20 text-emerald-300 border-emerald-400/40',
    dot: 'bg-emerald-400',
    hex: '#34d399',
  },
  CMO: {
    label: 'CMO',
    badge: 'bg-orange-500/20 text-orange-300 border-orange-400/40',
    dot: 'bg-orange-400',
    hex: '#fb923c',
  },
  CTO: {
    label: 'CTO',
    badge: 'bg-sky-500/20 text-sky-300 border-sky-400/40',
    dot: 'bg-sky-400',
    hex: '#38bdf8',
  },
  CPO: {
    label: 'CPO',
    badge: 'bg-purple-500/20 text-purple-300 border-purple-400/40',
    dot: 'bg-purple-400',
    hex: '#c084fc',
  },
};

const PERSONA_DEFAULT = {
  badge: 'bg-zinc-700 text-zinc-300 border-zinc-600',
  dot: 'bg-zinc-500',
  hex: '#a1a1aa',
};

const PENDING_STATES = new Set(['pending-review', 'needs-revision']);

function personaTone(p) {
  return PERSONA_TONE[(p || '').toUpperCase()] || PERSONA_DEFAULT;
}

function classNames(...xs) {
  return xs.filter(Boolean).join(' ');
}

// IdeaCard is the inbox row. Keeps its own collapsed/expanded state and
// note-textarea draft so the rest of the page isn't re-rendered on every
// keystroke when the user is composing a revision request.
function IdeaCard({ idea, busy, onAction }) {
  const tone = personaTone(idea.persona);
  const [expanded, setExpanded] = useState(false);
  const [revising, setRevising] = useState(false);
  const [note, setNote] = useState('');

  const pending = PENDING_STATES.has(idea.state);

  const submitAction = (action) => {
    onAction(idea.id, action, action === 'needs-revision' ? note : '');
    if (action === 'needs-revision') {
      setNote('');
      setRevising(false);
    }
  };

  return (
    <article
      id={`idea-${idea.id}`}
      className={classNames(
        'rounded-xl border bg-panel p-4 shadow-sm',
        pending ? 'border-line' : 'border-line opacity-60'
      )}
    >
      <header className="flex flex-wrap items-center gap-2">
        <span
          className={classNames(
            'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium',
            tone.badge
          )}
        >
          <span className={classNames('h-1.5 w-1.5 rounded-full', tone.dot)} />
          {idea.persona}
        </span>
        <span className="text-[11px] uppercase tracking-wide text-zinc-500">{idea.date}</span>
        {idea.revision_count > 0 && (
          <span className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[10px] font-semibold uppercase text-amber-300">
            v{idea.revision_count + 1}
          </span>
        )}
        {idea.state === 'accepted' && idea.accepted_bead_id && (
          <span className="rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] font-semibold uppercase text-emerald-300">
            accepted → {idea.accepted_bead_id}
          </span>
        )}
        {idea.state === 'declined' && (
          <span className="rounded-full bg-rose-500/15 px-2 py-0.5 text-[10px] font-semibold uppercase text-rose-300">
            declined
          </span>
        )}
        {idea.state === 'parked' && (
          <span className="rounded-full bg-zinc-500/15 px-2 py-0.5 text-[10px] font-semibold uppercase text-zinc-300">
            parked
          </span>
        )}
        {idea.state === 'archived' && (
          <span className="rounded-full bg-zinc-500/15 px-2 py-0.5 text-[10px] font-semibold uppercase text-zinc-300">
            archived
          </span>
        )}
      </header>

      <h3 className="mt-2 text-base font-semibold leading-snug text-zinc-100">
        {idea.title}
      </h3>

      {idea.synthesis && (
        <details className="mt-2 rounded-md border border-aura/30 bg-aura/5 px-3 py-2 text-[13px] leading-relaxed text-zinc-200">
          <summary className="cursor-pointer text-[11px] font-semibold uppercase tracking-wide text-aura">
            Why this matters
          </summary>
          <p className="mt-2 whitespace-pre-wrap text-zinc-300">{idea.synthesis}</p>
        </details>
      )}

      <div className="mt-3 flex flex-wrap gap-1.5 text-[11px]">
        {idea.effort && (
          <span className="rounded-md border border-line bg-bg px-2 py-0.5 text-zinc-300">
            effort: <span className="font-semibold text-zinc-100">{idea.effort}</span>
          </span>
        )}
        {idea.risk_if_we_dont && (
          <span className="rounded-md border border-line bg-bg px-2 py-0.5 text-zinc-400" title={idea.risk_if_we_dont}>
            risk if we don't act
          </span>
        )}
      </div>

      {idea.first_step && (
        <blockquote className="mt-3 border-l-2 border-aura/50 pl-3 text-[13px] italic text-zinc-300">
          first step → {idea.first_step}
        </blockquote>
      )}

      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className="mt-3 text-[11px] font-semibold uppercase tracking-wide text-aura hover:text-cyan-200"
      >
        {expanded ? 'hide body' : 'read body'}
      </button>
      {expanded && (
        <p className="mt-2 whitespace-pre-wrap text-[13px] leading-relaxed text-zinc-200">
          {idea.body}
        </p>
      )}

      {idea.references && idea.references.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-1.5">
          {idea.references.map((ref) => (
            <span
              key={ref}
              title={ref}
              className="rounded border border-line bg-bg px-1.5 py-0.5 font-mono text-[10px] text-zinc-400"
            >
              {ref}
            </span>
          ))}
        </div>
      )}

      {idea.note && (
        <div className="mt-3 rounded-md border border-amber-500/30 bg-amber-500/5 p-2 text-[12px] text-amber-200">
          <span className="font-semibold uppercase tracking-wide text-amber-300">note: </span>
          {idea.note}
        </div>
      )}

      {pending && (
        <div className="mt-4">
          {!revising ? (
            <div className="flex flex-wrap gap-2">
              <button
                disabled={busy}
                onClick={() => submitAction('accept')}
                className="rounded-md bg-emerald-500/90 px-3 py-1.5 text-sm font-semibold text-zinc-950 hover:bg-emerald-400 disabled:opacity-50"
              >
                Accept
              </button>
              <button
                disabled={busy}
                onClick={() => setRevising(true)}
                className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-1.5 text-sm font-semibold text-amber-200 hover:bg-amber-500/20 disabled:opacity-50"
              >
                Needs revision
              </button>
              <button
                disabled={busy}
                onClick={() => submitAction('decline')}
                className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-1.5 text-sm font-semibold text-rose-200 hover:bg-rose-500/20 disabled:opacity-50"
              >
                Decline
              </button>
              <button
                disabled={busy}
                onClick={() => submitAction('park')}
                className="rounded-md border border-line bg-bg px-3 py-1.5 text-sm font-semibold text-zinc-300 hover:bg-zinc-800 disabled:opacity-50"
              >
                Park
              </button>
              <button
                disabled={busy}
                onClick={() => submitAction('archive')}
                className="rounded-md border border-line bg-bg px-3 py-1.5 text-sm text-zinc-400 hover:bg-zinc-800 disabled:opacity-50"
              >
                Archive
              </button>
            </div>
          ) : (
            <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-3">
              <label className="block text-[11px] font-semibold uppercase tracking-wide text-amber-300">
                What needs revising?
              </label>
              <textarea
                autoFocus
                value={note}
                onChange={(e) => setNote(e.target.value)}
                placeholder="Be specific. The next pass reads this note."
                className="mt-2 h-24 w-full resize-y rounded-md border border-line bg-bg p-2 text-sm text-zinc-100 focus:border-aura focus:outline-none"
              />
              <div className="mt-2 flex gap-2">
                <button
                  disabled={busy || !note.trim()}
                  onClick={() => submitAction('needs-revision')}
                  className="rounded-md bg-amber-500 px-3 py-1.5 text-sm font-semibold text-zinc-950 hover:bg-amber-400 disabled:opacity-50"
                >
                  Send back for revision
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setRevising(false);
                    setNote('');
                  }}
                  className="rounded-md border border-line bg-bg px-3 py-1.5 text-sm text-zinc-300 hover:bg-zinc-800"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>
      )}
    </article>
  );
}

// ConnectionsGraph is a deliberately small, no-dependency force-directed
// SVG. Real d3-force would have been overkill for v1 — under ~25 nodes the
// O(n²) repulsion + Hooke spring on edges converges in <100 ticks at a
// barely-measurable cost. Recomputes on idea/edge changes (not every frame).
function ConnectionsGraph({ nodes, edges, onSelect }) {
  const svgRef = useRef(null);
  const [layout, setLayout] = useState({ positions: {}, width: 360, height: 360 });

  useEffect(() => {
    if (!nodes || nodes.length === 0) return;
    const width = 360;
    const height = 360;
    const center = { x: width / 2, y: height / 2 };
    const radius = 120;
    const positions = {};
    nodes.forEach((n, i) => {
      const angle = (i / nodes.length) * Math.PI * 2;
      positions[n.id] = {
        x: center.x + Math.cos(angle) * radius,
        y: center.y + Math.sin(angle) * radius,
        vx: 0,
        vy: 0,
      };
    });
    const adjacency = new Map();
    edges.forEach((e) => {
      if (!adjacency.has(e.source)) adjacency.set(e.source, new Set());
      if (!adjacency.has(e.target)) adjacency.set(e.target, new Set());
      adjacency.get(e.source).add(e.target);
      adjacency.get(e.target).add(e.source);
    });

    // 200 ticks of simple force sim. Repulsion 1/d², spring on edges. Center
    // pull toward middle to keep the graph from drifting off-canvas.
    for (let t = 0; t < 200; t++) {
      for (let i = 0; i < nodes.length; i++) {
        const a = positions[nodes[i].id];
        for (let j = i + 1; j < nodes.length; j++) {
          const b = positions[nodes[j].id];
          const dx = a.x - b.x;
          const dy = a.y - b.y;
          const dist2 = Math.max(50, dx * dx + dy * dy);
          const force = 1500 / dist2;
          const fx = (dx / Math.sqrt(dist2)) * force;
          const fy = (dy / Math.sqrt(dist2)) * force;
          a.vx += fx;
          a.vy += fy;
          b.vx -= fx;
          b.vy -= fy;
        }
      }
      edges.forEach((e) => {
        const a = positions[e.source];
        const b = positions[e.target];
        if (!a || !b) return;
        const dx = b.x - a.x;
        const dy = b.y - a.y;
        const dist = Math.sqrt(dx * dx + dy * dy) || 1;
        const ideal = 80;
        const k = 0.02;
        const f = (dist - ideal) * k;
        a.vx += (dx / dist) * f;
        a.vy += (dy / dist) * f;
        b.vx -= (dx / dist) * f;
        b.vy -= (dy / dist) * f;
      });
      nodes.forEach((n) => {
        const p = positions[n.id];
        p.vx += (center.x - p.x) * 0.01;
        p.vy += (center.y - p.y) * 0.01;
        p.vx *= 0.7;
        p.vy *= 0.7;
        p.x += p.vx * 0.1;
        p.y += p.vy * 0.1;
        p.x = Math.max(20, Math.min(width - 20, p.x));
        p.y = Math.max(20, Math.min(height - 20, p.y));
      });
    }
    setLayout({ positions, width, height });
  }, [nodes, edges]);

  if (!nodes || nodes.length === 0) {
    return (
      <div className="rounded-xl border border-line bg-panel p-6 text-center text-sm text-zinc-500">
        No ideas yet — nothing to graph.
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-line bg-panel p-3">
      <svg
        ref={svgRef}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
        className="block w-full"
        style={{ maxHeight: '60vh' }}
      >
        {edges.map((e) => {
          const a = layout.positions[e.source];
          const b = layout.positions[e.target];
          if (!a || !b) return null;
          return (
            <line
              key={`${e.source}-${e.target}`}
              x1={a.x}
              y1={a.y}
              x2={b.x}
              y2={b.y}
              stroke="#1f2a37"
              strokeWidth={1}
            />
          );
        })}
        {nodes.map((n) => {
          const p = layout.positions[n.id];
          if (!p) return null;
          const tone = personaTone(n.persona);
          const r = 8 + Math.min(n.revision_count || 0, 4) * 2;
          return (
            <g key={n.id} onClick={() => onSelect && onSelect(n.id)} style={{ cursor: 'pointer' }}>
              <circle cx={p.x} cy={p.y} r={r} fill={tone.hex} opacity={0.85} />
              <text
                x={p.x}
                y={p.y + r + 10}
                textAnchor="middle"
                fontSize="9"
                fill="#a1a1aa"
                style={{ pointerEvents: 'none' }}
              >
                {n.title.length > 22 ? `${n.title.slice(0, 22)}…` : n.title}
              </text>
            </g>
          );
        })}
      </svg>
      <div className="mt-2 flex flex-wrap gap-2 text-[10px] text-zinc-400">
        {Object.entries(PERSONA_TONE).map(([k, v]) => (
          <span key={k} className="inline-flex items-center gap-1">
            <span className="inline-block h-2 w-2 rounded-full" style={{ background: v.hex }} />
            {k}
          </span>
        ))}
        <span className="ml-auto text-zinc-500">edges = shared references</span>
      </div>
    </div>
  );
}

// PersonaPromptEditor lets the user edit the 5 persona blocks + the shared
// guardrails as separate textareas. Save POSTs to /api/persona-prompts which
// rewrites PERSONA-PROMPTS.md and runs the gate-string commit + push.
function PersonaPromptEditor() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [draft, setDraft] = useState(null);
  const [openKey, setOpenKey] = useState(null);
  const [status, setStatus] = useState(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetch('/api/persona-prompts')
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json();
      })
      .then((data) => {
        if (!cancelled) {
          setDraft({
            preamble: data.preamble || '',
            ceo: data.ceo || '',
            cfo: data.cfo || '',
            cmo: data.cmo || '',
            cto: data.cto || '',
            cpo: data.cpo || '',
            sharedGuardrails: data.sharedGuardrails || '',
          });
          setLoading(false);
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const handleSave = async () => {
    if (!draft) return;
    setSaving(true);
    setStatus(null);
    try {
      const res = await fetch('/api/persona-prompts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
      const data = await res.json();
      if (!res.ok) {
        setStatus({ ok: false, message: data.error || `HTTP ${res.status}` });
      } else if (data.pushed) {
        setStatus({ ok: true, message: 'Saved + committed + pushed.' });
      } else {
        setStatus({ ok: false, message: `Saved on disk but git failed: ${data.error}` });
      }
    } catch (e) {
      setStatus({ ok: false, message: String(e) });
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="p-4 text-sm text-zinc-400">Loading prompts…</div>;
  if (error) return <div className="p-4 text-sm text-rose-400">Failed to load: {error}</div>;
  if (!draft) return null;

  const sections = [
    { key: 'preamble', label: 'Preamble', tone: 'zinc' },
    { key: 'ceo', label: 'CEO', tone: 'indigo' },
    { key: 'cfo', label: 'CFO', tone: 'emerald' },
    { key: 'cmo', label: 'CMO', tone: 'orange' },
    { key: 'cto', label: 'CTO', tone: 'sky' },
    { key: 'cpo', label: 'CPO', tone: 'purple' },
    { key: 'sharedGuardrails', label: 'Shared guardrails + tuning', tone: 'zinc' },
  ];

  return (
    <div className="space-y-3">
      <p className="text-[12px] text-zinc-400">
        Edits save to <code className="text-zinc-300">PERSONA-PROMPTS.md</code> and auto-commit to
        the <code className="text-zinc-300">k3s-pi</code> repo with the gate string{' '}
        <code className="text-aura">vetted: k3s-gdxk persona prompts</code>.
      </p>
      {sections.map((s) => {
        const open = openKey === s.key;
        return (
          <div key={s.key} className="rounded-xl border border-line bg-panel">
            <button
              type="button"
              onClick={() => setOpenKey(open ? null : s.key)}
              className="flex w-full items-center justify-between px-4 py-3 text-left"
            >
              <span className="text-sm font-semibold text-zinc-100">{s.label}</span>
              <span className="text-xs text-zinc-500">{open ? 'hide' : 'edit'}</span>
            </button>
            {open && (
              <div className="border-t border-line p-3">
                <textarea
                  value={draft[s.key]}
                  onChange={(e) => setDraft({ ...draft, [s.key]: e.target.value })}
                  className="h-72 w-full resize-y rounded-md border border-line bg-bg p-3 font-mono text-[12px] leading-relaxed text-zinc-100 focus:border-aura focus:outline-none"
                />
              </div>
            )}
          </div>
        );
      })}
      <div className="flex items-center gap-3">
        <button
          type="button"
          disabled={saving}
          onClick={handleSave}
          className="rounded-md bg-aura px-4 py-2 text-sm font-semibold text-zinc-950 hover:bg-cyan-300 disabled:opacity-50"
        >
          {saving ? 'Saving…' : 'Save + commit + push'}
        </button>
        {status && (
          <span
            className={classNames(
              'text-sm',
              status.ok ? 'text-emerald-400' : 'text-rose-400'
            )}
          >
            {status.message}
          </span>
        )}
      </div>
    </div>
  );
}

export default function IdeasPage({ onExit }) {
  const [tab, setTab] = useState('inbox');
  const [ideas, setIdeas] = useState([]);
  const [counts, setCounts] = useState({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [filter, setFilter] = useState('pending');
  const [graph, setGraph] = useState({ nodes: [], edges: [] });

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch('/api/ideas');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      setIdeas(data.ideas || []);
      setCounts(data.counts || {});
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  const loadGraph = useCallback(async () => {
    try {
      const res = await fetch('/api/idea-graph');
      if (!res.ok) return;
      const data = await res.json();
      setGraph({ nodes: data.nodes || [], edges: data.edges || [] });
    } catch (_) {
      // best-effort — graph tab will show empty state
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    if (tab === 'connections') loadGraph();
  }, [tab, loadGraph]);

  const handleAction = useCallback(
    async (id, action, note) => {
      setBusy(true);
      try {
        const res = await fetch(`/api/ideas/${encodeURIComponent(id)}/action`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action, note: note || '' }),
        });
        if (!res.ok) {
          const text = await res.text();
          alert(`Action failed: ${text}`);
          return;
        }
        await load();
      } finally {
        setBusy(false);
      }
    },
    [load]
  );

  const pendingCount = (counts['pending-review'] || 0) + (counts['needs-revision'] || 0);

  const filteredIdeas = useMemo(() => {
    if (filter === 'all') return ideas;
    if (filter === 'accepted') return ideas.filter((i) => i.state === 'accepted');
    if (filter === 'declined') return ideas.filter((i) => i.state === 'declined');
    return ideas.filter((i) => PENDING_STATES.has(i.state));
  }, [ideas, filter]);

  return (
    <div className="flex h-full w-full flex-col overflow-hidden bg-bg text-zinc-100">
      <header className="flex shrink-0 items-center gap-2 border-b border-line bg-panel px-3 py-2">
        <button
          type="button"
          onClick={() => onExit && onExit()}
          className="rounded p-1 text-zinc-300 hover:bg-zinc-800"
          title="Back to terminal"
        >
          ←
        </button>
        <h1 className="flex-1 text-sm font-semibold">Ideas inbox</h1>
        {pendingCount > 0 && (
          <span className="rounded-full bg-aura/15 px-2 py-0.5 text-[11px] font-semibold text-aura">
            {pendingCount} pending
          </span>
        )}
        <button
          type="button"
          onClick={load}
          disabled={loading}
          className="rounded p-1 text-zinc-400 hover:bg-zinc-800 disabled:opacity-50"
          title="Refresh"
        >
          ↻
        </button>
      </header>

      <nav className="flex shrink-0 gap-1 border-b border-line bg-panel/60 px-3 py-2 text-sm">
        {[
          { k: 'inbox', l: 'Inbox' },
          { k: 'connections', l: 'Connections' },
          { k: 'personas', l: 'Personas' },
          { k: 'agents', l: 'Agents' },
          { k: 'gascity', l: 'Gas City' },
        ].map((t) => (
          <button
            key={t.k}
            type="button"
            onClick={() => setTab(t.k)}
            className={classNames(
              'rounded-md px-3 py-1.5 text-[13px] font-medium',
              tab === t.k
                ? 'bg-aura/15 text-aura'
                : 'text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200'
            )}
          >
            {t.l}
          </button>
        ))}
      </nav>

      <main className="min-h-0 flex-1 overflow-y-auto px-3 py-3">
        {tab === 'inbox' && (
          <>
            <div className="mb-3 flex flex-wrap gap-1.5 text-[11px]">
              {[
                { k: 'pending', l: `Pending (${pendingCount})` },
                { k: 'accepted', l: `Accepted (${counts['accepted'] || 0})` },
                { k: 'declined', l: `Declined (${counts['declined'] || 0})` },
                { k: 'all', l: `All (${ideas.length})` },
              ].map((f) => (
                <button
                  key={f.k}
                  type="button"
                  onClick={() => setFilter(f.k)}
                  className={classNames(
                    'rounded-full border px-2.5 py-1 text-[11px]',
                    filter === f.k
                      ? 'border-aura/60 bg-aura/10 text-aura'
                      : 'border-line bg-bg text-zinc-300 hover:bg-zinc-800'
                  )}
                >
                  {f.l}
                </button>
              ))}
            </div>

            {loading && <div className="py-6 text-center text-sm text-zinc-400">Loading…</div>}
            {error && (
              <div className="rounded-md border border-rose-500/40 bg-rose-500/10 p-3 text-sm text-rose-200">
                Failed to load ideas: {error}
              </div>
            )}
            {!loading && !error && filteredIdeas.length === 0 && (
              <div className="rounded-xl border border-line bg-panel p-6 text-center text-sm text-zinc-500">
                Nothing to review.
              </div>
            )}
            <div className="space-y-3">
              {filteredIdeas.map((idea) => (
                <IdeaCard key={idea.id} idea={idea} busy={busy} onAction={handleAction} />
              ))}
            </div>
          </>
        )}
        {tab === 'connections' && (
          <ConnectionsGraph
            nodes={graph.nodes}
            edges={graph.edges}
            onSelect={(id) => {
              setTab('inbox');
              setFilter('all');
              // best-effort scroll to the card next tick
              setTimeout(() => {
                const el = document.getElementById(`idea-${id}`);
                if (el) el.scrollIntoView({ behavior: 'smooth', block: 'center' });
              }, 50);
            }}
          />
        )}
        {tab === 'personas' && <PersonaPromptEditor />}
        {tab === 'agents' && <AgentsPanel />}
        {tab === 'gascity' && (
          <div className="h-full -mx-3 -my-3">
            <iframe
              src="/gc"
              title="Gas City Dashboard"
              className="h-full w-full border-0"
              style={{ minHeight: 'calc(100vh - 160px)' }}
            />
          </div>
        )}
      </main>
    </div>
  );
}
