import React, { useRef, useEffect, useCallback, useMemo, useId } from 'react';
import {
  useVoiceStore,
  type ActiveScreenShare,
  MAX_TUNED_SCREEN_SHARES,
} from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useAudioSettingsStore } from '../../stores/audioSettingsStore';
import { useSettingsStore, type AppearanceSettings } from '../../stores/settingsStore';
import ParticipantTile from './ParticipantTile';
import ShareTunePill from './ShareTunePill';
import { VOICE_MAX_SCALE, useVoiceMagnification } from './useVoiceMagnification';
import { useGridLayout } from '../../hooks/useGridLayout';
import { useRenderStateReporter } from '../../hooks/useRenderStateReporter';
import { errorMessage } from '../../utils/redactError';
import './ParticipantGrid.css';

/** Base vertical px reserved below each grid slot for the Tune In/Out pill row. */
const PILL_SPACE_BASE = 24;

/** Ceiling (px) for avatar-only frames. Their avatar is a fixed circle, so a
 *  larger frame is just empty space around a small dot. In an all-avatar grid
 *  this is applied as useGridLayout's `maxTileWidth`. In a mixed grid the JS
 *  layout emits a single 16:9 tile size for every tile, so the per-tile CSS cap
 *  keyed off this value (emitted as `--avatar-frame-cap`) does the capping the
 *  uniform JS size cannot. Both paths share this one source of truth. */
const AVATAR_FRAME_CAP_PX = 320;

/** Discrete font-size buckets, mirrored from the `[data-fontsize]` rules in
 *  styles/index.css. The effective `--font-scale` the pill band tracks is this
 *  discrete value times the continuous `--ui-scale` (uiScale). Keeping this map
 *  in sync with that CSS lets us derive the band from settings-store state
 *  instead of a synchronous getComputedStyle read on every voice re-render. */
const FONT_SCALE_DISCRETE: Record<AppearanceSettings['fontSize'], number> = {
  small: 0.825,
  default: 1,
  large: 1.175,
};

/** The pill's height tracks --font-scale (accessibility font sizes); keep the
 *  reserved band in sync so a scaled pill is never clipped or overlapped by
 *  the magnified active-speaker frame. `fontScale` is the effective
 *  `--font-scale` (discrete bucket × uiScale) derived from the settings store,
 *  so this stays a pure calc off already-reactive state rather than reading
 *  computed styles inline during render. */
function pillSpacePx(fontScale: number): number {
  const scale = Number.isFinite(fontScale) && fontScale > 0 ? fontScale : 1;
  return Math.ceil(PILL_SPACE_BASE * scale);
}

/** Maximum boost gain: +18 dB ≈ 8x linear. Prevents extreme noise amplification. */
const MAX_BOOST_LINEAR = Math.pow(10, 18 / 20); // ~7.94

type SinkAudioElement = HTMLAudioElement & { setSinkId: (id: string) => Promise<void> };
type SinkResult = { ok: true } | { ok: false; err: unknown };

async function playAudioElement(el: HTMLAudioElement): Promise<void> {
  try {
    await el.play();
  } catch {
    // Playback rejection is non-fatal; the call audio graph stays intact.
  }
}

async function setSinkThenPlay(el: SinkAudioElement, sinkId: string): Promise<SinkResult> {
  try {
    await el.setSinkId(sinkId);
  } catch (err) {
    await playAudioElement(el);
    return { ok: false, err };
  }

  await playAudioElement(el);
  return { ok: true };
}

/**
 * Plays a remote participant's audio stream through a Web Audio GainNode
 * chain for output volume + quiet user boost. Renders nothing visible — the
 * internal <audio> element is created in the effect and never attached to
 * the DOM (see the effect body for why).
 *
 * Chain: <audio>.srcObject → createMediaElementSource → analyser → volumeGain
 *        → boostGain → ctx.destination
 *
 * `volumeGain` applies the combined master × per-participant volume. When
 * `userId` is provided the per-participant override (default 100) is mixed in;
 * otherwise only the master `outputVolume` applies.
 */
