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
});
