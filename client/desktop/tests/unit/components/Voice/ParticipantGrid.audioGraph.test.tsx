/**
 * Regression: the receive audio graph was fed from `createMediaElementSource`
 * on an element whose source is a MediaStream via `srcObject`. MEASURED on
 * Electron 44 / Chromium 152: that capture yields SILENCE, and the element
 * keeps playing at full volume — so every node downstream was inert.
 *
 * Five shipped features rode those dead nodes: per-participant volume, master
 * output volume, quiet boost, the analyser that decides when to boost, and
 * output-device selection (its sink was set on nodes fed by the dead capture,
 * while the primary element was never muted and never re-sinked).
 *
 * These tests assert the CONTRACT the graph must satisfy — "exactly one
 * audible path, and the volume controls are on it" — rather than the shape of
 * our own output. The previous tests asserted `createMediaElementSource` was
 * called, which is precisely the call that made the feature inert.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, act } from '@testing-library/react';
import { FORCE_LEGACY_E2EE_KEY } from '@/renderer/services/e2ee/encodedTransformSupport';

const mockGainNode = () => ({
  gain: { value: 1, setTargetAtTime: vi.fn() },
  connect: vi.fn(),
  disconnect: vi.fn(),
});

const mockAnalyser = {
  fftSize: 256,
  smoothingTimeConstant: 0.3,
  frequencyBinCount: 128,
  connect: vi.fn(),
  getByteFrequencyData: vi.fn(),
};

let gains: ReturnType<typeof mockGainNode>[] = [];
let ctx: Record<string, unknown>;
let createdAudioEls: HTMLAudioElement[] = [];
let playSpy: ReturnType<typeof vi.spyOn>;

function makeCtx() {
  gains = [];
  return {
    state: 'running',
    currentTime: 0,
    destination: {},
    sampleRate: 48000,
    createAnalyser: vi.fn(() => mockAnalyser),
    createGain: vi.fn(() => {
      const g = mockGainNode();
      gains.push(g);
      return g;
    }),
    // BOTH are present. If only the one production currently calls existed, a
    // switch to the other would throw a TypeError — which reads exactly like a
    // successful falsification and is not one.
    createMediaElementSource: vi.fn(() => ({ connect: vi.fn() })),
    createMediaStreamSource: vi.fn(() => ({ connect: vi.fn() })),
    createMediaStreamDestination: vi.fn(() => ({ stream: {} })),
    resume: vi.fn().mockResolvedValue(undefined),
    close: vi.fn().mockResolvedValue(undefined),
    setSinkId: vi.fn().mockResolvedValue(undefined),
  };
}

function makeStream(id: string): MediaStream {
  const track = { readyState: 'live', enabled: true, kind: 'audio', stop: vi.fn() };
  return {
    id,
    active: true,
    getAudioTracks: () => [track],
    getTracks: () => [track],
  } as unknown as MediaStream;
}

/** Selects the modern RTCRtpScriptTransform path (no encodedInsertableStreams). */
function useScriptTransformPath() {
  vi.stubGlobal('RTCRtpScriptTransform', function RTCRtpScriptTransformStub() {});
  globalThis.localStorage?.removeItem(FORCE_LEGACY_E2EE_KEY);
  globalThis.sessionStorage?.removeItem('concord.e2eeLegacyFallback');
}

/** Selects the legacy createEncodedStreams path, where #295's precondition holds. */
function useLegacyPath() {
  vi.stubGlobal('RTCRtpScriptTransform', function RTCRtpScriptTransformStub() {});
  (RTCRtpSender.prototype as unknown as { createEncodedStreams?: unknown }).createEncodedStreams =
    () => ({});
  globalThis.localStorage?.setItem(FORCE_LEGACY_E2EE_KEY, '1');
}

import { AudioOutput } from '@/renderer/components/Voice/ParticipantGrid';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';

