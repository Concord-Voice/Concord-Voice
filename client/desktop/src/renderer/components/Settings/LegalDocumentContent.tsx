import React, { createContext, use } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeSanitize from 'rehype-sanitize';
import type { Schema } from 'hast-util-sanitize';
import type { Element, Root, RootContent } from 'hast';
import type { LegalDocument } from '../../legalDocuments';
import { useSettingsCollapsibleStore } from '../../stores/ui/settingsCollapsibleStore';
import SafeLink from '../Markdown/SafeLink';
import { legalDisclosureId, legalFragmentId, resolveLegalLink } from './legalDocumentLinks';
import './LegalDocumentContent.css';

interface LegalDocumentContentProps {
  document: LegalDocument;
}

// Legal prose has its own allowlist. Chat's sanitizer and custom plugins remain independent.
const legalSchema: Schema = {
  tagNames: [
    'p',
    'br',
    'a',
    'strong',
    'em',
    'del',
    'code',
    'pre',
    'blockquote',
    'ul',
    'ol',
    'li',
    'h1',
    'h2',
    'h3',
    'h4',
    'h5',
    'h6',
    'hr',
    'table',
    'thead',
    'tbody',
    'tr',
    'th',
    'td',
  ],
  attributes: {
    a: ['href', 'title'],
    ol: [['start', /^\d+$/]],
    th: [['align', 'left', 'center', 'right']],
    td: [['align', 'left', 'center', 'right']],
  },
  protocols: { href: ['http', 'https', 'mailto'] },
  strip: ['img', 'script', 'style', 'iframe', 'object', 'embed', 'input'],
  clobber: ['name', 'id'],
  clobberPrefix: 'legal-',
  allowComments: false,
  allowDoctypes: false,
};

function textContent(node: RootContent): string {
  if (node.type === 'text') return node.value;
  return 'children' in node ? node.children.map(textContent).join('') : '';
}

/** Assign document-scoped heading IDs after sanitizing; source HTML cannot supply an ID. */
function rehypeLegalHeadings(options: { document: LegalDocument }) {
  return (tree: Root) => {
    const usedSlugs = new Set<string>();
    const visit = (node: Root | Element) => {
      for (const child of node.children) {
        if (child.type !== 'element') continue;
        if (/^h[1-6]$/.test(child.tagName)) {
          const base =
            textContent(child)
              .toLowerCase()
              .replace(/[^\p{L}\p{M}\p{N}\s_-]/gu, '')
              .replace(/\s/g, '-') || 'section';
          let slug = base;
          let suffix = 0;
          while (usedSlugs.has(slug)) slug = `${base}-${++suffix}`;
          usedSlugs.add(slug);
          child.properties.id = legalFragmentId(options.document, slug);
          child.properties.tabIndex = -1;
        }
        visit(child);
      }
    };
    visit(tree);
  };
}

const LegalDocumentContext = createContext<LegalDocument | null>(null);

function focusLegalTarget(id: string): void {
  const target = globalThis.document.getElementById(id);
  if (!target) return;
  const focusTarget =
    target instanceof HTMLDetailsElement ? target.querySelector('summary') : target;
  focusTarget?.focus({ preventScroll: true });
  target.scrollIntoView({ block: 'start', behavior: 'instant' });
}

function revealLegalTarget(document: LegalDocument, fragment: string | null): void {
  const id = legalFragmentId(document, fragment);
  const target = globalThis.document.getElementById(id);
  const container = target ?? globalThis.document.getElementById(legalDisclosureId(document));
  if (!container) return;
  let ancestor = container.closest('details');
  while (ancestor) {
    if (ancestor.id) useSettingsCollapsibleStore.getState().setSectionOpen(ancestor.id, true);
    ancestor.open = true;
    ancestor = ancestor.parentElement?.closest('details') ?? null;
  }
  // Closed documents mount their prose only on expansion. Wait for that commit before
  // targeting a bundled document's heading; existing local headings focus immediately.
  if (target) focusLegalTarget(id);
  else requestAnimationFrame(() => focusLegalTarget(id));
}

const LegalAnchor: React.FC<React.AnchorHTMLAttributes<HTMLAnchorElement>> = ({
  href,
  title,
  children,
}) => {
  const document = use(LegalDocumentContext);
  const link = document ? resolveLegalLink(href, document) : null;
  if (!link) return <span>{children}</span>;
  if (link.kind === 'external')
    return (
      <SafeLink href={link.href} title={title}>
        {children}
      </SafeLink>
    );

  const id = legalFragmentId(link.document, link.fragment);
  return (
    <a
      href={`#${id}`}
      title={title}
      onClick={(event) => {
        event.preventDefault();
        revealLegalTarget(link.document, link.fragment);
      }}
    >
      {children}
    </a>
  );
};

const LegalTable: React.FC<React.TableHTMLAttributes<HTMLTableElement>> = ({ children }) => (
  // Chromium makes overflowing containers keyboard reachable, including through child links.
  <section className="legal-document-table" aria-label="Document table">
    <table>{children}</table>
  </section>
);

const LegalDocumentContent: React.FC<LegalDocumentContentProps> = ({ document }) => (
  <div className="about-legal-content">
    <div className="legal-document-content" data-legal-document={legalDisclosureId(document)}>
      <LegalDocumentContext value={document}>
        <ReactMarkdown
          remarkPlugins={[remarkGfm]}
          rehypePlugins={[
            [rehypeSanitize, legalSchema],
            [rehypeLegalHeadings, { document }],
          ]}
          components={{ a: LegalAnchor, table: LegalTable }}
          skipHtml
        >
          {document.content}
        </ReactMarkdown>
      </LegalDocumentContext>
    </div>
  </div>
);

const MemoizedLegalDocumentContent = React.memo(LegalDocumentContent);
MemoizedLegalDocumentContent.displayName = 'LegalDocumentContent';

export default MemoizedLegalDocumentContent;
