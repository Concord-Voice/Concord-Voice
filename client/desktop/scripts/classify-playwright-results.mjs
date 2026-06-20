#!/usr/bin/env node
// Classify Playwright JSON reporter output into a failure class for the
// playwright.yml workflow's sticky-comment posting step.
//
// Failure classes (per spec §9.1 outcome matrix):
//   none           — no test failures; workflow passes
//   snapshot-only  — all failures are toHaveScreenshot mismatches; non-blocking, workflow passes
//   real-failure   — at least one non-snapshot failure; workflow fails (exit 1)
//   infrastructure — no JSON report or malformed JSON; workflow fails (exit 1)
//
// Outputs are written to $GITHUB_OUTPUT in the format expected by
// GitHub Actions environment files. Falls back to stdout if the env var
// is unset (local-dev case).
//
// See: [internal]specs/2026-05-29-1074-playwright-ci-design.md §6.5

import fs from 'node:fs';
import path from 'node:path';

const REPORT_PATH = path.join(process.cwd(), 'client/desktop/test-results.json');

function emitOutput(failureClass, failureCount) {
  const outputFile = process.env.GITHUB_OUTPUT;
  const line = `failure_class=${failureClass}\nfailure_count=${failureCount}\n`;
  if (outputFile) {
    fs.appendFileSync(outputFile, line);
  } else {
    process.stdout.write(line);
  }
}

function extractSpecs(suite) {
  const own = suite.specs || [];
  const nested = (suite.suites || []).flatMap(extractSpecs);
  return [...own, ...nested];
}

// Snapshot-class failures take two forms:
//
//   1. Matcher-based: error has `matcherResult.name === 'toHaveScreenshot'`.
//      This fires when a baseline EXISTS but the pixel diff exceeds the
//      configured threshold.
//
//   2. "Snapshot doesn't exist": Playwright throws a plain Error without
//      a matcherResult field when the baseline FILE is missing entirely.
//      The message starts with "A snapshot doesn't exist". This is the
//      expected Linux-CI behavior when only Mac-captured baselines have
//      been committed (the post-#1074 transition state).
//
// Both forms are non-blocking snapshot-class failures per spec §9.1.
const SNAPSHOT_MESSAGE_PATTERN = /A snapshot doesn't exist|Screenshot comparison failed/i;

// A spec is snapshot-class ONLY when EVERY error across every test, every
// retry result, and every individual error is a snapshot error. Earlier
// versions used nested `.some()` — that returned true if ANY error matched,
// so a spec mixing a snapshot mismatch with a real assertion failure (in
// the same result, across retries, or across tests within the spec) was
// silently misclassified as snapshot-only → exit 0 → red CI escaped to
// merge. Flagged by Gitar (spec-level) and Copilot (also spec-level) and
// extended to the result level by code-reviewer. The fix flattens all
// errors and requires every single one to be snapshot-class.
function isSnapshotFailure(spec) {
  const allErrors = (spec.tests || []).flatMap((t) =>
    (t.results || []).flatMap((r) => r.errors || []),
  );
  if (allErrors.length === 0) {
    // No errors anywhere in this spec — can't be snapshot-class either.
    // Reached only via defensive guard; in practice this function is
    // called on already-failed specs which have at least one error.
    return false;
  }
  return allErrors.every(
    (e) =>
      e.matcherResult?.name === 'toHaveScreenshot' ||
      (typeof e.message === 'string' && SNAPSHOT_MESSAGE_PATTERN.test(e.message)),
  );
}

if (!fs.existsSync(REPORT_PATH)) {
  process.stderr.write(`No Playwright JSON report at ${REPORT_PATH}\n`);
  emitOutput('infrastructure', 0);
  process.exit(1);
}

let report;
try {
  report = JSON.parse(fs.readFileSync(REPORT_PATH, 'utf8'));
} catch (err) {
  process.stderr.write(`Failed to parse Playwright JSON report: ${err.message}\n`);
  emitOutput('infrastructure', 0);
  process.exit(1);
}

const allSpecs = (report.suites || []).flatMap(extractSpecs);
// Playwright test statuses encountered in JSON reports:
//   'expected'   — test passed
//   'unexpected' — test failed and did not recover on retry
//   'flaky'      — test failed initially but passed on retry; treated as
//                  PASSED here (a recovered test is not a failure for the
//                  non-blocking visual-diff workflow)
//   'skipped'    — test was skipped
// Only 'unexpected' / 'failed' count as failures for classification.
const failedSpecs = allSpecs.filter((s) =>
  (s.tests || []).some((t) => t.status === 'unexpected' || t.status === 'failed'),
);

if (failedSpecs.length === 0) {
  emitOutput('none', 0);
  process.exit(0);
}

const allSnapshotOnly = failedSpecs.every(isSnapshotFailure);
if (allSnapshotOnly) {
  emitOutput('snapshot-only', failedSpecs.length);
  process.exit(0); // non-blocking per spec §9.1
}

emitOutput('real-failure', failedSpecs.length);
process.exit(1);
