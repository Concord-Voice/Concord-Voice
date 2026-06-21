import React, { useRef, useEffect } from 'react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useAudioSettingsStore } from '../../stores/audioSettingsStore';
import ParticipantTile from './ParticipantTile';
import { useVoiceMagnification } from './useVoiceMagnification';
import { useGridLayout } from '../../hooks/useGridLayout';
import { errorMessage } from '../../utils/redactError';
import './ParticipantGrid.css';

/** Maximum boost gain: +18 dB ≈ 8x linear. Prevents extreme noise amplification. */
const MAX_BOOST_LINEAR = Math.pow(10, 18 / 20); // ~7.94

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
}> = ({ stream, outputDeviceId, userId }) => {
  const ctxRef = useRef<AudioContext | null>(null);
  const volumeGainRef = useRef<GainNode | null>(null);
  const boostGainRef = useRef<GainNode | null>(null);
  const analyserRef = useRef<AnalyserNode | null>(null);
  const boostTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Output-device fallback path creates a separate <audio> element bound to a
  // MediaStreamDestination. Track it so we can pause + detach on unmount —
  // without this, the fallback element keeps playing the silent stream and
  // retains the AudioContext destination graph.
  const fallbackElRef = useRef<HTMLAudioElement | null>(null);

  const outputVolume = useAudioSettingsStore((s) => s.outputVolume);
  const quietBoost = useAudioSettingsStore((s) => s.quietBoost);
  const quietBoostThreshold = useAudioSettingsStore((s) => s.quietBoostThreshold);
  const participantVolume = useAudioSettingsStore((s) =>
    userId ? (s.perParticipantVolume[userId] ?? 100) : 100
  );

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
      const perParticipant = userId ? (state.perParticipantVolume[userId] ?? 100) / 100 : 1;
      volumeGain.gain.value = master * perParticipant;
    }

    source.connect(analyser);
    analyser.connect(volumeGain);
    volumeGain.connect(boostGain);
    boostGain.connect(ctx.destination);

    analyserRef.current = analyser;
    boostGainRef.current = boostGain;
    volumeGainRef.current = volumeGain;

    // Set output device: prefer AudioContext.setSinkId (Chrome 110+),
    // fall back to routing through a MediaStreamDestination + <audio> with setSinkId
    if (outputDeviceId) {
      if ('setSinkId' in ctx) {
        (ctx as AudioContext & { setSinkId: (id: string) => Promise<void> })
          .setSinkId(outputDeviceId)
          .catch((err) => {
            console.warn('Failed to set audio output device:', errorMessage(err));
          });
      } else if ('setSinkId' in el) {
        // Fallback: disconnect from ctx.destination, route through a destination node
        // to a separate <audio> element that supports setSinkId
        boostGain.disconnect(ctx.destination);
        const fallbackDest = ctx.createMediaStreamDestination();
        boostGain.connect(fallbackDest);
        const fallbackEl = document.createElement('audio');
        fallbackEl.srcObject = fallbackDest.stream;
        (fallbackEl as HTMLAudioElement & { setSinkId: (id: string) => Promise<void> })
          .setSinkId(outputDeviceId)
          .catch((err) => {
            console.warn('Failed to set fallback audio output device:', errorMessage(err));
          });
        fallbackEl.play().catch(() => {});
        fallbackElRef.current = fallbackEl;
      }
    }

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
      if (fallbackElRef.current) {
        fallbackElRef.current.pause();
        fallbackElRef.current.srcObject = null;
        fallbackElRef.current = null;
      }
      if (ctx.state !== 'closed') ctx.close().catch(() => {});
      ctxRef.current = null;
      volumeGainRef.current = null;
      boostGainRef.current = null;
      analyserRef.current = null;
    };
    // `userId` is stable for a given AudioOutput instance (the parent keys by
    // userId), but ESLint needs it listed since we read it during setup.
  }, [stream, outputDeviceId, userId]);

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
          />,
        ];
      })}
    </>
  );
};

/**
 * Mode A layout: centered user frames grid with no scrollbars.
 * Tiles scale dynamically to fit the viewport.
 */
export const UserFrameGrid: React.FC = () => {
  const participants = useVoiceStore((s) => s.participants);
  const localUserId = useUserStore((s) => s.user?.id);
  const participantList = Object.values(participants);
  const scales = useVoiceMagnification(participants);
  const gridRef = useRef<HTMLDivElement>(null);
  const hasAnyVideo = participantList.some((p) => p.isVideoOn);
  const { tileWidth, tileHeight } = useGridLayout(gridRef, participantList.length, {
    aspectRatio: hasAnyVideo ? 16 / 9 : 1,
    maxTileWidth: 320,
  });

  return (
    <div
      ref={gridRef}
      className="user-frame-grid"
      style={
        {
          '--tile-w': `${tileWidth}px`,
          '--tile-h': `${tileHeight}px`,
        } as React.CSSProperties
      }
    >
      {participantList.map((p) => (
        <ParticipantTile
          key={p.userId}
          participant={p}
          isLocal={p.userId === localUserId}
          magnificationScale={scales[p.userId]}
        />
      ))}
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
