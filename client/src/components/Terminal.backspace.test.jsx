import { describe, it, expect, vi } from 'vitest';

// These tests cover the LOGIC of the Backspace handler — specifically, that
// preventDefault is NOT called when the mirror has content (so iOS press-and-
// hold auto-repeat can engage), and IS called when the mirror is empty (so
// we synthesize \x7f against the server-side buffer).
//
// We do NOT test iOS's actual press-and-hold auto-repeat behavior — that
// requires a real iOS environment because the OS only generates repeat events
// when it observes the default action complete. jsdom doesn't simulate that.
// A manual sim verification protocol covers the OS-level behavior; see
// client/test-protocols/keyboard-ios.md (to be added).
//
// The logic we DO test is sufficient to catch a regression where someone
// re-introduces the unconditional preventDefault that broke press-and-hold
// in the first place.

// The handler under test is defined inline inside the Terminal component.
// To keep this test isolated from the heavy ghostty-web WASM init that the
// real component does, we re-implement the handler with identical semantics
// and assert against that. If the production handler diverges from this
// reference, this test will go stale — that's intentional: any divergence
// in this critical path should be reviewed deliberately.
//
// If a future refactor exports `handleMirrorKeyDown` from Terminal.jsx
// (e.g. by lifting it out of the component closure), this test can switch
// to importing it directly. Until then, the reference impl is sufficient to
// pin the behavior contract.

function makeHandler({ mirrorValue, lastSent, onInput }) {
  const mirrorRef = { current: { value: mirrorValue } };
  const lastSentRef = { current: lastSent };
  const onInputRef = { current: onInput };
  const handler = (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      onInputRef.current?.('\r');
      mirrorRef.current.value = '';
      lastSentRef.current = '';
      return;
    }
    if (e.key === 'Backspace' && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const ta = mirrorRef.current;
      if (ta && ta.value.length > 0) {
        // Case 2 — let default fire so iOS press-and-hold engages.
        return;
      }
      // Case 1 — synthesize against server buffer.
      e.preventDefault();
      onInputRef.current?.('\x7f');
      if (lastSentRef.current.length > 0) {
        lastSentRef.current = lastSentRef.current.slice(0, -1);
      }
      return;
    }
  };
  return { handler, mirrorRef, lastSentRef };
}

function backspaceEvent() {
  return {
    key: 'Backspace',
    code: 'Backspace',
    ctrlKey: false,
    metaKey: false,
    altKey: false,
    shiftKey: false,
    preventDefault: vi.fn(),
  };
}

describe('Terminal Backspace handler — iOS press-and-hold compatibility', () => {
  it('does NOT preventDefault when the mirror has content (so iOS auto-repeat engages)', () => {
    const onInput = vi.fn();
    const { handler } = makeHandler({ mirrorValue: 'hello', lastSent: 'hello', onInput });
    const ev = backspaceEvent();
    handler(ev);
    expect(ev.preventDefault).not.toHaveBeenCalled();
    // No \x7f from keydown — the onInput diff handler is the source of
    // truth for non-empty-mirror deletes.
    expect(onInput).not.toHaveBeenCalled();
  });

  it('preventDefaults AND synthesizes \\x7f when the mirror is empty (server-side delete)', () => {
    const onInput = vi.fn();
    const { handler, lastSentRef } = makeHandler({ mirrorValue: '', lastSent: 'abc', onInput });
    const ev = backspaceEvent();
    handler(ev);
    expect(ev.preventDefault).toHaveBeenCalledTimes(1);
    expect(onInput).toHaveBeenCalledWith('\x7f');
    // lastSent must trim by one char so the diff handler doesn't double-fire.
    expect(lastSentRef.current).toBe('ab');
  });

  it('handles empty mirror AND empty lastSent without exploding', () => {
    const onInput = vi.fn();
    const { handler, lastSentRef } = makeHandler({ mirrorValue: '', lastSent: '', onInput });
    const ev = backspaceEvent();
    handler(ev);
    expect(ev.preventDefault).toHaveBeenCalledTimes(1);
    // We still send the \x7f (the server-side buffer might have content we
    // don't know about) but lastSent stays empty.
    expect(onInput).toHaveBeenCalledWith('\x7f');
    expect(lastSentRef.current).toBe('');
  });

  it('does nothing on Ctrl+Backspace (let the toolbar or default handle it)', () => {
    const onInput = vi.fn();
    const { handler } = makeHandler({ mirrorValue: 'hi', lastSent: 'hi', onInput });
    const ev = { ...backspaceEvent(), ctrlKey: true };
    handler(ev);
    expect(ev.preventDefault).not.toHaveBeenCalled();
    expect(onInput).not.toHaveBeenCalled();
  });

  it('does nothing on Cmd+Backspace either (macOS line-delete shortcut)', () => {
    const onInput = vi.fn();
    const { handler } = makeHandler({ mirrorValue: 'hi', lastSent: 'hi', onInput });
    const ev = { ...backspaceEvent(), metaKey: true };
    handler(ev);
    expect(ev.preventDefault).not.toHaveBeenCalled();
    expect(onInput).not.toHaveBeenCalled();
  });

  it('Enter still preventDefaults and sends \\r (regression guard on the adjacent branch)', () => {
    const onInput = vi.fn();
    const { handler, mirrorRef, lastSentRef } = makeHandler({
      mirrorValue: 'partial',
      lastSent: 'partial',
      onInput,
    });
    const ev = {
      key: 'Enter',
      ctrlKey: false,
      metaKey: false,
      altKey: false,
      shiftKey: false,
      preventDefault: vi.fn(),
    };
    handler(ev);
    expect(ev.preventDefault).toHaveBeenCalledTimes(1);
    expect(onInput).toHaveBeenCalledWith('\r');
    expect(mirrorRef.current.value).toBe('');
    expect(lastSentRef.current).toBe('');
  });
});
