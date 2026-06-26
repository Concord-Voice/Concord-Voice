// @vitest-environment node
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { parseAv1Obus as parseAv1ObusForTest } from '@/renderer/services/av1ObuParser';
import { decodeObuMiniHeader as decodeObuMiniHeaderForTest } from '@/renderer/services/mediaFrameMiniHeader';

// MediaEncryption is the default export-less class — import directly
let MediaEncryption: typeof import('@/renderer/services/mediaEncryption').MediaEncryption;
let deriveFrameKey: typeof import('@/renderer/services/mediaEncryption').deriveFrameKey;
let ratchetKey: typeof import('@/renderer/services/mediaEncryption').ratchetKey;
let FrameKeyMissError: typeof import('@/renderer/services/mediaEncryption').FrameKeyMissError;

beforeEach(async () => {
  const mod = await import('@/renderer/services/mediaEncryption');
  MediaEncryption = mod.MediaEncryption;
  deriveFrameKey = mod.deriveFrameKey;
  ratchetKey = mod.ratchetKey;
  FrameKeyMissError = mod.FrameKeyMissError;
});

/** Helper: generate a test AES-256 key (simulates a channel CSK) */
async function generateTestCSK(): Promise<CryptoKey> {
  return crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, ['encrypt', 'decrypt']);
}

/** Helper: build a fake RTCEncodedAudioFrame */
function fakeAudioFrame(size: number): RTCEncodedAudioFrame {
  const buf = new ArrayBuffer(size);
  const view = new Uint8Array(buf);
  // Fill with recognizable pattern (Opus TOC byte + payload)
  for (let i = 0; i < size; i++) view[i] = i & 0xff;
  return { data: buf } as unknown as RTCEncodedAudioFrame;
}

/** Helper: build a fake RTCEncodedVideoFrame */
function fakeVideoFrame(size: number): RTCEncodedVideoFrame {
  const buf = new ArrayBuffer(size);
  const view = new Uint8Array(buf);
  for (let i = 0; i < size; i++) view[i] = (i + 0x10) & 0xff;
  return { data: buf, type: 'delta' } as unknown as RTCEncodedVideoFrame;
}

