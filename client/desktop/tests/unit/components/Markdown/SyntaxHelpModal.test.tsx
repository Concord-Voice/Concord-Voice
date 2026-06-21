import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import SyntaxHelpModal from '@/renderer/components/Markdown/SyntaxHelpModal';

describe('SyntaxHelpModal', () => {
  it('renders when open', () => {
    render(<SyntaxHelpModal open onClose={vi.fn()} />);
    expect(screen.getByText(/Supported Markdown Syntax/i)).toBeInTheDocument();
  });

  it('does not render when open=false', () => {
    render(<SyntaxHelpModal open={false} onClose={vi.fn()} />);
    expect(screen.queryByText(/Supported Markdown Syntax/i)).not.toBeInTheDocument();
  });

  it('renders all must-have and should-have constructs', () => {
    render(<SyntaxHelpModal open onClose={vi.fn()} />);
    expect(screen.getByText(/\*\*bold\*\*/)).toBeInTheDocument();
    expect(screen.getByText(/\*italic\*/)).toBeInTheDocument();
    expect(screen.getByText(/~~strikethrough~~/)).toBeInTheDocument();
    expect(screen.getByText(/`inline code`/)).toBeInTheDocument();
    expect(screen.getByText(/\|\|spoiler\|\|/)).toBeInTheDocument();
    expect(screen.getByText(/:smile:/)).toBeInTheDocument();
    expect(screen.getByText(/5,120 characters per message/i)).toBeInTheDocument();
  });

  it('calls onClose when close button clicked', () => {
    const onClose = vi.fn();
    render(<SyntaxHelpModal open onClose={onClose} />);
    fireEvent.click(screen.getByLabelText(/close/i));
    expect(onClose).toHaveBeenCalled();
  });

  it('calls onClose when Escape pressed', () => {
    const onClose = vi.fn();
    render(<SyntaxHelpModal open onClose={onClose} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });

  it('shows the multi-line fenced-code-block tip (#807)', () => {
    render(<SyntaxHelpModal open onClose={vi.fn()} />);
    expect(
      screen.getByText(/opening and closing triple-backticks each on their own line/i)
    ).toBeInTheDocument();
  });

  it('tooltip text reflects 5120-char cap with auto-overflow phrasing', () => {
    render(<SyntaxHelpModal open onClose={vi.fn()} />);
    expect(screen.getByText(/Up to 5,120 characters per message/)).toBeInTheDocument();
    expect(screen.getByText(/sent as a \.md attachment/i)).toBeInTheDocument();
    // Negative: legacy "24,000" wording must be gone
    expect(screen.queryByText(/24,000/)).not.toBeInTheDocument();
  });
});
