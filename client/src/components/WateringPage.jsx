import { useCallback, useEffect, useMemo, useState } from 'react';

// WateringPage renders the Hubspace water-timer fleet: per-device cards with
// battery + rain-delay + per-spigot state and manual run/stop, the list of
// existing recurring schedules, and a form to create a new one. Everything
// proxies through /api/hubspace/* to the local Python sidecar daemon (which
// wraps the extended aioafero fork). If the sidecar isn't running the calls
// return 503 and we show a "start the sidecar" hint.

const DAYS = ['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN'];

async function api(path, opts) {
  const res = await fetch(`/api/hubspace${path}`, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(body.error || body.detail || `HTTP ${res.status}`);
  }
  return body;
}

// --- Calendar view helpers --------------------------------------------------
//
// The sidecar returns one schedule object per device, each holding an `events`
// array. An event is either ABSOLUTE (recurs weekly on `days` at `hour:minute`)
// or PERIODIC (recurs every `interval_days` from `start_date_local`, whose
// hour/minute carry the run time — the top-level hour/minute are 0). Durations
// are in `minutes`. We expand those into concrete occurrences for one week so
// the calendar can place them on a time grid.

// Friendly bed names per device + spigot, from WATERING_PLAN.md (L = spigot-1,
// R = spigot-2). Falls back to "<device> <spigot>" for anything unmapped.
const PLANTS = {
  'front flowerz': { 'spigot-1': 'Sunflowers (Great Divide)', 'spigot-2': 'House flowers' },
  'watter those': { 'spigot-1': 'Corner flowers', 'spigot-2': 'Road flowers' },
  'walled garden': { 'spigot-1': 'House flowers 2', 'spigot-2': 'Fire Fence' },
  pumpkorn: { 'spigot-1': 'Pumpkins', 'spigot-2': 'Sweet corn' },
  'garlick deez nuts': { 'spigot-1': 'Elephant garlic', 'spigot-2': 'Caragana' },
  'da-me-tree': { 'spigot-1': 'Shade trees', 'spigot-2': 'Garden' },
  'fruit of the womb': { 'spigot-1': 'Pear + apple guild', 'spigot-2': 'Raspberries' },
  'chicken fried': { 'spigot-1': 'Chicken waterer', 'spigot-2': 'Yarrow' },
  'yard knock life': { 'spigot-1': 'Back yard 2 + flowers', 'spigot-2': 'Back yard 1' },
};

// Strip the leading "1 " (Afero outlet index) the device names carry.
const normName = (name) => String(name || '').replace(/^\s*\d+\s*/, '').trim().toLowerCase();

function plantName(deviceName, spigot) {
  const beds = PLANTS[normName(deviceName)];
  return (beds && beds[spigot]) || `${normName(deviceName) || deviceName} · ${spigot}`;
}

const DOW_INDEX = {
  MONDAY: 0, TUESDAY: 1, WEDNESDAY: 2, THURSDAY: 3, FRIDAY: 4, SATURDAY: 5, SUNDAY: 6,
};
const DAY_MS = 86400000;

// A small, high-contrast palette; device id is hashed to a stable slot so the
// same timer keeps its colour across renders and weeks.
const PALETTE = [
  { bg: 'rgba(34,211,238,0.18)', border: 'rgba(34,211,238,0.55)', text: '#a5f3fc' }, // cyan
  { bg: 'rgba(74,222,128,0.18)', border: 'rgba(74,222,128,0.55)', text: '#bbf7d0' }, // green
  { bg: 'rgba(192,132,252,0.18)', border: 'rgba(192,132,252,0.55)', text: '#e9d5ff' }, // purple
  { bg: 'rgba(251,191,36,0.18)', border: 'rgba(251,191,36,0.55)', text: '#fde68a' }, // amber
  { bg: 'rgba(96,165,250,0.18)', border: 'rgba(96,165,250,0.55)', text: '#bfdbfe' }, // blue
  { bg: 'rgba(248,113,113,0.18)', border: 'rgba(248,113,113,0.55)', text: '#fecaca' }, // red
  { bg: 'rgba(244,114,182,0.18)', border: 'rgba(244,114,182,0.55)', text: '#fbcfe8' }, // pink
  { bg: 'rgba(45,212,191,0.18)', border: 'rgba(45,212,191,0.55)', text: '#99f6e4' }, // teal
  { bg: 'rgba(163,230,53,0.18)', border: 'rgba(163,230,53,0.55)', text: '#d9f99d' }, // lime
];
function colorForDevice(id) {
  let h = 0;
  for (let i = 0; i < String(id).length; i++) h = (h * 31 + String(id).charCodeAt(i)) | 0;
  return PALETTE[Math.abs(h) % PALETTE.length];
}

