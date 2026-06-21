import { render, screen, fireEvent } from '../../test-utils';
import { useMessageProfileCard } from '@/renderer/hooks/useMessageProfileCard';
import MessageProfileCardHost from '@/renderer/components/Chat/MessageProfileCardHost';
import type { MessageWithStatus, ChatContextType } from '@/renderer/types/chat';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { mockMessage, mockMember } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';

// Thin harness exercising the hook: an "open" button drives openProfileAt and
// MessageProfileCardHost renders the resolved card/modal below it. Mirrors how
// Message wires the hook + host pair (post-Sonar S6804 split — the hook owns
// state, the host owns rendering).
function Harness({
  message,
  currentUserId,
  chatContext = 'channel',
}: {
  message: MessageWithStatus;
  currentUserId: string;
  chatContext?: ChatContextType;
}) {
  const profileCardState = useMessageProfileCard({
    message,
    currentUserId,
    chatContext,
  });
  return (
    <div>
      <button type="button" onClick={() => profileCardState.openProfileAt({ x: 10, y: 10 })}>
        open
      </button>
      <MessageProfileCardHost state={profileCardState} />
    </div>
  );
}

describe('useMessageProfileCard', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('opens the profile card using server member data when available', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Harness message={mockMessage} currentUserId="user-2" />);
    fireEvent.click(screen.getByText('open'));
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });

  it('toggles closed on a second open for the same user', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Harness message={mockMessage} currentUserId="user-2" />);
    const trigger = screen.getByText('open');
    fireEvent.click(trigger);
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
    fireEvent.click(trigger);
    expect(document.querySelector('.member-profile-card')).not.toBeInTheDocument();
  });

  it('skips server member lookup in DM context and falls back to the friend store', () => {
    // Member present in the server store but should be ignored in DM context.
    useMemberStore.getState().addMember(mockMember);
    useFriendStore.setState({
      friends: [
        {
          id: 'f-1',
          userId: mockMessage.user_id,
          username: 'testuser',
          displayName: 'Test User',
          status: 'online',
        },
      ],
    });
    render(<Harness message={mockMessage} currentUserId="user-2" chatContext="dm" />);
    fireEvent.click(screen.getByText('open'));
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });

  it('falls back to message data when neither store has the user', () => {
    render(<Harness message={mockMessage} currentUserId="user-2" />);
    fireEvent.click(screen.getByText('open'));
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });
});
