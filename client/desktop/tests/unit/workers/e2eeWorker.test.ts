import { beforeEach, describe, expect, it, vi } from 'vitest';

const { mockDecryptFrame } = vi.hoisted(() => ({
  mockDecryptFrame: vi.fn(),
}));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MediaEncryption: class {
    initFromKey = vi.fn();
    addDecryptKeyDirect = vi.fn();
    rotateKeys = vi.fn().mockResolvedValue(undefined);
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = mockDecryptFrame;
    destroy = vi.fn();
  },
}));

describe('e2eeWorker keyframe recovery', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let rtctransformListener: ((event: { transformer: unknown }) => void) | undefined;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    postMessage = vi.fn();
    rtctransformListener = undefined;
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn((event: string, listener: EventListener) => {
        if (event === 'rtctransform') {
          rtctransformListener = listener as unknown as (event: { transformer: unknown }) => void;
        }
      }),
    });
    await import('@/renderer/workers/e2eeWorker');
  });

  it('posts requestKeyframe when video decrypt recovers after dropped frames', async () => {
    const dropped = { type: 'key', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const recovered = { type: 'delta', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];

    mockDecryptFrame
      .mockRejectedValueOnce(new DOMException('', 'OperationError'))
      .mockResolvedValueOnce(undefined);

    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(dropped);
        controller.enqueue(recovered);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() => {
      expect(postMessage).toHaveBeenCalledWith({
        type: 'requestKeyframe',
        senderUserId: 'sender-1',
      });
    });
    expect(written).toEqual([recovered]);
  });
});
