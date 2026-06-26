// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { MediaEncryption } from '../../../src/renderer/services/mediaEncryption';
import {
  FrameKeyMissError,
  MediaEncryption as MediaEncryptionClass,
} from '../../../src/renderer/services/mediaEncryption';
import {
  applyLegacyDecryptPipeline,
  type DecryptRecoveryCallbacks,
  type InsertableStreamsReceiver,
} from '../../../src/renderer/services/voiceE2eeTransforms';

// ─── Mock Helpers ────────────────────────────────────────────────────

function mockEncryption(opts?: { shouldFail?: boolean }): MediaEncryption {
  return {
    decryptFrame: opts?.shouldFail
      ? vi.fn().mockRejectedValue(new Error('decrypt failed'))
      : vi.fn().mockResolvedValue(undefined),
    getCurrentKeyId: vi.fn().mockReturnValue(0),
  } as unknown as MediaEncryption;
}

function mockCallbacks(): DecryptRecoveryCallbacks {
  return {
    getActiveChannelId: vi.fn().mockReturnValue('channel-1'),
    addDecryptKeyForUser: vi.fn().mockResolvedValue(true),
    invalidateChannelKey: vi.fn(),
    requestKeyframe: vi.fn(),
  };
}

type TransformFn = (
  frame: { data: ArrayBuffer; type?: string },
  controller: { enqueue: ReturnType<typeof vi.fn> }
) => Promise<void>;

let capturedTransformFn: TransformFn | null = null;

function mockReceiver(): InsertableStreamsReceiver {
  const mockReadable = {
    pipeThrough: vi.fn().mockReturnValue({ pipeTo: vi.fn().mockResolvedValue(undefined) }),
  };
  return {
    createEncodedStreams: vi.fn().mockReturnValue({
      readable: mockReadable,
      writable: new WritableStream(),
    }),
  };
}

function requireCapturedTransformFn(): TransformFn {
  if (!capturedTransformFn) throw new Error('decrypt transform was not captured');
  return capturedTransformFn;
}

// ─── Tests ───────────────────────────────────────────────────────────

