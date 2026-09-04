import { describe, it, expect } from 'vitest';
import {
  MAX_ICE_SERVERS,
  normalizeIceServers,
  extractSelectedCandidatePairType,
  describeIceServers,
} from '@/renderer/services/voice/iceServers';

const STUN = { urls: 'stun:turn.example.test:3478' };
const TURN = {
  urls: 'turn:turn.example.test:3478',
  username: '1780000000:user-1',
  credential: 'Zm9vYmFyYmF6',
};
const TURN_TCP = { ...TURN, urls: 'turn:turn.example.test:3478?transport=tcp' };
const TURNS = { ...TURN, urls: 'turns:turn.example.test:5349' };

describe('normalizeIceServers', () => {
  it('passes a valid mixed list through verbatim', () => {
    const out = normalizeIceServers([STUN, TURN, TURN_TCP, TURNS]);
    expect(out).toEqual([STUN, TURN, TURN_TCP, TURNS]);
  });

  it('returns [] for non-array input', () => {
    expect(normalizeIceServers(undefined)).toEqual([]);
    expect(normalizeIceServers(null)).toEqual([]);
    expect(normalizeIceServers('stun:x')).toEqual([]);
    expect(normalizeIceServers({ urls: 'stun:x' })).toEqual([]);
    expect(normalizeIceServers(42)).toEqual([]);
  });

  it('returns [] for an empty array', () => {
    expect(normalizeIceServers([])).toEqual([]);
  });

  it('caps the list at MAX_ICE_SERVERS entries', () => {
    const many = Array.from({ length: MAX_ICE_SERVERS + 5 }, (_, i) => ({
      urls: `stun:host-${i}.example.test:3478`,
    }));
    expect(normalizeIceServers(many)).toHaveLength(MAX_ICE_SERVERS);
    expect(normalizeIceServers(many)[0]).toEqual({ urls: 'stun:host-0.example.test:3478' });
  });

  it('drops a non-object entry but keeps its valid neighbours', () => {
    expect(normalizeIceServers([STUN, null, 'stun:x', 7, [], STUN])).toEqual([STUN, STUN]);
  });

  it('drops an entry whose urls is missing, non-string, or whitespace-only', () => {
    expect(normalizeIceServers([{}, { urls: 123 }, { urls: '' }, { urls: '   ' }, STUN])).toEqual([
      STUN,
    ]);
  });

  it('drops an entry whose scheme is not stun/stuns/turn/turns', () => {
    expect(
      normalizeIceServers([
        { urls: 'http://turn.example.test' },
        { urls: 'javascript:alert(1)' },
        { urls: 'turn.example.test:3478' },
        { urls: 'stuns:turn.example.test:5349' },
      ])
    ).toEqual([{ urls: 'stuns:turn.example.test:5349' }]);
  });

  it('drops a turn:/turns: entry missing username or credential', () => {
    const noUser = { urls: 'turn:turn.example.test:3478', credential: 'Zm9v' };
    const noCred = { urls: 'turn:turn.example.test:3478', username: '1780000000:user-1' };
    const badTypes = { urls: 'turns:turn.example.test:5349', username: 1, credential: {} };
    expect(normalizeIceServers([noUser, noCred, badTypes, STUN, TURN])).toEqual([STUN, TURN]);
  });

  it('keeps a stun: entry that carries no credentials', () => {
    expect(normalizeIceServers([STUN])).toEqual([STUN]);
  });

  it('copies entries rather than aliasing the input objects', () => {
    const input = [{ ...TURN }];
    const out = normalizeIceServers(input);
    expect(out[0]).not.toBe(input[0]);
    expect(out[0]).toEqual(TURN);
  });
});

/**
 * D2's contract is that a malformed ENTRY drops only itself — an all-or-nothing
 * drop reproduces the very outage #3104 exists to close. A throwing accessor is
 * the same class of hostile input as a malformed value, so it must cost the same
 * one entry. Adversarial pass A10.
 */
describe('normalizeIceServers — a throwing entry drops only itself (A10)', () => {
  const throwingUrls = {
    get urls(): string {
      throw new Error('urls getter trap');
    },
  };

  it('drops an entry whose urls getter throws and keeps its neighbours', () => {
    expect(normalizeIceServers([STUN, throwingUrls, TURN])).toEqual([STUN, TURN]);
  });

  it('drops a turn: entry whose username getter throws', () => {
    const entry = {
      urls: 'turn:trap.example.test:3478',
      get username(): string {
        throw new Error('username getter trap');
      },
      credential: 'Zm9v', // pragma: allowlist secret
    };
    expect(normalizeIceServers([entry, STUN])).toEqual([STUN]);
  });

  it('drops a turn: entry whose credential getter throws', () => {
    const entry = {
      urls: 'turn:trap.example.test:3478',
      username: '1780000000:user-1',
      get credential(): string {
        throw new Error('credential getter trap');
      },
    };
    expect(normalizeIceServers([entry, STUN])).toEqual([STUN]);
  });

  it('drops an array INDEX whose own getter throws', () => {
    const list: unknown[] = [STUN, null, TURN];
    Object.defineProperty(list, 1, {
      get() {
        throw new Error('index getter trap');
      },
      configurable: true,
    });
    expect(normalizeIceServers(list)).toEqual([STUN, TURN]);
  });

  it('still honours the cap when the list is padded with throwing entries', () => {
    const many: unknown[] = Array.from({ length: MAX_ICE_SERVERS + 5 }, (_, i) => ({
      urls: `stun:host-${i}.example.test:3478`,
    }));
    many.splice(2, 0, throwingUrls);
    const out = normalizeIceServers(many);
    expect(out.length).toBeLessThanOrEqual(MAX_ICE_SERVERS);
    expect(out).toContainEqual({ urls: 'stun:host-0.example.test:3478' });
  });
});

