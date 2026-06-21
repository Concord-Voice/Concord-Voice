import { render, screen, fireEvent } from '../../../test-utils';
import InfoTooltip from '@/renderer/components/Auth/InfoTooltip';

describe('InfoTooltip', () => {
  it('renders info icon button', () => {
    render(<InfoTooltip content="Helpful info" />);
    expect(screen.getByLabelText('More information')).toBeInTheDocument();
  });

  it('does not show tooltip content initially', () => {
    render(<InfoTooltip content="Helpful info" />);
    expect(screen.queryByText('Helpful info')).not.toBeInTheDocument();
  });

  it('shows tooltip on mouse enter', () => {
    render(<InfoTooltip content="Helpful info" />);
    fireEvent.mouseEnter(screen.getByLabelText('More information'));
    expect(screen.getByText('Helpful info')).toBeInTheDocument();
  });

  it('hides tooltip on mouse leave', () => {
    render(<InfoTooltip content="Helpful info" />);
    const btn = screen.getByLabelText('More information');
    fireEvent.mouseEnter(btn);
    expect(screen.getByText('Helpful info')).toBeInTheDocument();
    fireEvent.mouseLeave(btn);
    expect(screen.queryByText('Helpful info')).not.toBeInTheDocument();
  });

  it('toggles tooltip on click', () => {
    render(<InfoTooltip content="Helpful info" />);
    const btn = screen.getByLabelText('More information');
    fireEvent.click(btn);
    expect(screen.getByText('Helpful info')).toBeInTheDocument();
    fireEvent.click(btn);
    expect(screen.queryByText('Helpful info')).not.toBeInTheDocument();
  });
});