beforeEach(() => {
  createdAudioEls = [];
  ctx = makeCtx();
  vi.stubGlobal(
    'AudioContext',
    vi.fn(function MockAudioContextCtor() {
      return ctx;
    })
  );
  playSpy = vi.spyOn(HTMLAudioElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLAudioElement.prototype, 'pause').mockImplementation(() => {});

  // The element is created inside the effect and never appended to the DOM,
  // so there is no query that can reach it. Capture it at construction.
  const realCreate = document.createElement.bind(document);
  vi.spyOn(document, 'createElement').mockImplementation(((tag: string, opts?: unknown) => {
    const el = realCreate(tag as 'audio', opts as ElementCreationOptions);
    if (tag === 'audio') createdAudioEls.push(el as HTMLAudioElement);
    return el;
  }) as typeof document.createElement);

  if (!('RTCRtpSender' in globalThis)) {
    vi.stubGlobal('RTCRtpSender', function RTCRtpSenderStub() {});
  }
  useAudioSettingsStore.setState({ outputVolume: 100, perParticipantVolume: {} });
});

afterEach(() => {
  cleanup();
  delete (HTMLAudioElement.prototype as unknown as { setSinkId?: unknown }).setSinkId;
  // Order matters: `unstubAllGlobals` removes the RTCRtpSender stub, so
  // dereferencing it afterwards throws and the test goes red on TEARDOWN —
  // indistinguishable in the summary from a real assertion failure.
  if (typeof RTCRtpSender !== 'undefined') {
    delete (RTCRtpSender.prototype as unknown as { createEncodedStreams?: unknown })
      .createEncodedStreams;
  }
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  globalThis.localStorage?.clear();
  globalThis.sessionStorage?.clear();
});

/**
 * VACUITY GUARD. Every assertion below is about what happened to the receive
 * element and the graph. If the effect never ran, all of them pass trivially:
 * no element, no capture call, `muted` undefined. Assert the harness reached
 * the path FIRST, and fail loudly here rather than silently everywhere else.
 */
function elementUnderTest(): HTMLAudioElement {
  expect(createdAudioEls.length).toBeGreaterThan(0);
  const el = createdAudioEls[0];
  expect(el.srcObject).not.toBeNull();
  return el;
}

/**
 * The graph path's extra vacuity guard. It is NOT in `elementUnderTest`,
 * because an AudioContext is no longer built on the element path at all — it
 * would hold no nodes there and Chromium caps them per document. Asserting it
 * unconditionally made every legacy-path case fail for a reason unrelated to
 * what it was testing.
 */
function expectGraphContextBuilt(): void {
  expect(AudioContext).toHaveBeenCalledWith({ sampleRate: 48000 });
}

