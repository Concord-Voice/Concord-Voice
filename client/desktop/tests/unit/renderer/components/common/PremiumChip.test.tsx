import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import PremiumChip from '@/renderer/components/common/PremiumChip';

describe('PremiumChip', () => {
  it('renders the 🔒 glyph with its accessible name', () => {
    render(<PremiumChip />);
    const glyph = screen.getByLabelText('Premium feature');
    expect(glyph).toBeInTheDocument();
    expect(glyph.textContent).toBe('\u{1F512}');
  });

  it('renders the literal word "Premium" (a11y P2)', () => {
    render(<PremiumChip />);
    expect(screen.getByText('Premium')).toBeInTheDocument();
  });

  it('renders an optional trailing label', () => {
    render(<PremiumChip label="High/Hi-Fi/Studio" />);
    expect(screen.getByText('High/Hi-Fi/Studio')).toBeInTheDocument();
  });

  it('is a static span (no button role) when onActivate is omitted', () => {
    render(<PremiumChip />);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('is a native button when onActivate is supplied', () => {
    render(<PremiumChip onActivate={vi.fn()} />);
    expect(screen.getByRole('button')).toBeInTheDocument();
  });

  it('fires onActivate on click', async () => {
    const onActivate = vi.fn();
    render(<PremiumChip onActivate={onActivate} />);
    await userEvent.click(screen.getByRole('button'));
    expect(onActivate).toHaveBeenCalledTimes(1);
  });

  it('fires onActivate on Enter and Space (keyboard)', async () => {
    const onActivate = vi.fn();
    render(<PremiumChip onActivate={onActivate} />);
    const button = screen.getByRole('button');
    button.focus();
    await userEvent.keyboard('{Enter}');
    await userEvent.keyboard(' ');
    expect(onActivate).toHaveBeenCalled();
  });

  it('omits the glyph when locked={false}', () => {
    render(<PremiumChip locked={false} />);
    expect(screen.queryByLabelText('Premium feature')).not.toBeInTheDocument();
    // The word "Premium" still renders (P2 conveys state by word, not glyph alone).
    expect(screen.getByText('Premium')).toBeInTheDocument();
  });

  it('forwards id (for aria-describedby wiring)', () => {
    render(<PremiumChip id="chip-1" />);
    expect(document.getElementById('chip-1')).toBeInTheDocument();
  });
});
