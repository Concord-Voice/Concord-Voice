import { render, screen, userEvent, within } from '../../../test-utils';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import SystemPermissionsSection from '@/renderer/components/Settings/SystemPermissionsSection';

let originalElectron: typeof globalThis.electron;
beforeEach(() => {
  resetAllStores();
  originalElectron = globalThis.electron;
});
afterEach(() => {
  globalThis.electron = originalElectron;
});

it('shows OS statuses and opens the OS settings for a denied microphone', async () => {
  const openSettings = vi.fn().mockResolvedValue(undefined);
  globalThis.electron = {
    ...globalThis.electron,
    checkAllPermissions: vi.fn().mockResolvedValue({
      microphone: 'denied',
      camera: 'granted',
      screen: 'restricted',
      secureStorage: 'granted',
      notifications: 'not-determined',
    }),
    openPermissionSettings: openSettings,
  } as typeof globalThis.electron;
  render(<SystemPermissionsSection />);
  const user = userEvent.setup();
  await user.click(screen.getByText('System Permissions'));
  const microphone = (await screen.findByText('Microphone')).closest(
    '.settings-row'
  ) as HTMLElement;
  expect(within(microphone).getByText('Denied')).toBeInTheDocument();
  await user.click(within(microphone).getByRole('button', { name: 'Open System Settings' }));
  expect(openSettings).toHaveBeenCalledWith('microphone');
});

it('shows unavailable OS permissions honestly outside Electron', async () => {
  globalThis.electron = {
    ...globalThis.electron,
    checkAllPermissions: undefined,
  } as typeof globalThis.electron;
  render(<SystemPermissionsSection />);
  await userEvent.setup().click(screen.getByText('System Permissions'));
  expect(await screen.findAllByText('Unavailable')).toHaveLength(5);
  expect(screen.queryByRole('button', { name: 'Request' })).not.toBeInTheDocument();
});
