import { useRef, useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';

// The shared step-up credentials stage (design 2026-09-26-mfa-factor-picker
// §4.1, §4.2; plan 2026-10-07 §3): the password field, the factor picker and
// the always-mounted status line, plus the primary button's activation guard.
//
// No axe library exists in this repo and none is added. The accessibility
// contract is asserted directly in every §4.2 state (`expectContract`): roles
// and accessible names, `aria-describedby` targets that exist and carry text,
// `aria-invalid` only on a refused field and only with its alert, the
// always-mounted <output>, exactly one role="alert" per refusal, and an
// `aria-disabled` primary whose click is guarded.
//
// Two harnesses: hand-built `StepUpFactor` fixtures render each state
// deterministically, and a real `useStepUpFactor` (apiFetch mocked at the
// module boundary, as in the hook's own tests) pins the behaviours that live
// across the hook and the component.
//
// "Mutant:" comments name the production change each test exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import StepUpCredentials, { stepUpActivation } from '@/renderer/components/Auth/StepUpCredentials';
import {
  useStepUpFactor,
  type StepUpFactor,
  type StepUpFactorProps,
  type StepUpMissing,
  type StepUpNotice,
  type StepUpStatus,
  type StepUpSubmit,
} from '@/renderer/hooks/auth/useStepUpFactor';
import { totpHintExpiresAt } from '@/renderer/services/system/stepUpRequirements';
import { useTotpAcceptedStore } from '@/renderer/stores/auth/totpAcceptedStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import {
  captureApiRequestContext,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';
import { useAuthStore } from '@/renderer/stores/auth/authStore';

const ACCOUNT = 'acct-1';
const TYPED = 'swordfish';
const CHECKING = 'Checking your verification methods…';
const WAITING = 'Waiting for your passkey or security key…';
const PREPARING = 'Getting things ready…';
const RATE_LIMITED = 'Too many attempts. Try again in a few minutes.';
const ENROLLMENT = 'Set up an authenticator app or security key in Settings to do this.';
const RECENT = 'You just used a code from your authenticator app. Enter the next one it shows.';

// ── Fixtures ─────────────────────────────────────────────────────────────

function makeFactor(overrides: Partial<StepUpFactor> = {}): StepUpFactor {
  return {
    status: { kind: 'ready' },
    methods: ['totp'],
    method: 'totp',
    passwordLegShown: true,
    code: '',
    attempt: 0,
    phase: 'idle',
    notice: null,
    reread: null,
    setCode: vi.fn(),
    switchTo: vi.fn(),
    retryRead: vi.fn(),
    firstMissing: vi.fn(() => null),
    announceMissing: vi.fn(),
    run: vi.fn(async () => null),
    ...overrides,
  };
}

const ALL = ['webauthn', 'totp', 'backup'] as const;

function ready(method: 'totp' | 'webauthn' | 'backup', extra: Partial<StepUpFactor> = {}) {
  return makeFactor({ methods: ALL, method, ...extra });
}

/** No offered method: the password alone, or nothing at all on a terminal state. */
function none(status: StepUpStatus, extra: Partial<StepUpFactor> = {}) {
  return makeFactor({
    status,
    methods: [],
    method: null,
    passwordLegShown: false,
    firstMissing: vi.fn(() => 'unavailable'),
    ...extra,
  });
}

interface StageProps {
  factor: StepUpFactor;
  password?: string;
  onPasswordChange?: (value: string) => void;
  submit?: StepUpSubmit;
  sessionMessage?: string;
  /** False omits `headingRef`, so the enclosing dialog takes terminal focus. */
  heading?: boolean;
  /** The capture a host took when it began preparing, passed to `stepUpActivation`. */
  capture?: ApiRequestContext;
  focusOnReady?: boolean;
  /** Renders a Cancel button before the stage, for a focus the stage must not take. */
  cancel?: boolean;
}

/** A host: heading, the stage, and a primary wired with `stepUpActivation`. */
function Stage({
  factor,
  password = TYPED,
  onPasswordChange = () => undefined,
  submit = async () => ({ kind: 'success' }),
  sessionMessage,
  heading = true,
  capture,
  focusOnReady,
  cancel = false,
}: Readonly<StageProps>) {
  const primaryRef = useRef<HTMLButtonElement>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit, {
    capture,
  });
  return (
    <dialog open tabIndex={-1} aria-label="Confirm">
      <h2 ref={headingRef} tabIndex={-1}>
        Confirm
      </h2>
      {cancel && <button type="button">Cancel</button>}
      <StepUpCredentials
        factor={factor}
        password={password}
        onPasswordChange={onPasswordChange}
        primaryRef={primaryRef}
        headingRef={heading ? headingRef : undefined}
        sessionMessage={sessionMessage}
        focusOnReady={focusOnReady}
      />
      <button type="button" ref={primaryRef} aria-disabled={ariaDisabled} onClick={activate}>
        Continue
      </button>
    </dialog>
  );
}

const primary = () => screen.getByRole('button', { name: 'Continue' });
const passwordField = () => screen.getByLabelText('Password');
const statusLine = () => screen.getByRole('status');
const headingEl = () => screen.getByRole('heading', { name: 'Confirm' });

// ── The accessibility contract ───────────────────────────────────────────

function ids(element: Element): string[] {
  return (element.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);
}

/** Asserts the state-independent half of the contract; `alerts` is the refusal count. */
function expectContract(alerts: number) {
  // The status line is always mounted, once, inside the stage.
  const outputs = document.querySelectorAll('output');
  expect(outputs).toHaveLength(1);
  expect(statusLine()).toBe(outputs[0]);
  expect(outputs[0].closest('fieldset.step-up')).not.toBeNull();

  expect(screen.queryAllByRole('alert')).toHaveLength(alerts);

  // No dangling or empty description, and no duplicate id for one to land on.
  const allIds = [...document.querySelectorAll('[id]')].map((el) => el.id);
  expect(new Set(allIds).size).toBe(allIds.length);
  for (const element of document.querySelectorAll('[aria-describedby]')) {
    for (const id of ids(element)) {
      const target = document.getElementById(id);
      expect(target, `#${id} named by aria-describedby does not exist`).not.toBeNull();
      expect(target?.textContent?.trim(), `#${id} carries no text`).toBeTruthy();
    }
  }

  // A refused field says so, and what it points at includes the alert; nothing else does.
  const invalid = document.querySelectorAll('[aria-invalid="true"]');
  const reachable = [...invalid].flatMap((el) => ids(el).map((id) => document.getElementById(id)));
  expect(reachable.filter((el) => el?.getAttribute('role') === 'alert')).toHaveLength(
    invalid.length
  );
  expect(invalid.length).toBeLessThanOrEqual(alerts);
  if (alerts === 0) expect(document.querySelector('.step-up__error')).toBeNull();

  // Every control is named.
  for (const control of document.querySelectorAll('input, button, [role="group"]')) {
    expect(control).toHaveAccessibleName();
  }

  // The status line is text only: no glyph, no danger carrier.
  expect(outputs[0].className).toBe('step-up__status');
  expect(outputs[0].querySelector('svg, [class]')).toBeNull();
  expect(document.querySelector('[class*="danger"]')).toBeNull();
}

interface Row {
  name: string;
  factor: StepUpFactor;
  /** The <output>'s exact text. */
  status: string;
  /** role="alert" count: one per refused field, none for a status. */
  alerts: number;
  /** The accessible name of the field or group that must be present, if any. */
  panel?: string;
  /** True when the primary must be aria-disabled. */
  blocked: boolean;
}

const notice = (n: StepUpNotice, extra: Partial<StepUpFactor> = {}) =>
  ready('totp', { notice: n, ...extra });

