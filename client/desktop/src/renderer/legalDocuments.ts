import licenseText from 'virtual:concord-legal/license';
import noticeText from 'virtual:concord-legal/notice';
import privacyPolicyText from 'virtual:concord-legal/privacy-policy';
import termsOfServiceText from 'virtual:concord-legal/terms-of-service';

export interface LegalDocument {
  /** Stable disclosure identity; source paths also identify documents outside this registry. */
  id?: string;
  label: string;
  sourcePath: string;
  content: string;
}

export const LEGAL_DOCUMENTS: readonly LegalDocument[] = [
  {
    id: 'license',
    label: 'License',
    sourcePath: './LICENSE',
    content: licenseText,
  },
  {
    id: 'privacy-policy',
    label: 'Privacy Policy',
    sourcePath: 'docs/privacy-policy.md',
    content: privacyPolicyText,
  },
  {
    id: 'terms-of-service',
    label: 'Terms of Service',
    sourcePath: 'docs/legal/terms-of-service.md',
    content: termsOfServiceText,
  },
  {
    id: 'notice',
    label: 'Third-Party Software Disclosure',
    sourcePath: './NOTICE.md',
    content: noticeText,
  },
];
