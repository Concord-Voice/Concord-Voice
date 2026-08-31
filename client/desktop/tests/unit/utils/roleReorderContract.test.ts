/**
 * TypeScript consumer of the SHARED cross-language role-reorder contract fixture
 * (#2359).
 *
 * The fixture is
 * `services/control-plane/internal/rbac/testdata/role_reorder_contract.json`,
 * and it has two consumers that must agree:
 *
 * - Go (`internal/rbac/reorder_contract_test.go`) seeds each declared world into
 *   real PostgreSQL, PATCHes the declared payload as the declared actor, and
 *   asserts the REAL SERVER's verdict and the resulting row positions.
 * - This file asserts that a correct client would actually BUILD that payload.
 *
 * Neither side alone catches PR #2839's defect. #2839 shipped a reorder that
 * worked only for the server owner with a fully green suite, because the
 * production code and its Vitest suite encoded the SAME wrong belief about the
 * wire contract — sourced from one docstring, asserted against a `vi.fn()` stub
 * — while every green Go test for a non-owner hand-wrote a partial payload that
 * happened to sit inside every budget. The contradiction lived only in the gap
 * between the two suites. This fixture is that gap, made into an artifact.
 *
 * Consequently this file:
 * - re-implements NO server rule (no guard evaluation, no budget arithmetic),
 * - installs NO MSW handler that models the guards (an MSW guard model has
 *   exactly the defect this fixture exists to remove, in a new costume),
 * - asserts ONLY the pure functions against the fixture's declared expectations.
 *
 * The fixture is read from disk rather than imported so that a missing or moved
 * file is a loud failure with a path in it, and so nothing about the shared
 * oracle depends on Vite module resolution reaching outside `client/desktop`.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  buildReorderPayload,
  evaluateReorderPrecondition,
  moveWithinBand,
  partitionRoleHierarchy,
} from '../../../src/renderer/utils/policy/roleHierarchy';
import type {
  ReorderPrecondition,
  Role,
  RoleHierarchy,
  RoleViewer,
} from '../../../src/renderer/types/server';

const FIXTURE_PATH = resolve(
  __dirname,
  '../../../../../services/control-plane/internal/rbac/testdata/role_reorder_contract.json'
);

// --- Fixture shape (schema block at the top of the JSON) -------------------

interface FixtureRole {
  key: string;
  name: string;
  position: number;
  is_default: boolean;
  is_managed: boolean;
  permissions?: string[];
}

interface FixtureViewer {
  kind: 'owner' | 'bounded' | 'unknown';
  max_role_position?: number;
}

interface FixtureWorld {
  owner_is_actor: boolean;
  roles: FixtureRole[];
  actor: { held_role_keys: string[]; viewer: FixtureViewer };
}

interface FixtureProbe {
  id: string;
  purpose: 'falsification' | 'negative' | 'tolerance';
  wrong_builder: string;
  role_ids: string[];
  note?: string;
}

interface FixtureScenario {
  id: string;
  spec_scenario: number;
  title: string;
  world: FixtureWorld;
  client_snapshot: FixtureWorld | null;
  expected_hierarchy: {
    above_ceiling: string[];
    band: string[];
    managed: string[];
    pinned: string[];
  };
  expected_precondition: string;
  also_true?: string[];
  draft?: string;
  expected_payload: { role_ids: string[] } | null;
  probes: FixtureProbe[];
  go_exercisable: boolean;
}

interface Fixture {
  version: number;
  scenarios: FixtureScenario[];
}

const fixture = JSON.parse(readFileSync(FIXTURE_PATH, 'utf-8')) as Fixture;

// --- Fixture -> renderer type mapping -------------------------------------

/**
 * Role keys in the fixture are symbolic ("all", "admin", "mod"), so the key IS
 * the id here. Every assertion in this file compares ids, which keeps the
 * failure output readable and keeps both consumers naming the same rows.
 */
function toRole(fixtureRole: FixtureRole): Role {
  return {
    id: fixtureRole.key,
    server_id: 'contract-server',
    name: fixtureRole.name,
    position: fixtureRole.position,
    permissions: '0',
    is_default: fixtureRole.is_default,
    is_managed: fixtureRole.is_managed,
    display_separately: false,
    mentionable: false,
    created_at: '2026-08-21T00:00:00Z',
    updated_at: '2026-08-21T00:00:00Z',
  };
}

function toViewer(fixtureViewer: FixtureViewer): RoleViewer {
  switch (fixtureViewer.kind) {
    case 'owner':
      return { kind: 'owner' };
    case 'unknown':
      return { kind: 'unknown' };
    case 'bounded':
      if (typeof fixtureViewer.max_role_position !== 'number') {
        throw new Error('fixture: bounded viewer without max_role_position');
      }
      return { kind: 'bounded', maxRolePosition: fixtureViewer.max_role_position };
  }
}

