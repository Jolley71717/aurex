import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useBeads } from '../hooks/useBeads.js';

// 3-column board for bd issues. Status maps:
//   open / blocked  -> Open column
//   in_progress     -> In Progress column
//   closed          -> Closed column
//
// `bd list --json` doesn't return closed beads, so the Closed column shows
// only beads closed during this session (kept locally after the POST close).

const COLUMNS = [
  { key: 'open', label: 'Open', statuses: ['open', 'blocked'] },
  { key: 'in_progress', label: 'In Progress', statuses: ['in_progress'] },
  { key: 'closed', label: 'Closed', statuses: ['closed'] },
];

const PRIORITY_TONE = {
  0: { label: 'P0', cls: 'bg-rose-500/15 text-rose-300 border-rose-500/40' },
  1: { label: 'P1', cls: 'bg-orange-500/15 text-orange-300 border-orange-500/40' },
  2: { label: 'P2', cls: 'bg-amber-500/15 text-amber-300 border-amber-500/40' },
  3: { label: 'P3', cls: 'bg-zinc-700/40 text-zinc-300 border-zinc-600/40' },
};

function classNames(...xs) {
  return xs.filter(Boolean).join(' ');
}

function priorityPill(p) {
  const tone = PRIORITY_TONE[p] || PRIORITY_TONE[3];
  return (
    <span
      className={classNames(
        'inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase',
        tone.cls
      )}
    >
      {tone.label}
    </span>
  );
}

function relativeTime(iso) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const delta = Math.floor((Date.now() - t) / 1000);
  if (delta < 60) return `${delta}s ago`;
  if (delta < 3600) return `${Math.floor(delta / 60)}m ago`;
  if (delta < 86400) return `${Math.floor(delta / 3600)}h ago`;
  return `${Math.floor(delta / 86400)}d ago`;
}

function statusToColumnKey(status) {
  if (status === 'in_progress') return 'in_progress';
  if (status === 'closed') return 'closed';
  return 'open';
}

function matchesSearch(bead, q) {
  if (!q) return true;
  const needle = q.toLowerCase();
  if ((bead.title || '').toLowerCase().includes(needle)) return true;
  const labels = Array.isArray(bead.labels) ? bead.labels : [];
  if (labels.some((l) => String(l).toLowerCase().includes(needle))) return true;
  // priority can be matched as "p0", "p1", etc. or by raw number.
  const pri = `p${bead.priority ?? ''}`;
  if (pri.includes(needle)) return true;
  if (String(bead.priority ?? '').includes(needle)) return true;
  // metadata.* — flatten to "key=value" strings (lowercased)
  if (bead.metadata && typeof bead.metadata === 'object') {
    for (const [k, v] of Object.entries(bead.metadata)) {
      if (`${k}=${v}`.toLowerCase().includes(needle)) return true;
    }
  }
  return false;
}

