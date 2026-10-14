import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, within, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import AboutUpdateSection from '@/renderer/components/Settings/AboutUpdateSection';

vi.mock('virtual:concord-legal/license', () => ({
  default: '# License agreement\n\nConcord source license terms.',
}));
vi.mock('virtual:concord-legal/privacy-policy', () => ({
  default:
    '# Privacy details\n\n[Source license](../LICENSE)\n\n[License details](../LICENSE#license-agreement)\n\n| Item | Purpose |\n| --- | --- |\n| ID | Sign-in |',
}));
vi.mock('virtual:concord-legal/terms-of-service', () => ({ default: '# Terms agreement' }));
vi.mock('virtual:concord-legal/notice', () => ({ default: '# Software disclosure' }));

const originalScrollDescriptor = Object.getOwnPropertyDescriptor(
  HTMLElement.prototype,
  'scrollIntoView'
);
beforeEach(() => {
  resetAllStores();
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  if (originalScrollDescriptor)
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', originalScrollDescriptor);
  else delete (HTMLElement.prototype as Partial<HTMLElement>).scrollIntoView;
});

describe('About legal document navigation', () => {
  it('reveals a bundled document fragment and focuses its heading after the source mounts', async () => {
    render(<AboutUpdateSection />);
    const user = userEvent.setup();
    await user.click(screen.getByText('Client Info'));
    const privacy = screen.getByText('Privacy Policy').closest('details') as HTMLDetailsElement;
    const license = screen.getByText('License').closest('details') as HTMLDetailsElement;
    expect(license).not.toHaveAttribute('open');
    await user.click(within(privacy).getByText('Privacy Policy'));
    await user.click(within(privacy).getByRole('link', { name: 'License details' }));
    expect(license).toHaveAttribute('open');
    const heading = await within(license).findByRole('heading', { name: 'License agreement' });
    await waitFor(() => expect(heading).toHaveFocus());
    expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledWith({
      block: 'start',
      behavior: 'instant',
    });
  });

  it('opens a bundled License reference without leaving the in-app legal documents', async () => {
    render(<AboutUpdateSection />);
    const user = userEvent.setup();
    await user.click(screen.getByText('Client Info'));
    const privacy = screen.getByText('Privacy Policy').closest('details') as HTMLDetailsElement;
    const license = screen.getByText('License').closest('details') as HTMLDetailsElement;
    expect(license).not.toHaveAttribute('open');
    await user.click(within(privacy).getByText('Privacy Policy'));
    expect(within(privacy).getByRole('cell', { name: 'Sign-in' })).toBeInTheDocument();
    await user.click(within(privacy).getByRole('link', { name: 'Source license' }));
    expect(license).toHaveAttribute('open');
    expect(
      await within(license).findByRole('heading', { name: 'License agreement' })
    ).toBeInTheDocument();
  });
});