export const AudioOutput: React.FC<{
  stream: MediaStream;
  outputDeviceId?: string;
  userId?: string;
  volumeKind?: 'voice' | 'screen';
}> = ({ stream, outputDeviceId, userId, volumeKind = 'voice' }) => {
  const ctxRef = useRef<AudioContext | null>(null);
  const volumeGainRef = useRef<GainNode | null>(null);
  const boostGainRef = useRef<GainNode | null>(null);
  const analyserRef = useRef<AnalyserNode | null>(null);
  const boostTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const audioElRef = useRef<HTMLAudioElement | null>(null);
  // Output-device fallback path creates a separate <audio> element bound to a
  // MediaStreamDestination. Track it so we can pause + detach on unmount —
  // without this, the fallback element keeps playing the silent stream and
  // retains the AudioContext destination graph.
  const fallbackElRef = useRef<HTMLAudioElement | null>(null);
  const fallbackDestRef = useRef<MediaStreamAudioDestinationNode | null>(null);
  const appliedOutputDeviceRef = useRef(false);
  const appliedSinkIdRef = useRef<string | null>(null);
  const latestOutputDeviceIdRef = useRef(outputDeviceId);
  latestOutputDeviceIdRef.current = outputDeviceId;

  const outputVolume = useAudioSettingsStore((s) => s.outputVolume);
  const quietBoost = useAudioSettingsStore((s) => s.quietBoost);
  const quietBoostThreshold = useAudioSettingsStore((s) => s.quietBoostThreshold);
  const participantVolume = useAudioSettingsStore((s) => {
    if (!userId) return 100;
    const map = volumeKind === 'screen' ? s.perScreenShareVolume : s.perParticipantVolume;
    return map[userId] ?? 100;
  });

  const retargetOutputDevice = useCallback((selectedOutputDeviceId?: string) => {
    const ctx = ctxRef.current;
    const boostGain = boostGainRef.current;
    const el = audioElRef.current;
    if (!ctx || !boostGain || ctx.state === 'closed') return;
    if (!selectedOutputDeviceId && !appliedOutputDeviceRef.current) return;

    const sinkId = selectedOutputDeviceId ?? '';
    if (appliedOutputDeviceRef.current && appliedSinkIdRef.current === sinkId) return;
    appliedOutputDeviceRef.current = true;
    appliedSinkIdRef.current = sinkId;
    const resetAppliedSinkOnFailure = () => {
      if (appliedSinkIdRef.current !== sinkId) return;
      appliedOutputDeviceRef.current = false;
      appliedSinkIdRef.current = null;
    };

    const retargetViaAudioElement = () => {
      if (!el || !('setSinkId' in el)) return false;
      if (!selectedOutputDeviceId && !fallbackElRef.current) return false;
      if (!fallbackElRef.current) {
        boostGain.disconnect(ctx.destination);
        const fallbackDest = ctx.createMediaStreamDestination();
        fallbackDestRef.current = fallbackDest;
        boostGain.connect(fallbackDest);
        const fallbackEl = document.createElement('audio');
        fallbackEl.srcObject = fallbackDest.stream;
        fallbackElRef.current = fallbackEl;
      }

      const fallbackEl = fallbackElRef.current as SinkAudioElement;
      void setSinkThenPlay(fallbackEl, sinkId).then((result) => {
        if (!result.ok) {
          // Sink selection failed; keep call audio audible on the platform default.
          console.warn('Failed to set fallback audio output device:', errorMessage(result.err));
          resetAppliedSinkOnFailure();
        }
      });
      return true;
    };

    if (retargetViaAudioElement()) return;

    if (!('setSinkId' in ctx)) {
      resetAppliedSinkOnFailure();
      return;
    }

    (ctx as AudioContext & { setSinkId: (id: string) => Promise<void> })
      .setSinkId(sinkId)
      .catch((err) => {
        console.warn('Failed to set audio output device:', errorMessage(err));
        resetAppliedSinkOnFailure();
      });
  }, []);

  // Set up the Web Audio processing chain.
  //
  // Chromium 135+ broke createMediaStreamSource for WebRTC consumer tracks
  // with encodedInsertableStreams — the source node produces silence even though
  // the track is live. Raw <audio>.srcObject playback still works (#295).
  //
  // Fix: let the <audio> element play the stream, then capture its output via
  // createMediaElementSource. This routes the element's decoded audio through
  // the Web Audio chain (volume, boost, analysis) → ctx.destination.
  //
  // Important: we create the <audio> element INSIDE the effect rather than
  // reusing a ref'd JSX element. `createMediaElementSource` can only be called
  // once per HTMLMediaElement — even after the old AudioContext is closed,
  // the element stays bound to the orphaned source node. React Strict Mode's
  // double-mount (mount → cleanup → remount) would otherwise throw on the
  // second invocation. Creating a fresh element per mount avoids this.
  useEffect(() => {
    const el = document.createElement('audio');
    audioElRef.current = el;

    // Play the consumer stream through the <audio> element first
    el.srcObject = stream;
    el.play().catch((err) => {
      console.warn('Audio element play() rejected:', errorMessage(err));
    });

    const ctx = new AudioContext({ sampleRate: 48000 });
    ctxRef.current = ctx;

    if (ctx.state === 'suspended') {
      ctx.resume().catch((err) => {
        console.warn('AudioContext resume failed:', errorMessage(err));
      });
    }

    // Capture the audio element's output into the Web Audio graph.
    // createMediaElementSource redirects playback through the graph —
    // the element itself goes silent, audio comes out of ctx.destination.
    const source = ctx.createMediaElementSource(el);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 256;
    analyser.smoothingTimeConstant = 0.3;
    const boostGain = ctx.createGain();
    boostGain.gain.value = 1;
    const volumeGain = ctx.createGain();
    {
      const state = useAudioSettingsStore.getState();
      const master = state.outputVolume / 100;
      const volumeMap =
        volumeKind === 'screen' ? state.perScreenShareVolume : state.perParticipantVolume;
      const perParticipant = userId ? (volumeMap[userId] ?? 100) / 100 : 1;
      volumeGain.gain.value = master * perParticipant;
    }

    source.connect(analyser);
    analyser.connect(volumeGain);
    volumeGain.connect(boostGain);
    boostGain.connect(ctx.destination);

    analyserRef.current = analyser;
    boostGainRef.current = boostGain;
    volumeGainRef.current = volumeGain;
    retargetOutputDevice(latestOutputDeviceIdRef.current);

    console.debug('[AudioOutput] setup', {
      ctxState: ctx.state,
      streamActive: stream.active,
      trackCount: stream.getAudioTracks().length,
      trackState: stream.getAudioTracks()[0]?.readyState,
      trackEnabled: stream.getAudioTracks()[0]?.enabled,
      outputVolume: useAudioSettingsStore.getState().outputVolume,
    });

    return () => {
      if (boostTimerRef.current) {
        clearInterval(boostTimerRef.current);
        boostTimerRef.current = null;
      }
      // Stop playback and unbind the stream so the element can be GC'd.
      el.pause();
      el.srcObject = null;
      audioElRef.current = null;
      if (fallbackElRef.current) {
        fallbackElRef.current.pause();
        fallbackElRef.current.srcObject = null;
        fallbackElRef.current = null;
      }
      fallbackDestRef.current = null;
      appliedOutputDeviceRef.current = false;
      appliedSinkIdRef.current = null;
      if (ctx.state !== 'closed') ctx.close().catch(() => {});
      ctxRef.current = null;
      volumeGainRef.current = null;
      boostGainRef.current = null;
      analyserRef.current = null;
    };
    // `userId` is stable for a given AudioOutput instance (the parent keys by
    // userId), but ESLint needs it listed since we read it during setup.
  }, [stream, userId, volumeKind, retargetOutputDevice]);

  // Retarget the active output device without rebuilding the audio graph.
  useEffect(() => {
    retargetOutputDevice(outputDeviceId);
  }, [outputDeviceId, retargetOutputDevice]);

  // Update output volume in real-time. Applies master × per-participant.
  useEffect(() => {
    if (volumeGainRef.current && ctxRef.current && ctxRef.current.state !== 'closed') {
      const combined = (outputVolume / 100) * (participantVolume / 100);
      volumeGainRef.current.gain.setTargetAtTime(combined, ctxRef.current.currentTime, 0.01);
    }
  }, [outputVolume, participantVolume]);

  // Quiet user boost: dynamic gain based on audio level
  useEffect(() => {
    // Clear any previous boost polling
    if (boostTimerRef.current) {
      clearInterval(boostTimerRef.current);
      boostTimerRef.current = null;
    }

    const ctx = ctxRef.current;
    const analyser = analyserRef.current;
    const boostGain = boostGainRef.current;

    if (!quietBoost || !ctx || !analyser || !boostGain || ctx.state === 'closed') {
      // Boost disabled — reset gain to unity
      if (boostGain && ctx && ctx.state !== 'closed') {
        boostGain.gain.setTargetAtTime(1, ctx.currentTime, 0.05);
      }
      return;
    }

    // Convert dBFS threshold to linear amplitude (byte-range 0–128 offset from silence)
    const thresholdLinear = Math.pow(10, quietBoostThreshold / 20); // 0..1 linear
    const dataArray = new Uint8Array(analyser.frequencyBinCount);

    boostTimerRef.current = setInterval(() => {
      if (
        !analyserRef.current ||
        !boostGainRef.current ||
        !ctxRef.current ||
        ctxRef.current.state === 'closed'
      )
        return;

      analyserRef.current.getByteFrequencyData(dataArray);

      // Average level across frequency bins (0–255 range)
      let sum = 0;
      for (const val of dataArray) sum += val;
      const avgByte = sum / dataArray.length;

      // Convert byte average to linear amplitude (0–1 range, where 255 = 1.0)
      const measuredLinear = avgByte / 255;

      // Silence guard: don't boost below -60 dBFS (just noise)
      const silenceFloor = Math.pow(10, -60 / 20); // ~0.001
      if (measuredLinear <= silenceFloor) {
        // Silence — release boost slowly
        boostGainRef.current.gain.setTargetAtTime(1, ctxRef.current.currentTime, 0.3);
        return;
      }

      if (measuredLinear < thresholdLinear) {
        // Below threshold — calculate proportional gain to bring up to threshold
        const multiplier = Math.min(thresholdLinear / measuredLinear, MAX_BOOST_LINEAR);
        boostGainRef.current.gain.setTargetAtTime(multiplier, ctxRef.current.currentTime, 0.08); // 80ms attack
      } else {
        // At or above threshold — no boost needed
        boostGainRef.current.gain.setTargetAtTime(1, ctxRef.current.currentTime, 0.3); // 300ms release
      }
    }, 20); // 50 Hz poll

    return () => {
      if (boostTimerRef.current) {
        clearInterval(boostTimerRef.current);
        boostTimerRef.current = null;
      }
    };
  }, [quietBoost, quietBoostThreshold]);

  // The <audio> element is created inside the effect — nothing to render here.
  return null;
};

