import { render } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import ContentSafetyControls from '@/renderer/components/Settings/ContentSafetyControls';

vi.mock('@/renderer/services/gifProvider/klipyClient', () => ({
  klipyClient: {
    getCurrentCustomerId: vi.fn(() => null),
    rotateCustomerId: vi.fn(() => Promise.resolve('mock-rotated-id')),
  },
}));

describe('ContentSafetyControls', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the content safety subsection title', () => {
    const { getByText } = render(<ContentSafetyControls />);
    expect(getByText(/content safety/i)).toBeInTheDocument();
  });
});
