/**
 * Regression: the DM profile dialog rendered at viewport 0,0 and its close
 * button was mostly unclickable.
 *
 * One cause, both symptoms. A modal <dialog> is centred purely by the UA
 * sheet's `dialog { margin: auto }`, and styles/index.css opens with a global
 * `* { margin: 0 }` that outranks it, so both axes collapsed to the start
 * edge. With the dialog at top:0 the close button's 28px circle occupied
 * viewport y 12-40, and the app's custom titlebar is 32px of
 * `-webkit-app-region: drag`, which swallows pointer input regardless of paint
 * order -- leaving only the bottom arc live, narrowed further by border-radius.
 *
 * WHAT THIS TEST CANNOT SEE, stated plainly: jsdom performs no layout, so
 * nothing here proves the dialog is centred or the button is hittable. It
 * asserts only that the declaration which restores centring is still present.
 * That is a deletion guard, not a proof -- the real check is visual, and this
 * file exists because a dead or missing CSS rule is otherwise invisible to
 * every automated check in the repo.
 */
import { describe, it, expect } from 'vitest';
import { readCss, ruleBody } from '../../../helpers/cssRules';

const css = readCss('src/renderer/components/DirectMessages/DMProfileModal.css');

describe('DM profile dialog centring', () => {
  it('reads the stylesheet and finds the dialog rule (vacuity guard)', () => {
    // Without this, a renamed selector or moved file makes every assertion
    // below pass against an empty string.
    expect(css.length).toBeGreaterThan(0);
    expect(ruleBody(css, '.dm-profile-modal-container')).not.toBeNull();
  });

  it('restates margin on the dialog, which the global * { margin: 0 } removes', () => {
    const body = ruleBody(css, '.dm-profile-modal-container') ?? '';
    // `auto` is what the UA sheet uses; any fixed value would re-pin a corner.
    expect(body).toMatch(/margin:\s*auto\s*;/);
  });

  it('confirms the global reset that makes the restatement necessary still exists', () => {
    // If this ever stops being true the rule above becomes redundant rather
    // than load-bearing, and the comment on it would start lying. Fail here so
    // that gets re-read rather than silently rotting.
    const global = readCss('src/renderer/styles/index.css');
    expect(ruleBody(global, '*')).toMatch(/margin:\s*0\s*;/);
  });
});
