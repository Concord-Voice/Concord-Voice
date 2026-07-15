import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup, screen, fireEvent } from '@testing-library/react';
import { ServerNameField, ServerFormBanners } from '@/renderer/components/Servers/ServerNameField';
import { validateServerName, NAME_MAX } from '@/renderer/components/Servers/serverConstants';

describe('ServerNameField', () => {
  afterEach(() => {
    cleanup();
  });

  it('wires the label to the input via inputId', () => {
    render(
      <ServerNameField inputId="server-name-input" name="" disabled={false} onChange={vi.fn()} />
    );

    const input = screen.getByLabelText('Server Name');
    expect(input.id).toBe('server-name-input');
  });

  it('fires onChange with the raw typed value', () => {
    const onChange = vi.fn();
    render(
      <ServerNameField inputId="server-name-input" name="" disabled={false} onChange={onChange} />
    );

    fireEvent.change(screen.getByLabelText('Server Name'), { target: { value: 'My Server' } });

    expect(onChange).toHaveBeenCalledWith('My Server');
  });

  it('renders the error text and applies the error class when error is set', () => {
    render(
      <ServerNameField
        inputId="server-name-input"
        name="ab"
        error="Server name must be at least 3 characters"
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.getByText('Server name must be at least 3 characters')).toBeInTheDocument();
    expect(screen.getByLabelText('Server Name').className).toContain('error');
  });

  it('does not render an error and omits the error class when error is absent', () => {
    render(
      <ServerNameField
        inputId="server-name-input"
        name="Valid Name"
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.queryByText(/required|at least|at most/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Server Name').className).not.toContain('error');
  });

  it('disables the input when disabled is true', () => {
    render(
      <ServerNameField inputId="server-name-input" name="" disabled={true} onChange={vi.fn()} />
    );

    expect(screen.getByLabelText('Server Name')).toBeDisabled();
  });

  it('sets maxLength to NAME_MAX', () => {
    render(
      <ServerNameField inputId="server-name-input" name="" disabled={false} onChange={vi.fn()} />
    );

    expect(screen.getByLabelText('Server Name')).toHaveAttribute('maxLength', String(NAME_MAX));
  });

  it('shows the trimmed character count in the hint', () => {
    render(
      <ServerNameField
        inputId="server-name-input"
        name="  hello  "
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.getByText(`5/${NAME_MAX} characters`)).toBeInTheDocument();
  });

  it('defaults autoFocus to false', () => {
    render(
      <ServerNameField inputId="server-name-input" name="" disabled={false} onChange={vi.fn()} />
    );

    expect(screen.getByLabelText('Server Name')).not.toHaveFocus();
  });

  it('focuses the input when autoFocus is true', () => {
    render(
      <ServerNameField
        inputId="server-name-input"
        name=""
        disabled={false}
        autoFocus
        onChange={vi.fn()}
      />
    );

    expect(screen.getByLabelText('Server Name')).toHaveFocus();
  });
});

describe('ServerFormBanners', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders only the general error banner when generalError is set', () => {
    render(<ServerFormBanners generalError="Something went wrong" />);

    expect(screen.getByText('Something went wrong')).toBeInTheDocument();
  });

  it('renders only the success banner when successMessage is set', () => {
    render(<ServerFormBanners successMessage="Server created" />);

    expect(screen.getByText('Server created')).toBeInTheDocument();
  });

  it('renders neither banner when neither prop is set', () => {
    const { container } = render(<ServerFormBanners />);

    expect(container.querySelector('.form-error-banner')).toBeNull();
    expect(container.querySelector('.form-success-banner')).toBeNull();
  });
});

describe('validateServerName', () => {
  it('returns a required error for an empty name', () => {
    expect(validateServerName('')).toEqual({ name: 'Server name is required' });
  });

  it('returns a required error for a whitespace-only name', () => {
    expect(validateServerName('   ')).toEqual({ name: 'Server name is required' });
  });

  it('returns an at-least error for a 2-character name', () => {
    expect(validateServerName('ab')).toEqual({
      name: 'Server name must be at least 3 characters',
    });
  });

  it('returns an at-most error for a 101-character name', () => {
    const name = 'a'.repeat(101);
    expect(validateServerName(name)).toEqual({
      name: 'Server name must be at most 100 characters',
    });
  });

  it('returns no errors for a valid name', () => {
    expect(validateServerName('My Server')).toEqual({});
  });

  it('accepts the 3-character minimum boundary', () => {
    expect(validateServerName('abc')).toEqual({});
  });

  it('accepts the 100-character maximum boundary', () => {
    expect(validateServerName('a'.repeat(100))).toEqual({});
  });
});
