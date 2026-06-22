/**
 * E2EE Worker ↔ Main Thread communication protocol.
 * Shared types imported by both the Worker and voiceService.
 */

// ─── Main Thread → Worker ────────────────────────────────────────────

export type E2EEWorkerMessage =
  | { type: 'init'; encryptKey: CryptoKey; currentKeyId: number }
  | { type: 'addDecryptKey'; senderUserId: string; keyId: number; key: CryptoKey }
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
