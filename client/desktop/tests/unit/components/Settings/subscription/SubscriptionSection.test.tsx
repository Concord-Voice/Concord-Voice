import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { render, screen, userEvent, within } from '../../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../../mocks/server';
import { resetAllStores } from '../../../../helpers/store-helpers';
import SubscriptionSection from '@/renderer/components/Settings/subscription/SubscriptionSection';
import { useAuthStore } from '@/renderer/stores/authStore';

const STATUS_PATH = 'http://localhost:8080/api/v1/subscriptions/me';
const DISCLAIMER =
  'Subscriptions are coming in the near future to unlock the full Concord Voice experience. These planned options and prices may change before launch.';
const PERSONAL_COMPOSITION_NOTE =
  'Hypersonic includes Supersonic plus the selected Mach server boost. Wing includes two Supersonic seats; selecting a boost upgrades the primary seat to Hypersonic and adds the selected server price.';

const expectedPersonal = [
  ['Sonic', 'Free', 'Free'],
  ['Supersonic', '$8.99', '$98.89'],
  ['Hypersonic + Mach 1', '$11.99', '$131.89'],
  ['Hypersonic + Mach 2', '$21.99', '$241.89'],
  ['Hypersonic + Mach 3', '$41.99', '$461.89'],
  ['Wing, no boost', '$14.99', '$164.89'],
  ['Wing + Mach 1', '$19.98', '$219.78'],
  ['Wing + Mach 2', '$29.98', '$329.78'],
  ['Wing + Mach 3', '$49.98', '$549.78'],
] as const;

const expectedGroups = [
  ['Squadron-4 + Mach 1', '$32.99', '$362.89'],
  ['Squadron-4 + Mach 2', '$42.99', '$472.89'],
  ['Squadron-4 + Mach 3', '$62.99', '$692.89'],
  ['Squadron-8 + Mach 1', '$62.99', '$692.89'],
  ['Squadron-8 + Mach 2', '$72.99', '$802.89'],
  ['Squadron-8 + Mach 3', '$92.99', '$1,022.89'],
  ['Squadron-12 + Mach 1', '$94.99', '$1,044.89'],
  ['Squadron-12 + Mach 2', '$104.99', '$1,154.89'],
  ['Squadron-12 + Mach 3', '$124.99', '$1,374.89'],
] as const;

const expectedAfterburner = [
  ['Mach 2', '2', '$8.03', '$16.05'],
  ['Mach 2', '3', '$5.45', '$16.36'],
  ['Mach 3', '5', '$7.52', '$37.58'],
  ['Mach 3', '7', '$5.46', '$38.20'],
] as const;

const expectedServers = [
  ['Groundspeed', 'Free', 'Free'],
  ['Mach 1', '$4.99', '$54.89'],
  ['Mach 2', '$14.99', '$164.89'],
  ['Mach 3', '$34.99', '$384.89'],
] as const;

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('SubscriptionSection', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ tier: 'free', status: 'none' }, { status: 200 })
      )
    );
  });

  it('renders the disclaimer and five subscription sections in stable order', () => {
    render(<SubscriptionSection />);

    expect(screen.getByText(DISCLAIMER)).toBeInTheDocument();
    const sections = [...document.querySelectorAll('.subscription-section > details')];
    expect(sections).toHaveLength(5);
    expect(sections.map((node) => node.id)).toEqual([
      'section-current-plan',
      'section-personal-plans',
      'section-group-plans',
      'section-server-plans',
      'section-redeem-code',
    ]);
  });

  it('renders every planned price in independently labelled semantic tables', async () => {
    const user = userEvent.setup();
    render(<SubscriptionSection />);

    for (const [id, title] of [
      ['section-personal-plans', 'Personal Plans'],
      ['section-group-plans', 'Group Plans'],
      ['section-server-plans', 'Server Plans'],
    ] as const) {
      const details = document.getElementById(id);
      expect(details).toBeInstanceOf(HTMLDetailsElement);
      if (!(details as HTMLDetailsElement).open) {
        await user.click(within(details as HTMLElement).getByText(title));
      }
      expect(details).toHaveAttribute('open');
    }

    const personal = screen.getByRole('table', {
      name: 'Planned personal subscription prices',
    });
    expect(
      within(personal)
        .getAllByRole('columnheader')
        .map((cell) => cell.textContent)
    ).toEqual(['Plan/configuration', 'Monthly', 'Yearly']);
    assertPriceRows(personal, expectedPersonal);
    expect(screen.getByText(PERSONAL_COMPOSITION_NOTE)).toBeInTheDocument();

    const groups = screen.getByRole('table', { name: 'Planned group subscription prices' });
    expect(
      within(groups)
        .getAllByRole('columnheader')
        .map((cell) => cell.textContent)
    ).toEqual(['Plan/configuration', 'Monthly', 'Yearly']);
    assertPriceRows(groups, expectedGroups);

    const afterburner = screen.getByRole('table', {
      name: 'Planned Afterburner pledge-pool examples',
    });
    expect(
      within(afterburner)
        .getAllByRole('columnheader')
        .map((cell) => cell.textContent)
    ).toEqual(['Boost', 'Contributors', 'Per contributor/month', 'Pool/month']);
    expect(
      within(afterburner)
        .getAllByRole('row')
        .slice(1)
        .map((row) => [
          within(row).getByRole('rowheader').textContent,
          ...within(row)
            .getAllByRole('cell')
            .map((cell) => cell.textContent),
        ])
    ).toEqual(expectedAfterburner);

    const servers = screen.getByRole('table', { name: 'Planned server subscription prices' });
    expect(
      within(servers)
        .getAllByRole('columnheader')
        .map((cell) => cell.textContent)
    ).toEqual(['Plan', 'Monthly', 'Yearly']);
    assertPriceRows(servers, expectedServers);
  });

  it('offers no purchase, pledge, waitlist, checkout, or outbound action', () => {
    render(<SubscriptionSection />);

    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /buy|purchase|checkout|waitlist|pledge|upgrade/i })
    ).not.toBeInTheDocument();
  });
});

function assertPriceRows(
  table: HTMLElement,
  expected: readonly (readonly [name: string, monthly: string, yearly: string])[]
): void {
  for (const [name, monthly, yearly] of expected) {
    const row = within(table).getByRole('rowheader', { name }).closest('tr');
    expect(row).not.toBeNull();
    expect(
      within(row as HTMLTableRowElement)
        .getAllByRole('cell')
        .map((cell) => cell.textContent)
    ).toEqual([monthly, yearly]);
  }
}
