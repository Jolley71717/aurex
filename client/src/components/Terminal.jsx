import { useEffect, useReducer, useRef, useState } from 'react';
import { FitAddon, Ghostty, Terminal as GhosttyTerminal } from 'ghostty-web';
import {
  diffToPayload,
  mirrorPad,
  stripPad,
  padCount,
  MIRROR_PAD_LEN,
  MIRROR_PAD_MIN,
} from './terminalMirror.js';

// Diagnostic overlay for mobile input events — opt in via ?debug=input. Keeps
// the plumbing around so a future regression can be inspected without a code
// change; just navigate to /?debug=input on the device that's misbehaving.
const DEBUG_INPUT = typeof window !== 'undefined'
  && new URLSearchParams(window.location.search).get('debug') === 'input';

const THEME = {
  background: '#0b0f14',
  foreground: '#e5e7eb',
  cursor: '#22d3ee',
  cursorAccent: '#0b0f14',
  selectionBackground: 'rgba(34, 211, 238, 0.25)',
  black: '#1f2937',
  red: '#f87171',
  green: '#4ade80',
  yellow: '#fbbf24',
  blue: '#60a5fa',
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#e5e7eb',
  brightBlack: '#374151',
  brightRed: '#fca5a5',
  brightGreen: '#86efac',
  brightYellow: '#fde68a',
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#f9fafb',
};

// ghostty-web requires a one-time async WASM load that produces a Ghostty
// instance. That instance must be passed into every Terminal we construct
// (via the `ghostty:` option) — without it ghostty's WASM input pipeline
// isn't wired to the Terminal and keystrokes are silently dropped. The
// top-level init() function in ghostty-web's README is NOT sufficient; we
// have to go through Ghostty.load() and thread the instance through.
let ghosttyReady;
const loadGhostty = () => {
  if (!ghosttyReady) {
    ghosttyReady = Ghostty.load().catch((err) => {
      ghosttyReady = undefined;
      throw err;
    });
  }
  return ghosttyReady;
};

/**
 * Terminal renders Ghostty's VT100 emulator (via WASM) in a canvas. Same
 * onData/onResize/scrollLines API as the xterm.js version that lived here
 * before — see git history for the swap. Touch drag → term.scrollLines() so
 * mobile gets native scrollback flick.
 */