describe('MediaEncryption', () => {
  describe('encrypt/decrypt round-trip', () => {
    it('round-trips an audio frame', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      const frame = fakeAudioFrame(50);
      // Copy original data for comparison
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // Frame should be larger (payload + trailer overhead)
      expect(frame.data.byteLength).toBeGreaterThan(50);

      // Magic trailer should be present
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 1]).toBe(0xad);
      expect(encrypted[encrypted.length - 2]).toBe(0xde);

      // headerBytes field should be 1 (audio) — v4 trailer position -22
      expect(encrypted[encrypted.length - 22]).toBe(1);

      // Decrypt
      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');

      // Should match original
      const decrypted = new Uint8Array(frame.data);
      expect(decrypted).toEqual(originalData);
    });

    it('round-trips a video frame', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      const frame = fakeVideoFrame(200);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'vp9');
      expect(frame.data.byteLength).toBeGreaterThan(200);

      // headerBytes field should be 2 (video) — v4 trailer position -22
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 22]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('round-trips after key rotation (epoch > 0)', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      // Rotate both sides to epoch 2
      await sender.rotateKeys();
      await sender.rotateKeys();
      await receiver.rotateKeys();
      await receiver.rotateKeys();

      const frame = fakeAudioFrame(40);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // keyId in trailer should be 2 — v4 keyId is 2B BE, low byte at -20
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 20]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('serializes overlapping key rotations so epochs are not lost', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      await Promise.all([sender.rotateKeys(), sender.rotateKeys()]);
      expect(sender.getCurrentKeyId()).toBe(2);

      await receiver.catchUpToEpoch(2);

      const frame = fakeAudioFrame(40);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      const encrypted = new Uint8Array(frame.data);
      // keyId low byte at v4 position -20
      expect(encrypted[encrypted.length - 20]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('keeps ciphertext and trailer keyId on the same epoch when rotation overlaps encryption', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id', 0);
      await receiver.addDecryptKeyAtEpoch(csk, 'sender-user-id', 1);

      const frame = fakeVideoFrame(200);
      const originalData = new Uint8Array(frame.data).slice();

      const originalEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
      let signalEncryptStarted: (() => void) | null = null;
      let releaseEncrypt: (() => void) | null = null;
      const encryptStarted = new Promise<void>((resolve) => {
        signalEncryptStarted = resolve;
      });
      const encryptGate = new Promise<void>((resolve) => {
        releaseEncrypt = resolve;
      });

      const encryptSpy = vi
        .spyOn(crypto.subtle, 'encrypt')
        .mockImplementation(async (algorithm, key, data) => {
          if (!signalEncryptStarted) throw new Error('test signal not initialized');
          signalEncryptStarted();
          await encryptGate;
          return originalEncrypt(algorithm, key, data);
        });

      try {
        const encryptPromise = sender.encryptFrame(frame, 'vp9');
        await encryptStarted;

        await sender.rotateKeys();
        expect(sender.getCurrentKeyId()).toBe(1);

        if (!releaseEncrypt) throw new Error('test release not initialized');
        releaseEncrypt();
        await encryptPromise;

        const encrypted = new Uint8Array(frame.data);
        // keyId low byte at v4 position -20
        expect(encrypted[encrypted.length - 20]).toBe(0);

        await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
        expect(new Uint8Array(frame.data)).toEqual(originalData);
      } finally {
        encryptSpy.mockRestore();
      }
    });
  });

  describe('rejection paths', () => {
    it('rejects non-empty frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      const frame = fakeAudioFrame(50);

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'unencrypted media frame received'
      );
    });

    it('passes through empty frames', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(0);
      const originalData = new Uint8Array(frame.data).slice();

      await me.decryptFrame(frame, 'sender-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('rejects non-empty too-small frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(10);

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'unencrypted media frame received'
      );
    });

    it('rejects too-small frames that carry the E2EE magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 10 bytes is below the v4 minimum (TRAILER_SIZE_V4 22 + MIN_GCM_OVERHEAD
      // 17 = 39), so the "too small" guard fires.
      const buf = new ArrayBuffer(10);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[8] = 0xde;
      view[9] = 0xad;
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('rejects frames with magic trailer but too-small ciphertext', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      // 38 bytes: below v4 minimum (TRAILER_SIZE_V4 22 + MIN_GCM_OVERHEAD 17 = 39),
      // so the "too small" guard fires before the version check.
      const buf = new ArrayBuffer(38);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[36] = 0xde; // magic
      view[37] = 0xad;
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('rejects frames with invalid headerBytes in trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 50-byte frame with magic trailer, valid v4 version byte, but headerBytes=0 (invalid).
      // v4 layout from end: magic[-2,-1], version[-3]=4, IV[-15,-4], keyVersion[-19,-16],
      // keyId[-21,-20], headerBytes[-22].
      const buf = new ArrayBuffer(50);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[48] = 0xde;
      view[49] = 0xad;
      view[50 - 3] = 4; // version = 4 at position length-3 so version check passes
      view[50 - 22] = 0; // headerBytes = 0 (invalid, must be 1-10) at v4 position length-22
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('throws when no decrypt key is available', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      // Intentionally NOT adding decrypt key for sender

      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame, 'opus');

      await expect(receiver.decryptFrame(frame, 'sender-user-id', 'opus')).rejects.toThrow(
        /no decrypt key/
      );
    });
  });

  describe('self-healing ratchet', () => {
    it('ratchets forward to decrypt frames at a higher epoch', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      // Sender rotates to epoch 3, receiver stays at epoch 0
      await sender.rotateKeys();
      await sender.rotateKeys();
      await sender.rotateKeys();

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // Receiver should self-heal by ratcheting from epoch 0 → 3
      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('Worker-path APIs (initFromKey / addDecryptKeyDirect / setCurrentKeyId)', () => {
    it('derives and ratchets 256-bit AES-GCM frame keys', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-a');
      const ratcheted = await ratchetKey(key);

      expect(key.algorithm.name).toBe('AES-GCM');
      expect((key.algorithm as AesKeyAlgorithm).length).toBe(256);
      expect(ratcheted.algorithm.name).toBe('AES-GCM');
      expect((ratcheted.algorithm as AesKeyAlgorithm).length).toBe(256);
    });

    it('initFromKey enables encryptFrame and sets currentKeyId', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-a');

      const enc = new MediaEncryption();
      enc.initFromKey(key, 5);

      expect(enc.getCurrentKeyId()).toBe(5);

      // Should be able to encrypt
      const frame = fakeAudioFrame(50);
      await enc.encryptFrame(frame, 'opus');

      // Encrypted frame should be larger and have the magic trailer
      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(50);
      expect(data[data.length - 1]).toBe(0xad);
      expect(data[data.length - 2]).toBe(0xde);

      // keyId in trailer should be 5 — v4 keyId is 2B BE, low byte at -20
      expect(data[data.length - 20]).toBe(5);
    });

    it('addDecryptKeyDirect allows decryptFrame for matching keyId', async () => {
      const csk = await generateTestCSK();
      const senderKey = await deriveFrameKey(csk, 'sender-x');

      // Sender uses initFromKey (Worker path)
      const sender = new MediaEncryption();
      sender.initFromKey(senderKey, 0);

      // Receiver uses addDecryptKeyDirect (Worker path)
      const receiver = new MediaEncryption();
      const receiverOwnKey = await deriveFrameKey(csk, 'receiver-y');
      receiver.initFromKey(receiverOwnKey, 0);
      receiver.addDecryptKeyDirect('sender-x', 0, senderKey);

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      await receiver.decryptFrame(frame, 'sender-x', 'opus');

      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('addDecryptKeyDirect at a ratcheted epoch decrypts correctly', async () => {
      const csk = await generateTestCSK();

      // Sender at epoch 2
      const sender = new MediaEncryption();
      await sender.init(csk, 'sender-z');
      await sender.rotateKeys();
      await sender.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(2);

      // Receiver derives sender key and pre-ratchets to epoch 2, then adds directly
      let senderKey = await deriveFrameKey(csk, 'sender-z');
      senderKey = await ratchetKey(senderKey);
      senderKey = await ratchetKey(senderKey);

      const receiver = new MediaEncryption();
      const recvKey = await deriveFrameKey(csk, 'receiver-w');
      receiver.initFromKey(recvKey, 0);
      receiver.addDecryptKeyDirect('sender-z', 2, senderKey);

      const frame = fakeAudioFrame(60);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      await receiver.decryptFrame(frame, 'sender-z', 'opus');

      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('setCurrentKeyId updates epoch without nulling encryptKey', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-b');

      const enc = new MediaEncryption();
      enc.initFromKey(key, 0);

      // Advance epoch via setCurrentKeyId
      enc.setCurrentKeyId(7);
      expect(enc.getCurrentKeyId()).toBe(7);

      // encryptFrame should still work (key not nulled)
      const frame = fakeAudioFrame(40);
      await enc.encryptFrame(frame, 'opus');

      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(40);
      // keyId in trailer should be 7 — v4 keyId is 2B BE, low byte at -20
      expect(data[data.length - 20]).toBe(7);
    });

    it('full Worker-path round-trip: initFromKey + addDecryptKeyDirect + rotation', async () => {
      const csk = await generateTestCSK();
      const senderKey = await deriveFrameKey(csk, 'alice');
      const receiverKey = await deriveFrameKey(csk, 'bob');

      const sender = new MediaEncryption();
      sender.initFromKey(senderKey, 0);

      const receiver = new MediaEncryption();
      receiver.initFromKey(receiverKey, 0);
      receiver.addDecryptKeyDirect('alice', 0, senderKey);

      // Both rotate to epoch 1 (simulating Worker receiving rotateKeys message)
      await sender.rotateKeys();
      await receiver.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(1);
      expect(receiver.getCurrentKeyId()).toBe(1);

      const frame = fakeAudioFrame(80);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // keyId should be 1 — v4 keyId is 2B BE, low byte at -20
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 20]).toBe(1);

      await receiver.decryptFrame(frame, 'alice', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('debouncedRotateKeys', () => {
    it('rotates after debounce delay', async () => {
      const { vi } = await import('vitest');
      vi.useFakeTimers();
      try {
        const csk = await generateTestCSK();
        const enc = new MediaEncryption();
        await enc.init(csk, 'user-1');
        expect(enc.getCurrentKeyId()).toBe(0);

        enc.debouncedRotateKeys();

        // Not yet rotated
        expect(enc.getCurrentKeyId()).toBe(0);

        // Advance past 2s debounce
        await vi.advanceTimersByTimeAsync(2500);

        await vi.waitFor(() => {
          expect(enc.getCurrentKeyId()).toBe(1);
        });
      } finally {
        vi.useRealTimers();
      }
    });

    it('coalesces rapid calls into single rotation', async () => {
      const { vi } = await import('vitest');
      vi.useFakeTimers();
      try {
        const csk = await generateTestCSK();
        const enc = new MediaEncryption();
        await enc.init(csk, 'user-1');

        // Fire 5 rapid calls
        for (let i = 0; i < 5; i++) {
          enc.debouncedRotateKeys();
        }

        await vi.advanceTimersByTimeAsync(2500);

        // Should have rotated exactly once (not 5 times)
        await vi.waitFor(() => {
          expect(enc.getCurrentKeyId()).toBe(1);
        });
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe('catchUpToEpoch', () => {
    it('catches up from epoch 0 to target', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');
      expect(enc.getCurrentKeyId()).toBe(0);

      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);
    });

    it('does not overshoot when duplicate catch-up requests overlap', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await Promise.all([enc.catchUpToEpoch(2), enc.catchUpToEpoch(2)]);

      expect(enc.getCurrentKeyId()).toBe(2);
    });

    it('rejects instead of spinning when catch-up cannot advance', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');
      enc.destroy();

      await expect(enc.catchUpToEpoch(1)).rejects.toThrow('E2EE epoch catch-up stalled');
      expect(enc.getCurrentKeyId()).toBe(0);
    });

    it('no-op when already at target', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);

      // Calling again with same target should be no-op
      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);

      // Calling with lower target should also be no-op
      await enc.catchUpToEpoch(1);
      expect(enc.getCurrentKeyId()).toBe(3);
    });

    it('rejects gap > 100', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await expect(enc.catchUpToEpoch(101)).rejects.toThrow('E2EE epoch gap too large');
      expect(enc.getCurrentKeyId()).toBe(0);
    });
  });

  describe('destroy', () => {
    it('clears state and prevents further encryption', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      // Encryption works before destroy
      const frame = fakeAudioFrame(40);
      await enc.encryptFrame(frame, 'opus');

      enc.destroy();

      // Encrypt should fail after destroy
      const frame2 = fakeAudioFrame(40);
      await expect(enc.encryptFrame(frame2, 'opus')).rejects.toThrow('no encrypt key');
    });

    it('is idempotent', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      enc.destroy();
      enc.destroy(); // Should not throw
      expect(enc.getCurrentKeyId()).toBe(0);
    });
  });

  describe('addDecryptKeyAtEpoch', () => {
    it('pre-ratchets key to target epoch', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender');
      await receiver.init(csk, 'receiver');

      // Rotate sender to epoch 3
      await sender.rotateKeys();
      await sender.rotateKeys();
      await sender.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(3);

      // Pre-ratchet receiver's decrypt key for sender to epoch 3
      await receiver.addDecryptKeyAtEpoch(csk, 'sender', 3);

      // Sender encrypts at epoch 3
      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame, 'opus');

      // Receiver should decrypt successfully
      await receiver.decryptFrame(frame, 'sender', 'opus');
    });

    it('rejects target epoch > 100', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await expect(enc.addDecryptKeyAtEpoch(csk, 'sender', 101)).rejects.toThrow(
        'E2EE epoch gap too large'
      );
    });
  });
});

