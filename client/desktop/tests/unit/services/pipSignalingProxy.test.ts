import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { MockBroadcastChannel } from '../../helpers/broadcastChannelMock';
import { resetAllStores } from '../../helpers/store-helpers';

// ── Mocks (must be before imports) ──────────────────────────────────────

const mockVoiceService = {
  forwardToServer: vi.fn().mockResolvedValue({}),
  getRouterRtpCapabilities: vi.fn().mockReturnValue({
    codecs: [{ mimeType: 'audio/opus', kind: 'audio', clockRate: 48000 }],
  }),
  getConsumerIdsBySource: vi.fn().mockReturnValue([]),
  getConsumerMeta: vi.fn().mockReturnValue(new Map()),
  pauseConsumer: vi.fn(),
  resumeConsumer: vi.fn(),
  toggleMute: vi.fn().mockResolvedValue(undefined),
  toggleDeafen: vi.fn(),
  toggleVideo: vi.fn().mockResolvedValue(undefined),
  toggleScreenShare: vi.fn().mockResolvedValue(undefined),
  leaveChannel: vi.fn().mockResolvedValue(undefined),
};

vi.mock('mediasoup-client/types', () => ({}));

// ── Import after mocks ──────────────────────────────────────────────────

import { PipSignalingProxy } from '@/renderer/services/pipSignalingProxy';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';

// ── Helpers ─────────────────────────────────────────────────────────────

function getChannel(): MockBroadcastChannel {
  const ch = MockBroadcastChannel.latest;
  if (!ch) throw new Error('No MockBroadcastChannel instance');
  return ch;
}

/** Send an RPC request and wait for response */
async function sendRpc(
  channel: MockBroadcastChannel,
  method: string,
  params: unknown = {},
  pipId = 'test-pip'
): Promise<unknown> {
  const id = `req-${Date.now()}-${Math.random()}`;

  return new Promise((resolve, reject) => {
    const posted = channel.posted.length;

    channel.simulateMessage({
      kind: 'rpc-request',
      id,
      pipId,
      method,
      params,
    });

    // Wait for the response to appear in posted messages
    const check = () => {
      for (let i = posted; i < channel.posted.length; i++) {
        const msg = channel.posted[i] as { kind?: string; id?: string };
        if (msg.kind === 'rpc-response' && msg.id === id) {
          const resp = msg as { result?: unknown; error?: string };
          if (resp.error) reject(new Error(resp.error));
          else resolve(resp.result);
          return;
        }
      }
      setTimeout(check, 5);
    };
    check();
  });
}

// ── Tests ───────────────────────────────────────────────────────────────

