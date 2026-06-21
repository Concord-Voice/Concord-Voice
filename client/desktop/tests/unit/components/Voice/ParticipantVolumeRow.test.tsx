import { render, screen, fireEvent } from '../../../test-utils';
import { ParticipantVolumeRow } from '@/renderer/components/Voice/ParticipantVolumeRow';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/ParticipantVolumeRow.css', () => ({}));

describe('ParticipantVolumeRow', () => {
  beforeEach(() => {
    resetAllStores();
    useAudioSettingsStore.setState({ perParticipantVolume: {} });
  });

  it('renders with the default volume of 100% when no override exists', () => {
    render(<ParticipantVolumeRow userId="user-1" />);
    expect(screen.getByText('100%')).toBeInTheDocument();
    expect(screen.getByLabelText('Participant volume')).toHaveValue('100');
  });

  it('renders the stored volume when an override exists', () => {
    useAudioSettingsStore.setState({ perParticipantVolume: { 'user-1': 65 } });
    render(<ParticipantVolumeRow userId="user-1" />);
    expect(screen.getByText('65%')).toBeInTheDocument();
    expect(screen.getByLabelText('Participant volume')).toHaveValue('65');
  });

  it('calls setParticipantVolume with the new value on slider change', () => {
    render(<ParticipantVolumeRow userId="user-1" />);
    const slider = screen.getByLabelText('Participant volume');
    fireEvent.change(slider, { target: { value: '150' } });
    expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBe(150);
  });

  it('writes only to the target userId (does not disturb other entries)', () => {
    useAudioSettingsStore.setState({
      perParticipantVolume: { 'user-1': 100, 'user-2': 25 },
    });
    render(<ParticipantVolumeRow userId="user-1" />);
    fireEvent.change(screen.getByLabelText('Participant volume'), { target: { value: '75' } });
    expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({
      'user-1': 75,
      'user-2': 25,
    });
  });

  it('hides the reset button when volume is at default (100)', () => {
    render(<ParticipantVolumeRow userId="user-1" />);
    expect(screen.queryByText('Reset to 100%')).not.toBeInTheDocument();
  });

  it('shows the reset button when volume is not 100', () => {
    useAudioSettingsStore.setState({ perParticipantVolume: { 'user-1': 60 } });
    render(<ParticipantVolumeRow userId="user-1" />);
    expect(screen.getByText('Reset to 100%')).toBeInTheDocument();
  });

  it('reset button clears the override', () => {
    useAudioSettingsStore.setState({ perParticipantVolume: { 'user-1': 60 } });
    render(<ParticipantVolumeRow userId="user-1" />);
    fireEvent.click(screen.getByText('Reset to 100%'));
    expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBeUndefined();
  });

  it("stops mousedown propagation so dragging doesn't close the context menu", () => {
    const onOutsideClick = vi.fn();
    const { container } = render(
      <div onMouseDown={onOutsideClick}>
        <ParticipantVolumeRow userId="user-1" />
      </div>
    );
    const row = container.querySelector('.participant-volume-row') as HTMLElement;
    fireEvent.mouseDown(row);
    expect(onOutsideClick).not.toHaveBeenCalled();
  });

  it('preventDefaults on contextmenu so the native menu does not appear over the app menu', () => {
    const { container } = render(<ParticipantVolumeRow userId="user-1" />);
    const row = container.querySelector('.participant-volume-row') as HTMLElement;
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    const preventSpy = vi.spyOn(event, 'preventDefault');
    row.dispatchEvent(event);
    expect(preventSpy).toHaveBeenCalled();
  });
});
