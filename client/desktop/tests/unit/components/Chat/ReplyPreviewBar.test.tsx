import { render, screen, fireEvent } from '../../../test-utils';
import ReplyPreviewBar from '@/renderer/components/Chat/ReplyPreviewBar';
import { vi } from 'vitest';
import type { RepliedToMessage } from '@/renderer/types/chat';

describe('ReplyPreviewBar', () => {
  const repliedTo: RepliedToMessage = {
    id: 'msg-1',
    user_id: 'user-1',
    username: 'testuser',
    display_name: 'Test User',
    content: 'Hello, world!',
  };

  it('renders author and snippet', () => {
    render(<ReplyPreviewBar repliedTo={repliedTo} />);
    expect(screen.getByText('Test User')).toBeInTheDocument();
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
  });

  it('truncates long content at 100 characters', () => {
    const longContent = 'A'.repeat(150);
    const longMessage = { ...repliedTo, content: longContent };
    render(<ReplyPreviewBar repliedTo={longMessage} />);
    const snippet = screen.getByText(/^A+\.\.\.$/);
    expect(snippet.textContent).toHaveLength(103); // 100 chars + "..."
  });

  it('shows "Original message is unavailable" when isDeleted', () => {
    render(<ReplyPreviewBar repliedTo={null} isDeleted={true} />);
    expect(screen.getByText('Original message is unavailable')).toBeInTheDocument();
    expect(screen.queryByText('Test User')).not.toBeInTheDocument();
  });

  it('calls onCancel when cancel button clicked', () => {
    const onCancel = vi.fn();
    render(<ReplyPreviewBar repliedTo={repliedTo} onCancel={onCancel} />);
    fireEvent.click(screen.getByLabelText('Cancel reply'));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('calls onClick when bar clicked', () => {
    const onClick = vi.fn();
    render(<ReplyPreviewBar repliedTo={repliedTo} onClick={onClick} />);
    fireEvent.click(screen.getByText('Hello, world!'));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('returns null when repliedTo is null and isDeleted is false', () => {
    const { container } = render(<ReplyPreviewBar repliedTo={null} />);
    expect(container.querySelector('.reply-preview-bar')).not.toBeInTheDocument();
  });

  it('uses username when display_name is not set', () => {
    const noDisplayName = { ...repliedTo, display_name: undefined };
    render(<ReplyPreviewBar repliedTo={noDisplayName} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('applies input variant class', () => {
    render(<ReplyPreviewBar repliedTo={repliedTo} variant="input" />);
    expect(document.querySelector('.reply-preview-input')).toBeInTheDocument();
  });

  it('applies inline variant class', () => {
    render(<ReplyPreviewBar repliedTo={repliedTo} variant="inline" />);
    expect(document.querySelector('.reply-preview-inline')).toBeInTheDocument();
  });
});
