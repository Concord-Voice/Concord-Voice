import { canCarryScreenAudio } from '@/renderer/utils/policy/screenAudioCapability';

describe('canCarryScreenAudio', () => {
  it('allows a screen target on a platform with loopback', () => {
    expect(canCarryScreenAudio('screen:0', 'darwin')).toBe(true);
    expect(canCarryScreenAudio('screen:0', 'win32')).toBe(true);
  });

  // #2161: Electron's desktop audio capture ignores chromeMediaSourceId, so a window
  // target asking for audio ships every application's sound to the channel.
  it('refuses every window target regardless of platform', () => {
    expect(canCarryScreenAudio('window:1', 'darwin')).toBe(false);
    expect(canCarryScreenAudio('window:1', 'win32')).toBe(false);
    expect(canCarryScreenAudio('window:1', 'linux')).toBe(false);
  });

  // The spec's capability ladder makes every Linux target audio-incapable: this
  // capture path has no Linux loopback and falls back to silent video, so offering
  // the control there advertises an operation that cannot succeed.
  it('refuses a screen target on Linux', () => {
    expect(canCarryScreenAudio('screen:0', 'linux')).toBe(false);
  });

  it('refuses when nothing is selected', () => {
    expect(canCarryScreenAudio(null, 'darwin')).toBe(false);
  });

  // An unresolved platform is the dev/web path, which reaches getDisplayMedia --
  // OS-mediated consent, not the #2161 whole-desktop loopback. Allowing it keeps the
  // control honest there; the capture path still gates on the prefix.
  it('allows a screen target when the platform is not yet known', () => {
    expect(canCarryScreenAudio('screen:0', null)).toBe(true);
  });

  it('refuses an id shape it does not recognise, rather than guessing', () => {
    expect(canCarryScreenAudio('tab:7', 'darwin')).toBe(false);
    expect(canCarryScreenAudio('', 'darwin')).toBe(false);
  });
});