function startOfWeek(d) {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const mondayFirst = (x.getDay() + 6) % 7; // Sun=6, Mon=0
  x.setDate(x.getDate() - mondayFirst);
  return x;
}
function addDays(d, n) {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  x.setDate(x.getDate() + n);
  return x;
}
const fmtMin = (m) =>
  `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;

const DOW_FULL = ['MONDAY', 'TUESDAY', 'WEDNESDAY', 'THURSDAY', 'FRIDAY', 'SATURDAY', 'SUNDAY'];
const DOW_SHORT = {
  MONDAY: 'Mon', TUESDAY: 'Tue', WEDNESDAY: 'Wed', THURSDAY: 'Thu',
  FRIDAY: 'Fri', SATURDAY: 'Sat', SUNDAY: 'Sun',
};
function fmtDays(days) {
  const set = new Set(days || []);
  if (!set.size) return '';
  if (DOW_FULL.every((d) => set.has(d))) return 'daily';
  return DOW_FULL.filter((d) => set.has(d)).map((d) => DOW_SHORT[d]).join(' ');
}
// One-line description of a schedule event (handles ABSOLUTE + PERIODIC).
function describeEvent(ev) {
  const dur = `${ev.minutes}m`;
  if (ev.time_type === 'PERIODIC' && ev.start_date_local) {
    const sd = ev.start_date_local;
    const t = fmtMin((sd.hour || 0) * 60 + (sd.minute || 0));
    const every = ev.interval_days === 1 ? 'every day' : `every ${ev.interval_days} days`;
    return `${t} · ${every} · ${dur}`;
  }
  return `${fmtMin((ev.hour || 0) * 60 + (ev.minute || 0))} · ${fmtDays(ev.days)} · ${dur}`;
}

// Expand all schedule events into 7 day-columns (Mon..Sun) of occurrences for
// the week beginning `weekStart`. Each occurrence: { key, label, deviceId,
// deviceName, spigot, startMin, durMin }.
function expandWeek(schedules, weekStart) {
  const cols = Array.from({ length: 7 }, () => []);
  const weekDates = cols.map((_, i) => addDays(weekStart, i));
  for (const s of schedules || []) {
    for (let ei = 0; ei < (s.events || []).length; ei++) {
      const ev = s.events[ei];
      if (ev.enabled === false) continue;
      const durMin = Number(ev.minutes) || 0;
      const base = {
        label: plantName(s.device_name, ev.spigot),
        deviceId: s.device_id,
        deviceName: s.device_name,
        spigot: ev.spigot,
        durMin,
      };
      if (ev.time_type === 'PERIODIC' && ev.start_date_local && ev.interval_days > 0) {
        const sd = ev.start_date_local;
        const startDay = new Date(sd.year, sd.month - 1, sd.day);
        const startMin = (sd.hour || 0) * 60 + (sd.minute || 0);
        weekDates.forEach((d, i) => {
          const diff = Math.round((d - startDay) / DAY_MS);
          if (diff >= 0 && diff % ev.interval_days === 0) {
            cols[i].push({ ...base, startMin, key: `${s.schedule_id}-${ei}-${i}` });
          }
        });
      } else {
        const startMin = (ev.hour || 0) * 60 + (ev.minute || 0);
        for (const dn of ev.days || []) {
          const idx = DOW_INDEX[dn];
          if (idx != null) cols[idx].push({ ...base, startMin, key: `${s.schedule_id}-${ei}-${idx}` });
        }
      }
    }
  }
  cols.forEach((c) => c.sort((a, b) => a.startMin - b.startMin));
  return cols;
}

// Greedy lane assignment so overlapping runs on the same day sit side-by-side
// instead of stacking on top of each other.
function packLanes(items) {
  const laneEnds = [];
  const placed = items.map((it) => {
    const end = it.startMin + Math.max(it.durMin, 15);
    let lane = laneEnds.findIndex((e) => e <= it.startMin);
    if (lane === -1) {
      lane = laneEnds.length;
      laneEnds.push(end);
    } else {
      laneEnds[lane] = end;
    }
    return { ...it, lane };
  });
  return { placed, laneCount: Math.max(1, laneEnds.length) };
}

export default function WateringPage({ onExit }) {
  const [devices, setDevices] = useState([]);
  const [schedules, setSchedules] = useState([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(null); // freeform "what's in flight" label
  const [view, setView] = useState('calendar'); // 'calendar' | 'manage'

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
  const deleteSchedule = (deviceId, eventId) =>
    act('delete', () => api(`/devices/${deviceId}/events/${eventId}`, { method: 'DELETE' }));

  return (
    <div className="flex h-full w-full flex-col overflow-y-auto bg-bg text-zinc-100">
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-bg/95 px-4 py-3 backdrop-blur">
        <h1 className="text-sm font-semibold text-zinc-200">💧 Watering</h1>
        <div className="flex items-center gap-3">
          <div className="flex rounded-md border border-line p-0.5 text-xs">
            {[
              ['calendar', '📅 Calendar'],
              ['manage', '⚙ Manage'],
            ].map(([key, lbl]) => (
              <button
                key={key}
                onClick={() => setView(key)}
                className={`rounded px-2.5 py-1 ${
                  view === key ? 'bg-aura/15 text-aura' : 'text-zinc-400 hover:text-zinc-200'
                }`}
              >
                {lbl}
              </button>
            ))}
          </div>
          <button
            onClick={onExit}
            className="rounded-md border border-line px-3 py-1.5 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
          >
            ← Terminal
          </button>
        </div>
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
      ) : view === 'calendar' ? (
        <CalendarSection schedules={schedules} />
      ) : (
        <div className="space-y-6 p-4">
          <DevicesSection devices={devices} onRun={runSpigot} onStop={stopSpigot} />
          <SchedulesSection schedules={schedules} onDelete={deleteSchedule} busy={busy} />
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

function SchedulesSection({ schedules, onDelete, busy }) {
  // Flatten device→events into one sortable row per watering run.
  const rows = useMemo(() => {
    const out = [];
    for (const s of schedules || []) {
      for (const ev of s.events || []) out.push({ s, ev });
    }
    out.sort((a, b) => {
      const an = normName(a.s.device_name);
      const bn = normName(b.s.device_name);
      if (an !== bn) return an < bn ? -1 : 1;
      return (a.ev.hour || 0) * 60 + (a.ev.minute || 0) - ((b.ev.hour || 0) * 60 + (b.ev.minute || 0));
    });
    return out;
  }, [schedules]);

  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-zinc-500">
        Schedules
      </h2>
      {!rows.length ? (
        <p className="text-xs text-zinc-500">No schedules yet.</p>
      ) : (
        <ul className="space-y-1">
          {rows.map(({ s, ev }) => (
            <li
              key={`${s.device_id}-${ev.event_id}`}
              className="flex items-center justify-between rounded border border-line bg-zinc-900/40 px-3 py-2 text-xs"
            >
              <span className="text-zinc-300">
                <span className="text-zinc-200">{plantName(s.device_name, ev.spigot)}</span> ·{' '}
                {normName(s.device_name)} · {describeEvent(ev)}
                {ev.enabled === false ? ' · disabled' : ''}
              </span>
              <button
                onClick={() => onDelete(s.device_id, ev.event_id)}
                disabled={!ev.event_id || !!busy}
                className="rounded border border-line px-2 py-0.5 text-[11px] text-zinc-400 hover:border-red-500/40 hover:text-red-300 disabled:opacity-40"
              >
                Delete
              </button>
            </li>
          ))}
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

// CalendarSection renders a week time-grid of every scheduled run, coloured by
// timer, labelled with the bed it waters. Mon–Sun columns, a time gutter, and
// occurrence chips placed by start time with height proportional to duration.
const PX_PER_MIN = 0.8;

function CalendarSection({ schedules }) {
  const [weekStart, setWeekStart] = useState(() => startOfWeek(new Date()));
  const cols = useMemo(() => expandWeek(schedules, weekStart), [schedules, weekStart]);
  const occ = useMemo(() => cols.flat(), [cols]);

  // Compress the grid to the active part of the day (pad 30 min either side,
  // snap to the hour), so the empty 19:00–01:00 stretch isn't dead space.
  const { winStart, winEnd } = useMemo(() => {
    if (!occ.length) return { winStart: 4 * 60, winEnd: 22 * 60 };
    let mn = Infinity;
    let mx = -Infinity;
    for (const o of occ) {
      mn = Math.min(mn, o.startMin);
      mx = Math.max(mx, o.startMin + Math.max(o.durMin, 15));
    }
    return {
      winStart: Math.max(0, Math.floor((mn - 30) / 60) * 60),
      winEnd: Math.min(24 * 60, Math.ceil((mx + 30) / 60) * 60),
    };
  }, [occ]);

  const height = (winEnd - winStart) * PX_PER_MIN;
  const hourLines = [];
  for (let h = Math.ceil(winStart / 60) * 60; h <= winEnd; h += 60) hourLines.push(h);

  const today = new Date();
  const todayKey = today.toDateString();
  const dayDates = Array.from({ length: 7 }, (_, i) => addDays(weekStart, i));
  const weekEnd = addDays(weekStart, 6);
  const fmtRange = (a, b) => {
    const opts = { month: 'short', day: 'numeric' };
    return `${a.toLocaleDateString(undefined, opts)} – ${b.toLocaleDateString(undefined, opts)}`;
  };
  const dayNames = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
  const totalRuns = occ.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <button
            onClick={() => setWeekStart((w) => addDays(w, -7))}
            className="rounded border border-line px-2 py-1 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
            aria-label="Previous week"
          >
            ‹
          </button>
          <button
            onClick={() => setWeekStart(startOfWeek(new Date()))}
            className="rounded border border-line px-2.5 py-1 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
          >
            This week
          </button>
          <button
            onClick={() => setWeekStart((w) => addDays(w, 7))}
            className="rounded border border-line px-2 py-1 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
            aria-label="Next week"
          >
            ›
          </button>
          <span className="ml-1 text-sm font-medium text-zinc-200">
            {fmtRange(weekStart, weekEnd)}
          </span>
        </div>
        <span className="text-[11px] text-zinc-500">{totalRuns} runs this week</span>
      </div>

      {!totalRuns ? (
        <p className="text-xs text-zinc-500">No scheduled runs this week.</p>
      ) : (
        <div className="min-h-0 flex-1 overflow-auto rounded-lg border border-line bg-zinc-900/30">
          {/* Day headers — sticky so they stay visible while scrolling time. */}
          <div className="sticky top-0 z-10 flex border-b border-line bg-zinc-900/80 backdrop-blur">
            <div className="w-12 shrink-0" />
            {dayDates.map((d, i) => {
              const isToday = d.toDateString() === todayKey;
              return (
                <div
                  key={i}
                  className={`flex-1 border-l border-line px-1 py-1.5 text-center text-[11px] ${
                    isToday ? 'text-aura' : 'text-zinc-400'
                  }`}
                >
                  <div className="font-medium">{dayNames[i]}</div>
                  <div className={isToday ? 'text-aura' : 'text-zinc-600'}>{d.getDate()}</div>
                </div>
              );
            })}
          </div>

          {/* Time grid */}
          <div className="flex" style={{ height }}>
            <div className="relative w-12 shrink-0">
              {hourLines.map((h) => (
                <div
                  key={h}
                  className="absolute right-1 -translate-y-1/2 text-[10px] tabular-nums text-zinc-600"
                  style={{ top: (h - winStart) * PX_PER_MIN }}
                >
                  {fmtMin(h)}
                </div>
              ))}
            </div>
            <div className="relative flex-1">
              {hourLines.map((h) => (
                <div
                  key={h}
                  className="absolute left-0 right-0 border-t border-line/40"
                  style={{ top: (h - winStart) * PX_PER_MIN }}
                />
              ))}
              <div className="absolute inset-0 flex">
                {cols.map((dayItems, i) => (
                  <DayColumn
                    key={i}
                    items={dayItems}
                    winStart={winStart}
                    isToday={dayDates[i].toDateString() === todayKey}
                  />
                ))}
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function DayColumn({ items, winStart, isToday }) {
  const { placed, laneCount } = useMemo(() => packLanes(items), [items]);
  return (
    <div className={`relative flex-1 border-l border-line ${isToday ? 'bg-aura/[0.04]' : ''}`}>
      {placed.map((it) => {
        const color = colorForDevice(it.deviceId);
        const widthPct = 100 / laneCount;
        return (
          <div
            key={it.key}
            title={`${it.deviceName} · ${it.spigot}\n${fmtMin(it.startMin)}–${fmtMin(
              it.startMin + it.durMin,
            )} (${it.durMin} min)`}
            className="absolute overflow-hidden rounded border px-1 py-0.5 text-[10px] leading-tight"
            style={{
              top: (it.startMin - winStart) * PX_PER_MIN,
              height: Math.max(it.durMin * PX_PER_MIN, 16),
              left: `calc(${it.lane * widthPct}% + 1px)`,
              width: `calc(${widthPct}% - 2px)`,
              backgroundColor: color.bg,
              borderColor: color.border,
              color: color.text,
            }}
          >
            <span className="font-medium tabular-nums">{fmtMin(it.startMin)}</span>{' '}
            <span className="opacity-90">{it.label}</span>
            <span className="opacity-60"> · {it.durMin}m</span>
          </div>
        );
      })}
    </div>
  );
}
