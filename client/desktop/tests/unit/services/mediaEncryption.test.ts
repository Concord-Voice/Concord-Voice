// @vitest-environment node
import { describe, it, expect, beforeEach } from 'vitest';

// MediaEncryption is the default export-less class — import directly
let MediaEncryption: typeof import('@/renderer/services/mediaEncryption').MediaEncryption;
let deriveFrameKey: typeof import('@/renderer/services/mediaEncryption').deriveFrameKey;
let ratchetKey: typeof import('@/renderer/services/mediaEncryption').ratchetKey;

beforeEach(async () => {
  const mod = await import('@/renderer/services/mediaEncryption');
  MediaEncryption = mod.MediaEncryption;
  deriveFrameKey = mod.deriveFrameKey;
  ratchetKey = mod.ratchetKey;
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

      await sender.encryptFrame(frame);

      // Frame should be larger (payload + trailer overhead)
      expect(frame.data.byteLength).toBeGreaterThan(50);

      // Magic trailer should be present
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 1]).toBe(0xad);
      expect(encrypted[encrypted.length - 2]).toBe(0xde);

      // headerBytes field should be 1 (audio)
      expect(encrypted[encrypted.length - 16]).toBe(1);

      // Decrypt
      await receiver.decryptFrame(frame, 'sender-user-id');

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

      await sender.encryptFrame(frame);
      expect(frame.data.byteLength).toBeGreaterThan(200);

      // headerBytes field should be 2 (video)
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 16]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id');
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

      await sender.encryptFrame(frame);

      // keyId in trailer should be 2
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 15]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('rejection paths', () => {
    it('passes through frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      // No encryption → no magic trailer → should pass through unchanged
      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames that are too small', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(10);
      const originalData = new Uint8Array(frame.data).slice();

      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames with coincidental 0xDEAD but too small for valid ciphertext', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      // 33 bytes = minimum size to pass the first check, but with headerBytes=2
      // the ciphertext would be 33 - 16 (trailer) - 2 (header) = 15 bytes < 16
      const buf = new ArrayBuffer(33);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[31] = 0xde; // magic
      view[32] = 0xad;
      view[17] = 2; // headerBytes = 2 at position length-16
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;
      const originalData = new Uint8Array(buf).slice();

      // Should pass through: ciphertext is 15 bytes < 16 (GCM auth tag minimum)
      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames with invalid headerBytes in trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 50-byte frame with magic trailer but headerBytes=0 (invalid)
      const buf = new ArrayBuffer(50);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[48] = 0xde;
      view[49] = 0xad;
      view[34] = 0; // headerBytes = 0 (invalid, must be 1-10)
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;
      const originalData = new Uint8Array(buf).slice();

      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('throws when no decrypt key is available', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      // Intentionally NOT adding decrypt key for sender

      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame);

      await expect(receiver.decryptFrame(frame, 'sender-user-id')).rejects.toThrow(
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

      await sender.encryptFrame(frame);

      // Receiver should self-heal by ratcheting from epoch 0 → 3
      await receiver.decryptFrame(frame, 'sender-user-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('Worker-path APIs (initFromKey / addDecryptKeyDirect / setCurrentKeyId)', () => {
    it('initFromKey enables encryptFrame and sets currentKeyId', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-a');

      const enc = new MediaEncryption();
      enc.initFromKey(key, 5);

      expect(enc.getCurrentKeyId()).toBe(5);

      // Should be able to encrypt
      const frame = fakeAudioFrame(50);
      await enc.encryptFrame(frame);

      // Encrypted frame should be larger and have the magic trailer
      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(50);
      expect(data[data.length - 1]).toBe(0xad);
      expect(data[data.length - 2]).toBe(0xde);

      // keyId in trailer should be 5
      expect(data[data.length - 15]).toBe(5);
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

      await sender.encryptFrame(frame);
      await receiver.decryptFrame(frame, 'sender-x');

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

      await sender.encryptFrame(frame);
      await receiver.decryptFrame(frame, 'sender-z');

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
      await enc.encryptFrame(frame);

      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(40);
      // keyId in trailer should be 7
      expect(data[data.length - 15]).toBe(7);
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

      await sender.encryptFrame(frame);

      // keyId should be 1
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 15]).toBe(1);

      await receiver.decryptFrame(frame, 'alice');
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

        expect(enc.getCurrentKeyId()).toBe(1);
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
        expect(enc.getCurrentKeyId()).toBe(1);
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

      await enc.catchUpToEpoch(101);
      // Should not have advanced — gap too large
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
      await enc.encryptFrame(frame);

      enc.destroy();

      // Encrypt should fail after destroy
      const frame2 = fakeAudioFrame(40);
      await expect(enc.encryptFrame(frame2)).rejects.toThrow('no encrypt key');
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
      await sender.encryptFrame(frame);

      // Receiver should decrypt successfully
      await receiver.decryptFrame(frame, 'sender');
    });

    it('falls back to base key when epoch > 100', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      // Should not throw — falls back to addDecryptKey with epoch 0
      await enc.addDecryptKeyAtEpoch(csk, 'sender', 101);
      expect(enc.getCurrentKeyId()).toBeGreaterThanOrEqual(0);
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
    await sender.encryptFrame(frame);

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
    await sender.encryptFrame(frame);
    await receiver.decryptFrame(frame, 'sender-user-id');

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
    await sender.encryptFrame(frame);
    // 1-byte input encrypts to 33 bytes (>= the guard) — must decrypt, not pass through.
    await receiver.decryptFrame(frame, 'sender-user-id');
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
    await sender.encryptFrame(frame);

    const enc = new Uint8Array(frame.data);
    expect(enc[enc.length - 16]).toBe(1); // headerBytes field = actual header length

    await receiver.decryptFrame(frame, 'sender-user-id');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('does NOT consume an IV-counter value for a skipped empty frame (no nonce gap)', async () => {
    // Security invariant: the empty-frame early return must stay ABOVE the
    // frameCounter++ in encryptFrame, so a skipped 0-byte frame consumes no
    // GCM nonce counter. This regression-locks that ordering against a future
    // refactor that moves the guard below the counter increment.
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const empty = fakeAudioFrame(0);
    await sender.encryptFrame(empty); // must not advance the counter
    expect(empty.data.byteLength).toBe(0);

    const real = fakeAudioFrame(50);
    await sender.encryptFrame(real);
    const enc = new Uint8Array(real.data);
    // Trailer IV = bytes [-14, -2); its first 4 bytes are the big-endian counter.
    const ivStart = enc.length - 14;
    const counter =
      (enc[ivStart] << 24) | (enc[ivStart + 1] << 16) | (enc[ivStart + 2] << 8) | enc[ivStart + 3];
    expect(counter).toBe(0); // first real frame still uses counter 0
  });
});
