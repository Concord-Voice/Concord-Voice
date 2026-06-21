import { describe, it, expect } from 'vitest';
import {
  INVITE_HOST,
  isValidInviteCode,
  buildInviteUrl,
  extractInviteCodes,
  messageInviteCodes,
} from '@/renderer/utils/inviteUrl';

describe('inviteUrl', () => {
  it('builds the canonical URL', () => {
    expect(buildInviteUrl('GHJKMNPQ')).toBe(`https://${INVITE_HOST}/GHJKMNPQ`);
  });

  it('accepts an 8-char code from the ambiguity-stripped charset', () => {
    expect(isValidInviteCode('GHJKMNPQ')).toBe(true);
    expect(isValidInviteCode('LKJHmnpq')).toBe(true);
  });

  it('rejects wrong length and ambiguous chars (I, O, i, l, o, 0, 1)', () => {
    expect(isValidInviteCode('GHJKMNP')).toBe(false); // 7
    expect(isValidInviteCode('GHJKMNPQ6')).toBe(false); // 9
    expect(isValidInviteCode('GHJKINPQ')).toBe(false); // I
    expect(isValidInviteCode('GHJKlNPQ')).toBe(false); // l
    expect(isValidInviteCode('GHJK0NPQ')).toBe(false); // 0
  });

  it('extracts an exact-host canonical URL code from text', () => {
    expect(extractInviteCodes(`join here https://${INVITE_HOST}/GHJKMNPQ thanks`)).toEqual([
      'GHJKMNPQ',
    ]);
  });

  it('ignores bare codes, foreign hosts, and look-alike hosts', () => {
    expect(extractInviteCodes('GHJKMNPQ')).toEqual([]);
    expect(extractInviteCodes('https://evil.example/GHJKMNPQ')).toEqual([]);
    expect(extractInviteCodes(`https://evil.${INVITE_HOST}/GHJKMNPQ`)).toEqual([]);
    expect(extractInviteCodes(`https://${INVITE_HOST}.evil.com/GHJKMNPQ`)).toEqual([]);
    expect(extractInviteCodes(`http://${INVITE_HOST}/GHJKMNPQ`)).toEqual([]); // not https
  });

  it('dedupes and caps at 3', () => {
    const u = (c: string) => `https://${INVITE_HOST}/${c}`;
    const text = `${u('GGGGMNPQ')} ${u('GGGGMNPQ')} ${u('HHHHMNPQ')} ${u('JJJJMNPQ')} ${u('KKKKMNPQ')}`;
    expect(extractInviteCodes(text)).toEqual(['GGGGMNPQ', 'HHHHMNPQ', 'JJJJMNPQ']);
  });

  it('messageInviteCodes returns [] for undecryptable messages', () => {
    const url = `https://${INVITE_HOST}/GHJKMNPQ`;
    expect(messageInviteCodes(url, {})).toEqual(['GHJKMNPQ']);
    expect(messageInviteCodes(url, { pendingKeys: true })).toEqual([]);
    expect(messageInviteCodes(url, { decryptFailed: true })).toEqual([]);
  });

  it('extracts a canonical URL even with trailing sentence punctuation', () => {
    expect(extractInviteCodes(`join at https://${INVITE_HOST}/GHJKMNPQ.`)).toEqual(['GHJKMNPQ']);
    expect(extractInviteCodes(`(invite: https://${INVITE_HOST}/GHJKMNPQ)`)).toEqual(['GHJKMNPQ']);
    expect(extractInviteCodes(`see https://${INVITE_HOST}/GHJKMNPQ, thanks`)).toEqual(['GHJKMNPQ']);
  });

  it('rejects a non-default port on the invite host', () => {
    expect(extractInviteCodes(`https://${INVITE_HOST}:8443/GHJKMNPQ`)).toEqual([]);
  });
});
