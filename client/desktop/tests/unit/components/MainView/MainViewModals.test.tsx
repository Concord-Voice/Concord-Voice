import { render } from '../../../test-utils';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import MainViewModals from '@/renderer/components/MainView/MainViewModals';
import { resetAllStores } from '../../../helpers/store-helpers';
import type { ServerWithRole } from '@/renderer/types/server';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';

describe('MainViewModals', () => {
  beforeEach(() => resetAllStores());

  const noopProps = {
    isServerActionModalOpen: false,
    setIsServerActionModalOpen: vi.fn(),
    isCreateServerModalOpen: false,
    setIsCreateServerModalOpen: vi.fn(),
    isJoinServerModalOpen: false,
    setIsJoinServerModalOpen: vi.fn(),
    isCreateChannelModalOpen: false,
    setIsCreateChannelModalOpen: vi.fn(),
    isCreateCategoryModalOpen: false,
    setIsCreateCategoryModalOpen: vi.fn(),
    deletingServer: null,
    setDeletingServer: vi.fn(),
    leavingServer: null,
    setLeavingServer: vi.fn(),
    editingChannel: null,
    setEditingChannel: vi.fn(),
    deletingChannel: null,
    setDeletingChannel: vi.fn(),
    invitingServer: null,
    setInvitingServer: vi.fn(),
    editingCategory: null,
    setEditingCategory: vi.fn(),
    deletingCategory: null,
    setDeletingCategory: vi.fn(),
    channelPermissions: null,
    setChannelPermissions: vi.fn(),
    categoryPermissions: null,
    setCategoryPermissions: vi.fn(),
    activeServer: null,
    onCreateServerSuccess: vi.fn(),
    onCreateChannelSuccess: vi.fn(),
  };

  it('renders without crashing when all modals are closed', () => {
    const { container } = render(<MainViewModals {...noopProps} />);
    expect(container).toBeTruthy();
  });

  it('does not crash when all modal props are non-null (branch-coverage smoke)', () => {
    const mockServer: ServerWithRole = {
      id: 'server-1',
      name: 'Test Server',
      owner_id: 'user-1',
      allow_embedded_content: false,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
      role: 'owner',
      member_count: 1,
      online_count: 1,
    };

    const mockChannel: Channel = {
      id: 'chan-1',
      server_id: 'server-1',
      name: 'general',
      type: 'text',
      position: 0,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
    };

    const mockCategory: ChannelGroup = {
      id: 'cat-1',
      server_id: 'server-1',
      name: 'General',
      position: 0,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
    };

    const allOpenProps = {
      ...noopProps,
      isServerActionModalOpen: true,
      isCreateServerModalOpen: true,
      isJoinServerModalOpen: true,
      isCreateChannelModalOpen: true,
      isCreateCategoryModalOpen: true,
      deletingServer: mockServer,
      leavingServer: mockServer,
      editingChannel: mockChannel,
      deletingChannel: mockChannel,
      invitingServer: mockServer,
      editingCategory: mockCategory,
      deletingCategory: mockCategory,
      channelPermissions: mockChannel,
      categoryPermissions: mockCategory,
      activeServer: mockServer,
    };

    const { container } = render(<MainViewModals {...allOpenProps} />);
    expect(container).toBeTruthy();
  });
});
