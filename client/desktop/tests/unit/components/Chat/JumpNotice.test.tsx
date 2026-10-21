import { describe, it, expect } from 'vitest';
import { render, screen } from '../../../test-utils';
import JumpNotice from '@/renderer/components/Chat/JumpNotice';

const COPY = "Couldn't reach that message. It may have been deleted or expired.";

describe('JumpNotice (#3894)', () => {
  // kills: text ? <output/> : null
  it('C1 renders the text in a status role', () => {
    render(<JumpNotice text={COPY} />);
    const notice = screen.getByRole('status');
    expect(notice.tagName).toBe('OUTPUT');
    expect(notice).toHaveClass('jump-notice');
    expect(notice.textContent).toBe(COPY);
  });

  // kills: text ? <output/> : null
  it('C2 null leaves an empty element that is still in the document', () => {
    render(<JumpNotice text={null} />);
    const notice = screen.getByRole('status');
    expect(notice).toBeInTheDocument();
    expect(notice).toBeEmptyDOMElement();
  });

  // kills: text ? <output/> : null
  it('C3 the same DOM node survives a text change, so the live region keeps its identity', () => {
    const { rerender } = render(<JumpNotice text={null} />);
    const notice = screen.getByRole('status');
    expect(notice).toBeEmptyDOMElement();

    rerender(<JumpNotice text={COPY} />);
    expect(screen.getByRole('status')).toBe(notice);
    expect(notice.textContent).toBe(COPY);

    rerender(<JumpNotice text="Another sentence." />);
    expect(screen.getByRole('status')).toBe(notice);
    expect(notice.textContent).toBe('Another sentence.');

    rerender(<JumpNotice text={null} />);
    expect(screen.getByRole('status')).toBe(notice);
    expect(notice).toBeEmptyDOMElement();
  });
});
