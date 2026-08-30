import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';

const useRenderStateReporterMock = vi.fn();
vi.mock('../../../src/renderer/hooks/voice/useRenderStateReporter', () => ({
  useRenderStateReporter: (opts: unknown) => useRenderStateReporterMock(opts),
}));

import { useScreenTileVideo } from '@/renderer/hooks/voice/useScreenTileVideo';

function Harness({
  sharerUserId,
  stream,
  isPaused,
}: {
  sharerUserId?: string;
  stream?: MediaStream;
  isPaused?: boolean;
}) {
  const videoRef = useScreenTileVideo({ sharerUserId, stream, isPaused, role: 'focus' });
  return <video ref={videoRef} data-testid="tile-video" />;
}

describe('useScreenTileVideo', () => {
  let playSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    playSpy = vi.fn().mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockImplementation(playSpy);
    useRenderStateReporterMock.mockClear();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it('reports enabled=true with the sharer id when sharer + stream present and not paused', () => {
    const stream = {} as MediaStream;
    render(<Harness sharerUserId="user-1" stream={stream} isPaused={false} />);

    expect(useRenderStateReporterMock).toHaveBeenCalledWith(
      expect.objectContaining({
        userId: 'user-1',
        source: 'screen',
        role: 'focus',
        enabled: true,
      })
    );
  });

  it('reports enabled=false and userId="" when sharerUserId is undefined', () => {
    const stream = {} as MediaStream;
    render(<Harness stream={stream} isPaused={false} />);

    expect(useRenderStateReporterMock).toHaveBeenCalledWith(
      expect.objectContaining({ userId: '', enabled: false })
    );
  });

  it('reports enabled=false when stream is undefined', () => {
    render(<Harness sharerUserId="user-1" isPaused={false} />);

    expect(useRenderStateReporterMock).toHaveBeenCalledWith(
      expect.objectContaining({ userId: 'user-1', enabled: false })
    );
  });

  it('reports enabled=false when isPaused is true', () => {
    const stream = {} as MediaStream;
    render(<Harness sharerUserId="user-1" stream={stream} isPaused={true} />);

    expect(useRenderStateReporterMock).toHaveBeenCalledWith(
      expect.objectContaining({ userId: 'user-1', enabled: false })
    );
  });

  it('attaches the stream to the video element and calls play()', async () => {
    const stream = {} as MediaStream;
    const { getByTestId } = render(<Harness sharerUserId="user-1" stream={stream} />);

    const video = getByTestId('tile-video') as HTMLVideoElement;
    expect(video.srcObject).toBe(stream);
    expect(playSpy).toHaveBeenCalledTimes(1);
  });

  it('swallows a play() rejection (autoplay block) without an unhandled rejection', async () => {
    playSpy.mockRejectedValueOnce(new Error('NotAllowedError'));
    const stream = {} as MediaStream;
    const { getByTestId } = render(<Harness sharerUserId="user-1" stream={stream} />);

    const video = getByTestId('tile-video') as HTMLVideoElement;
    expect(video.srcObject).toBe(stream);
    // Flush the rejected promise; the hook's .catch must absorb it.
    await Promise.resolve();
    await Promise.resolve();
    expect(playSpy).toHaveBeenCalledTimes(1);
  });

  it('nulls srcObject when the stream becomes undefined on rerender', () => {
    const stream = {} as MediaStream;
    const { getByTestId, rerender } = render(<Harness sharerUserId="user-1" stream={stream} />);
    const video = getByTestId('tile-video') as HTMLVideoElement;
    expect(video.srcObject).toBe(stream);

    rerender(<Harness sharerUserId="user-1" />);

    expect(video.srcObject).toBeNull();
  });

  it('nulls srcObject on unmount', () => {
    const stream = {} as MediaStream;
    const { getByTestId, unmount } = render(<Harness sharerUserId="user-1" stream={stream} />);
    const video = getByTestId('tile-video') as HTMLVideoElement;
    expect(video.srcObject).toBe(stream);

    unmount();

    expect(video.srcObject).toBeNull();
  });
});
