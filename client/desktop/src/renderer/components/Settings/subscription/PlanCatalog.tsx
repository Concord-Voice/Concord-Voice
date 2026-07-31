import React from 'react';
import CollapsibleSection from '../CollapsibleSection';

/**
 * Static snapshot of planned pricing. **Not** authoritative — the source of truth is the
 * marketing site, `concordvoice-com/src/pages/pricing.astro` and `teams.astro`.
 *
 * Snapshot taken 2026-07-29. Only ~7 of these figures are stated there literally
 * (Supersonic, Hypersonic-at-Mach-2, Wing-no-boost, Mach 1/2/3, Squadron-4/8/12-at-Mach-2);
 * the rest are the combinations that page computes client-side from `data-monthly` /
 * `data-yearly` delta attributes. Two rules reproduce every row: **yearly = 11 × monthly**
 * (one month free), and a boost upgrade adds that Mach tier's own price (Mach 1 → 2 = +$10,
 * Mach 2 → 3 = +$20).
 *
 * Nothing enforces this at build or test time and the two repos cannot move in one PR, so a
 * marketing price change silently stales every derived row here. Re-derive against the
 * source pages whenever pricing moves.
 */
type PriceRow = readonly [name: string, monthly: string, yearly: string];
type AfterburnerRow = readonly [
  boost: string,
  contributors: string,
  perContributor: string,
  pool: string,
];

const PERSONAL_PLANS = [
  ['Sonic', 'Free', 'Free'],
  ['Supersonic', '$8.99', '$98.89'],
  ['Hypersonic + Mach 1', '$11.99', '$131.89'],
  ['Hypersonic + Mach 2', '$21.99', '$241.89'],
  ['Hypersonic + Mach 3', '$41.99', '$461.89'],
  ['Wing, no boost', '$14.99', '$164.89'],
  ['Wing + Mach 1', '$19.98', '$219.78'],
  ['Wing + Mach 2', '$29.98', '$329.78'],
  ['Wing + Mach 3', '$49.98', '$549.78'],
] as const satisfies readonly PriceRow[];

const GROUP_PLANS = [
  ['Squadron-4 + Mach 1', '$32.99', '$362.89'],
  ['Squadron-4 + Mach 2', '$42.99', '$472.89'],
  ['Squadron-4 + Mach 3', '$62.99', '$692.89'],
  ['Squadron-8 + Mach 1', '$62.99', '$692.89'],
  ['Squadron-8 + Mach 2', '$72.99', '$802.89'],
  ['Squadron-8 + Mach 3', '$92.99', '$1,022.89'],
  ['Squadron-12 + Mach 1', '$94.99', '$1,044.89'],
  ['Squadron-12 + Mach 2', '$104.99', '$1,154.89'],
  ['Squadron-12 + Mach 3', '$124.99', '$1,374.89'],
] as const satisfies readonly PriceRow[];

const SERVER_PLANS = [
  ['Groundspeed', 'Free', 'Free'],
  ['Mach 1', '$4.99', '$54.89'],
  ['Mach 2', '$14.99', '$164.89'],
  ['Mach 3', '$34.99', '$384.89'],
] as const satisfies readonly PriceRow[];

const AFTERBURNER_EXAMPLES = [
  ['Mach 2', '2', '$8.03', '$16.05'],
  ['Mach 2', '3', '$5.45', '$16.36'],
  ['Mach 3', '5', '$7.52', '$37.58'],
  ['Mach 3', '7', '$5.46', '$38.20'],
] as const satisfies readonly AfterburnerRow[];

function priceTable(
  caption: string,
  nameHeader: string,
  rows: readonly PriceRow[]
): React.ReactNode {
  return (
    <div className="subscription-catalog-table-wrap">
      <table className="subscription-catalog-table">
        <caption>{caption}</caption>
        <thead>
          <tr>
            <th scope="col">{nameHeader}</th>
            <th scope="col">Monthly</th>
            <th scope="col">Yearly</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([name, monthly, yearly]) => (
            <tr key={name}>
              <th scope="row">{name}</th>
              <td>{monthly}</td>
              <td>{yearly}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const PlanCatalog: React.FC = () => (
  <>
    <CollapsibleSection id="section-personal-plans" title="Personal Plans">
      {priceTable('Planned personal subscription prices', 'Plan/configuration', PERSONAL_PLANS)}
      <p className="settings-section-description">
        Hypersonic includes Supersonic plus the selected Mach server boost. Wing includes two
        Supersonic seats; selecting a boost upgrades the primary seat to Hypersonic and adds the
        selected server price.
      </p>
    </CollapsibleSection>

    <CollapsibleSection id="section-group-plans" title="Group Plans">
      {priceTable('Planned group subscription prices', 'Plan/configuration', GROUP_PLANS)}
      <div className="subscription-catalog-table-wrap">
        <table className="subscription-catalog-table">
          <caption>Planned Afterburner pledge-pool examples</caption>
          <thead>
            <tr>
              <th scope="col">Boost</th>
              <th scope="col">Contributors</th>
              <th scope="col">Per contributor/month</th>
              <th scope="col">Pool/month</th>
            </tr>
          </thead>
          <tbody>
            {AFTERBURNER_EXAMPLES.map(([boost, contributors, perContributor, pool]) => (
              <tr key={`${boost}-${contributors}`}>
                <th scope="row">{boost}</th>
                <td>{contributors}</td>
                <td>{perContributor}</td>
                <td>{pool}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="settings-section-description">
        Nobody is charged until the pledge pool reaches its target.
      </p>
    </CollapsibleSection>

    <CollapsibleSection id="section-server-plans" title="Server Plans">
      {priceTable('Planned server subscription prices', 'Plan', SERVER_PLANS)}
    </CollapsibleSection>
  </>
);

export default PlanCatalog;
