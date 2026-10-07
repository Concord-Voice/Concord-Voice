import React from 'react';

/** The authenticator-app glyph, extracted from `MFAMethodPicker` so the markup has one home. */
const PhoneIcon: React.FC = () => (
  <svg
    width="20"
    height="20"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="1.5"
  >
    <rect x="5" y="2" width="14" height="20" rx="2" />
    <line x1="12" y1="18" x2="12" y2="18.01" strokeWidth="2" strokeLinecap="round" />
  </svg>
);

export default PhoneIcon;
