/**
 * #1924 simulcast-screenshare quality fixes (client-side).
 *
 * Fix #5 — `removeRemoteVideoTile` clears screen demand on the LAST-surface unmount by
 *   emitting an explicit `visible:false` set-preferred-layers, instead of dropping local
 *   state silently (which pinned the layer/gate on the last visible demand). Camera is
 *   unchanged — hidden-ness routes through the pause coordinator, not a demand emit.
 *
 * Codec intent remains stable when a layering toggle is disabled. The selected codec keeps
 *   its priority; its encoding plan degrades from the codec's layered mode to a single stream.
 *   Simulcast stays server-gated on `screenLayeringEnabled`.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

describe('voiceService screen demand cleared on last-surface unmount (#1924 Fix #5)', () => {
  let svc: any;
  let emit: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.pauseCoordinator.reset();
    svc.tileVisibilityByUser.clear();
    svc.remoteVideoPressureByUser?.clear();
    svc.lastPreferredLayerKeyByConsumer?.clear();
    svc.remoteVideoRenderStateByUser?.clear();
    svc.remoteScreenRenderStateByUser?.clear();
    svc.documentHidden = false;
    (globalThis as any).devicePixelRatio = 1;
    emit = vi.fn();
    svc.socket = { emit };
  });

  const seedScreenState = (userId: string, tileId: string, over: Record<string, unknown> = {}) =>
    svc.setRemoteVideoRenderState(
      userId,
      tileId,
      { visible: true, cssWidth: 1280, cssHeight: 720, role: 'grid', focusedWindow: true, ...over },
      'screen'
    );

  const splCalls = () => emit.mock.calls.filter((c: unknown[]) => c[0] === 'set-preferred-layers');

  it('the LAST screen surface removal emits a visible:false demand for the screen consumer', () => {
    svc.consumerMeta.set('cons-screen', { source: 'screen', producerUserId: 'u1' });
    seedScreenState('u1', 'tile-1');
    emit.mockClear();

    svc.removeRemoteVideoTile('u1', 'tile-1', 'screen');

    const spl = splCalls();
    expect(spl.length).toBeGreaterThan(0);
    const payload = spl[spl.length - 1][1] as { consumerId: string; visible: boolean };
    expect(payload.consumerId).toBe('cons-screen');
    expect(payload.visible).toBe(false);
    // Local per-user screen state is pruned once the last surface is gone.
    expect(svc.remoteScreenRenderStateByUser.has('u1')).toBe(false);
  });

  it('a NON-last screen surface removal never emits visible:false (a visible tile remains)', () => {
    svc.consumerMeta.set('cons-screen', { source: 'screen', producerUserId: 'u1' });
    seedScreenState('u1', 'tile-1');
    seedScreenState('u1', 'tile-2', { cssWidth: 640, cssHeight: 360, role: 'thumbnail' });
    svc.lastPreferredLayerKeyByConsumer.clear();
    emit.mockClear();

    svc.removeRemoteVideoTile('u1', 'tile-1', 'screen');

    const spl = splCalls();
    expect(spl.every((c: unknown[]) => (c[1] as { visible: boolean }).visible !== false)).toBe(
      true
    );
    // It still recomputes the remaining tile's (visible) demand.
    expect(spl.some((c: unknown[]) => (c[1] as { visible: boolean }).visible === true)).toBe(true);
    // The remaining tile keeps this user's screen demand alive.
    expect(svc.remoteScreenRenderStateByUser.get('u1')?.has('tile-2')).toBe(true);
  });

  it('the last CAMERA surface removal does NOT emit set-preferred-layers (camera branch unchanged)', () => {
    const consumer = { id: 'cons-cam', kind: 'video', pause: vi.fn(), resume: vi.fn() };
    svc.consumers.set('cons-cam', consumer);
    svc.consumerMeta.set('cons-cam', {
      source: 'camera',
      producerUserId: 'u2',
      producerId: 'p-cam',
    });
    svc.setRemoteVideoRenderState(
      'u2',
      'cam-tile',
      { visible: true, cssWidth: 1280, cssHeight: 720, role: 'grid', focusedWindow: true },
      'camera'
    );
    emit.mockClear();

    svc.removeRemoteVideoTile('u2', 'cam-tile', 'camera');

    // Camera hidden-ness routes through the pause coordinator — never a demand emit here.
    expect(splCalls().length).toBe(0);
  });
});

// NOTE: this fixture pins `hardwareAcceleration: true` with `webrtcHwByMime: {}` and
// `codecCapabilities: []` — i.e. NO hardware evidence. `isHwAccelerated` therefore returns false
// for every key, `findPreferredCandidate` always asks for the 'hardware' backend, and
// `findCandidate` rejects every 'hardware' probe when `isHwAccelerated` is false — so
// `preferredVideoCodec` is discarded on EVERY case below and the Auto ladder always restarts at
// candidate zero. That discard is intended, documented behavior, not a bug
// (`[internal]rules/frontend.md`, "backend-eligible"). These six cases exercise the ENCODING PLAN
// only (simulcast/SVC/single collapse, bitrate derivation) — never codec intent. For codec-intent
// coverage see the sibling block below, "voiceService.pickScreenCodec — explicit codec preference
// survives the layering plan", whose fixture sets `hardwareAcceleration: false` so the
// preferred-codec probe asks for the 'software' backend, which `findCandidate` never gates on
// `isHwAccelerated` at all.
//
// One of these six is ALSO inert with respect to a SECOND thing its name suggests it tests: "gate
// OFF … collapses AV1 + Simulcast to a single stream" exercises the `supportSvc`/`supportSimulcast`
// eligibility collapse, not the `screenLayeringEnabled` GATE its name calls out. AV1 is an
// SVC-kind codec (`castingKindForCodec`), so `eligibility.simulcast` — and by extension
// `this.screenLayeringEnabled` — is never read for it; `supportSvc: false` alone fully determines
// the single-encoding outcome. Deleting the `&& this.screenLayeringEnabled` conjunct from
// `pickScreenCodec`'s eligibility ([internal]'s `risk: security` capacity guardrail — the thing
// that makes simulcast screen server-authoritative) changes nothing in this test. That is what
// makes the gate-OFF VP8/H.264 cases in the sibling block below load-bearing rather than merely
// thorough: they are the only cases in this file that actually exercise that conjunct.
describe('voiceService.pickScreenCodec — encoding plan for an Auto-selected AV1 across layering fallbacks', () => {
  let svc: any;

  beforeEach(() => {
    svc = voiceService as any;
    // A minimal send capability set: AV1 (SVC-kind) + VP8 (simulcast-kind fallback codec).
    svc.device = {
      rtpCapabilities: {
        codecs: [
          { mimeType: 'video/AV1', kind: 'video', clockRate: 90000, parameters: {} },
          { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
        ],
      },
    };
    useVoiceStore.setState({ codecFloor: null }); // every ladder rung admissible
    useVideoSettingsStore.setState({
      preferredVideoCodec: 'video/AV1',
      screenShareBitrate: 2_000_000, // non-zero → skip calculateScreenBitrate()
      screenSharePriority: 'medium',
      scalabilityMode: 'auto',
      hardwareAcceleration: true,
      hdrEncoding: false,
      webrtcHwByMime: {},
      codecCapabilities: [],
    });
  });

  it('Auto-selected AV1 falls back to a single stream when SVC is off (#2242)', () => {
    svc.screenLayeringEnabled = true;
    useVideoSettingsStore.setState({ supportSvc: false, supportSimulcast: true });

    const { codec, encodings } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(encodings).toHaveLength(1);
    expect(encodings[0].rid).toBeUndefined();
    expect(encodings[0].scalabilityMode).toBeUndefined();
  });

  it('Auto-selected AV1 + Support SVC ON → single encoding with a scalabilityMode', () => {
    svc.screenLayeringEnabled = true;
    useVideoSettingsStore.setState({ supportSvc: true, supportSimulcast: false });

    const { codec, encodings } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(encodings).toHaveLength(1);
    expect(encodings[0].scalabilityMode).toBeTruthy();
    expect(encodings[0].rid).toBeUndefined();
  });

  it('gate OFF (screenLayeringEnabled=false) collapses AV1 + Simulcast to a single stream', () => {
    svc.screenLayeringEnabled = false;
    useVideoSettingsStore.setState({ supportSvc: false, supportSimulcast: true });

    const { codec, encodings } = svc.pickScreenCodec();
    // Simulcast is server-gated; with the gate off there is no layering fallback.
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(encodings).toHaveLength(1);
    expect(encodings[0].rid).toBeUndefined();
  });

  it('AUTO bitrate follows the Auto-selected AV1 codec when SVC is off (#2242)', () => {
    svc.screenLayeringEnabled = true;
    useVoiceStore.setState({ activeScreenCodec: null });
    useVideoSettingsStore.setState({
      supportSvc: false,
      supportSimulcast: true,
      screenShareBitrate: 0, // auto → exercise calculateScreenBitrate()
      screenResolution: '1080p',
      screenFrameRate: 30,
    });

    const { codec, effectiveBitrate } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(effectiveBitrate).toBe(2_500_000);
  });

  it('AUTO bitrate uses AV1 (0.04 bpp) when the ladder keeps AV1 (SVC on) (#1924 review)', () => {
    svc.screenLayeringEnabled = true;
    useVoiceStore.setState({ activeScreenCodec: null });
    useVideoSettingsStore.setState({
      supportSvc: true,
      supportSimulcast: false,
      screenShareBitrate: 0,
      screenResolution: '1080p',
      screenFrameRate: 30,
    });

    const { codec, effectiveBitrate } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    // AV1 (0.04 bpp): 1920*1080*30*0.04 ≈ 2.49 Mbps → rounded to 2.5 Mbps.
    expect(effectiveBitrate).toBe(2_500_000);
  });

  it('a non-zero user bitrate override is honored verbatim regardless of the ladder codec', () => {
    svc.screenLayeringEnabled = true;
    useVideoSettingsStore.setState({
      supportSvc: false,
      supportSimulcast: true,
      screenShareBitrate: 3_333_000, // manual override
      screenResolution: '1080p',
      screenFrameRate: 30,
    });

    const { codec, effectiveBitrate } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(effectiveBitrate).toBe(3_333_000);
  });
});

describe('voiceService.pickScreenCodec — explicit codec preference survives the layering plan', () => {
  let svc: any;

  beforeEach(() => {
    svc = voiceService as any;
    // Same shape as the sibling fixture above, with THREE differences, not one: (1)
    // `hardwareAcceleration: false` — `findPreferredCandidate`'s backend probe now asks for the
    // 'software' backend, and `findCandidate` only gates on `isHwAccelerated` when a candidate's
    // backend is 'hardware', so the preference is actually reachable here regardless of
    // `webrtcHwByMime`/`codecCapabilities`; (2) `preferredVideoCodec` seeded `null` instead of
    // `'video/AV1'`, because every case below sets its own preference; and (3) an added H.264
    // capability with a realistic `profile-level-id` (High profile, '640034') so a bare
    // `video/H264` preference resolves through `H264_CODEC_KEYS` without a production change. A
    // `video/VP9` capability is also present (added for the SVC gate-off case below) — its
    // `parameters: {}` defaults to Profile 0 via `findSendCodec`. Includes a simulcast-kind codec
    // (VP8) alongside the SVC-kind AV1/VP9 so `castingKindForCodec` can route through simulcast.
    //
    // There is deliberately no "AV1 preferred" case anywhere in this block: with
    // `hardwareAcceleration` off and `hdrEncoding` off, AV1:sdr is already Auto's rank-0
    // candidate, so an AV1 preference is structurally indistinguishable from the unpreferenced
    // default here — adding one would recreate the exact vacuity the sibling block's annotation
    // warns about.
    svc.device = {
      rtpCapabilities: {
        codecs: [
          { mimeType: 'video/AV1', kind: 'video', clockRate: 90000, parameters: {} },
          { mimeType: 'video/VP9', kind: 'video', clockRate: 90000, parameters: {} },
          { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
          {
            mimeType: 'video/H264',
            kind: 'video',
            clockRate: 90000,
            parameters: { 'profile-level-id': '640034' },
          },
        ],
      },
    };
    useVoiceStore.setState({ codecFloor: null }); // every ladder rung admissible
    useVideoSettingsStore.setState({
      preferredVideoCodec: null,
      screenShareBitrate: 2_000_000, // non-zero → skip calculateScreenBitrate()
      screenSharePriority: 'medium',
      scalabilityMode: 'auto',
      hardwareAcceleration: false,
      hdrEncoding: false,
      webrtcHwByMime: {},
      codecCapabilities: [],
    });
  });

  afterEach(() => {
    // This block (unlike its sibling above) mutates the voiceService singleton's `device` and
    // `screenLayeringEnabled`, plus the shared `codecFloor` store — reset them so this file stays
    // order-independent for any future insertion. Scoped to this block only; the old block above
    // is out of scope for this cleanup ([internal] boy-scout rule applies to lines actually edited).
    svc.device = null;
    svc.screenLayeringEnabled = false;
    useVoiceStore.setState({ codecFloor: null });
  });

  // Every column here is live: the table used to hold `gateOn`/`expectedLen`/`expectRid`
  // constant across both rows (making the `else` "no rid" arm dead code). Folding in the H.264
  // gate-OFF row plus Gaps C and D below makes every column vary across at least one row.
  it.each([
    {
      label: 'VP8 + gate ON',
      preferred: 'video/VP8',
      gateOn: true,
      supportSimulcast: true,
      supportSvc: false,
      expectedMime: 'video/vp8',
      expectedLen: 3,
      expectRid: true,
      expectedProfile: undefined,
    },
    {
      label: 'H.264 + gate ON',
      preferred: 'video/H264',
      gateOn: true,
      supportSimulcast: true,
      supportSvc: false,
      expectedMime: 'video/h264',
      expectedLen: 3,
      expectRid: true,
      expectedProfile: '640034',
    },
    {
      // Folded in from the retired standalone "H.264 preferred + gate OFF" case — this is what
      // makes `gateOn`/`expectedLen`/`expectRid` vary against the two rows above instead of
      // holding constant.
      label: 'H.264 + gate OFF',
      preferred: 'video/H264',
      gateOn: false,
      supportSimulcast: true,
      supportSvc: false,
      expectedMime: 'video/h264',
      expectedLen: 1,
      expectRid: false,
      expectedProfile: '640034',
    },
    {
      // Gap C (criticality 7): locks the Support-Simulcast USER toggle for screen. A mutant
      // reducing `pickScreenCodec`'s eligibility from `vs.supportSimulcast && this.screenLayeringEnabled`
      // to just `this.screenLayeringEnabled` survives every other row here — they all leave
      // `supportSimulcast` at its default `true` — making the setting a silent no-op. This row
      // is the only one that varies `supportSimulcast` while the gate stays ON.
      label: 'VP8 + gate ON, Support Simulcast OFF (Gap C)',
      preferred: 'video/VP8',
      gateOn: true,
      supportSimulcast: false,
      supportSvc: false,
      expectedMime: 'video/vp8',
      expectedLen: 1,
      expectRid: false,
      expectedProfile: undefined,
    },
    {
      // Gap D (criticality 7): locks that the layering KIND is codec-derived
      // (`castingKindForCodec`), never toggle-derived. A mutant that computed kind from
      // `supportSvc` instead of the codec's mime would turn this VP8 case into a single SVC
      // encoding (length 1, no rid, a `scalabilityMode`); asserting 3 simulcast rids here
      // catches it even though `supportSvc` is ON.
      label: 'VP8 + gate ON, Support SVC ON (Gap D — stays simulcast, not svc)',
      preferred: 'video/VP8',
      gateOn: true,
      supportSimulcast: true,
      supportSvc: true,
      expectedMime: 'video/vp8',
      expectedLen: 3,
      expectRid: true,
      expectedProfile: undefined,
    },
  ])(
    '$label → codec stays $expectedMime with $expectedLen simulcast encoding(s)',
    ({
      preferred,
      gateOn,
      supportSimulcast,
      supportSvc,
      expectedMime,
      expectedLen,
      expectRid,
      expectedProfile,
    }) => {
      svc.screenLayeringEnabled = gateOn;
      useVideoSettingsStore.setState({
        preferredVideoCodec: preferred,
        supportSimulcast,
        supportSvc,
      });

      const { codec, encodings } = svc.pickScreenCodec();
      expect(codec?.mimeType?.toLowerCase()).toBe(expectedMime);
      expect(encodings).toHaveLength(expectedLen);
      if (expectRid) {
        expect(encodings.map((e: { rid?: string }) => e.rid)).toEqual(['q', 'h', 'f']);
      } else {
        expect(encodings[0].rid).toBeUndefined();
      }
      // Gap: pin the H.264 profile the fixture makes load-bearing — the ladder's '640034' is
      // what lets a bare `video/H264` preference resolve at all (`H264_CODEC_KEYS`), but
      // nothing previously asserted which profile came back. Precedent:
      // voiceService.test.ts's "pickCameraCodec preserves a floor-compatible manual H264
      // preference" case.
      if (expectedProfile) {
        expect(codec?.parameters?.['profile-level-id']).toBe(expectedProfile);
      }
    }
  );

  // THE MOST IMPORTANT CASE (regression lock): the retired `pickScreenLayeringCodec` ladder
  // dropped the preferred codec entirely when the server-side layering gate was off, falling
  // back to whatever Auto picked (AV1 by rank) instead of keeping the user's chosen codec as a
  // single stream. `pickScreenCodec`/`buildCameraEncodingPlan` must never repeat that: the codec
  // is selected BEFORE the eligibility gate is consulted, so the gate can only collapse the
  // ENCODING PLAN, never substitute a different codec.
  it('VP8 preferred + gate OFF → codec stays video/VP8 (does NOT fall back to AV1), single stream', () => {
    svc.screenLayeringEnabled = false;
    useVideoSettingsStore.setState({
      preferredVideoCodec: 'video/VP8',
      supportSimulcast: true,
      supportSvc: false,
    });

    const { codec, encodings } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/vp8');
    expect(encodings).toHaveLength(1);
    expect(encodings[0].rid).toBeUndefined();
  });

  // Gap A (criticality 7): `pickScreenCodec`'s doc comment calls SVC "server-passive,
  // cost-neutral, ungated" — i.e. AV1/VP9 screen layering must never read
  // `screenLayeringEnabled`. That claim was unfalsifiable: a mutant writing
  // `svc: vs.supportSvc && this.screenLayeringEnabled` into `pickScreenCodec`'s eligibility
  // (adding a gate conjunct that production never has) survives every pre-existing case in this
  // file and would silently disable AV1/VP9 screen layering in every gate-off room. Assert the
  // gate-OFF SVC outcome directly, with VP9 so AV1 is not the only SVC-kind codec this file ever
  // exercises through the gate.
  it('VP9 preferred + Support SVC ON, gate OFF → still SVC (1 encoding, scalabilityMode set)', () => {
    svc.screenLayeringEnabled = false;
    useVideoSettingsStore.setState({
      preferredVideoCodec: 'video/VP9',
      supportSimulcast: false,
      supportSvc: true,
    });

    const { codec, encodings } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/vp9');
    expect(encodings).toHaveLength(1);
    expect(encodings[0].scalabilityMode).toBeTruthy();
  });

  // Preference rejected by the codec floor: the preferred codec (VP8) is excluded from
  // `codecFloor`, so `findPreferredCandidate` fails and Auto restarts at candidate zero, landing
  // on AV1 by rank — never on the excluded preference. Contrast with the VP8 + gate ON row above
  // (same preference, `codecFloor: null`), which resolves to VP8: the only variable changed here
  // is the floor, and it is what flips the outcome.
  //
  // Gap B (criticality 7): this is the ONLY fixture in this file where the PUBLISHED codec (AV1,
  // floor-admitted) differs from `preferredVideoCodec` (VP8, floor-rejected) — the one place that
  // can prove `pickScreenCodec` derives the AUTO bitrate from the codec it actually selected
  // (`layeringCodec`), not from the user's rejected preference. A mutant reverting
  // `pickScreenCodec`'s `this.calculateScreenBitrate(layeringCodec?.mimeType ?? null)` call back
  // to `this.calculateScreenBitrate(null)` survives every other case in this file — none of them
  // differ between published and preferred — and reads VP8's 0.07 bpp
  // (1920*1080*30*0.07 ≈ 4.35 Mbps → 4.4 Mbps rounded) instead of AV1's 0.04 bpp
  // (≈ 2.49 Mbps → 2.5 Mbps rounded).
  it('preference rejected by the codec floor falls through to the Auto ladder, deriving bitrate from the published codec', () => {
    svc.screenLayeringEnabled = true;
    useVoiceStore.setState({ codecFloor: ['video/av1'], activeScreenCodec: null }); // excludes VP8
    useVideoSettingsStore.setState({
      preferredVideoCodec: 'video/VP8',
      supportSimulcast: true,
      supportSvc: false,
      screenShareBitrate: 0, // auto → exercise calculateScreenBitrate()
      screenResolution: '1080p',
      screenFrameRate: 30,
    });

    const { codec, effectiveBitrate } = svc.pickScreenCodec();
    expect(codec?.mimeType?.toLowerCase()).toBe('video/av1');
    expect(effectiveBitrate).toBe(2_500_000);
  });
});

describe('voiceService.reemitScreenDemandOnConsume — re-emit demand for a swapped-in screen consumer (#1924 Fix)', () => {
  let svc: any;
  let emit: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.lastPreferredLayerKeyByConsumer?.clear();
    svc.remoteVideoRenderStateByUser?.clear();
    svc.remoteScreenRenderStateByUser?.clear();
    svc.remoteVideoPressureByUser?.clear();
    (globalThis as any).devicePixelRatio = 1;
    emit = vi.fn();
    svc.socket = { emit };
  });

  const seedScreenState = (userId: string, tileId: string) =>
    svc.setRemoteVideoRenderState(
      userId,
      tileId,
      { visible: true, cssWidth: 1920, cssHeight: 1080, role: 'focus', focusedWindow: true },
      'screen'
    );

  const splCalls = () => emit.mock.calls.filter((c: unknown[]) => c[0] === 'set-preferred-layers');

  it('re-emits the stored render-state demand for the NEW screen consumer of that user', () => {
    // A reproduce/codec-swap replaces the screen consumer while the render surface stays
    // mounted (so the reporter never re-fires). The persisted render-state must reach the
    // fresh consumer or it strands at spatial layer 0.
    seedScreenState('u1', 'tile-1'); // stored before the swap; no consumer yet → seed no-ops
    emit.mockClear();
    // The fresh consumer is recorded by the consume path.
    svc.consumerMeta.set('cons-screen-new', { source: 'screen', producerUserId: 'u1' });

    svc.reemitScreenDemandOnConsume('screen', 'u1');

    const spl = splCalls();
    expect(spl.length).toBe(1);
    expect((spl[0][1] as { consumerId: string }).consumerId).toBe('cons-screen-new');
    expect((spl[0][1] as { visible: boolean }).visible).toBe(true);
  });

  it('no-ops for a non-screen (camera) source', () => {
    seedScreenState('u1', 'tile-1');
    svc.consumerMeta.set('cons-screen-new', { source: 'screen', producerUserId: 'u1' });
    emit.mockClear();

    svc.reemitScreenDemandOnConsume('camera', 'u1');

    expect(splCalls().length).toBe(0);
  });

  it('no-ops when the user has no stored screen render-state', () => {
    svc.consumerMeta.set('cons-screen-new', { source: 'screen', producerUserId: 'u1' });
    emit.mockClear();

    svc.reemitScreenDemandOnConsume('screen', 'u1');

    expect(splCalls().length).toBe(0);
  });
});
