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
  /**
   * The brand-voice summary zone: body text ABOVE the first `### ` category
   * heading. This is what the post-update modal shows — a short "what changed
   * for you" paragraph, not the full record.
   *
   * `body` remains the complete STE-flavored detail and stays the public
   * archival text. Empty string when a section has no preamble (every entry
   * predating this convention), which is the modal's signal to fall back to
   * `body` — so historical entries keep rendering exactly as before.
   */
  preamble: string;
}

// Matches: ## [0.2.1] — 2026-06-20 (optional trailing annotation)
// Dash variants tolerated: em/en dash or hyphen.
const HEADING_RE = /^## \[([^\]]+)\](?:\s*[—–-]+\s*(\d{4}-\d{2}-\d{2}))?/;
const VERSION_PREFIX_RE = /^(\d+\.\d+\.\d+)/;
// Keep a Changelog category heading — the boundary between the preamble zone
// and the detail zone. Exactly three hashes: `##` is the next version (already
// consumed by HEADING_RE) and `####` is a sub-heading inside the detail.
const CATEGORY_PREFIX = '### ';
// A leading blockquote is an editorial aside about the release itself ("versions
// 0.2.4–0.2.6 never reached you"), not a summary of what changed — five entries
// predating this convention open with one. Promoting those to modal copy would
// show the aside and DROP every change bullet, so a zone that is nothing but
// blockquote counts as absent. A zone mixing an aside with real prose is kept
// whole: the aside is context the reader wants alongside the summary.
const BLOCKQUOTE_RE = /^\s*>/;
// CommonMark fence opener — up to three leading spaces, then ``` or ~~~.
const FENCE_RE = /^\s{0,3}(```|~~~)/;

/**
 * Index of the line that opens the detail zone, or -1 when the body has none.
 *
 * Fence-aware: a fenced code block may legitimately contain a line beginning
 * `### ` (a Markdown sample, a shell comment). Treating that as the boundary
 * would truncate the preamble mid-thought and hand the modal a fragment.
 * CommonMark allows both ``` and ~~~ fences and a fence closes only with its OWN
 * marker, so a backtick line inside a tilde block is content rather than a close
 * — tracking the opening marker keeps that right. An unclosed fence swallows the
 * rest of the body, which is the safe direction: no boundary means an empty
 * preamble and a fall back to rendering the full body.
 */
function findCategoryBoundary(bodyLines: readonly string[]): number {
  let fenceMarker: string | null = null;
  for (let i = 0; i < bodyLines.length; i++) {
    const marker = FENCE_RE.exec(bodyLines[i])?.[1];
    if (marker) {
      // Open on the first marker seen; close only on that SAME marker, so a
      // ``` line inside a ~~~ block is content and leaves the fence open.
      if (fenceMarker === null) fenceMarker = marker;
      else if (fenceMarker === marker) fenceMarker = null;
      continue;
    }
    if (fenceMarker === null && bodyLines[i].startsWith(CATEGORY_PREFIX)) return i;
  }
  return -1;
}

/**
 * The brand-voice summary zone, or '' when the section has none.
 *
 * A body with no category heading is ALL detail, not a preamble — treating it as
 * one would let a pre-convention entry (a bare bullet list, a malformed section)
 * become the modal's entire summary. A zone holding only a blockquote is an
 * editorial aside about the release, not a summary of it, and likewise counts as
 * absent.
 */
function extractPreamble(bodyLines: readonly string[]): string {
  const boundary = findCategoryBoundary(bodyLines);
  if (boundary === -1) return '';
  const zone = bodyLines.slice(0, boundary);
  const hasProse = zone.some((l) => l.trim() !== '' && !BLOCKQUOTE_RE.test(l));
  return hasProse ? zone.join('\n').trim() : '';
}

export function parseChangelog(raw: string): ChangelogSection[] {
  const sections: ChangelogSection[] = [];
  let current: ChangelogSection | null = null;
  const bodyLines: string[] = [];

  const flush = () => {
    if (current) {
      current.body = bodyLines.join('\n').trim();
      current.preamble = extractPreamble(bodyLines);
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
          preamble: '',
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
