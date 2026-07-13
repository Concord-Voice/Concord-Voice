import React, { useEffect, useRef, useState, useCallback } from 'react';
import { useParams } from 'react-router-dom';
import { errorMessage } from '../../utils/redactError';
import {
  Pin,
  PinOff,
  X,
  Mic,
  MicOff,
  Volume2,
  VolumeX,
  Video,
  VideoOff,
  Monitor,
  MonitorOff,
  PhoneOff,
} from 'lucide-react';
import type { VoiceParticipant } from '../../stores/voiceStore';
import type { AnyPipBroadcast, VoiceStateResult } from '../../services/pipSignalingTypes';
import { PipVoiceClient } from '../../services/pipVoiceClient';
import ParticipantTile from './ParticipantTile';
import { useVoiceMagnification } from './useVoiceMagnification';
import './PipWindow.css';

/**
 * PiP window content. Three modes based on pipId prefix:
 * - "controls-*": Voice controls in a floating window
 * - "frames-*":   User Frames PiP — compact grid with real video/audio
 * - "screen-*":   Screen Share PiP — single screen share with real video
 */
const PipWindow: React.FC = () => {
  const { pipId } = useParams<{ pipId: string }>();
  const [pinned, setPinned] = useState(true);

  const isControlsPip = pipId?.startsWith('controls');
  const isFramesPip = pipId?.startsWith('frames');
  const isScreenPip = pipId?.startsWith('screen');

  // Toggle always-on-top
  const togglePin = useCallback(() => {
    const next = !pinned;
    setPinned(next);
    globalThis.electron?.setPipAlwaysOnTop(pipId || '', next);
  }, [pinned, pipId]);

  // Close this PiP window
  const handleClose = useCallback(() => {
    globalThis.electron?.closePipWindow(pipId || '');
  }, [pipId]);

  if (isControlsPip) {
    return (
      <ControlsPipContent
        pipId={pipId || ''}
        pinned={pinned}
        onTogglePin={togglePin}
        onClose={handleClose}
      />
    );
  }

  if (isFramesPip) {
    return (
      <FramesPipContent
        pipId={pipId || ''}
        pinned={pinned}
        onTogglePin={togglePin}
        onClose={handleClose}
      />
    );
  }

  if (isScreenPip) {
    return (
      <ScreenPipContent
        pipId={pipId || ''}
        pinned={pinned}
        onTogglePin={togglePin}
        onClose={handleClose}
      />
    );
  }

  return <div className="pip-window">Unknown PiP type</div>;
};

/** Delay native PiP destruction until the acknowledged media cleanup settles. */
function installPipCloseGuard(client: PipVoiceClient, pipId: string): () => void {
  let closeRequested = false;
  let allowClose = false;

  const finishClose = () => {
    allowClose = true;
    const closePromise = globalThis.electron?.closePipWindow(pipId);
    void closePromise?.catch(console.error);
  };

  const handleBeforeUnload = (event: BeforeUnloadEvent) => {
    if (allowClose) return;

    event.preventDefault();
    // Electron's renderer beforeunload veto still keys on the legacy returnValue signal.
    // Reflect invokes the native setter without reintroducing deprecated lib.dom property access.
    Reflect.set(event, 'returnValue', false);
    if (closeRequested) return;
    closeRequested = true;

    void client.dispose().then(finishClose, (err) => {
      console.error(err);
      finishClose();
    });
  };

  globalThis.addEventListener('beforeunload', handleBeforeUnload);
  return () => globalThis.removeEventListener('beforeunload', handleBeforeUnload);
}

// ── Controls PiP ────────────────────────────────────────────────

/**
 * Controls PiP — compact voice controls in a floating window.
 * Actions are proxied to the main window via PipVoiceClient.
 */