const ROWS: Row[] = [
  {
    name: 'reading, before the delay',
    factor: none(
      { kind: 'reading' },
      { passwordLegShown: true, firstMissing: vi.fn(() => 'reading') }
    ),
    status: '',
    alerts: 0,
    blocked: true,
  },
  {
    name: 'ready, authenticator app',
    factor: ready('totp'),
    status: '',
    alerts: 0,
    panel: 'Authenticator app code',
    blocked: false,
  },
  {
    name: 'ready, passkey or security key',
    factor: ready('webauthn'),
    status: '',
    alerts: 0,
    panel: 'Passkey or security key',
    blocked: false,
  },
  {
    name: 'ready, backup code',
    factor: ready('backup'),
    status: '',
    alerts: 0,
    panel: 'Backup code',
    blocked: false,
  },
  {
    name: 'ready, password only',
    factor: none({ kind: 'ready' }, { passwordLegShown: true, firstMissing: vi.fn(() => null) }),
    status: '',
    alerts: 0,
    blocked: false,
  },
  {
    name: 'blocked',
    factor: none({ kind: 'blocked' }),
    status: "We couldn't check your verification methods. Check your connection and try again.",
    alerts: 0,
    blocked: true,
  },
  {
    name: 'refused, account',
    factor: none({ kind: 'refused', reason: 'account' }),
    status: "Your account can't do this right now.",
    alerts: 0,
    blocked: true,
  },
  {
    name: 'refused, email unverified',
    factor: none({ kind: 'refused', reason: 'emailUnverified' }),
    status: 'Verify your email address to do this.',
    alerts: 0,
    blocked: true,
  },
  {
    name: 'refused, client',
    factor: none({ kind: 'refused', reason: 'client' }),
    status: "This isn't available right now.",
    alerts: 0,
    blocked: true,
  },
  {
    name: 'refused, session',
    factor: none({ kind: 'refused', reason: 'session' }),
    status: 'Sign in again to continue.',
    alerts: 0,
    blocked: true,
  },
  {
    name: 'no usable method',
    factor: none({ kind: 'noUsableMethod' }),
    status:
      "Your account's verification method can't be used here. Add an authenticator app or security key in Settings.",
    alerts: 0,
    blocked: true,
  },
  {
    name: 'session expired',
    factor: none({ kind: 'sessionExpired' }),
    status: 'Sign in again to continue.',
    alerts: 0,
    blocked: true,
  },
  {
    name: 'enrolment required',
    factor: none({ kind: 'enrollmentRequired' }),
    status: ENROLLMENT,
    alerts: 0,
    blocked: true,
  },
  {
    name: 'ceremony',
    factor: ready('webauthn', { phase: 'ceremony' }),
    status: WAITING,
    alerts: 0,
    panel: 'Passkey or security key',
    blocked: true,
  },
  {
    name: 'submitting',
    factor: ready('totp', { phase: 'submitting', code: '123456' }),
    status: '',
    alerts: 0,
    panel: 'Authenticator app code',
    blocked: true,
  },
  {
    name: 'notice: checking',
    factor: none(
      { kind: 'reading' },
      {
        passwordLegShown: true,
        notice: { kind: 'checking' },
        firstMissing: vi.fn(() => 'reading'),
      }
    ),
    status: CHECKING,
    alerts: 0,
    blocked: true,
  },
  {
    name: 'notice: missing password',
    factor: notice(
      { kind: 'missing', field: 'password' },
      { firstMissing: vi.fn(() => 'password') }
    ),
    status: '',
    alerts: 1,
    panel: 'Authenticator app code',
    blocked: true,
  },
  {
    name: 'notice: missing authenticator code',
    factor: notice({ kind: 'missing', field: 'totp' }, { firstMissing: vi.fn(() => 'code') }),
    status: '',
    alerts: 1,
    panel: 'Authenticator app code',
    blocked: true,
  },
  {
    name: 'notice: missing backup code',
    factor: ready('backup', {
      notice: { kind: 'missing', field: 'backup' },
      firstMissing: vi.fn(() => 'code'),
    }),
    status: '',
    alerts: 1,
    panel: 'Backup code',
    blocked: true,
  },
  {
    name: 'notice: missing passkey',
    factor: ready('webauthn', { notice: { kind: 'missing', field: 'webauthn' } }),
    status: '',
    alerts: 1,
    panel: 'Passkey or security key',
    blocked: false,
  },
  {
    name: 'notice: invalid password',
    factor: notice({ kind: 'invalidPassword' }),
    status: '',
    alerts: 1,
    panel: 'Authenticator app code',
    blocked: false,
  },
  {
    name: 'notice: invalid authenticator code',
    factor: notice({ kind: 'invalidFactor', method: 'totp' }),
    status: '',
    alerts: 1,
    panel: 'Authenticator app code',
    blocked: false,
  },
  {
    name: 'notice: invalid backup code',
    factor: ready('backup', { notice: { kind: 'invalidFactor', method: 'backup' } }),
    status: '',
    alerts: 1,
    panel: 'Backup code',
    blocked: false,
  },
  {
    name: 'notice: invalid passkey',
    factor: ready('webauthn', { notice: { kind: 'invalidFactor', method: 'webauthn' } }),
    status: '',
    alerts: 1,
    panel: 'Passkey or security key',
    blocked: false,
  },
  {
    name: 'notice: passkey cancelled',
    factor: ready('webauthn', { notice: { kind: 'webauthnCancelled' } }),
    status: 'Passkey or security key request was cancelled or timed out. Try again.',
    alerts: 0,
    panel: 'Passkey or security key',
    blocked: false,
  },
  {
    name: 'notice: preparing',
    factor: notice({ kind: 'preparing' }, { firstMissing: vi.fn(() => 'preparing') }),
    status: PREPARING,
    alerts: 0,
    panel: 'Authenticator app code',
    blocked: true,
  },
  {
    name: 'notice: passkey rate limited',
    factor: ready('webauthn', { notice: { kind: 'webauthnRateLimited' } }),
    status: RATE_LIMITED,
    alerts: 0,
    panel: 'Passkey or security key',
    blocked: false,
  },
  {
    name: 'notice: methods changed',
    factor: notice({ kind: 'methodsChanged' }),
    status: 'Your verification methods changed. Use the one shown.',
    alerts: 0,
    panel: 'Authenticator app code',
    blocked: false,
  },
];