/**
 * `client_snapshot: null` means "identical to world" (fixture schema). A
 * non-null snapshot models staleness (scenario s7) and is what the client
 * actually partitions — the server's own truth is the Go consumer's input.
 */
function snapshotOf(scenario: FixtureScenario): FixtureWorld {
  return scenario.client_snapshot ?? scenario.world;
}

function ids(roles: readonly Role[]): string[] {
  return roles.map((role) => role.id);
}

/**
 * VOCABULARY DRIFT, recorded rather than silently absorbed.
 *
 * The fixture names two read-only reasons with the spec's words, and
 * `ReorderPrecondition` (types/server.ts) names the same two states with
 * different words:
 *
 *   fixture "no-movable-roles" <-> union 'nothing-to-move'
 *   fixture "managed-present"  <-> union 'managed-gap'
 *
 * Both pairs are the same state under two names, so this map normalises them
 * for comparison. It maps NOTHING ELSE: an unrecognised fixture term is a hard
 * failure (see the "declares only known precondition terms" test), so a new
 * read-only reason cannot slip through as `undefined`.
 */
const FIXTURE_PRECONDITION_TO_KIND: Record<string, ReorderPrecondition['kind']> = {
  ok: 'ok',
  'unknown-viewer': 'unknown-viewer',
  'band-exceeds-ceiling': 'band-exceeds-ceiling',
  'managed-present': 'managed-gap',
  'no-movable-roles': 'nothing-to-move',
};

/**
 * Whether a correct client could emit exactly `roleIds` for this scenario.
 *
 * Two ways it cannot, and both are real defences rather than one dressed up
 * twice: the precondition refuses to offer a drag at all, or the payload
 * constructor refuses a draft that is not a permutation of the band. Every
 * probe in the fixture — falsification, negative AND tolerance — must be
 * unreachable by both routes at once.
 */
function clientCanEmit(
  hierarchy: RoleHierarchy,
  precondition: ReorderPrecondition,
  roleIds: readonly string[]
): boolean {
  if (precondition.kind !== 'ok') {
    return false;
  }
  try {
    return buildReorderPayload(hierarchy, roleIds).role_ids.join('|') === roleIds.join('|');
  } catch {
    return false;
  }
}

/**
 * The `from`/`to` of the single drag that turns the band's display order into
 * `target`, or null when no single move produces it. The fixture states each
 * draft in prose ("The user drags Support above Helper"), so this is how the
 * declared payload is checked to be a REACHABLE draft rather than an arbitrary
 * permutation someone typed next to the prose.
 */
function singleMoveTo(
  bandIds: readonly string[],
  target: readonly string[]
): [number, number] | null {
  for (let from = 0; from < bandIds.length; from += 1) {
    for (let to = 0; to < bandIds.length; to += 1) {
      if (moveWithinBand(bandIds, from, to).join('|') === target.join('|')) {
        return [from, to];
      }
    }
  }
  return null;
}

// --- Per-scenario assertions ----------------------------------------------