const ControlsPipContent: React.FC<{
  pipId: string;
  pinned: boolean;
  onTogglePin: () => void;
  onClose: () => void;
}> = ({ pipId, pinned, onTogglePin, onClose }) => {
  const clientRef = useRef<PipVoiceClient | null>(null);
  const localUserIdRef = useRef<string>('');
  const [state, setState] = useState<{
    isMuted: boolean;
    isDeafened: boolean;
    isVideoOn: boolean;
    isScreenSharing: boolean;
    participantCount: number;
  }>({
    isMuted: false,
    isDeafened: false,
    isVideoOn: false,
    isScreenSharing: false,
    participantCount: 0,
  });

  useEffect(() => {
    const client = new PipVoiceClient(pipId);
    const removeCloseGuard = installPipCloseGuard(client, pipId);
    clientRef.current = client;

    // Listen for state broadcasts to update button states
    client.onStateUpdate = (msg: AnyPipBroadcast) => {
      if (msg.type === 'state-update') {
        const participants = Object.values(msg.participants);
        // Update ref from broadcast in case init() hasn't resolved yet
        if (!localUserIdRef.current && msg.localUserId) {
          localUserIdRef.current = msg.localUserId;
        }
        const effectiveLocalUserId = localUserIdRef.current || msg.localUserId;
        const localP = participants.find((p) => p.userId === effectiveLocalUserId);
        if (localP) {
          setState({
            isMuted: localP.isMuted,
            isDeafened: localP.isDeafened || false,
            isVideoOn: localP.isVideoOn || false,
            isScreenSharing: localP.isScreenSharing || false,
            participantCount: participants.length,
          });
        } else {
          setState((prev) => ({
            ...prev,
            participantCount: participants.length,
          }));
        }
      } else if (msg.type === 'voice-ended') {
        onClose();
      }
    };

    // Request initial state (we don't need media, just participant info)
    client
      .init()
      .then((voiceState: VoiceStateResult) => {
        localUserIdRef.current = voiceState.localUserId;
        const participants = Object.values(voiceState.participants);
        const localP = participants.find((p) => p.userId === voiceState.localUserId);
        setState({
          isMuted: localP?.isMuted ?? false,
          isDeafened: localP?.isDeafened ?? false,
          isVideoOn: localP?.isVideoOn ?? false,
          isScreenSharing: localP?.isScreenSharing ?? false,
          participantCount: participants.length,
        });
      })
      .catch(console.error);

    return () => {
      removeCloseGuard();
      client.dispose().catch(console.error);
      clientRef.current = null;
    };
  }, [pipId, onClose]);

  const doAction = useCallback(
    (action: 'toggle-mute' | 'toggle-deafen' | 'leave' | 'toggle-video' | 'toggle-screen') => {
      clientRef.current?.action(action).catch(console.error);
    },
    []
  );

  return (
    <div className="pip-window pip-window--controls">
      <PipHeader pinned={pinned} onTogglePin={onTogglePin} onClose={onClose} />
      <div className="pip-controls-bar">
        <button
          className={`pip-control-btn ${state.isMuted ? 'pip-control-btn--danger' : ''}`}
          onClick={() => doAction('toggle-mute')}
          title={state.isMuted ? 'Unmute' : 'Mute'}
        >
          {state.isMuted ? <MicOff size={16} /> : <Mic size={16} />}
        </button>
        <button
          className={`pip-control-btn ${state.isDeafened ? 'pip-control-btn--danger' : ''}`}
          onClick={() => doAction('toggle-deafen')}
          title={state.isDeafened ? 'Undeafen' : 'Deafen'}
        >
          {state.isDeafened ? <VolumeX size={16} /> : <Volume2 size={16} />}
        </button>
        <button
          className={`pip-control-btn ${state.isVideoOn ? 'pip-control-btn--active' : ''}`}
          onClick={() => doAction('toggle-video')}
          title={state.isVideoOn ? 'Stop Video' : 'Start Video'}
        >
          {state.isVideoOn ? <Video size={16} /> : <VideoOff size={16} />}
        </button>
        <button
          className={`pip-control-btn ${state.isScreenSharing ? 'pip-control-btn--active' : ''}`}
          onClick={() => doAction('toggle-screen')}
          title={state.isScreenSharing ? 'Stop Sharing' : 'Share Screen'}
        >
          {state.isScreenSharing ? <Monitor size={16} /> : <MonitorOff size={16} />}
        </button>
        <button
          className="pip-control-btn pip-control-btn--leave"
          onClick={() => doAction('leave')}
          title="Leave Voice"
        >
          <PhoneOff size={16} />
        </button>
      </div>
      {state.participantCount > 0 && (
        <div className="pip-controls-status">
          {state.participantCount} participant{state.participantCount === 1 ? '' : 's'}
        </div>
      )}
    </div>
  );
};

