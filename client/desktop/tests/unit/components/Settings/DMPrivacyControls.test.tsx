import { render, screen } from '../../../test-utils';
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

  it('renders the rich-presence cross-reference note next to friends-of-friends (#1233)', () => {
    const { getByText } = render(
      <DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} />
    );
    expect(
      getByText(/also expands who can see your rich presence when set to friends tier/i)
    ).toBeInTheDocument();
  });
});

// #1241 / AC-19: the failed-PATCH revert applies to BOTH tier controls, via one
// shared helper. Before this, a rejected DM PATCH left the slider showing the
// level the server had refused — and rejected as an unhandled promise.
describe('DMPrivacyControls — save error (#1241 AC-19)', () => {
  it('surfaces a save error as an alert', () => {
    render(
      <DMPrivacyControls
        localDmLevel={1}
        setDmPrivacyLevel={vi.fn()}
        saveError="Failed to update privacy settings"
      />
    );
    expect(screen.getByRole('alert')).toHaveTextContent(/failed to update privacy settings/i);
  });

  it('renders no alert when there is no error', () => {
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
