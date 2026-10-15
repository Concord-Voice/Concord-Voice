import { describe, it, expect } from 'vitest';
import {
  evaluateEdit,
  getCounterAnnouncement,
  getCounterClass,
  getCounterLevel,
  getOverLimitReason,
  isCounterVisible,
} from '@/renderer/utils/messaging/messageCharLimit';

// Free is 5120 (75% = 3840, 90% = 4608, 95% = 4864); premium is 10240 (75% = 7680).
const FREE = 5120;
const PREMIUM = 10240;

describe('isCounterVisible', () => {
  it.each([
    [FREE, 3839, false],
    [FREE, 3840, true],
    [FREE, 5121, true],
    [PREMIUM, 7679, false],
    [PREMIUM, 7680, true],
  ])('limit %i, %i chars -> %s', (limit, chars, visible) => {
    expect(isCounterVisible(chars, limit)).toBe(visible);
  });

  it('scales to a small override limit (floor of 75%)', () => {
    // floor(10 * 0.75) = 7
    expect(isCounterVisible(6, 10)).toBe(false);
    expect(isCounterVisible(7, 10)).toBe(true);
  });
});

describe('getCounterLevel / getCounterClass', () => {
  it.each([
    [0, 'normal', 'counter'],
    [4863, 'normal', 'counter'],
    [4864, 'warn', 'counter warn'],
    [FREE, 'warn', 'counter warn'], // exactly AT the limit is not over it
    [5121, 'error', 'counter error'],
  ])('free limit, %i chars -> %s', (chars, level, className) => {
    expect(getCounterLevel(chars, FREE)).toBe(level);
    expect(getCounterClass(chars, FREE)).toBe(className);
  });

  it('uses the active limit, not a fixed one', () => {
    expect(getCounterLevel(5121, PREMIUM)).toBe('normal');
    expect(getCounterLevel(9728, PREMIUM)).toBe('warn'); // floor(10240 * 0.95)
    expect(getCounterLevel(10241, PREMIUM)).toBe('error');
  });
});

describe('getCounterAnnouncement', () => {
  it('is empty below 75%', () => {
    expect(getCounterAnnouncement(0, FREE)).toBe('');
    expect(getCounterAnnouncement(3839, FREE)).toBe('');
  });

  it('announces 75%, 90% and at-limit bands with the active limit', () => {
    expect(getCounterAnnouncement(3840, FREE)).toBe(
      'Message has reached 75% of the 5120-character limit.'
    );
    expect(getCounterAnnouncement(4608, FREE)).toBe('Approaching the 5120-character limit.');
    expect(getCounterAnnouncement(5120, FREE)).toBe(
      'Message at the 5120-character limit. Longer messages send as a .md attachment.'
    );
  });

  it('holds the at-limit text above the limit and does not change within a band', () => {
    expect(getCounterAnnouncement(6000, FREE)).toBe(getCounterAnnouncement(5120, FREE));
    expect(getCounterAnnouncement(3900, FREE)).toBe(getCounterAnnouncement(4000, FREE));
  });

  it('names the premium limit', () => {
    expect(getCounterAnnouncement(7680, PREMIUM)).toBe(
      'Message has reached 75% of the 10240-character limit.'
    );
  });
});

describe('evaluateEdit', () => {
  const original = 'Hello, world!';

  it('allows a changed edit under the limit', () => {
    expect(evaluateEdit('Hello again', original, FREE)).toEqual({
      trimmed: 'Hello again',
      length: 11,
      overLimit: false,
      canSave: true,
    });
  });

  it('allows an edit exactly AT the limit (the boundary is inclusive)', () => {
    const verdict = evaluateEdit('a'.repeat(FREE), original, FREE);
    expect(verdict.length).toBe(FREE);
    expect(verdict.overLimit).toBe(false);
    expect(verdict.canSave).toBe(true);
  });

  it('refuses an edit one over the limit and keeps the text untouched', () => {
    const draft = 'a'.repeat(FREE + 1);
    const verdict = evaluateEdit(draft, original, FREE);
    expect(verdict.overLimit).toBe(true);
    expect(verdict.canSave).toBe(false);
    expect(verdict.trimmed).toBe(draft); // never truncated
    expect(verdict.length).toBe(FREE + 1);
  });

  it('measures the TRIMMED draft: trailing whitespace past the limit does not count', () => {
    const body = 'a'.repeat(FREE);
    const verdict = evaluateEdit(`${body}   \n\t `, original, FREE);
    expect(verdict.trimmed).toBe(body);
    expect(verdict.length).toBe(FREE);
    expect(verdict.overLimit).toBe(false);
    expect(verdict.canSave).toBe(true);
  });

  it('trims leading whitespace too', () => {
    const body = 'a'.repeat(FREE);
    expect(evaluateEdit(`  \n${body}`, original, FREE).canSave).toBe(true);
  });

  it('refuses an empty or whitespace-only draft', () => {
    expect(evaluateEdit('', original, FREE).canSave).toBe(false);
    const blank = evaluateEdit('   \n ', original, FREE);
    expect(blank.canSave).toBe(false);
    expect(blank.length).toBe(0);
    expect(blank.overLimit).toBe(false);
  });

  it('refuses a draft that equals the original once trimmed', () => {
    expect(evaluateEdit(original, original, FREE).canSave).toBe(false);
    // The untrimmed draft differs from the original, the trimmed one does not: the save would
    // send `original` again, so Save must not be offered.
    expect(evaluateEdit(`${original}  `, original, FREE).canSave).toBe(false);
  });

  it('keeps Save off for an untouched original that carries leading or trailing whitespace', () => {
    const padded = ' Hello there ';
    // Opening the editor alone must not offer a save that would strip the padding.
    expect(evaluateEdit(padded, padded, FREE).canSave).toBe(false);
    // Removing the padding on purpose is an edit, so it saves.
    expect(evaluateEdit('Hello there', padded, FREE).canSave).toBe(true);
  });

  it('flags an original that is already over the limit (premium to free) and refuses it unchanged', () => {
    const old = 'a'.repeat(6000);
    const unchanged = evaluateEdit(old, old, FREE);
    expect(unchanged.overLimit).toBe(true);
    expect(unchanged.canSave).toBe(false);
    // Still over after a small trim of the text, still refused.
    expect(evaluateEdit(old.slice(0, 5500), old, FREE).canSave).toBe(false);
    // Shortened to the limit, it saves.
    expect(evaluateEdit(old.slice(0, FREE), old, FREE).canSave).toBe(true);
  });

  it('holds an edit to the premium limit when that is the active one', () => {
    const draft = 'a'.repeat(6000);
    expect(evaluateEdit(draft, original, PREMIUM).canSave).toBe(true);
    expect(evaluateEdit(draft, original, FREE).canSave).toBe(false);
    expect(evaluateEdit('a'.repeat(PREMIUM + 1), original, PREMIUM).canSave).toBe(false);
  });
});

describe('getOverLimitReason', () => {
  it('states the actual limit with a thousands separator and says how to proceed', () => {
    expect(getOverLimitReason(5120)).toBe(
      'This message is over your 5,120-character limit. Shorten it to save.'
    );
    expect(getOverLimitReason(10240)).toBe(
      'This message is over your 10,240-character limit. Shorten it to save.'
    );
  });

  it('uses the number it is given rather than a fixed one', () => {
    expect(getOverLimitReason(2000)).toContain('2,000-character');
    expect(getOverLimitReason(2000)).not.toContain('5,120');
  });
});