// ── Frames PiP ──────────────────────────────────────────────────

/**
 * User Frames PiP — all participants in a compact grid with real video.
 * Uses PipVoiceClient for independent mediasoup consumers.
 */
const FramesPipContent: React.FC<{
  pipId: string;
  pinned: boolean;
  onTogglePin: () => void;
  onClose: () => void;
}> = ({ pipId, pinned, onTogglePin, onClose }) => {
  const [participants, setParticipants] = useState<Record<string, VoiceParticipant>>({});
  const [loading, setLoading] = useState(true);
  const clientRef = useRef<PipVoiceClient | null>(null);
  /** Serial consume queue — mediasoup requires sequential SDP negotiations */
  const consumeQueueRef = useRef<Promise<void>>(Promise.resolve());

  /** Consume a producer and attach the resulting stream to participant state. */
  const consumeAndAttach = useCallback(
    async (client: PipVoiceClient, producerId: string, source: string, userId: string) => {
      const stream = await client.consume(producerId, source, userId);
      if (!stream) return;
      setParticipants((prev) => {
        const p = prev[userId];
        if (!p) return prev;
        return {
          ...prev,
          [userId]: {
            ...p,
            ...(source === 'mic' ? { audioStream: stream } : { videoStream: stream }),
          },
        };
      });
    },
    []
  );

  useEffect(() => {
    const client = new PipVoiceClient(pipId);
    const removeCloseGuard = installPipCloseGuard(client, pipId);
    clientRef.current = client;
    consumeQueueRef.current = Promise.resolve();

    client.onStateUpdate = (msg: AnyPipBroadcast) => {
      if (msg.type === 'state-update') {
        setParticipants(msg.participants);
      } else if (msg.type === 'producer-added') {
        if (msg.source === 'mic' || msg.source === 'camera') {
          // Queue sequentially — concurrent consume calls break SDP negotiation
          consumeQueueRef.current = consumeQueueRef.current.then(() =>
            consumeAndAttach(client, msg.producerId, msg.source, msg.userId)
          );
        }
      } else if (msg.type === 'producer-closed') {
        setParticipants((prev) => {
          const p = prev[msg.userId];
          if (!p) return prev;
          return {
            ...prev,
            [msg.userId]: {
              ...p,
              audioStream: undefined,
              videoStream: undefined,
              screenStream: undefined,
            },
          };
        });
      } else if (msg.type === 'voice-ended') {
        onClose();
      }
    };

    const setup = async () => {
      try {
        const voiceState = await client.init();
        setParticipants(voiceState.participants);

        // Consume all active mic and camera producers
        for (const producer of voiceState.activeProducers) {
          if (producer.source === 'mic' || producer.source === 'camera') {
            await consumeAndAttach(client, producer.producerId, producer.source, producer.userId);
          }
        }

        // Signal ready — main window will pause its consumers
        await client.signalReady();
      } catch (err) {
        console.error('[FramesPip] Init failed:', errorMessage(err));
      } finally {
        setLoading(false);
      }
    };

    setup();

    return () => {
      removeCloseGuard();
      client.dispose().catch(console.error);
      clientRef.current = null;
    };
  }, [pipId, onClose, consumeAndAttach]);

  const scales = useVoiceMagnification(participants);
  const participantList = Object.values(participants);

  return (
    <div className="pip-window">
      <PipHeader pinned={pinned} onTogglePin={onTogglePin} onClose={onClose} />
      <div className="pip-frames-grid">
        {(() => {
          if (loading) return <div className="pip-empty">Connecting...</div>;
          if (participantList.length === 0) return <div className="pip-empty">No participants</div>;
          return participantList.map((p) => (
            <ParticipantTile
              key={p.userId}
              participant={p}
              compact
              magnificationScale={scales[p.userId]}
            />
          ));
        })()}
      </div>
    </div>
  );
};

