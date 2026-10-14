import { beforeEach, describe, expect, it } from 'vitest';
import { resolveLegalLink } from '@/renderer/components/Settings/legalDocumentLinks';
import { LEGAL_DOCUMENTS, type LegalDocument } from '@/renderer/legalDocuments';
import { resetAllStores } from '../../../helpers/store-helpers';

const privacy: LegalDocument = {
  label: 'Privacy Policy',
  sourcePath: 'docs/privacy-policy.md',
  content: '',
};

beforeEach(() => {
  resetAllStores();
});

describe('legal document repository links', () => {
  it.each([
    ['../README.md', 'README.md'],
    ['/docs/../%52EADME.md', 'README.md'],
    ['legal/./commercial-license.md', 'docs/legal/commercial-license.md'],
    ['/docs/legal/%63ommercial-license.md', 'docs/legal/commercial-license.md'],
    ['runbooks/refresh-minio-image.md', 'docs/runbooks/refresh-minio-image.md'],
    ['/docs/legal/../runbooks/refresh-minio-image.md', 'docs/runbooks/refresh-minio-image.md'],
  ])('resolves the approved public path %s after normalization', (href, publicPath) => {
    expect(resolveLegalLink(href, privacy)).toEqual({
      kind: 'external',
      href: `https://github.com/Concord-Voice/Concord-Voice/blob/main/${publicPath}`,
    });
  });

  it.each([
    '/docs/legal/dependency-license-audit.md',
    '/[internal]skills/weekly-deps/SKILL.md',
    '/scripts/update-notice-md.py',
    '/docs/future-private-review.md',
    '/docs/legal/%64ependency-license-audit.md',
    '/docs/legal/commercial-license.md/private.md',
    '/docs/legal/commercial-license.md.bak',
  ])('leaves the unlisted private path %s inert', (href) => {
    expect(resolveLegalLink(href, privacy)).toBeNull();
  });

  it('keeps a normalized bundled document and its decoded fragment inside Settings', () => {
    const terms = LEGAL_DOCUMENTS.find((entry) => entry.id === 'terms-of-service');
    expect(terms).toBeDefined();
    expect(resolveLegalLink('legal/./terms-of-service.md#your%2Daccount', privacy)).toEqual({
      kind: 'document',
      document: terms,
      fragment: 'your-account',
    });
    expect(resolveLegalLink('#information%2Dwe%2Duse', privacy)).toEqual({
      kind: 'document',
      document: privacy,
      fragment: 'information-we-use',
    });
  });

  it('preserves a fragment on an approved public fallback', () => {
    expect(resolveLegalLink('../README.md#installation', privacy)).toEqual({
      kind: 'external',
      href: 'https://github.com/Concord-Voice/Concord-Voice/blob/main/README.md#installation',
    });
  });

  it.each(['https://example.com/legal', 'http://example.com/legal', 'mailto:privacy@example.com'])(
    'preserves an allowed absolute URL %s',
    (href) => {
      expect(resolveLegalLink(href, privacy)).toEqual({ kind: 'external', href });
    }
  );

  it.each([
    'javascript:alert%281%29',
    'data:text/html,attack',
    '//example.com/doc',
    '../%2FREADME.md',
    '../%5CREADME.md',
    '../%zz.md',
    '../../README.md',
    '../README.md#part%20name',
  ])('refuses the unsafe reference %s', (href) => {
    expect(resolveLegalLink(href, privacy)).toBeNull();
  });
});