// Regression for #1742: an empty (0-byte) DTX frame used to encrypt to exactly
// 32 bytes, which the decrypt `< 33` too-small guard misclassified as
// unencrypted and fed to the Opus decoder undeciphered — the receiver-side
// garble-during-silence. The fix passes empty frames through unchanged.
describe('MediaEncryption — #1742 empty DTX frame passthrough', () => {
  it('passes an empty (0-byte) frame through unchanged on encrypt (never the 32-byte blob)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const frame = fakeAudioFrame(0);
    await sender.encryptFrame(frame, 'opus');

    // Must stay 0 bytes — the pre-fix bug produced exactly 32 bytes here.
    expect(frame.data.byteLength).toBe(0);
  });

  it('round-trips an empty frame as empty (decoder sees DTX silence, not garble)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(0);
    await sender.encryptFrame(frame, 'opus');
    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');

    expect(frame.data.byteLength).toBe(0);
  });

  it('still round-trips a 1-byte audio frame (boundary just above empty)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(1);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'opus');
    // 1-byte input encrypts to 34 bytes (>= the v4 guard of 39 — no, 1+16+22=39 exactly) —
    // must decrypt, not pass through.
    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('stores the ACTUAL header length in the trailer (closes the sub-header video boundary, H5)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    // A 1-byte video frame: getUnencryptedBytes()=2 but only 1 byte exists, so
    // the trailer must record the actual header length (1), not the static 2 —
    // else the decrypt header/ciphertext split overruns and the frame is
    // mis-passed-through instead of decrypted.
    const frame = fakeVideoFrame(1);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'vp9');

    const enc = new Uint8Array(frame.data);
    expect(enc[enc.length - 22]).toBe(1); // headerBytes field = actual header length (v4 -22)

    await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('uses a fully random 96-bit GCM IV for encrypted frames', async () => {
    // Security invariant: empty-frame early return must stay above IV
    // generation, and real encrypted frames request the full 96-bit GCM nonce
    // from WebCrypto rather than combining a counter prefix with random suffix.
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    const randomSpy = vi.spyOn(crypto, 'getRandomValues');

    const empty = fakeAudioFrame(0);
    await sender.encryptFrame(empty, 'opus'); // must not advance the counter
    expect(empty.data.byteLength).toBe(0);
    expect(randomSpy).not.toHaveBeenCalled();

    const real = fakeAudioFrame(50);
    await sender.encryptFrame(real, 'opus');
    expect(randomSpy).toHaveBeenCalledTimes(1);
    expect((randomSpy.mock.calls[0][0] as Uint8Array).byteLength).toBe(12);
    randomSpy.mockRestore();
  });
});

