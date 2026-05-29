import { useCallback, useEffect, useMemo, useState } from 'react';

// SettingsPage — the home for all aurex configuration. Today the only
// section is Connectors (the typed registry of external systems this
// instance integrates with — Paperclip, Beads, Gas City, Ollama, etc.).
// Profile and Push live in their own panels for now; this file is the
// scaffold to bring them in later under one Settings roof.
//
// Mobile-first: every form field stacks vertically, tap targets are >=44pt,
// the connector cards collapse to one column under 600pt.

const TYPE_OPTIONS = [
  { value: 'paperclip', label: 'Paperclip', defaultURL: 'http://localhost:3100', tokenLabel: 'Token file path' },
  { value: 'beads',     label: 'Beads',     defaultURL: '',                       tokenLabel: '(not required)' },
  { value: 'gascity',   label: 'Gas City',  defaultURL: 'http://localhost:8372',  tokenLabel: '(not required)' },
  { value: 'ollama',    label: 'Ollama',    defaultURL: 'http://192.168.0.40:11434', tokenLabel: '(not required)' },
  { value: 'hubspace',  label: 'Hubspace',  defaultURL: 'http://127.0.0.1:8523',  tokenLabel: 'Token file path' },
];

const CAPABILITY_OPTIONS = ['agents', 'issues', 'ideas', 'llm', 'dashboard', 'supervisor', 'control', 'sensor', 'schedule'];

const STATUS_TONE = {
  healthy:        'border-emerald-500/40 bg-emerald-500/10 text-emerald-300',
  down:           'border-red-500/40 bg-red-500/10 text-red-300',
  unauthorized:   'border-amber-500/40 bg-amber-500/10 text-amber-300',
  misconfigured:  'border-amber-500/40 bg-amber-500/10 text-amber-300',
  disabled:       'border-zinc-700 bg-zinc-900 text-zinc-400',
  unsupported_type: 'border-red-500/40 bg-red-500/10 text-red-300',
  not_found:      'border-red-500/40 bg-red-500/10 text-red-300',
  '':             'border-zinc-700 bg-zinc-900 text-zinc-400',
};

function fmtTimestamp(unix) {
  if (!unix) return 'never';
  const ago = Math.max(0, Math.floor((Date.now() / 1000) - unix));
  if (ago < 60) return `${ago}s ago`;
  if (ago < 3600) return `${Math.floor(ago / 60)}m ago`;
  if (ago < 86400) return `${Math.floor(ago / 3600)}h ago`;
  return `${Math.floor(ago / 86400)}d ago`;
}

