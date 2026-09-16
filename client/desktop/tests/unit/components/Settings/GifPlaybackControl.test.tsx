import { render, screen, fireEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import GifPlaybackControl from '@/renderer/components/Settings/GifPlaybackControl';
import type { GifPlaybackMode } from '@/renderer/utils/ui/gifPlayback';

describe('GifPlaybackControl (#2369 T7)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  const renderControl = (
    mode: GifPlaybackMode,
    reduceAnimations: boolean,
    onChange: (mode: GifPlaybackMode) => void = vi.fn()
  ) =>
    render(
      <GifPlaybackControl mode={mode} reduceAnimations={reduceAnimations} onChange={onChange} />
    );

  it('B1: renders three radios; exactly the one matching mode is checked', () => {
    renderControl('always', false);

    expect(screen.getByRole('radio', { name: 'Auto' })).not.toBeChecked();
    expect(screen.getByRole('radio', { name: 'Always' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Hover only' })).not.toBeChecked();
  });

  it('B2: clicking a radio calls onChange with that mode exactly once, and the component does not change its own displayed selection without a prop update', () => {
    const onChange = vi.fn();
    renderControl('auto', false, onChange);

    fireEvent.click(screen.getByRole('radio', { name: 'Always' }));

    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith('always');
    // Controlled component: `mode` prop is still 'auto', so the DISPLAY must
    // not have flipped on its own -- Auto stays checked, Always stays unchecked.
    expect(screen.getByRole('radio', { name: 'Auto' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Always' })).not.toBeChecked();
  });

  it('B3: the hint text tracks BOTH mode and reduceAnimations (WCAG 3.2.x: hint changes as a side effect of a different control)', () => {
    const onChange = vi.fn();
    const { rerender } = render(
      <GifPlaybackControl mode="auto" reduceAnimations={false} onChange={onChange} />
    );
    expect(screen.getByRole('status')).toHaveTextContent(
      'Following Reduce Animations: GIFs play automatically.'
    );

    rerender(<GifPlaybackControl mode="auto" reduceAnimations={true} onChange={onChange} />);
    expect(screen.getByRole('status')).toHaveTextContent(
      'Following Reduce Animations: GIFs play on hover only.'
    );

    rerender(<GifPlaybackControl mode="always" reduceAnimations={true} onChange={onChange} />);
    expect(screen.getByRole('status')).toHaveTextContent(
      'GIFs always play, even with Reduce Animations on.'
    );

    rerender(<GifPlaybackControl mode="hover" reduceAnimations={true} onChange={onChange} />);
    expect(screen.getByRole('status')).toHaveTextContent(
      'GIFs only play while you hover over them.'
    );
  });

  it('B4: the role="status" element id IS the fieldset\'s aria-describedby (React useId, not a hardcoded id)', () => {
    renderControl('auto', false);

    const status = screen.getByRole('status');
    const fieldset = document.querySelector('fieldset');
    expect(fieldset).not.toBeNull();

    const describedBy = fieldset!.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    expect(status.id).toBe(describedBy);
  });

  it('B5: no radio is ever disabled or aria-disabled, even with reduceAnimations true -- Reduce Animations is followed, never enforced', () => {
    renderControl('auto', true);

    const radios = screen.getAllByRole('radio');
    expect(radios).toHaveLength(3);
    for (const radio of radios) {
      expect(radio).not.toBeDisabled();
      expect(radio).not.toHaveAttribute('aria-disabled');
    }
  });
});
