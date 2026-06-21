import { render, screen, fireEvent } from '../../../test-utils';
import ChannelItem, { type VoiceMemberInfo } from '@/renderer/components/Channels/ChannelItem';
import type { Channel } from '@/renderer/types/chat';
import { vi } from 'vitest';

const mockVoiceChannel: Channel = {
  id: 'voice-1',
  server_id: 'server-1',
  name: 'General Voice',
  type: 'voice',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const voiceMembers: VoiceMemberInfo[] = [
  { userId: 'u1', username: 'alice', displayName: 'Alice', isMuted: false, isSpeaking: false },
];

const baseProps = {
  channel: mockVoiceChannel,
  isActive: false,
  unread: 0,
  isGrouped: false,
  isLastInGroup: false,
  voiceMembers,
  linkedText: null as Channel | null,
  showLinkedText: false,
  isLinkedTextActive: false,
  linkedTextUnread: 0,
  canReorder: false,
  isDragging: false,
  showGhostBefore: false,
  showGhostAfter: false,
  onChannelClick: vi.fn(),
  onContextMenu: vi.fn(),
  onDragStart: vi.fn(),
  onDragOver: vi.fn(),
  onDrop: vi.fn(),
  onDragEnd: vi.fn(),
  onLinkedTextClick: vi.fn(),
  itemRef: vi.fn(),
};

describe('ChannelItem — voice participant interactivity (#487)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('marks participant names interactive when participant handlers are supplied', () => {
    render(
      <ChannelItem {...baseProps} onParticipantClick={vi.fn()} onParticipantContextMenu={vi.fn()} />
    );
    const name = screen.getByText('Alice');
    expect(name.className).toContain('voice-participant-name--interactive');
  });

  it('leaves participant names inert when no participant handlers are supplied', () => {
    render(<ChannelItem {...baseProps} />);
    const name = screen.getByText('Alice');
    expect(name.className).not.toContain('voice-participant-name--interactive');
  });

  it('calls onParticipantClick with the channel id and participant on click', () => {
    const onParticipantClick = vi.fn();
    render(<ChannelItem {...baseProps} onParticipantClick={onParticipantClick} />);
    fireEvent.click(screen.getByText('Alice'));
    expect(onParticipantClick).toHaveBeenCalledWith(
      expect.anything(),
      'voice-1',
      expect.objectContaining({ userId: 'u1' })
    );
  });

  it('renders the interactive participant name as a native button (keyboard-accessible by construction)', () => {
    // The interactive name is a native <button>, which provides Enter/Space
    // activation, focusability, and button semantics for free (a11y rules
    // S6819/S6845/S6848) — no role/tabIndex/onKeyDown shim needed.
    render(<ChannelItem {...baseProps} onParticipantClick={vi.fn()} />);
    const name = screen.getByText('Alice');
    expect(name.tagName).toBe('BUTTON');
    expect(name.getAttribute('type')).toBe('button');
  });

  it('renders an inert participant name as a plain span (no button semantics)', () => {
    render(<ChannelItem {...baseProps} />);
    const name = screen.getByText('Alice');
    expect(name.tagName).toBe('SPAN');
    expect(name.getAttribute('role')).toBeNull();
  });

  it('calls onParticipantContextMenu with the channel id and participant on right-click', () => {
    const onParticipantContextMenu = vi.fn();
    render(<ChannelItem {...baseProps} onParticipantContextMenu={onParticipantContextMenu} />);
    fireEvent.contextMenu(screen.getByText('Alice'));
    expect(onParticipantContextMenu).toHaveBeenCalledWith(
      expect.anything(),
      'voice-1',
      expect.objectContaining({ userId: 'u1' })
    );
  });

  it('makes the participant name draggable when onParticipantDragStart is supplied', () => {
    render(<ChannelItem {...baseProps} onParticipantDragStart={vi.fn()} />);
    expect(screen.getByText('Alice').getAttribute('draggable')).toBe('true');
  });

  it('applies the dragging class to the participant currently being dragged', () => {
    render(
      <ChannelItem {...baseProps} onParticipantDragStart={vi.fn()} draggingParticipantUserId="u1" />
    );
    expect(screen.getByText('Alice').className).toContain('voice-participant-name--dragging');
  });

  it('applies the participant drop-target highlight class to the channel row', () => {
    const { container } = render(<ChannelItem {...baseProps} isParticipantDropTarget />);
    expect(container.querySelector('.channel-item--participant-drop-target')).toBeInTheDocument();
  });
});
