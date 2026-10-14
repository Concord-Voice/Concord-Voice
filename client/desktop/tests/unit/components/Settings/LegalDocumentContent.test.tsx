import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import LegalDocumentContent from '@/renderer/components/Settings/LegalDocumentContent';
import { LEGAL_DOCUMENTS, type LegalDocument } from '@/renderer/legalDocuments';

const document: LegalDocument = {
  label: 'Privacy Policy',
  sourcePath: 'docs/privacy-policy.md',
  content: `# Privacy Policy

[Contents](#information-we-use)

## Information we use

- **Account** information
- Service preferences

| Data | Purpose |
| --- | --- |
| Account | Sign-in |

[Repository guide](../README.md)
[Escapes repository](../../../../outside.md)
[Unsafe script](javascript:alert%281%29)
[Unsafe data](data:text/html,attack)
[Remote image reference](https://example.com/image.png)
![Remote image](https://example.com/tracker.png)
`,
};

const originalScrollDescriptor = Object.getOwnPropertyDescriptor(
  HTMLElement.prototype,
  'scrollIntoView'
);
beforeEach(() => {
  resetAllStores();
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  if (originalScrollDescriptor)
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', originalScrollDescriptor);
  else delete (HTMLElement.prototype as Partial<HTMLElement>).scrollIntoView;
});

