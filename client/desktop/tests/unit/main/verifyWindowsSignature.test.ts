// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { verifyChain, type CertInfo } from '../../../src/main/verifyWindowsSignature';

const ALLOWED = ['Concord Voice LLC'] as const;

const validChain: CertInfo[] = [
  { subjectCN: 'Concord Voice LLC', issuerCN: 'Microsoft ID Verified CS EOC CA 01' },
  {
    subjectCN: 'Microsoft ID Verified CS EOC CA 01',
    issuerCN: 'Microsoft Identity Verification Root CA 2020',
  },
  {
    subjectCN: 'Microsoft Identity Verification Root CA 2020',
    issuerCN: 'Microsoft Identity Verification Root CA 2020',
  },
];

describe('verifyChain', () => {
  it('accepts a valid chain with pinned intermediate', () => {
    expect(verifyChain(validChain, ALLOWED)).toBeNull();
  });

  it('rejects an empty chain', () => {
    expect(verifyChain([], ALLOWED)).toMatch(/empty/);
  });

  it('rejects a leaf whose CN is not in the allow-list', () => {
    const bad = [{ ...validChain[0], subjectCN: 'Evil Co' }, ...validChain.slice(1)];
    expect(verifyChain(bad, ALLOWED)).toMatch(/unauthorized publisher/);
  });

  it('rejects a chain whose leaf is not issued by the pinned intermediate', () => {
    const bad = [
      { subjectCN: 'Concord Voice LLC', issuerCN: 'Some Other CA' },
      { subjectCN: 'Some Other CA', issuerCN: 'Some Other Root' },
      { subjectCN: 'Some Other Root', issuerCN: 'Some Other Root' },
    ];
    expect(verifyChain(bad, ALLOWED)).toMatch(/not issued by pinned/);
  });

  it('rejects a single-element (leaf-only) chain even if the CN claim starts with the pinned prefix', () => {
    const bad = [{ subjectCN: 'Concord Voice LLC', issuerCN: 'Microsoft ID Verified CS Fake' }];
    expect(verifyChain(bad, ALLOWED)).toMatch(/chain too short/);
  });

  it('accepts a leaf issued by any CS-pattern intermediate, not just EOC CA 01', () => {
    const chain = [
      { subjectCN: 'Concord Voice LLC', issuerCN: 'Microsoft ID Verified CS AOC CA 02' },
      {
        subjectCN: 'Microsoft ID Verified CS AOC CA 02',
        issuerCN: 'Microsoft Identity Verification Root CA 2020',
      },
      validChain[2],
    ];
    expect(verifyChain(chain, ALLOWED)).toBeNull();
  });
});
