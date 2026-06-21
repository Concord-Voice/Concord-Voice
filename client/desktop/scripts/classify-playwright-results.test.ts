import { describe, it, expect } from 'vitest';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';

// Resolve the script path relative to this test file so the test works
// regardless of which directory Vitest is invoked from (repo root vs.
// client/desktop). The classify script lives next to this test file.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SCRIPT = path.resolve(__dirname, 'classify-playwright-results.mjs');

function runClassify(reportContent) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'classify-'));
  try {
    const reportDir = path.join(tmpDir, 'client/desktop');
    fs.mkdirSync(reportDir, { recursive: true });
    if (reportContent !== null) {
      fs.writeFileSync(path.join(reportDir, 'test-results.json'), reportContent);
    }
    const outputFile = path.join(tmpDir, 'github_output');
    fs.writeFileSync(outputFile, '');

    const result = spawnSync('node', [SCRIPT], {
      cwd: tmpDir,
      env: { ...process.env, GITHUB_OUTPUT: outputFile },
      encoding: 'utf8',
    });

    const outputs: Record<string, string> = {};
    for (const line of fs.readFileSync(outputFile, 'utf8').split('\n')) {
      const [k, v] = line.split('=');
      if (k) outputs[k] = v;
    }

    return { exitCode: result.status, outputs, stderr: result.stderr };
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
}

const ALL_PASS = JSON.stringify({
  suites: [{ specs: [{ tests: [{ status: 'expected', results: [] }] }] }],
});

const SNAPSHOT_FAILURE_ONE = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [
                    { matcherResult: { name: 'toHaveScreenshot' }, message: 'diff > threshold' },
                  ],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

const SNAPSHOT_FAILURE_MANY = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ matcherResult: { name: 'toHaveScreenshot' } }],
                },
              ],
            },
          ],
        },
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ matcherResult: { name: 'toHaveScreenshot' } }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

const MIXED_FAILURE = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ matcherResult: { name: 'toHaveScreenshot' } }],
                },
              ],
            },
          ],
        },
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ message: 'Expected true to be false' }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

const ASSERTION_FAILURE = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ message: 'Expected element to be visible' }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

const TIMEOUT_FAILURE = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [{ message: 'Test timeout of 60000ms exceeded' }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

// Real-world Playwright output when a baseline file is missing entirely
// (Linux CI run against a tree with only Mac baselines). No matcherResult
// field — just an Error whose message starts with "A snapshot doesn't exist".
const SNAPSHOT_DOESNT_EXIST = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [
                    {
                      message:
                        "A snapshot doesn't exist at /path/to/login-dark-chromium-linux.png, writing actual.",
                    },
                  ],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

// Single spec containing two tests — one snapshot mismatch + one assertion
// failure. The earlier `.some()` shape returned true on the snapshot test
// and classified the whole spec snapshot-only, silently masking the real
// assertion failure. Flagged jointly by Gitar + Copilot (spec-level).
const MIXED_FAILURE_SAME_SPEC = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [{ errors: [{ matcherResult: { name: 'toHaveScreenshot' } }] }],
            },
            {
              status: 'unexpected',
              results: [{ errors: [{ message: 'Expected true to be false' }] }],
            },
          ],
        },
      ],
    },
  ],
});

// Single test with multiple results (retry attempts). One retry surfaced a
// snapshot error, another surfaced an assertion error. The result-level
// `.some()` shape would have masked the assertion. Extension of the
// previous fixture to the retry dimension (code-reviewer finding).
const MIXED_FAILURE_ACROSS_RETRIES = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                { errors: [{ matcherResult: { name: 'toHaveScreenshot' } }] }, // retry 1
                { errors: [{ message: 'Expected element to be visible' }] }, // retry 2
              ],
            },
          ],
        },
      ],
    },
  ],
});

