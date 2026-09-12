/**
 * Client/server camera layer-policy parity.
 *
 * `computeRemoteVideoLayerRequest` (desktop renderer) and `layerForRender`
 * (this service, reached via `storedDemand`) implement the same ladder twice:
 * the same 540/1280 edges, the same thumbnail->0 / grid->1 caps, the same
 * unfocused and pressure decrements. Each had its own suite; nothing pinned
 * them in AGREEMENT.
 *
 * #3094 made that newly load-bearing for a safety property. Its L2 guard uses
 * the CLIENT copy to predict whether the SERVER can still land a step, and
 * falls through to an IGNIS pause when it predicts no. Mispredict one way and
 * the single per-user pressure budget is burned on a request the SFU will not
 * honour; mispredict the other and a viewer is paused while a real step was
 * still available.
 *
 * The two are NOT textually identical, and the difference is the point:
 *
 *   client:  max(max(0,w) * sanitizedDpr, max(0,h) * sanitizedDpr)
 *   server:  max(w, h) * rawDpr
 *
 * Algebraically equal for positive finite inputs, since max(a*d, b*d) ===
 * max(a,b)*d when d > 0. They part company exactly where the client's guards
 * bite: a devicePixelRatio of 0, negative, NaN or Infinity, or a negative CSS
 * dimension. `parseCameraLayerDemand` refuses every one of those before
 * `layerForRender` is reached, so the divergence is unreachable over the wire.
 *
 * Parity is therefore a property OF THE ADMITTED DOMAIN, not of the two
 * functions in general — which is why this file sweeps the grid the parser
 * accepts and then proves, separately, that the parser is what keeps it true.
 */
import { describe, it, expect } from 'vitest';
import {
  clampCameraLayerDemand,
  parseCameraLayerDemand,
  storedDemand,
} from '@/lib/cameraLayerGovernor';
import { computeRemoteVideoLayerRequest } from '../../../client/desktop/src/renderer/services/voice/remoteVideoLayerPolicy';

const ROLES = ['thumbnail', 'grid', 'focus'] as const;
// Sizes straddle BOTH ladder edges (540, 1280) from either side, so an
// off-by-one introduced on one side of the boundary in either copy is caught.
//
// The PORTRAIT half is not symmetry for its own sake. Both copies reduce the
// tile to `Math.max(width, height)`, and every landscape fixture has
// `width >= height` — so replacing either `Math.max(w, h)` with a bare `w`
// leaves this entire suite green while the two copies disagree for any
// height-dominant tile. 300x541 is the worked case: max-edge 541 crosses the
// 540 edge, width-only 300 does not. A parity suite that cannot see its own
// reduction step is pinning the ladder and nothing else (Codex, #3277).
const LANDSCAPE_SIZES: ReadonlyArray<readonly [number, number]> = [
  [0, 0],
  [160, 90],
  [539, 300],
  [540, 304],
  [541, 304],
  [960, 540],
  [1279, 719],
  [1280, 720],
  [1281, 721],
  [1920, 1080],
  [3840, 2160],
];
// Swapped around BOTH boundaries, so the height-dominant path crosses each
// edge from either side exactly as the landscape set does.
const PORTRAIT_SIZES: ReadonlyArray<readonly [number, number]> = [
  [300, 539],
  [304, 540],
  [300, 541],
  [540, 960],
  [719, 1279],
  [720, 1280],
  [721, 1281],
  [1080, 1920],
];
const SIZES: ReadonlyArray<readonly [number, number]> = [...LANDSCAPE_SIZES, ...PORTRAIT_SIZES];
// Every value here must satisfy isValidDevicePixelRatio (finite, >0, <=8).
const DPRS = [1, 1.25, 1.5, 2, 3, 8] as const;

// BOTH entitlement caps, because the server applies one and the client models
// neither. `maxCameraSpatialLayerForParticipant` (roomManager.ts) returns 1 for
// a free viewer and 2 for a premium one, and the sweep used to hardcode 2 — so
// the free-tier domain, which is most viewers, went entirely untested.
//
// Parity is therefore parity MODULO THE CAP, and saying so is the honest claim:
// the two copies compute the same ladder, and the server then applies an
// entitlement clamp the client does not model at all. At cap 2 they agree
// outright; at cap 1 the server is `min(client, 1)`. See the free-tier test at
// the bottom of this file for what that costs #3094's L2 guard.
const MAX_SPATIAL_LAYERS = [1, 2] as const;

