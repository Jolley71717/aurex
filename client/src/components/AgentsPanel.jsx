import { useCallback, useEffect, useRef, useState } from 'react';

// AgentsPanel is the inline view for what `claude agents` is running on the
// same Mac as aurex. Polls /api/agents on a visibility-aware loop: 3s when
// the tab is visible, 30s when it's hidden. The backend reads
// ~/.claude/daemon/roster.json + ~/.claude/jobs/<id>/state.json on demand;
// there's no websocket so polling is the only path.
//
// Intended to be embedded as a fourth tab inside IdeasPage's tab strip
// alongside "Inbox", "Connections", "Personas". Self-contained — no props
// required — so it can also be dropped into any future tab host without
// changes.
//
// Mobile-first: every card collapses to one column on iPhone Safari at
// 375pt. Action buttons stay tall enough (h-10) to be tappable, and the
// log viewer is a <pre> with overflow-x-auto.

const STATUS_TONE = {
  working: {
    label: 'working',
    pill: 'bg-emerald-500/15 text-emerald-300 border-emerald-400/40 animate-aura',
  },
  running: {
    label: 'running',
    pill: 'bg-emerald-500/15 text-emerald-300 border-emerald-400/40 animate-aura',
  },
  active: {
    label: 'active',
    pill: 'bg-emerald-500/15 text-emerald-300 border-emerald-400/40 animate-aura',
  },
  'needs-input': {
    label: 'needs input',
    pill: 'bg-amber-500/15 text-amber-300 border-amber-400/40',
  },
  needs_input: {
    label: 'needs input',
    pill: 'bg-amber-500/15 text-amber-300 border-amber-400/40',
  },
  'input-required': {
    label: 'needs input',
    pill: 'bg-amber-500/15 text-amber-300 border-amber-400/40',
  },
  done: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  completed: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  success: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  finished: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  failed: { label: 'failed', pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40' },
  error: { label: 'failed', pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40' },
  errored: { label: 'failed', pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40' },
  crashed: { label: 'failed', pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40' },
  unparseable: {
    label: 'unparseable',
    pill: 'bg-zinc-700/40 text-zinc-400 border-zinc-600',
  },
};

const DEFAULT_TONE = {
  label: 'unknown',
  pill: 'bg-zinc-700/40 text-zinc-400 border-zinc-600',
};

function classNames(...xs) {
  return xs.filter(Boolean).join(' ');
}

function statusTone(status) {
  return STATUS_TONE[String(status || '').toLowerCase()] || DEFAULT_TONE;
}

function isWorking(status) {
  const s = String(status || '').toLowerCase();
  return s === 'working' || s === 'running' || s === 'active';
}

// formatAge turns a seconds-ago integer into a compact "5s / 12m / 3h" label.
function formatAge(seconds) {
  if (!seconds || seconds < 0) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

// AgentLogView fetches and renders the `claude logs` output for one agent.
// Re-renders on demand via the refresh callback; the parent owns the open
// state so collapsing keeps the panel small on mobile.
function AgentLogView({ agentId }) {
  const [text, setText] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agentId)}/log?lines=200`
      );
      const body = await res.text();
      if (!res.ok) {
        setError(`HTTP ${res.status}: ${body.slice(0, 300)}`);
      } else {
        setText(body);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="mt-3 rounded-md border border-line bg-bg">
      <div className="flex items-center justify-between border-b border-line px-3 py-1.5 text-[11px] uppercase tracking-wide text-zinc-500">
        <span>last 200 lines</span>
        <button
          type="button"
          onClick={load}
          disabled={loading}
          className="rounded px-2 py-0.5 text-zinc-300 hover:bg-zinc-800 disabled:opacity-50"
        >
          {loading ? '…' : 'refresh'}
        </button>
      </div>
      {error ? (
        <div className="p-3 text-[12px] text-rose-300">{error}</div>
      ) : (
        <pre className="max-h-72 overflow-x-auto overflow-y-auto p-3 font-mono text-[11px] leading-snug text-zinc-200">
          {text || (loading ? 'loading…' : '(no output)')}
        </pre>
      )}
    </div>
  );
}

// AgentCard renders one row. Owns its own "logs open" + "confirm stop" UI
// state so the parent list can re-render on the 3s poll without trashing
// the local interaction state.
function AgentCard({ agent, onStopped }) {
  const tone = statusTone(agent.status);
  const [showLogs, setShowLogs] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState(null);

  const shortId = agent.id ? agent.id.slice(0, 8) : '(no id)';

  const handleStop = async () => {
    setStopping(true);
    setStopError(null);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agent.id)}/stop`,
        { method: 'POST' }
      );
      if (!res.ok) {
        const body = await res.text();
        setStopError(`HTTP ${res.status}: ${body.slice(0, 200)}`);
      } else {
        setConfirming(false);
        if (onStopped) onStopped(agent.id);
      }
    } catch (e) {
      setStopError(String(e));
    } finally {
      setStopping(false);
    }
  };

  return (
    <article className="rounded-xl border border-line bg-panel p-4 shadow-sm">
      <header className="flex flex-wrap items-center gap-2">
        <span
          className={classNames(
            'inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide',
            tone.pill
          )}
        >
          {tone.label}
        </span>
        <span
          className="font-mono text-[11px] text-zinc-400"
          title={agent.id}
        >
          {agent.label || shortId}
        </span>
        {agent.label && (
          <span className="font-mono text-[10px] text-zinc-600" title={agent.id}>
            ({shortId})
          </span>
        )}
        {agent.parent_session_id && (
          <span
            className="rounded-full border border-line bg-bg px-2 py-0.5 font-mono text-[10px] text-zinc-400"
            title={`parent ${agent.parent_session_id}`}
          >
            parent: {agent.parent_session_id.slice(0, 6)}
          </span>
        )}
      </header>

      {agent.summary && (
        <p
          className="mt-2 line-clamp-2 italic text-[13px] leading-snug text-zinc-400"
          title={agent.summary}
        >
          {agent.summary}
        </p>
      )}

      <div className="mt-2 text-[11px] text-zinc-500">
        started {formatAge(agent.age_seconds)}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => setShowLogs((v) => !v)}
          className="rounded-md border border-line bg-bg px-3 py-1.5 text-[12px] font-semibold text-zinc-200 hover:bg-zinc-800"
        >
          {showLogs ? 'hide logs' : 'view logs'}
        </button>
        {isWorking(agent.status) && !confirming && (
          <button
            type="button"
            onClick={() => setConfirming(true)}
            disabled={stopping}
            className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-1.5 text-[12px] font-semibold text-rose-200 hover:bg-rose-500/20 disabled:opacity-50"
          >
            stop
          </button>
        )}
        {confirming && (
          <span className="inline-flex items-center gap-2 rounded-md border border-rose-500/40 bg-rose-500/5 px-2 py-1 text-[11px] text-rose-200">
            stop {shortId}?
            <button
              type="button"
              onClick={handleStop}
              disabled={stopping}
              className="rounded bg-rose-500 px-2 py-0.5 font-semibold text-zinc-950 hover:bg-rose-400 disabled:opacity-50"
            >
              {stopping ? '…' : 'yes'}
            </button>
            <button
              type="button"
              onClick={() => setConfirming(false)}
              className="rounded border border-line px-2 py-0.5 text-zinc-300 hover:bg-zinc-800"
            >
              cancel
            </button>
          </span>
        )}
      </div>
      {stopError && (
        <div className="mt-2 rounded-md border border-rose-500/40 bg-rose-500/10 p-2 text-[11px] text-rose-200">
          {stopError}
        </div>
      )}

      {showLogs && <AgentLogView agentId={agent.id} />}
    </article>
  );
}