/**
 * Audio outputs for all remote participants. Each `<AudioOutput>` returns
 * `null` — its `<audio>` element lives in the effect closure, not in the
 * rendered DOM. Separated so the audio graph can be managed independently
 * of the visual layout.
 */
export const AudioOutputs: React.FC = () => {
  const participants = useVoiceStore((s) => s.participants);
  const localUserId = useUserStore((s) => s.user?.id);
  const audioOutputDeviceId = useVoiceStore((s) => s.audioOutputDeviceId);
  const participantList = Object.values(participants);

  return (
    <>
      {participantList.flatMap((p) => {
        if (p.userId === localUserId || !p.audioStream) return [];
        return [
          <AudioOutput
            key={`audio-${p.userId}`}
            stream={p.audioStream}
            outputDeviceId={audioOutputDeviceId || undefined}
            userId={p.userId}
          />,
        ];
      })}
      {participantList.flatMap((p) => {
        if (p.userId === localUserId || !p.screenAudioStream) return [];
        return [
          <AudioOutput
            key={`screen-audio-${p.userId}`}
            stream={p.screenAudioStream}
            outputDeviceId={audioOutputDeviceId || undefined}
            userId={p.userId}
            volumeKind="screen"
          />,
        ];
      })}
    </>
  );
};

/**
 * Screen-share tile for the Tile view: a tuned-in stream rendered as a grid
 * sibling of the user frames (Discord-style). Clicking it switches to the
 * Front 'n Center view focused on that stream.
 */
