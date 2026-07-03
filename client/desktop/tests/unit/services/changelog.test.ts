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