// BeadCard renders one bead. Title is click-to-edit; status pill is a
// 3-segment toggle that POSTs to the matching backend route.
function BeadCard({ bead, childrenIds, onEdit, onSetColumn, busy }) {
  const [editing, setEditing] = useState(false);
  const [draftTitle, setDraftTitle] = useState(bead.title || '');
  const [draftDesc, setDraftDesc] = useState(bead.description || '');

  useEffect(() => {
    if (!editing) {
      setDraftTitle(bead.title || '');
      setDraftDesc(bead.description || '');
    }
  }, [bead.title, bead.description, editing]);

  const startEdit = () => {
    setDraftTitle(bead.title || '');
    setDraftDesc(bead.description || '');
    setEditing(true);
  };

  const cancelEdit = () => {
    setEditing(false);
    setDraftTitle(bead.title || '');
    setDraftDesc(bead.description || '');
  };

  const saveEdit = async () => {
    const titleChanged = draftTitle !== (bead.title || '');
    const descChanged = draftDesc !== (bead.description || '');
    if (!titleChanged && !descChanged) {
      setEditing(false);
      return;
    }
    setEditing(false);
    onEdit(bead.id, {
      title: titleChanged ? draftTitle : undefined,
      description: descChanged ? draftDesc : undefined,
    });
  };

  const deps = Array.isArray(bead.dependencies) ? bead.dependencies : [];
  const parentIds = deps
    .filter((d) => d && d.type === 'blocks' && d.depends_on_id)
    .map((d) => d.depends_on_id);

  const labels = Array.isArray(bead.labels) ? bead.labels : [];
  const currentCol = statusToColumnKey(bead.status);

  return (
    <article className="rounded-xl border border-line bg-panel p-3 shadow-sm">
      <header className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[11px] text-zinc-400">{bead.id}</span>
        {priorityPill(bead.priority)}
        {bead.issue_type && (
          <span className="rounded-full border border-line bg-bg px-2 py-0.5 text-[10px] uppercase text-zinc-400">
            {bead.issue_type}
          </span>
        )}
        <span className="ml-auto text-[10px] text-zinc-500" title={bead.updated_at}>
          {relativeTime(bead.updated_at)}
        </span>
      </header>

      {editing ? (
        <div className="mt-2 space-y-2">
          <input
            autoFocus
            value={draftTitle}
            onChange={(e) => setDraftTitle(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') cancelEdit();
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) saveEdit();
            }}
            className="w-full rounded-md border border-aura/40 bg-bg px-2 py-1.5 text-sm font-medium text-zinc-100 focus:outline-none"
          />
          <textarea
            value={draftDesc}
            onChange={(e) => setDraftDesc(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') cancelEdit();
            }}
            placeholder="Description"
            rows={5}
            className="w-full resize-y rounded-md border border-line bg-bg p-2 text-[13px] leading-relaxed text-zinc-200 focus:border-aura focus:outline-none"
          />
          <div className="flex gap-2 text-[11px]">
            <button
              type="button"
              onClick={saveEdit}
              className="rounded-md bg-aura px-3 py-1.5 font-semibold text-zinc-950 hover:bg-cyan-300"
            >
              Save
            </button>
            <button
              type="button"
              onClick={cancelEdit}
              className="rounded-md border border-line bg-bg px-3 py-1.5 text-zinc-300 hover:bg-zinc-800"
            >
              Cancel (Esc)
            </button>
          </div>
        </div>
      ) : (
        <h3
          role="button"
          tabIndex={0}
          onClick={startEdit}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              startEdit();
            }
          }}
          className="mt-2 cursor-text rounded text-sm font-medium leading-snug text-zinc-100 hover:text-aura"
        >
          {bead.title || <span className="text-zinc-500 italic">(untitled)</span>}
        </h3>
      )}

      {labels.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {labels.map((l) => (
            <span
              key={l}
              className="rounded border border-line bg-bg px-1.5 py-0.5 font-mono text-[10px] text-zinc-400"
            >
              {l}
            </span>
          ))}
        </div>
      )}

      {(parentIds.length > 0 || (childrenIds && childrenIds.length > 0)) && (
        <div className="mt-2 space-y-0.5 text-[11px] text-zinc-500">
          {parentIds.length > 0 && (
            <div className="truncate">
              <span className="text-zinc-600">parent:</span>{' '}
              <span className="font-mono text-zinc-400">{parentIds.join(', ')}</span>
            </div>
          )}
          {childrenIds && childrenIds.length > 0 && (
            <div className="truncate">
              <span className="text-zinc-600">⇒ children:</span>{' '}
              <span className="font-mono text-zinc-400">{childrenIds.join(', ')}</span>
            </div>
          )}
        </div>
      )}

      {!editing && (
        <div
          className="mt-3 inline-flex overflow-hidden rounded-md border border-line text-[11px]"
          role="group"
          aria-label="Status"
        >
          {COLUMNS.map((c) => {
            const active = c.key === currentCol;
            return (
              <button
                key={c.key}
                type="button"
                disabled={busy || active}
                onClick={() => onSetColumn(bead.id, c.key)}
                className={classNames(
                  'px-2.5 py-1.5 min-h-[40px] font-medium',
                  active
                    ? 'bg-aura/15 text-aura'
                    : 'bg-bg text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 disabled:opacity-50'
                )}
              >
                {c.label}
              </button>
            );
          })}
        </div>
      )}
    </article>
  );
}

