/**
 * CI path-filter coverage for cross-workspace imports.
 *
 * `cameraLayerPolicyParity.test.ts` lives in this workspace but imports the
 * DESKTOP copy of the camera layer ladder, because pinning the two
 * implementations in agreement is the entire point of that file. CI, however,
 * gates the media-plane test job on a path filter that listed only
 * `services/media-plane/**` — so a PR changing only the desktop policy skipped
 * the one test that guards it, and the guard was structurally blind to half of
 * what it guards.
 *
 * That is not a hypothetical: replacing the desktop copy's `Math.max(w, h)`
 * with a bare width still passes the desktop-side policy suite, whose own
 * fixtures are landscape-only. The parity suite is what catches it, and the
 * parity suite would not have run. Found by Codex on #3277.
 *
 * A comment in two workflow files cannot hold this — the next cross-workspace
 * import would be added without one, and nothing would say so. This test
 * derives the requirement from the imports themselves, so the filter has to
 * follow the code rather than the other way round.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, resolve, relative, sep } from 'node:path';

const TESTS_DIR = resolve(__dirname);
const MEDIA_PLANE_ROOT = resolve(__dirname, '..');
const REPO_ROOT = resolve(__dirname, '..', '..', '..');

const WORKFLOWS = ['.github/workflows/build.yml', '.github/workflows/pr-ci.yml'] as const;

/** Every `.test.ts` under this directory, recursively. */
function testFiles(dir: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      found.push(...testFiles(full));
      continue;
    }
    if (entry.endsWith('.test.ts')) found.push(full);
  }
  return found;
}

/**
 * Relative-specifier imports that resolve OUTSIDE services/media-plane.
 *
 * Deliberately ignores bare specifiers (`vitest`, `node:fs`) and the `@/` alias
 * — the first are packages, the second resolves inside this workspace by
 * config. Only a `../`-escape can reach another workspace's source.
 */
function crossWorkspaceImports(file: string): string[] {
  const source = readFileSync(file, 'utf8');
  // BOTH quote styles. Prettier enforces single quotes here, so a double-quoted
  // import does not survive formatting today — but a guard that depends on a
  // formatting convention has the exact blind spot it exists to close: the
  // import would go undetected and this test would pass while missing it
  // (Gitar, #3277).
  const specifiers = [...source.matchAll(/from\s+['"]([^'"]+)['"]/g)].map((match) => match[1]);
  const escaped: string[] = [];

  for (const specifier of specifiers) {
    if (!specifier.startsWith('.')) continue;
    const resolved = resolve(file, '..', specifier);
    if (resolved.startsWith(MEDIA_PLANE_ROOT + sep)) continue;
    // Normalise to a repo-relative path with forward slashes, and restore the
    // .ts extension the specifier omits.
    const repoRelative = relative(REPO_ROOT, resolved).split(sep).join('/');
    escaped.push(repoRelative.endsWith('.ts') ? repoRelative : `${repoRelative}.ts`);
  }
  return escaped;
}

/**
 * The quoted path list under a workflow filter's `media-plane-tests:` key.
 *
 * That key, NOT `media-plane`. The latter also gates `media-plane-docker-warm`,
 * a 4-vCPU cache-warm Docker build whose context is `services/media-plane` — so
 * widening it to cover this workspace's cross-workspace imports dragged an
 * unrelated 30-minute build onto every desktop-only change to the imported file
 * (Codex, #3277). The two questions have separate flags; this guard asks the one
 * that gates the TEST job.
 */
function mediaPlaneTestFilterPaths(workflow: string): string[] {
  const source = readFileSync(join(REPO_ROOT, workflow), 'utf8');
  const lines = source.split('\n');
  const start = lines.findIndex((line) => /^\s{12}media-plane-tests:\s*$/.test(line));
  expect(start, `no media-plane-tests filter block in ${workflow}`).toBeGreaterThan(-1);

  const paths: string[] = [];
  for (const line of lines.slice(start + 1)) {
    // The block ends at the next key at the same indentation.
    if (/^\s{12}\S/.test(line)) break;
    const match = /^\s+-\s+'([^']+)'\s*$/.exec(line);
    if (match) paths.push(match[1]);
  }
  return paths;
}

/**
 * Whether a `paths-filter` glob covers an exact repo-relative file path.
 *
 * Models exactly TWO shapes — an exact path, and a trailing `/**` prefix — which
 * is the entire vocabulary both filters use today. `paths-filter` accepts far
 * more (`*.ts`, a leading `**\/`, negations), and this function does not
 * pretend to.
 *
 * The narrowness is stated rather than silently absorbed. An unmodelled pattern
 * simply covers nothing here, which would surface as "import X is uncovered" —
 * a true statement about this function and a confusing one about the workflow,
 * sending the reader to debug the wrong file (Gitar, #3277). `supportedShape`
 * below turns that into an explicit failure naming the pattern instead.
 */
function filterCovers(pattern: string, path: string): boolean {
  if (pattern === path) return true;
  if (pattern.endsWith('/**')) return path.startsWith(pattern.slice(0, -2));
  return false;
}

/** Whether this guard's coverage model can reason about a pattern at all. */
function supportedShape(pattern: string): boolean {
  if (pattern.endsWith('/**')) return !pattern.slice(0, -3).includes('*');
  return !pattern.includes('*');
}