beforeEach(() => {
  resetAllStores();
  useUserStore.setState({ user: { id: ACCOUNT } as never });
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('the accessibility contract holds in every state', () => {
  // Mutant: a missing <output>; danger styling (an error box or aria-invalid) on a
  // refused, blocked or no-usable-method status; a second alert for one refusal.
  it.each(ROWS)('$name', ({ factor, status, alerts, panel, blocked }) => {
    render(<Stage factor={factor} />);
    expectContract(alerts);
    expect(statusLine().textContent).toBe(status);
    expect(primary()).toHaveAttribute('aria-disabled', String(blocked));
    // A guarded primary is never natively disabled: it must stay focusable.
    expect(primary()).not.toBeDisabled();
    if (panel !== undefined) {
      const named =
        screen.queryByRole('textbox', { name: panel }) ??
        screen.queryByRole('group', { name: panel });
      expect(named).not.toBeNull();
    } else {
      expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
      expect(document.querySelector('.step-up__panel')).toBeNull();
    }
  });

  it('the rows cover every StepUpStatus kind and every StepUpNotice kind', () => {
    const covered = new Set(ROWS.map((row) => row.factor.status.kind));
    expect([...covered].sort()).toEqual(
      [
        'blocked',
        'enrollmentRequired',
        'noUsableMethod',
        'ready',
        'reading',
        'refused',
        'sessionExpired',
      ].sort()
    );
    const notices = new Set(ROWS.map((row) => row.factor.notice?.kind).filter(Boolean));
    expect([...notices].sort()).toEqual(
      [
        'checking',
        'invalidFactor',
        'invalidPassword',
        'methodsChanged',
        'missing',
        'preparing',
        'webauthnCancelled',
        'webauthnRateLimited',
      ].sort()
    );
  });
});

// ── Copy and field wiring ────────────────────────────────────────────────

describe('refusals sit on the field they name', () => {
  it('a password refusal marks the password field and is its description', () => {
    render(<Stage factor={ready('totp', { notice: { kind: 'invalidPassword' } })} />);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('That password is not correct.');
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
    expect(ids(passwordField())).toEqual([alert.id]);
    // The code field is not the one refused.
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).not.toHaveAttribute(
      'aria-invalid'
    );
  });

  it('a password refusal under a passkey warns the key is asked for again', () => {
    render(<Stage factor={ready('webauthn', { notice: { kind: 'invalidPassword' } })} />);
    expect(screen.getByRole('alert')).toHaveTextContent(
      "That password is not correct. You'll be asked for your passkey or security key again."
    );
  });

  it('an empty password asks for the password', () => {
    render(<Stage factor={ready('totp', { notice: { kind: 'missing', field: 'password' } })} />);
    expect(screen.getByRole('alert')).toHaveTextContent('Enter your password to continue.');
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
  });

  it.each([
    [
      'totp',
      'Authenticator app code',
      'Enter the 6-digit code from your authenticator app to continue.',
    ],
    ['backup', 'Backup code', 'Enter a backup code to continue.'],
  ] as const)('an empty %s field asks for it', (field, name, text) => {
    render(<Stage factor={ready(field, { notice: { kind: 'missing', field } })} />);
    const input = screen.getByRole('textbox', { name });
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(within(screen.getByRole('alert')).getByText(text)).toBeInTheDocument();
    expect(ids(input)).toContain(screen.getByRole('alert').id);
    expect(passwordField()).not.toHaveAttribute('aria-invalid');
  });

  // Mutant (C6): the WebAuthn mfaRequired copy naming the authenticator app.
  it('the passkey "needs it too" copy never names the authenticator app', () => {
    render(
      <Stage factor={ready('webauthn', { notice: { kind: 'missing', field: 'webauthn' } })} />
    );
    const group = screen.getByRole('group', { name: 'Passkey or security key' });
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent(
      'This also needs your passkey or security key. Try again to use it.'
    );
    expect(alert.textContent).not.toMatch(/authenticator|app\b|code/i);
    expect(ids(group)).toContain(alert.id);
    // A group is not a field: it cannot be invalid.
    expect(group).not.toHaveAttribute('aria-invalid');
  });

  it.each([
    ['totp', 'Authenticator app code', /^That code didn't work\./],
    ['backup', 'Backup code', /^That backup code is not correct/],
  ] as const)('a refused %s code says so on that field', (method, name, pattern) => {
    render(<Stage factor={ready(method, { notice: { kind: 'invalidFactor', method } })} />);
    expect(screen.getByRole('alert').textContent).toMatch(pattern);
    expect(screen.getByRole('textbox', { name })).toHaveAttribute('aria-invalid', 'true');
  });

  it('a refused passkey says so on the group', () => {
    render(
      <Stage
        factor={ready('webauthn', { notice: { kind: 'invalidFactor', method: 'webauthn' } })}
      />
    );
    expect(screen.getByRole('alert')).toHaveTextContent(
      "We couldn't verify your passkey or security key. Try again."
    );
  });

  it('never uses the words factor or step-up', () => {
    for (const row of ROWS) {
      const { container, unmount } = render(<Stage factor={row.factor} />);
      expect(container.textContent).not.toMatch(/step-up|factor/i);
      unmount();
    }
  });
});

describe('the session sentence', () => {
  it('defaults, and takes the host sentence for a session refusal and an expiry', () => {
    const { rerender } = render(<Stage factor={none({ kind: 'refused', reason: 'session' })} />);
    expect(statusLine()).toHaveTextContent('Sign in again to continue.');
    rerender(
      <Stage
        factor={none({ kind: 'refused', reason: 'session' })}
        sessionMessage="Sign in again to clear history."
      />
    );
    expect(statusLine()).toHaveTextContent('Sign in again to clear history.');
    rerender(
      <Stage
        factor={none({ kind: 'sessionExpired' })}
        sessionMessage="Sign in again to clear history."
      />
    );
    expect(statusLine()).toHaveTextContent('Sign in again to clear history.');
  });

  it('does not hand the host sentence to the other refusals', () => {
    render(
      <Stage factor={none({ kind: 'refused', reason: 'account' })} sessionMessage="Custom." />
    );
    expect(statusLine()).toHaveTextContent("Your account can't do this right now.");
  });
});

describe('the password field', () => {
  it('is a labelled current-password field showing the host value', async () => {
    const onPasswordChange = vi.fn();
    render(<Stage factor={ready('totp')} password="ab" onPasswordChange={onPasswordChange} />);
    expect(passwordField()).toHaveAttribute('type', 'password');
    expect(passwordField()).toHaveAttribute('autocomplete', 'current-password');
    expect(passwordField()).toHaveValue('ab');
    expect(passwordField()).not.toHaveAttribute('aria-describedby');
    await userEvent.type(passwordField(), 'c');
    expect(onPasswordChange).toHaveBeenLastCalledWith('abc');
  });

  it('is absent when the hook does not show the password leg', () => {
    render(<Stage factor={ready('totp', { passwordLegShown: false })} />);
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // A terminal stage can send nothing, so the password it hid is dropped.
  // Mutant: the clearing effect deleted, or limited to one of the three kinds.
  it.each([
    ['refused', { kind: 'refused', reason: 'account' }],
    ['no usable method', { kind: 'noUsableMethod' }],
    ['enrolment required', { kind: 'enrollmentRequired' }],
    ['session expired', { kind: 'sessionExpired' }],
  ] as const)('is emptied through the host once the status is %s', async (_name, status) => {
    const onPasswordChange = vi.fn();
    render(<Stage factor={none(status)} password="typed" onPasswordChange={onPasswordChange} />); // pragma: allowlist secret
    await waitFor(() => expect(onPasswordChange).toHaveBeenCalledExactlyOnceWith(''));
  });

  // Mutant: the clearing effect unconditional, or keyed to something other than a terminal status.
  it.each([
    ['reading', none({ kind: 'reading' }, { passwordLegShown: true })],
    ['blocked', none({ kind: 'blocked' })],
    ['ready', ready('totp')],
  ])('is kept while the status is %s', (_name, factor) => {
    const onPasswordChange = vi.fn();
    render(<Stage factor={factor} password="typed" onPasswordChange={onPasswordChange} />); // pragma: allowlist secret
    expect(onPasswordChange).not.toHaveBeenCalled();
  });

  it('asks the host to empty nothing when a terminal stage holds no password', () => {
    const onPasswordChange = vi.fn();
    render(
      <Stage
        factor={none({ kind: 'noUsableMethod' })}
        password=""
        onPasswordChange={onPasswordChange}
      />
    );
    expect(onPasswordChange).not.toHaveBeenCalled();
  });

  it('hands the host its ref, so a host can aim initial focus at it', () => {
    function WithRef() {
      const passwordRef = useRef<HTMLInputElement>(null);
      const primaryRef = useRef<HTMLButtonElement>(null);
      return (
        <>
          <StepUpCredentials
            factor={ready('totp')}
            password=""
            onPasswordChange={() => undefined}
            primaryRef={primaryRef}
            passwordRef={passwordRef}
          />
          <button type="button" onClick={() => passwordRef.current?.focus()}>
            aim
          </button>
        </>
      );
    }
    render(<WithRef />);
    act(() => screen.getByRole('button', { name: 'aim' }).click());
    expect(passwordField()).toHaveFocus();
  });
});

// ── Phases ───────────────────────────────────────────────────────────────

describe('phases', () => {
  // Mutant (C37): the password editable during a WebAuthn ceremony.
  it('the password is read-only during the ceremony and ignores typing', async () => {
    const onPasswordChange = vi.fn();
    render(
      <Stage
        factor={ready('webauthn', { phase: 'ceremony' })}
        onPasswordChange={onPasswordChange}
      />
    );
    expect(passwordField()).toHaveAttribute('readonly');
    expect(passwordField()).not.toBeDisabled();
    await userEvent.type(passwordField(), 'x');
    expect(onPasswordChange).not.toHaveBeenCalled();
  });

  it('the password is editable when idle', () => {
    render(<Stage factor={ready('webauthn')} />);
    expect(passwordField()).not.toHaveAttribute('readonly');
  });

  it('keeps the switch links live during the ceremony, so a user can leave it', async () => {
    const factor = ready('webauthn', { phase: 'ceremony' });
    render(<Stage factor={factor} />);
    const link = screen.getByRole('button', { name: 'Use authenticator app instead' });
    expect(link).toBeEnabled();
    await userEvent.click(link);
    expect(factor.switchTo).toHaveBeenCalledExactlyOnceWith('totp');
  });

  it('disables every control while the request is out', () => {
    render(<Stage factor={ready('totp', { phase: 'submitting' })} />);
    expect(passwordField()).toBeDisabled();
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toBeDisabled();
    expect(
      screen.getByRole('button', { name: 'Use passkey or security key instead' })
    ).toBeDisabled();
    // The host's primary sits outside the fieldset and stays guarded, not disabled.
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
  });

  it('enables every control when idle', () => {
    render(<Stage factor={ready('totp')} />);
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toBeEnabled();
    expect(passwordField()).toBeEnabled();
  });
});

describe('Retry', () => {
  it('is offered only when blocked, and asks the hook to read again', async () => {
    const blocked = none({ kind: 'blocked' });
    const { rerender } = render(<Stage factor={blocked} />);
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(blocked.retryRead).toHaveBeenCalledTimes(1);
    for (const status of [{ kind: 'refused', reason: 'client' }, { kind: 'ready' }] as const) {
      rerender(<Stage factor={none(status)} />);
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    }
  });
});

// ── The reading notice ───────────────────────────────────────────────────

describe('the reading notice', () => {
  const reading = () => none({ kind: 'reading' }, { passwordLegShown: true });

  // Mutant: "Checking your verification methods…" shown before 400 ms of reading.
  it('appears only after 400 ms of reading, and withdraws when the read lands', () => {
    vi.useFakeTimers();
    const { rerender } = render(<Stage factor={reading()} />);
    expect(statusLine().textContent).toBe('');
    act(() => {
      vi.advanceTimersByTime(399);
    });
    expect(statusLine().textContent).toBe('');
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(statusLine().textContent).toBe(CHECKING);
    expectContract(0);

    rerender(<Stage factor={ready('totp')} />);
    expect(statusLine().textContent).toBe('');
  });

  it('restarts the delay for a second read (Retry)', () => {
    vi.useFakeTimers();
    const { rerender } = render(<Stage factor={reading()} />);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(statusLine().textContent).toBe(CHECKING);
    rerender(<Stage factor={none({ kind: 'blocked' })} />);
    expect(statusLine().textContent).not.toBe(CHECKING);
    rerender(<Stage factor={reading()} />);
    expect(statusLine().textContent).toBe('');
    act(() => {
      vi.advanceTimersByTime(400);
    });
    expect(statusLine().textContent).toBe(CHECKING);
  });

  it('does not show the notice for a read that is not in flight', () => {
    vi.useFakeTimers();
    render(<Stage factor={ready('totp')} />);
    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(statusLine().textContent).toBe('');
  });
});

// ── The recently-used code hint (S2a) ────────────────────────────────────

describe('the recently-used code hint', () => {
  const NOW = Date.UTC(2026, 9, 7, 12, 0, 10);

  function acceptedNow() {
    useTotpAcceptedStore.getState().noteTotpAccepted(ACCOUNT, Date.now());
  }

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });

  // Mutant: the hint rendered outside the TOTP panel (another panel, or the status line).
  it('replaces the helper inside the authenticator-app panel, described to the input', () => {
    acceptedNow();
    render(<Stage factor={ready('totp')} />);
    const input = screen.getByRole('textbox', { name: 'Authenticator app code' });
    const hint = screen.getByText(RECENT);
    expect(hint.closest('.step-up__panel')).not.toBeNull();
    expect(ids(input)).toEqual([hint.id]);
    expect(statusLine()).not.toHaveTextContent(RECENT);
    expectContract(0);
  });

  it.each(['backup', 'webauthn'] as const)('does not appear on the %s panel', (method) => {
    acceptedNow();
    const { container } = render(<Stage factor={ready(method)} />);
    expect(container).not.toHaveTextContent(RECENT);
  });

  it('does not appear for an account with no accepted code', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted('someone-else', Date.now());
    const { container } = render(<Stage factor={ready('totp')} />);
    expect(container).not.toHaveTextContent(RECENT);
  });

  // Mutant: the hint outliving totpHintExpiresAt.
  it('withdraws itself at the end of the period the server refuses a repeat from', () => {
    acceptedNow();
    const expiresAt = totpHintExpiresAt(Date.now());
    render(<Stage factor={ready('totp')} />);
    act(() => {
      vi.advanceTimersByTime(expiresAt - Date.now() - 1);
    });
    expect(screen.getByText(RECENT)).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(screen.queryByText(RECENT)).not.toBeInTheDocument();
    expect(
      screen.getByText('Enter the 6-digit code from your authenticator app.')
    ).toBeInTheDocument();
  });

  // Mutant: `setNow(Date.now())` in the expiry timer. A timer can fire a
  // millisecond before the instant it was set for (the clock was stepped back
  // after it was scheduled); `now` then lands short of the expiry, the hint
  // stays up and nothing is left to reschedule it.
  it('withdraws when the timer fires a millisecond early', () => {
    acceptedNow();
    const expiresAt = totpHintExpiresAt(Date.now());
    const remaining = expiresAt - Date.now();
    render(<Stage factor={ready('totp')} />);
    expect(screen.getByText(RECENT)).toBeInTheDocument();

    vi.setSystemTime(Date.now() - 1);
    act(() => {
      vi.advanceTimersByTime(remaining);
    });

    expect(Date.now()).toBe(expiresAt - 1);
    expect(screen.queryByText(RECENT)).not.toBeInTheDocument();
  });

  it('is already gone when the accepted code is older than the period', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted(ACCOUNT, NOW - 120_000);
    const { container } = render(<Stage factor={ready('totp')} />);
    expect(container).not.toHaveTextContent(RECENT);
  });
});