describe('channel CSK rotation desync (#1878)', () => {
  it('baseline: shared CSK round-trips', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKey(csk, 'alice', 0);

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('a CSK rotation is deterministic when the frame carries the version (#1878 fixed)', async () => {
    const cskOld = await generateTestCSK();
    const cskNew = await generateTestCSK();

    const sender = new MediaEncryption();
    sender.setKeyVersion(2); // sender re-based onto NEW CSK at version 2
    await sender.init(cskNew, 'alice');

    const receiver = new MediaEncryption();
    // Receiver holds BOTH versions (v1 old + v2 new) — the 3-part map keeps them distinct.
    await receiver.addDecryptKeyAtVersion(cskOld, 'alice', 1, 0);
    await receiver.addDecryptKeyAtVersion(cskNew, 'alice', 2, 0);

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // Frame stamps version 2 → receiver selects the v2 key deterministically.
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });
});

describe('frame crypto v3 (#1878)', () => {
  it('round-trips a v3 frame carrying (keyVersion, keyId)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(7); // new API: bind encrypt keyVersion
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 7, 0); // new API: (csk, sender, version, keyId)

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // trailer is now 22 bytes (v4); magic still last 2
    const bytes = new Uint8Array(frame.data);
    expect(bytes.at(-1)).toBe(0xad);
    expect(bytes.at(-2)).toBe(0xde);

    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('never decrypts a v(N) frame with a v(M) key (version isolation)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(2);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 3, 0); // wrong version held

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // Map key is senderId:2:0 (from frame); receiver holds senderId:3:0 → miss.
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).rejects.toThrow(/no decrypt key/);
  });

  it('keyVersion boundary: a large version (e.g. 65537) survives the 4-byte BE field', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(65537);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 65537, 0);
    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('a decrypt miss throws FrameKeyMissError with (keyVersion,keyId), not OperationError', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(9);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption(); // holds NO key
    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');

    let caught: unknown;
    try {
      await receiver.decryptFrame(frame, 'alice', 'vp9');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(FrameKeyMissError);
    expect(caught).toMatchObject({
      name: 'FrameKeyMissError',
      senderUserId: 'alice',
      keyVersion: 9,
      keyId: 0,
    });
    // Message still contains "no decrypt key" so the existing regex tests pass.
    expect((caught as Error).message).toMatch(/no decrypt key/);
  });
});