describe('LegalDocumentContent source-owned Markdown', () => {
  it('renders source headings, lists and GFM tables as semantic content', () => {
    render(<LegalDocumentContent document={document} />);
    expect(screen.getByRole('heading', { name: 'Privacy Policy', level: 1 })).toBeInTheDocument();
    expect(
      screen.getByRole('heading', { name: 'Information we use', level: 2 })
    ).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByRole('columnheader', { name: 'Data' })).toBeInTheDocument();
    const tableRegion = screen.getByRole('region', { name: 'Document table' });
    expect(within(tableRegion).getByRole('table')).toBeInTheDocument();
    expect(within(tableRegion).getByRole('cell', { name: 'Sign-in' })).toBeInTheDocument();
    expect(screen.getByText('Account', { selector: 'strong' })).toBeInTheDocument();
  });

  it('scopes table-of-contents anchors to their source document', async () => {
    render(
      <>
        <LegalDocumentContent document={document} />
        <LegalDocumentContent
          document={{
            ...document,
            label: 'Terms of Service',
            sourcePath: 'docs/legal/terms-of-service.md',
          }}
        />
      </>
    );
    const headings = screen.getAllByRole('heading', { name: 'Information we use' });
    expect(headings[0].id).toBeTruthy();
    expect(headings[1].id).toBeTruthy();
    expect(headings[0].id).not.toBe(headings[1].id);
    const links = screen.getAllByRole('link', { name: 'Contents' });
    expect(links[0]).toHaveAttribute('href', `#${headings[0].id}`);
    expect(links[1]).toHaveAttribute('href', `#${headings[1].id}`);
    await userEvent.setup().click(links[0]);
    expect(headings[0]).toHaveFocus();
    expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledWith({
      block: 'start',
      behavior: 'instant',
    });
  });

  it('resolves a repository-relative link to bounded public main content', () => {
    render(<LegalDocumentContent document={document} />);
    expect(screen.getByRole('link', { name: 'Repository guide' })).toHaveAttribute(
      'href',
      'https://github.com/Concord-Voice/Concord-Voice/blob/main/README.md'
    );
    expect(screen.queryByRole('link', { name: 'Escapes repository' })).not.toBeInTheDocument();
  });

  it('accepts an encoded local fragment and focuses its actual source heading', async () => {
    render(
      <LegalDocumentContent
        document={{
          ...document,
          content: '## Information we use\n\n[Encoded contents](#information%2Dwe%2Duse)',
        }}
      />
    );
    const heading = screen.getByRole('heading', { name: 'Information we use' });
    const link = screen.getByRole('link', { name: 'Encoded contents' });
    expect(link).toHaveAttribute('href', `#${heading.id}`);
    await userEvent.setup().click(link);
    expect(heading).toHaveFocus();
  });

  it('accepts bounded encoded repository references and direct privacy email links', () => {
    render(
      <LegalDocumentContent
        document={{
          ...document,
          content:
            '[Commercial license](/docs/legal/%63ommercial-license.md#contact)\n\n[Privacy contact](mailto:privacy@example.com)',
        }}
      />
    );
    expect(screen.getByRole('link', { name: 'Commercial license' })).toHaveAttribute(
      'href',
      'https://github.com/Concord-Voice/Concord-Voice/blob/main/docs/legal/commercial-license.md#contact'
    );
    expect(screen.getByRole('link', { name: 'Privacy contact' })).toHaveAttribute(
      'href',
      'mailto:privacy@example.com'
    );
  });

  it('keeps private source references in the bundled notice readable as inert text', () => {
    const notice = LEGAL_DOCUMENTS.find((entry) => entry.id === 'notice');
    expect(notice).toBeDefined();
    render(<LegalDocumentContent document={notice!} />);

    for (const label of [
      'docs/legal/dependency-license-audit.md',
      'scripts/update-notice-md.py',
      '/weekly-deps',
    ]) {
      const references = screen.getAllByText(label);
      expect(references.length).toBeGreaterThan(0);
      for (const reference of references) {
        expect(reference.closest('a')).toBeNull();
      }
      // The notice also repeats some path labels as plain inline code.
      // At least the formerly linked occurrence must retain the inert span.
      expect(references.some((reference) => reference.closest('span') !== null)).toBe(true);
    }
    expect(screen.getByRole('link', { name: 'MinIO refresh runbook' })).toHaveAttribute(
      'href',
      'https://github.com/Concord-Voice/Concord-Voice/blob/main/[internal]refresh-minio-image.md'
    );
  });

  it('renders an unknown future private repository reference as a span', () => {
    render(
      <LegalDocumentContent
        document={{
          ...document,
          content: '[Internal review notes](/docs/future-private-review.md)',
        }}
      />
    );
    expect(screen.getByText('Internal review notes').tagName).toBe('SPAN');
    expect(screen.queryByRole('link', { name: 'Internal review notes' })).not.toBeInTheDocument();
  });

  it.each([
    ['decoded whitespace', '#part%20name'],
    ['decoded control', '#part%0Aname'],
    ['decoded backslash', '#part%5Cname'],
    ['malformed repository escape', '../%zz.md'],
    ['encoded path separator', '../%2Foutside.md'],
    ['network-relative URL', '//example.com/doc'],
  ])('keeps a legal reference with %s as inert text', (_label, href) => {
    render(
      <LegalDocumentContent document={{ ...document, content: `[Refused reference](${href})` }} />
    );
    expect(screen.getByText('Refused reference')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Refused reference' })).not.toBeInTheDocument();
  });

  it('supports Setext headings and unique duplicate IDs while leaving raw HTML and malformed anchors inert', () => {
    const { container } = render(
      <LegalDocumentContent
        document={{
          label: 'License',
          sourcePath: './LICENSE',
          content:
            'License agreement\n---\n\n#### Details\n\n#### Details\n\n[Malformed anchor](#bad%zz)\n\n<script>alert(1)</script>',
        }}
      />
    );
    expect(
      screen.getByRole('heading', { name: 'License agreement', level: 2 })
    ).toBeInTheDocument();
    const repeated = screen.getAllByRole('heading', { name: 'Details', level: 4 });
    expect(repeated[0].id).toBeTruthy();
    expect(repeated[0].id).not.toBe(repeated[1].id);
    expect(screen.queryByRole('link', { name: 'Malformed anchor' })).not.toBeInTheDocument();
    expect(container.querySelector('script')).toBeNull();
  });

  it('rejects unsafe schemes and does not automatically load remote images', () => {
    const { container } = render(<LegalDocumentContent document={document} />);
    expect(screen.queryByRole('link', { name: 'Unsafe script' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Unsafe data' })).not.toBeInTheDocument();
    expect(container.querySelector('img[src]')).toBeNull();
    expect(screen.getByRole('link', { name: 'Remote image reference' })).toHaveAttribute(
      'href',
      'https://example.com/image.png'
    );
  });
});
