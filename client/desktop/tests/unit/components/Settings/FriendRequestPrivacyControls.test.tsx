import { render, screen, fireEvent } from '../../../test-utils';
import FriendRequestPrivacyControls from '@/renderer/components/Settings/FriendRequestPrivacyControls';
import type { FriendRequestPrivacyMode } from '@/renderer/stores/ui/privacyStore';

const setup = (over: Partial<React.ComponentProps<typeof FriendRequestPrivacyControls>> = {}) => {
  const setMode = vi.fn();
  render(
    <FriendRequestPrivacyControls
      localMode={'everyone' as FriendRequestPrivacyMode}
      setMode={setMode}
      saveError={null}
      isLoaded={true}
      {...over}
    />
  );
  return { setMode };
};

describe('FriendRequestPrivacyControls', () => {
  it('renders the three modes with most-restrictive first', () => {
    setup();
    expect(screen.getAllByRole('button').map((b) => b.textContent)).toEqual([
      'No One',
      'Mutual Servers',
      'Everyone',
    ]);
  });

  it('marks the active mode with aria-pressed', () => {
    setup({ localMode: 'mutual_servers' });
    expect(screen.getByRole('button', { name: 'Mutual Servers' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.getByRole('button', { name: 'Everyone' })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('calls setMode with the enum value, not the slider index', () => {
    const { setMode } = setup();
    fireEvent.click(screen.getByRole('button', { name: 'No One' }));
    expect(setMode).toHaveBeenCalledWith('nobody');
  });

  // The precedent (DMPrivacyControls.tsx:68-76) ships a range with NO
  // accessible name. Do not inherit that WCAG 4.1.2 failure.
  it('gives the range an accessible name and a human-readable value', () => {
    setup({ localMode: 'nobody' });
    const slider = screen.getByRole('slider', { name: /who can send you friend requests/i });
    expect(slider).toHaveAttribute('aria-valuetext', 'No One');
  });

  it('maps the slider position to the enum, not the index', () => {
    const { setMode } = setup({ localMode: 'nobody' });
    fireEvent.change(screen.getByRole('slider'), { target: { value: '2' } });
    expect(setMode).toHaveBeenCalledWith('everyone');
  });

  // Q1 corollary: the setting gates inbound CREATION only.
  it('explains that existing requests survive when nobody is selected', () => {
    setup({ localMode: 'nobody' });
    expect(screen.getByText(/you can still accept them/i)).toBeInTheDocument();
  });

  it('omits that copy for the permissive modes', () => {
    setup({ localMode: 'everyone' });
    expect(screen.queryByText(/you can still accept them/i)).not.toBeInTheDocument();
  });

  // A security-adjacent value must not flash a wrong-and-more-permissive mode.
  it('disables every input and marks the group busy until the server confirms', () => {
    setup({ isLoaded: false });
    expect(screen.getByRole('group')).toHaveAttribute('aria-busy', 'true');
    expect(screen.getByRole('slider')).toBeDisabled();
    screen.getAllByRole('button').forEach((b) => expect(b).toBeDisabled());
  });

  it('surfaces a save error as an alert', () => {
    setup({ saveError: "This version of the server doesn't support this setting yet." });
    expect(screen.getByRole('alert')).toHaveTextContent(/doesn't support this setting yet/);
  });

  it('renders no alert when there is no error', () => {
    setup();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // Non-disclosure: this control edits the viewer's OWN setting and must never
  // describe, imply, or explain another user's.
  // The serious half is not the one-frame flash: if the privacy fetch FAILS,
  // isLoading returns to false and the permissive default would sit on screen
  // indefinitely, telling a user who chose "No One" that anyone may contact them.
  it('shows no mode value at all until the server has confirmed one', () => {
    setup({ isLoaded: false, localMode: 'nobody' });
    // The choice labels still render — they are the options on offer, not a
    // claim about the user. What must NOT appear is any indication of which one
    // is selected, or a description asserting a mode.
    screen
      .getAllByRole('button')
      .forEach((b) => expect(b).toHaveAttribute('aria-pressed', 'false'));
    expect(screen.queryByText(/no one can send you a friend request/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/anyone can send you/i)).not.toBeInTheDocument();
    expect(screen.getByText(/has not loaded yet/i)).toBeInTheDocument();
    expect(screen.getByRole('slider')).toHaveAttribute('aria-valuetext', 'Loading');
  });

  it('suppresses the nobody corollary copy until loaded', () => {
    setup({ isLoaded: false, localMode: 'nobody' });
    expect(screen.queryByText(/you can still accept them/i)).not.toBeInTheDocument();
  });

  it('never references another user setting', () => {
    setup({ localMode: 'nobody' });
    expect(document.body.textContent).not.toMatch(/their setting|they have set|because they/i);
  });
});
