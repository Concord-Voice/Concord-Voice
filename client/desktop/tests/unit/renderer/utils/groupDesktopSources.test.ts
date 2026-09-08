import {
  groupDesktopSources,
  type DesktopSourceLike,
} from '@/renderer/utils/ui/groupDesktopSources';

const src = (over: Partial<DesktopSourceLike> & Pick<DesktopSourceLike, 'id' | 'name'>) => ({
  thumbnail: 'thumb',
  appIcon: null,
  ...over,
});

describe('groupDesktopSources', () => {
  it('returns empty groups for empty input', () => {
    expect(groupDesktopSources([])).toEqual({ screens: [], apps: [], windows: [] });
  });

  it('keeps screens in input order — display order is meaningful', () => {
    const out = groupDesktopSources([
      src({ id: 'screen:1', name: 'Display 2' }),
      src({ id: 'screen:0', name: 'Display 1' }),
      src({ id: 'window:9', name: 'Notes' }),
    ]);
    expect(out.screens.map((s) => s.id)).toEqual(['screen:1', 'screen:0']);
  });

  it('groups windows sharing an appIcon into one application', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Inbox', appIcon: 'ICON_CHROME' }),
      src({ id: 'window:2', name: 'Docs', appIcon: 'ICON_CHROME' }),
    ]);
    expect(out.apps).toHaveLength(1);
    expect(out.apps[0].windows.map((w) => w.id)).toEqual(['window:1', 'window:2']);
    expect(out.apps[0].appIcon).toBe('ICON_CHROME');
  });

  it('separates windows with distinct appIcons', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Inbox', appIcon: 'ICON_CHROME' }),
      src({ id: 'window:2', name: 'main.ts', appIcon: 'ICON_CODE' }),
    ]);
    expect(out.apps).toHaveLength(2);
    expect(out.apps.every((a) => a.windows.length === 1)).toBe(true);
  });

  // appIcon is null whenever fetchWindowIcons is off or the platform has no icon.
  // Falling back to the title suffix keeps grouping useful instead of collapsing
  // every window into one bogus group.
  it('falls back to the trailing app segment when appIcon is missing', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'README.md - Visual Studio Code' }),
      src({ id: 'window:2', name: 'main.ts - Visual Studio Code' }),
    ]);
    expect(out.apps).toHaveLength(1);
    expect(out.apps[0].appName).toBe('Visual Studio Code');
    expect(out.apps[0].windows).toHaveLength(2);
  });

  it('handles the em-dash separator some platforms use', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Song — Spotify' }),
      src({ id: 'window:2', name: 'Album — Spotify' }),
    ]);
    expect(out.apps).toHaveLength(1);
    expect(out.apps[0].appName).toBe('Spotify');
  });

  // A window we cannot group is NOT an error — it becomes its own group, which the
  // picker renders as a direct share target. Graceful degradation, not a failure.
  it('gives an ungroupable window its own single-window group keyed on its name', () => {
    const out = groupDesktopSources([src({ id: 'window:7', name: 'Calculator' })]);
    expect(out.apps).toHaveLength(1);
    expect(out.apps[0].appName).toBe('Calculator');
    expect(out.apps[0].windows).toHaveLength(1);
  });

  it('never lets a null appIcon group unrelated windows together', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Calculator' }),
      src({ id: 'window:2', name: 'Activity Monitor' }),
    ]);
    expect(out.apps).toHaveLength(2);
  });

  it('exposes every window flat as well, for the Windows tab', () => {
    const out = groupDesktopSources([
      src({ id: 'screen:0', name: 'Display 1' }),
      src({ id: 'window:1', name: 'Inbox', appIcon: 'ICON_CHROME' }),
      src({ id: 'window:2', name: 'Docs', appIcon: 'ICON_CHROME' }),
    ]);
    expect(out.windows.map((w) => w.id)).toEqual(['window:1', 'window:2']);
  });

  it('sorts applications by name so the tab does not reshuffle between openings', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Zulip' }),
      src({ id: 'window:2', name: 'Alacritty' }),
    ]);
    expect(out.apps.map((a) => a.appName)).toEqual(['Alacritty', 'Zulip']);
  });

  it('gives two same-named groups distinct groupKeys, so React cannot collide them', () => {
    // The real shape: one window of an app reports an appIcon and keys on the icon,
    // a sibling window reports none and keys on its title suffix. Both DISPLAY
    // "Chrome". `key={app.appName}` therefore emits a duplicate React key and
    // reconciles the wrong tile when the source list changes.
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Docs - Chrome', appIcon: 'ICON_CHROME' }),
      src({ id: 'window:2', name: 'Mail - Chrome', appIcon: null }),
    ]);
    expect(out.apps).toHaveLength(2);
    expect(out.apps.map((a) => a.appName)).toEqual(['Chrome', 'Chrome']);
    const keys = out.apps.map((a) => a.groupKey);
    expect(new Set(keys).size).toBe(2);
  });

  it('prefers the icon key over the title suffix when both are available', () => {
    // Two windows of one app whose titles name DIFFERENT trailing words must still
    // group together when the icon says they are the same app.
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Inbox - Gmail', appIcon: 'ICON_CHROME' }),
      src({ id: 'window:2', name: 'Calendar - Google', appIcon: 'ICON_CHROME' }),
    ]);
    expect(out.apps).toHaveLength(1);
    expect(out.apps[0].windows).toHaveLength(2);
  });
});