// ── The activation guard ─────────────────────────────────────────────────

describe('stepUpActivation', () => {
  const submit: StepUpSubmit = async () => ({ kind: 'success' });

  // Mutant: an unguarded aria-disabled primary (activate calling run while anything is missing).
  it.each(['reading', 'password', 'code', 'unavailable'] as const)(
    'is aria-disabled and runs nothing while %s is missing',
    (missing: StepUpMissing) => {
      const factor = makeFactor({ firstMissing: vi.fn(() => missing) });
      const { ariaDisabled, activate } = stepUpActivation(factor, '', submit);
      expect(ariaDisabled).toBe(true);
      activate();
      expect(factor.run).not.toHaveBeenCalled();
      expect(factor.announceMissing).toHaveBeenCalledExactlyOnceWith(missing);
    }
  );

  // Mutant: the busy check dropped, so a second click starts a second activation.
  it.each(['ceremony', 'submitting'] as const)('does nothing at all in the %s phase', (phase) => {
    const factor = makeFactor({ phase });
    const { ariaDisabled, activate } = stepUpActivation(factor, TYPED, submit);
    expect(ariaDisabled).toBe(true);
    activate();
    expect(factor.run).not.toHaveBeenCalled();
    expect(factor.announceMissing).not.toHaveBeenCalled();
  });

  it('a busy primary does not even announce what is missing', () => {
    const factor = makeFactor({ phase: 'ceremony', firstMissing: vi.fn(() => 'code') });
    stepUpActivation(factor, '', submit).activate();
    expect(factor.announceMissing).not.toHaveBeenCalled();
    expect(factor.run).not.toHaveBeenCalled();
  });

  // Without a capture the run works against its instance's own, so `run`
  // receives the host submit and no capture.
  // Mutant: a capture invented here (a fresh one) for a host that gave none.
  it('runs once with the host submit and no capture when complete', () => {
    const factor = makeFactor();
    const { ariaDisabled, activate } = stepUpActivation(factor, TYPED, submit);
    expect(ariaDisabled).toBe(false);
    activate();
    expect(factor.run).toHaveBeenCalledExactlyOnceWith(submit, undefined);
    expect(factor.announceMissing).not.toHaveBeenCalled();
  });

  // Mutant (C82): the capture dropped, so `run` takes a fresh one and a
  // capture taken before preparation is spent against whoever is current now.
  it('runs with the capture it was given', () => {
    const factor = makeFactor();
    const capture = captureApiRequestContext();
    const { activate } = stepUpActivation(factor, TYPED, submit, { capture });
    activate();
    expect(factor.run).toHaveBeenCalledExactlyOnceWith(submit, capture);
  });

  // Mutant: the capture read from the wrong option, or only on the busy path.
  it('an empty options object is the same as none', () => {
    const factor = makeFactor();
    stepUpActivation(factor, TYPED, submit, {}).activate();
    expect(factor.run).toHaveBeenCalledExactlyOnceWith(submit, undefined);
  });

  it('asks the hook about the password it was given', () => {
    const factor = makeFactor();
    stepUpActivation(factor, 'typed', submit);
    expect(factor.firstMissing).toHaveBeenCalledWith('typed');
  });

  it('a click on the rendered primary follows the same guard', async () => {
    const factor = makeFactor({ firstMissing: vi.fn(() => 'code') });
    render(<Stage factor={factor} />);
    await userEvent.click(primary());
    expect(factor.run).not.toHaveBeenCalled();
    expect(factor.announceMissing).toHaveBeenCalledExactlyOnceWith('code');
  });
});

// ── Focus (plan §3's table) ──────────────────────────────────────────────

describe('focus', () => {
  /** Mount `before`, then move the same stage to `after`, as the hook would. */
  function move(before: StepUpFactor, after: StepUpFactor, props: Partial<StageProps> = {}) {
    const view = render(<Stage factor={before} {...props} />);
    view.rerender(<Stage factor={after} {...props} />);
    return view;
  }

  function expectNeitherBodyNorPrimary() {
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement).not.toBe(primary());
  }

  it('moves nothing on first mount of a usable state', () => {
    render(<Stage factor={ready('totp')} />);
    expect(document.activeElement).toBe(document.body);
  });

  describe('a refusal', () => {
    it('a password refusal goes to the password field', () => {
      move(ready('totp'), ready('totp', { notice: { kind: 'invalidPassword' } }));
      expect(passwordField()).toHaveFocus();
    });

    it('an empty password goes to the password field', () => {
      move(ready('totp'), ready('totp', { notice: { kind: 'missing', field: 'password' } }));
      expect(passwordField()).toHaveFocus();
    });

    it.each([
      ['totp', 'Authenticator app code'],
      ['backup', 'Backup code'],
    ] as const)('a refused %s code goes to the remounted input', (method, name) => {
      const view = render(<Stage factor={ready(method)} />);
      const before = screen.getByRole('textbox', { name });
      view.rerender(
        <Stage factor={ready(method, { attempt: 1, notice: { kind: 'invalidFactor', method } })} />
      );
      const after = screen.getByRole('textbox', { name });
      expect(after).not.toBe(before);
      expect(after).toHaveFocus();
    });

    it.each([
      ['totp', 'Authenticator app code'],
      ['backup', 'Backup code'],
    ] as const)('an empty %s code goes to the input', (method, name) => {
      move(ready(method), ready(method, { notice: { kind: 'missing', field: method } }));
      expect(screen.getByRole('textbox', { name })).toHaveFocus();
    });

    it.each([
      ['a passkey refusal', { kind: 'invalidFactor', method: 'webauthn' }],
      ['a cancelled or timed-out passkey request', { kind: 'webauthnCancelled' }],
      ['a passkey the server still needs', { kind: 'missing', field: 'webauthn' }],
    ] as const)('%s goes to the primary, because retrying needs a touch', (_name, n) => {
      move(ready('webauthn'), ready('webauthn', { notice: n }));
      expect(primary()).toHaveFocus();
    });

    it('a methods-changed notice goes to the new panel', () => {
      move(ready('webauthn'), ready('totp', { notice: { kind: 'methodsChanged' } }));
      expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toHaveFocus();
    });

    it('a reading notice moves focus nowhere', () => {
      move(none({ kind: 'reading' }), none({ kind: 'reading' }, { notice: { kind: 'checking' } }));
      expect(document.activeElement).toBe(document.body);
    });
  });

  describe('an automatic switch', () => {
    // Mutant (C54): focus falling to <body> after an automatic method switch.
    it.each([
      ['webauthn', 'totp', () => screen.getByRole('textbox', { name: 'Authenticator app code' })],
      ['totp', 'backup', () => screen.getByRole('textbox', { name: 'Backup code' })],
      [
        'backup',
        'webauthn',
        () =>
          within(screen.getByRole('group', { name: 'Passkey or security key' })).getByText(
            'Passkey or security key'
          ),
      ],
    ] as const)('%s to %s lands on the new panel', (from, to, target) => {
      const view = render(<Stage factor={ready(from)} />);
      const stale = document.querySelector<HTMLElement>(
        '.step-up__input, .step-up__label[tabindex]'
      );
      stale?.focus();
      expect(document.activeElement).toBe(stale);
      view.rerender(<Stage factor={ready(to)} />);
      expect(target()).toHaveFocus();
      expectNeitherBodyNorPrimary();
    });

    // Mutant (C54): an empty offered set leaving focus on the unmounted panel's body.
    it('an emptied set lands on the stage heading', () => {
      const view = render(<Stage factor={ready('totp')} />);
      screen.getByRole('textbox', { name: 'Authenticator app code' }).focus();
      view.rerender(<Stage factor={none({ kind: 'ready' }, { passwordLegShown: true })} />);
      expect(headingEl()).toHaveFocus();
    });

    it('an emptied set lands on the dialog when the host named no heading', () => {
      const view = render(<Stage factor={ready('totp')} heading={false} />);
      screen.getByRole('textbox', { name: 'Authenticator app code' }).focus();
      view.rerender(
        <Stage factor={none({ kind: 'ready' }, { passwordLegShown: true })} heading={false} />
      );
      expect(screen.getByRole('dialog')).toHaveFocus();
    });

    it('a user-made switch lands on the new input too', () => {
      move(ready('totp'), ready('backup'));
      expect(screen.getByRole('textbox', { name: 'Backup code' })).toHaveFocus();
    });
  });

  describe('a terminal state', () => {
    it.each([
      ['refused', { kind: 'refused', reason: 'account' }],
      ['no usable method', { kind: 'noUsableMethod' }],
      ['enrolment required', { kind: 'enrollmentRequired' }],
      ['session expired', { kind: 'sessionExpired' }],
    ] as const)('%s lands on the heading, never the primary or body', (_name, status) => {
      move(ready('totp'), none(status));
      expect(headingEl()).toHaveFocus();
      expectNeitherBodyNorPrimary();
    });

    it('lands on the heading when it is the first thing mounted', () => {
      render(<Stage factor={none({ kind: 'refused', reason: 'client' })} />);
      expect(headingEl()).toHaveFocus();
    });

    it('lands on the dialog when the host named no heading', () => {
      render(<Stage factor={none({ kind: 'noUsableMethod' })} heading={false} />);
      expect(screen.getByRole('dialog')).toHaveFocus();
    });
  });

  describe('Retry', () => {
    // Mutant (C54): Retry unmounting with focus still on it, so focus falls to <body>.
    it('moves focus to the heading before the read restarts', async () => {
      function Retrying() {
        const [factor, setFactor] = useState(() => none({ kind: 'blocked' }));
        return (
          <Stage
            factor={{
              ...factor,
              retryRead: () => setFactor(none({ kind: 'reading' }, { passwordLegShown: true })),
            }}
          />
        );
      }
      render(<Retrying />);
      await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(headingEl()).toHaveFocus();
      expect(document.activeElement).not.toBe(document.body);
    });

    it('moves focus to the dialog when the host named no heading', async () => {
      function Retrying() {
        const [factor, setFactor] = useState(() => none({ kind: 'blocked' }));
        return (
          <Stage
            heading={false}
            factor={{ ...factor, retryRead: () => setFactor(none({ kind: 'reading' })) }}
          />
        );
      }
      render(<Retrying />);
      await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
      expect(screen.getByRole('dialog')).toHaveFocus();
    });
  });
});

