/**
 * #1924 simulcast-screenshare quality fixes (client-side).
 *
 * Fix #5 — `removeRemoteVideoTile` clears screen demand on the LAST-surface unmount by
 *   emitting an explicit `visible:false` set-preferred-layers, instead of dropping local
 *   state silently (which pinned the layer/gate on the last visible demand). Camera is
 *   unchanged — hidden-ness routes through the pause coordinator, not a demand emit.
 *
 * Fix #6a — `pickScreenCodec` picks from the SVC-first ladder (`pickScreenLayeringCodec`)
 *   so an AV1 pick that is SVC-ineligible (Support SVC off) descends past AV1/VP9 to
 *   H264/VP8 → real simulcast, instead of collapsing to a single stream. Simulcast stays
 *   server-gated on `screenLayeringEnabled`.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { voiceService } from '@/renderer/services/voiceService';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

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

describe('voiceService.pickScreenCodec — SVC-first ladder for screen simulcast (#1924 Fix #6a)', () => {
  let svc: any;

  beforeEach(() => {
    svc = voiceService as any;
    // A minimal send capability set: AV1 (SVC-kind) + VP8 (simulcast-kind).
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

  it('AV1 preferred + Support SVC OFF + Simulcast ON + gate ON → descends to a real VP8 simulcast plan', () => {
    svc.screenLayeringEnabled = true;
    useVideoSettingsStore.setState({ supportSvc: false, supportSimulcast: true });

    const { codec, encodings } = svc.pickScreenCodec();
    // AV1/VP9 are SVC-ineligible (Support SVC off) so the ladder skips them → VP8.
    expect(codec?.mimeType?.toLowerCase()).toBe('video/vp8');
    expect(encodings).toHaveLength(3);
    expect(encodings.map((e: { rid?: string }) => e.rid)).toEqual(['q', 'h', 'f']);
  });

  it('AV1 preferred + Support SVC ON → stays AV1 SVC (single encoding with a scalabilityMode)', () => {
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

    const { encodings } = svc.pickScreenCodec();
    // Simulcast is server-gated; with the gate off there is no layering fallback.
    expect(encodings).toHaveLength(1);
    expect(encodings[0].rid).toBeUndefined();
  });

  it('AUTO bitrate is derived from the ladder-chosen codec (VP8), not the AV1 preference (#1924 review)', () => {
    // AV1 preferred but SVC-ineligible → the ladder publishes VP8. calculateScreenBitrate
    // is codec-dependent (efficient codecs use 0.04 bits/px, VP8/H264 use 0.07). Computing
    // it BEFORE the AV1→VP8 swap under-budgeted the VP8 encode; the fix computes it after.
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
    expect(codec?.mimeType?.toLowerCase()).toBe('video/vp8');
    // VP8 (0.07 bpp): 1920*1080*30*0.07 ≈ 4.35 Mbps → rounded to 4.4 Mbps.
    expect(effectiveBitrate).toBe(4_400_000);
    // The AV1 (0.04 bpp) value would have been 2.5 Mbps — proving the bitrate follows the
    // ACTUAL published codec, not the preference.
    expect(effectiveBitrate).not.toBe(2_500_000);
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
    expect(codec?.mimeType?.toLowerCase()).toBe('video/vp8');
    expect(effectiveBitrate).toBe(3_333_000);
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
