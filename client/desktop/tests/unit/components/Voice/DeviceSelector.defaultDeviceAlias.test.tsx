import React from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import DeviceSelector from '@/renderer/components/Voice/DeviceSelector';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

const devices = [
  { deviceId: 'default', kind: 'audioinput', label: 'Default - current device' },
  { deviceId: 'mic-usb', kind: 'audioinput', label: 'USB Microphone' },
  { deviceId: 'default', kind: 'audiooutput', label: 'Default - current device' },
  { deviceId: 'speaker-usb', kind: 'audiooutput', label: 'USB Speakers' },
  { deviceId: 'camera-front', kind: 'videoinput', label: 'Front Camera' },
  { deviceId: 'camera-usb', kind: 'videoinput', label: 'USB Camera' },
] as unknown as MediaDeviceInfo[];

beforeEach(() => {
  resetAllStores();
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    value: {
      enumerateDevices: vi.fn().mockResolvedValue(devices),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
  });
});

describe('DeviceSelector default alias duplication diagnosis', () => {
  it.each([
    {
      kind: 'audioinput' as const,
      physical: 'USB Microphone',
      value: 'mic-usb',
      systemType: 'microphone',
    },
    {
      kind: 'audiooutput' as const,
      physical: 'USB Speakers',
      value: 'speaker-usb',
      systemType: 'speaker',
    },
  ])(
    'renders one system-default choice for $systemType and retains physical devices',
    async ({ kind, physical, value }) => {
      render(<DeviceSelector kind={kind} />);

      await screen.findByRole('option', { name: physical });
      const options = screen.getAllByRole('option');
      expect(options.map((option) => (option as HTMLOptionElement).value)).toContain(value);
      const defaults = options.filter((option) => /default/i.test(option.textContent ?? ''));
      expect(defaults).toHaveLength(1);
    }
  );

  it('camera control renders one app default and retains both physical cameras', async () => {
    render(<DeviceSelector kind="videoinput" />);

    await screen.findByRole('option', { name: 'USB Camera' });
    const options = screen.getAllByRole('option');
    expect(options.filter((option) => /default/i.test(option.textContent ?? ''))).toHaveLength(1);
    expect(options.map((option) => (option as HTMLOptionElement).value)).toEqual([
      '',
      'camera-front',
      'camera-usb',
    ]);
  });

  it('renders a legacy literal default device ID as the app default selection', async () => {
    useVoiceStore.setState({ audioInputDeviceId: 'default' });
    render(<DeviceSelector kind="audioinput" />);

    await screen.findByRole('option', { name: 'USB Microphone' });
    expect((screen.getByRole('combobox', { name: 'Microphone' }) as HTMLSelectElement).value).toBe(
      ''
    );
  });
});
