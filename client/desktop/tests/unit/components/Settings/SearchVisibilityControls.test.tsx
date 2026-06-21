import { render } from '../../../test-utils';
import { describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import SearchVisibilityControls from '@/renderer/components/Settings/SearchVisibilityControls';

describe('SearchVisibilityControls', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the search visibility subsection title', () => {
    const { getByText } = render(<SearchVisibilityControls />);
    expect(getByText(/search visibility/i)).toBeInTheDocument();
  });
});