export default function Terminal({ onReady, onInput, onResize }) {
  const containerRef = useRef(null);
  const termRef = useRef(null);
  const fitRef = useRef(null);
  const [followBottom, setFollowBottom] = useState(true);
  // Mirror of followBottom for use inside the once-at-mount effect, which can't
  // see state updates through its own stale closure.
  const followRef = useRef(true);

  // Mobile input strategy: a hidden HTML textarea overlays the terminal and
  // captures all typing. The browser handles autocorrect / swipe / IME / paste
  // natively inside the textarea, so we never have to interpret individual
  // beforeinput events (which differ across keyboards). After each change we
  // diff the textarea's current value against what we've already sent to the
  // server and emit only the delta — backspaces for removed characters, then
  // the new characters. This is keyboard-agnostic.
  const mirrorRef = useRef(null);
  const lastSentRef = useRef('');
  const [isTouch, setIsTouch] = useState(false);
  // Touch-scroll accumulator state lifted to refs so both the container and
  // the mirror (when armed) can drive term.scrollLines from a shared bucket.
  // Refs survive Terminal re-mounts (component instance) and React renders
  // without re-binding listeners. The handlers themselves read term out of
  // termRef.current so they're robust against ghostty init ordering.
  const touchStartYRef = useRef(null);
  const touchStartXRef = useRef(null);
  const touchAccumRef = useRef(0);
  const touchMovedTotalRef = useRef(0);
  // Soft-keyboard "armed" flag. When false, the mirror renders with
  // inputMode="none" and readOnly, so iOS / Android refuse to show the soft
  // keyboard even if focus is restored to the mirror by an OS-level auto-
  // focus heuristic (e.g. focus restoration on touchstart during a scroll
  // gesture). The toggle button on the toolbar drives this state by calling
  // the imperative focus()/blur() methods on the handle.
  const [keyboardArmed, setKeyboardArmed] = useState(false);
  // Ref mirror so isKeyboardOpen() can return synchronously without React
  // having to re-render first.
  const keyboardArmedRef = useRef(false);
  useEffect(() => { keyboardArmedRef.current = keyboardArmed; }, [keyboardArmed]);
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const touch = ('ontouchstart' in window) || (navigator.maxTouchPoints || 0) > 0;
    setIsTouch(touch);
  }, []);

  // Bind the touch handlers to the hidden mirror textarea once it's in the
  // DOM. Separate from the ghostty-init effect because:
  //   * Ghostty.load() is cached as a module-level singleton; on Terminal
  //     remount (e.g., user navigates Ideas → terminal → Ideas → terminal)
  //     it resolves in a microtask BEFORE React commits the isTouch state
  //     update on the new mount. So binding inside .then() would see
  //     mirrorRef.current === null and skip the binding silently.
  //   * The mirror is rendered conditionally on isTouch. By keying this
  //     effect on isTouch + the keyboard-armed state we re-bind whenever
  //     either changes, and the mirror is guaranteed to be in the DOM by
  //     the time React invokes the effect callback.
  //
  // The mirror only intercepts touchmove when it has `pointer-events: auto`
  // (i.e., keyboard armed). When the keyboard is down, touchmove on the
  // canvas hits the container's listeners instead — both surfaces drive
  // the same accumulator refs so there's no double-fire.
  useEffect(() => {
    if (!isTouch) return undefined;
    const mirror = mirrorRef.current;
    const handlers = termRef.current?.__touchHandlers;
    if (!mirror || !handlers) return undefined;
    const { onTouchStart, onTouchMove, onTouchEnd } = handlers;
    mirror.addEventListener('touchstart', onTouchStart, { passive: true });
    mirror.addEventListener('touchmove', onTouchMove, { passive: false });
    mirror.addEventListener('touchend', onTouchEnd);
    mirror.addEventListener('touchcancel', onTouchEnd);
    return () => {
      mirror.removeEventListener('touchstart', onTouchStart);
      mirror.removeEventListener('touchmove', onTouchMove);
      mirror.removeEventListener('touchend', onTouchEnd);
      mirror.removeEventListener('touchcancel', onTouchEnd);
    };
    // Re-bind whenever the keyboard arms — armMirror flips pointer-events
    // on the mirror and that's the state where the mirror starts swallowing
    // touchmove. The cleanup function correctly removes the previous
    // listeners; React calls it before re-running the effect.
  }, [isTouch, keyboardArmed]);

  const handleMirrorInput = (e) => {
    const ta = mirrorRef.current;
    if (!ta) return;
    const newVal = ta.value;
    const oldVal = lastSentRef.current;
    const payload = diffToPayload(oldVal, newVal);
    pushDbg('mi', { it: e?.nativeEvent?.inputType, ol: oldVal.length, nl: newVal.length, pl: payload.length });
    if (payload) onInputRef.current?.(payload);
    lastSentRef.current = newVal;
    // Top the zero-width pad back up if a held delete has eaten into it, so the
    // next press-and-hold never runs out of chars for iOS auto-repeat to chew.
    maybeReplenishPad();
  };

  // Keep the mirror prefixed with a run of zero-width pad chars (see
  // terminalMirror.js). seedMirror is called whenever the soft keyboard arms;
  // maybeReplenishPad tops it up as the user deletes into it. Both resync
  // lastSentRef to the new value so the pad change itself emits nothing.
  const seedMirror = () => {
    const ta = mirrorRef.current;
    if (!ta) return;
    ta.value = mirrorPad() + stripPad(ta.value);
    try { ta.setSelectionRange(ta.value.length, ta.value.length); } catch {}
    lastSentRef.current = ta.value;
  };
  const maybeReplenishPad = () => {
    const ta = mirrorRef.current;
    if (!ta) return;
    if (padCount(ta.value) >= MIRROR_PAD_MIN) return;
    // Only top up with the caret at the very end, so we never shift text out
    // from under a mid-line caret (which would corrupt the diff).
    const atEnd = ta.selectionStart === ta.value.length && ta.selectionEnd === ta.value.length;
    if (!atEnd) return;
    ta.value = mirrorPad() + stripPad(ta.value);
    try { ta.setSelectionRange(ta.value.length, ta.value.length); } catch {}
    lastSentRef.current = ta.value;
  };
  const handleMirrorKeyUp = (e) => {
    if (e.key === 'Backspace') maybeReplenishPad();
  };

  // Native beforeinput listener — installed via useEffect below so we get the
  // real DOM event (React's synthetic onBeforeInput has historically been
  // unreliable across versions). Mainly catches word-backward / line-backward
  // deletes from soft keyboards that wouldn't fire a per-character Backspace
  // keydown.
  useEffect(() => {
    const ta = mirrorRef.current;
    if (!ta || !isTouch) return undefined;
    const onBI = (e) => {
      pushDbg('mbi', { it: e.inputType, data: e.data });
      if (e.inputType === 'deleteWordBackward' || e.inputType === 'deleteSoftLineBackward') {
        // Word/line deletes don't map cleanly to a single backspace, and the
        // input event that follows will compute the correct diff for what
        // was removed from the textarea's value. If the textarea was empty
        // (server-side content), we send one backspace as a best effort.
        if (ta.value.length === 0) {
          onInputRef.current?.('\x7f');
        }
        // For non-empty: let default delete the text; input handler diffs.
      }
    };
    ta.addEventListener('beforeinput', onBI);
    return () => ta.removeEventListener('beforeinput', onBI);
  }, [isTouch]);

  const handleMirrorKeyDown = (e) => {
    pushDbg('mkd', { key: e.key, code: e.code });
    // Submit line on Enter (without shift). Send \r, drop our buffer.
    if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      onInputRef.current?.('\r');
      if (mirrorRef.current) {
        // Reset to a fresh pad (not empty) so the next held delete still has
        // zero-width chars for iOS auto-repeat to consume.
        mirrorRef.current.value = mirrorPad();
        try { mirrorRef.current.setSelectionRange(MIRROR_PAD_LEN, MIRROR_PAD_LEN); } catch {}
      }
      lastSentRef.current = mirrorPad();
      return;
    }
    // Backspace handling has two cases. NOTE: with the zero-width pad
    // (seedMirror / maybeReplenishPad) the mirror is virtually never empty, so
    // Case 2 is the normal path even when deleting server-side text — that's
    // what lets iOS press-and-hold auto-repeat keep going. Case 1 is now only a
    // fallback for the rare moment the pad is fully exhausted.
    //
    //  1. Mirror is empty but the server's prompt has text the user didn't
    //     type locally (e.g. they reconnected to a session with text in
    //     Claude Code's input). We must synthesize a \x7f because the
    //     default action would no-op against an empty textarea.
    //
    //  2. Mirror has content. We MUST let the default delete fire — iOS's
    //     press-and-hold auto-repeat only kicks in when it sees the
    //     default action complete on the first keystroke. If we
    //     preventDefault, iOS treats the key as consumed-but-ignored and
    //     never starts repeating. After the default removes the char, the
    //     onInput handler diffs ta.value against lastSentRef and sends
    //     exactly one \x7f per removed char — so press-and-hold sends a
    //     stream of \x7f for as long as the user holds.
    if (e.key === 'Backspace' && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const ta = mirrorRef.current;
      if (ta && ta.value.length > 0) {
        // Case 2 — let default + onInput diff do the work.
        return;
      }
      // Case 1 — synthesize the delete against the server-side buffer.
      e.preventDefault();
      onInputRef.current?.('\x7f');
      if (lastSentRef.current.length > 0) {
        lastSentRef.current = lastSentRef.current.slice(0, -1);
      }
      return;
    }
    // Everything else (printable chars, IME) flows through the input event
    // after the textarea updates. Special keys like Escape / arrows / Tab /
    // Ctrl-* are routed via the mobile toolbar.
  };

  const dbgLogRef = useRef([]);
  const [, dbgTick] = useReducer((x) => (x + 1) & 0xffff, 0);
  const pushDbg = (kind, info) => {
    if (!DEBUG_INPUT) return;
    const entry = { t: Date.now() % 100000, kind, ...info };
    const next = dbgLogRef.current.slice(-19);
    next.push(entry);
    dbgLogRef.current = next;
    dbgTick();
  };

  // Stale-closure bridge: onInput changes when ctrlArmed flips, and ghostty's
  // onData is registered once at mount. Without the ref the CTRL toolbar key
  // would never transform the next keystroke.
  const onInputRef = useRef(onInput);
  const onResizeRef = useRef(onResize);
  useEffect(() => { onInputRef.current = onInput; }, [onInput]);
  useEffect(() => { onResizeRef.current = onResize; }, [onResize]);

  const setFollow = (v) => {
    followRef.current = v;
    setFollowBottom(v);
  };

  useEffect(() => {
    if (!containerRef.current) return undefined;

    let disposed = false;
    const cleanups = [];

    const isDesktop = window.matchMedia('(min-width: 768px)').matches;
    const fontSize = isDesktop ? 15 : 14;

    loadGhostty()
      .then((ghostty) => {
        if (disposed || !containerRef.current) return;

        // Defensive: empty the container before ghostty appends its canvas +
        // textarea. If a previous ghostty instance didn't fully clean up on
        // dispose (or HMR replaced this effect), leftover DOM would stack
        // behind the new one — the "old session showing through new session"
        // bug on session switch.
        while (containerRef.current.firstChild) {
          containerRef.current.removeChild(containerRef.current.firstChild);
        }

        const term = new GhosttyTerminal({
          ghostty, // critical — wires WASM input pipeline into the Terminal
          theme: THEME,
          fontFamily: 'JetBrains Mono, Fira Code, Menlo, ui-monospace, monospace',
          fontSize,
          cursorBlink: true,
          // 100k lines of in-browser scrollback. xterm/ghostty stores
          // these as JS strings on the heap; ~30 chars/line averages out
          // to ~6 MB of browser memory which is fine on any device that
          // can run Compose-for-Web. Pair this with the 256 MiB server-side
          // ring buffer (see NewOutputBuffer in sessions.go) so a long
          // reconnect replays into a buffer that can actually hold it.
          scrollback: 100000,
        });
        const fit = new FitAddon();
        term.loadAddon(fit);
        term.open(containerRef.current);
        try { fit.fit(); } catch {}
        term.focus();

        termRef.current = term;
        fitRef.current = fit;

        const dataDispose = term.onData((data) => onInputRef.current?.(data));
        const resizeDispose = term.onResize(({ cols, rows }) => onResizeRef.current?.(cols, rows));
        cleanups.push(() => dataDispose.dispose?.());
        cleanups.push(() => resizeDispose.dispose?.());

        // Follow-bottom logic. Ghostty auto-scrolls to bottom on every write,
        // which on mobile means streaming output yanks the viewport back to
        // the bottom every time the user tries to scroll up to read history
        // or see the top of an option picker. We suppress that by saving
        // viewportY before each write and restoring it after, offset by any
        // lines pushed into scrollback. Follow-mode re-arms when the user
        // touch-scrolls back to the bottom or taps the jump-to-bottom chip.
        //
        // viewportY semantics in ghostty-web: number of lines scrolled up from
        // the bottom; 0 = at bottom.
        // Strip control sequences that would otherwise damage scrollback —
        // some TUI redraw paths emit \x1b[3J (erase scrollback) or alt-screen
        // enter/exit sequences as part of a full repaint. We don't want any of
        // those touching ghostty's history, since on mobile the only way the
        // user can re-read a long response is to scroll back through it.
        // Stripping is conservative: we keep all other ANSI behavior intact.
        const stripDangerousAnsi = (s) => {
          if (typeof s !== 'string') return s;
          return s
            .replace(/\x1b\[3J/g, '')      // erase entire scrollback
            .replace(/\x1b\[\?47[hl]/g, '')  // legacy alt-screen
            .replace(/\x1b\[\?1047[hl]/g, '') // alt-screen w/ save/restore
            .replace(/\x1b\[\?1049[hl]/g, ''); // alt-screen w/ cursor + erase
        };

        // Critical: ghostty's term.write() unconditionally calls
        // scrollToBottom() when viewportY > 0, which fires onScroll(0). If we
        // don't suppress that, our onScroll listener re-arms follow-mode and
        // we lose the user's scroll position on the very next write. So hold
        // the suppress flag across the entire write + restore window.
        let suppressScrollEvent = false;
        const writeAndPreserveScroll = (data) => {
          const cleaned = stripDangerousAnsi(data);
          if (followRef.current) {
            term.write(cleaned);
            return;
          }
          const prevY = term.viewportY;
          const prevLen = term.getScrollbackLength?.() ?? 0;
          suppressScrollEvent = true;
          try {
            term.write(cleaned);
            const newLen = term.getScrollbackLength?.() ?? 0;
            const added = Math.max(0, newLen - prevLen);
            const targetY = prevY + added;
            // scrollToLine(N) clamps N to [0, scrollbackLength] and sets
            // viewportY = N. Direct viewportY assignment bypasses the render
            // pipeline; scrollToLine fires the proper events.
            try { term.scrollToLine(targetY); } catch {}
          } finally {
            suppressScrollEvent = false;
          }
        };

        const scrollDispose = term.onScroll?.((y) => {
          if (suppressScrollEvent) return;
          // User-initiated scroll (scrollbar drag, etc). y is viewportY:
          // 0 = at bottom, > 0 = scrolled up.
          if (y === 0 && !followRef.current) setFollow(true);
          else if (y > 0 && followRef.current) setFollow(false);
        });
        if (scrollDispose) cleanups.push(() => scrollDispose.dispose?.());

        // NOTE: do NOT set tabIndex on the container. ghostty.open() puts
        // tabindex=0 + contenteditable=true on it as part of how its
        // InputHandler routes keys to WASM. Overriding either breaks input.

        // On touch devices, all typing should go through the mobile input
        // mirror (rendered above) — that's the only path that gets autocorrect
        // right. But ghostty's host is contenteditable, so a tap, focus
        // event, or programmatic call can steal focus to it. If that happens,
        // the IME starts firing events on the host instead of the mirror, and
        // typing becomes intermittently broken (the "autocorrect sometimes
        // works, sometimes doesn't" bug). Bounce focus back to the mirror
        // whenever the host receives it.
        const host = containerRef.current;
        let lastKeyAt = 0;
        let lastKeyHadValue = false;
        const onKeyDownCap = (e) => {
          lastKeyAt = performance.now();
          lastKeyHadValue = !!(e.key && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey);
          // Intercept Cmd+V / Ctrl+V before ghostty's keymap sees them.
          // ghostty-web 0.4 binds Meta+V (and Ctrl+V) to an internal action
          // that emits a stray character ("o" in practice on macOS) — we need
          // its keydown handler to never run for the paste shortcut. We
          // stopImmediatePropagation (so ghostty's bubble-phase listener
          // doesn't fire) but do NOT preventDefault, so the browser still
          // dispatches its native 'paste' event for our handler below to
          // route (text → beforeinput insertFromPaste, image → /api/paste/image).
          if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey) {
            const key = (e.key || '').toLowerCase();
            if (key === 'v') {
              e.stopImmediatePropagation();
            }
          }
        };
        const onHostFocusIn = () => {
          const mirror = mirrorRef.current;
          if (!mirror) return;
          if (document.activeElement === mirror) return;
          // Defer so we don't fight a focus event that's still being dispatched.
          requestAnimationFrame(() => {
            try { mirror.focus({ preventScroll: true }); } catch {
              mirror.focus();
            }
          });
        };
        // Only install the redirect on touch devices. On desktop we want
        // ghostty's native keydown path to handle typing (mirror isn't even
        // rendered there).
        const touchCapable = ('ontouchstart' in window) || (navigator.maxTouchPoints || 0) > 0;
        if (touchCapable) {
          host.addEventListener('focusin', onHostFocusIn);
          cleanups.push(() => host.removeEventListener('focusin', onHostFocusIn));
        }

        // Image-paste fast path. The browser's paste event runs BEFORE
        // beforeinput and exposes clipboardData.items with their MIME types,
        // so we can intercept image clipboards (Cmd+V on a screenshot) and
        // route them through /api/paste/image — matches the Toolbar PASTE
        // button's flow, just keyboard-triggered. Plain-text Cmd+V is left
        // alone so the existing insertFromPaste handler above keeps owning it.
        const onPaste = (e) => {
          const items = e.clipboardData && e.clipboardData.items;
          if (!items || items.length === 0) return;
          let imgItem = null;
          for (const it of items) {
            if (it.kind === 'file' && it.type && it.type.startsWith('image/')) {
              imgItem = it;
              break;
            }
          }
          if (!imgItem) return; // not an image — let the normal text path handle it
          e.preventDefault();
          e.stopPropagation();
          const blob = imgItem.getAsFile();
          if (!blob) return;
          fetch('/api/paste/image', {
            method: 'POST',
            headers: { 'Content-Type': blob.type || 'image/png' },
            body: blob,
            credentials: 'include',
          })
            .then((res) => (res.ok ? res.json() : Promise.reject(new Error('upload ' + res.status))))
            .then((data) => {
              if (data && data.path) {
                onInputRef.current?.(data.path + ' ');
              }
            })
            .catch((err) => {
              console.error('aurex: image paste upload failed', err);
            });
        };
        host.addEventListener('paste', onPaste, true);
        cleanups.push(() => host.removeEventListener('paste', onPaste, true));

        // Debounced fit — many resize bursts (orientation, keyboard, sidebar)
        // collapse to one PTY resize call. We always emit our current size via
        // onResize after fit, even if ghostty thinks the size didn't change —
        // otherwise switching devices leaves the server's PTY at the previous
        // (often smaller) client's size, since ghostty's onResize event only
        // fires on an internal dimension change.
        let resizeTimer = null;
        const emitSize = () => {
          if (term.cols > 0 && term.rows > 0) {
            onResizeRef.current?.(term.cols, term.rows);
          }
        };
        const handleResize = () => {
          if (resizeTimer) clearTimeout(resizeTimer);
          resizeTimer = setTimeout(() => {
            resizeTimer = null;
            try { fit.fit(); } catch {}
            emitSize();
          }, 100);
        };
        const refitNow = () => {
          if (resizeTimer) {
            clearTimeout(resizeTimer);
            resizeTimer = null;
          }
          try { fit.fit(); } catch {}
          emitSize();
        };
        window.addEventListener('resize', handleResize);
        window.addEventListener('orientationchange', handleResize);
        const ro = new ResizeObserver(handleResize);
        ro.observe(containerRef.current);
        cleanups.push(() => {
          if (resizeTimer) clearTimeout(resizeTimer);
          window.removeEventListener('resize', handleResize);
          window.removeEventListener('orientationchange', handleResize);
          ro.disconnect();
        });

        // Mobile touch scroll — drag finger → term.scrollLines(). ghostty's
        // canvas doesn't expose a DOM scroll target on its own, so we drive
        // scrollLines manually. Cell height estimated from fontSize × lineHeight
        // (no DOM grid to measure against, unlike the old xterm.js setup).
        // Accumulators live in component-scope refs (touchStartYRef etc.) so
        // both the container and the mirror surface (when armed) drive the
        // same scroll bucket — see the separate useEffect below that binds
        // the mirror handlers after isTouch flips and the textarea is in DOM.
        const cellHeight = fontSize * 1.2;
        termRef.current = term;
        const onTouchStart = (e) => {
          if (e.touches.length !== 1) return;
          touchStartYRef.current = e.touches[0].clientY;
          touchStartXRef.current = e.touches[0].clientX;
          touchAccumRef.current = 0;
          touchMovedTotalRef.current = 0;
        };
        const onTouchMove = (e) => {
          if (touchStartYRef.current === null || e.touches.length !== 1) return;
          // Claim the gesture so iOS Safari can't interpret a vertical drag
          // as a page-scroll once ghostty's scrollback hits its edge. Without
          // this, dragging down past the bottom of the buffer let iOS pan
          // the document, leaving a black gap between the toolbar and the
          // mobile input bar. preventDefault requires the listener to be
          // non-passive — set below where we register it.
          if (e.cancelable) e.preventDefault();
          const y = e.touches[0].clientY;
          const x = e.touches[0].clientX;
          touchMovedTotalRef.current += Math.abs(y - touchStartYRef.current) + Math.abs(x - (touchStartXRef.current ?? x));
          touchAccumRef.current += touchStartYRef.current - y; // finger up = positive accum = scroll forward
          touchStartYRef.current = y;
          touchStartXRef.current = x;
          const steps = Math.trunc(touchAccumRef.current / cellHeight);
          if (steps === 0) return;
          try { term.scrollLines(steps); } catch {}
          touchAccumRef.current -= steps * cellHeight;
          // After the scroll, update follow mode based on where we landed.
          // onScroll handles this too, but updating eagerly here avoids a
          // one-frame flicker of the chip during a quick flick.
          const atBottom = (term.viewportY ?? 0) === 0;
          if (atBottom !== followRef.current) setFollow(atBottom);
        };
        const onTouchEnd = () => {
          // A clean tap (no significant finger travel) arms the keyboard so
          // users don't have to hunt for the toolbar's ⌨️ button. Any
          // scroll / flick registers far more than 8 px of accumulated
          // movement and is excluded. Only acts when the keyboard is
          // currently down — reopening on every tap-while-typing would be
          // disruptive (the original reason auto-focus was removed).
          if (
            touchStartYRef.current !== null &&
            touchMovedTotalRef.current < 8 &&
            !keyboardArmedRef.current &&
            mirrorRef.current
          ) {
            // Must run synchronously inside the touchend handler so iOS
            // attributes the focus() call to the active user gesture.
            try {
              armMirror(mirrorRef.current);
              armMirror(term.textarea);
              armHost(containerRef.current);
              mirrorRef.current.focus({ preventScroll: true });
              keyboardArmedRef.current = true;
              setKeyboardArmed(true);
              seedMirror();
            } catch {}
          }
          touchStartYRef.current = null;
          touchStartXRef.current = null;
          touchAccumRef.current = 0;
          touchMovedTotalRef.current = 0;
        };
        // Expose the handlers + helpers via termRef so the separate
        // mirror-binding useEffect (below) can wire them up after the
        // mirror is in the DOM, on every (re)mount of Terminal.
        termRef.current.__touchHandlers = { onTouchStart, onTouchMove, onTouchEnd };
        containerRef.current.addEventListener('touchstart', onTouchStart, { passive: true });
        // touchmove must be non-passive so onTouchMove can call preventDefault
        // and claim the gesture (see comment in the handler).
        containerRef.current.addEventListener('touchmove', onTouchMove, { passive: false });
        containerRef.current.addEventListener('touchend', onTouchEnd);
        containerRef.current.addEventListener('touchcancel', onTouchEnd);
        cleanups.push(() => {
          containerRef.current?.removeEventListener('touchstart', onTouchStart);
          containerRef.current?.removeEventListener('touchmove', onTouchMove);
          containerRef.current?.removeEventListener('touchend', onTouchEnd);
          containerRef.current?.removeEventListener('touchcancel', onTouchEnd);
        });
        // (Mirror touch-handler binding now lives in a separate useEffect
        // keyed on `isTouch` — see below. Doing it here was unreliable on
        // remount because the cached ghostty load promise resolved before
        // the mirror was rendered after the isTouch state committed.)

        // armMirror / disarmMirror set up the mirror so iOS will (or won't)
        // show the soft keyboard when it's focused. iOS's "show keyboard"
        // heuristic only fires when ALL of these are true at the moment of
        // focus():
        //   * editable: not [disabled] and not [readonly]
        //   * has a usable inputmode (not "none")
        //   * pointer-events on the element are interactive (NOT "none")
        //   * caret is visible (caret-color not transparent)
        //   * call is inside an active user gesture
        // The mirror is normally `pointer-events: none` + transparent caret
        // so it doesn't intercept touch and stays invisible. We flip both
        // when arming so iOS treats it as a real focus target.
        const armMirror = (n) => {
          if (!n) return;
          try {
            n.removeAttribute('readonly');
            n.setAttribute('inputmode', 'text');
            n.style.pointerEvents = 'auto';
            n.style.caretColor = 'transparent'; // still hide the caret visually
            // but keep readonly off + inputmode=text so the keyboard shows
          } catch {}
        };
        const disarmMirror = (n) => {
          if (!n) return;
          try {
            n.setAttribute('readonly', '');
            n.setAttribute('inputmode', 'none');
            n.style.pointerEvents = 'none';
          } catch {}
        };
        // armHost / disarmHost gate ghostty's own host element. ghostty makes
        // the host `contenteditable=true` + `tabindex=0` so its WASM input
        // pipeline can receive keystrokes. iOS treats that as an editable
        // surface and will focus it on touchstart (even during a scroll
        // gesture), which pops the keyboard. Turning contenteditable off
        // when the user hasn't asked for the keyboard prevents that.
        const armHost = (host) => {
          if (!host) return;
          try {
            host.setAttribute('contenteditable', 'true');
            host.setAttribute('tabindex', '0');
          } catch {}
        };
        const disarmHost = (host) => {
          if (!host) return;
          try {
            host.setAttribute('contenteditable', 'false');
            host.setAttribute('tabindex', '-1');
          } catch {}
        };

        // Initial disarmed state for ghostty's own surfaces. The mirror's
        // JSX already starts with inputMode="none" + readOnly via React.
        const ghosttyTextarea = term.textarea
          || containerRef.current?.querySelector('textarea');
        disarmMirror(ghosttyTextarea);
        disarmHost(containerRef.current);

        const focusTerm = () => {
          if (!mirrorRef.current) {
            // Desktop: no mirror, focus ghostty's textarea (or term).
            const ta = term.textarea;
            if (ta && typeof ta.focus === 'function') ta.focus();
            else term.focus();
            return;
          }
          // Touch path — must run ENTIRELY synchronously inside the click
          // handler so iOS counts focus() as inside the active user gesture.
          armMirror(mirrorRef.current);
          armMirror(term.textarea);
          armHost(containerRef.current);
          try { mirrorRef.current.focus({ preventScroll: true }); } catch {
            try { mirrorRef.current.focus(); } catch {}
          }
          keyboardArmedRef.current = true;
          setKeyboardArmed(true);
          seedMirror();
        };
        const blurTerm = () => {
          keyboardArmedRef.current = false;
          setKeyboardArmed(false);
          // Disarm everything iOS could use as a back door BEFORE blurring,
          // so even if iOS tries to restore focus to ghostty's host on the
          // next touchstart, it sees `contenteditable=false` and stops.
          disarmMirror(mirrorRef.current);
          disarmMirror(term.textarea);
          disarmHost(containerRef.current);
          try { mirrorRef.current?.blur(); } catch {}
          try { if (term.textarea && typeof term.textarea.blur === 'function') term.textarea.blur(); } catch {}
          try {
            const host = containerRef.current;
            if (host && typeof host.blur === 'function') host.blur();
          } catch {}
        };
        const isKeyboardOpen = () => keyboardArmedRef.current;
        // Initial focus: desktop wants the terminal hot for typing immediately,
        // but on touch the soft keyboard should NOT pop up just because the
        // session loaded — wait for the user to explicitly tap the toolbar
        // keyboard button.
        if (!mirrorRef.current) {
          focusTerm();
        }

        const jumpToBottom = () => {
          try { term.scrollToBottom(); } catch {}
          setFollow(true);
        };

        onReady?.({
          write: writeAndPreserveScroll,
          focus: focusTerm,
          blur: blurTerm,
          isKeyboardOpen,
          sendKey: (s) => onInputRef.current?.(s),
          fit: handleResize,
          refit: refitNow,
          jumpToBottom,
        });

        // Initial size handshake so the server knows our dimensions
        // immediately, before any output arrives.
        refitNow();
      })
      .catch((err) => {
        // WASM init failure is rare but worth surfacing.
        console.error('aurex: ghostty-web init failed', err);
      });

    return () => {
      disposed = true;
      for (const fn of cleanups) {
        try { fn(); } catch {}
      }
      if (termRef.current) {
        try { termRef.current.dispose(); } catch {}
      }
      termRef.current = null;
      fitRef.current = null;
      // ghostty.dispose() doesn't always remove the appended canvas + textarea.
      // Clean them out so the host div is empty if React keeps it alive (e.g.,
      // strict mode double-mount in dev).
      if (containerRef.current) {
        while (containerRef.current.firstChild) {
          containerRef.current.removeChild(containerRef.current.firstChild);
        }
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const onJumpClick = () => {
    const term = termRef.current;
    if (term) {
      try { term.scrollToBottom(); } catch {}
    }
    setFollow(true);
  };

  return (
    // No onClick handler on the inner container — ghostty installs
    // mousedown/touchend listeners on its canvas that already call
    // textarea.focus(). React's synthetic onClick here would compete with
    // ghostty's preventDefault on touchend.
    <div className="relative flex-1 min-h-0 bg-bg">
      <div ref={containerRef} className="absolute inset-0" />
      {isTouch && (
        // Hidden input mirror. pointer-events:none lets touch events fall
        // through to the container below for scrolling — the container's
        // touch-end handler is what programmatically focuses this textarea
        // on a tap, which opens the soft keyboard. Everything inside is
        // transparent so the user sees only ghostty's rendering.
        <textarea
          ref={mirrorRef}
          autoCapitalize="off"
          autoComplete="off"
          autoCorrect="on"
          spellCheck={true}
          aria-label="Terminal input"
          // inputMode + readOnly are the levers that keep the soft keyboard
          // down when the user hasn't tapped the ⌨️ toggle. Both iOS and
          // Android refuse to render the soft keyboard for an input with
          // inputmode="none"; readOnly is the belt-and-suspenders fallback
          // for browsers that don't honor inputmode on a textarea.
          inputMode={keyboardArmed ? 'text' : 'none'}
          readOnly={!keyboardArmed}
          className="absolute inset-0 z-10 resize-none border-0 bg-transparent p-0 outline-none"
          style={{
            color: 'transparent',
            caretColor: 'transparent',
            WebkitTextFillColor: 'transparent',
            pointerEvents: 'none',
            font: 'inherit',
          }}
          onInput={handleMirrorInput}
          onKeyDown={handleMirrorKeyDown}
          onKeyUp={handleMirrorKeyUp}
        />
      )}
      {DEBUG_INPUT && (
        <div className="absolute left-1 top-1 z-20 max-w-[95%] rounded border border-amber-500/50 bg-black/85 p-1.5 font-mono text-[10px] leading-tight text-amber-200">
          <div className="mb-1 flex items-center gap-2">
            <span className="text-amber-400">input debug ({dbgLogRef.current.length})</span>
            <button
              type="button"
              tabIndex={-1}
              onMouseDown={(e) => e.preventDefault()}
              onTouchStart={(e) => e.preventDefault()}
              onClick={() => {
                const txt = dbgLogRef.current.map((e) => {
                  if (e.kind === 'mi') return `mi ${e.it || '?'} ol=${e.ol} nl=${e.nl} bs=${e.bs} ad=${JSON.stringify(e.ad)}`;
                  if (e.kind === 'mbi') return `mbi ${e.it} d=${JSON.stringify(e.data)}`;
                  if (e.kind === 'mkd') return `mkd ${JSON.stringify(e.key)} (${e.code})`;
                  if (e.kind === 'bi') return `bi ${e.it} d=${JSON.stringify(e.data)} rng=${e.rng} wl=${e.wl}${e.cmp ? ' cmp' : ''}${e.rk ? ' rk' : ''}`;
                  if (e.kind === 'keydown') return `kd ${JSON.stringify(e.key)} (${e.code})`;
                  if (e.kind === 'compStart') return `compStart d=${JSON.stringify(e.data)} wl=${e.wl}`;
                  if (e.kind === 'compEnd') return `compEnd d=${JSON.stringify(e.data)} wl=${e.wl}`;
                  return JSON.stringify(e);
                }).join('\n');
                navigator.clipboard?.writeText(txt).catch(() => {});
              }}
              className="rounded border border-amber-400/60 px-1.5 py-0.5 text-amber-300 active:bg-amber-500/20"
            >
              copy
            </button>
            <button
              type="button"
              tabIndex={-1}
              onMouseDown={(e) => e.preventDefault()}
              onTouchStart={(e) => e.preventDefault()}
              onClick={() => { dbgLogRef.current = []; dbgTick(); }}
              className="rounded border border-amber-400/60 px-1.5 py-0.5 text-amber-300 active:bg-amber-500/20"
            >
              clear
            </button>
          </div>
          {dbgLogRef.current.slice(-12).map((e, i) => (
            <div key={i} className="whitespace-pre">
              {e.kind === 'mi'
                ? `mi ${e.it || '?'} ol=${e.ol} nl=${e.nl} bs=${e.bs} ad=${JSON.stringify(e.ad)}`
                : e.kind === 'mbi'
                ? `mbi ${e.it} d=${JSON.stringify(e.data)}`
                : e.kind === 'mkd'
                ? `mkd ${JSON.stringify(e.key)} (${e.code})`
                : e.kind === 'bi'
                ? `bi ${e.it} d=${JSON.stringify(e.data)} rng=${e.rng} wl=${e.wl}${e.cmp ? ' cmp' : ''}${e.rk ? ' rk' : ''}`
                : e.kind === 'keydown'
                ? `kd ${JSON.stringify(e.key)} (${e.code})`
                : e.kind === 'compStart'
                ? `compStart d=${JSON.stringify(e.data)} wl=${e.wl}`
                : e.kind === 'compEnd'
                ? `compEnd d=${JSON.stringify(e.data)} wl=${e.wl}`
                : JSON.stringify(e)}
            </div>
          ))}
        </div>
      )}
      {!followBottom && (
        <button
          type="button"
          tabIndex={-1}
          onMouseDown={(e) => e.preventDefault()}
          onTouchStart={(e) => e.preventDefault()}
          onClick={onJumpClick}
          aria-label="Jump to bottom"
          className="absolute bottom-3 right-3 z-10 flex items-center gap-1 rounded-full border border-aura/40 bg-aura/15 px-3 py-1.5 text-xs font-mono text-aura shadow-lg backdrop-blur active:bg-aura/25"
        >
          <span aria-hidden="true">↓</span>
          <span>jump to bottom</span>
        </button>
      )}
    </div>
  );
}
