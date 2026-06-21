import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { MediaEncryption } from '../../../src/renderer/services/mediaEncryption';
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
  };
}

type TransformFn = (
  frame: { data: ArrayBuffer },
  controller: { enqueue: ReturnType<typeof vi.fn> }
) => Promise<void>;

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

// ─── Tests ───────────────────────────────────────────────────────────

describe('applyLegacyDecryptPipeline', () => {
  const origTransformStream = globalThis.TransformStream;
  let capturedTransformFn: TransformFn | null = null;

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

  it('warns when createEncodedStreams is not available', () => {
    const receiver: InsertableStreamsReceiver = {};
    applyLegacyDecryptPipeline(receiver, 'user-1', mockEncryption(), mockCallbacks(), false);
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

  it('handles createEncodedStreams throwing', () => {
    const receiver = {
      createEncodedStreams: vi.fn().mockImplementation(() => {
        throw new Error('API not ready');
      }),
    };

    applyLegacyDecryptPipeline(receiver, 'user-1', mockEncryption(), mockCallbacks(), false);

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
    await capturedTransformFn!(frame, controller);

    expect(encryption.decryptFrame).toHaveBeenCalledWith(frame, 'user-1');
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
    await capturedTransformFn!(frame, controller);

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
      await capturedTransformFn!(frame, controller);
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
    await capturedTransformFn!(frame, controller);

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
      await capturedTransformFn!(frame, controller);
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
      await capturedTransformFn!(frame, controller);
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
      await capturedTransformFn!(frame, controller);
    }

    expect(console.debug).toHaveBeenCalledWith(
      expect.stringContaining('E2EE: decrypt recovered after 3 dropped frames')
    );
    expect(controller.enqueue).toHaveBeenCalledTimes(1);
  });
});
