import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import Spoiler from '@/renderer/components/Markdown/Spoiler';

describe('Spoiler', () => {
  it('renders content hidden by default', () => {
    render(<Spoiler>secret text</Spoiler>);
    const el = screen.getByText('secret text');
    expect(el).toHaveClass('spoiler');
    expect(el).not.toHaveClass('spoiler-revealed');
  });

  it('reveals content on click', () => {
    render(<Spoiler>secret</Spoiler>);
    const el = screen.getByText('secret');
    fireEvent.click(el);
    expect(el).toHaveClass('spoiler-revealed');
  });

  it('remains revealed after reveal (no toggle back)', () => {
    render(<Spoiler>secret</Spoiler>);
    const el = screen.getByText('secret');
    fireEvent.click(el);
    fireEvent.click(el);
    expect(el).toHaveClass('spoiler-revealed');
  });

  it('has role button and is keyboard-accessible', () => {
    render(<Spoiler>secret</Spoiler>);
    const el = screen.getByRole('button');
    fireEvent.keyDown(el, { key: 'Enter' });
    expect(el).toHaveClass('spoiler-revealed');
  });
});