describe('applyLegacyDecryptPipeline', () => {
  const origTransformStream = globalThis.TransformStream;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
    capturedTransformFn = null;

    // Mock TransformStream to capture the transform callback
    globalThis.TransformStream = class MockTransformStream {
      readable = new ReadableStream();
      writable = new WritableStream();
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      constructor(opts: any) {
        capturedTransformFn = opts.transform;
      }
    } as typeof TransformStream;
  });

  afterEach(() => {
    globalThis.TransformStream = origTransformStream;
    vi.restoreAllMocks();
  });

  it('throws when createEncodedStreams is not available', () => {
    const receiver: InsertableStreamsReceiver = {};
    expect(() =>
      applyLegacyDecryptPipeline(receiver, 'user-1', mockEncryption(), mockCallbacks(), false)
    ).toThrow('no Insertable Streams API available');
    expect(console.warn).toHaveBeenCalledWith(
      'E2EE: no Insertable Streams API available — frames will not be decrypted'
    );
  });

  it('creates a transform pipeline when createEncodedStreams is available', () => {
    // Restore real TransformStream for this test (no need to capture)
    globalThis.TransformStream = origTransformStream;
    const mockWritable = new WritableStream();
    const mockReadable = new ReadableStream();
    const receiver = {
      createEncodedStreams: vi.fn().mockReturnValue({
        readable: mockReadable,
        writable: mockWritable,
      }),
    };

    applyLegacyDecryptPipeline(receiver, 'user-1', mockEncryption(), mockCallbacks(), false);

    expect(receiver.createEncodedStreams).toHaveBeenCalled();
    expect(console.debug).toHaveBeenCalledWith(
      'E2EE: decrypt transform applied for user-1 (createEncodedStreams)'
    );
  });

  it('throws when createEncodedStreams throws', () => {
    const receiver = {
      createEncodedStreams: vi.fn().mockImplementation(() => {
        throw new Error('API not ready');
      }),
    };

    expect(() =>
      applyLegacyDecryptPipeline(receiver, 'user-1', mockEncryption(), mockCallbacks(), false)
    ).toThrow('createEncodedStreams failed on receiver');

    expect(console.error).toHaveBeenCalledWith(
      'E2EE: createEncodedStreams failed on receiver:',
      expect.any(String)
    );
  });

  it('decrypts frames successfully through the transform', async () => {
    const encryption = mockEncryption();
    applyLegacyDecryptPipeline(mockReceiver(), 'user-1', encryption, mockCallbacks(), false);

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };
    await requireCapturedTransformFn()(frame, controller);

    expect(encryption.decryptFrame).toHaveBeenCalledWith(frame, 'user-1', undefined);
    expect(controller.enqueue).toHaveBeenCalledWith(frame);
  });

  it('drops frames and logs on decrypt failure', async () => {
    applyLegacyDecryptPipeline(
      mockReceiver(),
      'user-1',
      mockEncryption({ shouldFail: true }),
      mockCallbacks(),
      false
    );

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };
    await requireCapturedTransformFn()(frame, controller);

    expect(controller.enqueue).not.toHaveBeenCalled();
    expect(console.warn).toHaveBeenCalledWith(
      expect.stringContaining('E2EE: dropping undecryptable frame for sender'),
      'user-1',
      expect.any(Number),
      expect.any(Object)
    );
  });

  it('triggers self-healing recovery after threshold drops', async () => {
    const callbacks = mockCallbacks();
    applyLegacyDecryptPipeline(
      mockReceiver(),
      'user-1',
      mockEncryption({ shouldFail: true }),
      callbacks,
      false
    );

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };

    for (let i = 0; i < 50; i++) {
      await requireCapturedTransformFn()(frame, controller);
    }

    expect(callbacks.invalidateChannelKey).toHaveBeenCalledWith('channel-1');
    expect(callbacks.addDecryptKeyForUser).toHaveBeenCalledWith('channel-1', 'user-1');
  });

  it('logs verbose frame diagnostics when verbose=true', async () => {
    applyLegacyDecryptPipeline(
      mockReceiver(),
      'user-1',
      mockEncryption({ shouldFail: true }),
      mockCallbacks(),
      true
    );

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };
    await requireCapturedTransformFn()(frame, controller);

    expect(console.warn).toHaveBeenCalledWith(
      expect.stringContaining('E2EE: dropping frame for sender'),
      'user-1',
      expect.any(Number),
      expect.objectContaining({ trailerHex: expect.any(String), hasMagic: expect.any(Boolean) })
    );
  });

  it('logs persistent failure at 500 drops', async () => {
    applyLegacyDecryptPipeline(
      mockReceiver(),
      'user-1',
      mockEncryption({ shouldFail: true }),
      mockCallbacks(),
      false
    );

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };

    for (let i = 0; i < 500; i++) {
      await requireCapturedTransformFn()(frame, controller);
    }

    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('E2EE: persistent decrypt failure for user-1')
    );
  });

  it('skips recovery when no active channel', async () => {
    const callbacks = mockCallbacks();
    (callbacks.getActiveChannelId as ReturnType<typeof vi.fn>).mockReturnValue(null);
    applyLegacyDecryptPipeline(
      mockReceiver(),
      'user-1',
      mockEncryption({ shouldFail: true }),
      callbacks,
      false
    );

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };

    for (let i = 0; i < 50; i++) {
      await requireCapturedTransformFn()(frame, controller);
    }

    expect(callbacks.addDecryptKeyForUser).not.toHaveBeenCalled();
  });

  it('logs redacted error when the decrypt pipeline rejects', async () => {
    // Make pipeTo reject so the .catch((err) => console.warn(...)) path fires
    const rejectingReceiver: InsertableStreamsReceiver = {
      createEncodedStreams: vi.fn().mockReturnValue({
        readable: {
          pipeThrough: vi.fn().mockReturnValue({
            pipeTo: vi.fn().mockRejectedValue(new Error('boom')),
          }),
        },
        writable: new WritableStream(),
      }),
    };
    applyLegacyDecryptPipeline(
      rejectingReceiver,
      'user-1',
      mockEncryption(),
      mockCallbacks(),
      false
    );
    // .catch fires asynchronously after the pipeline rejects
    await vi.waitFor(() => {
      expect(console.warn).toHaveBeenCalledWith('E2EE decrypt pipe error:', 'boom');
    });
  });

  it('logs recovery on successful decrypt after failures', async () => {
    let failCount = 0;
    const encryption = {
      decryptFrame: vi.fn().mockImplementation(() => {
        failCount++;
        if (failCount <= 3) throw new Error('key mismatch');
        return Promise.resolve();
      }),
      getCurrentKeyId: vi.fn().mockReturnValue(0),
    } as unknown as MediaEncryption;

    applyLegacyDecryptPipeline(mockReceiver(), 'user-1', encryption, mockCallbacks(), false);

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100) };
    const controller = { enqueue: vi.fn() };

    for (let i = 0; i < 4; i++) {
      await requireCapturedTransformFn()(frame, controller);
    }

    expect(console.debug).toHaveBeenCalledWith(
      expect.stringContaining('E2EE: decrypt recovered after 3 dropped frames')
    );
    expect(controller.enqueue).toHaveBeenCalledTimes(1);
  });

  it('requests a fresh keyframe when video decrypt recovers after dropped frames', async () => {
    let failCount = 0;
    const encryption = {
      decryptFrame: vi.fn().mockImplementation(() => {
        failCount++;
        if (failCount <= 3) throw new Error('key mismatch');
        return Promise.resolve();
      }),
      getCurrentKeyId: vi.fn().mockReturnValue(0),
    } as unknown as MediaEncryption;
    const callbacks = mockCallbacks();

    applyLegacyDecryptPipeline(mockReceiver(), 'user-1', encryption, callbacks, false);

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100), type: 'delta' };
    const controller = { enqueue: vi.fn() };

    for (let i = 0; i < 4; i++) {
      await requireCapturedTransformFn()(frame, controller);
    }

    expect(callbacks.requestKeyframe).toHaveBeenCalledWith('user-1');
    expect(callbacks.requestKeyframe).toHaveBeenCalledTimes(1);
  });

  // ─── #1895 regression: v3 key-version provisioning on the legacy path ──

  it('provisions the exact key version on a typed FrameKeyMiss (regression #1895)', async () => {
    // The legacy createEncodedStreams decrypt path is the path all current
    // Electron builds run (USE_SCRIPT_TRANSFORM=false, #295). Under mid-session
    // CSK rotation, decryptFrame throws a typed FrameKeyMissError carrying the
    // frame's (senderUserId, keyVersion, keyId). The Worker path routes that to
    // on-demand provisioning (e2eeWorker.ts requestFrameKeyOnce); the legacy
    // path must do the same via a requestFrameKey callback — otherwise it
    // black-screens video (#1878/#1885 residual: v3 provisioning was wired into
    // the Worker decrypt path only, never applyLegacyDecryptPipeline).
    const encryption = {
      decryptFrame: vi
        .fn()
        .mockRejectedValue(
          new FrameKeyMissError(
            'user-1',
            5,
            0,
            'E2EE: no decrypt key for sender=user-1 v=5 keyId=0'
          )
        ),
      getCurrentKeyId: vi.fn().mockReturnValue(0),
    } as unknown as MediaEncryption;

    // requestFrameKey is supplied via an intersection cast so this repro needs
    // no production change to compile: it fails today because the pipeline catch
    // block never calls it (Phase-1 RED), and turns green once the fix wires the
    // typed-miss branch onto the callback.
    const requestFrameKey = vi.fn();
    const callbacks = {
      ...mockCallbacks(),
      requestFrameKey,
    } as DecryptRecoveryCallbacks & { requestFrameKey: typeof requestFrameKey };

    applyLegacyDecryptPipeline(mockReceiver(), 'user-1', encryption, callbacks, false);

    expect(capturedTransformFn).toBeTypeOf('function');
    const frame = { data: new ArrayBuffer(100), type: 'delta' };
    const controller = { enqueue: vi.fn() };
    await requireCapturedTransformFn()(frame, controller);

    // Fail-closed: the undecryptable frame is dropped, never enqueued as ciphertext.
    expect(controller.enqueue).not.toHaveBeenCalled();
    // The fix: the exact (keyVersion, keyId) is requested on the FIRST typed
    // miss — not after 50 version-blind self-heal drops, and not never.
    expect(requestFrameKey).toHaveBeenCalledWith('user-1', 5, 0);
  });
});

