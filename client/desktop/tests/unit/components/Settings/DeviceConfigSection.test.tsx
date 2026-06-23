import { vi, describe, it, expect, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';

const mockPlayTestTone = vi.fn().mockResolvedValue(undefined);
const mockToggleCameraTest = vi.fn().mockResolvedValue(undefined);
const mockStartMicTest = vi.fn().mockResolvedValue(undefined);
const mockStopMicTest = vi.fn();

let outputTestState = {
  isTesting: false,
  error: null as string | null,
  playTestTone: mockPlayTestTone,
};
let cameraTestState = {
  isTesting: false,
  error: null as string | null,
  stream: null as MediaStream | null,
  toggleTest: mockToggleCameraTest,
  stopTest: vi.fn(),
};

vi.mock('@/renderer/hooks/useMicTest', () => ({
  useMicTest: () => ({
    isTesting: false,
    dbfsLevel: -Infinity,
    error: null,
    startTest: mockStartMicTest,
    stopTest: mockStopMicTest,
  }),
}));

vi.mock('@/renderer/hooks/useOutputTest', () => ({
  useOutputTest: () => outputTestState,
}));

vi.mock('@/renderer/hooks/useCameraTest', () => ({
  useCameraTest: () => cameraTestState,
}));

let mockConnectionState = 'disconnected';
let mockLocalIsTesting = false;
vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: vi.fn((s: any) =>
    s
      ? s({ connectionState: mockConnectionState, localIsTesting: mockLocalIsTesting })
      : { connectionState: mockConnectionState, localIsTesting: mockLocalIsTesting }
  ),
}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn(() => 100),
  setDraftAudioSetting: vi.fn(),
}));

vi.mock('@/renderer/components/Voice/DeviceSelector', () => ({
  default: ({ kind }: { kind: string }) => <div>DeviceSelector:{kind}</div>,
}));

import DeviceConfigSection from '@/renderer/components/Settings/DeviceConfigSection';

describe('DeviceConfigSection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    outputTestState = {
      isTesting: false,
      error: null,
      playTestTone: mockPlayTestTone,
    };
    cameraTestState = {
      isTesting: false,
      error: null,
      stream: null,
      toggleTest: mockToggleCameraTest,
      stopTest: vi.fn(),
    };
    mockConnectionState = 'disconnected';
    mockLocalIsTesting = false;
  });

  it('renders Input, Output, and Camera subsections', () => {
    render(<DeviceConfigSection />);
    expect(screen.getByText('Input')).toBeInTheDocument();
    expect(screen.getByText('Output')).toBeInTheDocument();
    expect(screen.getByText('Camera')).toBeInTheDocument();
  });

  it('renders three Test buttons (Input, Output, Camera)', () => {
    render(<DeviceConfigSection />);
    const testButtons = screen.getAllByRole('button', { name: /^Test$/ });
    expect(testButtons).toHaveLength(3);
  });

  it('invokes playTestTone when Output Test button is clicked', () => {
    render(<DeviceConfigSection />);
    const buttons = screen.getAllByRole('button', { name: /^Test$/ });
    // Second button is the Output test (after mic input)
    fireEvent.click(buttons[1]);
    expect(mockPlayTestTone).toHaveBeenCalled();
  });

  it('invokes toggleCameraTest when Camera Test button is clicked', () => {
    render(<DeviceConfigSection />);
    const buttons = screen.getAllByRole('button', { name: /^Test$/ });
    fireEvent.click(buttons[2]);
    expect(mockToggleCameraTest).toHaveBeenCalled();
  });

  it('shows Playing label and disabled state while output tone is playing', () => {
    outputTestState = { ...outputTestState, isTesting: true };
    render(<DeviceConfigSection />);
    expect(screen.getByRole('button', { name: /Playing/ })).toBeDisabled();
  });

  it('shows Stop Preview label when camera preview is active', () => {
    cameraTestState = {
      ...cameraTestState,
      isTesting: true,
      stream: { getTracks: () => [] } as unknown as MediaStream,
    };
    render(<DeviceConfigSection />);
    expect(screen.getByRole('button', { name: /Stop Preview/ })).toBeInTheDocument();
  });

  it('shows output test error text in settings-output-test-error element', () => {
    outputTestState = { ...outputTestState, error: 'No audio context' };
    render(<DeviceConfigSection />);
    const errEl = screen.getByText('No audio context');
    expect(errEl).toBeInTheDocument();
    expect(errEl).toHaveClass('settings-output-test-error');
  });

  it('shows camera test error text in settings-camera-test-error element', () => {
    cameraTestState = { ...cameraTestState, error: 'Camera access denied' };
    render(<DeviceConfigSection />);
    const errEl = screen.getByText('Camera access denied');
    expect(errEl).toBeInTheDocument();
    expect(errEl).toHaveClass('settings-camera-test-error');
  });

  it('output test button has settings-output-test-btn class', () => {
    outputTestState = { ...outputTestState, isTesting: true };
    render(<DeviceConfigSection />);
    const btn = screen.getByRole('button', { name: /Playing/ });
    expect(btn).toHaveClass('settings-output-test-btn');
  });

  it('camera test button has settings-camera-test-btn class', () => {
    cameraTestState = { ...cameraTestState, isTesting: true };
    render(<DeviceConfigSection />);
    const btn = screen.getByRole('button', { name: /Stop Preview/ });
    expect(btn).toHaveClass('settings-camera-test-btn');
  });

  it('keeps mic/output tests enabled in a voice call but leaves camera disabled', () => {
    mockConnectionState = 'connected';
    render(<DeviceConfigSection />);
    const testButtons = screen.getAllByRole('button', { name: /^Test$/ });
    expect(testButtons[0]).not.toBeDisabled();
    expect(testButtons[1]).not.toBeDisabled();
    expect(testButtons[2]).toBeDisabled();
  });

  it('disables the other audio test while one is already running', () => {
    mockLocalIsTesting = true;
    render(<DeviceConfigSection />);
    const testButtons = screen.getAllByRole('button', { name: /^Test$/ });
    expect(testButtons[0]).toBeDisabled();
    expect(testButtons[1]).toBeDisabled();
    expect(testButtons[2]).not.toBeDisabled();
  });
});
