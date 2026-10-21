import React from 'react';
import { Blob as NodeBlob, File as NodeFile } from 'node:buffer';
import { beforeAll, afterAll, afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { deferred } from '../../../helpers/deferred';
import { resetAllStores } from '../../../helpers/store-helpers';
import VoiceTextChat from '@/renderer/components/Voice/VoiceTextChat';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

vi.mock('@/renderer/components/Chat/MessageList', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({ default: () => null }));
vi.mock('@/renderer/hooks/messaging/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));
vi.mock('@/renderer/hooks/messaging/useDMSubscription', () => ({ useDMSubscription: vi.fn() }));
vi.mock('@/renderer/hooks/messaging/useMessageFetch', () => ({
  useMessageFetch: () => ({
    messages: [],
    isLoading: false,
    hasMore: false,
    error: null,
    handleLoadMore: vi.fn(),
    isHistoryReady: true,
  }),
}));

const sendMessage = vi.hoisted(() => vi.fn());
vi.mock('@/renderer/hooks/messaging/useChatController', () => ({
  useChatController: () => ({
    sendMessage,
    sendTyping: vi.fn(),
    editMessage: vi.fn(),
    deleteMessage: vi.fn(),
    deleteRefusal: null,
    confirmDelete: vi.fn(),
    dismissDeleteRefusal: vi.fn(),
    replyingTo: null,
    handleReply: vi.fn(),
    cancelReply: vi.fn(),
    canPin: false,
    handlePinToggle: vi.fn(),
    chatContext: { type: 'dm', id: 'call-dm' },
  }),
}));

let attachmentKey: CryptoKey;
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    getChannelKeyMaterial: async (id: string) => {
      // Match the real key service's admission: an empty target cannot have a key.
      if (!id) throw new Error('INVALID_REQUEST');
      return { channelKey: attachmentKey, keyVersion: 1 };
    },
    createChannelOperationGuard: () => ({ assertCurrent: () => {} }),
    revokeChannelAccess: vi.fn(),
  },
}));

const server = setupServer();
beforeAll(async () => {
  server.listen({ onUnhandledFrame: 'error' });
  // undici needs native multipart classes to encode and parse a file part.
  vi.stubGlobal('File', NodeFile);
  vi.stubGlobal('Blob', NodeBlob);
  const form = await new Response('a=b', {
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
  }).formData();
  vi.stubGlobal('FormData', form.constructor);
});
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => {
  server.close();
  vi.unstubAllGlobals();
});

beforeEach(async () => {
  resetAllStores();
  vi.clearAllMocks();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({ user: { id: 'me', username: 'me' } } as never);
  useClientConfigStore.setState({ chunkedUploadCapability: { status: 'supported' } });
  attachmentKey = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, [
    'encrypt',
    'decrypt',
  ]);
});

