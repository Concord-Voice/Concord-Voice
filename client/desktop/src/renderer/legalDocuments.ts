import licenseText from 'virtual:concord-legal/license';
import noticeText from 'virtual:concord-legal/notice';
import privacyPolicyText from 'virtual:concord-legal/privacy-policy';
import termsOfServiceText from 'virtual:concord-legal/terms-of-service';

export interface LegalDocument {
  label: string;
  sourcePath: string;
  content: string;
}

export const LEGAL_DOCUMENTS: readonly LegalDocument[] = [
  {
    label: 'LICENSE',
    sourcePath: './LICENSE',
    content: licenseText,
  },
  {
    label: 'Privacy Policy',
    sourcePath: 'docs/privacy-policy.md',
    content: privacyPolicyText,
  },
  {
    label: 'Terms of Service',
    sourcePath: 'docs/legal/terms-of-service.md',
    content: termsOfServiceText,
  },
  {
    label: 'NOTICE.md',
    sourcePath: './NOTICE.md',
    content: noticeText,
  },
];
