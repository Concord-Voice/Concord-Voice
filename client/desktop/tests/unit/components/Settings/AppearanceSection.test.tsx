import { act, render, screen, fireEvent } from '../../../test-utils';
import { LayoutSection } from '@/renderer/components/Settings/AppearanceSection';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// LayoutSection is the "Lock Interface" row that lives in Appearance settings
// (#188, per markdrogersjr's spec comment). It is wired directly to the real
// layoutStore — immediate-apply, per-device, NOT the draft/Save cycle used by
// Color Scheme / Theme — so assertions go against store state rather than a
// draft-setter spy. Exported from AppearanceSection.tsx for isolated testing,
// mirroring the ClientBehaviorSection pattern.
describe('AppearanceSection — Lock Interface (#188)', () => {
  beforeEach(() => {
    resetAllStores();
    useLayoutStore.setState({ interfaceLocked: false, sidebarLayoutsDecoupled: false });
  });

  const getCheckbox = () =>
    screen
      .getByText('Lock Interface')
      .closest('.settings-row')
      ?.querySelector('input[type="checkbox"]') as HTMLInputElement;

  it('renders the Layout section with a Lock Interface row', () => {
    render(<LayoutSection />);
    expect(screen.getByText('Layout')).toBeInTheDocument();
    expect(screen.getByText('Lock Interface')).toBeInTheDocument();
  });

  it('shows the device-scoped description from the issue spec', () => {
    render(<LayoutSection />);
    expect(
      screen.getByText(
        'Prevent accidental resizing and panel toggling. This setting applies to this device only.'
      )
    ).toBeInTheDocument();
  });

  it('reflects the unlocked state on the toggle by default', () => {
    render(<LayoutSection />);
    expect(getCheckbox().checked).toBe(false);
  });

  it('reflects the locked state on the toggle', () => {
    act(() => useLayoutStore.setState({ interfaceLocked: true }));
    render(<LayoutSection />);
    expect(getCheckbox().checked).toBe(true);
  });

  it('toggles layoutStore.interfaceLocked on and back off when clicked', () => {
    render(<LayoutSection />);
    const checkbox = getCheckbox();

    fireEvent.click(checkbox);
    expect(useLayoutStore.getState().interfaceLocked).toBe(true);

    fireEvent.click(checkbox);
    expect(useLayoutStore.getState().interfaceLocked).toBe(false);
  });

  it('immediately toggles separate DM and server sidebar layouts', () => {
    render(<LayoutSection />);
    const toggle = screen.getByRole('checkbox', {
      name: 'Use separate DM and server sidebar layouts',
    });

    fireEvent.click(toggle);

    expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(true);
  });

  it('disables the sidebar layout toggle and shows the approved helper while locked', () => {
    render(<LayoutSection />);
    fireEvent.click(screen.getByText('Layout'));
    const toggle = screen.getByRole('checkbox', {
      name: 'Use separate DM and server sidebar layouts',
    });

    act(() => useLayoutStore.setState({ interfaceLocked: true }));

    expect(toggle).toBeDisabled();
    expect(screen.getByText('Unlock Interface to change sidebar layout settings.')).toBeVisible();
  });
});