/**
 * The `if:` block of one job, as raw text.
 *
 * Text rather than a parsed tree because neither `yaml` nor `js-yaml` is a
 * dependency of this workspace, and adding one to assert five string literals
 * is a poor trade. The block is delimited the same way `mediaPlaneTestFilterPaths`
 * delimits the filter: start at the job key, stop at the next key of equal
 * indentation.
 */
function jobIfBlock(workflowSource: string, jobName: string): string {
  const lines = workflowSource.split('\n');
  const start = lines.findIndex((line) => line === `  ${jobName}:`);
  expect(start, `no job named ${jobName}`).toBeGreaterThan(-1);

  const body: string[] = [];
  let inIf = false;
  for (const line of lines.slice(start + 1)) {
    if (/^ {2}\S/.test(line)) break; // next job
    if (/^ {4}if:/.test(line)) {
      inIf = true;
      body.push(line);
      continue;
    }
    if (inIf) {
      // The `if:` value ends at the next key at the same indentation.
      if (/^ {4}\S/.test(line)) break;
      body.push(line);
    }
  }
  expect(body.length, `job ${jobName} has no if: block`).toBeGreaterThan(0);
  return body.join('\n');
}

describe('CI path filters cover this workspace real dependencies', () => {
  it('finds the cross-workspace imports it exists to protect', () => {
    const all = testFiles(TESTS_DIR).flatMap(crossWorkspaceImports);
    // Vacuity floor. With zero escaping imports every assertion below is
    // trivially satisfied, and this guard would pass while guarding nothing —
    // the exact failure mode it was written in response to.
    expect(all.length).toBeGreaterThan(0);
    expect(all).toContain('client/desktop/src/renderer/services/voice/remoteVideoLayerPolicy.ts');
  });

  it.each(WORKFLOWS)('%s gates the media-plane TEST job on every one of them', (workflow) => {
    const filter = mediaPlaneTestFilterPaths(workflow);

    // Check the model BEFORE the coverage question. An unmodelled glob would
    // otherwise fail the assertion below with "import X is uncovered", which is
    // a fact about this function rather than about the workflow — the reader
    // then debugs the guard instead of the filter.
    expect(
      filter.filter((pattern) => !supportedShape(pattern)),
      `${workflow} uses a glob shape this guard does not model — widen filterCovers or narrow the filter`
    ).toEqual([]);

    const uncovered = [...new Set(testFiles(TESTS_DIR).flatMap(crossWorkspaceImports))].filter(
      (imported) => !filter.some((pattern) => filterCovers(pattern, imported))
    );

    expect(uncovered, `${workflow} would skip the media-plane job for a change to these`).toEqual(
      []
    );
  });

  // The filter LISTS being right is necessary and not sufficient. Between the
  // list and the job sits a chain — filter output, detect-changes output,
  // workflow_call argument, reusable input, job `if` — and every link is a
  // separate string that can be renamed or dropped while the lists stay
  // pristine. The tests above would still pass and the parity suite would
  // silently stop running for a desktop-policy change: the original defect,
  // reached through the wiring rather than through the filter (Codex, #3277).
  it('wires the media-plane-tests result all the way to the job', () => {
    const build = readFileSync(join(REPO_ROOT, '.github/workflows/build.yml'), 'utf8');
    const prCi = readFileSync(join(REPO_ROOT, '.github/workflows/pr-ci.yml'), 'utf8');

    // Link 1-2: each workflow publishes the filter result as a job output.
    expect(build).toContain('media-plane-tests: ${{ steps.filter.outputs.media-plane-tests }}');
    expect(prCi).toContain('media-plane-tests: ${{ steps.filter.outputs.media-plane-tests }}');
    // Link 3: build.yml accepts it as a precomputed input.
    expect(build).toContain('media-plane-tests-changed:');
    // Link 4: pr-ci.yml actually passes its own result into that input.
    expect(prCi).toContain(
      'media-plane-tests-changed: ${{ needs.detect-changes.outputs.media-plane-tests }}'
    );

    // Link 5: the TEST job gates on both forms — the precomputed input for the
    // pr-ci path and the local output for the push path. Missing either one
    // silently drops half the entry points.
    const testGate = jobIfBlock(build, 'media-plane');
    expect(testGate).toContain("inputs.media-plane-tests-changed) == 'true'");
    expect(testGate).toContain("needs.changes.outputs.media-plane-tests == 'true'");

    // And the split itself: the Docker job must NOT consult the tests flag, or
    // the conflation this separation exists to undo is simply back.
    const dockerGate = jobIfBlock(build, 'media-plane-docker-warm');
    expect(dockerGate).not.toContain('media-plane-tests');
    expect(dockerGate).toContain("needs.changes.outputs.media-plane == 'true'");
  });

  it('keeps the two workflow filters identical', () => {
    // pr-ci.yml's copy is a stated mirror of build.yml's. Drift between them
    // means one entry point runs the suite and the other does not, which is
    // harder to notice than either being wrong on its own.
    const [first, second] = WORKFLOWS.map(mediaPlaneTestFilterPaths);
    expect(first).toEqual(second);
  });
});
