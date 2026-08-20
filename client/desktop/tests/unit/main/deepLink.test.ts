import { describe, expect, it } from 'vitest';

import { extractInviteDeepLinkFromArgv, normalizeInviteDeepLink } from '@/main/deepLink';

describe('normalizeInviteDeepLink', () => {
  it('accepts concord://invite/{exact-8-code}', () => {
    expect(normalizeInviteDeepLink('concord://invite/GHJKMNPQ')).toEqual({
      ok: true,
      kind: 'invite',
      code: 'GHJKMNPQ',
    });
  });

  it('does not rely on URL.origin for custom schemes', () => {
    expect(new URL('concord://invite/GHJKMNPQ').origin).toBe('null');
    expect(normalizeInviteDeepLink('concord://invite/GHJKMNPQ').ok).toBe(true);
  });

  it.each([
    ['https://invite.concordvoice.chat/GHJKMNPQ', 'wrong-protocol'],
    ['concord://server/GHJKMNPQ', 'wrong-host'],
    ['concord://invite/GHJKMNPQ/extra', 'bad-path'],
    ['concord://invite/GHJKMNP', 'bad-code'],
    ['concord://invite/GHJKMNPQQ', 'bad-code'],
    ['concord://invite/BADCODE1', 'bad-code'],
    ['not a url', 'invalid-url'],
  ] as const)('rejects %s with %s', (raw, reason) => {
    expect(normalizeInviteDeepLink(raw)).toEqual({ ok: false, reason });
  });
});

describe('normalizeInviteDeepLink — two arms', () => {
  it('accepts a friend deep link', () => {
    expect(normalizeInviteDeepLink('concord://friend/AbCdEfGh')).toEqual({
      ok: true,
      kind: 'friend',
      code: 'AbCdEfGh',
    });
  });

  it('still accepts an invite deep link (regression)', () => {
    expect(normalizeInviteDeepLink('concord://invite/AbCdEfGh')).toEqual({
      ok: true,
      kind: 'invite',
      code: 'AbCdEfGh',
    });
  });

  it('rejects a plural host', () => {
    expect(normalizeInviteDeepLink('concord://friends/AbCdEfGh')).toEqual({
      ok: false,
      reason: 'wrong-host',
    });
  });

  it('rejects the wrong scheme', () => {
    expect(normalizeInviteDeepLink('concordvoice://friend/AbCdEfGh')).toEqual({
      ok: false,
      reason: 'wrong-protocol',
    });
  });

  it('rejects a bad charset', () => {
    expect(normalizeInviteDeepLink('concord://friend/badcode')).toEqual({
      ok: false,
      reason: 'bad-code',
    });
  });

  it('rejects extra path segments', () => {
    expect(normalizeInviteDeepLink('concord://friend/AbCdEfGh/x')).toEqual({
      ok: false,
      reason: 'bad-path',
    });
  });
});

describe('extractInviteDeepLinkFromArgv', () => {
  it('finds the first valid deep link in argv', () => {
    expect(extractInviteDeepLinkFromArgv(['app', '--flag', 'concord://invite/GHJKMNPQ'])).toEqual({
      ok: true,
      kind: 'invite',
      code: 'GHJKMNPQ',
    });
  });

  it('forwards the friend arm unchanged', () => {
    expect(extractInviteDeepLinkFromArgv(['app', '--flag', 'concord://friend/GHJKMNPQ'])).toEqual({
      ok: true,
      kind: 'friend',
      code: 'GHJKMNPQ',
    });
  });

  it('returns empty when argv has no valid invite link', () => {
    expect(extractInviteDeepLinkFromArgv(['app', '--flag'])).toEqual({
      ok: false,
      reason: 'empty',
    });
  });

  it('returns empty when argv is missing', () => {
    expect(extractInviteDeepLinkFromArgv()).toEqual({
      ok: false,
      reason: 'empty',
    });
  });
});
