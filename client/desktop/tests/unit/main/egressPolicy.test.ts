// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { classifyAddress, isDialPermitted } from '@/main/egressPolicy';

describe('classifyAddress — tier 1 (never approvable)', () => {
  it.each([
    ['169.254.169.254', 'metadata_link_local'],
    ['0.0.0.0', 'unspecified'],
    ['224.0.0.1', 'multicast'],
    ['255.255.255.255', 'broadcast'],
    ['192.0.0.8', 'reserved'],
    ['198.18.0.1', 'reserved'],
    ['240.0.0.1', 'reserved'],
    ['::', 'unspecified'],
    ['fe80::1', 'metadata_link_local'],
    ['fec0::1', 'deprecated_site_local'],
    ['ff02::1', 'multicast'],
    ['64:ff9b::a00:5', 'reserved'], // NAT64 — BlockList does NOT unwrap the embedded IPv4
    ['64:ff9b:1::1', 'reserved'], // NAT64 local-use prefix
    ['2002:0a00:0005::1', 'reserved'], // 6to4 wrapping 10.0.0.5
    ['100::1', 'reserved'], // discard-only
    ['2001:db8::1', 'reserved'],
    ['3fff::1', 'reserved'],
    ['::ffff:169.254.169.254', 'metadata_link_local'],
  ])('%s → tier1 %s', (addr, reason) => {
    expect(classifyAddress(addr)).toEqual({ tier: 'tier1', reason });
  });
});

describe('classifyAddress — tier 2 (approvable) and public', () => {
  it.each([
    ['127.0.0.1', 'loopback'],
    ['127.0.0.2', 'loopback'], // closes the exact-match isLocalhost hole
    ['::1', 'loopback'],
    ['10.0.0.5', 'private'],
    ['172.16.0.1', 'private'],
    ['192.168.1.1', 'private'],
    ['100.64.0.1', 'cgnat'],
    ['fc00::1', 'ula'],
    ['::ffff:10.0.0.5', 'private'],
  ])('%s → tier2 %s', (addr, reason) => {
    expect(classifyAddress(addr)).toEqual({ tier: 'tier2', reason });
  });

  it.each([['93.184.216.34'], ['2606:2800:220:1:248:1893:25c8:1946']])(
    'classifies the routable address %s as public',
    (addr) => {
      expect(classifyAddress(addr)).toEqual({ tier: 'public' });
    }
  );
});

describe('classifyAddress — fail closed on malformed input', () => {
  it.each([
    ['', 'empty'],
    ['not-an-ip', 'unparseable'],
    ['999.999.999.999', 'unparseable'],
  ])('%s → invalid %s', (addr, reason) => {
    expect(classifyAddress(addr)).toEqual({ tier: 'invalid', reason });
  });

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['a number', 123],
  ])('treats %s as invalid rather than throwing', (_label, value) => {
    expect(classifyAddress(value as unknown as string)).toEqual({
      tier: 'invalid',
      reason: 'empty',
    });
  });

  it('strips brackets before net.isIP (mapped literal from new URL().hostname)', () => {
    expect(classifyAddress('[::ffff:169.254.169.254]')).toEqual({
      tier: 'tier1',
      reason: 'metadata_link_local',
    });
  });

  it('strips brackets on a plain IPv6 literal — [::1] is tier 2, not invalid', () => {
    expect(classifyAddress('[::1]')).toEqual({ tier: 'tier2', reason: 'loopback' });
  });
});

describe('isDialPermitted — tier 1 is absolute', () => {
  it('denies tier 1 even when the origin is approved', () => {
    expect(isDialPermitted(classifyAddress('169.254.169.254'), true)).toBe(false);
  });
  it('denies invalid (fail closed)', () => {
    expect(isDialPermitted({ tier: 'invalid', reason: 'unparseable' }, true)).toBe(false);
  });
  it('gates tier 2 on approval', () => {
    expect(isDialPermitted(classifyAddress('127.0.0.1'), false)).toBe(false);
    expect(isDialPermitted(classifyAddress('127.0.0.1'), true)).toBe(true);
  });
  it('permits public unconditionally', () => {
    expect(isDialPermitted(classifyAddress('93.184.216.34'), false)).toBe(true);
  });
});
