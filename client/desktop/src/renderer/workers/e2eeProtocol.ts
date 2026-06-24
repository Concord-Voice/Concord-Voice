/**
 * E2EE Worker ↔ Main Thread communication protocol.
 * Shared types imported by both the Worker and voiceService.
 */

// ─── Main Thread → Worker ────────────────────────────────────────────

export type E2EEWorkerMessage =
  // #1878: init carries the authoritative CSK keyVersion so the worker stamps
  // it into every outgoing v3 frame trailer (binds encrypt version at init /
  // reconnect / post-rotation — never a stale 0 when the channel is higher).
  | { type: 'init'; encryptKey: CryptoKey; currentKeyId: number; keyVersion: number }
  // #1878: addDecryptKey carries the CSK keyVersion so the worker keys its
  // decrypt map by senderId:keyVersion:keyId (matches the v3 frame trailer).
  | {
      type: 'addDecryptKey';
      senderUserId: string;
      keyVersion: number;
      keyId: number;
      key: CryptoKey;
    }
  | { type: 'rotateKeys' }
  | { type: 'catchUpToEpoch'; targetEpoch: number }
  | { type: 'destroy' };

// ─── Worker → Main Thread ────────────────────────────────────────────

export type E2EEMainMessage =
  | { type: 'rotationComplete'; newKeyId: number }
  | { type: 'requestKeyframe'; senderUserId: string }
  | {
      type: 'requestRecovery';
      senderUserId: string;
      dropCount: number;
    }
  // #1878: typed decrypt-miss → on-demand key request for an exact
  // (senderUserId, keyVersion, keyId). Distinct from requestRecovery, which
  // handles the OperationError/persistent-failure self-heal case.
  | {
      type: 'requestFrameKey';
      senderUserId: string;
      keyVersion: number;
      keyId: number;
    }
  | {
      type: 'log';
      level: 'debug' | 'info' | 'warn' | 'error';
      message: string;
      data?: Record<string, unknown>;
    };

// ─── RTCRtpScriptTransform options ───────────────────────────────────

export interface E2EETransformOptions {
  role: 'encrypt' | 'decrypt';
  senderUserId?: string;
}
