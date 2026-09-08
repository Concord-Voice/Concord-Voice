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
    expect(groupDesktopSources([])).toEqual({ screens: [], windows: [] });
  });

  it('keeps screens in input order — display order is meaningful', () => {
    const out = groupDesktopSources([
      src({ id: 'screen:1', name: 'Display 2' }),
      src({ id: 'screen:0', name: 'Display 1' }),
      src({ id: 'window:9', name: 'Notes' }),
    ]);
    expect(out.screens.map((s) => s.id)).toEqual(['screen:1', 'screen:0']);
  });

  it('keeps windows flat and in platform order', () => {
    const out = groupDesktopSources([
      src({ id: 'window:2', name: 'Docs' }),
      src({ id: 'screen:0', name: 'Display 1' }),
      src({ id: 'window:1', name: 'Inbox' }),
    ]);
    expect(out.windows.map((w) => w.id)).toEqual(['window:2', 'window:1']);
  });

  it('drops sources whose id prefix is not understood', () => {
    const out = groupDesktopSources([
      src({ id: 'tab:9', name: 'Some browser tab' }),
      src({ id: 'window:1', name: 'Notes' }),
    ]);
    expect(out.windows.map((w) => w.id)).toEqual(['window:1']);
    expect(out.screens).toEqual([]);
  });

  it('preserves appIcon on the source, which the individual card still renders', () => {
    const out = groupDesktopSources([src({ id: 'window:1', name: 'Inbox', appIcon: 'ICON' })]);
    expect(out.windows[0].appIcon).toBe('ICON');
  });

  // B1, converted. This began as a reproduction asserting `out.apps` had TWO
  // entries rather than one phantom "183x62" application. The fix deleted the
  // grouping outright, so the original assertion has no subject any more — the
  // oracle is unchanged (unrelated terminals must never be presented as one
  // application) but in a world with no groups it can only be expressed as the
  // absence of grouping. That is the durable form: if app grouping ever comes
  // back, this fails and whoever brings it back has to confront B1 again.
  it('exposes no app-name grouping at all', () => {
    const out = groupDesktopSources([
      src({ id: 'window:1', name: 'Concord-Voice-Alpha — -zsh — 183×62' }),
      src({ id: 'window:2', name: 'other-project — -zsh — 183×62' }),
    ]);

    expect(out).not.toHaveProperty('apps');
    // Both survive, with their titles untouched — no segment is ever treated as
    // an application name, so nothing can collapse them.
    expect(out.windows.map((w) => w.name)).toEqual([
      'Concord-Voice-Alpha — -zsh — 183×62',
      'other-project — -zsh — 183×62',
    ]);
  });
});
