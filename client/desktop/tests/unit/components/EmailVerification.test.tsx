// This file is superseded by tests/unit/components/Auth/EmailVerification.test.tsx
// which covers the updated EmailVerification component (pendingRegistrationStore-based).
// Kept as a thin smoke-test to satisfy file-level coverage requirements.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import EmailVerification from '../../../src/renderer/components/Auth/EmailVerification';
import { usePendingRegistrationStore } from '../../../src/renderer/stores/pendingRegistrationStore';

vi.mock('../../../src/renderer/services/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
}));

describe('EmailVerification (smoke)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'smoke-pending',
      email: 'smoke@example.com',
      expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
    });
  });

  it('renders with masked email by default', () => {
    render(<EmailVerification onSuccess={vi.fn()} />);
    expect(screen.getByText('Verify your email')).toBeInTheDocument();
    expect(screen.getByText(/s\*+e@example\.com/)).toBeInTheDocument();
  });

  it('renders with unmasked email when maskEmail=false', () => {
    render(<EmailVerification onSuccess={vi.fn()} maskEmail={false} />);
    expect(screen.getByText('smoke@example.com')).toBeInTheDocument();
  });

  it('renders 6 digit input fields', () => {
    render(<EmailVerification onSuccess={vi.fn()} />);
    expect(screen.getAllByRole('textbox')).toHaveLength(6);
  });

  it('shows resend button on mount', () => {
    render(<EmailVerification onSuccess={vi.fn()} />);
    expect(screen.getByText('Send new code')).not.toBeDisabled();
  });
});