// ─── #1885 regression lock: legacy path dispatches by codec ──────────────────
//
// fakeReceiver drives applyLegacyDecryptPipeline with real Node streams (no
// TransformStream mock) and real MediaEncryption so the decrypt path is
// exercised end-to-end — not through a captured callback. This is the
// regression lock for #1885: wiring v3/v4 provisioning into the Worker path
// only, never applyLegacyDecryptPipeline, caused black-screen video for all
// current Electron builds (USE_SCRIPT_TRANSFORM=false, #295).

/** Identity-streams fake receiver: readable yields the queued frames; writable collects them. */
function fakeReceiver(
  frames: Array<{ data: ArrayBuffer; type?: string }>,
  sink: Array<{ data: ArrayBuffer }>
) {
  return {
    createEncodedStreams() {
      const readable = new ReadableStream({
        start(controller) {
          for (const f of frames) controller.enqueue(f);
          controller.close();
        },
      });
      const writable = new WritableStream({
        write(chunk: { data: ArrayBuffer }) {
          sink.push(chunk);
        },
      });
      return { readable, writable };
    },
  };
}

function makeCallbacks(): DecryptRecoveryCallbacks {
  return {
    getActiveChannelId: () => 'chan',
    addDecryptKeyForUser: async () => true,
    invalidateChannelKey: () => {},
    requestKeyframe: () => {},
    requestFrameKey: () => {},
  };
}