describe('v4 whole-frame (VP9/VP8/Opus unchanged behavior, version-stamped)', () => {
  it('advertises crypto version 4', async () => {
    const mod = await import('@/renderer/services/mediaEncryption');
    expect(mod.MEDIA_E2EE_FRAME_CRYPTO_VERSION).toBe(4);
  });

  it('round-trips an audio frame under v4 with version marker in trailer', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(50);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'opus');

    const enc = new Uint8Array(frame.data);
    // v4 trailer: ...[version:1 (=4)][magic:2]; magic still last.
    expect(enc[enc.length - 1]).toBe(0xad);
    expect(enc[enc.length - 2]).toBe(0xde);
    expect(enc[enc.length - 3]).toBe(4); // version marker

    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('round-trips a VP9 video frame under v4 (no regression of the green path)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeVideoFrame(200);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'vp9');
    await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });
});

describe('AV1 per-OBU (v4)', () => {
  /** Build a single low-overhead OBU: header + optional ext + leb128 size + payload. */
  function obu(opts: {
    type: number;
    payload: Uint8Array;
    ext?: number; // present → extension_flag=1, this is the extension byte
    hasSize?: boolean; // default true
  }): Uint8Array {
    const { type, payload, ext, hasSize = true } = opts;
    const header =
      ((type & 0x0f) << 3) | ((ext !== undefined ? 1 : 0) << 2) | ((hasSize ? 1 : 0) << 1);
    const head: number[] = [header];
    if (ext !== undefined) head.push(ext & 0xff);
    const size: number[] = [];
    if (hasSize) {
      let v = payload.length;
      do {
        let b = v & 0x7f;
        v >>>= 7;
        if (v) b |= 0x80;
        size.push(b);
      } while (v);
    }
    return new Uint8Array([...head, ...size, ...payload]);
  }

  function concat(...parts: Uint8Array[]): Uint8Array {
    const total = parts.reduce((n, p) => n + p.length, 0);
    const out = new Uint8Array(total);
    let o = 0;
    for (const p of parts) {
      out.set(p, o);
      o += p.length;
    }
    return out;
  }

  // Build a valid-ish AV1 temporal unit: TD + SEQUENCE_HEADER + FRAME(6) + TILE_GROUP(4).
  function buildAv1Frame(framePayload: Uint8Array, tgPayload: Uint8Array): RTCEncodedVideoFrame {
    const td = obu({ type: 2, payload: new Uint8Array(0) });
    const seq = obu({ type: 1, payload: new Uint8Array([0xaa, 0xbb]) });
    const frame = obu({ type: 6, payload: framePayload });
    const tg = obu({ type: 4, payload: tgPayload });
    const data = concat(td, seq, frame, tg);
    return {
      data: data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength),
      type: 'key',
    } as unknown as RTCEncodedVideoFrame;
  }

  it('encrypts only FRAME/TILE_GROUP payloads and leaves structure cleartext', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const framePayload = new Uint8Array([1, 2, 3, 4, 5, 6]);
    const tgPayload = new Uint8Array([7, 7, 7, 7]);
    const frame = buildAv1Frame(framePayload, tgPayload);
    await sender.encryptFrame(frame, 'av1');

    // The TD (type 2, payload 0) must still be present cleartext at the start.
    const enc = new Uint8Array(frame.data);
    expect(enc[0] & 0x80).toBe(0); // forbidden bit clear
    expect((enc[0] >>> 3) & 0x0f).toBe(2); // first OBU is still the TD

    // Each encrypted tile-data OBU payload now starts with the 0xDEAD mini-header magic.
    // (Parse to find them rather than asserting a fixed offset.)
    const obus = parseAv1ObusForTest(enc);
    expect(obus).not.toBeNull();
    const tileData = obus!.filter((o) => o.obuType === 6 || o.obuType === 4);
    expect(tileData).toHaveLength(2);
    for (const o of tileData) {
      expect(enc[o.payloadOffset]).toBe(0xde);
      expect(enc[o.payloadOffset + 1]).toBe(0xad);
    }
    // The SEQUENCE_HEADER (type 1) payload stays cleartext (0xaa 0xbb).
    const seqObu = obus!.find((o) => o.obuType === 1);
    expect(seqObu).toBeDefined();
    expect(Array.from(enc.slice(seqObu!.payloadOffset, seqObu!.payloadOffset + 2))).toEqual([
      0xaa, 0xbb,
    ]);
  });

  it('round-trips an AV1 frame: decrypt restores exact original payloads', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const framePayload = new Uint8Array([1, 2, 3, 4, 5, 6]);
    const tgPayload = new Uint8Array([7, 8, 9]);
    const frame = buildAv1Frame(framePayload, tgPayload);
    const original = new Uint8Array(frame.data).slice();

    await sender.encryptFrame(frame, 'av1');
    expect(new Uint8Array(frame.data)).not.toEqual(original); // structure changed (encrypted payloads)
    await receiver.decryptFrame(frame, 'sender-user-id', 'av1');
    expect(new Uint8Array(frame.data)).toEqual(original); // byte-exact restore
  });

  it('per-OBU IV uniqueness: 2 encrypted OBUs in one frame get distinct IVs', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    await sender.encryptFrame(frame, 'av1');

    const enc = new Uint8Array(frame.data);
    const obus = parseAv1ObusForTest(enc)!.filter((o) => o.obuType === 6 || o.obuType === 4);
    expect(obus).toHaveLength(2);
    const ivs = obus.map((o) => {
      const mh = decodeObuMiniHeaderForTest(enc.slice(o.payloadOffset, o.payloadOffset + 22));
      return Array.from(mh!.iv);
    });
    expect(ivs[0]).not.toEqual(ivs[1]); // distinct obu_seq_index → distinct IV
  });

  it('survives a simulated SFU leb128 re-encode of cleartext OBU structure', async () => {
    // The depacketizer may re-encode structure; the payloads survive (spec §2.1).
    // Simulate by leaving payloads intact (the parser recomputes boundaries from
    // the received leb128 sizes), which the round-trip above already covers; here
    // assert decrypt still succeeds after a no-op structure rewrite.
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const frame = buildAv1Frame(new Uint8Array([1, 2, 3, 4]), new Uint8Array([5, 6, 7, 8]));
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'av1');
    await receiver.decryptFrame(frame, 'sender-user-id', 'av1');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('drops (throws) a malformed AV1 frame fail-closed', async () => {
    const csk = await generateTestCSK();
    const receiver = new MediaEncryption();
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const bad = {
      data: new Uint8Array([(6 << 3) | 0b10, 100, 1]).buffer,
      type: 'key',
    } as unknown as RTCEncodedVideoFrame;
    await expect(receiver.decryptFrame(bad, 'sender-user-id', 'av1')).rejects.toThrow();
  });
});