const StreamGridTile: React.FC<{
  producerId: string;
  name: string;
  stream?: MediaStream;
  /** Local share detached by Auto-Pause — show the placeholder, not a blank tile. */
  isPaused?: boolean;
  /**
   * Producing user of a REMOTE screen share this tile renders — set only for a remote
   * share so this Tile-view surface feeds receiver screen layer demand (#1924). A screen
   * watched ONLY in Tile view otherwise reports zero demand and stays pinned at spatial
   * layer 0 (screen consumers seed at layer 0 and ramp up on reported render-size demand).
   * Undefined for a local share (we never consume our own screen).
   */
  sharerUserId?: string;
  onFocus: (producerId: string) => void;
}> = ({ producerId, name, stream, isPaused = false, sharerUserId, onFocus }) => {
  const videoRef = useRef<HTMLVideoElement>(null);
  const tileId = useId();

  // #1924: report this Tile-view cell's rendered size/visibility for the remote screen so
  // the SFU can forward its smallest-sufficient layer and flip the screen-layering gate.
  // role 'grid' — it is a grid sibling of the participant frames. Inert for a local share.
  useRenderStateReporter({
    userId: sharerUserId ?? '',
    tileId,
    source: 'screen',
    elementRef: videoRef,
    role: 'grid',
    enabled: !!sharerUserId && !!stream && !isPaused,
  });

  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    if (stream) {
      el.srcObject = stream;
      el.play().catch(() => {});
    } else {
      el.srcObject = null;
    }
    return () => {
      el.srcObject = null;
    };
  }, [stream]);

  return (
    <button
      type="button"
      className="stream-grid-tile"
      onClick={() => onFocus(producerId)}
      title="Show front 'n center"
      aria-label={`Focus ${name}'s screen`}
    >
      {isPaused ? (
        <div className="stream-grid-tile__paused">
          <span className="stream-grid-tile__paused-title">Your Screen Is Still Streaming</span>
        </div>
      ) : (
        <video ref={videoRef} className="stream-grid-tile__video" autoPlay playsInline muted />
      )}
      <span className="stream-grid-tile__name">{`${name}’s screen`}</span>
    </button>
  );
};

