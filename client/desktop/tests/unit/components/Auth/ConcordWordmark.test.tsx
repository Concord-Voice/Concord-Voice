import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen } from '../../../test-utils';
import ConcordWordmark from '@/renderer/components/Auth/ConcordWordmark';

/**
 * What jsdom can and cannot say about this component.
 *
 * jsdom applies no stylesheet, so both variants are always "visible" here — whether the
 * RIGHT one shows for the active theme is a cascade question, asserted in real Chromium
 * by tests/e2e/auth-wordmark.spec.ts. These tests pin the markup contract and the asset
 * pair; a green run here says nothing about which logo a user sees.
 */

const LOGOS = resolve(__dirname, '../../../../public/branding/Concord-Voice/logos');

describe('ConcordWordmark', () => {
  it('renders both theme variants, each labelled, each carrying the call-site class', () => {
    render(<ConcordWordmark className="login-logo" />);

    const imgs = screen.getAllByAltText('Concord Voice');
    expect(imgs).toHaveLength(2);

    const [dark, light] = imgs;
    expect(dark).toHaveAttribute(
      'src',
      './branding/Concord-Voice/logos/main-logo-transparent-vector.svg'
    );
    expect(light).toHaveAttribute(
      'src',
      './branding/Concord-Voice/logos/main-logo-transparent-vector-light.svg'
    );

    // The call site's sizing class must land on BOTH, or the variant that shows in one
    // theme renders at intrinsic size — 2400px wide for this asset.
    for (const img of imgs) expect(img).toHaveClass('login-logo', 'concord-wordmark');
    expect(dark).toHaveClass('concord-wordmark--dark-theme');
    expect(light).toHaveClass('concord-wordmark--light-theme');
  });

  it('never gives the light variant an empty alt', () => {
    // An empty alt would read as "decorative" and leave light-theme screen-reader users
    // with an unlabelled logo. The hidden variant is display:none, which already removes
    // it from the accessibility tree, so duplicating the label costs nothing.
    render(<ConcordWordmark className="register-logo" />);
    for (const img of screen.getAllByRole('img')) {
      expect(img.getAttribute('alt')).toBe('Concord Voice');
    }
  });
});

describe('wordmark asset pair', () => {
  const dark = readFileSync(resolve(LOGOS, 'main-logo-transparent-vector.svg'), 'utf-8');
  const light = readFileSync(resolve(LOGOS, 'main-logo-transparent-vector-light.svg'), 'utf-8');

  it('the dark-theme asset actually contains white fills (guards a vacuous comparison)', () => {
    // Without this, a redesigned wordmark with no #ffffff at all would make the equality
    // below pass trivially while both files were wrong for a light background.
    expect(dark.match(/fill="#ffffff"/g)?.length ?? 0).toBeGreaterThan(0);
  });

  it('the light variant is the dark one with only its white fills darkened', () => {
    // The two files must move together. If the wordmark is ever redrawn and only one is
    // replaced, the auth screens show two different logos depending on theme — and
    // nothing else in the suite would notice. #333333 is already in the asset's own
    // palette (the moon glyph's detail paths), so the light variant adds no new colour.
    expect(light).toBe(dark.replaceAll('fill="#ffffff"', 'fill="#333333"'));
  });

  it('no white fill survives in the light variant', () => {
    expect(light).not.toContain('fill="#ffffff"');
  });
});

describe('ConcordWordmark.css', () => {
  const css = readFileSync(
    resolve(__dirname, '../../../../src/renderer/components/Auth/ConcordWordmark.css'),
    'utf-8'
  );

  it('hides the dark variant in the light theme', () => {
    expect(css).toContain("[data-theme='light'] .concord-wordmark--dark-theme");
  });

  it('hides the light variant in every theme that is not light', () => {
    // `:not(light)` rather than `[data-theme='dark']`: an unset or unexpected theme must
    // fall back to the original asset, which is what every auth screen showed before.
    expect(css).toContain(":root:not([data-theme='light']) .concord-wordmark--light-theme");
  });
});
