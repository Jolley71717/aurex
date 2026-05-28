import { useState } from 'react';

// Trimmed set: only the keys that physical phone keyboards don't have or are
// awkward to reach. Anything else (symbols, Pg keys, copy-mode shortcuts) is
// either typeable on the keyboard or handled by the scrollback overlay.
const KEYS = [
  { label: 'ESC', seq: '\x1b' },
  { label: 'TAB', seq: '\t' },
  { label: '↑',   seq: '\x1b[A' },
  { label: '↓',   seq: '\x1b[B' },
  { label: '←',   seq: '\x1b[D' },
  { label: '→',   seq: '\x1b[C' },
];

// When CTRL is sticky, pressing a letter sends the corresponding control code.
function ctrlOf(ch) {
  if (!ch || ch.length !== 1) return null;
  const code = ch.toLowerCase().charCodeAt(0);
  if (code < 97 || code > 122) return null;
  return String.fromCharCode(code - 96);
}

/**
 * Toolbar. Buttons use tabIndex=-1 plus preventDefault on mousedown/touchstart
 * so they can never steal focus from xterm's hidden textarea — the soft
 * keyboard stays up across taps on mobile.
 *
 * `desktop` mode renders only the PASTE button (desktop keyboards have
 * ESC/TAB/arrows/CTRL natively, but Cmd+V can't transport clipboard images
 * so PASTE is still useful for screenshots).
 *
 * The rightmost button (when `onToggleKeyboard` is passed) is a keyboard
 * toggle (⌨) that explicitly opens or dismisses the soft keyboard. Tapping
 * the terminal area no longer auto-focuses the mirror, so the toggle is the
 * user's only path to typing on mobile.
 */