interface UserFrameGridProps {
  /** Tile view: render tuned-in screen shares as grid tiles alongside frames. */
  includeStreamTiles?: boolean;
}

/**
 * Mode A layout: centered user frames grid with no scrollbars.
 * Tiles scale dynamically to fit the viewport.
 *
 * With `includeStreamTiles` (the Tile view while streams are tuned in),
 * tuned-in screen shares render as sibling 16:9 tiles ahead of the frames.
 * Participants producing a stream get a Tune In/Out pill below their frame.
 */
export const UserFrameGrid: React.FC<UserFrameGridProps> = ({ includeStreamTiles = false }) => {
  const participants = useVoiceStore((s) => s.participants);
  const localUserId = useUserStore((s) => s.user?.id);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);
  const setDominantScreenShare = useVoiceStore((s) => s.setDominantScreenShare);
  const setVoiceViewMode = useVoiceStore((s) => s.setVoiceViewMode);
  const setStageLayout = useVoiceStore((s) => s.setStageLayout);
  const localStreamPaused = useVoiceStore((s) => s.localStreamPaused);
  // Effective --font-scale inputs (discrete bucket × continuous uiScale). Read
  // from the store so the pill band recomputes only when the accessibility font
  // setting actually changes, not on every speaking-state toggle.
  const fontSize = useSettingsStore((s) => s.appearance.fontSize);
  const uiScale = useSettingsStore((s) => s.appearance.uiScale);
  const participantList = Object.values(participants);
  const scales = useVoiceMagnification(participants);
  // Active-speaker dominance (#1040): only meaningful with >1 tile (a lone tile
  // glowing looks broken), and inactive-desaturation only kicks in while someone
  // is actually speaking (so a silent room does not dim every tile).
  const dominanceActive = participantList.length > 1;
  const anySpeaking = participantList.some((p) => p.isSpeaking);
  const gridRef = useRef<HTMLDivElement>(null);

  // Tile view: one tile per tuned-in share, owner resolved via the #2088 seam.
  const streamTiles = includeStreamTiles
    ? Object.keys(tunedInScreenShares).map((producerId) => {
        const meta = activeScreenShares[producerId];
        const isLocal = meta?.isLocal ?? false;
        const stream = meta ? participants[meta.userId]?.screenStream : undefined;
        const paused = isLocal && localStreamPaused;
        return {
          producerId,
          name: meta?.displayName || meta?.username || 'Unknown',
          // Remote sharer's userId feeds this tile's screen layer demand (#1924);
          // undefined for a local share (we never consume our own screen).
          sharerUserId: !isLocal && meta?.userId ? meta.userId : undefined,
          // Honor Auto-Pause for the local preview: when the window blurs,
          // VoiceView sets localStreamPaused and the stage/stream-bar paths drop
          // the local stream. Do the same here so the Tile view does not keep the
          // local capture attached and rendering while unfocused.
          stream: paused ? undefined : stream,
          paused,
        };
      })
    : [];

  // First announced remote share per producer user → Tune In/Out pill below
  // their frame (relocated from the retired ScreenShareControls dock).
  const shareByUser: Record<string, ActiveScreenShare> = {};
  for (const share of Object.values(activeScreenShares)) {
    if (!share.isLocal && !(share.userId in shareByUser)) shareByUser[share.userId] = share;
  }
  const atCap = Object.keys(tunedInScreenShares).length >= MAX_TUNED_SCREEN_SHARES;
  const hasPills = participantList.some((p) => shareByUser[p.userId]);
  const pillSpace = useMemo(
    () => (hasPills ? pillSpacePx((FONT_SCALE_DISCRETE[fontSize] ?? 1) * uiScale) : 0),
    [hasPills, fontSize, uiScale]
  );

  const hasAnyVideo = participantList.some((p) => p.isVideoOn) || streamTiles.length > 0;
  const reservedScale = VOICE_MAX_SCALE;
  const { tileWidth, tileHeight } = useGridLayout(
    gridRef,
    participantList.length + streamTiles.length,
    {
      aspectRatio: hasAnyVideo ? 16 / 9 : 1,
      // Video/screen tiles fill the available voice-area space — computeGridLayout
      // still bounds them by the container, so "uncapped" means "as big as fits."
      // Avatar-only frames keep the cap: their avatar is a fixed 56px circle, so an
      // oversized frame is just a huge empty rounded rect around a tiny dot. In a
      // MIXED grid (hasAnyVideo) useGridLayout emits ONE uniform 16:9 size for every
      // tile, so this JS cap can't single out avatar frames — the per-tile CSS cap
      // (--avatar-frame-cap, ParticipantGrid.css) caps them there instead.
      // ponytail: AVATAR_FRAME_CAP_PX is the ceiling; drop it (and the CSS rule) if
      // avatar frames should grow too.
      maxTileWidth: hasAnyVideo ? undefined : AVATAR_FRAME_CAP_PX,
      scale: reservedScale,
      extraTileHeight: pillSpace,
    }
  );
  const tileSlotWidth = tileWidth * reservedScale;
  const tileSlotHeight = tileHeight * reservedScale;

  const handleFocusStream = useCallback(
    (producerId: string) => {
      setDominantScreenShare(producerId);
      // The dominant id is only honored by the stage's 'focus' sub-layout —
      // a persisted 'equal' preference would silently ignore the click.
      setStageLayout('focus');
      setVoiceViewMode('front-center');
    },
    [setDominantScreenShare, setStageLayout, setVoiceViewMode]
  );

  return (
    <div
      ref={gridRef}
      className="user-frame-grid"
      style={
        {
          '--tile-slot-w': `${tileSlotWidth}px`,
          '--tile-slot-h': `${tileSlotHeight}px`,
          '--tile-w': `${tileWidth}px`,
          '--tile-h': `${tileHeight}px`,
          '--pill-space': `${pillSpace}px`,
          '--avatar-frame-cap': `${AVATAR_FRAME_CAP_PX}px`,
        } as React.CSSProperties
      }
    >
      {streamTiles.map((tile) => (
        <div key={`stream-${tile.producerId}`} className="user-frame-grid__slot">
          <StreamGridTile
            producerId={tile.producerId}
            name={tile.name}
            stream={tile.stream}
            isPaused={tile.paused}
            sharerUserId={tile.sharerUserId}
            onFocus={handleFocusStream}
          />
        </div>
      ))}
      {participantList.map((p) => {
        const scale = scales[p.userId] ?? 1;
        const share = shareByUser[p.userId];
        return (
          <div key={p.userId} className="user-frame-grid__slot">
            <ParticipantTile
              participant={p}
              isLocal={p.userId === localUserId}
              magnificationScale={scale}
              activeSpeaker={dominanceActive && p.isSpeaking}
              dimmed={dominanceActive && anySpeaking && !p.isSpeaking}
            />
            {share && (
              <ShareTunePill
                share={share}
                tunedIn={share.producerId in tunedInScreenShares}
                atCap={atCap}
              />
            )}
          </div>
        );
      })}
    </div>
  );
};

/**
 * ParticipantGrid — orchestrator for voice visual layout.
 * Renders AudioOutputs + the appropriate visual layout mode.
 * Will be replaced by VoiceView integration in Phase 4.
 */
const ParticipantGrid: React.FC = () => {
  return (
    <>
      <AudioOutputs />
      <UserFrameGrid />
    </>
  );
};

export default ParticipantGrid;
