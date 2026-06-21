import { render, screen } from '../../../test-utils';
import { fireEvent } from '@testing-library/react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { SSOButton } from '@/renderer/components/Auth/SSOButton';
import { resetAllStores } from '../../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
});

describe('SSOButton', () => {
  it('renders Google branded button with correct text', () => {
    render(<SSOButton provider="google" onClick={() => {}} />);
    expect(screen.getByRole('button', { name: /sign in with google/i })).toBeInTheDocument();
  });

  it('calls onClick when clicked', () => {
    const onClick = vi.fn();
    render(<SSOButton provider="google" onClick={onClick} />);
    fireEvent.click(screen.getByRole('button'));
    expect(onClick).toHaveBeenCalledOnce();
  });

  it('disables when disabled prop is true', () => {
    render(<SSOButton provider="google" onClick={() => {}} disabled />);
    expect(screen.getByRole('button')).toBeDisabled();
  });

  it('renders Apple variant when provider=apple (forward-compat for #271)', () => {
    render(<SSOButton provider="apple" onClick={() => {}} />);
    expect(screen.getByRole('button', { name: /sign in with apple/i })).toBeInTheDocument();
  });
});
