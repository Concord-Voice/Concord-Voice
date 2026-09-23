/**
 * The auth-screen wordmark shows the variant the active theme can read.
 *
 * This is the assertion jsdom cannot make: it applies no stylesheet, so both variants are
 * always "visible" there. Only a real cascade can say which one a user actually sees.
 *
 * The app opens at ConnectionSelector, which renders the wordmark with no auth, so '/' is
 * enough. The theme is driven the way the app drives it — by `data-theme` on the root —
 * and the schemes are chosen for what they stress: the base light block, Eclipse light
 * (the lowest-contrast light background, 10.56:1 against the #333333 fill), Defacto light
 * (pure #ffffff, where the original white wordmark vanishes completely), and a dark
 * scheme as the control.
 */
import { test, expect, type Page } from '@playwright/test';

async function setTheme(page: Page, scheme: string, theme: string | null): Promise<void> {
  await page.evaluate(
    ({ s, t }) => {
      const root = document.documentElement;
      root.setAttribute('data-scheme', s);
      if (t === null) root.removeAttribute('data-theme');
      else root.setAttribute('data-theme', t);
    },
    { s: scheme, t: theme }
  );
}

const dark = (page: Page) => page.locator('.concord-wordmark--dark-theme');
const light = (page: Page) => page.locator('.concord-wordmark--light-theme');

for (const scheme of ['concord', 'eclipse', 'defacto']) {
  test(
    `light theme shows the dark-lettered wordmark — ${scheme}`,
    { tag: '@renderer-only' },
    async ({ page }) => {
      await page.goto('/');
      await expect(dark(page)).toHaveCount(1);
      await setTheme(page, scheme, 'light');

      await expect(light(page)).toBeVisible();
      await expect(dark(page)).toBeHidden();
    }
  );
}

test(
  'dark theme shows the original white-lettered wordmark',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    await setTheme(page, 'concord', 'dark');

    await expect(dark(page)).toBeVisible();
    await expect(light(page)).toBeHidden();
  }
);

test(
  'an unset theme falls back to the original wordmark',
  { tag: '@renderer-only' },
  async ({ page }) => {
    // The hide rule is `:not([data-theme='light'])`, not `[data-theme='dark']`, precisely so
    // this case shows the asset every auth screen rendered before the fix — never neither.
    await page.goto('/');
    await setTheme(page, 'concord', null);

    await expect(dark(page)).toBeVisible();
    await expect(light(page)).toBeHidden();
  }
);

test(
  'exactly one wordmark is visible at a time, across a live theme switch',
  {
    tag: '@renderer-only',
  },
  async ({ page }) => {
    // Both hidden would be a missing logo; both shown would be a doubled one. Switching on a
    // live page covers the user flipping the theme toggle while the auth screen is open.
    await page.goto('/');
    for (const theme of ['light', 'dark', 'light']) {
      await setTheme(page, 'concord', theme);
      const visible =
        ((await dark(page).isVisible()) ? 1 : 0) + ((await light(page).isVisible()) ? 1 : 0);
      expect(visible, `visible wordmarks in ${theme}`).toBe(1);
    }
  }
);
