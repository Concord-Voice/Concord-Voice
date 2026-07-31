import net from 'node:net';

export type Tier1Reason =
  | 'metadata_link_local'
  | 'unspecified'
  | 'multicast'
  | 'broadcast'
  | 'deprecated_site_local'
  | 'reserved';
export type Tier2Reason = 'loopback' | 'private' | 'ula' | 'cgnat';

export type EgressDecision =
  | { tier: 'tier1'; reason: Tier1Reason }
  | { tier: 'tier2'; reason: Tier2Reason }
  | { tier: 'public' }
  | { tier: 'invalid'; reason: 'unparseable' | 'empty' };

type Subnet = [addr: string, prefix: number, family: 'ipv4' | 'ipv6'];

function blockList(subnets: Subnet[]): net.BlockList {
  const list = new net.BlockList();
  for (const [addr, prefix, family] of subnets) list.addSubnet(addr, prefix, family);
  return list;
}

// Ordered: broadcast (255.255.255.255/32) is checked before the reserved 240.0.0.0/4
// that contains it, so the specific reason wins. NAT64 / 6to4 are tier 1 because
// net.BlockList does NOT unwrap their embedded IPv4 (measured) — an IPv4 laundering channel.
const TIER1: { list: net.BlockList; reason: Tier1Reason }[] = [
  {
    reason: 'metadata_link_local',
    list: blockList([
      ['169.254.0.0', 16, 'ipv4'],
      ['fe80::', 10, 'ipv6'],
    ]),
  },
  {
    reason: 'unspecified',
    list: blockList([
      ['0.0.0.0', 8, 'ipv4'],
      ['::', 128, 'ipv6'],
    ]),
  },
  {
    reason: 'multicast',
    list: blockList([
      ['224.0.0.0', 4, 'ipv4'],
      ['ff00::', 8, 'ipv6'],
    ]),
  },
  { reason: 'broadcast', list: blockList([['255.255.255.255', 32, 'ipv4']]) },
  { reason: 'deprecated_site_local', list: blockList([['fec0::', 10, 'ipv6']]) },
  {
    reason: 'reserved',
    list: blockList([
      ['192.0.0.0', 24, 'ipv4'],
      ['198.18.0.0', 15, 'ipv4'],
      ['240.0.0.0', 4, 'ipv4'],
      ['100::', 64, 'ipv6'],
      ['2001:db8::', 32, 'ipv6'],
      ['3fff::', 20, 'ipv6'],
      ['2002::', 16, 'ipv6'],
      ['64:ff9b::', 96, 'ipv6'],
      ['64:ff9b:1::', 48, 'ipv6'],
    ]),
  },
];

const TIER2: { list: net.BlockList; reason: Tier2Reason }[] = [
  {
    reason: 'loopback',
    list: blockList([
      ['127.0.0.0', 8, 'ipv4'],
      ['::1', 128, 'ipv6'],
    ]),
  },
  {
    reason: 'private',
    list: blockList([
      ['10.0.0.0', 8, 'ipv4'],
      ['172.16.0.0', 12, 'ipv4'],
      ['192.168.0.0', 16, 'ipv4'],
    ]),
  },
  { reason: 'cgnat', list: blockList([['100.64.0.0', 10, 'ipv4']]) },
  { reason: 'ula', list: blockList([['fc00::', 7, 'ipv6']]) },
];

/** Pure. Total. Never throws. Never takes an approval flag. */
export function classifyAddress(address: string): EgressDecision {
  if (typeof address !== 'string' || address.length === 0) {
    return { tier: 'invalid', reason: 'empty' };
  }
  // Mandatory: new URL().hostname yields bracketed IPv6; net.isIP('[::1]') === 0.
  const bare = address.replace(/^\[|\]$/g, '');
  const fam = net.isIP(bare); // 0 = not an IP; net.BlockList fails OPEN on non-IPs, so this is the gate.
  if (fam === 0) return { tier: 'invalid', reason: 'unparseable' };
  const type = fam === 4 ? 'ipv4' : 'ipv6';
  for (const { list, reason } of TIER1)
    if (list.check(bare, type)) return { tier: 'tier1', reason };
  for (const { list, reason } of TIER2)
    if (list.check(bare, type)) return { tier: 'tier2', reason };
  return { tier: 'public' };
}

export function isDialPermitted(d: EgressDecision, originApproved: boolean): boolean {
  switch (d.tier) {
    case 'tier1':
      return false; // no branch on originApproved — absolute, by construction
    case 'invalid':
      return false; // fail closed
    case 'tier2':
      return originApproved;
    case 'public':
      return true;
  }
}
