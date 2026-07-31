import { describe, it, expect } from 'vitest';
import {
  parseChangelog,
  compareSemver,
  sectionsBetween,
  decideChangelogAction,
} from '../../../src/renderer/services/changelog';

const FIXTURE = `# Changelog

All notable changes to Concord Voice will be documented in this file.

## [0.2.21] — 2026-07-02

### Fixed

- **Something** ([#2000](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2000)) — detail.

## [0.2.20] — 2026-06-30 (hotfix)

### Added

- Another thing.

## [0.2.0-Beta] — 2026-06-20 (Phase 2 — Beta release)

### Added

- Beta rollup.
`;

describe('parseChangelog', () => {
  it('parses version, label, date, and body for each section', () => {
    const sections = parseChangelog(FIXTURE);
    expect(sections).toHaveLength(3);
    expect(sections[0]).toMatchObject({ version: '0.2.21', label: '0.2.21', date: '2026-07-02' });
    expect(sections[0].body).toContain('**Something**');
    expect(sections[1]).toMatchObject({ version: '0.2.20', label: '0.2.20', date: '2026-06-30' });
  });

  it('parses suffixed labels to their numeric triple ([0.2.0-Beta] → 0.2.0)', () => {
    const sections = parseChangelog(FIXTURE);
    expect(sections[2]).toMatchObject({ version: '0.2.0', label: '0.2.0-Beta' });
  });

  it('returns [] on malformed / empty input without throwing', () => {
    expect(parseChangelog('')).toEqual([]);
    expect(parseChangelog('no headings here')).toEqual([]);
    expect(parseChangelog('## [not-a-version]\nbody')).toEqual([]);
  });
});

