import { useEffect, useMemo, useState } from 'react';

// GoLivePage renders the open beads graph as a force-directed dependency map:
// every non-closed issue is a node (colored by priority), every dependency
// edge between two open nodes is a line (parent-child dashed, blocks solid).
// Priority filter chips let you collapse the 140+ node graph down to the
// go-live-critical P0/P1 set. Data comes from GET /api/beads/graph.

const PRIORITY = {
  0: { label: 'P0', hex: '#ef4444', name: 'critical' },
  1: { label: 'P1', hex: '#f97316', name: 'high' },
  2: { label: 'P2', hex: '#eab308', name: 'medium' },
  3: { label: 'P3', hex: '#64748b', name: 'low' },
  4: { label: 'P4', hex: '#3f3f46', name: 'backlog' },
};

function prio(p) {
  return PRIORITY[p] || PRIORITY[3];
}

const W = 900;
const H = 620;

// runForceLayout is the same tiny O(n²) sim aurex's idea graph uses, sized up
// for the larger canvas. Deterministic seed (angle by index) so the layout is
// stable across renders for the same node set.
function runForceLayout(nodes, edges) {
  if (!nodes.length) return {};
  const center = { x: W / 2, y: H / 2 };
  const radius = Math.min(W, H) / 2.6;
  const pos = {};
  nodes.forEach((n, i) => {
    const angle = (i / nodes.length) * Math.PI * 2;
    pos[n.id] = {
      x: center.x + Math.cos(angle) * radius,
      y: center.y + Math.sin(angle) * radius,
      vx: 0,
      vy: 0,
    };
  });
  const ticks = nodes.length > 80 ? 160 : 240;
  for (let t = 0; t < ticks; t++) {
    for (let i = 0; i < nodes.length; i++) {
      const a = pos[nodes[i].id];
      for (let j = i + 1; j < nodes.length; j++) {
        const b = pos[nodes[j].id];
        const dx = a.x - b.x;
        const dy = a.y - b.y;
        const dist2 = Math.max(80, dx * dx + dy * dy);
        const force = 4000 / dist2;
        const fx = (dx / Math.sqrt(dist2)) * force;
        const fy = (dy / Math.sqrt(dist2)) * force;
        a.vx += fx;
        a.vy += fy;
        b.vx -= fx;
        b.vy -= fy;
      }
    }
    edges.forEach((e) => {
      const a = pos[e.from];
      const b = pos[e.to];
      if (!a || !b) return;
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const dist = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (dist - 110) * 0.02;
      a.vx += (dx / dist) * f;
      a.vy += (dy / dist) * f;
      b.vx -= (dx / dist) * f;
      b.vy -= (dy / dist) * f;
    });
    nodes.forEach((n) => {
      const p = pos[n.id];
      p.vx += (center.x - p.x) * 0.008;
      p.vy += (center.y - p.y) * 0.008;
      p.vx *= 0.72;
      p.vy *= 0.72;
      p.x += p.vx * 0.1;
      p.y += p.vy * 0.1;
      p.x = Math.max(24, Math.min(W - 24, p.x));
      p.y = Math.max(24, Math.min(H - 24, p.y));
    });
  }
  return pos;
}

