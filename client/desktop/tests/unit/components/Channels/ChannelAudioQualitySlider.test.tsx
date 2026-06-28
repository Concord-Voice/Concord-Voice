import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import ChannelAudioQualitySlider from '@/renderer/components/Channels/ChannelAudioQualitySlider';
import { resetAllStores } from '../../../helpers/store-helpers';

describe('ChannelAudioQualitySlider', () => {
  beforeEach(() => resetAllStores());

  it('renders a Personal stop plus the 7 tiers (8 labels)', () => {
    render(<ChannelAudioQualitySlider value={null} onChange={() => {}} serverTier="groundspeed" />);
    // "Personal" appears in the label AND the kbps badge when value=null; at least one is enough.
    expect(screen.getAllByText('Personal').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText('Standard')).toBeInTheDocument();
    expect(screen.getByText('Studio')).toBeInTheDocument();
  });

  it('locks tiers above the server ceiling (groundspeed → high/hifi/studio locked)', () => {
    render(<ChannelAudioQualitySlider value={null} onChange={() => {}} serverTier="groundspeed" />);
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
    expect(screen.getByRole('button', { name: /High locked/ })).not.toHaveAttribute(
      'aria-disabled'
    );
    expect(screen.getByRole('button', { name: 'Personal' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it('selecting Personal emits null', () => {
    const onChange = vi.fn();
    render(
      <ChannelAudioQualitySlider value="standard" onChange={onChange} serverTier="groundspeed" />
    );
    fireEvent.click(screen.getByText('Personal'));
    expect(onChange).toHaveBeenCalledWith(null);
  });

  it('selecting an allowed tier emits the tier name', () => {
    const onChange = vi.fn();
    render(<ChannelAudioQualitySlider value={null} onChange={onChange} serverTier="groundspeed" />);
    fireEvent.click(screen.getByText('Moderate'));
    expect(onChange).toHaveBeenCalledWith('moderate');
  });

  it('selecting a locked tier only shows the lock hint', () => {
    const onChange = vi.fn();
    render(
      <ChannelAudioQualitySlider value="standard" onChange={onChange} serverTier="groundspeed" />
    );
    fireEvent.click(screen.getByText('Studio'));
    expect(onChange).not.toHaveBeenCalled();
    expect(
      screen.getByText('High-fidelity channel audio is available on Mach-boosted servers.')
    ).toBeInTheDocument();
  });

  it('selecting a locked tier from Personal does not overwrite Personal', () => {
    const onChange = vi.fn();
    render(<ChannelAudioQualitySlider value={null} onChange={onChange} serverTier="groundspeed" />);
    fireEvent.click(screen.getByText('Studio'));
    expect(onChange).not.toHaveBeenCalled();
  });

  it('on a Mach server, Studio is selectable', () => {
    const onChange = vi.fn();
    render(<ChannelAudioQualitySlider value={null} onChange={onChange} serverTier="mach" />);
    fireEvent.click(screen.getByText('Studio'));
    expect(onChange).toHaveBeenCalledWith('studio');
  });
});
