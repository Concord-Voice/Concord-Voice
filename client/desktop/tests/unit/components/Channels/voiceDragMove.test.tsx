import { render, screen, fireEvent } from '../../../test-utils';
import ChannelList from '@/renderer/components/Channels/ChannelList';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  ADMIN_PERMISSIONS,
  BASE_PERMISSIONS,
  MOVE_MEMBERS,
} from '@/renderer/utils/permissions';
import { vi } from 'vitest';
import type { Channel } from '@/renderer/types/chat';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true }),
  API_BASE: 'http://localhost:3001',
}));

const mockMove = vi.fn().mockResolvedValue({ ok: true });
vi.mock('@/renderer/services/voiceParticipantApi', () => ({
  moveVoiceParticipant: (...args: unknown[]) => mockMove(...args),
  disconnectVoiceParticipant: vi.fn(),
}));

const SERVER_ID = 'server-1';
const SOURCE_VC = 'voice-source';
const TARGET_VC = 'voice-target';
const TEXT_CH = 'text-1';
const DRAGGED_USER = 'user-dragged';

const PARTICIPANT_MIME = 'application/concord-voice-participant';

const sourceVoice: Channel = {
  id: SOURCE_VC,
  server_id: SERVER_ID,
  name: 'Source Voice',
  type: 'voice',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};
const targetVoice: Channel = {
  id: TARGET_VC,
  server_id: SERVER_ID,
  name: 'Target Voice',
  type: 'voice',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};
const textChannel: Channel = {
  id: TEXT_CH,
  server_id: SERVER_ID,
  name: 'general',
  type: 'text',
  position: 2,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

/** Minimal DataTransfer stub backed by a string map. */
function makeDataTransfer(seed: Record<string, string> = {}) {
  const store: Record<string, string> = { ...seed };
  return {
    dropEffect: 'none',
    effectAllowed: 'none',
    get types() {
      return Object.keys(store);
    },
    setData: (type: string, val: string) => {
      store[type] = val;
    },
    getData: (type: string) => store[type] ?? '',
    setDragImage: vi.fn(),
  } as unknown as DataTransfer;
}

function seedSidebar(perms: bigint = ADMIN_PERMISSIONS, selfId = 'user-self') {
  useServerStore.setState({ activeServerId: SERVER_ID });
  useUserStore.setState({ user: { id: selfId, username: 'me' } as never });
  usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: perms } });
  useChannelStore.setState({
    channels: [sourceVoice, targetVoice, textChannel],
    channelGroups: [],
    isLoading: false,
    error: null,
    fetchChannels: vi.fn() as unknown as (id: string) => Promise<void>,
    clearChannels: vi.fn() as unknown as () => void,
  });
  // Put the dragged user in the source voice channel's sidebar list.
  useVoiceStore.setState({
    channelVoiceMembers: {
      [SOURCE_VC]: [
        {
          userId: DRAGGED_USER,
          username: 'dragged',
          displayName: 'Dragged User',
          isMuted: false,
          serverMuted: false,
          serverDeafened: false,
        },
      ],
    },
  });
}

const renderList = () =>
  render(
    <ChannelList
      onContextMenu={vi.fn()}
      onEmptyContextMenu={vi.fn()}
      onCategoryContextMenu={vi.fn()}
    />
  );

/** Begin a participant drag and return the populated DataTransfer. */
function startParticipantDrag(): DataTransfer {
  const dt = makeDataTransfer();
  fireEvent.dragStart(screen.getByText('Dragged User'), { dataTransfer: dt });
  return dt;
}

describe('voice participant drag-and-drop move (#487 Scope B)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('sets the participant MIME payload on drag start', () => {
    seedSidebar();
    renderList();
    const dt = startParticipantDrag();
    expect(dt.types).toContain(PARTICIPANT_MIME);
    const payload = JSON.parse(dt.getData(PARTICIPANT_MIME));
    expect(payload).toEqual({
      participantUserId: DRAGGED_USER,
      sourceChannelId: SOURCE_VC,
      sourceServerId: SERVER_ID,
    });
  });

  it('dropping on a different same-server voice channel calls moveVoiceParticipant', () => {
    seedSidebar();
    renderList();
    const dt = startParticipantDrag();
    const targetRow = screen.getByText('Target Voice').closest('.channel-item')!;
    fireEvent.dragOver(targetRow, { dataTransfer: dt });
    fireEvent.drop(targetRow, { dataTransfer: dt });
    expect(mockMove).toHaveBeenCalledWith(SERVER_ID, DRAGGED_USER, TARGET_VC);
  });

  it('highlights a valid voice drop target during drag-over', () => {
    seedSidebar();
    const { container } = renderList();
    const dt = startParticipantDrag();
    const targetRow = screen.getByText('Target Voice').closest('.channel-item')!;
    fireEvent.dragOver(targetRow, { dataTransfer: dt });
    expect(
      container.querySelector('.channel-item--participant-drop-target')
    ).toBeInTheDocument();
  });

  it('dropping on a TEXT channel is a no-op (no move call)', () => {
    seedSidebar();
    renderList();
    const dt = startParticipantDrag();
    const textRow = screen.getByText('general').closest('.channel-item')!;
    fireEvent.dragOver(textRow, { dataTransfer: dt });
    fireEvent.drop(textRow, { dataTransfer: dt });
    expect(mockMove).not.toHaveBeenCalled();
  });

  it('dropping on the SOURCE channel is a no-op (no move call)', () => {
    seedSidebar();
    renderList();
    const dt = startParticipantDrag();
    const sourceRow = screen.getByText('Source Voice').closest('.channel-item')!;
    fireEvent.dragOver(sourceRow, { dataTransfer: dt });
    fireEvent.drop(sourceRow, { dataTransfer: dt });
    expect(mockMove).not.toHaveBeenCalled();
  });

  it('does NOT highlight a text channel on drag-over (invalid target)', () => {
    seedSidebar();
    const { container } = renderList();
    const dt = startParticipantDrag();
    const textRow = screen.getByText('general').closest('.channel-item')!;
    fireEvent.dragOver(textRow, { dataTransfer: dt });
    expect(
      container.querySelector('.channel-item--participant-drop-target')
    ).not.toBeInTheDocument();
  });

  it('self-drag is permitted even without MOVE_MEMBERS', () => {
    // Self is the dragged user; base perms (no MOVE_MEMBERS).
    seedSidebar(BASE_PERMISSIONS, DRAGGED_USER);
    renderList();
    const dt = makeDataTransfer();
    fireEvent.dragStart(screen.getByText('Dragged User'), { dataTransfer: dt });
    // Drag payload is set → drag was not prevented.
    expect(dt.types).toContain(PARTICIPANT_MIME);
  });

  it('dragging OTHERS without MOVE_MEMBERS does not set a drag payload', () => {
    // Self differs from the dragged user; base perms lack MOVE_MEMBERS.
    seedSidebar(BASE_PERMISSIONS, 'someone-else');
    renderList();
    const dt = makeDataTransfer();
    fireEvent.dragStart(screen.getByText('Dragged User'), { dataTransfer: dt });
    expect(dt.types).not.toContain(PARTICIPANT_MIME);
  });

  it('dragging OTHERS WITH MOVE_MEMBERS sets a drag payload', () => {
    seedSidebar(BASE_PERMISSIONS | MOVE_MEMBERS, 'someone-else');
    renderList();
    const dt = makeDataTransfer();
    fireEvent.dragStart(screen.getByText('Dragged User'), { dataTransfer: dt });
    expect(dt.types).toContain(PARTICIPANT_MIME);
  });
});
