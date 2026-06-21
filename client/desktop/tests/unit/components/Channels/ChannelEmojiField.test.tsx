import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';

vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: ({ onSelect, onClose }: { onSelect: (emoji: string) => void; onClose: () => void }) => (
    <div data-testid="emoji-picker">
      <button data-testid="pick-emoji" onClick={() => onSelect('🎉')}>
        Pick
      </button>
      <button data-testid="close-picker" onClick={onClose}>
        Close
      </button>
    </div>
  ),
}));

import ChannelEmojiField from '@/renderer/components/Channels/ChannelEmojiField';

describe('ChannelEmojiField', () => {
  const mockOnChange = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // 1. Renders "Pick an emoji" placeholder when no emoji
  it('renders placeholder when no emoji is set', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    expect(screen.getByText('Pick an emoji')).toBeInTheDocument();
  });

  // 2. Renders emoji when emoji is set
  it('renders emoji when emoji is set', () => {
    render(<ChannelEmojiField emoji="🔥" onChange={mockOnChange} />);
    expect(screen.getByText('🔥')).toBeInTheDocument();
  });

  // 3. Shows clear button when emoji is set
  it('shows clear button when emoji is set', () => {
    render(<ChannelEmojiField emoji="🔥" onChange={mockOnChange} />);
    expect(screen.getByTitle('Remove emoji')).toBeInTheDocument();
  });

  // 4. Hides clear button when no emoji
  it('hides clear button when no emoji', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    expect(screen.queryByTitle('Remove emoji')).not.toBeInTheDocument();
  });

  // 5. Opens picker on button click
  it('opens picker on button click', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();
  });

  // 6. Closes picker and calls onChange when emoji selected
  it('closes picker and calls onChange when emoji selected', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pick-emoji'));
    expect(mockOnChange).toHaveBeenCalledWith('🎉');
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();
  });

  // 7. Closes picker when onClose called
  it('closes picker when onClose is called', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('close-picker'));
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();
  });

  // 8. Clear button calls onChange with empty string
  it('clear button calls onChange with empty string', () => {
    render(<ChannelEmojiField emoji="🔥" onChange={mockOnChange} />);
    fireEvent.click(screen.getByTitle('Remove emoji'));
    expect(mockOnChange).toHaveBeenCalledWith('');
  });

  // 9. Disabled state disables buttons
  it('disables buttons when disabled prop is true', () => {
    render(<ChannelEmojiField emoji="🔥" onChange={mockOnChange} disabled />);
    expect(screen.getByTitle('Change emoji')).toBeDisabled();
    expect(screen.getByTitle('Remove emoji')).toBeDisabled();
  });

  // 10. Renders custom hint text
  it('renders custom hint text', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} hint="Choose a fun emoji!" />);
    expect(screen.getByText('Choose a fun emoji!')).toBeInTheDocument();
  });

  it('renders default hint text', () => {
    render(<ChannelEmojiField emoji="" onChange={mockOnChange} />);
    expect(screen.getByText('Click to pick a channel emoji')).toBeInTheDocument();
  });
});