describe('describeIceServers', () => {
  it('reports count and sorted distinct schemes and nothing else', () => {
    expect(describeIceServers([TURNS, STUN, TURN, TURN_TCP])).toEqual({
      count: 4,
      schemes: ['stun', 'turn', 'turns'],
    });
  });

  it('reports an empty list as zero with no schemes', () => {
    expect(describeIceServers([])).toEqual({ count: 0, schemes: [] });
  });
});

/**
 * This is the one function whose entire purpose is to be safe to log, so its
 * output alphabet must be closed for ANY input, not only for the normalized list
 * production happens to hand it today. Adversarial pass A8b.
 */
describe('describeIceServers — closed output alphabet on non-normalized input (A8b)', () => {
  const CLOSED_LABELS = ['other', 'stun', 'stuns', 'turn', 'turns'];

  it('never echoes an attacker-chosen scheme prefix', () => {
    const described = describeIceServers([
      { urls: 'ATTACKER-CONTROLLED-PREFIX:host' } as RTCIceServer,
    ]);
    expect(described.schemes).toEqual(['other']);
    expect(JSON.stringify(described)).not.toContain('ATTACKER');
  });

  it('emits only closed labels for a battery of hostile urls values', () => {
    const hostile = [
      { urls: 'javascript:alert(1)' },
      { urls: 'data:text/html,<script>alert(1)</script>' },
      { urls: 'sensitive-looking-prefix:x' },
      { urls: 'no-colon-at-all' },
      { urls: ':leading-colon' },
      { urls: 'line-one\nINJECTED LOG LINE:x' },
      { urls: 'STUN:upper.example.test:3478' },
      { urls: 'x'.repeat(4096) + ':y' },
    ] as RTCIceServer[];
    const described = describeIceServers(hostile);
    expect(described.count).toBe(hostile.length);
    expect(described.schemes).toEqual(['other']);
    for (const s of described.schemes) expect(CLOSED_LABELS).toContain(s);
  });

  it('reads the legal string[] form of urls without echoing it', () => {
    const described = describeIceServers([
      { urls: ['turn:a.example.test:3478', 'SECRET-PREFIX:b'] } as RTCIceServer,
    ]);
    expect(described).toEqual({ count: 1, schemes: ['other', 'turn'] });
  });

  it('is total on non-array input', () => {
    for (const bad of [undefined, null, 'stun:x', 42, { urls: 'stun:x' }]) {
      expect(describeIceServers(bad)).toEqual({ count: 0, schemes: [] });
    }
  });

  it('is total on non-object and accessor-trapped entries', () => {
    const list: unknown[] = [
      null,
      7,
      'stun:x',
      [],
      {
        get urls(): string {
          throw new Error('describe getter trap');
        },
      },
      STUN,
    ];
    expect(describeIceServers(list)).toEqual({ count: 6, schemes: ['stun'] });
  });

  it('never surfaces a credential pair even when the entry carries one', () => {
    const described = describeIceServers([
      {
        urls: 'turn:relay.example.test:3478',
        username: '1780000000:user-1',
        credential: 'S3NT1NEL-CREDENTIAL', // pragma: allowlist secret
      } as RTCIceServer,
    ]);
    const blob = JSON.stringify(described);
    expect(blob).not.toContain('S3NT1NEL-CREDENTIAL');
    expect(blob).not.toContain('relay.example.test');
    expect(blob).not.toContain('user-1');
  });
});

/** RTCStatsReport is Map-like; a Map satisfies the read path under test. */
function statsReport(entries: Array<[string, Record<string, unknown>]>): RTCStatsReport {
  return new Map(entries) as unknown as RTCStatsReport;
}

describe('extractSelectedCandidatePairType', () => {
  it('reads the type of the nominated succeeded pair via its local candidate', () => {
    const stats = statsReport([
      [
        'cp-1',
        { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
      ],
      ['lc-1', { type: 'local-candidate', candidateType: 'relay' }],
    ]);
    expect(extractSelectedCandidatePairType(stats)).toBe('relay');
  });

  it('prefers an explicitly selected pair over a merely nominated one', () => {
    const stats = statsReport([
      [
        'cp-1',
        { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
      ],
      ['lc-1', { type: 'local-candidate', candidateType: 'host' }],
      [
        'cp-2',
        { type: 'candidate-pair', state: 'succeeded', selected: true, localCandidateId: 'lc-2' },
      ],
      ['lc-2', { type: 'local-candidate', candidateType: 'relay' }],
    ]);
    expect(extractSelectedCandidatePairType(stats)).toBe('relay');
  });

  it('returns null when no pair has succeeded yet', () => {
    const stats = statsReport([
      [
        'cp-1',
        {
          type: 'candidate-pair',
          state: 'in-progress',
          nominated: false,
          localCandidateId: 'lc-1',
        },
      ],
      ['lc-1', { type: 'local-candidate', candidateType: 'host' }],
    ]);
    expect(extractSelectedCandidatePairType(stats)).toBeNull();
  });

  it('returns null when the local candidate is missing', () => {
    const stats = statsReport([
      [
        'cp-1',
        { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'gone' },
      ],
    ]);
    expect(extractSelectedCandidatePairType(stats)).toBeNull();
  });

  it('returns null for a candidateType outside the closed set', () => {
    const stats = statsReport([
      [
        'cp-1',
        { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
      ],
      ['lc-1', { type: 'local-candidate', candidateType: 'weird-new-thing' }],
    ]);
    expect(extractSelectedCandidatePairType(stats)).toBeNull();
  });

  it('returns null for an empty report', () => {
    expect(extractSelectedCandidatePairType(statsReport([]))).toBeNull();
  });
});
