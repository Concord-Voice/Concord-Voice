/**
 * Labels on fills the contrast ratchet cannot see.
 *
 * The ratchet measures stylesheet pairs. Two things it cannot see put labels
 * below 4.5:1 on #3514's branch, and review found both:
 *
 * - **A user's own colours, set inline.** Initials on another user's gradient
 *   took that raw gradient with `#fff` or the viewer's `--on-brand`. White on
 *   Hacker's green read 1.37:1. `resolveUserAccentColors` now returns a `fill`
 *   and `text` pair for initials; the raw `gradient` stays for surfaces with no
 *   text. This pins where `gradient` may still be read, so a new initials site
 *   cannot quietly take it.
 * - **Opacity.** A hover that fades a label and its fill blends both into the
 *   row behind them: `.avatar-circle` at 0.8 took Hacker light to 3.29:1.
 * - **A filter over a label.** A filter on an element post-processes every
 *   descendant: the inactive voice tile's `saturate(0.7) brightness(0.9)` took
 *   Morky's initials from 5.04:1 to about 3.49:1, and dimmed the name too.
 * - **A modifier that swaps the fill.** The ratchet pairs a rule's own colour
 *   and background, so a modifier class declaring only a background keeps the
 *   base rule's label unmeasured: the account-reset red kept the brand label at
 *   4.05:1 in Concord.
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { contrastRatio, stripComments, toRgb, winningDeclaration } from './contrastPairs';

const RENDERER = resolve(__dirname, '../../../src/renderer');

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const full = join(dir, name);
    if (statSync(full).isDirectory()) return sources(full);
    return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [full] : [];
  });
}

describe('identity fills', () => {
  it("reads a user's raw gradient only where no text sits on it", () => {
    const uses: Record<string, number> = {};
    for (const file of sources(join(RENDERER, 'components'))) {
      const count = (readFileSync(file, 'utf8').match(/\.gradient\b/g) ?? []).length;
      if (count > 0) uses[relative(RENDERER, file)] = count;
    }
    // Profile banners, the voice tile's banner and the scheme swatches carry no
    // text. Anything that draws initials takes `fill` and `text` instead.
    expect(uses).toEqual({
      'components/DirectMessages/DMProfileModal.tsx': 1,
      'components/Members/MemberProfileCard.tsx': 1,
      'components/Members/UserProfileModal.tsx': 1,
      'components/Settings/AppearanceSection.tsx': 1,
      'components/Voice/ParticipantTile.tsx': 1,
    });
  });
});

describe('labels that fade on hover', () => {
  it('never dims the fallback avatar circle', () => {
    const css = stripComments(readFileSync(join(RENDERER, 'components/Chat/Message.css'), 'utf8'));
    for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      const selectors = m[1].split(',').map((s) => s.trim());
      const dims = /(^|;)\s*opacity\s*:/.test(m[2]);
      for (const selector of selectors) {
        if (selector.includes('.avatar-circle') && selector.includes(':hover')) {
          expect(dims, `${selector} fades the initial and its fill`).toBe(false);
        }
      }
    }
  });
});

describe('filled buttons that fade on hover', () => {
  // A 0.9 fade blends a tuned pair into the panel behind it. These three were
  // left fading when the brand buttons moved to a lift.
  it.each([
    ['components/Channels/CreateChannelModal.css', '.btn-primary'],
    ['components/Settings/SettingsPage.css', '.btn-primary'],
    ['components/Auth/KeyRecoveryPrompt.css', '.key-recovery-prompt__danger'],
  ])('%s never fades %s', (file, button) => {
    const css = stripComments(readFileSync(join(RENDERER, file), 'utf8'));
    let hovers = 0;
    for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      for (const selector of m[1].split(',').map((s) => s.trim())) {
        if (!selector.includes(button) || !selector.includes(':hover')) continue;
        hovers += 1;
        expect(
          /(^|;)\s*(opacity|filter)\s*:/.test(m[2]),
          `${selector} fades its label and fill`
        ).toBe(false);
      }
    }
    expect(hovers, `${button} keeps a hover state`).toBeGreaterThan(0);
  });
});

describe('filters over labels', () => {
  it('dims only the voice tile surfaces that carry no text', () => {
    // A child cannot undo its parent's filter, so a filter on the tile, or on
    // anything holding the initials or the name, dims that text with it.
    const surfaces = [
      '.participant-tile__banner',
      '.participant-tile__avatar-img',
      '.participant-tile__video',
    ];
    const css = stripComments(
      readFileSync(join(RENDERER, 'components/Voice/ParticipantTile.css'), 'utf8')
    );
    let filters = 0;
    for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      if (!/(^|;)\s*filter\s*:/.test(m[2])) continue;
      for (const selector of m[1].split(',').map((s) => s.trim())) {
        filters += 1;
        expect(
          surfaces.some((surface) => selector.endsWith(surface)),
          `${selector} filters text`
        ).toBe(true);
      }
    }
    expect(filters, 'the inactive tile still recedes').toBeGreaterThan(0);
  });
});

describe('modifiers that replace the brand fill', () => {
  it('gives the account-reset red its own label', () => {
    const css = stripComments(readFileSync(join(RENDERER, 'components/Auth/Login.css'), 'utf8'));
    const body = /\.login-submit-btn--danger\s*\{([^}]*)\}/.exec(css)?.[1] ?? '';
    const fill = toRgb(
      winningDeclaration(body, /(?:^|[;\s])background(?:-color)?\s*:\s*([^;]+);/g)
    );
    const label = toRgb(winningDeclaration(body, /(?:^|[;\s])color\s*:\s*([^;]+);/g));
    expect(fill, 'the modifier declares its fill').not.toBeNull();
    expect(label, 'the modifier declares the label for that fill').not.toBeNull();
    expect(contrastRatio(label!, fill!)).toBeGreaterThanOrEqual(4.5);
  });
});

describe('rows that hold identity initials', () => {
  // Opacity on a row post-composites every label in it into the list behind
  // it, the initials pair included: Codex measured an offline member's black
  // initial on Morky dark's red-to-orange fill at about 1.9 to 2.4:1. Only the
  // imagery inside a row may dim.
  const rows: Array<[string, string[], string[]]> = [
    [
      'components/Members/MemberList.css',
      ['member-item', 'member-item--compact'],
      ['.member-item.offline', '.member-item--compact.invisible'],
    ],
    [
      'components/DirectMessages/DirectMessages.css',
      ['friend-item', 'friend-request-item', 'friend-request-outgoing'],
      ['.friend-item.offline'],
    ],
  ];

  it.each(rows)('%s never fades a row itself', (file, rowClasses, offlineStates) => {
    const css = stripComments(readFileSync(join(RENDERER, file), 'utf8'));
    const faded: string[] = [];
    const statesStyled = new Set<string>();
    for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
      for (const selector of m[1].split(',').map((s) => s.trim())) {
        for (const state of offlineStates) if (selector.includes(state)) statesStyled.add(state);
        if (!/(^|;)\s*(opacity|filter)\s*:/.test(m[2])) continue;
        const subject =
          selector
            .split(/\s+|>|\+|~/)
            .filter(Boolean)
            .at(-1) ?? '';
        if (rowClasses.some((row) => new RegExp(`\\.${row}(?![\\w-])`).test(subject))) {
          faded.push(selector);
        }
      }
    }
    expect(faded, 'rules that fade a whole row').toEqual([]);
    // A control, not the bug: an offline row must still look offline, so a
    // fix cannot simply delete the state's styling.
    expect([...statesStyled].sort(), 'offline states still styled').toEqual(
      [...offlineStates].sort()
    );
  });
});

describe('a button class shared between stylesheets', () => {
  // Server Settings' stylesheet is loaded lazily and also styled
  // .invite-generate-btn at the same specificity. Once Settings had been
  // opened, it restyled the invite modal's brand button everywhere: its hover
  // became --accent-primary on --bg-tertiary, 2.19:1 in Concord light.
  function stylesheets(dir: string): string[] {
    return readdirSync(dir).flatMap((name) => {
      const full = join(dir, name);
      if (statSync(full).isDirectory()) return stylesheets(full);
      return name.endsWith('.css') ? [full] : [];
    });
  }

  it('.invite-generate-btn is styled by the invite modal alone', () => {
    const owners = stylesheets(RENDERER)
      .filter((file) =>
        /\.invite-generate-btn(?![\w-])/.test(stripComments(readFileSync(file, 'utf8')))
      )
      .map((file) => relative(RENDERER, file));
    expect(owners).toEqual(['components/Servers/InviteToServerModal.css']);
  });

  it('Server Settings does not borrow the invite modal class', () => {
    // A settings button left on the modal's class takes the modal's brand
    // style instead of its own, whichever stylesheet then owns the class.
    const tsx = readFileSync(join(RENDERER, 'components/Servers/ServerSettingsPage.tsx'), 'utf8');
    expect(tsx).not.toMatch(/\binvite-generate-btn\b/);
  });
});