describe('applyLegacyDecryptPipeline — both schemes wired (#1885 lock)', () => {
  it('decrypts a VP9 v4 whole-frame on the legacy path', async () => {
    const key = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
      'encrypt',
      'decrypt',
    ]);
    const sender = new MediaEncryptionClass();
    const receiver = new MediaEncryptionClass();
    await sender.init(key, 'sender');
    await receiver.init(key, 'me');
    await receiver.addDecryptKey(key, 'sender');

    const frame = { data: new Uint8Array([10, 20, 30, 40, 50]).buffer, type: 'delta' };
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame as never, 'vp9');

    const sink: Array<{ data: ArrayBuffer }> = [];
    applyLegacyDecryptPipeline(
      fakeReceiver([frame], sink) as never,
      'sender',
      receiver,
      makeCallbacks(),
      false,
      'vp9'
    );
    await new Promise((r) => setTimeout(r, 20));
    expect(new Uint8Array(sink[0].data)).toEqual(original);
  });

  it('decrypts an AV1 v4 per-OBU frame on the legacy path (regression lock)', async () => {
    const key = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
      'encrypt',
      'decrypt',
    ]);
    const sender = new MediaEncryptionClass();
    const receiver = new MediaEncryptionClass();
    await sender.init(key, 'sender');
    await receiver.init(key, 'me');
    await receiver.addDecryptKey(key, 'sender');

    // Build a minimal valid AV1 temporal unit (TD + FRAME).
    const frameData = new Uint8Array([
      (2 << 3) | 0b10,
      0, // TD, size 0
      (6 << 3) | 0b10,
      4,
      1,
      2,
      3,
      4, // FRAME, size 4, payload [1,2,3,4]
    ]);
    const frame = { data: frameData.buffer, type: 'key' };
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame as never, 'av1');

    const sink: Array<{ data: ArrayBuffer }> = [];
    applyLegacyDecryptPipeline(
      fakeReceiver([frame], sink) as never,
      'sender',
      receiver,
      makeCallbacks(),
      false,
      'av1'
    );
    await new Promise((r) => setTimeout(r, 20));
    expect(new Uint8Array(sink[0].data)).toEqual(original); // black-screen regression lock
  });
});

// ─── #1895 diagnostic cleanup guard ──────────────────────────────────────────
//
// Asserts that all temporary #1895 diagnostic symbols have been removed from
// voiceE2eeTransforms.ts and voiceService.ts. These tests fail when any
// diagnostic code is still present, preventing accidental shipping of
// pass-through code that disables video E2EE (AV1_PASSTHROUGH_DIAG=true).

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

describe('#1895 diagnostics removed (rig out of pass-through)', () => {
  it('voiceE2eeTransforms.ts has no AV1_PASSTHROUGH_DIAG / parseAv1Obus / fnv1a32Hex', () => {
    const src = readFileSync(
      resolve(__dirname, '../../../src/renderer/services/voiceE2eeTransforms.ts'),
      'utf8'
    );
    expect(src).not.toMatch(/AV1_PASSTHROUGH_DIAG/);
    expect(src).not.toMatch(/\bfnv1a32Hex\b/);
    expect(src).not.toMatch(/export function parseAv1Obus/);
  });
  it('voiceService.ts has E2EE_VERBOSE=false and no passthrough diagnostic', () => {
    const src = readFileSync(
      resolve(__dirname, '../../../src/renderer/services/voiceService.ts'),
      'utf8'
    );
    expect(src).toMatch(/const E2EE_VERBOSE = false/);
    expect(src).not.toMatch(/AV1_PASSTHROUGH_DIAG/);
    expect(src).not.toMatch(/AV1-PAYLOAD SEND/);
  });
});
