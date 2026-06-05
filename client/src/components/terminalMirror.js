// Pure helpers for the mobile terminal input mirror (see Terminal.jsx).
//
// Touch typing is captured in a hidden, transparent <textarea> ("the mirror").
// After each change we diff the textarea's value against what we've already
// sent and emit only the delta. These functions are the keyboard-agnostic core
// of that path, factored out so they can be unit-tested directly (the inline
// versions in Terminal.jsx historically could only be tested by re-implementing
// them — see Terminal.backspace.test.jsx).
//
// THE PAD: the mirror is kept prefixed with a run of zero-width characters.
// This is what makes the iOS soft-keyboard delete key auto-repeat work when
// erasing text that lives on the *server's* prompt (so the mirror would
// otherwise be empty). iOS soft keys do NOT emit repeated `keydown` events on
// press-and-hold the way a hardware key does — the OS instead repeats the
// delete *action* at the input level, but only while there is something left
// in the field to delete. With an empty field it deletes once and stops (the
// "one char then stops" bug). By always leaving zero-width pad chars to the
// left of the caret, every auto-repeat tick removes one pad char, fires an
// `input` event, and we translate that into one `\x7f` to the PTY.

// Zero-width space: invisible, and acts as a word boundary so it doesn't fuse
// with the user's first real word (which would confuse autocorrect and
// word-delete). The textarea is transparent anyway, so it's never seen.
export const MIRROR_PAD_CH = '​';
export const MIRROR_PAD_LEN = 200;
// Below this many remaining pad chars we top the pad back up (see Terminal.jsx).
export const MIRROR_PAD_MIN = 20;

export const mirrorPad = (n = MIRROR_PAD_LEN) => MIRROR_PAD_CH.repeat(n);

// Strip the leading pad run, returning just the user-typed text.
export const stripPad = (v) => String(v || '').replace(/^​+/, '');

// Count the leading pad run.
export const padCount = (v) => String(v || '').length - stripPad(v).length;

// Diff oldVal → newVal into a terminal payload: one \x7f per character removed
// from the tail, followed by any characters added at the tail. Uses a
// longest-common-prefix diff, which models the single tail edit that a soft
// keyboard (typing, single/word/line delete, or autocorrect replace) produces.
// Removed pad chars count as backspaces just like removed real chars — that's
// exactly how a held delete becomes a stream of \x7f.
export function diffToPayload(oldVal, newVal) {
  const o = String(oldVal || '');
  const n = String(newVal || '');
  let i = 0;
  const minLen = Math.min(o.length, n.length);
  while (i < minLen && o.charCodeAt(i) === n.charCodeAt(i)) i++;
  const backspaces = o.length - i;
  const additions = n.slice(i);
  let payload = '';
  if (backspaces > 0) payload = '\x7f'.repeat(backspaces);
  payload += additions;
  return payload;
}