// ── Enrolment required (E8; T2) ──────────────────────────────────────────

describe('enrolment required', () => {
  /** What the state must be, whichever way it was reached: a sentence and nothing to act on. */
  function expectEnrolmentState() {
    expect(statusLine().tagName).toBe('OUTPUT');
    expect(statusLine().textContent).toBe(ENROLLMENT);
    // Not an error: no alert role, no danger carrier, no invalid field.
    expect(screen.queryAllByRole('alert')).toHaveLength(0);
    expect(document.querySelector('[class*="danger"], .step-up__error')).toBeNull();
    expect(document.querySelector('[aria-invalid]')).toBeNull();
    // Nothing to collect and nothing to retry or follow (E4 is #3456's).
    expect(document.querySelectorAll('input')).toHaveLength(0);
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    expect(document.querySelector('.step-up__panel')).toBeNull();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    const stage = statusLine().closest('fieldset');
    expect(stage).not.toBeNull();
    expect(within(stage as HTMLElement).queryAllByRole('button')).toHaveLength(0);
    // The primary is the host's, and it cannot be activated.
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    expect(primary()).not.toBeDisabled();
    // The stand-in for an axe run (no axe library exists in this repo).
    expectContract(0);
  }

  // Mutant: the arm routed to `blocked` or `refused` (a Retry, a danger carrier,
  // a different sentence); an alert role or danger class on the sentence.
  it('shows the sentence as a polite status with nothing to act on', () => {
    render(<Stage factor={none({ kind: 'enrollmentRequired' })} />);
    expectEnrolmentState();
  });

  // Mutant: focus left on the primary or body, or the heading missing from the table.
  it('puts focus on the heading, not the primary or body', () => {
    render(<Stage factor={none({ kind: 'enrollmentRequired' })} />);
    expect(headingEl()).toHaveFocus();
  });

  it('puts focus on the dialog when the host named no heading', () => {
    render(<Stage factor={none({ kind: 'enrollmentRequired' })} heading={false} />);
    expect(screen.getByRole('dialog')).toHaveFocus();
  });

  // Mutant: the sentence says it only for the seeded case, so a stage that
  // reaches enrolment later keeps the previous status text.
  it('replaces a usable stage with it, moving focus to the heading', () => {
    const view = render(<Stage factor={ready('totp')} />);
    passwordField().focus();
    view.rerender(<Stage factor={none({ kind: 'enrollmentRequired' })} />);
    expectEnrolmentState();
    expect(headingEl()).toHaveFocus();
  });

  // Mutant: `activate` running, or announcing, on a state no input can complete.
  it('activating the primary does nothing, by click or by keyboard', async () => {
    const factor = none({ kind: 'enrollmentRequired' });
    const submit = vi.fn<StepUpSubmit>(async () => ({ kind: 'success' }));
    render(<Stage factor={factor} submit={submit} />);

    await userEvent.click(primary());
    primary().focus();
    await userEvent.keyboard('{Enter}');
    await userEvent.keyboard(' ');

    expect(factor.run).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
    expectEnrolmentState();
  });
});

// ── focusOnReady (T10b) ──────────────────────────────────────────────────

describe('focusOnReady', () => {
  const reading = () => none({ kind: 'reading' }, { passwordLegShown: true });
  const withoutPassword = (method: 'totp' | 'webauthn') =>
    ready(method, { passwordLegShown: false });
  const keyLabel = () =>
    within(screen.getByRole('group', { name: 'Passkey or security key' })).getByText(
      'Passkey or security key'
    );

  /** Opens on a read in flight, with focus where a host leaves it. */
  function open(
    where: 'heading' | 'dialog' | 'nowhere',
    props: Partial<StageProps> = {}
  ): { rerender: (factor: StepUpFactor) => void } {
    const stage = (factor: StepUpFactor) => (
      <Stage factor={factor} focusOnReady heading={where !== 'dialog'} {...props} />
    );
    const view = render(stage(reading()));
    if (where === 'heading') headingEl().focus();
    if (where === 'dialog') screen.getByRole('dialog').focus();
    return { rerender: (factor) => view.rerender(stage(factor)) };
  }

  // Mutant: the landing effect gated on something other than `ready`, or the
  // password branch missing.
  it.each(['heading', 'dialog', 'nowhere'] as const)(
    'lands on the password field when the read arrives and focus is on %s',
    (where) => {
      const view = open(where);
      expect(passwordField()).not.toHaveFocus();

      view.rerender(ready('totp'));

      expect(passwordField()).toHaveFocus();
    }
  );

  // Mutant: the picker branch missing, or the password preferred when it is absent.
  it('with no password leg, lands on the code input', () => {
    const view = open('heading');
    view.rerender(withoutPassword('totp'));
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toHaveFocus();
  });

  it('with no password leg and a security key, lands on the key label', () => {
    const view = open('heading');
    view.rerender(withoutPassword('webauthn'));
    expect(keyLabel()).toHaveFocus();
  });

  // Mutant: the pending flag never spent, so every later `ready` takes focus.
  it('fires once: a second landing in the same instance takes nothing', () => {
    const view = open('heading');
    view.rerender(ready('totp'));
    expect(passwordField()).toHaveFocus();

    // The instance restarts its read (a new purpose) and the host's focus returns to the heading.
    view.rerender(reading());
    expect(headingEl()).toHaveFocus();
    view.rerender(ready('totp'));

    expect(headingEl()).toHaveFocus();
    expect(passwordField()).not.toHaveFocus();
  });

  // Mutant: the `focusIsUnclaimed` check dropped, so a person who has already
  // reached another control is pulled away from it.
  it('does not take focus the person moved elsewhere', () => {
    const view = open('nowhere', { cancel: true });
    screen.getByRole('button', { name: 'Cancel' }).focus();

    view.rerender(ready('totp'));

    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(passwordField()).not.toHaveFocus();
  });

  it('does not take focus from a field the person already reached', () => {
    const view = open('nowhere');
    passwordField().focus();

    view.rerender(ready('totp'));

    expect(passwordField()).toHaveFocus();
  });

  // Mutant: a fallback that blurs, or focuses `body`, when there is nothing to focus.
  it('never focuses the body: with nothing to land on, focus stays where it was', () => {
    const view = open('heading');

    view.rerender(none({ kind: 'ready' }, { passwordLegShown: false }));

    expect(headingEl()).toHaveFocus();
    expect(document.activeElement).not.toBe(document.body);
  });

  // Mutant: the flag armed by `focusOnReady` alone, so an instance that opens
  // `ready` (no read to wait for) takes focus on mount.
  it('a stage that mounts already ready leaves the host focus alone', () => {
    const view = render(<Stage factor={ready('totp')} focusOnReady />);
    expect(document.activeElement).toBe(document.body);

    // A later read is not the opening one: it still takes nothing.
    view.rerender(<Stage factor={reading()} focusOnReady />);
    view.rerender(<Stage factor={ready('totp')} focusOnReady />);

    expect(passwordField()).not.toHaveFocus();
    expect(headingEl()).toHaveFocus();
  });

  // Mutant: the prop defaulting to true, or ignored.
  it('moves nothing when the host did not ask', () => {
    const view = render(<Stage factor={reading()} />);
    headingEl().focus();

    view.rerender(<Stage factor={ready('totp')} />);

    expect(headingEl()).toHaveFocus();
  });

  // Mutant: the landing effect declared after the notice effect, so it wins over a refusal's target.
  it('yields to a refusal that lands on the same commit', () => {
    const view = open('heading');

    view.rerender(ready('totp', { notice: { kind: 'missing', field: 'totp' } }));

    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toHaveFocus();
  });
});

// ── Against the real hook ────────────────────────────────────────────────

const READ = '/api/v1/mfa/step-up';
const BEGIN = '/api/v1/mfa/webauthn/verify-inline/begin';
const FINISH = '/api/v1/mfa/webauthn/verify-inline/finish';

type Route = (init: RequestInit) => Response | Promise<Response>;
let routes: Record<string, Route>;
let mockGet: ReturnType<typeof vi.fn>;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function readBody(methods: string[], defaultMethod: string | null, backupCodeAvailable = false) {
  return json({
    methods,
    default_method: defaultMethod,
    backup_code_available: backupCodeAvailable,
  });
}

const CREDENTIAL = {
  id: 'credential-id',
  rawId: new Uint8Array([1, 2, 3]).buffer,
  type: 'public-key',
  response: {
    authenticatorData: new Uint8Array([10, 20]).buffer,
    clientDataJSON: new Uint8Array([30, 40]).buffer,
    signature: new Uint8Array([50, 60]).buffer,
    userHandle: null,
  },
};

const BASE: StepUpFactorProps = {
  enabled: true,
  purpose: 'dm.purge',
  passwordLeg: 'always', // pragma: allowlist secret
  readFailure: 'block',
  allowBackup: true,
};

