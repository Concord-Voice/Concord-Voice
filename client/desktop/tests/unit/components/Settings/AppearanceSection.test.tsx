import { render, screen, fireEvent } from '../../../test-utils';
import { LayoutSection } from '@/renderer/components/Settings/AppearanceSection';
import { useLayoutStore } from '@/renderer/stores/layoutStore';

// LayoutSection is the "Lock Interface" row that lives in Appearance settings
// (#188, per markdrogersjr's spec comment). It is wired directly to the real
// layoutStore — immediate-apply, per-device, NOT the draft/Save cycle used by
// Color Scheme / Theme — so assertions go against store state rather than a
// draft-setter spy. Exported from AppearanceSection.tsx for isolated testing,
// mirroring the ClientBehaviorSection pattern.
describe('AppearanceSection — Lock Interface (#188)', () => {
  beforeEach(() => {
    useLayoutStore.setState({ interfaceLocked: false });
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
    useLayoutStore.setState({ interfaceLocked: true });
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
});
