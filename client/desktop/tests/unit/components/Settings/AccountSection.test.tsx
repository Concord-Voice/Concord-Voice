import { render, screen } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => ({ nsfwAuth: 'unknown' }),
}));

import AccountSection from '@/renderer/components/Settings/AccountSection';

describe('AccountSection', () => {
  beforeEach(() => resetAllStores());

  it('renders the NSFW Content Access collapsible section containing the gate', () => {
    render(<AccountSection />);
    expect(screen.getByText('NSFW Content Access')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /verify age/i })).toBeInTheDocument();
  });
});