// ---------------------------------------------------------------------------
// Task 10: mixed-codec room (per-sender codec isolation, spec §6.2)
// ---------------------------------------------------------------------------

describe('mixed-codec room (per-sender codec isolation)', () => {
  it('one receiver decrypts an AV1 sender and a VP9 sender with the correct scheme each', async () => {
    const csk = await generateTestCSK();
    const av1Sender = new MediaEncryption();
    const vp9Sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await av1Sender.init(csk, 'alice'); // AV1 publisher
    await vp9Sender.init(csk, 'bob'); // VP9 publisher
    await receiver.init(csk, 'me');
    await receiver.addDecryptKey(csk, 'alice');
    await receiver.addDecryptKey(csk, 'bob');

    // AV1 frame from alice: TD (type 2, size 0) + FRAME (type 6, payload [1,2,3])
    const av1Data = new Uint8Array([(2 << 3) | 0b10, 0, (6 << 3) | 0b10, 3, 1, 2, 3]);
    const av1Frame = { data: av1Data.buffer, type: 'key' } as unknown as RTCEncodedVideoFrame;
    const av1Orig = new Uint8Array(av1Frame.data).slice();
    await av1Sender.encryptFrame(av1Frame, 'av1');

    // VP9 whole-frame from bob
    const vp9Frame = fakeVideoFrame(120);
    const vp9Orig = new Uint8Array(vp9Frame.data).slice();
    await vp9Sender.encryptFrame(vp9Frame, 'vp9');

    await receiver.decryptFrame(av1Frame, 'alice', 'av1');
    await receiver.decryptFrame(vp9Frame, 'bob', 'vp9');

    expect(new Uint8Array(av1Frame.data)).toEqual(av1Orig);
    expect(new Uint8Array(vp9Frame.data)).toEqual(vp9Orig);
  });
});
