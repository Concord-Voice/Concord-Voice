import { createRef } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, within } from '../../../test-utils';
import MFAFactorPicker, {
  StepUpFieldError,
  type MFAFactorPickerHandle,
  type MFAFactorPickerProps,
} from '@/renderer/components/Auth/MFAFactorPicker';

// The step-up picker's method region (design 2026-09-26-mfa-factor-picker §4.2;
// plan 2026-10-07 §3). The component is presentational: every state is a prop
// set, so each panel is rendered directly. No axe library exists in this repo,
// so the accessibility contract is asserted for what it is: roles, accessible
// names, `aria-describedby` targets and `aria-invalid`.
//
// "Mutant:" comments name the production change each test exists to turn red.

const RECENT = 'You just used a code from your authenticator app. Enter the next one it shows.';

function props(overrides: Partial<MFAFactorPickerProps> = {}): MFAFactorPickerProps {
  return {
    methods: ['webauthn', 'totp', 'backup'],
    method: 'totp',
    code: '',
    attempt: 0,
    onCodeChange: vi.fn(),
    onSwitch: vi.fn(),
    recentlyUsedCode: false,
    error: null,
    ...overrides,
  };
}

/** The elements an `aria-describedby` names; every id must resolve (no dangling reference). */
function describedBy(element: HTMLElement): HTMLElement[] {
  const ids = (element.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);
  return ids.map((id) => {
    const target = element.ownerDocument.getElementById(id);
    expect(target, `aria-describedby names #${id}, which does not exist`).not.toBeNull();
    return target as HTMLElement;
  });
}