describe('PipSignalingProxy', () => {
  let proxy: PipSignalingProxy;

  beforeEach(() => {
    MockBroadcastChannel.install();
    resetAllStores();
    vi.clearAllMocks();

    useUserStore.setState({ user: { id: 'local-user-123' } as any });

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    proxy = new PipSignalingProxy(mockVoiceService as any);
  });

  afterEach(() => {
    proxy.dispose();
    MockBroadcastChannel.uninstall();
  });

  // ── Constructor & Lifecycle ─────────────────────────────────────────

  describe('constructor & lifecycle', () => {
    it('creates BroadcastChannel with name concord-pip', () => {
      const ch = getChannel();
      expect(ch.name).toBe('concord-pip');
    });

    it('subscribes to voice store changes and broadcasts state updates', async () => {
      const ch = getChannel();

      // Update participants in voice store — should trigger broadcast
      useVoiceStore.setState({
        participants: {
          'user-1': {
            userId: 'user-1',
            username: 'alice',
            isMuted: false,
            isDeafened: false,
            isVideoOn: false,
            isScreenSharing: false,
            isSpeaking: false,
          } as any,
        },
        tunedInScreenShares: {},
      });

      // Wait for subscription to fire
      await vi.waitFor(() => {
        const broadcasts = ch.posted.filter(
          (m: any) => m.kind === 'broadcast' && m.type === 'state-update'
        );
        expect(broadcasts.length).toBeGreaterThan(0);
      });

      const broadcast = ch.posted.find(
        (m: any) => m.kind === 'broadcast' && m.type === 'state-update'
      ) as any;
      expect(broadcast.participants['user-1'].username).toBe('alice');
      expect(broadcast.localUserId).toBe('local-user-123');
    });

    it('dispose() broadcasts voice-ended', () => {
      const ch = getChannel();
      const prevLength = ch.posted.length;
      proxy.dispose();

      const voiceEnded = ch.posted
        .slice(prevLength)
        .find((m: any) => m.kind === 'broadcast' && m.type === 'voice-ended');
      expect(voiceEnded).toBeDefined();
    });
  });

  // ── handleRequestState ──────────────────────────────────────────────

  describe('handleRequestState', () => {
    it('returns sanitized participants, rtpCapabilities, and localUserId', async () => {
      useVoiceStore.setState({
        participants: {
          'user-1': {
            userId: 'user-1',
            username: 'alice',
            isMuted: true,
            isDeafened: false,
            isVideoOn: false,
            isScreenSharing: false,
            isSpeaking: false,
            audioStream: {} as any, // Should be stripped
            videoStream: {} as any, // Should be stripped
          } as any,
        },
        tunedInScreenShares: { 'prod-1': 'cons-1' },
      });

      const result = (await sendRpc(getChannel(), 'request-state')) as any;

      expect(result.localUserId).toBe('local-user-123');
      expect(result.participants['user-1'].username).toBe('alice');
      expect(result.participants['user-1'].isMuted).toBe(true);
      // MediaStream objects should be stripped
      expect(result.participants['user-1'].audioStream).toBeUndefined();
      expect(result.participants['user-1'].videoStream).toBeUndefined();
      expect(result.routerRtpCapabilities).toBeDefined();
      expect(result.tunedInScreenShares).toEqual({ 'prod-1': 'cons-1' });
    });

    it('deduplicates active producers by source+userId', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([
          ['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }],
          ['c2', { source: 'mic', producerUserId: 'u1', producerId: 'p1-dup' }],
          ['c3', { source: 'camera', producerUserId: 'u1', producerId: 'p2' }],
        ])
      );

      const result = (await sendRpc(getChannel(), 'request-state')) as any;

      // mic-u1 should only appear once despite two consumer entries
      const micProducers = result.activeProducers.filter(
        (p: any) => p.source === 'mic' && p.userId === 'u1'
      );
      expect(micProducers).toHaveLength(1);
      expect(result.activeProducers).toHaveLength(2); // mic + camera
    });

    it('handles empty consumerMeta gracefully', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(new Map());

      const result = (await sendRpc(getChannel(), 'request-state')) as any;
      expect(result.activeProducers).toEqual([]);
    });
  });

  // ── Transport & Consume handlers ────────────────────────────────────

  describe('transport & consume handlers', () => {
    it('create-recv-transport forwards to server with direction recv', async () => {
      mockVoiceService.forwardToServer.mockResolvedValueOnce({
        transportId: 'tp-1',
        iceParameters: {},
        iceCandidates: [],
        dtlsParameters: {},
      });

      const result = (await sendRpc(getChannel(), 'create-recv-transport', {
        forceTcp: true,
      })) as any;

      expect(mockVoiceService.forwardToServer).toHaveBeenCalledWith('create-transport', {
        direction: 'recv',
        forceTcp: true,
      });
      expect(result.transportId).toBe('tp-1');
    });

    it('connect-transport forwards transportId and dtlsParameters', async () => {
      mockVoiceService.forwardToServer.mockResolvedValueOnce(undefined);

      await sendRpc(getChannel(), 'connect-transport', {
        transportId: 'tp-1',
        dtlsParameters: { role: 'client' },
      });

      expect(mockVoiceService.forwardToServer).toHaveBeenCalledWith('connect-transport', {
        transportId: 'tp-1',
        dtlsParameters: { role: 'client' },
      });
    });

    it('consume forwards producerId, rtpCapabilities, and transportId', async () => {
      mockVoiceService.forwardToServer.mockResolvedValueOnce({
        consumerId: 'c1',
        producerId: 'p1',
        kind: 'audio',
        rtpParameters: {},
      });

      const result = (await sendRpc(getChannel(), 'consume', {
        producerId: 'p1',
        transportId: 'tp-1',
        rtpCapabilities: { codecs: [] },
      })) as any;

      expect(result.consumerId).toBe('c1');
    });

    it('resume-consumer forwards consumerId', async () => {
      mockVoiceService.forwardToServer.mockResolvedValueOnce(undefined);

      await sendRpc(getChannel(), 'resume-consumer', { consumerId: 'c1' });

      expect(mockVoiceService.forwardToServer).toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'c1',
      });
    });

    it('pause-consumer forwards consumerId', async () => {
      mockVoiceService.forwardToServer.mockResolvedValueOnce(undefined);

      await sendRpc(getChannel(), 'pause-consumer', { consumerId: 'c1' });

      expect(mockVoiceService.forwardToServer).toHaveBeenCalledWith('pause-consumer', {
        consumerId: 'c1',
      });
    });
  });

  // ── handleAction ────────────────────────────────────────────────────

  describe('handleAction', () => {
    it('toggle-mute calls voiceService.toggleMute()', async () => {
      await sendRpc(getChannel(), 'action', { action: 'toggle-mute' });
      expect(mockVoiceService.toggleMute).toHaveBeenCalled();
    });

    it('toggle-deafen calls voiceService.toggleDeafen()', async () => {
      await sendRpc(getChannel(), 'action', { action: 'toggle-deafen' });
      expect(mockVoiceService.toggleDeafen).toHaveBeenCalled();
    });

    it('toggle-video calls voiceService.toggleVideo()', async () => {
      await sendRpc(getChannel(), 'action', { action: 'toggle-video' });
      expect(mockVoiceService.toggleVideo).toHaveBeenCalled();
    });

    it('toggle-screen calls voiceService.toggleScreenShare()', async () => {
      await sendRpc(getChannel(), 'action', { action: 'toggle-screen' });
      expect(mockVoiceService.toggleScreenShare).toHaveBeenCalled();
    });

    it('leave calls voiceService.leaveChannel()', async () => {
      await sendRpc(getChannel(), 'action', { action: 'leave' });
      expect(mockVoiceService.leaveChannel).toHaveBeenCalled();
    });

    it('unknown action responds with error', async () => {
      await expect(sendRpc(getChannel(), 'action', { action: 'fly' })).rejects.toThrow(
        'Unknown action: fly'
      );
    });
  });

  // ── handlePipReady & handlePipClosing ───────────────────────────────

  describe('pip-ready & pip-closing', () => {
    it('pip-ready pauses matching main window consumers', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([
          ['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }],
          ['c2', { source: 'camera', producerUserId: 'u1', producerId: 'p2' }],
          ['c3', { source: 'mic', producerUserId: 'u2', producerId: 'p3' }],
        ])
      );

      await sendRpc(
        getChannel(),
        'pip-ready',
        {
          consumerSources: [
            { source: 'mic', producerUserId: 'u1' },
            { source: 'camera', producerUserId: 'u1' },
          ],
        },
        'frames-1'
      );

      expect(mockVoiceService.pauseConsumer).toHaveBeenCalledWith('c1');
      expect(mockVoiceService.pauseConsumer).toHaveBeenCalledWith('c2');
      expect(mockVoiceService.pauseConsumer).not.toHaveBeenCalledWith('c3');
    });

    it('pip-ready broadcasts ownership-transferred', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }]])
      );

      await sendRpc(
        getChannel(),
        'pip-ready',
        { consumerSources: [{ source: 'mic', producerUserId: 'u1' }] },
        'frames-1'
      );

      const ch = getChannel();
      const transferred = ch.posted.find(
        (m: any) => m.kind === 'broadcast' && m.type === 'ownership-transferred'
      ) as any;
      expect(transferred).toBeDefined();
      expect(transferred.pipId).toBe('frames-1');
      expect(transferred.pausedConsumerIds).toContain('c1');
    });

    it('pip-closing resumes previously paused consumers', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }]])
      );

      // First pip-ready to pause
      await sendRpc(
        getChannel(),
        'pip-ready',
        { consumerSources: [{ source: 'mic', producerUserId: 'u1' }] },
        'frames-1'
      );

      vi.clearAllMocks();

      // Then pip-closing to resume
      await sendRpc(getChannel(), 'pip-closing', {}, 'frames-1');
      expect(mockVoiceService.resumeConsumer).toHaveBeenCalledWith('c1');
    });

    it('onPipClosed resumes consumers even without pip-closing RPC', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }]])
      );

      await sendRpc(
        getChannel(),
        'pip-ready',
        { consumerSources: [{ source: 'mic', producerUserId: 'u1' }] },
        'frames-1'
      );

      vi.clearAllMocks();

      // Simulate abnormal close via IPC
      proxy.onPipClosed('frames-1');
      expect(mockVoiceService.resumeConsumer).toHaveBeenCalledWith('c1');
    });

    it('dispose() resumes all paused consumers across all PiPs', async () => {
      mockVoiceService.getConsumerMeta.mockReturnValue(
        new Map([['c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' }]])
      );

      await sendRpc(
        getChannel(),
        'pip-ready',
        { consumerSources: [{ source: 'mic', producerUserId: 'u1' }] },
        'frames-1'
      );

      vi.clearAllMocks();
      proxy.dispose();
      expect(mockVoiceService.resumeConsumer).toHaveBeenCalledWith('c1');
    });
  });

  // ── Broadcasts ──────────────────────────────────────────────────────

  describe('broadcasts', () => {
    it('broadcastStateUpdate strips MediaStream fields', () => {
      const ch = getChannel();
      const prevLength = ch.posted.length;

      proxy.broadcastStateUpdate(
        {
          u1: {
            userId: 'u1',
            username: 'alice',
            audioStream: new Object() as any,
            videoStream: new Object() as any,
            screenStream: new Object() as any,
            screenAudioStream: new Object() as any,
            isMuted: false,
          },
        },
        {}
      );

      const broadcast = ch.posted
        .slice(prevLength)
        .find((m: any) => m.kind === 'broadcast' && m.type === 'state-update') as any;

      expect(broadcast).toBeDefined();
      expect(broadcast.participants['u1'].username).toBe('alice');
      expect(broadcast.participants['u1'].audioStream).toBeUndefined();
      expect(broadcast.participants['u1'].videoStream).toBeUndefined();
      expect(broadcast.participants['u1'].screenStream).toBeUndefined();
      expect(broadcast.participants['u1'].screenAudioStream).toBeUndefined();
    });

    it('broadcastProducerAdded posts correct data', () => {
      const ch = getChannel();
      const prevLength = ch.posted.length;

      proxy.broadcastProducerAdded('p1', 'u1', 'camera');

      const broadcast = ch.posted
        .slice(prevLength)
        .find((m: any) => m.kind === 'broadcast' && m.type === 'producer-added') as any;
      expect(broadcast).toBeDefined();
      expect(broadcast.producerId).toBe('p1');
      expect(broadcast.userId).toBe('u1');
      expect(broadcast.source).toBe('camera');
    });

    it('broadcastProducerClosed posts correct data', () => {
      const ch = getChannel();
      const prevLength = ch.posted.length;

      proxy.broadcastProducerClosed('p1', 'u1');

      const broadcast = ch.posted
        .slice(prevLength)
        .find((m: any) => m.kind === 'broadcast' && m.type === 'producer-closed') as any;
      expect(broadcast).toBeDefined();
      expect(broadcast.producerId).toBe('p1');
      expect(broadcast.userId).toBe('u1');
    });

    it('does not broadcast after dispose', () => {
      const ch = getChannel();
      proxy.dispose();
      const lengthAfterDispose = ch.posted.length;

      proxy.broadcastProducerAdded('p1', 'u1', 'mic');

      // No new broadcasts should have been posted after dispose
      const newBroadcasts = ch.posted
        .slice(lengthAfterDispose)
        .filter((m: any) => m.kind === 'broadcast' && m.type === 'producer-added');
      expect(newBroadcasts).toHaveLength(0);
    });
  });

  // ── Error handling ──────────────────────────────────────────────────

  describe('error handling', () => {
    it('catches thrown errors and responds with error string', async () => {
      mockVoiceService.forwardToServer.mockRejectedValueOnce(new Error('network fail'));

      await expect(sendRpc(getChannel(), 'create-recv-transport', {})).rejects.toThrow(
        'network fail'
      );
    });

    it('unknown method responds with error', async () => {
      await expect(sendRpc(getChannel(), 'nonexistent-method')).rejects.toThrow(
        'Unknown method: nonexistent-method'
      );
    });

    it('ignores non-rpc-request messages', () => {
      const ch = getChannel();
      const prevLength = ch.posted.length;

      // Simulate a broadcast or rpc-response — proxy should ignore
      ch.simulateMessage({ kind: 'broadcast', type: 'state-update' });
      ch.simulateMessage({ kind: 'rpc-response', id: 'xyz' });

      // No new messages posted (proxy didn't try to handle them)
      expect(ch.posted.length).toBe(prevLength);
    });

    it('logs redacted error via Unhandled RPC error catch when handleRpcRequest rejects', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const ch = getChannel();

      // Force handleRpcRequest to reject by spying on the private method
      const handleRpcSpy = vi
        .spyOn(proxy as any, 'handleRpcRequest')
        .mockRejectedValueOnce(new Error('boom'));

      ch.simulateMessage({
        kind: 'rpc-request',
        id: 'req-unhandled',
        pipId: 'test-pip',
        method: 'request-state',
        params: {},
      });

      await vi.waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith('[PipSignalingProxy] Unhandled RPC error:', 'boom');
      });
      handleRpcSpy.mockRestore();
      consoleSpy.mockRestore();
    });

    it('logs error when respond postMessage throws (channel closed)', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const ch = getChannel();

      // Close the channel so postMessage throws DOMException
      ch.close();

      // Simulate an RPC request — the proxy will try to respond but the channel is closed
      ch.simulateMessage({
        kind: 'rpc-request',
        id: 'req-closed',
        pipId: 'test-pip',
        method: 'request-state',
        params: {},
      });

      await vi.waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith(
          '[PipSignalingProxy] Failed to send response:',
          expect.any(String)
        );
      });
      consoleSpy.mockRestore();
    });

    it('logs error when broadcast postMessage throws (channel closed)', () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const ch = getChannel();

      // Close the channel so postMessage throws
      ch.close();

      // Trigger a broadcast by changing voice store state
      useVoiceStore.setState({ participants: { 'user-x': { userId: 'user-x' } as any } });

      expect(consoleSpy).toHaveBeenCalledWith(
        '[PipSignalingProxy] Failed to broadcast:',
        expect.any(String)
      );
      consoleSpy.mockRestore();
    });
  });
});
