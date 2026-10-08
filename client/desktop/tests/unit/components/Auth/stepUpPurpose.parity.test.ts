// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  PASSWORD_STEP_UP_PURPOSES,
  STEP_UP_PURPOSES,
} from '../../../../src/renderer/components/Auth/stepUpPurpose';

// The TS `StepUpPurpose` union mirrors the server's closed `stepup.Purpose`
// set by hand (stepUpPurpose.ts's own docblock says so). A purpose added here
// without its Go constant compiles clean and fails only at runtime, as a 400
// on `verify-inline/begin` (#3455, orchestrator amendment X20). This guards
// the direction that matters for a client build: every TS purpose must exist
// in Go. The reverse (a Go purpose with no TS consumer) is fine — the server
// set is allowed to be a superset.
describe('StepUpPurpose Go/TS parity (#3455)', () => {
  const purposeGoPath = resolve(
    __dirname,
    '../../../../../../services/control-plane/internal/stepup/purpose.go'
  );

  it('every TS StepUpPurpose exists in the Go closed set', () => {
    const source = readFileSync(purposeGoPath, 'utf-8');
    const goValues = new Set(Array.from(source.matchAll(/Purpose\s*=\s*"([^"]+)"/g), (m) => m[1]));
    const missing = STEP_UP_PURPOSES.filter((purpose) => !goValues.has(purpose));
    expect(missing, `TS purposes missing from Go's allPurposes: ${missing.join(', ')}`).toEqual([]);
  });

  // #3509: the password mint accepts exactly Go's ownRulePurposes, so this
  // mirror must equal it in BOTH directions — a purpose only TS has can never
  // mint, and one only Go has is a route whose password prompt cannot finish.
  it('the password step-up purposes equal the Go ownRulePurposes, exactly', () => {
    const source = readFileSync(purposeGoPath, 'utf-8');
    const valueOf = new Map(
      Array.from(source.matchAll(/(Purpose\w+)\s+Purpose\s*=\s*"([^"]+)"/g), (m) => [m[1], m[2]])
    );
    const block = /var ownRulePurposes = \[\.\.\.\]Purpose\{([^}]*)\}/.exec(source);
    expect(block, 'ownRulePurposes not found in purpose.go').not.toBeNull();
    const goValues = Array.from((block?.[1] ?? '').matchAll(/Purpose\w+/g), (m) =>
      valueOf.get(m[0])
    );
    expect(goValues).not.toContain(undefined);
    expect([...PASSWORD_STEP_UP_PURPOSES].sort()).toEqual([...goValues].sort());
  });

  // #3456: the 14 purposes the MFA-enforcement toggle's OFF confirmation and
  // the dangerous-action gates mint for. The superset check above cannot see a
  // purpose DELETED from the mirror, which would leave a route whose factor
  // picker begins a WebAuthn token with no purpose to name.
  // Mutant: any one of these removed from STEP_UP_PURPOSES.
  const MFA_ENFORCEMENT_PURPOSES = [
    'servers.mfa_enforcement_disable',
    'channels.delete',
    'servers.update',
    'media.server_icon_upload',
    'media.server_banner_upload',
    'members.ban',
    'members.kick_purge',
    'roles.delete',
    'roles.create',
    'roles.update',
    'channels.expiration_shorten',
    'servers.delete',
    'overrides.channel_upsert',
    'overrides.category_upsert',
  ];

  it('the 14 #3456 purposes are in the TS mirror and in the Go closed set', () => {
    const source = readFileSync(purposeGoPath, 'utf-8');
    const goValues = new Set(Array.from(source.matchAll(/Purpose\s*=\s*"([^"]+)"/g), (m) => m[1]));
    expect(MFA_ENFORCEMENT_PURPOSES).toHaveLength(14);
    const asStrings: readonly string[] = STEP_UP_PURPOSES;
    for (const purpose of MFA_ENFORCEMENT_PURPOSES) {
      expect(asStrings, `${purpose} missing from the TS mirror`).toContain(purpose);
      expect(goValues, `${purpose} missing from Go`).toContain(purpose);
    }
  });

  // Mutant: a #3456 purpose added to PASSWORD_STEP_UP_PURPOSES. No D1 gate
  // reads a password, so a password mint for one would be refused by Go's
  // ownRulePurposes with a 400.
  it('none of the 14 #3456 purposes is a password step-up purpose', () => {
    const passwordPurposes: readonly string[] = PASSWORD_STEP_UP_PURPOSES;
    for (const purpose of MFA_ENFORCEMENT_PURPOSES) {
      expect(passwordPurposes).not.toContain(purpose);
    }
  });

  // Mutant: a duplicate or a typo'd entry in the mirror.
  it('the mirror has no duplicate purposes', () => {
    expect(new Set(STEP_UP_PURPOSES).size).toBe(STEP_UP_PURPOSES.length);
  });
});
