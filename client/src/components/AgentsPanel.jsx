import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

// AgentsPanel is the inline view for what `claude agents` are running on the
// same Mac as aurex. There are two data sources:
//
//   - "gc" (default when available): Gas City supervisor at /gc-api/*. Lists
//     real running sessions (id, pool, state, title) plus the static agent
//     roster (templates). Live updates via SSE on /gc-api/v0/events/stream.
//
//   - "claude": original aurex daemon view at /api/agents. Reads
//     ~/.claude/daemon/roster.json + ~/.claude/jobs/<id>/state.json. Polled
//     every 3s (or 30s when the tab is hidden).
//
// A small toggle at the top of the panel lets the user pick. Default is "gc"
// when the supervisor is up (probed at /gc-api/health); auto-fallback to
// "claude" when the supervisor is unreachable. The user can flip back
// manually at any time.
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
  ready: {
    label: 'ready',
    pill: 'bg-sky-500/15 text-sky-300 border-sky-400/40',
  },
  starting: {
    label: 'starting',
    pill: 'bg-sky-500/15 text-sky-300 border-sky-400/40 animate-pulse',
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
  suspended: {
    label: 'suspended',
    pill: 'bg-amber-500/15 text-amber-300 border-amber-400/40',
  },
  stopped: {
    label: 'stopped',
    pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30',
  },
  done: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  completed: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  success: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  finished: { label: 'done', pill: 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30' },
  failed: { label: 'failed', pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40' },
  'failed-create': {
    label: 'failed-create',
    pill: 'bg-rose-500/15 text-rose-300 border-rose-400/40',
  },
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

// secondsBetween returns whole-seconds elapsed from an ISO timestamp.
// Tolerant of missing/garbage values — returns null in those cases.
function secondsSince(iso) {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  return Math.max(0, Math.floor((Date.now() - t) / 1000));
}

// ---------------------------------------------------------------------------
// Claude Code (original) data source
// ---------------------------------------------------------------------------

// AgentLogView fetches and renders the `claude logs` output for one agent.
// Only used by the Claude Code source — Gas City has its own transcript
// endpoint we may wire up later, but for now the GC card links to /gc
// instead of inlining logs.
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

// ClaudeAgentCard renders one row from the claude daemon roster.
function ClaudeAgentCard({ agent, onStopped }) {
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
function useVisibilityAwarePoll(fn, enabled) {
  const ref = useRef(fn);
  ref.current = fn;

  useEffect(() => {
    if (!enabled) return undefined;
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
  }, [enabled]);
}

function ClaudeView() {
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

  useVisibilityAwarePoll(load, true);

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
          <ClaudeAgentCard key={agent.id} agent={agent} onStopped={load} />
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Gas City data source
// ---------------------------------------------------------------------------

// POOL_TONE colors the small "pool" / template tag, matching the persona
// badge palette IdeasPage uses for persona avatars (sky/violet/amber/etc.)
// so the surface feels consistent.
const POOL_TONE = {
  dog: 'bg-amber-500/15 text-amber-200 border-amber-500/40',
  persona: 'bg-violet-500/15 text-violet-200 border-violet-500/40',
  mayor: 'bg-sky-500/15 text-sky-200 border-sky-500/40',
  deacon: 'bg-rose-500/15 text-rose-200 border-rose-500/40',
};

function poolTone(pool) {
  if (!pool) return 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30';
  return POOL_TONE[String(pool).toLowerCase()] || 'bg-zinc-500/15 text-zinc-300 border-zinc-400/30';
}

// GcSessionCard renders one live Gas City session.
function GcSessionCard({ session }) {
  const state = session.state || (session.running ? 'running' : 'stopped');
  const tone = statusTone(state);
  const age = secondsSince(session.created_at);
  const pool = session.pool || session.template;

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
          className="font-mono text-[12px] text-zinc-200"
          title={session.id}
        >
          {session.session_name || session.id}
        </span>
        {pool && (
          <span
            className={classNames(
              'inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide',
              poolTone(pool)
            )}
          >
            {pool}
          </span>
        )}
        {session.provider && (
          <span className="rounded-full border border-line bg-bg px-2 py-0.5 text-[10px] text-zinc-400">
            {session.display_name || session.provider}
          </span>
        )}
      </header>

      {session.title && session.title !== pool && (
        <p
          className="mt-2 line-clamp-2 italic text-[13px] leading-snug text-zinc-400"
          title={session.title}
        >
          {session.title}
        </p>
      )}

      <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-zinc-500">
        {age != null && <span>started {formatAge(age)}</span>}
        {session.attached && (
          <span className="text-emerald-300/80">attached</span>
        )}
        {session.options?.effort && (
          <span>effort: {session.options.effort}</span>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2">
        <a
          href={`/gc/sessions/${encodeURIComponent(session.id)}`}
          target="_blank"
          rel="noreferrer"
          className="rounded-md border border-line bg-bg px-3 py-1.5 text-[12px] font-semibold text-zinc-200 hover:bg-zinc-800"
        >
          open in dashboard
        </a>
      </div>
    </article>
  );
}

// GcAgentCard renders one entry from the static agent roster (templates).
function GcAgentCard({ agent }) {
  const tone = statusTone(agent.state || (agent.running ? 'running' : 'stopped'));
  const pool = agent.pool || (agent.name?.split('-')[0]);

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
        <span className="font-mono text-[12px] text-zinc-200">{agent.name}</span>
        {pool && (
          <span
            className={classNames(
              'inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide',
              poolTone(pool)
            )}
          >
            {pool}
          </span>
        )}
        {agent.provider && (
          <span className="rounded-full border border-line bg-bg px-2 py-0.5 text-[10px] text-zinc-400">
            {agent.display_name || agent.provider}
          </span>
        )}
      </header>

      <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-zinc-500">
        {agent.suspended && <span className="text-amber-300">suspended</span>}
        {agent.available === false && <span className="text-rose-300">unavailable</span>}
      </div>
    </article>
  );
}

// useGcHealth probes the supervisor and returns
// { state: 'probing' | 'up' | 'down', error, lastChecked, retry }.
function useGcHealth() {
  const [state, setState] = useState('probing');
  const [error, setError] = useState(null);
  const [lastChecked, setLastChecked] = useState(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      setState((s) => (s === 'up' ? 'up' : 'probing'));
      try {
        const res = await fetch('/gc-api/health', { cache: 'no-store' });
        if (cancelled) return;
        if (!res.ok) {
          setError(`HTTP ${res.status}`);
          setState('down');
        } else {
          setError(null);
          setState('up');
        }
      } catch (e) {
        if (cancelled) return;
        setError(String(e));
        setState('down');
      } finally {
        if (!cancelled) setLastChecked(Date.now());
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [nonce]);

  return {
    state,
    error,
    lastChecked,
    retry: useCallback(() => setNonce((n) => n + 1), []),
  };
}

// SSE event types that should trigger a refresh of the GC view.
const GC_REFRESH_EVENTS = new Set([
  'controller.started',
  'controller.stopped',
  'order.fired',
  'order.completed',
  'order.failed',
  'session.state-changed',
  'session.created',
  'session.closed',
  'session.killed',
  'agent.state-changed',
  'agent.created',
  'agent.deleted',
]);

function GcView({ onSupervisorDown }) {
  const [city, setCity] = useState(null);
  const [sessions, setSessions] = useState([]);
  const [agents, setAgents] = useState([]);
  const [loadError, setLoadError] = useState(null);
  const [initialLoad, setInitialLoad] = useState(true);
  const [lastEvent, setLastEvent] = useState(null);

  // Refresh sequence: cities -> sessions + agents in parallel.
  const load = useCallback(async () => {
    try {
      const citiesRes = await fetch('/gc-api/v0/cities');
      if (!citiesRes.ok) {
        if (citiesRes.status >= 500 && onSupervisorDown) onSupervisorDown();
        throw new Error(`cities HTTP ${citiesRes.status}`);
      }
      const citiesJson = await citiesRes.json();
      const list = citiesJson.items || [];
      if (list.length === 0) {
        setCity(null);
        setSessions([]);
        setAgents([]);
        setLoadError(null);
        return;
      }
      const c = list.find((x) => x.name === 'gascity') || list[0];
      setCity(c);

      const [sRes, aRes] = await Promise.all([
        fetch(`/gc-api/v0/city/${encodeURIComponent(c.name)}/sessions`),
        fetch(`/gc-api/v0/city/${encodeURIComponent(c.name)}/agents`),
      ]);
      if (sRes.ok) {
        const sJson = await sRes.json();
        setSessions(sJson.items || []);
      }
      if (aRes.ok) {
        const aJson = await aRes.json();
        setAgents(aJson.items || []);
      }
      setLoadError(null);
    } catch (e) {
      setLoadError(String(e));
    } finally {
      setInitialLoad(false);
    }
  }, [onSupervisorDown]);

  useVisibilityAwarePoll(load, true);

  // SSE subscription. Reconnects automatically (EventSource does this by
  // default), but we also gate on document.visibility so phone screen-locks
  // don't keep a dead socket open for ages. When the tab becomes visible
  // again we tear down the old source (in case it was stale) and open a
  // fresh one.
  useEffect(() => {
    let es = null;
    let cancelled = false;

    const open = () => {
      if (cancelled) return;
      try {
        es = new EventSource('/gc-api/v0/events/stream');
        es.onmessage = (ev) => {
          let parsed = null;
          try {
            parsed = JSON.parse(ev.data);
          } catch {
            return;
          }
          setLastEvent({ type: parsed?.type, ts: parsed?.ts, seq: parsed?.seq });
          if (parsed?.type && GC_REFRESH_EVENTS.has(parsed.type)) {
            load();
          }
        };
        es.onerror = () => {
          // EventSource will auto-retry. Nothing to do, but if the supervisor
          // truly went down, the next /health probe will catch it and the
          // parent will flip the toggle.
        };
      } catch (e) {
        // Stream not supported (very old browser). Polling continues.
      }
    };

    const onVis = () => {
      if (document.visibilityState === 'visible') {
        if (es) {
          es.close();
          es = null;
        }
        open();
      } else if (es) {
        es.close();
        es = null;
      }
    };

    open();
    document.addEventListener('visibilitychange', onVis);
    return () => {
      cancelled = true;
      document.removeEventListener('visibilitychange', onVis);
      if (es) es.close();
    };
  }, [load]);

  const runningSessions = useMemo(
    () => sessions.filter((s) => s.running || isWorking(s.state)),
    [sessions]
  );

  if (!initialLoad && city == null) {
    return (
      <div className="rounded-xl border border-amber-500/40 bg-amber-500/10 p-6 text-center text-sm text-amber-100">
        <div className="font-semibold text-amber-200">No cities registered.</div>
        <p className="mt-2 text-[12px] leading-relaxed text-amber-200/80">
          Initialize one with{' '}
          <code className="text-amber-100">gc init</code>, then refresh this view.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-[12px]">
        <span className="inline-flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/10 px-2 py-0.5 font-semibold text-emerald-300">
          <span
            className={classNames(
              'h-1.5 w-1.5 rounded-full',
              runningSessions.length > 0 ? 'bg-emerald-400 animate-pulse' : 'bg-zinc-600'
            )}
          />
          {runningSessions.length} running
        </span>
        <span className="text-zinc-500">
          {sessions.length} sessions · {agents.length} agents
        </span>
        {city && (
          <span
            className="font-mono text-[11px] text-zinc-500"
            title={city.path}
          >
            {city.name}
          </span>
        )}
        {lastEvent?.type && (
          <span
            className="hidden sm:inline text-[10px] text-zinc-600"
            title={lastEvent.ts}
          >
            last event: {lastEvent.type}
          </span>
        )}
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
          Failed to load Gas City: {loadError}
        </div>
      )}

      {sessions.length > 0 && (
        <>
          <h3 className="mt-1 text-[11px] font-semibold uppercase tracking-wide text-zinc-500">
            Sessions
          </h3>
          <div className="space-y-3">
            {sessions.map((s) => (
              <GcSessionCard key={s.id} session={s} />
            ))}
          </div>
        </>
      )}

      {agents.length > 0 && (
        <>
          <h3 className="mt-4 text-[11px] font-semibold uppercase tracking-wide text-zinc-500">
            Agent roster
          </h3>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            {agents.map((a) => (
              <GcAgentCard key={a.name} agent={a} />
            ))}
          </div>
        </>
      )}

      {!initialLoad && sessions.length === 0 && agents.length === 0 && (
        <div className="rounded-xl border border-line bg-panel p-6 text-center text-sm text-zinc-400">
          <div className="text-zinc-200">No sessions or agents yet.</div>
          <p className="mt-2 text-[12px] leading-relaxed text-zinc-500">
            City <code className="text-zinc-300">{city?.name}</code> is up but the
            roster is empty. Bring up an agent with{' '}
            <code className="text-zinc-300">gc resume</code> or fire an order.
          </p>
        </div>
      )}
    </div>
  );
}

function GcSupervisorDownNotice({ error, lastChecked, onRetry }) {
  return (
    <div className="rounded-xl border border-amber-500/40 bg-amber-500/10 p-6 text-center text-sm text-amber-100">
      <div className="font-semibold text-amber-200">
        Gas City supervisor not reachable.
      </div>
      <p className="mt-2 text-[12px] leading-relaxed text-amber-200/80">
        Run <code className="text-amber-100">cd ~/gascity &amp;&amp; gc resume</code>{' '}
        on the Mac to start it, then retry.
      </p>
      {error && (
        <p className="mt-1 font-mono text-[11px] text-amber-200/60">{error}</p>
      )}
      <div className="mt-3 flex flex-wrap items-center justify-center gap-3 text-[11px] text-amber-200/70">
        <button
          type="button"
          onClick={onRetry}
          className="rounded-md border border-amber-400/50 bg-amber-500/20 px-3 py-1.5 font-semibold text-amber-100 hover:bg-amber-500/30"
        >
          retry
        </button>
        {lastChecked && (
          <span>checked {formatAge(Math.floor((Date.now() - lastChecked) / 1000))}</span>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Source toggle + top-level component
// ---------------------------------------------------------------------------

const SOURCE_STORAGE_KEY = 'aurex.agents.source';

function readStoredSource() {
  if (typeof window === 'undefined') return null;
  try {
    const v = window.localStorage.getItem(SOURCE_STORAGE_KEY);
    if (v === 'gc' || v === 'claude') return v;
  } catch {
    /* ignore */
  }
  return null;
}

function writeStoredSource(v) {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(SOURCE_STORAGE_KEY, v);
  } catch {
    /* ignore */
  }
}

export default function AgentsPanel() {
  // userChoice: explicit "gc" / "claude" picked from the toggle; null means
  // "let the probe decide". We persist the explicit choice so a return visit
  // doesn't re-fall-back surprisingly.
  const [userChoice, setUserChoice] = useState(() => readStoredSource());
  const health = useGcHealth();

  // Effective source: explicit choice wins. Otherwise default to gc while
  // probing or up, fall back to claude when supervisor is confirmed down.
  let source = userChoice;
  if (!source) {
    source = health.state === 'down' ? 'claude' : 'gc';
  }

  const pick = (next) => {
    setUserChoice(next);
    writeStoredSource(next);
  };

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-[11px]">
        <span className="text-zinc-500">Source:</span>
        <div className="inline-flex overflow-hidden rounded-md border border-line bg-bg">
          <button
            type="button"
            onClick={() => pick('gc')}
            className={classNames(
              'px-3 py-1 font-semibold',
              source === 'gc'
                ? 'bg-zinc-800 text-zinc-100'
                : 'text-zinc-400 hover:bg-zinc-900'
            )}
          >
            Gas City
            <span
              className={classNames(
                'ml-2 inline-block h-1.5 w-1.5 rounded-full',
                health.state === 'up'
                  ? 'bg-emerald-400'
                  : health.state === 'down'
                  ? 'bg-rose-400'
                  : 'bg-amber-400 animate-pulse'
              )}
              title={`supervisor ${health.state}`}
            />
          </button>
          <button
            type="button"
            onClick={() => pick('claude')}
            className={classNames(
              'border-l border-line px-3 py-1 font-semibold',
              source === 'claude'
                ? 'bg-zinc-800 text-zinc-100'
                : 'text-zinc-400 hover:bg-zinc-900'
            )}
          >
            Claude Code
          </button>
        </div>
        {userChoice && (
          <button
            type="button"
            onClick={() => {
              setUserChoice(null);
              try {
                window.localStorage.removeItem(SOURCE_STORAGE_KEY);
              } catch {
                /* ignore */
              }
            }}
            className="text-zinc-500 hover:text-zinc-300"
            title="clear explicit choice"
          >
            auto
          </button>
        )}
      </div>

      {source === 'gc' ? (
        health.state === 'down' ? (
          <GcSupervisorDownNotice
            error={health.error}
            lastChecked={health.lastChecked}
            onRetry={health.retry}
          />
        ) : (
          <GcView onSupervisorDown={health.retry} />
        )
      ) : (
        <ClaudeView />
      )}
    </div>
  );
}