export default function GoLivePage({ onExit }) {
  const [data, setData] = useState({ nodes: [], edges: [] });
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  // Default: show the critical/near-critical set; P3/P4 off to cut clutter.
  const [enabled, setEnabled] = useState({ 0: true, 1: true, 2: true, 3: false, 4: false });
  const [selected, setSelected] = useState(null);

  useEffect(() => {
    let alive = true;
    fetch('/api/beads/graph')
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return res.json();
      })
      .then((d) => {
        if (alive) setData({ nodes: d.nodes || [], edges: d.edges || [] });
      })
      .catch((e) => alive && setErr(String(e.message || e)))
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
    };
  }, []);

  const counts = useMemo(() => {
    const c = { 0: 0, 1: 0, 2: 0, 3: 0, 4: 0 };
    data.nodes.forEach((n) => {
      c[n.priority] = (c[n.priority] || 0) + 1;
    });
    return c;
  }, [data.nodes]);

  const { nodes, edges } = useMemo(() => {
    const keep = new Set(
      data.nodes.filter((n) => enabled[n.priority]).map((n) => n.id)
    );
    return {
      nodes: data.nodes.filter((n) => keep.has(n.id)),
      edges: data.edges.filter((e) => keep.has(e.from) && keep.has(e.to)),
    };
  }, [data, enabled]);

  const positions = useMemo(() => runForceLayout(nodes, edges), [nodes, edges]);

  const selectedNode = selected ? data.nodes.find((n) => n.id === selected) : null;
  const blockedBy = selected ? edges.filter((e) => e.to === selected) : [];
  const blocks = selected ? edges.filter((e) => e.from === selected) : [];
  const titleOf = (id) => data.nodes.find((n) => n.id === id)?.title || id;

  return (
    <div className="flex h-full w-full flex-col bg-bg text-zinc-100">
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-bg/95 px-4 py-3 backdrop-blur">
        <h1 className="text-sm font-semibold text-zinc-200">🚀 Go-Live tracker</h1>
        <button
          onClick={onExit}
          className="rounded-md border border-line px-3 py-1.5 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
        >
          ← Terminal
        </button>
      </header>

      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2 text-xs">
        <span className="text-zinc-500">{data.nodes.length} open · filter:</span>
        {[0, 1, 2, 3, 4].map((p) => {
          const t = prio(p);
          const on = enabled[p];
          return (
            <button
              key={p}
              onClick={() => setEnabled((e) => ({ ...e, [p]: !e[p] }))}
              className={[
                'flex items-center gap-1.5 rounded-full border px-2.5 py-1',
                on ? 'border-aura/40 text-zinc-100' : 'border-line text-zinc-500 opacity-60',
              ].join(' ')}
              title={`${t.name} — ${counts[p] || 0} open`}
            >
              <span className="inline-block h-2 w-2 rounded-full" style={{ background: t.hex }} />
              {t.label} ({counts[p] || 0})
            </button>
          );
        })}
      </div>

      <div className="flex min-h-0 flex-1 flex-col md:flex-row">
        <div className="min-h-0 flex-1 overflow-auto p-3">
          {loading && <div className="p-8 text-center text-sm text-zinc-500">loading graph…</div>}
          {err && (
            <div className="p-8 text-center text-sm text-red-400">
              Failed to load beads graph: {err}
            </div>
          )}
          {!loading && !err && nodes.length === 0 && (
            <div className="p-8 text-center text-sm text-zinc-500">
              No issues at the selected priorities. Toggle a chip above.
            </div>
          )}
          {!loading && !err && nodes.length > 0 && (
            <svg viewBox={`0 0 ${W} ${H}`} className="block w-full" style={{ minHeight: '50vh' }}>
              {edges.map((e, i) => {
                const a = positions[e.from];
                const b = positions[e.to];
                if (!a || !b) return null;
                // Treat every hierarchy variant (parent-child, parent,
                // parent-of) as dashed; blocks/blocked-by/related stay solid.
                const parentChild = (e.type || '').includes('parent');
                return (
                  <line
                    key={`${e.from}-${e.to}-${i}`}
                    x1={a.x}
                    y1={a.y}
                    x2={b.x}
                    y2={b.y}
                    stroke={parentChild ? '#334155' : '#475569'}
                    strokeWidth={1}
                    strokeDasharray={parentChild ? '3 3' : undefined}
                  />
                );
              })}
              {nodes.map((n) => {
                const p = positions[n.id];
                if (!p) return null;
                const t = prio(n.priority);
                const isEpic = n.type === 'epic';
                const r = isEpic ? 11 : 7;
                const isSel = n.id === selected;
                return (
                  <g
                    key={n.id}
                    onClick={() => setSelected(n.id === selected ? null : n.id)}
                    style={{ cursor: 'pointer' }}
                  >
                    <circle
                      cx={p.x}
                      cy={p.y}
                      r={r}
                      fill={t.hex}
                      opacity={isSel ? 1 : 0.85}
                      stroke={isSel ? '#e4e4e7' : isEpic ? '#e4e4e7' : 'none'}
                      strokeWidth={isSel ? 2.5 : isEpic ? 1 : 0}
                    />
                    <text
                      x={p.x}
                      y={p.y + r + 9}
                      textAnchor="middle"
                      fontSize="8"
                      fill={isSel ? '#e4e4e7' : '#a1a1aa'}
                      style={{ pointerEvents: 'none' }}
                    >
                      {n.id}
                    </text>
                  </g>
                );
              })}
            </svg>
          )}
        </div>

        {selectedNode && (
          <aside className="w-full shrink-0 overflow-auto border-t border-line p-4 text-xs md:w-80 md:border-l md:border-t-0">
            <div className="mb-2 flex items-center gap-2">
              <span
                className="inline-block h-2.5 w-2.5 rounded-full"
                style={{ background: prio(selectedNode.priority).hex }}
              />
              <span className="font-mono text-zinc-400">{selectedNode.id}</span>
              <span className="ml-auto rounded border border-line px-1.5 py-0.5 text-[10px] text-zinc-400">
                {selectedNode.type} · {selectedNode.status}
              </span>
            </div>
            <div className="mb-3 text-sm text-zinc-100">{selectedNode.title}</div>

            <Section title={`Blocked by / parent (${blockedBy.length})`} items={blockedBy.map((e) => ({ id: e.from, type: e.type }))} titleOf={titleOf} />
            <Section title={`Blocks / children (${blocks.length})`} items={blocks.map((e) => ({ id: e.to, type: e.type }))} titleOf={titleOf} />

            <button
              onClick={() => setSelected(null)}
              className="mt-3 rounded border border-line px-2 py-1 text-[11px] text-zinc-400 hover:text-zinc-200"
            >
              Clear selection
            </button>
          </aside>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-3 border-t border-line px-4 py-2 text-[10px] text-zinc-400">
        {[0, 1, 2, 3, 4].map((p) => (
          <span key={p} className="inline-flex items-center gap-1">
            <span className="inline-block h-2 w-2 rounded-full" style={{ background: prio(p).hex }} />
            {prio(p).label}
          </span>
        ))}
        <span className="inline-flex items-center gap-1">
          <span className="inline-block h-2 w-2 rounded-full border border-zinc-300" /> epic
        </span>
        <span className="ml-auto text-zinc-500">solid = blocks · dashed = parent/child · tap a node</span>
      </div>
    </div>
  );
}

function Section({ title, items, titleOf }) {
  return (
    <div className="mb-3">
      <div className="mb-1 text-[10px] font-semibold uppercase tracking-wider text-zinc-500">{title}</div>
      {items.length === 0 ? (
        <div className="text-zinc-600">none</div>
      ) : (
        <ul className="space-y-1">
          {items.map((it, i) => (
            <li key={`${it.id}-${i}`} className="truncate">
              <span className="font-mono text-zinc-400">{it.id}</span>{' '}
              <span className="text-zinc-300">{titleOf(it.id)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