export default function SettingsPage({ onExit }) {
  const [connectors, setConnectors] = useState([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState(null); // connector being edited, or null
  const [savingIds, setSavingIds] = useState(new Set());

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/connectors');
      const body = await res.json();
      if (!res.ok) {
        setErr(body.error || `HTTP ${res.status}`);
        setConnectors([]);
      } else {
        setErr(null);
        setConnectors(body.connectors || []);
      }
    } catch (e) {
      setErr(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    // Poll for fresh health every 30s — backend refreshes every 60s so this
    // is responsive without being noisy.
    const id = setInterval(load, 30000);
    return () => clearInterval(id);
  }, [load]);

  const probe = useCallback(async (id) => {
    setSavingIds((s) => new Set(s).add(id));
    try {
      await fetch(`/api/connectors/${id}/test`);
      await load();
    } finally {
      setSavingIds((s) => {
        const next = new Set(s);
        next.delete(id);
        return next;
      });
    }
  }, [load]);

  const toggleEnabled = useCallback(async (c) => {
    setSavingIds((s) => new Set(s).add(c.id));
    try {
      await fetch(`/api/connectors/${c.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...c, enabled: !c.enabled }),
      });
      await load();
    } finally {
      setSavingIds((s) => {
        const next = new Set(s);
        next.delete(c.id);
        return next;
      });
    }
  }, [load]);

  const remove = useCallback(async (c) => {
    if (!confirm(`Remove the "${c.name}" connector? This only removes it from aurex — the upstream system is untouched.`)) return;
    await fetch(`/api/connectors/${c.id}`, { method: 'DELETE' });
    await load();
  }, [load]);

  if (loading) {
    return <div className="p-6 text-sm text-zinc-500">Loading settings…</div>;
  }

  return (
    <div className="flex h-full w-full flex-col bg-bg text-zinc-100">
      <header className="flex shrink-0 items-center gap-3 border-b border-line bg-panel px-4 py-3">
        <button
          onClick={onExit}
          onMouseDown={(e) => e.preventDefault()}
          onTouchStart={(e) => e.preventDefault()}
          className="rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
          aria-label="Back"
        >
          ←
        </button>
        <h1 className="text-base font-semibold tracking-tight">⚙ Settings</h1>
      </header>

      <main className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
        <section>
          <div className="mb-3 flex items-center justify-between">
            <div>
              <h2 className="text-sm font-semibold text-zinc-200">Connectors</h2>
              <p className="mt-0.5 text-xs text-zinc-500">
                External systems aurex talks to. Tokens are stored by file reference, never inline. Health re-checked every 60s.
              </p>
            </div>
            {!adding && (
              <button
                onClick={() => setAdding(true)}
                className="shrink-0 rounded-md border border-aura/40 bg-aura/10 px-3 py-1.5 text-xs font-medium text-aura active:bg-aura/25"
              >
                + Add connector
              </button>
            )}
          </div>

          {err && (
            <div className="mb-3 rounded border border-red-500/40 bg-red-950/30 p-3 text-sm text-red-200">
              <div className="font-semibold mb-1">Connectors unavailable</div>
              <div className="font-mono text-xs">{err}</div>
            </div>
          )}

          {adding && (
            <ConnectorForm
              onCancel={() => setAdding(false)}
              onSave={async (c) => {
                await fetch('/api/connectors', {
                  method: 'POST',
                  headers: { 'Content-Type': 'application/json' },
                  body: JSON.stringify(c),
                });
                setAdding(false);
                await load();
              }}
            />
          )}

          <div className="flex flex-col gap-2">
            {connectors.map((c) => (
              editing === c.id ? (
                <ConnectorForm
                  key={c.id}
                  existing={c}
                  onCancel={() => setEditing(null)}
                  onSave={async (next) => {
                    await fetch(`/api/connectors/${c.id}`, {
                      method: 'PATCH',
                      headers: { 'Content-Type': 'application/json' },
                      body: JSON.stringify(next),
                    });
                    setEditing(null);
                    await load();
                  }}
                />
              ) : (
                <ConnectorRow
                  key={c.id}
                  connector={c}
                  saving={savingIds.has(c.id)}
                  onProbe={() => probe(c.id)}
                  onToggle={() => toggleEnabled(c)}
                  onEdit={() => setEditing(c.id)}
                  onRemove={() => remove(c)}
                />
              )
            ))}
            {connectors.length === 0 && !adding && (
              <div className="rounded border border-dashed border-line p-4 text-center text-sm text-zinc-500">
                No connectors registered yet. Tap “+ Add connector” to register Paperclip, Beads, or another system.
              </div>
            )}
          </div>
        </section>
      </main>
    </div>
  );
}

function ConnectorRow({ connector, saving, onProbe, onToggle, onEdit, onRemove }) {
  const tone = STATUS_TONE[connector.health?.status || ''] || STATUS_TONE[''];
  return (
    <div className="rounded-lg border border-line bg-panel p-3">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="text-sm font-semibold text-zinc-100">{connector.name}</span>
            <span className="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-zinc-400">
              {connector.type}
            </span>
            <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${tone}`}>
              {connector.health?.status || 'unknown'}
            </span>
          </div>
          {connector.url && (
            <div className="mt-1 truncate font-mono text-xs text-zinc-500" title={connector.url}>
              {connector.url}
            </div>
          )}
          {connector.token_ref && (
            <div className="mt-0.5 truncate font-mono text-[11px] text-zinc-600" title={connector.token_ref}>
              token: {connector.token_ref}
            </div>
          )}
          <div className="mt-1 flex flex-wrap items-center gap-2 text-[11px] text-zinc-500">
            <span>checked {fmtTimestamp(connector.health?.updated_unix)}</span>
            {(connector.capabilities || []).length > 0 && (
              <span>· {connector.capabilities.join(', ')}</span>
            )}
          </div>
          {connector.health?.detail && (
            <div className="mt-1 truncate text-[11px] text-zinc-500" title={connector.health.detail}>
              {connector.health.detail}
            </div>
          )}
        </div>
        <div className="flex flex-col gap-1">
          <button
            disabled={saving}
            onClick={onProbe}
            className="rounded border border-line bg-bg px-2 py-1 text-xs text-zinc-300 active:bg-zinc-800 disabled:opacity-50"
          >
            {saving ? '…' : 'Test'}
          </button>
          <button
            onClick={onToggle}
            className={[
              'rounded border px-2 py-1 text-xs',
              connector.enabled
                ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300'
                : 'border-line bg-bg text-zinc-400',
            ].join(' ')}
          >
            {connector.enabled ? 'On' : 'Off'}
          </button>
        </div>
      </div>
      <div className="mt-2 flex gap-2 border-t border-line pt-2">
        <button onClick={onEdit} className="text-[11px] text-zinc-400 hover:text-aura">Edit</button>
        <span className="text-zinc-700">·</span>
        <button onClick={onRemove} className="text-[11px] text-zinc-400 hover:text-red-400">Remove</button>
      </div>
    </div>
  );
}

function ConnectorForm({ existing, onCancel, onSave }) {
  const editing = !!existing;
  const [type, setType] = useState(existing?.type || 'paperclip');
  const [name, setName] = useState(existing?.name || '');
  const [url, setUrl] = useState(existing?.url || '');
  const [tokenRef, setTokenRef] = useState(existing?.token_ref || '');
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [caps, setCaps] = useState(new Set(existing?.capabilities || []));

  const typeMeta = useMemo(() => TYPE_OPTIONS.find((o) => o.value === type) || TYPE_OPTIONS[0], [type]);

  // When type changes for a NEW connector, pre-fill the URL with the
  // type-typical default so the user doesn't have to remember localhost:3100.
  useEffect(() => {
    if (!editing && !url) setUrl(typeMeta.defaultURL);
    // Suggest a name if it's empty too.
    if (!editing && !name) setName(typeMeta.label);
  }, [type, editing]);  // eslint-disable-line react-hooks/exhaustive-deps

  const toggleCap = (c) => {
    setCaps((s) => {
      const next = new Set(s);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });
  };

  const submit = (e) => {
    e.preventDefault();
    onSave({
      type,
      name: name.trim(),
      url: url.trim(),
      token_ref: tokenRef.trim(),
      enabled,
      capabilities: Array.from(caps),
    });
  };

  return (
    <form onSubmit={submit} className="mb-3 rounded-lg border border-aura/30 bg-aura/5 p-3">
      <div className="mb-2 text-xs font-semibold uppercase tracking-wider text-aura">
        {editing ? `Edit "${existing.name}"` : 'New connector'}
      </div>

      <label className="block text-xs text-zinc-400">
        Type
        <select
          disabled={editing}
          value={type}
          onChange={(e) => setType(e.target.value)}
          className="mt-1 block w-full rounded border border-line bg-bg px-2 py-1.5 text-sm text-zinc-100 disabled:opacity-60"
        >
          {TYPE_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
      </label>

      <label className="mt-2 block text-xs text-zinc-400">
        Name
        <input
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
          className="mt-1 block w-full rounded border border-line bg-bg px-2 py-1.5 text-sm text-zinc-100"
        />
      </label>

      <label className="mt-2 block text-xs text-zinc-400">
        URL or path
        <input
          type="text"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder={typeMeta.defaultURL}
          className="mt-1 block w-full rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs text-zinc-100"
        />
      </label>

      <label className="mt-2 block text-xs text-zinc-400">
        {typeMeta.tokenLabel}
        <input
          type="text"
          value={tokenRef}
          onChange={(e) => setTokenRef(e.target.value)}
          placeholder={type === 'paperclip' ? '/tmp/pcp-token' : ''}
          className="mt-1 block w-full rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs text-zinc-100"
        />
        <span className="mt-0.5 block text-[10px] text-zinc-500">
          Path to a file (mode 0600) — aurex reads the token at probe time. Never inline.
        </span>
      </label>

      <fieldset className="mt-2">
        <legend className="text-xs text-zinc-400">Capabilities</legend>
        <div className="mt-1 flex flex-wrap gap-1.5">
          {CAPABILITY_OPTIONS.map((cap) => (
            <label
              key={cap}
              className={[
                'cursor-pointer rounded border px-2 py-0.5 text-[11px]',
                caps.has(cap)
                  ? 'border-aura/40 bg-aura/15 text-aura'
                  : 'border-line bg-bg text-zinc-400',
              ].join(' ')}
            >
              <input
                type="checkbox"
                className="hidden"
                checked={caps.has(cap)}
                onChange={() => toggleCap(cap)}
              />
              {cap}
            </label>
          ))}
        </div>
      </fieldset>

      <label className="mt-2 flex items-center gap-2 text-xs text-zinc-400">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
        />
        Enabled (health probes run; surfaces consume)
      </label>

      <div className="mt-3 flex gap-2">
        <button
          type="submit"
          className="rounded-md border border-aura/40 bg-aura/15 px-3 py-1.5 text-xs font-medium text-aura active:bg-aura/30"
        >
          {editing ? 'Save changes' : 'Add connector'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="rounded-md border border-line bg-bg px-3 py-1.5 text-xs text-zinc-300 active:bg-zinc-800"
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