function Host({
  submit,
  capture,
  focusOnReady,
  cancel,
  ...config
}: Readonly<
  Partial<StepUpFactorProps> & {
    submit: StepUpSubmit;
    capture?: ApiRequestContext;
    focusOnReady?: boolean;
    cancel?: boolean;
  }
>) {
  const factor = useStepUpFactor({ ...BASE, ...config });
  const [typed, setTyped] = useState('');
  return (
    <Stage
      factor={factor}
      password={typed}
      onPasswordChange={setTyped}
      submit={submit}
      capture={capture}
      focusOnReady={focusOnReady}
      cancel={cancel}
    />
  );
}

const okSubmit = () => vi.fn<StepUpSubmit>(async () => ({ kind: 'success' }));

function hits(path: string) {
  return mockApiFetch.mock.calls.filter(([p]) => p === path);
}

describe('against the real hook', () => {
  beforeEach(() => {
    routes = {
      [READ]: () => readBody(['webauthn', 'totp'], 'totp', true),
      [BEGIN]: () =>
        json({ publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] } }),
      [FINISH]: () => json({ mfa_token: 'inline-token' }),
    };
    mockApiFetch.mockReset();
    mockApiFetch.mockImplementation(async (path: string, init: RequestInit) => {
      const route = routes[path];
      if (!route) throw new Error(`unexpected request to ${path}`);
      return route(init);
    });
    mockGet = vi.fn().mockResolvedValue(CREDENTIAL);
    Object.defineProperty(navigator, 'credentials', {
      value: { get: mockGet },
      writable: true,
      configurable: true,
    });
  });

  afterEach(() => {
    resetRuntimeServerBase();
  });

  const totpInput = () => screen.findByRole('textbox', { name: 'Authenticator app code' });

  describe('what is offered', () => {
    // Mutant: Backup offered without backup_code_available.
    it('offers no backup code unless the read reported one', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', false);
      render(<Host submit={okSubmit()} />);
      await totpInput();
      expect(screen.queryByRole('button', { name: /backup code/i })).not.toBeInTheDocument();
    });

    it('offers a backup code when the read reported one and TOTP is offered', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      render(<Host submit={okSubmit()} />);
      await totpInput();
      expect(screen.getByRole('button', { name: 'Use a backup code instead' })).toBeVisible();
    });

    // Mutant: Backup offered without TOTP.
    it('offers no backup code without TOTP, even when one is available', async () => {
      routes[READ] = () => readBody(['webauthn'], 'webauthn', true);
      render(<Host submit={okSubmit()} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      expect(screen.queryByRole('button', { name: /backup code/i })).not.toBeInTheDocument();
    });

    it('offers no backup code when the surface does not allow one', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      render(<Host submit={okSubmit()} allowBackup={false} />);
      await totpInput();
      expect(screen.queryByRole('button', { name: /backup code/i })).not.toBeInTheDocument();
    });

    // Mutant: a security key offered with `purpose: null`.
    it('offers no security key without a purpose, whatever the server defaults to', async () => {
      routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
      render(<Host submit={okSubmit()} purpose={null} />);
      await totpInput();
      expect(
        screen.queryByRole('group', { name: 'Passkey or security key' })
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: 'Use passkey or security key instead' })
      ).not.toBeInTheDocument();
    });

    it('offers the security key when there is a purpose', async () => {
      routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
      render(<Host submit={okSubmit()} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      expect(screen.getByRole('button', { name: 'Use authenticator app instead' })).toBeVisible();
    });
  });

  describe('a partial code is never sent', () => {
    // Every send spends a unit of the route's rate-limited budget, and a code
    // short of its factor's shape cannot pass. Mutant: completeness judged by
    // emptiness alone, so one typed digit is sent.
    it.each([
      [
        'an authenticator code',
        'Authenticator app code',
        '12345',
        '123456',
        'Enter the 6-digit code from your authenticator app to continue.',
      ],
      ['a backup code', 'Backup code', 'ABCD123', 'ABCD1234', 'Enter a backup code to continue.'],
    ])('holds %s until it has its full shape', async (_label, name, partial, full, prompt) => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await totpInput();
      if (name === 'Backup code') {
        await userEvent.click(screen.getByRole('button', { name: 'Use a backup code instead' }));
      }
      await userEvent.type(passwordField(), TYPED);
      const input = screen.getByRole('textbox', { name });
      await userEvent.type(input, partial);

      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      await userEvent.click(primary());
      expect(within(screen.getByRole('alert')).getByText(prompt)).toBeInTheDocument();
      expect(submit).not.toHaveBeenCalled();

      await userEvent.clear(input);
      await userEvent.type(input, full);
      expect(primary()).not.toHaveAttribute('aria-disabled', 'true');
      await userEvent.click(primary());
      await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
      expect(submit.mock.calls[0][0]).toBe(full);
    });
  });

  describe('switching methods', () => {
    // Mutant: a switch that does not clear the code.
    it('clears the code, so it is not there when the method comes back', async () => {
      render(<Host submit={okSubmit()} />);
      await userEvent.type(await totpInput(), '123456');
      await userEvent.click(screen.getByRole('button', { name: 'Use a backup code instead' }));
      await userEvent.click(screen.getByRole('button', { name: 'Use authenticator app instead' }));
      expect(await totpInput()).toHaveValue('');
    });

    // Mutant: a switch that does not abort the running ceremony.
    it('aborts a running ceremony and withdraws its status', async () => {
      routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
      mockGet.mockImplementation(() => new Promise(() => undefined));
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      await userEvent.click(primary());
      await waitFor(() => expect(mockGet).toHaveBeenCalled());
      expect(statusLine()).toHaveTextContent(WAITING);
      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      const { signal } = mockGet.mock.calls[0][0] as { signal: AbortSignal };
      expect(signal.aborted).toBe(false);

      await userEvent.click(screen.getByRole('button', { name: 'Use authenticator app instead' }));

      expect(signal.aborted).toBe(true);
      await waitFor(() => expect(statusLine().textContent).toBe(''));
      expect(passwordField()).not.toHaveAttribute('readonly');
      expect(primary()).toHaveAttribute('aria-disabled', 'true'); // an empty code
      expect(submit).not.toHaveBeenCalled();
    });
  });

  describe('the primary', () => {
    it('asks for the password first, guarded, and focuses it', async () => {
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await userEvent.type(await totpInput(), '123456');
      expect(primary()).toHaveAttribute('aria-disabled', 'true');

      await userEvent.click(primary());

      expect(screen.getByRole('alert')).toHaveTextContent('Enter your password to continue.');
      expect(passwordField()).toHaveFocus();
      expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
      expectContract(1);
      expect(submit).not.toHaveBeenCalled();
    });

    it('then asks for the code, guarded, and focuses it', async () => {
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      const input = await totpInput();
      expect(primary()).toHaveAttribute('aria-disabled', 'true');

      await userEvent.click(primary());

      expect(screen.getByRole('alert')).toHaveTextContent(
        'Enter the 6-digit code from your authenticator app to continue.'
      );
      expect(input).toHaveFocus();
      expect(input).toHaveAttribute('aria-invalid', 'true');
      expect(submit).not.toHaveBeenCalled();
      expect(hits(BEGIN)).toHaveLength(0);
    });

    it('says it is still checking at once while the read is in flight', async () => {
      routes[READ] = () => new Promise<Response>(() => undefined);
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);

      await userEvent.click(primary());

      expect(statusLine().textContent).toBe(CHECKING);
      expect(submit).not.toHaveBeenCalled();
    });

    it('submits the code once when complete, and not again while that is out', async () => {
      let finish!: (outcome: { kind: 'success' }) => void;
      const submit = vi.fn<StepUpSubmit>(() => new Promise((resolve) => (finish = resolve)));
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(await totpInput(), '123 456');
      expect(primary()).toHaveAttribute('aria-disabled', 'false');

      await userEvent.click(primary());
      await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
      expect(submit.mock.calls[0][0]).toBe('123456');
      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      expect(passwordField()).toBeDisabled();

      await userEvent.click(primary());
      expect(submit).toHaveBeenCalledTimes(1);
      await act(async () => finish({ kind: 'success' }));
    });

    // Mutant (C37), end to end: the password editable while the ceremony runs.
    it('locks the password while the ceremony runs', async () => {
      routes[READ] = () => readBody(['webauthn'], 'webauthn', false);
      mockGet.mockImplementation(() => new Promise(() => undefined));
      render(<Host submit={okSubmit()} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      await userEvent.click(primary());
      await waitFor(() => expect(passwordField()).toHaveAttribute('readonly'));
      expectContract(0);
    });
  });

  // The host's own password state is invisible once a terminal status hides the
  // field, so this host prints it.
  describe('a terminal status', () => {
    function ProbedHost({ submit }: Readonly<{ submit: StepUpSubmit }>) {
      const factor = useStepUpFactor(BASE);
      const [typed, setTyped] = useState('');
      return (
        <>
          <Stage factor={factor} password={typed} onPasswordChange={setTyped} submit={submit} />
          <p data-testid="host-password">{typed}</p>
        </>
      );
    }
    const hostPassword = () => screen.getByTestId('host-password');

    // Mutant: the clearing effect deleted.
    it('empties a password typed before a refused read (403 account_disabled)', async () => {
      const read = deferred<Response>();
      routes[READ] = () => read.promise;
      render(<ProbedHost submit={okSubmit()} />);
      await userEvent.type(passwordField(), TYPED);
      expect(hostPassword()).toHaveTextContent(TYPED);

      await act(async () => read.resolve(json({ error_code: 'account_disabled' }, 403)));

      await waitFor(() =>
        expect(statusLine()).toHaveTextContent("Your account can't do this right now.")
      );
      await waitFor(() => expect(hostPassword()).toBeEmptyDOMElement());
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });

    // Mutant: the clearing effect deleted.
    it('empties a password the server then found no usable method for', async () => {
      routes[READ] = () => readBody(['totp'], 'totp');
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'mfaRequired', methods: ['email'] },
      }));
      render(<ProbedHost submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(await totpInput(), '123456');
      expect(hostPassword()).toHaveTextContent(TYPED);

      await userEvent.click(primary());

      await waitFor(() => expect(statusLine()).toHaveTextContent(/can't be used here/));
      await waitFor(() => expect(hostPassword()).toBeEmptyDOMElement());
    });

    // Mutant: the clearing effect deleted. The 401 comes from the passkey begin.
    it('empties a password when the session turns out to be expired', async () => {
      routes[READ] = () => readBody(['webauthn'], 'webauthn');
      routes[BEGIN] = () => json({ error: 'Unauthorized' }, 401);
      render(<ProbedHost submit={okSubmit()} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      expect(hostPassword()).toHaveTextContent(TYPED);

      await userEvent.click(primary());

      await waitFor(() => expect(statusLine()).toHaveTextContent('Sign in again to continue.'));
      await waitFor(() => expect(hostPassword()).toBeEmptyDOMElement());
    });

    // Mutant: the clearing effect or the heading focus missing the enrolment
    // kind (E8), which leaves a dead password field and the primary focused.
    it('empties the password and lands on the heading when the request answers enrolment required', async () => {
      routes[READ] = () => readBody(['totp'], 'totp');
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'enrollmentRequired' },
      }));
      render(<ProbedHost submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(await totpInput(), '123456');

      await userEvent.click(primary());

      await waitFor(() => expect(statusLine().textContent).toBe(ENROLLMENT));
      await waitFor(() => expect(hostPassword()).toBeEmptyDOMElement());
      expect(headingEl()).toHaveFocus();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
      expect(screen.queryAllByRole('alert')).toHaveLength(0);
      expectContract(0);
    });
  });

  // A refusal-triggered surface opens straight on the state (G2).
  describe('an instance seeded with enrolment required', () => {
    // Mutant: the seed still starting the read, or showing a password leg.
    it('shows the sentence at once, reads nothing and offers nothing to act on', async () => {
      const submit = okSubmit();
      render(<Host submit={submit} seed={{ kind: 'enrollmentRequired' }} />);

      expect(statusLine().textContent).toBe(ENROLLMENT);
      expect(headingEl()).toHaveFocus();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      expectContract(0);

      await userEvent.click(primary());

      expect(submit).not.toHaveBeenCalled();
      expect(hits(READ)).toHaveLength(0);
      expect(statusLine().textContent).toBe(ENROLLMENT);
      expect(screen.queryAllByRole('alert')).toHaveLength(0);
    });
  });

  // Mutant: begin's `res.json()` without the `.catch`. A proxy's HTML 401 then
  // rejects with a SyntaxError, which the hook reads as a failed ceremony
  // rather than an expired session.
  it('ends a passkey ceremony in session-expired when begin answers 401 with an HTML body', async () => {
    routes[READ] = () => readBody(['webauthn'], 'webauthn');
    routes[BEGIN] = () =>
      new Response('<html>Unauthorized</html>', {
        status: 401,
        headers: { 'Content-Type': 'text/html' },
      });
    const submit = okSubmit();
    render(<Host submit={submit} />);
    await screen.findByRole('group', { name: 'Passkey or security key' });
    await userEvent.type(passwordField(), TYPED);

    await userEvent.click(primary());

    await waitFor(() => expect(statusLine()).toHaveTextContent('Sign in again to continue.'));
    expect(mockGet).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  // Mutant: finish's `res.json()` without the `.catch`.
  it('ends a passkey ceremony in session-expired when finish answers 401 with an HTML body', async () => {
    routes[READ] = () => readBody(['webauthn'], 'webauthn');
    routes[FINISH] = () =>
      new Response('<html>Unauthorized</html>', {
        status: 401,
        headers: { 'Content-Type': 'text/html' },
      });
    const submit = okSubmit();
    render(<Host submit={submit} />);
    await screen.findByRole('group', { name: 'Passkey or security key' });
    await userEvent.type(passwordField(), TYPED);

    await userEvent.click(primary());

    await waitFor(() => expect(statusLine()).toHaveTextContent('Sign in again to continue.'));
    expect(mockGet).toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  describe('a refusal that moves the panel', () => {
    it('lands on the new input and names the authenticator app only for the app', async () => {
      routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'mfaRequired', methods: ['totp'] },
      }));
      render(<Host submit={submit} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      await userEvent.click(primary());

      const input = await totpInput();
      await waitFor(() => expect(input).toHaveFocus());
      expect(screen.getByRole('alert')).toHaveTextContent(
        'Enter the 6-digit code from your authenticator app to continue.'
      );
      expectContract(1);
    });

    it('stays on the passkey, with the primary focused and no mention of the app', async () => {
      routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'mfaRequired', methods: ['webauthn', 'totp'] },
      }));
      render(<Host submit={submit} />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      await userEvent.click(primary());

      await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument());
      expect(screen.getByRole('alert').textContent).not.toMatch(/authenticator/i);
      await waitFor(() => expect(primary()).toHaveFocus());
      expectContract(1);
    });
  });

  // ── The host is still preparing (T1c; D11, Q6, C82) ─────────────────────

  describe('preparing', () => {
    /** A complete form on an authenticator-app account, while the host prepares. */
    async function completeForm(props: Partial<React.ComponentProps<typeof Host>> = {}) {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const submit = okSubmit();
      const view = render(<Host submit={submit} preparing {...props} />);
      await userEvent.type(passwordField(), TYPED);
      const input = await totpInput();
      await userEvent.type(input, '123456');
      return { submit, view, input };
    }

    // Mutant: `preparing` treated as non-blocking.
    it('keeps the primary aria-disabled while preparing, then frees it', async () => {
      const { submit, view } = await completeForm();

      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      expect(primary()).not.toBeDisabled();

      view.rerender(<Host submit={submit} preparing={false} />);

      expect(primary()).toHaveAttribute('aria-disabled', 'false');
    });

    // Mutant: the click handler starting the ceremony, or running, before
    // preparation has finished.
    it('an activation shows "Getting things ready…" and submits nothing', async () => {
      const { submit } = await completeForm();

      await userEvent.click(primary());

      expect(statusLine().textContent).toBe(PREPARING);
      expect(screen.queryAllByRole('alert')).toHaveLength(0);
      expect(submit).not.toHaveBeenCalled();
      expectContract(0);
    });

    it('an activation starts no passkey ceremony either', async () => {
      routes[READ] = () => readBody(['webauthn'], 'webauthn', false);
      const submit = okSubmit();
      render(<Host submit={submit} preparing />);
      await screen.findByRole('group', { name: 'Passkey or security key' });
      await userEvent.type(passwordField(), TYPED);
      expect(primary()).toHaveAttribute('aria-disabled', 'true');

      await userEvent.click(primary());

      expect(statusLine().textContent).toBe(PREPARING);
      expect(hits(BEGIN)).toHaveLength(0);
      expect(mockGet).not.toHaveBeenCalled();
      expect(submit).not.toHaveBeenCalled();
    });

    // Mutant: `preparing` checked before the fields, hiding what the person can still fill in.
    it('a missing password still wins, with its own text', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const submit = okSubmit();
      render(<Host submit={submit} preparing />);
      await userEvent.type(await totpInput(), '123456');

      await userEvent.click(primary());

      expect(screen.getByRole('alert')).toHaveTextContent('Enter your password to continue.');
      expect(statusLine().textContent).toBe('');
      expect(passwordField()).toHaveFocus();
    });

    it('a missing code still wins, with its own text', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const submit = okSubmit();
      render(<Host submit={submit} preparing />);
      await userEvent.type(passwordField(), TYPED);
      const input = await totpInput();

      await userEvent.click(primary());

      expect(screen.getByRole('alert')).toHaveTextContent(
        'Enter the 6-digit code from your authenticator app to continue.'
      );
      expect(statusLine().textContent).toBe('');
      expect(input).toHaveFocus();
    });

    // Mutant: `preparing` joining the instance key, which would clear the code
    // and read again; or the notice outliving the preparation it answered.
    it('flipping preparing false without a remount keeps the code and the instance', async () => {
      const { submit, view, input } = await completeForm();
      await userEvent.click(primary());
      expect(statusLine().textContent).toBe(PREPARING);

      view.rerender(<Host submit={submit} preparing={false} />);

      expect(statusLine().textContent).toBe('');
      expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).toBe(input);
      expect(input).toHaveValue('123456');
      expect(passwordField()).toHaveValue(TYPED);
      expect(hits(READ)).toHaveLength(1);

      await userEvent.click(primary());
      await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
      expect(submit.mock.calls[0][0]).toBe('123456');
    });

    // Mutant (C82): the capture not passed on, so `run` takes a fresh one.
    it('run works against the capture the host took when it began preparing', async () => {
      const capture = captureApiRequestContext();
      const { submit, view } = await completeForm({ capture });
      view.rerender(<Host submit={submit} capture={capture} preparing={false} />);

      await userEvent.click(primary());

      await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
      expect(submit.mock.calls[0][1]).toBe(capture);
    });

    it('a capture taken before an account change ends in session-expired, sending nothing', async () => {
      const capture = captureApiRequestContext();
      const { submit, view } = await completeForm({ capture });
      view.rerender(<Host submit={submit} capture={capture} preparing={false} />);
      act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));

      await userEvent.click(primary());

      await waitFor(() => expect(statusLine()).toHaveTextContent('Sign in again to continue.'));
      expect(submit).not.toHaveBeenCalled();
    });
  });

  // SE3: what was typed into a stage was typed for the account and server it
  // opened for, so a change before the activation sends nothing anywhere.
  describe('an account or server change after the stage opened', () => {
    async function typedForm() {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const submit = okSubmit();
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(await totpInput(), '123456');
      return submit;
    }

    // Mutant: `run` defaulting to a capture taken at activation, not at open.
    it.each([
      ['server', () => setRuntimeServerBase('https://other-server.example.test')],
      ['account', () => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }))],
    ])('a %s change sends nothing and shows the session sentence', async (_name, change) => {
      const submit = await typedForm();
      act(() => change());

      await userEvent.click(primary());

      await waitFor(() => expect(statusLine()).toHaveTextContent('Sign in again to continue.'));
      expect(submit).not.toHaveBeenCalled();
      expect(mockApiFetch.mock.calls.map(([path]) => path)).toEqual([READ]);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });

    it('with no change, the same flow sends once', async () => {
      const submit = await typedForm();

      await userEvent.click(primary());

      await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
      expect(submit.mock.calls[0][0]).toBe('123456');
    });
  });

  // ── A 429 from the passkey ceremony (D19) ───────────────────────────────

  describe('a 429 from the passkey ceremony', () => {
    // Mutant: a 429 mapped back to the invalid-key copy (an alert on the group).
    it.each([
      ['begin', BEGIN],
      ['finish', FINISH],
    ] as const)(
      '%s: says so as a polite status, with no alert and no re-read',
      async (_step, path) => {
        routes[READ] = () => readBody(['webauthn', 'totp'], 'webauthn', false);
        routes[path] = () => json({ error: 'Too many requests' }, 429);
        const submit = okSubmit();
        render(<Host submit={submit} />);
        await screen.findByRole('group', { name: 'Passkey or security key' });
        await userEvent.type(passwordField(), TYPED);

        await userEvent.click(primary());

        await waitFor(() => expect(statusLine().textContent).toBe(RATE_LIMITED));
        expect(statusLine().closest('[role="alert"]')).toBeNull();
        expect(screen.queryAllByRole('alert')).toHaveLength(0);
        expect(document.body.textContent).not.toMatch(/couldn't verify/i);
        expect(hits(READ)).toHaveLength(1);
        expect(submit).not.toHaveBeenCalled();
        // Retrying needs a touch, so focus returns to the primary, which stays usable.
        expect(primary()).toHaveFocus();
        expect(primary()).toHaveAttribute('aria-disabled', 'false');
        expectContract(0);
      }
    );
  });

  // ── focusOnReady against the real read (T10b) ───────────────────────────

  describe('focusOnReady', () => {
    it('lands on the password field once the opening read arrives', async () => {
      const read = deferred<Response>();
      routes[READ] = () => read.promise;
      render(<Host submit={okSubmit()} focusOnReady />);
      headingEl().focus();
      expect(headingEl()).toHaveFocus();

      await act(async () => read.resolve(readBody(['totp'], 'totp', true)));

      await waitFor(() => expect(passwordField()).toHaveFocus());
    });

    it('with no password leg, lands on the security key', async () => {
      const read = deferred<Response>();
      routes[READ] = () => read.promise;
      render(
        <Host
          submit={okSubmit()}
          focusOnReady
          passwordLeg="whenNoMfa" // pragma: allowlist secret
        />
      );
      headingEl().focus();

      await act(async () => read.resolve(readBody(['webauthn'], 'webauthn', false)));

      const group = await screen.findByRole('group', { name: 'Passkey or security key' });
      await waitFor(() => expect(within(group).getByText('Passkey or security key')).toHaveFocus());
    });

    it('does not take focus from Cancel when the read lands', async () => {
      const read = deferred<Response>();
      routes[READ] = () => read.promise;
      render(<Host submit={okSubmit()} focusOnReady cancel />);
      screen.getByRole('button', { name: 'Cancel' }).focus();

      await act(async () => read.resolve(readBody(['totp'], 'totp', true)));
      await totpInput();

      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    });
  });
});

