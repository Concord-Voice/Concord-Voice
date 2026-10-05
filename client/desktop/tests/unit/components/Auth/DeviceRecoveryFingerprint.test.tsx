import { render, screen } from '../../../test-utils';
import { beforeEach, describe, expect, it } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import DeviceRecoveryFingerprint from '@/renderer/components/Auth/DeviceRecoveryFingerprint';

beforeEach(() => resetAllStores());
describe('DeviceRecoveryFingerprint', () => {
  it('keeps eight ordered visible groups and names each character for screen readers', () => {
    render(<DeviceRecoveryFingerprint fingerprint="4EE9 732D B326 1892 958F FB75 3700 10A8" />);
    const group = screen.getByRole('group', { name: 'Recovery fingerprint' });
    expect(group).toBeInstanceOf(HTMLFieldSetElement);
    expect(group).not.toHaveAttribute('role');
    expect(group.children).toHaveLength(8);
    expect(screen.getAllByRole('img').map((node) => node.getAttribute('aria-label'))).toEqual([
      'Group 1: 4 E E 9',
      'Group 2: 7 3 2 D',
      'Group 3: B 3 2 6',
      'Group 4: 1 8 9 2',
      'Group 5: 9 5 8 F',
      'Group 6: F B 7 5',
      'Group 7: 3 7 0 0',
      'Group 8: 1 0 A 8',
    ]);
    expect(group).toHaveTextContent('4EE9732DB3261892958FFB75370010A8');
  });
  it.each([
    '',
    '4EE9 732D',
    '4ee9 732D B326 1892 958F FB75 3700 10A8',
    '4EE9 732D B326 1892 958F FB75 3700 !!!!',
  ])('withholds a malformed fingerprint', (fingerprint) => {
    render(<DeviceRecoveryFingerprint fingerprint={fingerprint} />);
    expect(screen.queryByRole('group', { name: 'Recovery fingerprint' })).not.toBeInTheDocument();
  });
});