describe('MFAFactorPicker panels', () => {
  it('authenticator app: a labelled numeric one-time-code input with its helper', () => {
    render(<MFAFactorPicker {...props()} />);
    const input = screen.getByRole('textbox', { name: 'Authenticator app code' });
    expect(input).toHaveAttribute('inputmode', 'numeric');
    expect(input).toHaveAttribute('autocomplete', 'one-time-code');
    expect(input).toHaveAttribute('spellcheck', 'false');
    expect(input).toHaveClass('step-up__input', 'step-up__input--totp');
    expect(describedBy(input).map((el) => el.textContent)).toEqual([
      'Enter the 6-digit code from your authenticator app.',
    ]);
    expect(input).not.toHaveAttribute('aria-invalid');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('backup code: a labelled text input that does not ask for a one-time-code', () => {
    render(<MFAFactorPicker {...props({ method: 'backup' })} />);
    const input = screen.getByRole('textbox', { name: 'Backup code' });
    expect(input).not.toHaveAttribute('inputmode');
    expect(input).toHaveAttribute('autocomplete', 'off');
    expect(input).toHaveClass('step-up__input--backup');
    expect(describedBy(input).map((el) => el.textContent)).toEqual([
      'Enter one of your 8-character backup codes.',
    ]);
  });

  it('passkey or security key: a named group with no input, described by its helper', () => {
    render(<MFAFactorPicker {...props({ method: 'webauthn' })} />);
    const group = screen.getByRole('group', { name: 'Passkey or security key' });
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    expect(describedBy(group).map((el) => el.textContent)).toEqual([
      "You'll be asked for it when you continue.",
    ]);
    // aria-invalid is not a valid attribute of a group.
    expect(group).not.toHaveAttribute('aria-invalid');
  });

  it('shows the code it is given and reports typing', async () => {
    const onCodeChange = vi.fn();
    render(<MFAFactorPicker {...props({ code: '12', onCodeChange })} />);
    const input = screen.getByRole('textbox', { name: 'Authenticator app code' });
    expect(input).toHaveValue('12');
    await userEvent.type(input, '3');
    expect(onCodeChange).toHaveBeenCalledWith('123');
  });
});

describe('MFAFactorPicker switch links', () => {
  // One link per OTHER offered method (E1), named for the method it goes to.
  it.each([
    ['totp', ['Use passkey or security key instead', 'Use a backup code instead']],
    ['webauthn', ['Use authenticator app instead', 'Use a backup code instead']],
    ['backup', ['Use passkey or security key instead', 'Use authenticator app instead']],
  ] as const)('on the %s panel offers exactly the other two methods', (method, labels) => {
    render(<MFAFactorPicker {...props({ method })} />);
    const names = screen.getAllByRole('button').map((b) => b.textContent);
    expect(names).toEqual(labels);
  });

  it('offers a link for each of the other methods and no link to the active one', () => {
    render(<MFAFactorPicker {...props({ methods: ['webauthn', 'totp'], method: 'totp' })} />);
    expect(
      screen.getByRole('button', { name: 'Use passkey or security key instead' })
    ).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Use authenticator app instead' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Use a backup code instead' })).toBeNull();
  });

  it('offers no switch with a single method', () => {
    render(<MFAFactorPicker {...props({ methods: ['totp'], method: 'totp' })} />);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('reports the method a link goes to', async () => {
    const onSwitch = vi.fn();
    render(<MFAFactorPicker {...props({ onSwitch })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Use a backup code instead' }));
    expect(onSwitch).toHaveBeenCalledExactlyOnceWith('backup');
  });

  it('the links are type=button so they never submit a host form', () => {
    render(<MFAFactorPicker {...props()} />);
    for (const button of screen.getAllByRole('button')) {
      expect(button).toHaveAttribute('type', 'button');
    }
  });
});

describe('MFAFactorPicker refusal wiring', () => {
  it('marks the input invalid and describes it by the helper and the one alert', () => {
    render(<MFAFactorPicker {...props({ error: 'That code did not work.' })} />);
    const input = screen.getByRole('textbox', { name: 'Authenticator app code' });
    const alert = screen.getByRole('alert');
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(input).toHaveAttribute('aria-invalid', 'true');
    const targets = describedBy(input);
    expect(targets.map((el) => el.textContent)).toEqual([
      'Enter the 6-digit code from your authenticator app.',
      'That code did not work.',
    ]);
    expect(targets[1]).toBe(alert);
  });

  it('the WebAuthn group is described by its helper and the alert', () => {
    render(<MFAFactorPicker {...props({ method: 'webauthn', error: 'Try again.' })} />);
    const group = screen.getByRole('group', { name: 'Passkey or security key' });
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(describedBy(group)).toContain(screen.getByRole('alert'));
  });

  it('renders no alert when there is no error', () => {
    render(<MFAFactorPicker {...props()} />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

describe('MFAFactorPicker recently-used hint (S2a)', () => {
  // Mutant: the hint rendered on every panel, or outside the helper the input is described by.
  it('replaces the helper on the authenticator-app panel only', () => {
    render(<MFAFactorPicker {...props({ recentlyUsedCode: true })} />);
    const input = screen.getByRole('textbox', { name: 'Authenticator app code' });
    expect(describedBy(input).map((el) => el.textContent)).toEqual([RECENT]);
    expect(screen.getAllByText(RECENT)).toHaveLength(1);
    expect(
      screen.queryByText('Enter the 6-digit code from your authenticator app.')
    ).not.toBeInTheDocument();
  });

  it.each(['backup', 'webauthn'] as const)('does not touch the %s panel', (method) => {
    const { container } = render(
      <MFAFactorPicker {...props({ method, recentlyUsedCode: true })} />
    );
    expect(container).not.toHaveTextContent(RECENT);
  });
});

describe('MFAFactorPicker input identity and focus handle', () => {
  // The key is `${method}-${attempt}`: a spent code and a switch remount the field.
  it('remounts the input when the attempt advances, and keeps it otherwise', () => {
    const view = render(<MFAFactorPicker {...props({ attempt: 0 })} />);
    const first = screen.getByRole('textbox');
    view.rerender(<MFAFactorPicker {...props({ attempt: 0, code: '1' })} />);
    expect(screen.getByRole('textbox')).toBe(first);
    view.rerender(<MFAFactorPicker {...props({ attempt: 1 })} />);
    expect(screen.getByRole('textbox')).not.toBe(first);
  });

  it('focus() lands on the code input', () => {
    const ref = createRef<MFAFactorPickerHandle>();
    render(<MFAFactorPicker {...props({ ref })} />);
    ref.current?.focus();
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toHaveFocus();
  });

  it('focus() lands on the label of the WebAuthn panel, which has no input', () => {
    const ref = createRef<MFAFactorPickerHandle>();
    render(<MFAFactorPicker {...props({ ref, method: 'webauthn' })} />);
    ref.current?.focus();
    const label = within(screen.getByRole('group')).getByText('Passkey or security key');
    expect(label).toHaveFocus();
    expect(label).toHaveAttribute('tabindex', '-1');
  });
});

describe('StepUpFieldError', () => {
  it('is one alert carrying the message, with a decorative glyph', () => {
    const { container } = render(<StepUpFieldError id="err-1" message="Nope." />);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveAttribute('id', 'err-1');
    expect(alert).toHaveTextContent('Nope.');
    const glyph = container.querySelector('svg');
    expect(glyph).toHaveAttribute('aria-hidden', 'true');
    expect(glyph).toHaveClass('step-up__error-glyph');
    expect(alert.textContent).toBe('Nope.');
  });
});
