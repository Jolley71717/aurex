import { useCallback, useEffect, useMemo, useState } from 'react';

// WateringPage renders the Hubspace water-timer fleet: per-device cards with
// battery + rain-delay + per-spigot state and manual run/stop, the list of
// existing recurring schedules, and a form to create a new one. Everything
// proxies through /api/hubspace/* to the local Python sidecar daemon (which
// wraps the extended aioafero fork). If the sidecar isn't running the calls
// return 503 and we show a "start the sidecar" hint.

const DAYS = ['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN'];

function localTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

async function api(path, opts) {
  const res = await fetch(`/api/hubspace${path}`, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(body.error || body.detail || `HTTP ${res.status}`);
  }
  return body;
}

export default function WateringPage({ onExit }) {
  const [devices, setDevices] = useState([]);
  const [schedules, setSchedules] = useState([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(null); // freeform "what's in flight" label

  const load = useCallback(async () => {
    try {
      const [d, s] = await Promise.all([api('/devices'), api('/schedules')]);
      setDevices(d.devices || []);
      setSchedules(s.schedules || []);
      setErr(null);
    } catch (e) {
      setErr(String(e.message || e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const id = setInterval(load, 15000);
    return () => clearInterval(id);
  }, [load]);

  const act = useCallback(
    async (label, fn) => {
      setBusy(label);
      try {
        await fn();
        await load();
      } catch (e) {
        setErr(String(e.message || e));
      } finally {
        setBusy(null);
      }
    },
    [load],
  );

  const runSpigot = (dev, inst) =>
    act(`run ${inst}`, () => api(`/devices/${dev}/spigot/${inst}/run`, { method: 'POST' }));
  const stopSpigot = (dev, inst) =>
    act(`stop ${inst}`, () => api(`/devices/${dev}/spigot/${inst}/stop`, { method: 'POST' }));
  const deleteSchedule = (ruleId) =>
    act(`delete ${ruleId}`, () => api(`/schedules/${ruleId}`, { method: 'DELETE' }));

  return (
    <div className="flex h-full w-full flex-col overflow-y-auto bg-bg text-zinc-100">
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-bg/95 px-4 py-3 backdrop-blur">
        <h1 className="text-sm font-semibold text-zinc-200">💧 Watering</h1>
        <button
          onClick={onExit}
          className="rounded-md border border-line px-3 py-1.5 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
        >
          ← Terminal
        </button>
      </header>

      {err && (
        <div className="mx-4 mt-4 rounded-md border border-red-500/40 bg-red-950/30 px-3 py-2 text-xs text-red-300">
          {err}
          {/HTTP 503|sidecar token/.test(err) && (
            <div className="mt-1 text-red-400/80">
              The Hubspace sidecar may not be running — start it with{' '}
              <code className="text-red-200">python -m sidecar</code>.
            </div>
          )}
        </div>
      )}
      {busy && <div className="px-4 pt-2 text-[11px] text-zinc-500">working: {busy}…</div>}

      {loading ? (
        <div className="flex flex-1 items-center justify-center text-sm text-zinc-500">loading…</div>
      ) : (
        <div className="space-y-6 p-4">
          <DevicesSection devices={devices} onRun={runSpigot} onStop={stopSpigot} />
          <SchedulesSection schedules={schedules} devices={devices} onDelete={deleteSchedule} />
          <NewScheduleForm devices={devices} onCreated={load} setErr={setErr} />
        </div>
      )}
    </div>
  );
}

function DevicesSection({ devices, onRun, onStop }) {
  if (!devices.length) {
    return <p className="text-xs text-zinc-500">No water timers found.</p>;
  }
  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-zinc-500">Timers</h2>
      <div className="grid gap-3 sm:grid-cols-2">
        {devices.map((d) => (
          <div key={d.id} className="rounded-lg border border-line bg-zinc-900/40 p-3">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium text-zinc-200">{d.name}</span>
              <span className="text-[11px] text-zinc-500">
                🔋 {d.battery ?? '—'}%{d.rain_delay?.active ? ' · 🌧 rain delay' : ''}
              </span>
            </div>
            <div className="mt-2 space-y-1">
              {Object.values(d.spigots || {}).map((sp) => (
                <div key={sp.instance} className="flex items-center justify-between gap-2 text-xs">
                  <span className="text-zinc-300">
                    {sp.instance}
                    <span className={sp.open ? 'text-aura' : 'text-zinc-600'}>
                      {' '}
                      {sp.open ? '● on' : '○ off'}
                    </span>
                    {sp.max_on_time ? (
                      <span className="text-zinc-600"> · max {sp.max_on_time}m</span>
                    ) : null}
                  </span>
                  <span className="flex gap-1">
                    <button
                      onClick={() => onRun(d.id, sp.instance)}
                      className="rounded border border-line px-2 py-0.5 text-[11px] text-zinc-300 hover:border-aura/40 hover:text-aura"
                    >
                      Run
                    </button>
                    <button
                      onClick={() => onStop(d.id, sp.instance)}
                      className="rounded border border-line px-2 py-0.5 text-[11px] text-zinc-300 hover:border-red-500/40 hover:text-red-300"
                    >
                      Stop
                    </button>
                  </span>
                </div>
              ))}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function SchedulesSection({ schedules, devices, onDelete }) {
  const nameFor = useMemo(() => {
    const m = {};
    for (const d of devices) m[d.id] = d.name;
    return m;
  }, [devices]);

  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-zinc-500">
        Schedules
      </h2>
      {!schedules.length ? (
        <p className="text-xs text-zinc-500">No schedules yet.</p>
      ) : (
        <ul className="space-y-1">
          {schedules.map((s) => {
            const t = s.schedule?.time;
            const when = s.schedule
              ? `${(s.schedule.day_of_week || []).join(',')} @ ${String(t.hour).padStart(2, '0')}:${String(
                  t.minute,
                ).padStart(2, '0')}`
              : '(no schedule)';
            const dur = s.actions?.[0]?.duration_seconds;
            return (
              <li
                key={s.rule_id}
                className="flex items-center justify-between rounded border border-line bg-zinc-900/40 px-3 py-2 text-xs"
              >
                <span className="text-zinc-300">
                  <span className="text-zinc-200">{s.label || '(unlabeled)'}</span> ·{' '}
                  {nameFor[s.device_id] || s.device_id} · {when}
                  {dur ? ` · ${Math.round(dur / 60)}m` : ''}
                  {s.enabled ? '' : ' · disabled'}
                </span>
                <button
                  onClick={() => onDelete(s.rule_id)}
                  className="rounded border border-line px-2 py-0.5 text-[11px] text-zinc-400 hover:border-red-500/40 hover:text-red-300"
                >
                  Delete
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

function NewScheduleForm({ devices, onCreated, setErr }) {
  const [deviceId, setDeviceId] = useState('');
  const [instance, setInstance] = useState('');
  const [days, setDays] = useState(() => new Set(['MON', 'WED', 'FRI']));
  const [hour, setHour] = useState(6);
  const [minute, setMinute] = useState(0);
  const [duration, setDuration] = useState(10);
  const [label, setLabel] = useState('');
  const [saving, setSaving] = useState(false);

  const spigots = useMemo(() => {
    const d = devices.find((x) => x.id === deviceId);
    return d ? Object.keys(d.spigots || {}) : [];
  }, [devices, deviceId]);

  const toggleDay = (d) =>
    setDays((cur) => {
      const next = new Set(cur);
      next.has(d) ? next.delete(d) : next.add(d);
      return next;
    });

  const submit = async (e) => {
    e.preventDefault();
    if (!deviceId || !instance || !days.size) {
      setErr('Pick a timer, a spigot, and at least one day.');
      return;
    }
    setSaving(true);
    try {
      await api('/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          device_id: deviceId,
          instance,
          days: DAYS.filter((d) => days.has(d)),
          hour: Number(hour),
          minute: Number(minute),
          time_zone: localTimeZone(),
          duration_minutes: Number(duration) || null,
          label: label || null,
        }),
      });
      setLabel('');
      onCreated();
    } catch (e2) {
      setErr(String(e2.message || e2));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-zinc-500">
        New schedule
      </h2>
      <form
        onSubmit={submit}
        className="space-y-3 rounded-lg border border-line bg-zinc-900/40 p-3 text-xs"
      >
        <div className="flex flex-wrap gap-2">
          <select
            value={deviceId}
            onChange={(e) => {
              setDeviceId(e.target.value);
              setInstance('');
            }}
            className="rounded border border-line bg-bg px-2 py-1 text-zinc-200"
          >
            <option value="">— timer —</option>
            {devices.map((d) => (
              <option key={d.id} value={d.id}>
                {d.name}
              </option>
            ))}
          </select>
          <select
            value={instance}
            onChange={(e) => setInstance(e.target.value)}
            className="rounded border border-line bg-bg px-2 py-1 text-zinc-200"
            disabled={!spigots.length}
          >
            <option value="">— spigot —</option>
            {spigots.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-wrap gap-1">
          {DAYS.map((d) => (
            <button
              type="button"
              key={d}
              onClick={() => toggleDay(d)}
              className={`rounded border px-2 py-1 ${
                days.has(d)
                  ? 'border-aura/40 bg-aura/10 text-aura'
                  : 'border-line text-zinc-400'
              }`}
            >
              {d}
            </button>
          ))}
        </div>

        <div className="flex flex-wrap items-center gap-2 text-zinc-300">
          <label className="flex items-center gap-1">
            time
            <input
              type="number"
              min="0"
              max="23"
              value={hour}
              onChange={(e) => setHour(e.target.value)}
              className="w-14 rounded border border-line bg-bg px-2 py-1 text-zinc-200"
            />
            :
            <input
              type="number"
              min="0"
              max="59"
              value={minute}
              onChange={(e) => setMinute(e.target.value)}
              className="w-14 rounded border border-line bg-bg px-2 py-1 text-zinc-200"
            />
          </label>
          <label className="flex items-center gap-1">
            for
            <input
              type="number"
              min="1"
              max="360"
              value={duration}
              onChange={(e) => setDuration(e.target.value)}
              className="w-16 rounded border border-line bg-bg px-2 py-1 text-zinc-200"
            />
            min
          </label>
          <input
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="label (e.g. corn morning)"
            className="flex-1 rounded border border-line bg-bg px-2 py-1 text-zinc-200"
          />
        </div>

        <button
          type="submit"
          disabled={saving}
          className="rounded-md border border-aura/40 bg-aura/10 px-3 py-1.5 text-aura hover:bg-aura/20 disabled:opacity-50"
        >
          {saving ? 'creating…' : 'Create schedule'}
        </button>
      </form>
    </section>
  );
}
