// Pure changelog parsing/selection for the post-update changelog modal (#1857).
// No DOM, no store, no IO — every function is unit-testable in isolation and
// fail-safe: malformed input yields empty results / null, never a throw.

export interface ChangelogSection {
  /** Comparable numeric triple parsed from the heading label, e.g. '0.2.0'. */
  version: string;
  /** Raw heading token inside the brackets, e.g. '0.2.0-Beta'. */
  label: string;
  /** ISO date from the heading when present. */
  date: string | null;
  /** Markdown body between this heading and the next version heading. */
  body: string;
}

// Matches: ## [0.2.1] — 2026-06-20 (optional trailing annotation)
// Dash variants tolerated: em/en dash or hyphen.
const HEADING_RE = /^## \[([^\]]+)\](?:\s*[—–-]+\s*(\d{4}-\d{2}-\d{2}))?/;
const VERSION_PREFIX_RE = /^(\d+\.\d+\.\d+)/;

export function parseChangelog(raw: string): ChangelogSection[] {
  const sections: ChangelogSection[] = [];
  let current: ChangelogSection | null = null;
  const bodyLines: string[] = [];

  const flush = () => {
    if (current) {
      current.body = bodyLines.join('\n').trim();
      sections.push(current);
    }
    bodyLines.length = 0;
  };

  for (const line of raw.split('\n')) {
    const heading = HEADING_RE.exec(line);
    if (heading) {
      flush();
      const versionMatch = VERSION_PREFIX_RE.exec(heading[1]);
      if (versionMatch) {
        current = {
          version: versionMatch[1],
          label: heading[1],
          date: heading[2] ?? null,
          body: '',
        };
      } else {
        current = null; // non-semver heading — skip its body too
      }
      continue;
    }
    if (current) bodyLines.push(line);
  }
  flush();
  return sections;
}

/**
 * Compare two x.y.z strings numerically. Negative when a < b, 0 when equal,
 * positive when a > b; null when either side is not a plain numeric triple.
 */
export function compareSemver(a: string, b: string): number | null {
  const parse = (v: string): number[] | null => {
    const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(v);
    return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
  };
  const pa = parse(a);
  const pb = parse(b);
  if (!pa || !pb) return null;
  for (let i = 0; i < 3; i++) {
    if (pa[i] !== pb[i]) return pa[i] - pb[i];
  }
  return 0;
}

/**
 * Sections with sinceExclusive < version <= untilInclusive, preserving the
 * file's newest-first order. Sections whose version fails to compare are skipped.
 */
export function sectionsBetween(
  sections: ChangelogSection[],
  sinceExclusive: string,
  untilInclusive: string
): ChangelogSection[] {
  return sections.filter((s) => {
    const aboveSince = compareSemver(s.version, sinceExclusive);
    const atOrBelowUntil = compareSemver(s.version, untilInclusive);
    return aboveSince !== null && atOrBelowUntil !== null && aboveSince > 0 && atOrBelowUntil <= 0;
  });
}

export type ChangelogDecision = { kind: 'none' } | { kind: 'record' } | { kind: 'show' };

/**
 * Startup decision table (spec §5.1): null lastSeen → record silently
 * (fresh install / first feature-carrying build, AC3 carve-out); equal →
 * nothing; upgrade → show; downgrade or unparseable stored value → record
 * silently (fail-safe).
 */
export function decideChangelogAction(lastSeen: string | null, current: string): ChangelogDecision {
  if (compareSemver(current, current) === null) return { kind: 'none' }; // current unparseable (dev builds)
  if (lastSeen === null) return { kind: 'record' };
  const cmp = compareSemver(lastSeen, current);
  if (cmp === null) return { kind: 'record' };
  if (cmp === 0) return { kind: 'none' };
  if (cmp > 0) return { kind: 'record' };
  return { kind: 'show' };
}
