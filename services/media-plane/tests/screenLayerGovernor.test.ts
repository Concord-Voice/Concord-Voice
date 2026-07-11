import { describe, it, expect } from 'vitest';
import {
  computeScreenLayeringGate,
  type StoredScreenLayerDemand,
} from '../src/lib/screenLayerGovernor';

// userId defaults to the consumerId, so distinct consumers are distinct viewers
// unless a test overrides userId to model one client owning several consumers.
// sharerUserId defaults to a single shared value: computeScreenLayeringGate is
// sharer-agnostic (the CALLER pre-filters by sharerUserId, #1924 fix "B"), so
// these unit tests model one sharer's already-filtered viewer set.
const demand = (o: Partial<StoredScreenLayerDemand>): StoredScreenLayerDemand => ({
  consumerId: 'c',
  userId: o.consumerId ?? 'c',
  sharerUserId: 'sharer',
  visible: true,
  maxUsefulSpatialLayer: 1,
  pressureStepDown: false,
  ...o,
});

describe('computeScreenLayeringGate', () => {
  it('is OFF with fewer than two visible demands', () => {
    expect(computeScreenLayeringGate({ demands: [demand({ consumerId: 'a' })] })).toBe(false);
  });
  it('is ON with two DISTINCT viewers where one wants a sub-top layer', () => {
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({ consumerId: 'a', userId: 'alice', maxUsefulSpatialLayer: 0 }),
          demand({ consumerId: 'b', userId: 'bob', maxUsefulSpatialLayer: 2 }),
        ],
      })
    ).toBe(true);
  });
  it('is OFF when ONE client owns two visible screen consumers (single client cannot trip the gate)', () => {
    // #1924 review fix: the gate counts distinct owning userIds, not consumer
    // entries. A single attacker consuming a victim's screen twice must NOT
    // enable the room gate and force honest publishers into 3x simulcast.
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({ consumerId: 'a', userId: 'attacker', maxUsefulSpatialLayer: 0 }),
          demand({ consumerId: 'b', userId: 'attacker', maxUsefulSpatialLayer: 0 }),
        ],
      })
    ).toBe(false);
  });
  it('is OFF at two distinct viewers that all want the top layer (no benefit)', () => {
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({ consumerId: 'a', userId: 'alice', maxUsefulSpatialLayer: 2 }),
          demand({ consumerId: 'b', userId: 'bob', maxUsefulSpatialLayer: 2 }),
        ],
      })
    ).toBe(false);
  });
  it('stays ON at the one-viewer hysteresis floor when previouslyEnabled', () => {
    expect(
      computeScreenLayeringGate({
        demands: [demand({ consumerId: 'a', userId: 'alice', maxUsefulSpatialLayer: 0 })],
        previouslyEnabled: true,
      })
    ).toBe(true);
  });
  it('honors pressure-only benefit across two distinct viewers', () => {
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({
            consumerId: 'a',
            userId: 'alice',
            maxUsefulSpatialLayer: 2,
            pressureStepDown: true,
          }),
          demand({ consumerId: 'b', userId: 'bob', maxUsefulSpatialLayer: 2 }),
        ],
      })
    ).toBe(true);
  });
  it('ignores hidden demands', () => {
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({ consumerId: 'a', userId: 'alice', visible: false, maxUsefulSpatialLayer: 0 }),
          demand({ consumerId: 'b', userId: 'bob', maxUsefulSpatialLayer: 0 }),
        ],
      })
    ).toBe(false);
  });
  it('does NOT depend on producer count — ON with two heterogeneous viewers regardless', () => {
    // Screen gate is demand-driven only; there is no producer-count arm (contrast camera).
    expect(
      computeScreenLayeringGate({
        demands: [
          demand({ consumerId: 'a', userId: 'alice', maxUsefulSpatialLayer: 0 }),
          demand({ consumerId: 'b', userId: 'bob', maxUsefulSpatialLayer: 1 }),
        ],
      })
    ).toBe(true);
  });
});
