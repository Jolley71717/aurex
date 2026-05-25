import { useCallback, useEffect, useState } from 'react';

// PaperclipFleetPanel lists the 10 Paperclip persona agents (CEO/CTO/CMO/CFO/
// CPO/Planner/Lead Engineer/Engineer/UI Engineer/Janitor) with their current
// status + heartbeat interval, and gives each one a "Wake now" button that
// fires POST /api/paperclip-agents/{id}/wake. The Go side proxies to
// Paperclip's /api/agents/{id}/wakeup with the bearer cached at
// /tmp/pcp-token.
//
// Distinct from AgentsPanel.jsx — that one shows Claude Code's local sub-
// agent daemon. This one shows Paperclip personas. Both can be on screen
// at once; failure of one MCP/daemon shouldn't take out the other surface.

const STATUS_TONE = {
  idle: 'border-zinc-700 bg-zinc-900 text-zinc-300',
  running: 'border-aura/40 bg-aura/10 text-aura',
  error: 'border-red-500/50 bg-red-950/40 text-red-300',
  paused: 'border-amber-500/40 bg-amber-950/30 text-amber-300',
};

function fmtInterval(sec) {
  if (!sec) return '—';
  if (sec >= 3600) {
    const h = sec / 3600;
    return `${h % 1 === 0 ? h : h.toFixed(1)} h`;
  }
  if (sec >= 60) return `${Math.round(sec / 60)} m`;
  return `${sec} s`;
}

export default function PaperclipFleetPanel() {
  const [agents, setAgents] = useState([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  // Per-agent "is currently being woken" state so the button can show a spinner
  // without blocking the whole panel.
  const [wakingIds, setWakingIds] = useState(() => new Set());
  // Per-agent last-action message (queued / failed) shown next to the row for
  // a few seconds after the user taps Wake.
  const [flashes, setFlashes] = useState({});

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/paperclip-agents');
      const body = await res.json();
      if (!res.ok) {
        setErr(body.error || `HTTP ${res.status}`);
        setAgents([]);
      } else {
        setErr(null);
        setAgents(body.agents || []);
      }
    } catch (e) {
      setErr(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const id = setInterval(load, 8000);
    return () => clearInterval(id);
  }, [load]);

  const wake = useCallback(async (agent) => {
    setWakingIds((cur) => new Set(cur).add(agent.id));
    try {
      const res = await fetch(`/api/paperclip-agents/${agent.id}/wake`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      const body = await res.json().catch(() => ({}));
      const ok = res.status >= 200 && res.status < 300;
      const msg = ok
        ? `queued (${body.status || 'queued'})`
        : `failed: ${body.error || `HTTP ${res.status}`}`;
      setFlashes((cur) => ({ ...cur, [agent.id]: { ok, msg, at: Date.now() } }));
      // refresh the list so a status change shows up
      load();
    } catch (e) {
      setFlashes((cur) => ({ ...cur, [agent.id]: { ok: false, msg: String(e), at: Date.now() } }));
    } finally {
      setWakingIds((cur) => {
        const next = new Set(cur);
        next.delete(agent.id);
        return next;
      });
      // auto-clear flash after 5s
      setTimeout(() => {
        setFlashes((cur) => {
          if (!cur[agent.id]) return cur;
          const { [agent.id]: _, ...rest } = cur;
          return rest;
        });
      }, 5000);
    }
  }, [load]);

  if (loading) {
    return (
      <div className="px-4 py-6 text-sm text-zinc-500">Loading Paperclip fleet…</div>
    );
  }

  if (err) {
    return (
      <div className="mx-3 my-3 rounded border border-red-500/40 bg-red-950/30 p-3 text-sm text-red-200">
        <div className="font-semibold mb-1">Paperclip fleet unavailable</div>
        <div className="font-mono text-xs">{err}</div>
        <div className="mt-2 text-xs text-red-300/80">
          If the Paperclip server is running, check that <code className="bg-black/30 px-1 rounded">/tmp/pcp-token</code> exists and is mode 0600.
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-2 px-2 py-3">
      <div className="px-2 pb-2 text-xs uppercase tracking-wider text-zinc-500">
        Paperclip fleet · {agents.length} agents · tap Wake to fire an on-demand run
      </div>
      {agents.map((a) => {
        const waking = wakingIds.has(a.id);
        const flash = flashes[a.id];
        const tone = STATUS_TONE[a.status] || STATUS_TONE.idle;
        return (
          <div
            key={a.id}
            className="flex items-center gap-3 rounded-lg border border-line bg-panel px-3 py-2.5"
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="text-sm font-semibold text-zinc-100">{a.name}</span>
                <span className={`shrink-0 rounded-full border px-2 py-0.5 text-[10px] font-medium ${tone}`}>
                  {a.status || 'unknown'}
                </span>
              </div>
              <div className="mt-0.5 text-xs text-zinc-500">
                {a.role || '—'} · heartbeat {a.heartbeat_enabled ? fmtInterval(a.heartbeat_sec) : 'off'}
              </div>
              {flash && (
                <div className={`mt-1 text-xs ${flash.ok ? 'text-aura' : 'text-red-400'}`}>
                  {flash.msg}
                </div>
              )}
            </div>
            <button
              type="button"
              disabled={waking}
              onClick={() => wake(a)}
              className={`shrink-0 rounded-md border px-3 py-1.5 text-xs font-medium ${
                waking
                  ? 'border-zinc-700 bg-zinc-800 text-zinc-500'
                  : 'border-aura/50 bg-aura/15 text-aura active:bg-aura/30'
              }`}
            >
              {waking ? '…' : '⚡ Wake'}
            </button>
          </div>
        );
      })}
    </div>
  );
}