// ── An expired confirmation (#3509, T1d) ─────────────────────────────────

describe('an expired confirmation (#3509)', () => {
  const EXPIRED_COPY = 'Your confirmation expired. Enter your password again.';

  // Mutants: the notice placed off the password field; the copy not the shared one.
  it('sits on the password field with the shared copy, marked and described', async () => {
    const { STEP_UP_TOKEN_EXPIRED_MESSAGE } =
      await import('@/renderer/services/system/stepUpToken');
    render(<Stage factor={ready('totp', { notice: { kind: 'tokenExpired' } })} />);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent(STEP_UP_TOKEN_EXPIRED_MESSAGE);
    expect(alert).toHaveTextContent(EXPIRED_COPY);
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
    expect(ids(passwordField())).toEqual([alert.id]);
    expect(screen.getByRole('textbox', { name: 'Authenticator app code' })).not.toHaveAttribute(
      'aria-invalid'
    );
    expectContract(1);
  });

  // Mutant: `tokenExpired` missing from the focus table.
  it('moves focus to the password field', () => {
    const view = render(<Stage factor={ready('totp')} />);
    view.rerender(<Stage factor={ready('totp', { notice: { kind: 'tokenExpired' } })} />);
    expect(passwordField()).toHaveFocus();
  });

  describe('against the real hook', () => {
    beforeEach(() => {
      routes = { [READ]: () => readBody(['totp'], 'totp', true) };
      mockApiFetch.mockReset();
      mockApiFetch.mockImplementation(async (path: string, init: RequestInit) => {
        const route = routes[path];
        if (!route) throw new Error(`unexpected request to ${path}`);
        return route(init);
      });
    });

    afterEach(() => {
      resetRuntimeServerBase();
    });

    // Mutant: the hook ignoring `tokenExpired` on a run's outcome.
    it('a refused token from the request shows the expiry on the password field', async () => {
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'passwordRequired', tokenExpired: true },
      }));
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(
        await screen.findByRole('textbox', { name: 'Authenticator app code' }),
        '123456'
      );

      await userEvent.click(primary());

      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent(EXPIRED_COPY);
      expect(alert).not.toHaveTextContent('Enter your password to continue.');
      expect(passwordField()).toHaveFocus();
      expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
      expect(submit).toHaveBeenCalledTimes(1);
      expectContract(1);
    });

    // Mutant: the hook ignoring `tokenExpired` on the seed.
    it('a seeded refused token shows the expiry on the password field once the read lands', async () => {
      routes[READ] = () => readBody([], null);
      render(
        <Host
          submit={vi.fn<StepUpSubmit>()}
          passwordLeg="whenNoMfa" // pragma: allowlist secret
          seed={{ kind: 'passwordRequired', tokenExpired: true }}
        />
      );

      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent(EXPIRED_COPY);
      expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
      expect(ids(passwordField())).toEqual([alert.id]);
    });

    // Mutant: an unmarked password refusal worded as expired.
    it('an unmarked password refusal still asks for the password', async () => {
      const submit = vi.fn<StepUpSubmit>(async () => ({
        kind: 'refusal',
        refusal: { kind: 'passwordRequired' },
      }));
      render(<Host submit={submit} />);
      await userEvent.type(passwordField(), TYPED);
      await userEvent.type(
        await screen.findByRole('textbox', { name: 'Authenticator app code' }),
        '123456'
      );

      await userEvent.click(primary());

      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent('Enter your password to continue.');
      expect(alert).not.toHaveTextContent(EXPIRED_COPY);
    });
  });
});

