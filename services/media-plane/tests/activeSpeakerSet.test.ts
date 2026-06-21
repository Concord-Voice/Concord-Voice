import { describe, it, expect } from 'vitest';
import { ActiveSpeakerSet } from '../src/lib/activeSpeakerSet.js';

describe('ActiveSpeakerSet', () => {
  it('admits a speaker immediately (added on first appearance)', () => {
    const s = new ActiveSpeakerSet(8, 2500);
    const d = s.update(['p1'], 1000);
    expect(d.added).toEqual(['p1']);
    expect(d.removed).toEqual([]);
    expect([...s.current()]).toEqual(['p1']);
  });

  it('keeps a briefly-silent speaker (< hold) — no flap', () => {
    const s = new ActiveSpeakerSet(8, 2500);
    s.update(['p1'], 1000);
    const d = s.update([], 2000); // 1000ms silence < 2500ms hold
    expect(d.removed).toEqual([]);
    expect([...s.current()]).toEqual(['p1']);
  });

  it('evicts a speaker after the hold expires', () => {
    const s = new ActiveSpeakerSet(8, 2500);
    s.update(['p1'], 1000);
    const d = s.update([], 1000 + 2500 + 1); // just past hold
    expect(d.removed).toEqual(['p1']);
    expect([...s.current()]).toEqual([]);
  });

  it('never exceeds capacity — displaces the longest-silent when full', () => {
    const s = new ActiveSpeakerSet(2, 2500);
    s.update(['a'], 1000);
    s.update(['a', 'b'], 1100); // full: a(1100 via refresh? no — a not in ranked) ...
    // refresh order: at 1100 ranked=[a,b] -> both lastSeen=1100; size 2 == cap.
    const d = s.update(['b', 'c'], 1200); // a now silent, c new; cap 2
    // a lastSeen=1100 (oldest), b=1200, c=1200 -> size 3 -> evict oldest (a)
    expect(s.current().size).toBe(2);
    expect(s.current().has('a')).toBe(false);
    expect(s.current().has('c')).toBe(true);
    expect(d.added).toContain('c');
    expect(d.removed).toContain('a');
  });

  it('full set evicts immediately on contention (no hold when no slack)', () => {
    const s = new ActiveSpeakerSet(1, 2500);
    s.update(['a'], 1000);
    const d = s.update(['b'], 1100); // a silent but cap=1 and b is loud -> a evicted now
    expect([...s.current()]).toEqual(['b']);
    expect(d.removed).toEqual(['a']);
    expect(d.added).toEqual(['b']);
  });

  it('remove() drops a producer (producer-close cleanup)', () => {
    const s = new ActiveSpeakerSet(8, 2500);
    s.update(['p1', 'p2'], 1000);
    s.remove('p1');
    expect([...s.current()].sort()).toEqual(['p2']);
  });

  it('re-appearing within hold refreshes (does not double-add)', () => {
    const s = new ActiveSpeakerSet(8, 2500);
    s.update(['p1'], 1000);
    const d = s.update(['p1'], 1500);
    expect(d.added).toEqual([]);
    expect(d.removed).toEqual([]);
  });
});
