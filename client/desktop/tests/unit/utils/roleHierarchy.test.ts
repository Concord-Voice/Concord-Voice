/**
 * Unit tests for the pure role-hierarchy model behind the role reorder rail (#2359).
 *
 * These tests exercise `src/renderer/utils/roleHierarchy.ts` directly: no DOM, no
 * store, no React. Where the implementation's ACTUAL behaviour differs from the
 * behaviour the copy implies, the test asserts the ACTUAL behaviour and names the
 * gap in a comment — see `evaluateReorderPrecondition` § known copy defect.
 */
import { describe, it, expect } from 'vitest';

import {
  buildReorderPayload,
  evaluateReorderPrecondition,
  moveWithinBand,
  partitionRoleHierarchy,
} from '@/renderer/utils/roleHierarchy';
import type { Role, RoleHierarchy, RoleViewer } from '@/renderer/types/server';

const SERVER_ID = '11111111-1111-4111-8111-111111111111';

/** A complete `Role`, so tests state only the fields under test. */
function role(overrides: Partial<Role> & Pick<Role, 'id' | 'position'>): Role {
  return {
    server_id: SERVER_ID,
    name: `role-${overrides.id}`,
    permissions: '0',
    is_default: false,
    is_managed: false,
    display_separately: false,
    mentionable: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

/**
 * The real `@all`: created `is_default = TRUE, is_managed = TRUE`
 * (`internal/servers/handlers.go:191`). Every test that touches the partition
 * uses THIS shape rather than a default-only stub — a default-only stub is
 * exactly what would let the `&& !is_default` clause be deleted unnoticed.
 */
const atAll = role({ id: 'at-all', position: 0, name: '@all', is_default: true, is_managed: true });

const OWNER: RoleViewer = { kind: 'owner' };
const UNKNOWN: RoleViewer = { kind: 'unknown' };
const bounded = (maxRolePosition: number): RoleViewer => ({ kind: 'bounded', maxRolePosition });

/** A hand-built hierarchy, for precondition states the partition cannot reach. */
function hierarchy(parts: Partial<RoleHierarchy> = {}): RoleHierarchy {
  return { aboveCeiling: [], band: [], managed: [], pinned: [], ...parts };
}

const ids = (roles: readonly Role[]): string[] => roles.map((r) => r.id);

describe('partitionRoleHierarchy', () => {
  it('puts a realistic @all (is_default AND is_managed) in pinned, never in managed', () => {
    const admin = role({ id: 'admin', position: 3 });

    const result = partitionRoleHierarchy([atAll, admin], OWNER);

    expect(ids(result.pinned)).toEqual(['at-all']);
    expect(ids(result.managed)).toEqual([]);
    expect(ids(result.band)).toEqual(['admin']);
  });

  it('classifies a non-default managed role as managed, not band', () => {
    const bot = role({ id: 'bot', position: 2, is_managed: true });
    const admin = role({ id: 'admin', position: 3 });

    const result = partitionRoleHierarchy([atAll, bot, admin], OWNER);

    expect(ids(result.managed)).toEqual(['bot']);
    expect(ids(result.band)).toEqual(['admin']);
    expect(ids(result.pinned)).toEqual(['at-all']);
  });

  it('leaves aboveCeiling EMPTY for an owner — every non-locked role is draggable', () => {
    const roles = [atAll, role({ id: 'a', position: 1 }), role({ id: 'b', position: 9 })];

    const result = partitionRoleHierarchy(roles, OWNER);

    expect(result.aboveCeiling).toEqual([]);
    expect(ids(result.band)).toEqual(['b', 'a']);
  });

  it('puts roles at position >= maxRolePosition above a bounded viewer’s ceiling', () => {
    const roles = [
      atAll,
      role({ id: 'p1', position: 1 }),
      role({ id: 'p2', position: 2 }),
      role({ id: 'p3', position: 3 }), // exactly AT the ceiling — locked, not draggable
      role({ id: 'p4', position: 4 }),
    ];

    const result = partitionRoleHierarchy(roles, bounded(3));

    expect(ids(result.aboveCeiling)).toEqual(['p4', 'p3']);
    expect(ids(result.band)).toEqual(['p2', 'p1']);
    expect(ids(result.pinned)).toEqual(['at-all']);
  });

  it('leaves NOTHING draggable for an unknown viewer (fail closed)', () => {
    const bot = role({ id: 'bot', position: 5, is_managed: true });
    const roles = [atAll, bot, role({ id: 'a', position: 1 }), role({ id: 'b', position: 2 })];

    const result = partitionRoleHierarchy(roles, UNKNOWN);

    expect(result.band).toEqual([]);
    expect(ids(result.aboveCeiling)).toEqual(['b', 'a']);
    expect(ids(result.managed)).toEqual(['bot']);
    expect(ids(result.pinned)).toEqual(['at-all']);
  });

  it('sorts every group by position DESCENDING', () => {
    const roles = [
      role({ id: 'low', position: 1 }),
      role({ id: 'high', position: 8 }),
      role({ id: 'mid', position: 4 }),
      role({ id: 'ceil-low', position: 10 }),
      role({ id: 'ceil-high', position: 12 }),
      role({ id: 'bot-low', position: 2, is_managed: true }),
      role({ id: 'bot-high', position: 7, is_managed: true }),
    ];

    const result = partitionRoleHierarchy(roles, bounded(9));

    expect(ids(result.band)).toEqual(['high', 'mid', 'low']);
    expect(ids(result.aboveCeiling)).toEqual(['ceil-high', 'ceil-low']);
    expect(ids(result.managed)).toEqual(['bot-high', 'bot-low']);
  });

  it('keeps incoming relative order for roles tied on position (stable sort)', () => {
    const roles = [
      role({ id: 'tie-a', position: 2 }),
      role({ id: 'tie-b', position: 2 }),
      role({ id: 'top', position: 3 }),
      role({ id: 'tie-c', position: 2 }),
    ];

    const result = partitionRoleHierarchy(roles, OWNER);

    expect(ids(result.band)).toEqual(['top', 'tie-a', 'tie-b', 'tie-c']);
  });

  it('does not mutate or reorder the input array', () => {
    const roles = [role({ id: 'a', position: 1 }), role({ id: 'b', position: 5 }), atAll];
    const snapshot = ids(roles);

    partitionRoleHierarchy(roles, OWNER);

    expect(ids(roles)).toEqual(snapshot);
    expect(roles).toHaveLength(3);
  });
});

describe('evaluateReorderPrecondition', () => {
  it('returns ok for a bounded viewer with a band that fits under the ceiling', () => {
    const h = hierarchy({ band: [role({ id: 'a', position: 2 }), role({ id: 'b', position: 1 })] });

    expect(evaluateReorderPrecondition(h, bounded(5))).toEqual({ kind: 'ok' });
    expect(evaluateReorderPrecondition(h, OWNER)).toEqual({ kind: 'ok' });
  });

  it('returns unknown-viewer, and that wins over every other failing condition', () => {
    const h = hierarchy({
      band: [role({ id: 'a', position: 1 })],
      managed: [role({ id: 'bot', position: 3, is_managed: true })],
    });

    // managed-gap AND nothing-to-move are both true here; unknown still wins.
    expect(evaluateReorderPrecondition(h, UNKNOWN)).toEqual({ kind: 'unknown-viewer' });
  });

  it('returns managed-gap whenever a non-default managed role is present', () => {
    const h = hierarchy({
      band: [role({ id: 'a', position: 2 }), role({ id: 'b', position: 1 })],
      managed: [role({ id: 'bot', position: 3, is_managed: true })],
    });

    expect(evaluateReorderPrecondition(h, OWNER)).toEqual({ kind: 'managed-gap' });
    expect(evaluateReorderPrecondition(h, bounded(9))).toEqual({ kind: 'managed-gap' });
  });

  it('ranks band-exceeds-ceiling above managed-gap when both are true', () => {
    const h = hierarchy({
      band: [
        role({ id: 'a', position: 3 }),
        role({ id: 'b', position: 2 }),
        role({ id: 'c', position: 1 }),
      ],
      managed: [role({ id: 'bot', position: 4, is_managed: true })],
    });

    // Both conditions hold: a non-default managed role is present, AND
    // band.length (3) > maxRolePosition - 1 (1).
    //
    // band-exceeds-ceiling wins because it is the condition under which NO
    // constructible order can succeed. The band already excludes managed
    // roles, so removing them would not make any order fit — pointing the user
    // at integration roles would name something that is not the binding
    // constraint. Scenario s5 of the shared cross-language contract fixture
    // (services/control-plane/internal/rbac/testdata/role_reorder_contract.json)
    // pins this precedence; that fixture is what caught the original inversion.
    expect(evaluateReorderPrecondition(h, bounded(2))).toEqual({
      kind: 'band-exceeds-ceiling',
    });
  });

  it('returns band-exceeds-ceiling when band.length > maxRolePosition - 1', () => {
    const band = [
      role({ id: 'a', position: 3 }),
      role({ id: 'b', position: 2 }),
      role({ id: 'c', position: 1 }),
    ];

    // 3 > 3 - 1 → refuse; 3 > 4 - 1 is false → ok.
    expect(evaluateReorderPrecondition(hierarchy({ band }), bounded(3))).toEqual({
      kind: 'band-exceeds-ceiling',
    });
    expect(evaluateReorderPrecondition(hierarchy({ band }), bounded(4))).toEqual({ kind: 'ok' });
  });

  it('never returns band-exceeds-ceiling for an owner, however large the band', () => {
    const band = Array.from({ length: 25 }, (_, i) => role({ id: `r${i}`, position: 25 - i }));

    expect(evaluateReorderPrecondition(hierarchy({ band }), OWNER)).toEqual({ kind: 'ok' });
  });

  it('returns nothing-to-move for a band of fewer than 2 roles', () => {
    expect(evaluateReorderPrecondition(hierarchy({ band: [] }), OWNER)).toEqual({
      kind: 'nothing-to-move',
    });
    expect(
      evaluateReorderPrecondition(hierarchy({ band: [role({ id: 'a', position: 1 })] }), OWNER)
    ).toEqual({ kind: 'nothing-to-move' });
    // A bounded viewer whose ceiling comfortably clears the band takes the same branch.
    expect(
      evaluateReorderPrecondition(hierarchy({ band: [role({ id: 'a', position: 1 })] }), bounded(9))
    ).toEqual({ kind: 'nothing-to-move' });
  });

  it('KNOWN COPY DEFECT: bounded maxRolePosition 0 reports band-exceeds-ceiling, not nothing-to-move', () => {
    // A viewer with maxRolePosition 0 can name no role at all, so the band is
    // NECESSARILY empty. `band.length > maxRolePosition - 1` is then `0 > -1`,
    // i.e. TRUE, and the ceiling branch fires before the `band.length < 2`
    // branch that describes the situation accurately.
    //
    // This is asserted AS-IS on purpose. Both outcomes are read-only, so there
    // is no authorization consequence; the only wrong artifact is the
    // user-facing notice copy ("your roles outrank the ceiling" instead of
    // "there is nothing to reorder"), which the UI layer compensates for.
    // DO NOT "fix" the function to satisfy this test — if the function is ever
    // corrected, this test is the thing that must change.
    expect(evaluateReorderPrecondition(hierarchy({ band: [] }), bounded(0))).toEqual({
      kind: 'band-exceeds-ceiling',
    });

    // Same shape one notch up: ceiling 1 also has a necessarily-empty band
    // (0 > 0 is false), so it DOES reach the accurate answer. The defect is
    // specific to maxRolePosition 0.
    expect(evaluateReorderPrecondition(hierarchy({ band: [] }), bounded(1))).toEqual({
      kind: 'nothing-to-move',
    });
  });
});

describe('moveWithinBand', () => {
  const band = ['a', 'b', 'c', 'd'];

  it('returns a NEW array and never mutates the input', () => {
    const input = [...band];

    const result = moveWithinBand(input, 0, 2);

    expect(result).not.toBe(input);
    expect(input).toEqual(['a', 'b', 'c', 'd']);
    expect(result).toEqual(['b', 'c', 'a', 'd']);
  });

  it('treats `to` as the TARGET index in the RETURNED array, not an insertion index', () => {
    // The single most likely off-by-one in this feature. With `from = 0`:
    //   target index 3      → 'a' ends up AT index 3            → [b, c, d, a]
    //   insertion index 3   → 'a' inserted BEFORE old index 3   → [b, c, a, d]
    // These differ, and this assertion pins the target-index reading.
    expect(moveWithinBand(band, 0, 3)).toEqual(['b', 'c', 'd', 'a']);
    expect(moveWithinBand(band, 0, 3)[3]).toBe('a');

    // The documented conversion from a `decideDropIndex` INSERTION index:
    //   to = dropIndex > from ? dropIndex - 1 : dropIndex
    const from = 0;
    const dropIndex = 3;
    const to = dropIndex > from ? dropIndex - 1 : dropIndex;
    expect(moveWithinBand(band, from, to)).toEqual(['b', 'c', 'a', 'd']);

    // Downward drops need no conversion: dropIndex 1 with from 3 is already the
    // target index.
    const upward = moveWithinBand(band, 3, 1 > 3 ? 0 : 1);
    expect(upward).toEqual(['a', 'd', 'b', 'c']);
    expect(upward[1]).toBe('d');
  });

  it('moves an entry up and down by one (the keyboard mechanisms)', () => {
    expect(moveWithinBand(band, 2, 1)).toEqual(['a', 'c', 'b', 'd']); // Alt+Up
    expect(moveWithinBand(band, 1, 2)).toEqual(['a', 'c', 'b', 'd']); // Alt+Down
  });

  it('is a no-op when `to` equals `from`', () => {
    expect(moveWithinBand(band, 2, 2)).toEqual(band);
  });

  it('clamps an out-of-range `to` to the band edge, so edge keyboard moves are no-ops', () => {
    // Alt+Up on the first row: to = -1 → clamped to 0 → equals `from` → no-op.
    expect(moveWithinBand(band, 0, -1)).toEqual(band);
    // Alt+Down on the last row: to = 4 → clamped to 3 → equals `from` → no-op.
    expect(moveWithinBand(band, 3, 4)).toEqual(band);

    // The clamp is an edge clamp, NOT a rejection: a far out-of-range `to` from
    // a non-edge row still moves the entry to that edge.
    expect(moveWithinBand(band, 1, -50)).toEqual(['b', 'a', 'c', 'd']);
    expect(moveWithinBand(band, 1, 50)).toEqual(['a', 'c', 'd', 'b']);
  });

  it('truncates a fractional `to` toward zero', () => {
    expect(moveWithinBand(band, 0, 2.9)).toEqual(['b', 'c', 'a', 'd']);
  });

  it('returns an unchanged copy for an out-of-range or non-integer `from`', () => {
    for (const from of [-1, 4, 99, 1.5, Number.NaN]) {
      const result = moveWithinBand(band, from, 0);
      expect(result).toEqual(band);
      expect(result).not.toBe(band);
    }
  });

  it('handles an empty band without throwing', () => {
    expect(moveWithinBand([], 0, 0)).toEqual([]);
  });

  it('handles a single-entry band as a no-op', () => {
    expect(moveWithinBand(['only'], 0, 1)).toEqual(['only']);
  });
});

describe('buildReorderPayload', () => {
  const roles = [
    atAll,
    role({ id: 'a', position: 3 }),
    role({ id: 'b', position: 2 }),
    role({ id: 'c', position: 1 }),
  ];
  const h = partitionRoleHierarchy(roles, OWNER);

  it('emits role_ids in draftOrder, highest authority FIRST', () => {
    const payload = buildReorderPayload(h, ['a', 'b', 'c']);

    expect(payload.role_ids).toEqual(['a', 'b', 'c']);
    expect(payload.__brand).toBe('RoleReorderPayload');
  });

  it('preserves a reordered permutation verbatim rather than re-sorting it', () => {
    expect(buildReorderPayload(h, ['c', 'a', 'b']).role_ids).toEqual(['c', 'a', 'b']);
  });

  it('copies draftOrder instead of aliasing it', () => {
    const draft = ['a', 'b', 'c'];
    const payload = buildReorderPayload(h, draft);

    expect(payload.role_ids).not.toBe(draft);
    draft.push('mutated');
    expect(payload.role_ids).toEqual(['a', 'b', 'c']);
  });

  it('NEVER names @all in the payload', () => {
    const payload = buildReorderPayload(h, ['a', 'b', 'c']);

    expect(payload.role_ids).not.toContain(atAll.id);
    // …and it cannot be smuggled in either: @all is pinned, so it is not a band id.
    expect(() => buildReorderPayload(h, ['a', 'b', 'c', atAll.id])).toThrow(
      /not a permutation of the band/
    );
  });

  it('THROWS on a SUBSET of the band (the silent server-side collision case)', () => {
    // Sending [a, b] would make the server renumber ONLY a and b and leave c at
    // its existing position — HTTP 200, duplicate positions, no error anywhere.
    expect(() => buildReorderPayload(h, ['a', 'b'])).toThrow(/not a permutation of the band/);
    expect(() => buildReorderPayload(h, [])).toThrow(/not a permutation of the band/);
  });

  it('THROWS on a SUPERSET of the band (an id that is not draggable)', () => {
    expect(() => buildReorderPayload(h, ['a', 'b', 'c', 'ghost'])).toThrow(
      /not a permutation of the band/
    );
  });

  it('THROWS on a duplicated band id', () => {
    expect(() => buildReorderPayload(h, ['a', 'b', 'b'])).toThrow(/not a permutation of the band/);
  });

  it('THROWS on an empty band, with a distinct message', () => {
    const empty = partitionRoleHierarchy([atAll], OWNER);

    expect(empty.band).toEqual([]);
    expect(() => buildReorderPayload(empty, [])).toThrow(
      /refusing to build an empty payload from an empty band/
    );
  });

  it('never puts role ids in the error message (counts only)', () => {
    // Distinctive ids so "the id is absent" is a real assertion and not a
    // coincidence of short names colliding with ordinary English words.
    const secretive = partitionRoleHierarchy(
      [
        atAll,
        role({ id: 'role-alpha-9f3c', position: 3 }),
        role({ id: 'role-bravo-2d81', position: 2 }),
        role({ id: 'role-charlie-77e', position: 1 }),
      ],
      OWNER
    );

    let message = '';
    try {
      buildReorderPayload(secretive, ['role-alpha-9f3c', 'role-bravo-2d81']);
    } catch (err) {
      message = (err as Error).message;
    }

    expect(message).toContain('band=3');
    expect(message).toContain('draft=2');
    expect(message).not.toContain('role-alpha-9f3c');
    expect(message).not.toContain('role-bravo-2d81');
    expect(message).not.toContain('role-charlie-77e');
  });
});
