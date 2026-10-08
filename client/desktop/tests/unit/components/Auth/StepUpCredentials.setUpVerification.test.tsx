import { useRef } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import StepUpCredentials from '@/renderer/components/Auth/StepUpCredentials';
import type { StepUpFactor, StepUpStatus } from '@/renderer/hooks/auth/useStepUpFactor';
import {
  captureApiRequestContext,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';

// The stage's optional "Set up verification" link (#3456 §3.6a, T2b). Given
// `onSetUpVerification`, the enrolment state offers it after its sentence;
// omitted, every host that predates it renders exactly what it rendered before.
//
// The no-prop markup below was recorded from the stage as it stood before the
// prop existed (`git show HEAD:` of the component, rendered with these same
// fixtures), so the assertions pin "byte-identical", not "whatever it renders now".
//
// "Mutant:" comments name the production change each test exists to turn red.

const ENROLMENT_SENTENCE = 'Set up an authenticator app or security key in Settings to do this.';
const BLOCKED_SENTENCE =
  "We couldn't check your verification methods. Check your connection and try again.";

// Recorded from the pre-change component. No `useId` value reaches either state's markup.
const ENROLMENT_HTML =
  '<fieldset class="step-up"><div class="step-up__region"><output class="step-up__status">' +
  ENROLMENT_SENTENCE +
  '</output></div></fieldset>';
const BLOCKED_HTML =
  '<fieldset class="step-up"><div class="step-up__region"><output class="step-up__status">' +
  BLOCKED_SENTENCE +
  '</output><button type="button" class="step-up__link">Retry</button></div></fieldset>';

function makeFactor(status: StepUpStatus, overrides: Partial<StepUpFactor> = {}): StepUpFactor {
  return {
    status,
    methods: [],
    method: null,
    passwordLegShown: false,
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
    confirmCurrent: vi.fn(() => true),
    ...overrides,
  };
}

interface HostProps {
  factor: StepUpFactor;
  onSetUpVerification?: () => void;
  capture?: ApiRequestContext;
}

/** A host with a heading for the stage to land focus on, as the dialog has. */
function Host({ factor, onSetUpVerification, capture }: Readonly<HostProps>) {
  const headingRef = useRef<HTMLHeadingElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
  return (
    <dialog open tabIndex={-1} aria-label="Confirm">
      <h2 ref={headingRef} tabIndex={-1}>
        Confirm
      </h2>
      <StepUpCredentials
        factor={factor}
        password=""
        onPasswordChange={() => undefined}
        primaryRef={primaryRef}
        headingRef={headingRef}
        onSetUpVerification={onSetUpVerification}
        capture={capture}
      />
      <button type="button" ref={primaryRef}>
        Continue
      </button>
    </dialog>
  );
}

const ENROLMENT = makeFactor({ kind: 'enrollmentRequired' });
const setUp = () => screen.queryByRole('button', { name: 'Set up verification' });
const stage = (container: HTMLElement) => container.querySelector('fieldset.step-up');

beforeEach(() => {
  resetAllStores();
});

describe('without onSetUpVerification', () => {
  // Mutant: the link rendered whatever the prop is, or any other markup added to the stage.
  it('renders the enrolment state exactly as it did before the prop existed', () => {
    const { container } = render(<Host factor={ENROLMENT} />);
    expect(stage(container)?.outerHTML).toBe(ENROLMENT_HTML);
    expect(setUp()).toBeNull();
  });

  // Mutant: the new button sharing the Retry condition, or sitting outside the enrolment branch.
  it('renders the blocked state exactly as it did before the prop existed', () => {
    const { container } = render(<Host factor={makeFactor({ kind: 'blocked' })} />);
    expect(stage(container)?.outerHTML).toBe(BLOCKED_HTML);
    expect(setUp()).toBeNull();
  });
});

describe('with onSetUpVerification', () => {
  // Mutant: the link placed before the sentence, or the sentence itself changed.
  it('adds one link after the status line and changes nothing else in the enrolment state', () => {
    const { container } = render(<Host factor={ENROLMENT} onSetUpVerification={vi.fn()} />);
    const link = setUp();
    expect(link).not.toBeNull();
    expect(link?.className).toBe('step-up__link');
    expect(link?.getAttribute('type')).toBe('button');

    const output = screen.getByRole('status');
    expect(output).toHaveTextContent(ENROLMENT_SENTENCE);
    const siblings = Array.from(output.parentElement?.children ?? []);
    expect(siblings.indexOf(link as Element)).toBe(siblings.indexOf(output) + 1);

    const clone = stage(container)?.cloneNode(true) as HTMLElement;
    clone.querySelector('button.step-up__link')?.remove();
    expect(clone.outerHTML).toBe(ENROLMENT_HTML);
  });

  // Mutant: focus sent to the new link (or the primary) instead of the heading.
  it('leaves focus on the heading, so the sentence is read before the link is reached', () => {
    render(<Host factor={ENROLMENT} onSetUpVerification={vi.fn()} />);
    expect(screen.getByRole('heading', { name: 'Confirm' })).toHaveFocus();
  });

  // Mutant: the link made unfocusable, or ordered after the primary.
  it('is the next control in tab order after the heading', async () => {
    render(<Host factor={ENROLMENT} onSetUpVerification={vi.fn()} />);
    await userEvent.tab();
    expect(setUp()).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Continue' })).toHaveFocus();
  });

  // Mutant: onClick not wired, or wired to the Retry handler.
  it('calls the prop once per activation, by pointer or by keyboard', async () => {
    const onSetUpVerification = vi.fn();
    const factor = makeFactor({ kind: 'enrollmentRequired' });
    render(<Host factor={factor} onSetUpVerification={onSetUpVerification} />);

    await userEvent.click(setUp() as HTMLElement);
    expect(onSetUpVerification).toHaveBeenCalledTimes(1);

    setUp()?.focus();
    await userEvent.keyboard('{Enter}');
    expect(onSetUpVerification).toHaveBeenCalledTimes(2);
    expect(factor.retryRead).not.toHaveBeenCalled();
  });

  // Mutant: the link calling the prop without `factor.confirmCurrent`, or ignoring its answer.
  it('acts only while the stage is current', async () => {
    const onSetUpVerification = vi.fn();
    const confirmCurrent = vi.fn(() => false);
    const factor = makeFactor({ kind: 'enrollmentRequired' }, { confirmCurrent });
    render(<Host factor={factor} onSetUpVerification={onSetUpVerification} />);

    await userEvent.click(setUp() as HTMLElement);

    expect(confirmCurrent).toHaveBeenCalledTimes(1);
    expect(onSetUpVerification).not.toHaveBeenCalled();
  });

  // Mutant: the stage asking `confirmCurrent()` without the host's capture (C82).
  it("checks currency against the host's capture when it gives one", async () => {
    const confirmCurrent = vi.fn(() => true);
    const factor = makeFactor({ kind: 'enrollmentRequired' }, { confirmCurrent });
    const capture = captureApiRequestContext();
    render(<Host factor={factor} onSetUpVerification={vi.fn()} capture={capture} />);

    await userEvent.click(setUp() as HTMLElement);

    expect(confirmCurrent).toHaveBeenCalledWith(capture);
  });

  // Mutant: the link given an alert role or no accessible name.
  it('adds no alert to the enrolment state, and the link is named', () => {
    render(<Host factor={ENROLMENT} onSetUpVerification={vi.fn()} />);
    expect(screen.queryAllByRole('alert')).toHaveLength(0);
    expect(setUp()).toHaveAccessibleName('Set up verification');
  });

  // Mutant: the `enrollmentRequired` condition dropped or widened to another status.
  it.each<[string, StepUpFactor]>([
    ['reading', makeFactor({ kind: 'reading' })],
    ['ready with a method', makeFactor({ kind: 'ready' }, { methods: ['totp'], method: 'totp' })],
    ['ready with none offered', makeFactor({ kind: 'ready' })],
    ['blocked', makeFactor({ kind: 'blocked' })],
    ['refused', makeFactor({ kind: 'refused', reason: 'client' })],
    ['no usable method', makeFactor({ kind: 'noUsableMethod' })],
    ['session expired', makeFactor({ kind: 'sessionExpired' })],
  ])('renders no link in the %s state', (_name, factor) => {
    render(<Host factor={factor} onSetUpVerification={vi.fn()} />);
    expect(setUp()).toBeNull();
  });

  // Mutant: the Retry button replaced by the new link, or the two sharing a condition.
  it('keeps Retry in the blocked state', async () => {
    const factor = makeFactor({ kind: 'blocked' });
    render(<Host factor={factor} onSetUpVerification={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(factor.retryRead).toHaveBeenCalledTimes(1);
  });
});