describe('role reorder wire contract (shared fixture, #2359)', () => {
  it('reads the shared fixture the Go consumer reads', () => {
    expect(fixture.version).toBe(1);
    expect(fixture.scenarios.length).toBeGreaterThan(0);
  });

  it('covers spec scenarios 1-8 exactly once each', () => {
    const specNumbers = fixture.scenarios.map((s) => s.spec_scenario).sort((a, b) => a - b);
    expect(specNumbers).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
  });

  it('declares only known precondition terms', () => {
    const declared = new Set<string>();
    for (const scenario of fixture.scenarios) {
      declared.add(scenario.expected_precondition);
      for (const alsoTrue of scenario.also_true ?? []) {
        declared.add(alsoTrue);
      }
    }
    for (const term of declared) {
      expect(
        Object.keys(FIXTURE_PRECONDITION_TO_KIND),
        `fixture precondition term "${term}" has no ReorderPrecondition counterpart`
      ).toContain(term);
    }
  });

  describe.each(fixture.scenarios.map((s) => [s.id, s] as const))('%s', (_id, scenario) => {
    const snapshot = snapshotOf(scenario);
    const roles = snapshot.roles.map(toRole);
    const viewer = toViewer(snapshot.actor.viewer);
    const hierarchy = partitionRoleHierarchy(roles, viewer);
    const precondition = evaluateReorderPrecondition(hierarchy, viewer);

    it('partitions the client snapshot into the declared four groups', () => {
      expect(ids(hierarchy.aboveCeiling)).toEqual(scenario.expected_hierarchy.above_ceiling);
      expect(ids(hierarchy.band)).toEqual(scenario.expected_hierarchy.band);
      expect(ids(hierarchy.managed)).toEqual(scenario.expected_hierarchy.managed);
      expect(ids(hierarchy.pinned)).toEqual(scenario.expected_hierarchy.pinned);
    });

    it('partitions every role exactly once (the groups are disjoint and total)', () => {
      const partitioned = [
        ...ids(hierarchy.aboveCeiling),
        ...ids(hierarchy.band),
        ...ids(hierarchy.managed),
        ...ids(hierarchy.pinned),
      ].sort();
      expect(partitioned).toEqual(snapshot.roles.map((r) => r.key).sort());
    });

    it('evaluates the declared precondition', () => {
      expect(precondition.kind).toBe(FIXTURE_PRECONDITION_TO_KIND[scenario.expected_precondition]);
    });

    const expectedPayload = scenario.expected_payload;

    if (expectedPayload) {
      it('builds EXACTLY the declared payload from the declared draft', () => {
        // The fixture states the draft in prose and declares the order it
        // produces (`ordering_rules.payload_order`), so the declared order IS
        // the draft. The teeth are in the constructor's permutation check and
        // in the reachability + set-equality assertions below, not in this
        // round-trip alone.
        const payload = buildReorderPayload(hierarchy, expectedPayload.role_ids);
        expect(payload.role_ids).toEqual(expectedPayload.role_ids);
      });

      it('declares a payload that is exactly the band (the table-wide invariant)', () => {
        // invariant.structural_consequence: for every scenario carrying an
        // expected_payload, set(role_ids) == set(band). NOTHING MORE (#2839's
        // loud 403), NOTHING LESS (the silent subset collision).
        expect([...expectedPayload.role_ids].sort()).toEqual(
          [...scenario.expected_hierarchy.band].sort()
        );
      });

      it('declares a payload reachable from the band display order by one drag', () => {
        expect(
          singleMoveTo(ids(hierarchy.band), expectedPayload.role_ids),
          'declared payload is not one moveWithinBand away from the band display order'
        ).not.toBeNull();
      });
    } else {
      it('has no payload to build — the band cannot be projected', () => {
        // expected_payload: null means the client must send NOTHING. Never [] —
        // the server refuses an empty array at the binding (min=1).
        const refuses =
          precondition.kind !== 'ok' ||
          (() => {
            try {
              buildReorderPayload(hierarchy, ids(hierarchy.band));
              return false;
            } catch {
              return true;
            }
          })();
        expect(refuses).toBe(true);
      });
    }

    it.each(scenario.probes.map((probe) => [probe.id, probe] as const))(
      'cannot emit probe %s',
      (_probeId, probe) => {
        expect(
          clientCanEmit(hierarchy, precondition, probe.role_ids),
          `${probe.id}: a correct client must never build ${probe.wrong_builder}`
        ).toBe(false);
      }
    );
  });

  // --- Meta-test: mirrors the Go side's
  // TestRoleReorderContract_FalsificationProbesAreDeclared, so deleting a probe
  // reds BOTH suites. A table whose red demonstrations have been quietly
  // removed still passes every other assertion in this file.
  describe('falsification probes are declared', () => {
    function scenarioForSpec(specScenario: number): FixtureScenario {
      const scenario = fixture.scenarios.find((s) => s.spec_scenario === specScenario);
      if (!scenario) {
        throw new Error(`fixture: no scenario for spec scenario ${specScenario}`);
      }
      return scenario;
    }

    it('spec scenario 1 carries a full-display-list falsification probe', () => {
      // #2839's ACTUAL behaviour: every rendered role, @all last. Loud 403.
      const probes = scenarioForSpec(1).probes.filter((p) => p.purpose === 'falsification');
      expect(probes.map((p) => p.wrong_builder).join(' ')).toContain('full-display-list');
    });

    it('spec scenario 6 carries a dragged-roles-only falsification probe', () => {
      // The obvious correction to #2839, and worse: HTTP 200, duplicate
      // positions, no error anywhere.
      const probes = scenarioForSpec(6).probes.filter((p) => p.purpose === 'falsification');
      expect(probes.map((p) => p.wrong_builder).join(' ')).toContain('dragged-roles-only');
    });

    it('every falsification probe differs from its scenario expected_payload', () => {
      const falsifications = fixture.scenarios.flatMap((scenario) =>
        scenario.probes
          .filter((probe) => probe.purpose === 'falsification')
          .map((probe) => ({ scenario, probe }))
      );
      expect(falsifications.length).toBeGreaterThanOrEqual(2);

      for (const { scenario, probe } of falsifications) {
        // A falsification probe identical to the correct payload falsifies
        // nothing — it would be green against the very builder it accuses.
        expect(
          scenario.expected_payload,
          `${probe.id}: scenario declares no payload`
        ).not.toBeNull();
        expect(
          probe.role_ids.join('|'),
          `${probe.id} does not differ from expected_payload`
        ).not.toBe(scenario.expected_payload?.role_ids.join('|'));
      }
    });
  });
});