describe('voice text-chat attachment target', () => {
  it.each([
    ['dm', 'chunked'],
    ['channel', 'chunked'],
    ['dm', 'legacy'],
    ['channel', 'legacy'],
  ] as const)(
    'uploads and sends an attachment to the %s text target using %s upload',
    async (kind, mode) => {
      useClientConfigStore.setState({
        chunkedUploadCapability: {
          status: mode === 'chunked' ? 'supported' : 'confirmed-unsupported',
        },
      });
      useVoiceStore.setState({
        activeChannelId: kind === 'dm' ? 'call-dm' : 'call-voice',
        isDMCall: kind === 'dm',
        dmConversationId: kind === 'dm' ? 'call-dm' : null,
      });
      useChannelStore.setState({
        channels: [
          {
            id: 'linked-text',
            server_id: 'server-1',
            name: 'linked-chat',
            type: 'text',
            linked_voice_channel_id: 'call-voice',
            position: 0,
            created_at: '',
            updated_at: '',
          },
        ],
      });

      const initBodies: Record<string, unknown>[] = [];
      const chunks: ArrayBuffer[] = [];
      const sessionId = 'a'.repeat(43);
      const path = '*/api/v1/media/upload/attachment/session';
      server.use(
        http.post('*/api/v1/media/upload/attachment', async ({ request }) => {
          const form = await request.formData();
          const file = form.get('file') as File;
          chunks.push(await file.arrayBuffer());
          initBodies.push({
            ...Object.fromEntries([...form.entries()].filter(([name]) => name !== 'file')),
            key_version: Number(form.get('key_version')),
          });
          return HttpResponse.json({ file_id: 'uploaded-file' });
        }),
        http.post(path, async ({ request }) => {
          initBodies.push((await request.json()) as Record<string, unknown>);
          return HttpResponse.json({ session_id: sessionId }, { status: 201 });
        }),
        http.put(`${path}/${sessionId}/chunk/0`, async ({ request }) => {
          chunks.push(await request.arrayBuffer());
          return new HttpResponse(null, { status: 204 });
        }),
        http.post(`${path}/${sessionId}/commit`, () =>
          HttpResponse.json({
            file_id: 'uploaded-file',
            storage_key: 'attachments/test',
            file_type: 'file',
            file_size: 5,
          })
        )
      );

      const { container } = render(<VoiceTextChat />);
      const input = container.querySelector<HTMLInputElement>('input[type="file"]');
      expect(input).not.toBeNull();
      fireEvent.change(input as HTMLInputElement, {
        target: { files: [new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' })] },
      });
      await screen.findByText('hello.txt');
      fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter', code: 'Enter' });

      await waitFor(() =>
        expect(sendMessage).toHaveBeenCalledWith(
          ' ',
          expect.objectContaining({
            attachmentIds: ['uploaded-file'],
          })
        )
      );
      expect(initBodies).toHaveLength(1);
      expect(initBodies[0]).toMatchObject(
        kind === 'dm'
          ? { conversation_id: 'call-dm', key_version: 1 }
          : { channel_id: 'linked-text', key_version: 1 }
      );
      expect(initBodies[0]).not.toHaveProperty(kind === 'dm' ? 'channel_id' : 'conversation_id');
      expect(chunks).toHaveLength(1);
      expect(chunks[0].byteLength).toBeGreaterThan(5);
      expect(screen.queryByText('Failed to upload attachments')).not.toBeInTheDocument();
    }
  );
  it('shows a real legacy upload failure without sending the message', async () => {
    useClientConfigStore.setState({ chunkedUploadCapability: { status: 'confirmed-unsupported' } });
    useVoiceStore.setState({
      activeChannelId: 'call-dm',
      isDMCall: true,
      dmConversationId: 'call-dm',
    });
    server.use(
      http.post('*/api/v1/media/upload/attachment', () =>
        HttpResponse.json({ error: 'Upload unavailable' }, { status: 503 })
      )
    );
    const { container } = render(<VoiceTextChat />);
    fireEvent.change(container.querySelector('input[type="file"]') as HTMLInputElement, {
      target: { files: [new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' })] },
    });
    await screen.findByText('hello.txt');
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'keep this draft' } });
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter', code: 'Enter' });
    await screen.findByText('Failed to upload all attachments');
    expect(screen.getByRole('textbox')).toHaveValue('keep this draft');
    expect(sendMessage).not.toHaveBeenCalled();
  });

  it.each(['cancel', 'unmount'] as const)(
    'aborts a legacy DM upload on %s without sending a message',
    async (action) => {
      useClientConfigStore.setState({
        chunkedUploadCapability: { status: 'confirmed-unsupported' },
      });
      useVoiceStore.setState({
        activeChannelId: 'call-dm',
        isDMCall: true,
        dmConversationId: 'call-dm',
      });
      const started = deferred();
      // MSW reconstructs the handler's signal; inspect the caller's while
      // still allowing the real fetch to reach the MSW endpoint.
      const fetchSpy = vi.spyOn(globalThis, 'fetch');
      const release = deferred();
      const finished = deferred();
      server.use(
        http.post('*/api/v1/media/upload/attachment', async () => {
          started.resolve();
          await release.promise;
          finished.resolve();
          return HttpResponse.json({ file_id: 'late-file' });
        })
      );
      const view = render(<VoiceTextChat />);
      const input = view.container.querySelector<HTMLInputElement>('input[type="file"]');
      fireEvent.change(input as HTMLInputElement, {
        target: { files: [new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' })] },
      });
      await screen.findByText('hello.txt');
      fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter', code: 'Enter' });
      await started.promise;
      const requestIndex = fetchSpy.mock.calls.findIndex(([url]) =>
        String(url).endsWith('/api/v1/media/upload/attachment')
      );
      expect(requestIndex).toBeGreaterThanOrEqual(0);
      const signal = fetchSpy.mock.calls[requestIndex][1]?.signal;
      const requestDone = fetchSpy.mock.results[requestIndex].value as Promise<Response>;
      expect(signal).toBeDefined();
      try {
        if (action === 'cancel') {
          // Starting the request does not guarantee React committed the uploading UI.
          fireEvent.click(
            await screen.findByRole('button', { name: 'Cancel upload of hello.txt' })
          );
        } else {
          view.unmount();
        }
        expect(signal?.aborted).toBe(true);
      } finally {
        await act(async () => {
          release.resolve();
          await finished.promise;
          await requestDone.catch(() => undefined);
        });
      }
      if (action === 'cancel') {
        await waitFor(() =>
          expect(screen.getByRole('button', { name: 'Send message' })).toBeEnabled()
        );
        expect(screen.queryByText('Failed to upload all attachments')).not.toBeInTheDocument();
        expect(screen.queryByText('Failed to upload attachments')).not.toBeInTheDocument();
      }
      expect(sendMessage).not.toHaveBeenCalled();
    }
  );
});
