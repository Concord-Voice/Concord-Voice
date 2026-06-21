import { render } from '../../../test-utils';
import LoadingSpinner from '@/renderer/components/Auth/LoadingSpinner';

describe('LoadingSpinner', () => {
  it('renders with default medium size', () => {
    const { container } = render(<LoadingSpinner />);
    expect(container.querySelector('.loading-spinner-medium')).toBeInTheDocument();
  });

  it('renders with small size', () => {
    const { container } = render(<LoadingSpinner size="small" />);
    expect(container.querySelector('.loading-spinner-small')).toBeInTheDocument();
  });

  it('renders with large size', () => {
    const { container } = render(<LoadingSpinner size="large" />);
    expect(container.querySelector('.loading-spinner-large')).toBeInTheDocument();
  });

  it('renders inline variant', () => {
    const { container } = render(<LoadingSpinner inline />);
    expect(container.querySelector('.inline')).toBeInTheDocument();
  });

  it('renders spinner element', () => {
    const { container } = render(<LoadingSpinner />);
    expect(container.querySelector('.spinner')).toBeInTheDocument();
  });
});
