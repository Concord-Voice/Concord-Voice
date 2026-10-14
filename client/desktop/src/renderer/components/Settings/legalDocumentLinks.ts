import { LEGAL_DOCUMENTS, type LegalDocument } from '../../legalDocuments';

type LegalLink =
  | { kind: 'document'; document: LegalDocument; fragment: string | null }
  | { kind: 'external'; href: string };

const PUBLIC_SOURCE_BASE = 'https://github.com/Concord-Voice/Concord-Voice/blob/main/';
const PUBLIC_SOURCE_PATHS = new Set([
  'README.md',
  'docs/legal/commercial-license.md',
  'docs/runbooks/refresh-minio-image.md',
]);
const UNSAFE_CHARACTERS = /[\u0000-\u0020\u007f\\]/;

export function legalDisclosureId(document: LegalDocument): string {
  const identity = document.id ?? document.sourcePath.replace(/^\.\//, '');
  return `legal-document-${identity.toLowerCase().replace(/[^a-z0-9_-]+/g, '-')}`;
}

/** Normalize within the repository, refusing traversal beyond its root or encoded separators. */
function repositoryPath(path: string): string | null {
  const normalized: string[] = [];
  for (const encodedPart of path.split('/')) {
    let part: string;
    try {
      part = decodeURIComponent(encodedPart);
    } catch {
      return null;
    }
    if (!part || part === '.') continue;
    if (part === '..') {
      if (normalized.length === 0) return null;
      normalized.pop();
    } else {
      if (UNSAFE_CHARACTERS.test(part) || /[/:?#]/.test(part)) return null;
      normalized.push(part);
    }
  }
  return normalized.length > 0 ? normalized.join('/') : null;
}

export function legalFragmentId(document: LegalDocument, fragment: string | null): string {
  const disclosureId = legalDisclosureId(document);
  return fragment ? `${disclosureId}--${fragment}` : disclosureId;
}

function parseLegalReference(href: string): { reference: string; fragment: string | null } | null {
  const hashIndex = href.indexOf('#');
  if (hashIndex < 0) return { reference: href, fragment: null };

  let fragment: string | null;
  try {
    fragment = decodeURIComponent(href.slice(hashIndex + 1)) || null;
  } catch {
    return null;
  }
  if (fragment && UNSAFE_CHARACTERS.test(fragment)) return null;
  return { reference: href.slice(0, hashIndex), fragment };
}

/** Resolve source-owned links without ever navigating a client route or an arbitrary scheme. */
export function resolveLegalLink(
  href: string | undefined,
  document: LegalDocument
): LegalLink | null {
  if (!href || UNSAFE_CHARACTERS.test(href) || href.startsWith('//')) return null;
  if (/^[a-z][a-z\d+.-]*:/i.test(href)) {
    return /^(?:https?|mailto):/i.test(href) ? { kind: 'external', href } : null;
  }

  const parsed = parseLegalReference(href);
  if (!parsed) return null;
  const { reference, fragment } = parsed;
  if (!reference) return { kind: 'document', document, fragment };

  const source = repositoryPath(document.sourcePath);
  if (!source) return null;
  const directory = source.slice(0, Math.max(0, source.lastIndexOf('/') + 1));
  const resolvedPath = repositoryPath(
    reference.startsWith('/') ? reference.slice(1) : directory + reference
  );
  if (!resolvedPath) return null;

  const bundled = LEGAL_DOCUMENTS.find(
    (entry) => repositoryPath(entry.sourcePath) === resolvedPath
  );
  if (bundled) return { kind: 'document', document: bundled, fragment };
  if (!PUBLIC_SOURCE_PATHS.has(resolvedPath)) return null;

  const external = new URL(
    resolvedPath.split('/').map(encodeURIComponent).join('/'),
    PUBLIC_SOURCE_BASE
  );
  if (fragment) external.hash = fragment;
  return { kind: 'external', href: external.href };
}
