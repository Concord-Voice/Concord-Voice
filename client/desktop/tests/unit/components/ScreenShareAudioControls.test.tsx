import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ScreenShareAudioControls } from '@/renderer/components/Voice/ScreenShareAudioControls';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { voiceService } from '@/renderer/services/voiceService';

/** Mark the local viewer as server-deafened (moderator-enforced). */
function setLocalServerDeafened(deafened: boolean): void {
  useUserStore.setState({
    user: { id: 'me', username: 'me' },
  });
  const local: VoiceParticipant = {
    userId: 'me',
    username: 'me',
    isMuted: false,
    isDeafened: false,
    serverMuted: false,
    serverDeafened: deafened,
    isVideoOn: false,
    isScreenSharing: false,
    isSpeaking: false,
  };
  useVoiceStore.setState({ participants: { me: local } });
}

describe('ScreenShareAudioControls (#2162)', () => {
  beforeEach(() => {
    useAudioSettingsStore.getState().clearAllScreenShareVolumes();
    useVoiceStore.getState().reset();
    useUserStore.setState({ user: null });
    vi.restoreAllMocks();
  });

  it('defaults to 100% and drags volume → setScreenShareVolume', () => {
    render(<ScreenShareAudioControls sharerUserId="u1" />);
    const slider = screen.getByLabelText('Screen share volume') as HTMLInputElement;
    expect(slider.value).toBe('100');
    fireEvent.change(slider, { target: { value: '150' } });
    expect(useAudioSettingsStore.getState().perScreenShareVolume['u1']).toBe(150);
  });

  it('mute button calls voiceService.muteScreenShare', () => {
    const mute = vi.spyOn(voiceService, 'muteScreenShare').mockImplementation(() => {});
    render(<ScreenShareAudioControls sharerUserId="u1" />);
    fireEvent.click(screen.getByRole('button', { name: /mute screen audio/i }));
    expect(mute).toHaveBeenCalledWith('u1');
  });

  it('shows unmute affordance + disabled slider when already muted, and calls unmute', () => {
    const unmute = vi.spyOn(voiceService, 'unmuteScreenShare').mockImplementation(() => {});
    useVoiceStore.getState().setScreenShareMuted('u1', true);
    render(<ScreenShareAudioControls sharerUserId="u1" />);
    expect((screen.getByLabelText('Screen share volume') as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText('Muted')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: /unmute screen audio/i }));
    expect(unmute).toHaveBeenCalledWith('u1');
  });

  it('disables controls and shows "Deafened" when the local viewer is server-deafened', () => {
    const mute = vi.spyOn(voiceService, 'muteScreenShare').mockImplementation(() => {});
    setLocalServerDeafened(true);
    render(<ScreenShareAudioControls sharerUserId="u1" />);

    const button = screen.getByRole('button', { name: /server-deafened by a moderator/i });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByLabelText('Screen share volume') as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText('Deafened')).toBeTruthy();

    // The mute action never fires while server-deafened (moderation is authoritative).
    fireEvent.click(button);
    expect(mute).not.toHaveBeenCalled();
  });

  it('leaves controls enabled when the local viewer is not server-deafened', () => {
    setLocalServerDeafened(false);
    render(<ScreenShareAudioControls sharerUserId="u1" />);
    expect(
      (screen.getByRole('button', { name: /mute screen audio/i }) as HTMLButtonElement).disabled
    ).toBe(false);
    expect((screen.getByLabelText('Screen share volume') as HTMLInputElement).disabled).toBe(false);
  });

  // #2162: VoiceStage registers a window-level ArrowLeft/ArrowRight keydown
  // listener that cycles the dominant share in focus mode. Arrow keys are also
  // the range input's native volume-adjust keys, so the controls must stop the
  // keydown from bubbling to that window listener — otherwise adjusting volume
  // would also switch streams. stopPropagation must NOT preventDefault (the
  // slider still adjusts).
  it('stops slider/button arrow keydowns from reaching a window keydown listener', () => {
    const windowListener = vi.fn();
    globalThis.addEventListener('keydown', windowListener);
    try {
      render(<ScreenShareAudioControls sharerUserId="u1" />);
      const slider = screen.getByLabelText('Screen share volume');
      const muteBtn = screen.getByRole('button', { name: /mute screen audio/i });

      fireEvent.keyDown(slider, { key: 'ArrowLeft' });
      fireEvent.keyDown(slider, { key: 'ArrowRight' });
      fireEvent.keyDown(muteBtn, { key: 'ArrowLeft' });

      expect(windowListener).not.toHaveBeenCalled();
    } finally {
      globalThis.removeEventListener('keydown', windowListener);
    }
  });

  it('does not preventDefault on slider keydown (native volume adjust preserved)', () => {
    render(<ScreenShareAudioControls sharerUserId="u1" />);
    const slider = screen.getByLabelText('Screen share volume');
    const defaultPrevented = !fireEvent.keyDown(slider, { key: 'ArrowRight' });
    expect(defaultPrevented).toBe(false);
  });
});