// useVisibilityAwarePoll fires `fn` on an interval that's 3s when the page is
// visible and 30s when it isn't. Re-fires immediately on tab refocus so the
// list catches up after the user comes back. Returns nothing — the caller
// keeps its own state.
function useVisibilityAwarePoll(fn) {
  // Hold the latest fn in a ref so the interval doesn't restart on every
  // parent render (parent renders are frequent — they happen on every poll).
  const ref = useRef(fn);
  ref.current = fn;

  useEffect(() => {
    let cancelled = false;
    let timer = null;

    const tick = () => {
      if (cancelled) return;
      ref.current();
    };

    const scheduleNext = () => {
      if (timer) clearInterval(timer);
      const ms = document.visibilityState === 'visible' ? 3000 : 30000;
      timer = setInterval(tick, ms);
    };

    const onVis = () => {
      tick();
      scheduleNext();
    };

    tick();
    scheduleNext();
    document.addEventListener('visibilitychange', onVis);
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
      document.removeEventListener('visibilitychange', onVis);
    };
  }, []);
}

export default function AgentsPanel() {
  const [data, setData] = useState({
    agents: [],
    claudeCliAvailable: true,
    dataRoot: '',
    error: null,
  });
  const [loadError, setLoadError] = useState(null);
  const [initialLoad, setInitialLoad] = useState(true);

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/agents');
      if (!res.ok) {
        setLoadError(`HTTP ${res.status}`);
        setInitialLoad(false);
        return;
      }
      const json = await res.json();
      setData({
        agents: json.agents || [],
        claudeCliAvailable: json.claudeCliAvailable !== false,
        dataRoot: json.dataRoot || '',
        error: json.error || null,
      });
      setLoadError(null);
    } catch (e) {
      setLoadError(String(e));
    } finally {
      setInitialLoad(false);
    }
  }, []);

  useVisibilityAwarePoll(load);

  const workingCount = data.agents.filter((a) => isWorking(a.status)).length;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-[12px]">
        <span className="inline-flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/10 px-2 py-0.5 font-semibold text-emerald-300">
          <span
            className={classNames(
              'h-1.5 w-1.5 rounded-full',
              workingCount > 0 ? 'bg-emerald-400 animate-pulse' : 'bg-zinc-600'
            )}
          />
          {workingCount} working
        </span>
        <span className="text-zinc-500">{data.agents.length} total</span>
        <button
          type="button"
          onClick={load}
          className="ml-auto rounded-md border border-line bg-bg px-2 py-1 text-[11px] text-zinc-300 hover:bg-zinc-800"
        >
          refresh
        </button>
      </div>

      {loadError && (
        <div className="rounded-md border border-rose-500/40 bg-rose-500/10 p-3 text-[12px] text-rose-200">
          Failed to load agents: {loadError}
        </div>
      )}

      {data.error && (
        <div className="rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-[12px] text-amber-200">
          {data.error}
        </div>
      )}

      {!initialLoad && data.agents.length === 0 && data.claudeCliAvailable && (
        <div className="rounded-xl border border-line bg-panel p-6 text-center text-sm text-zinc-400">
          <div className="text-zinc-200">No agents running.</div>
          <p className="mt-2 text-[12px] leading-relaxed text-zinc-500">
            Aurex sees the daemon at{' '}
            <code className="text-zinc-300">{data.dataRoot || '~/.claude'}</code>. New
            agents started from any <code className="text-zinc-300">claude</code> session
            on this Mac will appear here within 3 seconds.
          </p>
        </div>
      )}

      {!initialLoad && !data.claudeCliAvailable && (
        <div className="rounded-xl border border-amber-500/40 bg-amber-500/10 p-6 text-center text-sm text-amber-100">
          <div className="font-semibold text-amber-200">Claude Code CLI not found.</div>
          <p className="mt-2 text-[12px] leading-relaxed text-amber-200/80">
            Run <code className="text-amber-100">which claude</code> from a terminal —
            aurex needs <code className="text-amber-100">claude</code> in its PATH to
            read agent state and to support stop/log actions.
          </p>
        </div>
      )}

      <div className="space-y-3">
        {data.agents.map((agent) => (
          <AgentCard key={agent.id} agent={agent} onStopped={load} />
        ))}
      </div>
    </div>
  );
}