// NewBeadDrawer slides up from the bottom on mobile, sits as a side sheet
// on desktop. Title is required; everything else is optional.
function NewBeadDrawer({ open, onClose, onCreate, busy }) {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('2');
  const [type, setType] = useState('task');

  useEffect(() => {
    if (open) {
      setTitle('');
      setDescription('');
      setPriority('2');
      setType('task');
    }
  }, [open]);

  if (!open) return null;

  const submit = (e) => {
    e.preventDefault();
    if (!title.trim()) return;
    onCreate({
      title: title.trim(),
      description: description.trim(),
      priority,
      type,
    });
  };

  return (
    <div className="fixed inset-0 z-50 flex items-end justify-end md:items-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} aria-hidden="true" />
      <form
        onSubmit={submit}
        className="relative w-full max-w-md rounded-t-2xl border-t border-line bg-panel p-4 md:m-4 md:rounded-2xl md:border"
      >
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold text-zinc-100">New bead</h2>
          <button
            type="button"
            onClick={onClose}
            className="min-h-[40px] min-w-[40px] rounded text-zinc-400 hover:bg-zinc-800"
            aria-label="Close"
          >
            ✕
          </button>
        </div>
        <label className="block">
          <span className="block text-[11px] uppercase tracking-wide text-zinc-400">Title *</span>
          <input
            autoFocus
            required
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            className="mt-1 w-full rounded-md border border-line bg-bg p-2 text-sm text-zinc-100 focus:border-aura focus:outline-none"
          />
        </label>
        <label className="mt-3 block">
          <span className="block text-[11px] uppercase tracking-wide text-zinc-400">Description</span>
          <textarea
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            rows={4}
            className="mt-1 w-full resize-y rounded-md border border-line bg-bg p-2 text-[13px] text-zinc-200 focus:border-aura focus:outline-none"
          />
        </label>
        <div className="mt-3 grid grid-cols-2 gap-3">
          <label>
            <span className="block text-[11px] uppercase tracking-wide text-zinc-400">Priority</span>
            <select
              value={priority}
              onChange={(e) => setPriority(e.target.value)}
              className="mt-1 w-full rounded-md border border-line bg-bg p-2 text-sm text-zinc-100 focus:border-aura focus:outline-none"
            >
              <option value="0">P0 (critical)</option>
              <option value="1">P1 (high)</option>
              <option value="2">P2 (medium)</option>
              <option value="3">P3 (low)</option>
            </select>
          </label>
          <label>
            <span className="block text-[11px] uppercase tracking-wide text-zinc-400">Type</span>
            <select
              value={type}
              onChange={(e) => setType(e.target.value)}
              className="mt-1 w-full rounded-md border border-line bg-bg p-2 text-sm text-zinc-100 focus:border-aura focus:outline-none"
            >
              <option value="task">task</option>
              <option value="bug">bug</option>
              <option value="feature">feature</option>
              <option value="chore">chore</option>
              <option value="epic">epic</option>
            </select>
          </label>
        </div>
        <div className="mt-4 flex gap-2">
          <button
            type="submit"
            disabled={busy || !title.trim()}
            className="rounded-md bg-aura px-4 py-2 text-sm font-semibold text-zinc-950 hover:bg-cyan-300 disabled:opacity-50"
          >
            {busy ? 'Creating…' : 'Create bead'}
          </button>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-line bg-bg px-4 py-2 text-sm text-zinc-300 hover:bg-zinc-800"
          >
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}

function EventFeed({ events, sseConnected, onSelect }) {
  return (
    <aside className="flex h-full min-h-0 flex-col rounded-xl border border-line bg-panel">
      <header className="flex items-center justify-between border-b border-line px-3 py-2">
        <h2 className="text-[11px] font-semibold uppercase tracking-wide text-zinc-300">
          Live events
        </h2>
        <span
          className={classNames(
            'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px]',
            sseConnected
              ? 'bg-emerald-500/15 text-emerald-300'
              : 'bg-zinc-700/40 text-zinc-400'
          )}
          title={sseConnected ? 'SSE connected' : 'SSE offline — polling'}
        >
          <span
            className={classNames(
              'inline-block h-1.5 w-1.5 rounded-full',
              sseConnected ? 'bg-emerald-400' : 'bg-zinc-500'
            )}
          />
          {sseConnected ? 'live' : 'polling'}
        </span>
      </header>
      <ol className="min-h-0 flex-1 overflow-y-auto px-3 py-2 text-[12px]">
        {events.length === 0 && (
          <li className="py-2 text-center text-[11px] text-zinc-500">
            No events yet.
          </li>
        )}
        {events.map((ev) => (
          <li
            key={ev._key}
            className="border-b border-line/60 py-1.5 last:border-b-0"
          >
            <button
              type="button"
              onClick={() => onSelect && ev.beadId && onSelect(ev.beadId)}
              className="block w-full text-left hover:text-aura"
            >
              <span className="font-mono text-zinc-300">{ev.beadId || '—'}</span>{' '}
              <span className="text-zinc-500">{ev.type}</span>
              <div className="text-[10px] text-zinc-500">{relativeTime(ev.at)}</div>
            </button>
          </li>
        ))}
      </ol>
    </aside>
  );
}

export default function BeadsPage({ onExit }) {
  const {
    beads,
    childrenOf,
    loading,
    error,
    events,
    sseConnected,
    refetchAll,
    setBeadOptimistic,
    addBeadOptimistic,
    removeBeadOptimistic,
  } = useBeads();

  const [search, setSearch] = useState('');
  const [busy, setBusy] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [actionError, setActionError] = useState(null);

  const filtered = useMemo(
    () => beads.filter((b) => matchesSearch(b, search.trim())),
    [beads, search]
  );

  const grouped = useMemo(() => {
    const out = { open: [], in_progress: [], closed: [] };
    for (const b of filtered) {
      const key = statusToColumnKey(b.status);
      out[key].push(b);
    }
    // priority asc (0 = highest), then most-recently-updated first.
    for (const k of Object.keys(out)) {
      out[k].sort((a, b) => {
        const pa = a.priority ?? 99;
        const pb = b.priority ?? 99;
        if (pa !== pb) return pa - pb;
        return (b.updated_at || '').localeCompare(a.updated_at || '');
      });
    }
    return out;
  }, [filtered]);

  const handleSetColumn = useCallback(
    async (id, targetCol) => {
      const cur = beads.find((b) => b.id === id);
      if (!cur) return;
      const curCol = statusToColumnKey(cur.status);
      if (curCol === targetCol) return;

      const prevStatus = cur.status;
      let optimisticStatus = prevStatus;
      let url;
      if (targetCol === 'closed') {
        url = `/api/beads/${encodeURIComponent(id)}/close`;
        optimisticStatus = 'closed';
      } else if (targetCol === 'in_progress') {
        url = `/api/beads/${encodeURIComponent(id)}/state`;
        optimisticStatus = 'in_progress';
      } else if (targetCol === 'open') {
        // Reopen is the only way to come back from closed. From in_progress
        // we set-state to "open".
        if (prevStatus === 'closed') {
          url = `/api/beads/${encodeURIComponent(id)}/reopen`;
        } else {
          url = `/api/beads/${encodeURIComponent(id)}/state`;
        }
        optimisticStatus = 'open';
      } else {
        return;
      }

      setBeadOptimistic(id, { status: optimisticStatus });
      setBusy(true);
      setActionError(null);
      try {
        const body =
          url.endsWith('/state') ? JSON.stringify({ state: optimisticStatus }) : undefined;
        const res = await fetch(url, {
          method: 'POST',
          headers: body ? { 'Content-Type': 'application/json' } : undefined,
          body,
        });
        if (!res.ok) {
          setBeadOptimistic(id, { status: prevStatus });
          const t = await res.text();
          setActionError(`Move failed: ${t || res.status}`);
        }
      } catch (e) {
        setBeadOptimistic(id, { status: prevStatus });
        setActionError(String(e));
      } finally {
        setBusy(false);
      }
    },
    [beads, setBeadOptimistic]
  );

  const handleEdit = useCallback(
    async (id, patch) => {
      const cur = beads.find((b) => b.id === id);
      if (!cur) return;
      const prev = { title: cur.title, description: cur.description };
      const optimistic = {};
      if (patch.title !== undefined) optimistic.title = patch.title;
      if (patch.description !== undefined) optimistic.description = patch.description;
      setBeadOptimistic(id, { ...optimistic, updated_at: new Date().toISOString() });
      setBusy(true);
      setActionError(null);
      try {
        const res = await fetch(`/api/beads/${encodeURIComponent(id)}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(patch),
        });
        if (!res.ok) {
          setBeadOptimistic(id, prev);
          const t = await res.text();
          setActionError(`Save failed: ${t || res.status}`);
        }
      } catch (e) {
        setBeadOptimistic(id, prev);
        setActionError(String(e));
      } finally {
        setBusy(false);
      }
    },
    [beads, setBeadOptimistic]
  );

  const handleCreate = useCallback(
    async (input) => {
      setBusy(true);
      setActionError(null);
      const tempId = `tmp-${Date.now()}`;
      const optimistic = {
        id: tempId,
        title: input.title,
        description: input.description,
        priority: Number(input.priority),
        issue_type: input.type,
        status: 'open',
        updated_at: new Date().toISOString(),
        created_at: new Date().toISOString(),
      };
      addBeadOptimistic(optimistic);
      setDrawerOpen(false);
      try {
        const res = await fetch('/api/beads', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(input),
        });
        if (!res.ok) {
          removeBeadOptimistic(tempId);
          const t = await res.text();
          setActionError(`Create failed: ${t || res.status}`);
          return;
        }
        const created = await res.json();
        removeBeadOptimistic(tempId);
        addBeadOptimistic(created);
      } catch (e) {
        removeBeadOptimistic(tempId);
        setActionError(String(e));
      } finally {
        setBusy(false);
      }
    },
    [addBeadOptimistic, removeBeadOptimistic]
  );

  const focusBead = useCallback((id) => {
    const el = document.getElementById(`bead-${id}`);
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }, []);

  return (
    <div className="flex h-full w-full flex-col overflow-hidden bg-bg text-zinc-100">
      <header className="flex shrink-0 items-center gap-2 border-b border-line bg-panel px-3 py-2">
        <button
          type="button"
          onClick={() => onExit && onExit()}
          className="min-h-[40px] min-w-[40px] rounded p-1 text-zinc-300 hover:bg-zinc-800"
          title="Back to terminal"
          aria-label="Back"
        >
          ←
        </button>
        <h1 className="text-sm font-semibold">Beads</h1>
        <div className="ml-2 flex-1">
          <input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="search title / priority / labels / metadata…"
            className="w-full rounded-md border border-line bg-bg px-3 py-2 text-sm placeholder-zinc-500 focus:border-aura focus:outline-none"
          />
        </div>
        <button
          type="button"
          onClick={refetchAll}
          disabled={loading}
          className="min-h-[40px] rounded p-1 text-zinc-400 hover:bg-zinc-800 disabled:opacity-50"
          title="Refresh"
          aria-label="Refresh"
        >
          ↻
        </button>
        <button
          type="button"
          onClick={() => setDrawerOpen(true)}
          className="min-h-[40px] rounded-md bg-aura px-3 py-1.5 text-sm font-semibold text-zinc-950 hover:bg-cyan-300"
        >
          + New
        </button>
      </header>

      {(error || actionError) && (
        <div className="border-b border-rose-500/30 bg-rose-500/10 px-3 py-2 text-[12px] text-rose-200">
          {error || actionError}
        </div>
      )}

      <main className="grid min-h-0 flex-1 grid-cols-1 gap-3 overflow-hidden p-3 md:grid-cols-[1fr_280px]">
        <section className="grid min-h-0 grid-cols-1 gap-3 overflow-hidden md:grid-cols-3">
          {COLUMNS.map((col) => {
            const items = grouped[col.key] || [];
            return (
              <div
                key={col.key}
                className="flex min-h-0 flex-col rounded-xl border border-line bg-panel/40"
              >
                <header className="flex shrink-0 items-center justify-between border-b border-line px-3 py-2">
                  <span className="text-[12px] font-semibold uppercase tracking-wide text-zinc-300">
                    {col.label}
                  </span>
                  <span className="rounded-full bg-zinc-800/80 px-2 py-0.5 text-[10px] text-zinc-400">
                    {items.length}
                  </span>
                </header>
                <div className="min-h-0 flex-1 space-y-2 overflow-y-auto p-2">
                  {loading && items.length === 0 && (
                    <div className="py-4 text-center text-[12px] text-zinc-500">Loading…</div>
                  )}
                  {!loading && items.length === 0 && (
                    <div className="py-4 text-center text-[11px] text-zinc-500">empty</div>
                  )}
                  {items.map((b) => (
                    <div key={b.id} id={`bead-${b.id}`}>
                      <BeadCard
                        bead={b}
                        childrenIds={childrenOf[b.id]}
                        onEdit={handleEdit}
                        onSetColumn={handleSetColumn}
                        busy={busy}
                      />
                    </div>
                  ))}
                </div>
              </div>
            );
          })}
        </section>

        <div className="hidden min-h-0 md:block">
          <EventFeed events={events} sseConnected={sseConnected} onSelect={focusBead} />
        </div>
      </main>

      {/* Mobile event feed: collapsed bar that expands on tap. */}
      <details className="border-t border-line bg-panel md:hidden">
        <summary className="cursor-pointer px-3 py-2 text-[11px] font-semibold uppercase tracking-wide text-zinc-300">
          Live events {events.length > 0 && <span className="ml-1 text-zinc-500">({events.length})</span>}
        </summary>
        <div className="h-48 overflow-hidden p-2">
          <EventFeed events={events} sseConnected={sseConnected} onSelect={focusBead} />
        </div>
      </details>

      <NewBeadDrawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        onCreate={handleCreate}
        busy={busy}
      />
    </div>
  );
}