export default function Toolbar({ onSendKey, onCtrlArm, ctrlArmed, onToggleKeyboard, desktop = false }) {
  const [pressed, setPressed] = useState(null);
  // pasteStatus: null | 'ok' | 'empty' | 'denied' | 'unsupported'
  // Drives the brief flash colour on the Paste button so the user gets
  // feedback when iOS denies clipboard access or the buffer is empty —
  // a silent no-op is the worst UX.
  const [pasteStatus, setPasteStatus] = useState(null);

  const handleTap = (k) => {
    setPressed(k.label);
    setTimeout(() => setPressed(null), 80);
    onSendKey(k.seq);
  };

  const handleKeyboardToggle = () => {
    // Briefly flash the button so the user knows the tap registered, even
    // when the keyboard takes a moment to animate in/out.
    setPressed('KBD');
    setTimeout(() => setPressed(null), 80);
    onToggleKeyboard?.();
  };

  const noFocusSteal = (e) => e.preventDefault();

  const handlePaste = async () => {
    // Modern Clipboard API requires a secure context (https/localhost) AND a
    // user gesture (this click counts). iOS Safari additionally pops its own
    // "Allow Paste" prompt the first time per session.
    //
    // We try navigator.clipboard.read() FIRST so we can offer image-paste —
    // it returns ClipboardItem[] with MIME types. Fall back to readText()
    // if read() isn't there (older browsers).
    if (!navigator.clipboard) {
      setPasteStatus('unsupported');
      setTimeout(() => setPasteStatus(null), 1500);
      return;
    }

    const flashAndClear = (status, ms = 1500) => {
      setPasteStatus(status);
      setTimeout(() => setPasteStatus(null), ms);
    };

    try {
      // ClipboardItem path — picks up images.
      if (navigator.clipboard.read) {
        const items = await navigator.clipboard.read();
        for (const item of items) {
          // Prefer image types if present (the screenshot case).
          const imgType = item.types.find((t) => t.startsWith('image/'));
          if (imgType) {
            const blob = await item.getType(imgType);
            const res = await fetch('/api/paste/image', {
              method: 'POST',
              headers: { 'Content-Type': blob.type || 'image/png' },
              body: blob,
              credentials: 'include',
            });
            if (!res.ok) {
              flashAndClear('uploaderr');
              return;
            }
            const data = await res.json().catch(() => null);
            if (!data || !data.path) {
              flashAndClear('uploaderr');
              return;
            }
            // Type the absolute path into the terminal so downstream tools
            // (Claude Code, vim+iTerm-image-render, etc.) can open it. The
            // trailing space gives the shell a delimiter so the path doesn't
            // glue onto whatever's typed next.
            onSendKey(data.path + ' ');
            flashAndClear('ok', 600);
            return;
          }
        }
        // No image item — fall through to text inspection.
        for (const item of items) {
          if (item.types.includes('text/plain')) {
            const blob = await item.getType('text/plain');
            const text = await blob.text();
            if (!text) {
              flashAndClear('empty', 1200);
              return;
            }
            onSendKey(text);
            flashAndClear('ok', 600);
            return;
          }
        }
        flashAndClear('empty', 1200);
        return;
      }

      // Fallback path — older browsers that only ship readText.
      if (!navigator.clipboard.readText) {
        flashAndClear('unsupported');
        return;
      }
      const text = await navigator.clipboard.readText();
      if (!text) {
        flashAndClear('empty', 1200);
        return;
      }
      onSendKey(text);
      flashAndClear('ok', 600);
    } catch (err) {
      // iOS shows the permission prompt and throws here if user taps Deny,
      // or if no user gesture is registered. Either way: tell them.
      flashAndClear('denied');
    }
  };

  const pasteLabel = (() => {
    switch (pasteStatus) {
      case 'ok':          return 'Pasted';
      case 'empty':       return 'Empty';
      case 'denied':      return 'Denied';
      case 'unsupported': return 'N/A';
      case 'uploaderr':   return 'Upload?';
      default:            return 'PASTE';
    }
  })();
  const pasteTone = pasteStatus === 'ok'
    ? 'border-aura bg-aura/15 text-aura'
    : pasteStatus === 'denied' || pasteStatus === 'unsupported' || pasteStatus === 'uploaderr'
    ? 'border-red-500 bg-red-500/15 text-red-300'
    : pasteStatus === 'empty'
    ? 'border-yellow-500 bg-yellow-500/15 text-yellow-300'
    : 'border-line bg-bg text-zinc-200 active:bg-zinc-800';

  return (
    <div
      className="flex shrink-0 flex-wrap items-center gap-1 border-t border-line bg-panel px-2 py-2"
      style={{ paddingBottom: 'max(0.5rem, env(safe-area-inset-bottom))' }}
    >
      {!desktop && (
        <button
          tabIndex={-1}
          onMouseDown={noFocusSteal}
          onTouchStart={noFocusSteal}
          onClick={onCtrlArm}
          className={[
            'shrink-0 rounded-md border px-3 py-2 text-xs font-mono',
            ctrlArmed
              ? 'border-aura bg-aura/15 text-aura'
              : 'border-line bg-bg text-zinc-200 active:bg-zinc-800',
          ].join(' ')}
        >
          CTRL
        </button>
      )}
      <button
        tabIndex={-1}
        onMouseDown={noFocusSteal}
        onTouchStart={noFocusSteal}
        onClick={handlePaste}
        className={[
          'shrink-0 rounded-md border px-3 py-2 text-xs font-mono',
          pasteTone,
        ].join(' ')}
      >
        {pasteLabel}
      </button>
      {!desktop && KEYS.map((k) => (
        <button
          key={k.label}
          tabIndex={-1}
          onMouseDown={noFocusSteal}
          onTouchStart={noFocusSteal}
          onClick={() => handleTap(k)}
          className={[
            'shrink-0 rounded-md border px-3 py-2 text-xs font-mono',
            pressed === k.label
              ? 'border-aura bg-aura/15 text-aura'
              : 'border-line bg-bg text-zinc-200 active:bg-zinc-800',
          ].join(' ')}
        >
          {k.label}
        </button>
      ))}
      {onToggleKeyboard && (
        <button
          tabIndex={-1}
          onMouseDown={noFocusSteal}
          onTouchStart={noFocusSteal}
          onClick={handleKeyboardToggle}
          aria-label="Show or hide keyboard"
          title="Show / hide keyboard"
          className={[
            'shrink-0 rounded-md border px-3 py-2 text-xs font-mono',
            pressed === 'KBD'
              ? 'border-aura bg-aura/15 text-aura'
              : 'border-line bg-bg text-zinc-200 active:bg-zinc-800',
          ].join(' ')}
        >
          ⌨️
        </button>
      )}
    </div>
  );
}

export { ctrlOf };