describe('camera layer policy parity (client renderer vs media-plane governor)', () => {
  it('agrees on the spatial layer for every demand the parser admits, modulo the entitlement cap', () => {
    const mismatches: string[] = [];
    let checked = 0;

    for (const maxSpatialLayer of MAX_SPATIAL_LAYERS) {
      for (const role of ROLES) {
        for (const [cssWidth, cssHeight] of SIZES) {
          for (const devicePixelRatio of DPRS) {
            for (const focusedWindow of [true, false]) {
              for (const pressureStepDown of [true, false]) {
                for (const visible of [true, false]) {
                  const shared = {
                    visible,
                    cssWidth,
                    cssHeight,
                    devicePixelRatio,
                    role,
                    focusedWindow,
                    pressureStepDown,
                  };

                  // Guard the guard: a fixture the parser would reject cannot
                  // speak to parity, and silently including one would let this
                  // test drift outside the domain the claim covers.
                  const parsed = parseCameraLayerDemand({
                    consumerId: 'c1',
                    spatialLayer: 2,
                    temporalLayer: 2,
                    ...shared,
                  });
                  expect(parsed.ok).toBe(true);

                  const client = computeRemoteVideoLayerRequest(shared);
                  const server = storedDemand(
                    { consumerId: 'c1', spatialLayer: 2, temporalLayer: 2, ...shared },
                    maxSpatialLayer
                  ).maxUsefulSpatialLayer;

                  // The cap is applied HERE rather than folded into the client
                  // call, because the client genuinely does not take one — that
                  // asymmetry is the finding, not an artefact of the fixture.
                  const expected = Math.min(client.spatialLayer, maxSpatialLayer);

                  checked++;
                  if (expected !== server) {
                    mismatches.push(
                      `cap=${maxSpatialLayer} ${role} ${cssWidth}x${cssHeight} ` +
                        `dpr=${devicePixelRatio} focus=${focusedWindow} ` +
                        `pressure=${pressureStepDown} visible=${visible}: ` +
                        `client=${client.spatialLayer} expected=${expected} server=${server}`
                    );
                  }
                }
              }
            }
          }
        }
      }
    }

    expect(mismatches).toEqual([]);
    expect(checked).toBe(
      MAX_SPATIAL_LAYERS.length * ROLES.length * SIZES.length * DPRS.length * 2 * 2 * 2 // vacuity floor
    );
    // The portrait half must actually be in the sweep. The floor above is a
    // product, so dropping PORTRAIT_SIZES and editing the expected count keeps
    // it green — this is what makes that impossible without a visible lie.
    expect(SIZES.length).toBe(LANDSCAPE_SIZES.length + PORTRAIT_SIZES.length);
    expect(PORTRAIT_SIZES.every(([w, h]) => h > w)).toBe(true);
  });

  it('never clamps a correctly-computed PREMIUM request, which is what L2 predicts', () => {
    // L2 asks "can the server still land a step?" by computing the pressured
    // and unpressured payloads CLIENT-side. That prediction is only sound if a
    // request the client computed survives the server's clamp untouched — which
    // holds at cap 2 and NOT at cap 1. The title says "premium" for that reason;
    // the free half is the next test, and it is the one with teeth.
    for (const role of ROLES) {
      for (const [cssWidth, cssHeight] of SIZES) {
        for (const pressureStepDown of [true, false]) {
          // HIDDEN as well as visible. A hidden tile is parser-admitted and is
          // the ONLY client request carrying `temporalLayer: 0` — every visible
          // one is 1 or 2 — so pinning the no-clamp guarantee on visible
          // requests alone leaves the entire temporal floor untested. Dropping
          // the incoming-temporal minimum from clampCameraLayerDemand would
          // return 1 for a hidden request whose client value is 0, clamping a
          // correctly-computed request while every other assertion in this file
          // stayed green: the broader sweep above compares only
          // maxUsefulSpatialLayer (Codex, #3277).
          for (const visible of [true, false]) {
            const shared = {
              visible,
              cssWidth,
              cssHeight,
              devicePixelRatio: 2,
              role,
              focusedWindow: true,
              pressureStepDown,
            };
            const client = computeRemoteVideoLayerRequest(shared);
            const clamped = clampCameraLayerDemand(
              {
                consumerId: 'c1',
                spatialLayer: client.spatialLayer,
                temporalLayer: client.temporalLayer,
                ...shared,
              },
              2
            );
            // BOTH layers, not just spatial — the temporal half is the whole
            // point of sweeping visibility.
            expect({ role, cssWidth, cssHeight, pressureStepDown, visible, ...clamped }).toEqual({
              role,
              cssWidth,
              cssHeight,
              pressureStepDown,
              visible,
              spatialLayer: client.spatialLayer,
              temporalLayer: client.temporalLayer,
            });
          }
        }
      }
    }

    // The hidden half must actually be reachable: a client request with
    // temporalLayer 0 has to exist, or the paragraph above describes a case the
    // sweep never produces.
    expect(
      computeRemoteVideoLayerRequest({
        visible: false,
        cssWidth: 1920,
        cssHeight: 1080,
        devicePixelRatio: 2,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ visible: false, spatialLayer: 0, temporalLayer: 0 });
  });

  // This test PINS A DIVERGENCE rather than an agreement, and it is deliberately
  // written so that fixing the production defect turns it red.
  //
  // `maxPressureSteps(request)` is `request.visible ? request.spatialLayer : 0`,
  // computed entirely client-side, and #3094's L2 guard refuses a step only when
  // `nextSteps > maxPressureSteps(unpressured)`. For a FREE viewer the server has
  // already clamped to layer 1, so on a large focused tile the client believes it
  // holds two steps when the SFU will honour one — and the first step it spends
  // moves the forwarded layer from 1 to 1. IGNIS records a step that reduced no
  // decoder load, the zone stays red, and relief is delayed by a full cycle.
  //
  // The fix is client-side (teach the layer policy the viewer's own entitlement
  // cap), so it does not belong in a test-only PR. Pinning it here means the next
  // change to that policy cannot land silently: it will fail this test and have
  // to say what it did.
  it('OVERSTATES the available pressure steps for a free viewer (known #3094 L2 gap)', () => {
    const largeFocusedTile = {
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 2,
      role: 'focus' as const,
      focusedWindow: true,
      pressureStepDown: false,
    };

    const client = computeRemoteVideoLayerRequest(largeFocusedTile);
    expect(client.spatialLayer).toBe(2); // what L2 counts its steps against

    const freeCap = 1; // maxCameraSpatialLayerForParticipant, free branch
    const unpressuredServer = storedDemand(
      { consumerId: 'c1', spatialLayer: 2, temporalLayer: 2, ...largeFocusedTile },
      freeCap
    ).maxUsefulSpatialLayer;
    const pressuredServer = storedDemand(
      {
        consumerId: 'c1',
        spatialLayer: 2,
        temporalLayer: 2,
        ...largeFocusedTile,
        pressureStepDown: true,
      },
      freeCap
    ).maxUsefulSpatialLayer;

    // The whole finding in two assertions: the client thinks it has a step to
    // spend, and spending it changes nothing the SFU forwards.
    expect(unpressuredServer).toBe(1);
    expect(pressuredServer).toBe(1);
    expect(client.spatialLayer).toBeGreaterThan(unpressuredServer);
  });

  it('is the PARSER that keeps the two in agreement, not the arithmetic', () => {
    // The inputs below are exactly where the two implementations disagree. Each
    // must be refused before layerForRender is ever reached. If a future change
    // relaxes any of these, parity above stops being true and this test says so.
    const divergent = [
      { label: 'dpr 0', devicePixelRatio: 0 },
      { label: 'dpr negative', devicePixelRatio: -2 },
      { label: 'dpr NaN', devicePixelRatio: Number.NaN },
      { label: 'dpr Infinity', devicePixelRatio: Number.POSITIVE_INFINITY },
      { label: 'dpr above the cap', devicePixelRatio: 8.01 },
    ];
    for (const { label, devicePixelRatio } of divergent) {
      const parsed = parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 2,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1920,
        cssHeight: 1080,
        devicePixelRatio,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      });
      expect(parsed.ok, label).toBe(false);
    }

    for (const label of ['negative width', 'negative height']) {
      const parsed = parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 2,
        temporalLayer: 2,
        visible: true,
        cssWidth: label === 'negative width' ? -1920 : 1920,
        cssHeight: label === 'negative height' ? -1080 : 1080,
        devicePixelRatio: 2,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      });
      expect(parsed.ok, label).toBe(false);
    }
  });
});
