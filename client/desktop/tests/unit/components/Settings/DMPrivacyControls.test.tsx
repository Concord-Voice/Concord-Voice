import { render } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import DMPrivacyControls from '@/renderer/components/Settings/DMPrivacyControls';

describe('DMPrivacyControls', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the DM privacy slider header', () => {
    const { getByText } = render(
      <DMPrivacyControls localDmLevel={2} setDmPrivacyLevel={vi.fn()} />
    );
    expect(getByText(/who can dm you/i)).toBeInTheDocument();
  });

  it('renders the custom-status cross-reference note next to friends-of-friends (#1233)', () => {
    const { getByText } = render(
      <DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} />
    );
    expect(
      getByText(/also expands who can see your custom status at the friends tier/i)
    ).toBeInTheDocument();
  });
});
