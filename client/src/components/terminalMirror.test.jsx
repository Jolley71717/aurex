import { describe, it, expect } from 'vitest';
import {
  diffToPayload,
  mirrorPad,
  stripPad,
  padCount,
  MIRROR_PAD_CH,
  MIRROR_PAD_LEN,
} from './terminalMirror.js';

// These cover the pure core of the mobile input mirror — the diff that turns a
// textarea change into a PTY payload, and the zero-width pad helpers that make
// iOS press-and-hold delete repeat (see terminalMirror.js for the why).

const PAD = MIRROR_PAD_CH;

describe('diffToPayload', () => {
  it('emits added tail characters when typing', () => {
    expect(diffToPayload('hel', 'hell')).toBe('l');
    expect(diffToPayload('', 'hi')).toBe('hi');
  });

  it('emits one \\x7f per character removed from the tail', () => {
    expect(diffToPayload('hello', 'hell')).toBe('\x7f');
    expect(diffToPayload('hello', 'he')).toBe('\x7f\x7f\x7f');
  });

  it('emits nothing when nothing changed (pad reseed is a no-op)', () => {
    expect(diffToPayload('abc', 'abc')).toBe('');
    expect(diffToPayload(mirrorPad(), mirrorPad())).toBe('');
  });

  it('treats removed pad chars exactly like removed real chars', () => {
    // The heart of the fix: with an otherwise-empty mirror, each held-delete
    // tick removes one pad char and must produce exactly one backspace.
    const five = PAD.repeat(5);
    const four = PAD.repeat(4);
    expect(diffToPayload(five, four)).toBe('\x7f');
  });

  it('handles a tail replace (autocorrect) as delete-back + retype', () => {
    // "teh" -> "the": common prefix "t", remove "eh", add "he".
    expect(diffToPayload('teh', 'the')).toBe('\x7f\x7fhe');
  });

  it('never emits the pad as additions when typing after a padded prefix', () => {
    const before = mirrorPad() + 'ls';
    const after = mirrorPad() + 'ls ';
    expect(diffToPayload(before, after)).toBe(' ');
  });
});

describe('pad helpers', () => {
  it('mirrorPad builds a run of the configured length', () => {
    expect(mirrorPad().length).toBe(MIRROR_PAD_LEN);
    expect(mirrorPad(3)).toBe(PAD.repeat(3));
  });

  it('stripPad removes only the leading pad run, keeping user text', () => {
    expect(stripPad(mirrorPad() + 'echo hi')).toBe('echo hi');
    expect(stripPad('echo hi')).toBe('echo hi');
    expect(stripPad('')).toBe('');
  });

  it('padCount counts the leading pad run', () => {
    expect(padCount(PAD.repeat(7) + 'x')).toBe(7);
    expect(padCount('x')).toBe(0);
  });
});