// A single result carrying multiple errors at once — both a snapshot and a
// non-snapshot error in the same errors[] array. The innermost `.some()`
// would have matched on the snapshot error and classified the result
// snapshot-only.
const MIXED_FAILURE_SAME_RESULT = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'unexpected',
              results: [
                {
                  errors: [
                    { matcherResult: { name: 'toHaveScreenshot' } },
                    { message: 'Expected true to be false' },
                  ],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

// Playwright's "flaky" status — failed initially, passed on retry. Should
// be treated as PASSED (not a failure), so classification is 'none'.
// Documents the intentional skip in the failedSpecs filter (code-reviewer
// finding).
const FLAKY_STATUS = JSON.stringify({
  suites: [
    {
      specs: [
        {
          tests: [
            {
              status: 'flaky',
              results: [
                { errors: [{ message: 'Some transient error' }] },
                { errors: [] }, // passed on retry
              ],
            },
          ],
        },
      ],
    },
  ],
});

// Nested suite tree — `extractSpecs` must recurse into `suite.suites` and
// find specs at any depth. Verifies the recursion is correct (code-reviewer
// coverage gap finding).
const NESTED_SUITES = JSON.stringify({
  suites: [
    {
      suites: [
        {
          specs: [
            {
              tests: [
                {
                  status: 'unexpected',
                  results: [{ errors: [{ matcherResult: { name: 'toHaveScreenshot' } }] }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
});

const EMPTY_REPORT = JSON.stringify({ suites: [] });

describe('classify-playwright-results', () => {
  it('all-pass JSON → failure_class=none, exit 0', () => {
    const { exitCode, outputs } = runClassify(ALL_PASS);
    expect(outputs.failure_class).toBe('none');
    expect(outputs.failure_count).toBe('0');
    expect(exitCode).toBe(0);
  });

  it('single snapshot-mismatch → failure_class=snapshot-only, exit 0', () => {
    const { exitCode, outputs } = runClassify(SNAPSHOT_FAILURE_ONE);
    expect(outputs.failure_class).toBe('snapshot-only');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(0);
  });

  it("snapshot doesn't exist (no matcherResult) → failure_class=snapshot-only, exit 0", () => {
    const { exitCode, outputs } = runClassify(SNAPSHOT_DOESNT_EXIST);
    expect(outputs.failure_class).toBe('snapshot-only');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(0);
  });

  it('multiple snapshot-mismatches → failure_class=snapshot-only, exit 0', () => {
    const { exitCode, outputs } = runClassify(SNAPSHOT_FAILURE_MANY);
    expect(outputs.failure_class).toBe('snapshot-only');
    expect(outputs.failure_count).toBe('2');
    expect(exitCode).toBe(0);
  });

  it('mixed snapshot + assertion failure → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(MIXED_FAILURE);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('2');
    expect(exitCode).toBe(1);
  });

  it('single assertion failure → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(ASSERTION_FAILURE);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(1);
  });

  it('single timeout failure → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(TIMEOUT_FAILURE);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(1);
  });

  it('missing JSON file → failure_class=infrastructure, exit 1', () => {
    const { exitCode, outputs, stderr } = runClassify(null);
    expect(outputs.failure_class).toBe('infrastructure');
    expect(exitCode).toBe(1);
    expect(stderr).toMatch(/No Playwright JSON report/);
  });

  it('malformed JSON → failure_class=infrastructure, exit 1', () => {
    const { exitCode, outputs, stderr } = runClassify('{not valid json');
    expect(outputs.failure_class).toBe('infrastructure');
    expect(exitCode).toBe(1);
    expect(stderr).toMatch(/Failed to parse/);
  });

  it('empty JSON (no suites) → failure_class=none, exit 0', () => {
    const { exitCode, outputs } = runClassify(EMPTY_REPORT);
    expect(outputs.failure_class).toBe('none');
    expect(outputs.failure_count).toBe('0');
    expect(exitCode).toBe(0);
  });

  it('mixed snapshot + assertion within SAME spec → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(MIXED_FAILURE_SAME_SPEC);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(1);
  });

  it('mixed snapshot + assertion across RETRY RESULTS → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(MIXED_FAILURE_ACROSS_RETRIES);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(1);
  });

  it('mixed snapshot + assertion in SAME RESULT errors[] → failure_class=real-failure, exit 1', () => {
    const { exitCode, outputs } = runClassify(MIXED_FAILURE_SAME_RESULT);
    expect(outputs.failure_class).toBe('real-failure');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(1);
  });

  it('flaky test (failed then passed on retry) → failure_class=none, exit 0', () => {
    const { exitCode, outputs } = runClassify(FLAKY_STATUS);
    expect(outputs.failure_class).toBe('none');
    expect(outputs.failure_count).toBe('0');
    expect(exitCode).toBe(0);
  });

  it('nested suites (recursive extractSpecs) → snapshot-only classification works at depth', () => {
    const { exitCode, outputs } = runClassify(NESTED_SUITES);
    expect(outputs.failure_class).toBe('snapshot-only');
    expect(outputs.failure_count).toBe('1');
    expect(exitCode).toBe(0);
  });
});
