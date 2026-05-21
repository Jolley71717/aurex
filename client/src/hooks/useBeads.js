import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

// useBeads bootstraps the bead list from GET /api/beads and keeps it live.
//
// Liveness strategy:
//   1. Open an EventSource to /api/beads/events. If a `bead` event arrives,
//      we refetch the affected id (one bead at a time keeps the patch small)
//      and merge it into the keyed map.
//   2. If SSE never connects, or drops and won't recover, fall back to 10s
//      polling of GET /api/beads.
//
// The store is exposed as both a plain map keyed by id (for O(1) lookup
// during merges and optimistic edits) and a memoized sorted array used by
// the page for rendering columns.
//
// `bd list --json` only returns open/in_progress/blocked beads. Beads that
// the user closes in this session stay in the local map under status="closed"
// so the Closed column reflects in-session activity. A page refresh wipes
// it — that's accepted for v1.
export function useBeads() {
  const [beadsById, setBeadsById] = useState({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [events, setEvents] = useState([]);
  const [sseConnected, setSseConnected] = useState(false);

  const seenIdsRef = useRef(new Set());
  const pollTimerRef = useRef(null);
  const sseRef = useRef(null);
  const fallbackToPollingRef = useRef(false);
  // EventSource auto-reconnects on errors, which is wasteful when the
  // endpoint is permanently missing (UNC-81 not yet deployed). After this
  // many consecutive failures without an open event, give up.
  const sseFailureCountRef = useRef(0);

  const mergeBeads = useCallback((incoming) => {
    setBeadsById((prev) => {
      const next = { ...prev };
      for (const b of incoming) {
        if (!b || !b.id) continue;
        // Don't overwrite a local optimistic "closed" with a stale list entry
        // that still says open — the SSE refresh path patches one id at a
        // time and won't hit this case.
        next[b.id] = b;
        seenIdsRef.current.add(b.id);
      }
      return next;
    });
  }, []);

  const refetchOne = useCallback(async (id) => {
    try {
      const res = await fetch(`/api/beads/${encodeURIComponent(id)}`);
      if (res.status === 404) {
        // Bead was deleted — drop it.
        setBeadsById((prev) => {
          if (!(id in prev)) return prev;
          const next = { ...prev };
          delete next[id];
          return next;
        });
        return;
      }
      if (!res.ok) return;
      const b = await res.json();
      mergeBeads([b]);
    } catch (_) {
      // network blip — ignore, next event or poll will retry
    }
  }, [mergeBeads]);

  const refetchAll = useCallback(async () => {
    try {
      const res = await fetch('/api/beads');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const list = await res.json();
      if (!Array.isArray(list)) throw new Error('list is not an array');
      // Replace the open-set with the server's truth, but keep any local
      // optimistic closes that the server-side list doesn't include yet.
      setBeadsById((prev) => {
        const next = {};
        for (const b of list) next[b.id] = b;
        for (const [id, b] of Object.entries(prev)) {
          if (!(id in next) && b && b.status === 'closed') next[id] = b;
        }
        return next;
      });
      setError(null);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  // Initial bootstrap.
  useEffect(() => {
    refetchAll();
  }, [refetchAll]);

  const startPolling = useCallback(() => {
    if (pollTimerRef.current) return;
    pollTimerRef.current = setInterval(() => {
      refetchAll();
    }, 10000);
  }, [refetchAll]);

  const stopPolling = useCallback(() => {
    if (pollTimerRef.current) {
      clearInterval(pollTimerRef.current);
      pollTimerRef.current = null;
    }
  }, []);

  // SSE subscription. If the endpoint isn't available (UNC-81 not yet
  // deployed), the EventSource will reconnect-loop on 404 — we fall back to
  // polling on the first error and stop trying.
  useEffect(() => {
    let cancelled = false;
    let es;
    try {
      es = new EventSource('/api/beads/events');
    } catch (_) {
      fallbackToPollingRef.current = true;
      startPolling();
      return undefined;
    }
    sseRef.current = es;

    es.onopen = () => {
      if (cancelled) return;
      setSseConnected(true);
      sseFailureCountRef.current = 0;
      fallbackToPollingRef.current = false;
      stopPolling();
    };

    es.addEventListener('bead', (ev) => {
      if (cancelled) return;
      let data;
      try {
        data = JSON.parse(ev.data);
      } catch (_) {
        return;
      }
      const at = data.at || new Date().toISOString();
      setEvents((prev) => {
        const next = [{ ...data, at, _key: `${at}-${data.beadId || ''}` }, ...prev];
        return next.slice(0, 50);
      });
      if (data.beadId) refetchOne(data.beadId);
    });

    es.onerror = () => {
      if (cancelled) return;
      setSseConnected(false);
      if (!fallbackToPollingRef.current) {
        fallbackToPollingRef.current = true;
        startPolling();
      }
      // If we've never successfully opened and we've seen multiple errors,
      // assume the endpoint isn't deployed yet and stop reconnecting.
      sseFailureCountRef.current += 1;
      if (sseFailureCountRef.current >= 3 && es.readyState !== EventSource.OPEN) {
        es.close();
      }
    };

    return () => {
      cancelled = true;
      es.close();
      sseRef.current = null;
      stopPolling();
    };
  }, [refetchOne, startPolling, stopPolling]);

  const setBeadOptimistic = useCallback((id, patch) => {
    setBeadsById((prev) => {
      const cur = prev[id];
      if (!cur) return prev;
      return { ...prev, [id]: { ...cur, ...patch } };
    });
  }, []);

  const addBeadOptimistic = useCallback((bead) => {
    setBeadsById((prev) => ({ ...prev, [bead.id]: bead }));
  }, []);

  const removeBeadOptimistic = useCallback((id) => {
    setBeadsById((prev) => {
      if (!(id in prev)) return prev;
      const next = { ...prev };
      delete next[id];
      return next;
    });
  }, []);

  // Reverse-deps map: childId -> [parentId]. Computed from each bead's
  // dependencies (where type === "blocks"). children = beads whose
  // dependencies include this bead.
  const beadsArray = useMemo(() => Object.values(beadsById), [beadsById]);

  const childrenOf = useMemo(() => {
    const out = {};
    for (const b of beadsArray) {
      const deps = Array.isArray(b.dependencies) ? b.dependencies : [];
      for (const d of deps) {
        if (d.type !== 'blocks') continue;
        const parent = d.depends_on_id;
        if (!parent) continue;
        if (!out[parent]) out[parent] = [];
        out[parent].push(b.id);
      }
    }
    return out;
  }, [beadsArray]);

  return {
    beads: beadsArray,
    beadsById,
    childrenOf,
    loading,
    error,
    events,
    sseConnected,
    refetchAll,
    refetchOne,
    setBeadOptimistic,
    addBeadOptimistic,
    removeBeadOptimistic,
  };
}
