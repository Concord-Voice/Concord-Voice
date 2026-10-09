import { render, screen, fireEvent } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import DMPrivacyControls from '@/renderer/components/Settings/DMPrivacyControls';

describe('DMPrivacyControls', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the DM privacy slider header', () => {
    const { getByText } = render(
      <DMPrivacyControls localDmLevel={2} setDmPrivacyLevel={vi.fn()} isLoaded />
    );
    expect(getByText(/who can dm you/i)).toBeInTheDocument();
  });

  it('renders the rich-presence cross-reference note next to friends-of-friends (#1233)', () => {
    const { getByText } = render(
      <DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded />
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
        isLoaded
      />
    );
    expect(screen.getByRole('alert')).toHaveTextContent(/failed to update privacy settings/i);
  });

  it('renders no alert when there is no error', () => {
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

// Until the server answers, the privacy store holds PLACEHOLDERS (dmFriendsOfFriends
// is true by default), not the user's choices. Rendering them as a live "on" switch
// would present a guess as a decision and let a click overwrite the real value.
describe('DMPrivacyControls — before the privacy settings load', () => {
  const realUpdatePrivacy = usePrivacyStore.getState().updatePrivacy;
  const updatePrivacy = vi.fn();

  beforeEach(() => {
    updatePrivacy.mockReset();
    usePrivacyStore.setState({
      loaded: false,
      updatePrivacy,
      settings: {
        ...usePrivacyStore.getState().settings,
        dmFriendsOfFriends: true,
        autoAcceptFriendCodes: true,
      },
    });
  });

  afterEach(() => {
    usePrivacyStore.setState({ updatePrivacy: realUpdatePrivacy });
  });

  it('disables both switches and shows them unchecked despite placeholder values of true', () => {
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded={false} />);

    const [fof, autoAccept] = screen.getAllByRole('checkbox');
    expect(fof).toBeDisabled();
    expect(fof).not.toBeChecked();
    expect(autoAccept).toBeDisabled();
    expect(autoAccept).not.toBeChecked();
  });

  it('calls no updatePrivacy when either switch is clicked', () => {
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded={false} />);

    for (const toggle of screen.getAllByRole('checkbox')) fireEvent.click(toggle);

    expect(updatePrivacy).not.toHaveBeenCalled();
  });

  it('shows the stored values and writes through once loaded', () => {
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded />);

    const [fof, autoAccept] = screen.getAllByRole('checkbox');
    expect(fof).toBeEnabled();
    expect(fof).toBeChecked();
    expect(autoAccept).toBeEnabled();
    expect(autoAccept).toBeChecked();

    fireEvent.click(fof);
    fireEvent.click(autoAccept);
    expect(updatePrivacy).toHaveBeenCalledWith({ dmFriendsOfFriends: false });
    expect(updatePrivacy).toHaveBeenCalledWith({ autoAcceptFriendCodes: false });
  });

  it('shows the stored false value (not the placeholder true) once loaded', () => {
    usePrivacyStore.setState({
      loaded: true,
      settings: {
        ...usePrivacyStore.getState().settings,
        dmFriendsOfFriends: false,
        autoAcceptFriendCodes: false,
      },
    });
    render(<DMPrivacyControls localDmLevel={1} setDmPrivacyLevel={vi.fn()} isLoaded />);

    for (const toggle of screen.getAllByRole('checkbox')) expect(toggle).not.toBeChecked();
  });
});