describe('parseChangelog — preamble zone (post-update modal copy)', () => {
  const WITH_PREAMBLE = `# Changelog

## [0.2.40] — 2026-08-02

Sign-in got steadier this release, and screen sharing stops
dropping to a blur when someone joins mid-call.

### Fixed

- **A thing** ([#1](https://example.com/1)) — long STE-flavored detail that the
  modal must not render.

### Security

- **Another thing** ([#2](https://example.com/2)) — more detail.

## [0.2.39] — 2026-08-01

### Fixed

- Legacy entry with no preamble.
`;

  it('captures body text above the first ### as the preamble', () => {
    const [latest] = parseChangelog(WITH_PREAMBLE);
    expect(latest.preamble).toBe(
      'Sign-in got steadier this release, and screen sharing stops\ndropping to a blur when someone joins mid-call.'
    );
  });

  it('excludes every category and bullet from the preamble', () => {
    const [latest] = parseChangelog(WITH_PREAMBLE);
    expect(latest.preamble).not.toContain('### ');
    expect(latest.preamble).not.toContain('**A thing**');
    expect(latest.preamble).not.toContain('**Another thing**');
  });

  it('keeps body as the complete detail — the preamble is additive, not a replacement', () => {
    const [latest] = parseChangelog(WITH_PREAMBLE);
    expect(latest.body).toContain('Sign-in got steadier');
    expect(latest.body).toContain('### Security');
    expect(latest.body).toContain('**Another thing**');
  });

  it('yields an empty preamble for entries predating the convention', () => {
    const sections = parseChangelog(WITH_PREAMBLE);
    expect(sections[1].preamble).toBe('');
    // Fallback contract: the modal renders `preamble || body`, so an empty
    // preamble must leave a non-empty body or the section renders blank.
    expect(sections[1].body).toContain('Legacy entry');
  });

  it('every section of the real fixture has an empty preamble (pre-convention)', () => {
    for (const s of parseChangelog(FIXTURE)) {
      expect(s.preamble).toBe('');
    }
  });

  it('treats a body with no category heading as detail, never as a preamble', () => {
    // Guards the failure mode where a malformed or bullet-only section would
    // otherwise become the modal's entire summary.
    const [only] = parseChangelog('## [0.3.0] — 2026-09-01\n\n- A bare bullet, no category.\n');
    expect(only.preamble).toBe('');
    expect(only.body).toContain('A bare bullet');
  });

  it('ignores a blockquote-only zone — an editorial aside is not modal copy', () => {
    // Five pre-convention entries (0.2.7, 0.2.1, 0.2.0-Beta, 0.1.40, 0.1.39) open
    // with a "> versions X–Y never reached you" note. Promoting one to modal copy
    // would show the aside and DROP every change bullet for that version.
    const [only] = parseChangelog(
      '## [0.2.7] — 2026-06-23\n\n> Versions 0.2.4–0.2.6 never reached you.\n\n### Added\n\n- The real change.\n'
    );
    expect(only.preamble).toBe('');
    expect(only.body).toContain('The real change.');
  });

  it('keeps an aside that sits alongside real prose — context the reader wants', () => {
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\n> Versions 0.2.9–0.2.11 never shipped.\n\nSign-in got steadier.\n\n### Fixed\n\n- Detail.\n'
    );
    expect(only.preamble).toContain('never shipped');
    expect(only.preamble).toContain('Sign-in got steadier.');
  });

  it('every 0.2.x entry in the REAL CHANGELOG.md has a non-blank preamble', async () => {
    // The modal renders `preamble || body`. An entry that gained detail bullets
    // without gaining a preamble renders its whole body, which is the wall of text
    // the preamble exists to avoid. 0.1.x deliberately has none and falls back.
    const raw = (await import('virtual:concord-changelog')).default;
    const missing = parseChangelog(raw)
      .filter((s) => s.version.startsWith('0.2.'))
      .filter((s) => s.preamble.trim() === '')
      .map((s) => s.label);
    expect(missing, `0.2.x entries with no preamble: ${missing.join(', ')}`).toEqual([]);
  });

  it('no entry in the REAL CHANGELOG.md is summarized by a blockquote-only aside', async () => {
    // Fixture-only coverage missed this: the committed changelog already had five
    // blockquote zones the fixture did not model. Import the same virtual module
    // the modal imports, so this checks what actually ships through the real path.
    const raw = (await import('virtual:concord-changelog')).default;
    const sections = parseChangelog(raw);
    expect(sections.length).toBeGreaterThan(0);
    for (const s of sections) {
      if (s.preamble === '') continue; // falls back to body — the safe path
      const prose = s.preamble.split('\n').filter((l) => l.trim() !== '' && !/^\s*>/.test(l));
      expect(prose.length, `[${s.label}] preamble is blockquote-only`).toBeGreaterThan(0);
    }
  });

  it('ignores a ### inside a fenced code block when finding the boundary', () => {
    // A fenced block may legitimately contain a line beginning `### ` (a Markdown
    // sample, a shell comment). Treating it as the boundary truncates the preamble
    // mid-thought and hands the modal a fragment.
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\nSummary opens here.\n\n```md\n### Not a real category\n```\n\nAnd continues after the sample.\n\n### Fixed\n\n- Real detail.\n'
    );
    expect(only.preamble).toContain('Summary opens here.');
    expect(only.preamble).toContain('And continues after the sample.');
    expect(only.preamble).not.toContain('Real detail.');
    expect(only.body).toContain('### Fixed');
  });

  it('treats ``` inside a ~~~ fence as content, not a fence close', () => {
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\nSummary.\n\n~~~md\n```\n### Still inside the tilde fence\n```\n~~~\n\n### Fixed\n\n- Detail.\n'
    );
    expect(only.preamble).toContain('Summary.');
    expect(only.preamble).not.toContain('Detail.');
    expect(only.body).toContain('### Fixed');
  });

  it('falls back to the body when a fence is never closed', () => {
    // Safe direction: an unclosed fence swallows the rest, leaving no boundary,
    // so the section renders its full body rather than a truncated fragment.
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\nSummary.\n\n```\nunclosed\n\n### Fixed\n\n- Detail.\n'
    );
    expect(only.preamble).toBe('');
    expect(only.body).toContain('Detail.');
  });

  it('still finds the boundary after a closed fence', () => {
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\nSummary.\n\n```\ncode\n```\n\n### Fixed\n\n- Detail.\n'
    );
    expect(only.preamble).toContain('Summary.');
    expect(only.preamble).not.toContain('Detail.');
  });

  it('splits on the first ### only, so a #### sub-heading stays in the detail', () => {
    const [only] = parseChangelog(
      '## [0.3.0] — 2026-09-01\n\nSummary line.\n\n### Fixed\n\n#### Subsection\n\n- Detail.\n'
    );
    expect(only.preamble).toBe('Summary line.');
    expect(only.body).toContain('#### Subsection');
  });
});

describe('compareSemver', () => {
  it('orders numerically, not lexically', () => {
    expect(compareSemver('0.2.9', '0.2.21')).toBeLessThan(0);
    expect(compareSemver('0.2.21', '0.2.9')).toBeGreaterThan(0);
    expect(compareSemver('0.2.21', '0.2.21')).toBe(0);
    expect(compareSemver('1.0.0', '0.9.9')).toBeGreaterThan(0);
  });

  it('returns null on unparseable input', () => {
    expect(compareSemver('abc', '0.2.1')).toBeNull();
    expect(compareSemver('0.2', '0.2.1')).toBeNull();
  });
});

describe('sectionsBetween', () => {
  const sections = parseChangelog(FIXTURE);

  it('selects sinceExclusive < v <= untilInclusive, newest-first', () => {
    const out = sectionsBetween(sections, '0.2.0', '0.2.21');
    expect(out.map((s) => s.version)).toEqual(['0.2.21', '0.2.20']);
  });

  it('excludes the since version and includes the until version', () => {
    const out = sectionsBetween(sections, '0.2.20', '0.2.21');
    expect(out.map((s) => s.version)).toEqual(['0.2.21']);
  });

  it('matches a suffixed label section by numeric version (0.1.65 → 0.2.0 upgrade)', () => {
    const out = sectionsBetween(sections, '0.1.65', '0.2.0');
    expect(out.map((s) => s.label)).toEqual(['0.2.0-Beta']);
  });

  it('returns [] when nothing is in range', () => {
    expect(sectionsBetween(sections, '0.2.21', '0.2.22')).toEqual([]);
  });
});

describe('decideChangelogAction', () => {
  it('records silently on fresh install (null lastSeen)', () => {
    expect(decideChangelogAction(null, '0.2.21')).toEqual({ kind: 'record' });
  });
  it('does nothing when versions match', () => {
    expect(decideChangelogAction('0.2.21', '0.2.21')).toEqual({ kind: 'none' });
  });
  it('shows on upgrade', () => {
    expect(decideChangelogAction('0.2.20', '0.2.21')).toEqual({ kind: 'show' });
  });
  it('records silently on downgrade (fail-safe)', () => {
    expect(decideChangelogAction('0.2.22', '0.2.21')).toEqual({ kind: 'record' });
  });
  it('records silently when stored value is unparseable', () => {
    expect(decideChangelogAction('garbage', '0.2.21')).toEqual({ kind: 'record' });
  });
  it('does nothing when current version is unparseable', () => {
    expect(decideChangelogAction('0.2.20', 'dev')).toEqual({ kind: 'none' });
  });
});
