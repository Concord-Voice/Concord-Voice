import { createStore } from '../utils/createStore';
// Re-export from recoveryService so there is a single source of truth.
// ConnectionLostOverlay.tsx imports these types from connectionStore — the
// re-export keeps that contract working without touching import sites.
// DiagnosticResults is also used locally (in ConnectionState), so it needs
// the two-step import + export. CheckResult is a pure re-export.
import type { DiagnosticResults } from '../services/recoveryService';
export type { DiagnosticResults };
export type { CheckResult } from '../services/recoveryService';

export type RecoveryPhase =
  | 'stable' // Normal operation
  | 'grace_period' // First 15s — aggressive reconnect, no UI overlay
  | 'preflight' // Running diagnostics
  | 'recovery_a' // Server issue — graceful reset + wait for server
  | 'recovery_b' // Client issue — soft restart attempt
  | 'fatal'; // Unrecoverable — show full overlay

interface ConnectionState {
  /** Current recovery phase */
  phase: RecoveryPhase;

  /** Results from preflight diagnostics */
  diagnostics: DiagnosticResults | null;

  /** Timestamp when connection was lost */
  lostAt: number | null;

  /** Voice channel the user was in when connection dropped (for grace-period rejoin) */
  lastVoiceChannelId: string | null;

  /** Number of recovery attempts in this disconnection episode */
  recoveryAttempts: number;

  /**
   * Cumulative count of WebSocket payloads rejected by schema validation
   * since the current connection was established. Resets to 0 on every
   * fresh connection (handled by the 'connected' envelope event in
   * websocketService.handleMessage, via `useConnectionStore.setState({ wireViolationCount: 0 })`).
   *
   * Surface: no UI today. Read via useConnectionStore.getState().wireViolationCount
   * for debug-panel visibility (future). Persistent non-zero values during a
   * session indicate server/client schema drift; the per-event `[WS] wire violation`
   * log entries reveal which event types are drifting.
   *
   * @see [internal]specs/2026-05-23-709-ws-discriminated-union-design.md §5
   */
  wireViolationCount: number;

  /** Start the grace period (called when WS transitions to RECONNECTING) */
  startGracePeriod: () => void;

  /** Transition to preflight diagnostics phase */
  enterPreflight: () => void;

  /** Store diagnostic results */
  setDiagnostics: (d: DiagnosticResults) => void;

  /** Transition to recovery A (server issue — wait for reconnect) */
  enterRecoveryA: () => void;

  /** Transition to recovery B (client issue — soft restart) */
  enterRecoveryB: () => void;

  /** Transition to fatal (unrecoverable — user action required) */
  enterFatal: () => void;

  /** Store the voice channel ID before emergency cleanup */
  setLastVoiceChannelId: (id: string | null) => void;

  /** Increment recovery attempt counter */
  incrementRecoveryAttempts: () => void;

  /**
   * Increment the wire-violation counter. Called from wsService.handleMessage
   * when WebSocketEventSchema.safeParse rejects an incoming payload.
   */
  incrementWireViolation: () => void;

  /** Reset everything (called when connection is restored or session resets) */
  reset: () => void;
}

export const useConnectionStore = createStore<ConnectionState>()((set) => ({
  phase: 'stable',
  diagnostics: null,
  lostAt: null,
  lastVoiceChannelId: null,
  recoveryAttempts: 0,
  wireViolationCount: 0,

  startGracePeriod: () =>
    set({
      phase: 'grace_period',
      lostAt: Date.now(),
    }),

  enterPreflight: () =>
    set({
      phase: 'preflight',
    }),

  setDiagnostics: (d) => set({ diagnostics: d }),

  enterRecoveryA: () =>
    set({
      phase: 'recovery_a',
    }),

  enterRecoveryB: () =>
    set({
      phase: 'recovery_b',
    }),

  enterFatal: () =>
    set({
      phase: 'fatal',
    }),

  setLastVoiceChannelId: (id) => set({ lastVoiceChannelId: id }),

  incrementRecoveryAttempts: () =>
    set((s) => ({
      recoveryAttempts: s.recoveryAttempts + 1,
    })),

  incrementWireViolation: () =>
    set((s) => ({
      wireViolationCount: s.wireViolationCount + 1,
    })),

  reset: () =>
    set({
      phase: 'stable',
      diagnostics: null,
      lostAt: null,
      lastVoiceChannelId: null,
      recoveryAttempts: 0,
      wireViolationCount: 0,
    }),
}));
