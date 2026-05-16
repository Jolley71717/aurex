import { useEffect, useRef } from 'react';
import { FitAddon, Ghostty, Terminal as GhosttyTerminal } from 'ghostty-web';

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

  // Stale-closure bridge: onInput changes when ctrlArmed flips, and ghostty's
  // onData is registered once at mount. Without the ref the CTRL toolbar key
  // would never transform the next keystroke.
  const onInputRef = useRef(onInput);
  const onResizeRef = useRef(onResize);
  useEffect(() => { onInputRef.current = onInput; }, [onInput]);
  useEffect(() => { onResizeRef.current = onResize; }, [onResize]);

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
          // can run Compose-for-Web. Pair this with the 32 MiB server-side
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

        // NOTE: do NOT set tabIndex on the container. ghostty.open() puts
        // tabindex=0 + contenteditable=true on it as part of how its
        // InputHandler routes keys to WASM. Overriding either breaks input.

        // Mobile soft-keyboard input fix.
        //
        // ghostty-web 0.4 only routes characters via the keydown path. Most
        // Android soft keyboards (Gboard especially) don't fire keydown for
        // printable characters — they only fire beforeinput / input. Ghostty
        // blocks beforeinput with preventDefault but never reads e.data, so
        // mobile typing silently drops every character.
        //
        // Workaround: read e.data ourselves on beforeinput and send it via
        // onInput. Dedupe by checking whether a recent keydown carried a real
        // key — when it did (desktop / hardware keyboard) ghostty already
        // handled it; when it didn't (mobile) we send.
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
        const onBeforeInput = (e) => {
          const recentRealKey = performance.now() - lastKeyAt < 50 && lastKeyHadValue;
          if (recentRealKey) return;
          switch (e.inputType) {
            case 'insertText':
            case 'insertFromPaste':
            case 'insertReplacementText':
              if (e.data) onInputRef.current?.(e.data);
              break;
            case 'insertLineBreak':
            case 'insertParagraph':
              onInputRef.current?.('\r');
              break;
            case 'deleteContentBackward':
              onInputRef.current?.('\x7f');
              break;
            case 'deleteContentForward':
              onInputRef.current?.('\x1b[3~');
              break;
            // insertCompositionText is handled by ghostty's compositionend path.
          }
        };
        host.addEventListener('keydown', onKeyDownCap, true);
        host.addEventListener('beforeinput', onBeforeInput);
        cleanups.push(() => {
          host.removeEventListener('keydown', onKeyDownCap, true);
          host.removeEventListener('beforeinput', onBeforeInput);
        });

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
        //
        // "Stick to user's position" behavior: while the user has scrolled
        // away from the bottom, suppress the auto-scroll that term.write
        // does on every new chunk. Without this, every output frame snaps
        // the viewport back to the bottom — making scrollback unusable on
        // mobile when an agent is streaming output.
        let touchStartY = null;
        let touchAccum = 0;
        const cellHeight = fontSize * 1.2;
        // `userScrollAwayUntilTs` is a wall-clock deadline. While now < it,
        // term.write is wrapped to preserve the user's viewport position.
        // The deadline is pushed forward on every touchmove that scrolls up,
        // and is cleared when the user scrolls back to the bottom.
        let userScrollAwayUntilTs = 0;
        const STICK_DECAY_MS = 5_000;
        const isAtBottom = () => {
          try {
            const buf = term.buffer?.active;
            if (!buf) return true;
            // viewportY === baseY means scrollback is at the live cursor row.
            return buf.viewportY >= buf.baseY;
          } catch {
            return true;
          }
        };
        termRef.current = term;
        const onTouchStart = (e) => {
          if (e.touches.length !== 1) return;
          touchStartY = e.touches[0].clientY;
          touchAccum = 0;
        };
        const onTouchMove = (e) => {
          if (touchStartY === null || e.touches.length !== 1) return;
          const y = e.touches[0].clientY;
          touchAccum += touchStartY - y; // finger up = positive accum = scroll forward
          touchStartY = y;
          const steps = Math.trunc(touchAccum / cellHeight);
          if (steps === 0) return;
          try { term.scrollLines(steps); } catch {}
          touchAccum -= steps * cellHeight;
          // After scrollLines, check whether the user is still scrolled up.
          // If so, pin the "stick to position" window forward; if they're
          // back at the bottom, drop the pin so auto-scroll resumes.
          if (isAtBottom()) {
            userScrollAwayUntilTs = 0;
          } else {
            userScrollAwayUntilTs = Date.now() + STICK_DECAY_MS;
          }
        };
        const onTouchEnd = () => {
          touchStartY = null;
          touchAccum = 0;
        };
        containerRef.current.addEventListener('touchstart', onTouchStart, { passive: true });
        containerRef.current.addEventListener('touchmove', onTouchMove, { passive: true });
        containerRef.current.addEventListener('touchend', onTouchEnd);
        containerRef.current.addEventListener('touchcancel', onTouchEnd);
        cleanups.push(() => {
          containerRef.current?.removeEventListener('touchstart', onTouchStart);
          containerRef.current?.removeEventListener('touchmove', onTouchMove);
          containerRef.current?.removeEventListener('touchend', onTouchEnd);
          containerRef.current?.removeEventListener('touchcancel', onTouchEnd);
        });

        // Focus the actual textarea ghostty places inside the host. On mobile,
        // term.focus() alone can fail to open the soft keyboard because the
        // browser requires the focus to land on a real input element from a
        // user gesture — direct .focus() on the textarea reliably triggers it.
        const focusTerm = () => {
          const ta = term.textarea;
          if (ta && typeof ta.focus === 'function') {
            ta.focus();
          } else {
            term.focus();
          }
        };
        focusTerm();

        // Wrapper around term.write that preserves the user's scrollback
        // position when they've scrolled away from the bottom. ghostty's
        // term.write moves the cursor and (for output that wraps the last
        // row) emits scrolls that snap viewportY=baseY. By capturing
        // viewportY pre-write and restoring it post-write while the
        // "stick" deadline is active, the user's history view stays put
        // through a stream of incoming chunks.
        const writeKeepingPosition = (s) => {
          if (Date.now() >= userScrollAwayUntilTs) {
            // Either user is at bottom or the stick window has decayed —
            // let normal auto-scroll happen.
            term.write(s);
            return;
          }
          let savedViewportY = null;
          try {
            savedViewportY = term.buffer?.active?.viewportY ?? null;
          } catch {}
          term.write(s);
          if (savedViewportY != null) {
            try {
              const baseY = term.buffer?.active?.baseY ?? 0;
              const target = Math.min(savedViewportY, baseY);
              const delta = target - (term.buffer?.active?.viewportY ?? target);
              if (delta !== 0) term.scrollLines(delta);
            } catch {}
          }
        };

        onReady?.({
          write: writeKeepingPosition,
          focus: focusTerm,
          sendKey: (s) => onInputRef.current?.(s),
          fit: handleResize,
          refit: refitNow,
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

  return (
    // No onClick handler — ghostty installs mousedown/touchend listeners on
    // its canvas that already call textarea.focus(). React's synthetic onClick
    // here would compete with ghostty's preventDefault on touchend.
    <div
      ref={containerRef}
      className="flex-1 min-h-0 bg-bg"
    />
  );
}