// ── Enter presses the primary (design §4.3) ──────────────────────────────

describe('Enter in a field', () => {
  const codeField = () => screen.getByRole('textbox', { name: 'Authenticator app code' });

  // Mutant: the password field without its Enter handler.
  it('in the password field runs a complete activation', () => {
    const run = vi.fn(async () => null);
    render(<Stage factor={ready('totp', { run })} />);
    fireEvent.keyDown(passwordField(), { key: 'Enter' });
    expect(run).toHaveBeenCalledOnce();
  });

  // Mutant: the picker's onEnter not wired to the host's primary.
  it('in the code field runs a complete activation', () => {
    const run = vi.fn(async () => null);
    render(<Stage factor={ready('totp', { run })} />);
    fireEvent.keyDown(codeField(), { key: 'Enter' });
    expect(run).toHaveBeenCalledOnce();
  });

  // Mutant: Enter calling `run` directly, past the activation guard.
  it('on an incomplete stage names what is missing and sends nothing', () => {
    const run = vi.fn(async () => null);
    const announceMissing = vi.fn();
    render(
      <Stage factor={ready('totp', { run, announceMissing, firstMissing: vi.fn(() => 'totp') })} />
    );
    fireEvent.keyDown(passwordField(), { key: 'Enter' });
    expect(announceMissing).toHaveBeenCalledWith('totp');
    expect(run).not.toHaveBeenCalled();
  });

  // Mutant: the guard's busy check bypassed by a key.
  it('while a run is in flight does nothing', () => {
    const run = vi.fn(async () => null);
    render(<Stage factor={ready('totp', { run, phase: 'ceremony' })} />);
    fireEvent.keyDown(passwordField(), { key: 'Enter' });
    expect(run).not.toHaveBeenCalled();
  });

  // Mutant: the password field pressing the primary on a modified Enter.
  it.each([
    ['Shift', { shiftKey: true }],
    ['Ctrl', { ctrlKey: true }],
  ])('%s+Enter in the password field does nothing; a plain Enter then runs', (_k, modifier) => {
    const run = vi.fn(async () => null);
    render(<Stage factor={ready('totp', { run })} />);
    fireEvent.keyDown(passwordField(), { key: 'Enter', ...modifier });
    expect(run).not.toHaveBeenCalled();
    fireEvent.keyDown(passwordField(), { key: 'Enter' });
    expect(run).toHaveBeenCalledOnce();
  });

  // Mutant: the password field submitting on an IME's Enter.
  it('while an input method is composing does nothing', () => {
    const run = vi.fn(async () => null);
    render(<Stage factor={ready('totp', { run })} />);
    fireEvent.keyDown(passwordField(), { key: 'Enter', isComposing: true });
    expect(run).not.toHaveBeenCalled();
    fireEvent.keyDown(passwordField(), { key: 'Enter' });
    expect(run).toHaveBeenCalledOnce();
  });
});
