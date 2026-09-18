/**
 * Regression guard: "Eligible audience" became a hover disclosure, and the way
 * it is hidden is an accessibility contract, not a style preference.
 *
 * The status-menu button's `aria-describedby` points into this subtree
 * (UserPanel.tsx), so the node must stay in the accessibility tree while it is
 * visually hidden. `display: none` and `visibility: hidden` both remove it —
 * and jsdom applies no CSS, so UserPanel.test.tsx's aria assertions pass just
 * as happily either way. Nothing else in the repo can see that regression.
 *
 * This proves nothing about whether the disclosure LOOKS right on hover. That
 * is a visual check.
 */
import { describe, it, expect } from 'vitest';
import { readCss, readSource, ruleBody } from '../../../helpers/cssRules';

const css = readCss('src/renderer/components/User/UserPanel.css');

describe('eligible-audience hover disclosure', () => {
  it('finds the rule it is about (vacuity guard)', () => {
    expect(css.length).toBeGreaterThan(0);
    expect(ruleBody(css, '.user-panel-activity-policy')).not.toBeNull();
  });

  it('hides it without removing it from the accessibility tree', () => {
    const body = ruleBody(css, '.user-panel-activity-policy') ?? '';
    expect(body).toMatch(/opacity:\s*0\s*;/);
    // The two that would break aria-describedby.
    expect(body).not.toMatch(/display:\s*none/);
    expect(body).not.toMatch(/visibility:\s*hidden/);
  });

  it('keeps it out of flow so revealing it cannot reflow the panel', () => {
    const body = ruleBody(css, '.user-panel-activity-policy') ?? '';
    expect(body).toMatch(/position:\s*absolute\s*;/);
    // Invisible but hit-testable would swallow clicks meant for the panel.
    expect(body).toMatch(/pointer-events:\s*none\s*;/);
  });

  it('triggers on the button, since a zero-opacity node cannot reveal itself', () => {
    expect(css).toMatch(/\.user-panel-menu-btn:hover\s+\.user-panel-activity-policy/);
    expect(css).toMatch(/\.user-panel-menu-btn:focus-visible\s+\.user-panel-activity-policy/);
  });

  /**
   * The assertion above is PRESENCE, and presence is what let the previous
   * trigger ship dead: it named `.user-panel-info`, a `<span>` with no
   * focusable descendant, so `:focus-within` matched nothing reachable and a
   * keyboard user could never see the line. The selector text was there and the
   * test was green.
   *
   * This pair is the PROPERTY that made it dead, read from the component rather
   * than the stylesheet: the element the CSS keys focus on must be the one that
   * actually receives focus. Still text-level -- it cannot prove the rule paints
   * -- but it fails on the specific mistake rather than on the spelling.
   */
  it('keys focus on an element that can actually take focus', () => {
    const tsx = readSource('src/renderer/components/User/UserPanel.tsx');

    // The trigger class sits on a <button>: focusable, so :focus-visible fires.
    expect(tsx).toMatch(/<button[^>]*className=\{`user-panel-menu-btn/);

    // The class it used to key on is a <span>, and every descendant of it is
    // one too -- which is precisely why :focus-within could not match.
    expect(tsx).toMatch(/<span className="user-panel-info">/);
    expect(css).not.toMatch(/\.user-panel-info:focus-within/);
  });

  it('drops the reveal transition under reduced motion', () => {
    // Reachable only since the rule reader learned to descend into at-rules;
    // before that this assertion would have passed against a missing rule.
    const body = ruleBody(css, '.user-panel-activity-policy') ?? '';
    expect(body).toMatch(/transition:\s*none/);
  });
});
