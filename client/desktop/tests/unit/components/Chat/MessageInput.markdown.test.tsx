import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { resetAllStores } from '../../../helpers/store-helpers';
import userEvent from '@testing-library/user-event';

// Mock child components and stores that MessageInput depends on
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/layoutStore', () => ({
  useLayoutStore: () => false,
}));

const defaultProps = {
  channelId: 'c1',
  onSendMessage: vi.fn(),
  canAttachFiles: false,
};

describe('MessageInput markdown UX', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('shows the hint text with the ? help icon', () => {
    render(<MessageInput {...defaultProps} />);
    expect(screen.getByText(/Supports/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/markdown syntax help/i)).toBeInTheDocument();
  });

  it('opens the SyntaxHelpModal when ? clicked', async () => {
    render(<MessageInput {...defaultProps} />);
    await userEvent.click(screen.getByLabelText(/markdown syntax help/i));
    expect(screen.getByText(/Supported Markdown Syntax/i)).toBeInTheDocument();
  });

  it('does not show character counter below 75% capacity', async () => {
    render(<MessageInput {...defaultProps} />);
    const textarea = screen.getByRole('textbox');
    await userEvent.type(textarea, 'a'.repeat(100));
    expect(screen.queryByText(/\/5120/)).not.toBeInTheDocument();
  });

  it('shows character counter at >=3840 chars (75% of 5120)', () => {
    render(<MessageInput {...defaultProps} />);
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'a'.repeat(3840) } });
    expect(screen.getByText(/3840\/5120/)).toBeInTheDocument();
  });

  it('shows warning class at >=4864 chars (95% of 5120)', () => {
    render(<MessageInput {...defaultProps} />);
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'a'.repeat(4864) } });
    const counter = screen.getByText(/4864\/5120/);
    expect(counter.className).toContain('warn');
  });

  it('shows error class on counter when over 5120 chars (overflow path enabled, button active)', () => {
    // With the overflow path wired, the send button is no longer disabled when
    // content exceeds the cap — handleSend converts it to a .md attachment.
    // The visual signal is the counter entering the error (red) state.
    render(<MessageInput {...defaultProps} />);
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'a'.repeat(5121) } });
    const counter = screen.getByText(/5121\/5120/);
    expect(counter.className).toContain('error');
  });
});