// ── Screen PiP ──────────────────────────────────────────────────

/**
 * Report the PiP screen's receiver render-state demand to the SFU for the PiP's OWN
 * consumer (#1924). Measures the video element's layout box and skips a not-yet-laid-out
 * (0×0) element — the video is display:none until the stream mounts, so the ResizeObserver
 * re-fires this with a real size once it is displayed. role:'focus' + focusedWindow:true +
 * the large PiP size unsticks an H264/VP8 simulcast screen off spatial layer 0. Best-effort:
 * a failure only costs a lower spatial layer, never a hard error.
 */
function reportPipScreenDemand(
  client: PipVoiceClient,
  consumerId: string,
  el: HTMLVideoElement
): void {
  const rect = el.getBoundingClientRect();
  if (rect.width < 1 || rect.height < 1) return;
  client
    .reportPreferredLayers({
      consumerId,
      cssWidth: rect.width,
      cssHeight: rect.height,
      visible: true,
      role: 'focus',
      focusedWindow: true,
    })
    .catch((err) => console.error('[ScreenPip] demand report failed:', errorMessage(err)));
}

/** Trailing debounce for PiP resize-driven demand reports (#1924 review). A continuous
 *  drag-resize fires the ResizeObserver many times; coalesce the burst into one report. */
const PIP_SCREEN_DEMAND_DEBOUNCE_MS = 200;

/** Handle returned by observePipScreenDemand: disconnects the ResizeObserver AND clears
 *  any pending debounced report, so an unmount mid-drag never fires a late RPC (#1924). */
interface PipScreenDemandHandle {
  disconnect(): void;
}

/**
 * Seed an initial demand report and observe the video element so the report re-fires
 * once it lays out (display:none → block) and on later PiP resizes (#1924). Resize-driven
 * reports are debounced (#1924 review) so a continuous drag-resize coalesces into a single
 * trailing set-preferred-layers RPC over the BroadcastChannel instead of one per fire.
 * Returns a handle (or null where ResizeObserver is unavailable) for effect cleanup.
 */
function observePipScreenDemand(
  client: PipVoiceClient,
  consumerId: string,
  el: HTMLVideoElement
): PipScreenDemandHandle | null {
  reportPipScreenDemand(client, consumerId, el);
  if (typeof ResizeObserver === 'undefined') return null;
  let debounceTimer: ReturnType<typeof setTimeout> | null = null;
  const observer = new ResizeObserver(() => {
    if (debounceTimer !== null) clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => {
      debounceTimer = null;
      reportPipScreenDemand(client, consumerId, el);
    }, PIP_SCREEN_DEMAND_DEBOUNCE_MS);
  });
  observer.observe(el);
  return {
    disconnect() {
      if (debounceTimer !== null) {
        clearTimeout(debounceTimer);
        debounceTimer = null;
      }
      observer.disconnect();
    },
  };
}

/**
 * Screen Share PiP — single screen share video with real MediaStream
 * via PipVoiceClient's own mediasoup consumer.
 */
