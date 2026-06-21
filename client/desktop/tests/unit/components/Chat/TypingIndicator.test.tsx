import { render, screen } from '../../../test-utils';
import TypingIndicator from '@/renderer/components/Chat/TypingIndicator';
import { useChatStore } from '@/renderer/stores/chatStore';
import { resetAllStores } from '../../../helpers/store-helpers';

describe('TypingIndicator', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders nothing when no one is typing', () => {
    const { container } = render(<TypingIndicator channelId="channel-1" />);
    expect(container.querySelector('.typing-indicator-text')).toBeNull();
  });

  it('shows typing text for one user', () => {
    useChatStore.getState().setTyping('channel-1', 'user-2', true, 'testuser2');
    render(<TypingIndicator channelId="channel-1" />);
    expect(screen.getByText(/testuser2.*typing/i)).toBeInTheDocument();
  });

  it('shows typing text for two users', () => {
    useChatStore.getState().setTyping('channel-1', 'user-2', true, 'alice');
    useChatStore.getState().setTyping('channel-1', 'user-3', true, 'bob');
    render(<TypingIndicator channelId="channel-1" />);
    expect(screen.getByText(/alice.*bob.*typing/i)).toBeInTheDocument();
  });

  it('shows three people typing with comma-separated names', () => {
    useChatStore.getState().setTyping('channel-1', 'u1', true, 'alice');
    useChatStore.getState().setTyping('channel-1', 'u2', true, 'bob');
    useChatStore.getState().setTyping('channel-1', 'u3', true, 'charlie');
    render(<TypingIndicator channelId="channel-1" />);
    expect(screen.getByText(/alice.*bob.*charlie.*typing/i)).toBeInTheDocument();
  });

  it('shows several people for 4+ users', () => {
    useChatStore.getState().setTyping('channel-1', 'u1', true, 'a');
    useChatStore.getState().setTyping('channel-1', 'u2', true, 'b');
    useChatStore.getState().setTyping('channel-1', 'u3', true, 'c');
    useChatStore.getState().setTyping('channel-1', 'u4', true, 'd');
    render(<TypingIndicator channelId="channel-1" />);
    expect(screen.getByText(/several people.*typing/i)).toBeInTheDocument();
  });

  it('does not show typing for a different channel', () => {
    useChatStore.getState().setTyping('channel-2', 'user-2', true, 'testuser2');
    const { container } = render(<TypingIndicator channelId="channel-1" />);
    expect(container.querySelector('.typing-indicator-text')).toBeNull();
  });

  it('renders typing dots when someone is typing', () => {
    useChatStore.getState().setTyping('channel-1', 'user-2', true, 'testuser2');
    const { container } = render(<TypingIndicator channelId="channel-1" />);
    expect(container.querySelector('.typing-dots')).toBeInTheDocument();
  });
});
