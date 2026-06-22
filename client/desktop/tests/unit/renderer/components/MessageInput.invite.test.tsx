import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { INVITE } from '@/renderer/utils/permissions';
import MessageInput from '@/renderer/components/Chat/MessageInput';

// --- Mocks required by MessageInput ---

vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/layoutStore', () => ({
  useLayoutStore: () => false,
}));
vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: [],
    addFiles: vi.fn(),
    removeFile: vi.fn(),
    clearFiles: vi.fn(),
    uploadAll: vi.fn().mockResolvedValue({ ids: [], summaries: [] }),
    isUploading: false,
    hasFiles: false,
  }),
}));
vi.mock('@/renderer/hooks/useDraftMessage', () => ({
  useDraftMessage: () => ({
    initialDraft: undefined,
    saveDraft: vi.fn(),
    clearDraft: vi.fn(),
  }),
}));
vi.mock('@/renderer/components/Chat/MentionAutocomplete', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Chat/ReplyPreviewBar', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/GifPicker/LazyGifPicker', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Markdown/SyntaxHelpModal', () => ({
  default: () => null,
}));

describe('MessageInput — invite affordance', () => {
  beforeEach(() => {
    resetAllStores();
    useServerStore.setState({ servers: [{ id: 's1', name: 'Acme', icon_url: null }] as never });
    usePermissionStore.setState({ serverPermissions: { s1: INVITE } as never });
    vi.spyOn(useInviteStore.getState(), 'createInvite').mockResolvedValue({
      id: 'i1',
      server_id: 's1',
      code: 'GHJKMNPQ',
      max_uses: 1,
      use_count: 0,
      expires_at: null,
      is_revoked: false,
      created_at: '2026-01-01T00:00:00Z',
    } as never);
  });

  it('inserts the canonical invite URL after picking a server', async () => {
    render(<MessageInput onSendMessage={vi.fn()} conversationId="dm-1" />);
    fireEvent.click(screen.getByRole('button', { name: /invite to a server/i }));
    fireEvent.click(await screen.findByText('Acme'));
    const textarea = screen.getByRole('textbox');
    await waitFor(() =>
      expect((textarea as HTMLTextAreaElement).value).toContain(
        'https://invite.concordvoice.chat/GHJKMNPQ'
      )
    );
  });

  it('does not show the invite button outside DM context', () => {
    render(<MessageInput onSendMessage={vi.fn()} serverId="srv-1" />);
    expect(screen.queryByRole('button', { name: /invite to a server/i })).not.toBeInTheDocument();
  });
});
