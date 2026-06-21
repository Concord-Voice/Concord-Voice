import { render, screen, fireEvent } from '../../../test-utils';
import ReactionBar from '@/renderer/components/Chat/ReactionBar';
import { mockReaction, mockReaction2 } from '../../../mocks/fixtures';
import { vi } from 'vitest';
import type { ReactionSummary } from '@/renderer/types/chat';

// Mock the reaction service
vi.mock('@/renderer/services/reactionService', () => ({
  toggleReaction: vi.fn(() => Promise.resolve({ action: 'added' })),
}));

// Mock LazyEmojiPicker to avoid loading the full emoji dataset
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: ({ onSelect, onClose }: { onSelect: (emoji: string) => void; onClose: () => void }) => (
    <div data-testid="emoji-picker">
      <button onClick={() => onSelect('🎉')} data-testid="picker-select">
        Select
      </button>
      <button onClick={onClose} data-testid="picker-close">
        Close
      </button>
    </div>
  ),
}));

describe('ReactionBar', () => {
  const reactions: ReactionSummary[] = [mockReaction, mockReaction2];

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders reaction chips with emoji and count', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    expect(screen.getByText('👍')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(screen.getByText('❤️')).toBeInTheDocument();
    expect(screen.getByText('1')).toBeInTheDocument();
  });

  it('highlights active reaction (me=true)', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    // mockReaction has me=true (👍)
    const thumbsUp = screen.getByText('👍').closest('.reaction-chip');
    expect(thumbsUp).toHaveClass('reaction-chip-active');
  });

  it('does not highlight non-active reaction (me=false)', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    // mockReaction2 has me=false (❤️)
    const heart = screen.getByText('❤️').closest('.reaction-chip');
    expect(heart).not.toHaveClass('reaction-chip-active');
  });

  it('calls toggleReaction on chip click', async () => {
    const { toggleReaction } = await import('@/renderer/services/reactionService');
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    const chip = screen.getByText('👍').closest('.reaction-chip') as HTMLElement;
    fireEvent.click(chip);

    expect(toggleReaction).toHaveBeenCalledWith('msg-1', '👍');
  });

  it('shows add-reaction button', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);
    expect(screen.getByLabelText('Add reaction')).toBeInTheDocument();
  });

  it('opens emoji picker on add-reaction click', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    fireEvent.click(screen.getByLabelText('Add reaction'));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();
  });

  it('calls toggleReaction when emoji selected from picker', async () => {
    const { toggleReaction } = await import('@/renderer/services/reactionService');
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    fireEvent.click(screen.getByLabelText('Add reaction'));
    fireEvent.click(screen.getByTestId('picker-select'));

    expect(toggleReaction).toHaveBeenCalledWith('msg-1', '🎉');
  });

  it('closes emoji picker after selection', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    fireEvent.click(screen.getByLabelText('Add reaction'));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('picker-select'));
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();
  });

  it('returns null when reactions array is empty', () => {
    const { container } = render(<ReactionBar messageId="msg-1" reactions={[]} />);
    expect(container.querySelector('.reaction-bar')).not.toBeInTheDocument();
  });

  it('shows usernames in title tooltip', () => {
    render(<ReactionBar messageId="msg-1" reactions={reactions} />);

    const thumbsUp = screen.getByText('👍').closest('.reaction-chip');
    expect(thumbsUp).toHaveAttribute('title', 'Test User, Test User 2');
  });
});