const ScreenPipContent: React.FC<{
  pipId: string;
  pinned: boolean;
  onTogglePin: () => void;
  onClose: () => void;
}> = ({ pipId, pinned, onTogglePin, onClose }) => {
  const videoRef = useRef<HTMLVideoElement>(null);
  const clientRef = useRef<PipVoiceClient | null>(null);
  const demandObserverRef = useRef<PipScreenDemandHandle | null>(null);
  const [sharerName, setSharerName] = useState('');
  const [loading, setLoading] = useState(true);
  const [hasStream, setHasStream] = useState(false);

  // Extract producerId from pipId: "screen-{producerId}"
  const producerId = pipId.replace(/^screen-/, '');

  useEffect(() => {
    const client = new PipVoiceClient(pipId);
    const removeCloseGuard = installPipCloseGuard(client, pipId);
    clientRef.current = client;

    client.onStateUpdate = (msg: AnyPipBroadcast) => {
      if (
        (msg.type === 'producer-closed' && msg.producerId === producerId) ||
        msg.type === 'voice-ended'
      ) {
        // Screen share ended or voice session ended — close PiP
        onClose();
      }
    };

    const setup = async () => {
      try {
        const voiceState = await client.init();

        // Find the screen producer matching our producerId
        const screenProducer = voiceState.activeProducers.find(
          (p) => p.producerId === producerId && p.source === 'screen'
        );

        if (screenProducer) {
          // Find sharer name
          const sharer = Object.values(voiceState.participants).find(
            (p) => p.userId === screenProducer.userId
          );
          setSharerName(sharer?.displayName || sharer?.username || 'Unknown');

          // Consume the screen share
          const stream = await client.consume(
            screenProducer.producerId,
            'screen',
            screenProducer.userId
          );

          if (stream && videoRef.current) {
            videoRef.current.srcObject = stream;
            videoRef.current.play().catch(() => {});
            setHasStream(true);
          }

          // Signal ready — main window will pause its screen consumer
          await client.signalReady();

          // #1924: report screen render-state demand for the PiP's OWN consumer so an
          // H264/VP8 simulcast screen unsticks off spatial layer 0. The PiP has no
          // socket; PipVoiceClient proxies the demand to the main window over RPC.
          const screenConsumerId = client.getConsumerIdBySource('screen', screenProducer.userId);
          const screenVideoEl = videoRef.current;
          if (screenConsumerId && screenVideoEl) {
            demandObserverRef.current = observePipScreenDemand(
              client,
              screenConsumerId,
              screenVideoEl
            );
          }
        }
      } catch (err) {
        console.error('[ScreenPip] Init failed:', errorMessage(err));
      } finally {
        setLoading(false);
      }
    };

    setup();

    const videoEl = videoRef.current;
    const clientEl = clientRef.current;
    return () => {
      removeCloseGuard();
      demandObserverRef.current?.disconnect();
      demandObserverRef.current = null;
      if (videoEl) videoEl.srcObject = null;
      client.dispose().catch(console.error);
      if (clientEl === clientRef.current) clientRef.current = null;
    };
  }, [pipId, producerId, onClose]);

  return (
    <div className="pip-window pip-window--screen">
      <PipHeader pinned={pinned} onTogglePin={onTogglePin} onClose={onClose} />
      {(() => {
        if (loading) {
          return (
            <div className="pip-screen-placeholder">
              <span className="pip-screen-placeholder__text">Connecting...</span>
            </div>
          );
        }
        if (!hasStream) {
          return (
            <div className="pip-screen-placeholder">
              <span className="pip-screen-placeholder__text">Screen share ended</span>
              <span className="pip-screen-placeholder__sub">The stream is no longer available</span>
            </div>
          );
        }
        return null;
      })()}
      <video
        ref={videoRef}
        className="pip-screen-video"
        autoPlay
        playsInline
        muted
        style={{ display: hasStream ? 'block' : 'none' }}
      />
      {hasStream && sharerName && (
        <div className="pip-screen-overlay">
          <span className="pip-screen-overlay__name">{sharerName}&apos;s screen</span>
        </div>
      )}
    </div>
  );
};

// ── Shared Header ───────────────────────────────────────────────

/**
 * Frameless window header with drag region, pin toggle, and close.
 */
const PipHeader: React.FC<{
  pinned: boolean;
  onTogglePin: () => void;
  onClose: () => void;
}> = ({ pinned, onTogglePin, onClose }) => (
  <div className="pip-header">
    <div className="pip-header__drag" />
    <button
      className={`pip-header__btn ${pinned ? 'pip-header__btn--active' : ''}`}
      onClick={onTogglePin}
      title={pinned ? 'Unpin from top' : 'Pin to top'}
    >
      {pinned ? <Pin size={12} /> : <PinOff size={12} />}
    </button>
    <button className="pip-header__btn pip-header__btn--close" onClick={onClose} title="Close PiP">
      <X size={12} />
    </button>
  </div>
);

export default PipWindow;