describe('receive audio graph — exactly one audible path (Chromium 152)', () => {
  it('feeds the graph from the STREAM, not the element, on the script-transform path', () => {
    useScriptTransformPath();
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    elementUnderTest();
    expectGraphContextBuilt();

    // The contract: the capture that actually carries audio is the stream one.
    expect(ctx.createMediaStreamSource).toHaveBeenCalledTimes(1);
    // And the one measured to capture silence is not used to carry audio.
    expect(ctx.createMediaElementSource).not.toHaveBeenCalled();
  });

  it('mutes the element when the graph owns the audio, leaving no second path', () => {
    useScriptTransformPath();
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    const el = elementUnderTest();
    expectGraphContextBuilt();

    // A remote track renders only while an element pulls it, so the element
    // must keep playing — MEASURED: a muted element still pulls (peak 0.9035).
    // It must not also be audible, or the gain nodes govern nothing.
    expect(el.muted).toBe(true);
    // Assert the CALL, not `el.paused`: jsdom's play() is a stub that never
    // flips `paused`, so asserting on it tests the mock rather than the code.
    expect(playSpy).toHaveBeenCalled();
  });

  it('puts volume 0 on the audible path — no route survives at full volume', () => {
    useScriptTransformPath();
    useAudioSettingsStore.setState({ outputVolume: 0, perParticipantVolume: { u1: 0 } });
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    const el = elementUnderTest();
    expectGraphContextBuilt();

    // Whichever path is audible must be at zero. The element is muted, and the
    // graph's volume node was constructed at zero.
    const volumeGain = gains[gains.length - 1];
    expect(el.muted || el.volume === 0).toBe(true);
    expect(volumeGain.gain.value).toBe(0);
  });

  it('sets the output sink on the PRIMARY element when the element carries the audio', async () => {
    // The branch this covers is the whole reason device selection was broken:
    // every other route sets the sink downstream of a capture that carries no
    // audio, so on the element-driven path the sink has to land on the element
    // itself. Asserting it is called is not enough — assert it is called on the
    // element that is actually audible, and that no fallback graph is built.
    const setSinkId = vi.fn().mockResolvedValue(undefined);
    (HTMLAudioElement.prototype as unknown as { setSinkId?: unknown }).setSinkId = setSinkId;
    useLegacyPath();

    render(<AudioOutput stream={makeStream('s1')} userId="u1" outputDeviceId="device-7" />);
    const el = elementUnderTest();

    await vi.waitFor(() => expect(setSinkId).toHaveBeenCalledWith('device-7'));
    expect(el.muted).toBe(false);
    // No MediaStreamDestination: the fallback element exists only to carry the
    // graph's output, and on this path there is no graph.
    expect(ctx.createMediaStreamDestination).not.toHaveBeenCalled();
    expect(ctx.setSinkId).not.toHaveBeenCalled();
  });

  it('gives up quietly on the legacy path when the element cannot take a sink', () => {
    // jsdom's HTMLAudioElement has no setSinkId, which is the real shape on any
    // engine that does not implement it. The branch must refuse rather than
    // throw, and must not fall through to the graph routes below it.
    delete (HTMLAudioElement.prototype as unknown as { setSinkId?: unknown }).setSinkId;
    useLegacyPath();

    expect(() =>
      render(<AudioOutput stream={makeStream('s1')} userId="u1" outputDeviceId="device-7" />)
    ).not.toThrow();
    elementUnderTest();
    expect(ctx.createMediaStreamDestination).not.toHaveBeenCalled();
    expect(ctx.setSinkId).not.toHaveBeenCalled();
  });

  it('drives the ELEMENT on the legacy path, where a stream source is unsafe (#295)', () => {
    useLegacyPath();
    useAudioSettingsStore.setState({ outputVolume: 50, perParticipantVolume: { u1: 50 } });
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    const el = elementUnderTest();

    // encodedInsertableStreams IS set on this path, which is exactly #295's
    // precondition — a stream source there can be silent, so it must not be
    // used. The element stays audible and carries the volume itself.
    expect(ctx.createMediaStreamSource).not.toHaveBeenCalled();
    expect(el.muted).toBe(false);
    expect(el.volume).toBeCloseTo(0.25, 5);
  });

  it('follows the TRANSPORT, not the resolver, when the two disagree', () => {
    // The TOCTOU this fork used to carry. `currentTransformPath()` re-reads two
    // storage overrides on every call; `encodedInsertableStreams` is fixed when
    // the transport is built. Clearing `concord.forceLegacyE2EE` mid-call moves
    // the first and not the second, and an AudioOutput mounting after that
    // muted its element AND built an inert stream source over an
    // insertable-streams peer connection — silent, nothing thrown.
    //
    // Resolver says modern; the transport says it was built legacy. The
    // transport wins, because it is the one #295 actually keys on.
    useScriptTransformPath();
    const fact = vi.spyOn(voiceService, 'recvTransportUsesInsertableStreams').mockReturnValue(true);

    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    const el = elementUnderTest();

    expect(fact).toHaveBeenCalledWith('audio'); // vacuity: it was actually asked
    expect(el.muted).toBe(false);
    expect(ctx.createMediaStreamSource).not.toHaveBeenCalled();
  });

  it('builds no AudioContext at all on the element-driven path', () => {
    // It used to build one unconditionally and then wire nothing into it on
    // this path. Chromium caps concurrent contexts per document and this
    // component mints one per remote participant plus one per screen-audio
    // stream, so an empty one is real pressure against that cap for nothing.
    useLegacyPath();
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    elementUnderTest();

    expect(AudioContext).not.toHaveBeenCalled();
  });

  it('applies a LATER volume change to the element on the legacy path', () => {
    // The construction-time value was already covered; this drives the volume
    // EFFECT's element branch, which nothing reached. Delete that branch and
    // the suite stayed green while "dragging the slider does nothing" returned
    // on exactly the path this PR was opened to repair.
    useLegacyPath();
    useAudioSettingsStore.setState({ outputVolume: 100, perParticipantVolume: { u1: 100 } });
    render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    const el = elementUnderTest();
    expect(el.volume).toBeCloseTo(1, 5); // precondition, not the assertion

    act(() => {
      useAudioSettingsStore.setState({ outputVolume: 20, perParticipantVolume: { u1: 50 } });
    });
    expect(el.volume).toBeCloseTo(0.1, 5);
  });

  it('keeps the quiet-boost poll alive across a stream swap', () => {
    // The setup effect is keyed on `stream` and its cleanup clears the boost
    // timer; the boost effect was keyed only on the two boost settings. A
    // stream-only change therefore killed the 50 Hz poll permanently — a
    // reconnect or codec re-produce is enough — and quiet boost, one of the
    // four features repaired here, silently stopped for the rest of the call.
    vi.useFakeTimers();
    try {
      useScriptTransformPath();
      useAudioSettingsStore.setState({ quietBoost: true, quietBoostThreshold: -30 });

      const { rerender } = render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
      expectGraphContextBuilt();

      act(() => void vi.advanceTimersByTime(100));
      const ticksBeforeSwap = mockAnalyser.getByteFrequencyData.mock.calls.length;
      expect(ticksBeforeSwap).toBeGreaterThan(0); // vacuity: the poll ran at all

      act(() => {
        rerender(<AudioOutput stream={makeStream('s2')} userId="u1" />);
      });
      act(() => void vi.advanceTimersByTime(100));

      expect(mockAnalyser.getByteFrequencyData.mock.calls.length).toBeGreaterThan(ticksBeforeSwap);
    } finally {
      vi.useRealTimers();
    }
  });

  it('ignores a resume() rejection from a graph that has already been replaced', async () => {
    // `ctx.resume()` rejects ASYNCHRONOUSLY, and the degrade helper captures its
    // own run's element while mutating the SHARED refs. Without a fence, a
    // rejection landing after a stream swap disconnects the SUCCESSOR's
    // boostGain, closes the successor's context and unmutes this run's detached
    // element — leaving the live element muted behind no graph at all. Silence,
    // produced by the guard that exists to prevent silence.
    useScriptTransformPath();
    const rejecters: Array<(e: unknown) => void> = [];
    ctx.state = 'suspended';
    (ctx.resume as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise((_resolve, reject) => rejecters.push(reject))
    );

    const { rerender } = render(<AudioOutput stream={makeStream('s1')} userId="u1" />);
    expect(createdAudioEls).toHaveLength(1);

    act(() => {
      rerender(<AudioOutput stream={makeStream('s2')} userId="u1" />);
    });
    // Vacuity: the swap must really have re-run setup, or there is no stale
    // closure for the rejection below to belong to.
    expect(createdAudioEls).toHaveLength(2);

    // Baseline AFTER the swap: the first run's cleanup has already closed its
    // context, so anything beyond this count is the stale callback's doing.
    const closesAfterSwap = (ctx.close as ReturnType<typeof vi.fn>).mock.calls.length;

    await act(async () => {
      rejecters[0]?.(new Error('late'));
      await Promise.resolve();
    });

    // `createdAudioEls[1].muted` is NOT the assertion, and that is the point:
    // the stale callback unmutes the OLD element, so the successor's stays
    // muted either way and the check cannot tell the two apart. What the
    // callback actually destroys is the SUCCESSOR's graph — so measure that.
    expect((ctx.close as ReturnType<typeof vi.fn>).mock.calls.length).toBe(closesAfterSwap);

    // And the successor's graph still governs volume: a change routes to the
    // gain node, not to the element.
    const volumeGain = gains[gains.length - 1];
    const writesBefore = volumeGain.gain.setTargetAtTime.mock.calls.length;
    act(() => {
      useAudioSettingsStore.setState({ outputVolume: 40, perParticipantVolume: { u1: 100 } });
    });
    expect(volumeGain.gain.setTargetAtTime.mock.calls.length).toBeGreaterThan(writesBefore);
  });

  it('hands the audio back to the element when the graph cannot be built', () => {
    // `createMediaStreamSource` throws InvalidStateError for a stream with no
    // audio track, and the AudioContext constructor throws at the per-document
    // cap. Before the guard, the element was ALREADY muted by then and the
    // throw escaped the effect body — zero audible paths plus a render crash,
    // which is strictly worse than the ungoverned single path we started from.
    useScriptTransformPath();
    (ctx.createMediaStreamSource as ReturnType<typeof vi.fn>).mockImplementation(() => {
      throw new DOMException('no audio track', 'InvalidStateError');
    });

    expect(() => render(<AudioOutput stream={makeStream('s1')} userId="u1" />)).not.toThrow();
    const el = elementUnderTest();

    // Exactly one audible path, and it is the element.
    expect(el.muted).toBe(false);
    expect(ctx.close).toHaveBeenCalled();
  });
});
