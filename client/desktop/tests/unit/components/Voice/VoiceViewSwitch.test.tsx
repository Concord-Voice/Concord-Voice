import { render, screen, fireEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { beforeEach, describe, expect, it } from 'vitest';
import VoiceViewSwitch from '@/renderer/components/Voice/VoiceViewSwitch';

describe('VoiceViewSwitch', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders both segments as buttons', () => {
    render(<VoiceViewSwitch />);
    expect(screen.getByRole('button', { name: 'Tile view' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: "Front 'n Center" })).toBeInTheDocument();
  });

  it('marks the active mode with aria-pressed (tile)', () => {
    useVoiceStore.setState({ voiceViewMode: 'tile' });
    render(<VoiceViewSwitch />);
    expect(screen.getByRole('button', { name: 'Tile view' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.getByRole('button', { name: "Front 'n Center" })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('marks the active mode with aria-pressed (front-center)', () => {
    useVoiceStore.setState({ voiceViewMode: 'front-center' });
    render(<VoiceViewSwitch />);
    expect(screen.getByRole('button', { name: "Front 'n Center" })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.getByRole('button', { name: 'Tile view' })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('switches to front-center when that segment is clicked', () => {
    useVoiceStore.setState({ voiceViewMode: 'tile' });
    render(<VoiceViewSwitch />);
    fireEvent.click(screen.getByRole('button', { name: "Front 'n Center" }));
    expect(useVoiceStore.getState().voiceViewMode).toBe('front-center');
  });

  it('switches to tile when that segment is clicked', () => {
    useVoiceStore.setState({ voiceViewMode: 'front-center' });
    render(<VoiceViewSwitch />);
    fireEvent.click(screen.getByRole('button', { name: 'Tile view' }));
    expect(useVoiceStore.getState().voiceViewMode).toBe('tile');
  });

  it('clicking the already-active segment keeps the mode (idempotent)', () => {
    useVoiceStore.setState({ voiceViewMode: 'tile' });
    render(<VoiceViewSwitch />);
    fireEvent.click(screen.getByRole('button', { name: 'Tile view' }));
    expect(useVoiceStore.getState().voiceViewMode).toBe('tile');
  });
});
