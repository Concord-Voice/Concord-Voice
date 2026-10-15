// @vitest-environment node
import { existsSync, readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { parse as parseYaml } from 'yaml';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../src/constants/allowedWindowsPublishers.mts';

// The script runs on the two Windows release legs, which no PR job covers, so a break in it or in
// its wiring is first seen in a release. These tests pin what it gates on and that the workflow
// still runs it. PowerShell cannot run here, and Get-AuthenticodeSignature exists only on Windows.
const script = readFileSync(new URL('./verify-windows-signature.ps1', import.meta.url), 'utf8');

describe('verify-windows-signature.ps1', () => {
  it('refuses an installer whose signature is not Valid', () => {
    expect(script).toContain("$sig.Status -ne 'Valid'");
  });

  it('pins the signer CN to the publisher allow-list', () => {
    const pinned = /\$cn -ne '([^']+)'/.exec(script)?.[1];
    expect(pinned).toBeDefined();
    // One CN is compared, so a second allowed publisher means this script changes with the list.
    expect([...ALLOWED_WINDOWS_PUBLISHERS]).toEqual([pinned]);
  });

  it('requires an RFC 3161 timestamp and the Microsoft Artifact Signing issuer', () => {
    expect(script).toContain('-not $sig.TimeStamperCertificate');
    expect(script).toContain("$issuerCN -notmatch '^Microsoft ID Verified CS '");
  });

  it('fails when no installer was produced, so a missing build cannot pass', () => {
    expect(script).toMatch(/\$setups\.Count -eq 0\) \{[^}]*exit 1/);
  });
});

// ─── workflow wiring ────────────────────────────────────────────────────────────────────────

// The public mirror publishes client/ but not the private workflows parsed below, and its overlay
// adds a CI workflow of its own, which makes that file the marker. Anywhere else a missing
// workflow must fail loudly: a wiring test that skips itself is the silent loss it exists to stop.
const WORKFLOWS = new URL('../../../.github/workflows/', import.meta.url);
const inPublicMirror = existsSync(new URL('public-ci-frontend.yml', WORKFLOWS));

describe('workflow wiring: the skip is confined to the public mirror', () => {
  it('never skips where the private workflows are present', () => {
    expect(existsSync(new URL('build-desktop.yml', WORKFLOWS)) && inPublicMirror).toBe(false);
  });
});

describe.skipIf(inPublicMirror)('workflow wiring', () => {
  it('runs the script once, from the repo root, on the Windows legs, and lets it fail the job', () => {
    const workflow = parseYaml(readFileSync(new URL('build-desktop.yml', WORKFLOWS), 'utf8')) as {
      jobs: Record<string, { steps: Array<Record<string, unknown>> }>;
    };
    const steps = workflow.jobs['release-build'].steps.filter(
      (s) => s.name === 'Verify Windows signature'
    );
    expect(steps).toHaveLength(1);
    // Whole-object equality: a continue-on-error, a working-directory or a different path or shell
    // is each a way for the check to stop gating a release, and each fails here.
    expect(steps[0]).toEqual({
      name: 'Verify Windows signature',
      if: "matrix.platform == 'win32'",
      shell: 'pwsh',
      run: './client/desktop/scripts/verify-windows-signature.ps1',
    });
  });
});
